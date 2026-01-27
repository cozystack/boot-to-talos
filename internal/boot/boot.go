//go:build linux

package boot

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/cockroachdb/errors"
	"golang.org/x/sys/unix"

	"github.com/cozystack/boot-to-talos/internal/cli"
	"github.com/cozystack/boot-to-talos/internal/efi"
	"github.com/cozystack/boot-to-talos/internal/types"
)

// CreateMemfdFromReader creates an anonymous file in memory via memfd_create and copies data from reader.
func CreateMemfdFromReader(name string, reader io.Reader) (*os.File, error) {
	const MFD_CLOEXEC = 0x0001

	nameBytes := []byte(name + "\x00")
	fd, _, errno := unix.Syscall(sysMemfdCreate, uintptr(unsafe.Pointer(&nameBytes[0])), MFD_CLOEXEC, 0)
	if errno != 0 {
		return nil, errors.Newf("memfd_create failed: %v", errno)
	}

	file := os.NewFile(fd, name)
	if file == nil {
		return nil, errors.New("failed to create file from fd")
	}

	// Copy data from reader to memfd
	if _, err := io.Copy(file, reader); err != nil {
		file.Close()
		return nil, errors.Wrap(err, "failed to copy to memfd")
	}

	// Reset position to beginning
	if _, err := file.Seek(0, 0); err != nil {
		file.Close()
		return nil, errors.Wrap(err, "failed to seek memfd")
	}

	return file, nil
}

// KexecLoadFromAssets loads kernel via kexec_file_load syscall from BootAssets.
func KexecLoadFromAssets(assets *types.BootAssets, extraCmdline string) error {
	log.Printf("using KexecFileLoad")

	// Create memfd for kernel from reader
	kernelFile, err := CreateMemfdFromReader("kernel", assets.Kernel)
	if err != nil {
		return errors.Wrap(err, "failed to create kernel memfd")
	}
	defer kernelFile.Close()

	// Create memfd for initramfs from reader
	initrdFile, err := CreateMemfdFromReader("initramfs", assets.Initrd)
	if err != nil {
		return errors.Wrap(err, "failed to create initramfs memfd")
	}
	defer initrdFile.Close()
	initrdFD := int(initrdFile.Fd())

	// Combine cmdline from assets with additional arguments
	cmdlineParts := []string{}
	if assets.Cmdline != "" {
		cmdlineParts = append(cmdlineParts, assets.Cmdline)
	}
	if extraCmdline != "" {
		cmdlineParts = append(cmdlineParts, extraCmdline)
	}
	cmdline := strings.Join(cmdlineParts, " ")

	log.Printf("cmdline: %s", cmdline)

	// Call kexec_file_load via syscall
	// long kexec_file_load(int kernel_fd, int initrd_fd, unsigned long cmdline_len, const char *cmdline, unsigned long flags)
	// KEXEC_FILE_LOAD_UNSAFE = 0x00000001 - skip signature verification (if lockdown is not enabled)
	const KEXEC_FILE_LOAD_UNSAFE = 0x00000001

	cmdlineBytes := []byte(cmdline)
	if len(cmdlineBytes) > 0 {
		cmdlineBytes = append(cmdlineBytes, 0) // null terminator
	}

	var cmdlinePtr uintptr
	if len(cmdlineBytes) > 0 {
		cmdlinePtr = uintptr(unsafe.Pointer(&cmdlineBytes[0]))
	}

	// Try first without flags (requires signed kernel)
	var flags uintptr = 0
	_, _, errno := unix.Syscall6(
		sysKexecFileLoad,
		uintptr(kernelFile.Fd()),   // kernel_fd
		uintptr(initrdFD),          // initrd_fd (-1 if none)
		uintptr(len(cmdlineBytes)), // cmdline_len
		cmdlinePtr,                 // cmdline
		flags,                      // flags
		0,                          // unused
	)

	// If we got EPERM and it's not due to sysctl, try with flag to skip signature verification
	if errno == unix.EPERM {
		log.Printf("kexec_file_load failed with EPERM, trying with KEXEC_FILE_LOAD_UNSAFE flag (may require lockdown=off)")
		flags = KEXEC_FILE_LOAD_UNSAFE
		_, _, errno = unix.Syscall6(
			sysKexecFileLoad,
			uintptr(kernelFile.Fd()),   // kernel_fd
			uintptr(initrdFD),          // initrd_fd (-1 if none)
			uintptr(len(cmdlineBytes)), // cmdline_len
			cmdlinePtr,                 // cmdline
			flags,                      // flags
			0,                          // unused
		)
	}

	if errno != 0 {
		return handleKexecError(errno)
	}

	log.Printf("kexec loaded successfully, rebooting...")

	// Call reboot with LINUX_REBOOT_CMD_KEXEC
	const LINUX_REBOOT_CMD_KEXEC = 0x45584543
	const LINUX_REBOOT_MAGIC1 = 0xfee1dead
	const LINUX_REBOOT_MAGIC2 = 672274793
	_, _, errno2 := unix.Syscall6(
		sysReboot, // arch-specific syscall number
		LINUX_REBOOT_MAGIC1,    // magic1
		LINUX_REBOOT_MAGIC2,    // magic2
		LINUX_REBOOT_CMD_KEXEC, // cmd
		0,                      // arg (unused)
		0,                      // unused
		0,                      // unused
	)
	if errno2 != 0 {
		return errors.Newf("reboot with kexec failed: %v", errno2)
	}

	// Code should not reach here, as reboot restarts the system
	return nil
}

// handleKexecError translates errno to descriptive error message.
func handleKexecError(errno syscall.Errno) error {
	switch errno { //nolint:exhaustive
	case unix.ENOSYS:
		return errors.New("kexec support is disabled in the kernel (CONFIG_KEXEC not enabled)")
	case unix.EPERM:
		// EPERM can mean several things:
		// 1. sysctl is disabled
		// 2. lockdown mode is enabled (caused by Secure Boot)
		// 3. kernel signature is required
		lockdownData, _ := os.ReadFile("/sys/kernel/security/lockdown")
		lockdown := strings.TrimSpace(string(lockdownData))
		if strings.Contains(lockdown, "[confidentiality]") || strings.Contains(lockdown, "[integrity]") {
			sbHint := ""
			if sbState, err := efi.GetSecureBootState(); err == nil && sbState.Enabled {
				sbHint = "\n  Note: Secure Boot is enabled, which activates kernel lockdown"
			}
			return errors.Newf("kexec blocked: kernel is in lockdown mode (%s).%s\nSolutions:\n  1. Disable Secure Boot in BIOS/UEFI settings\n  2. Boot with 'lockdown=none' kernel parameter", lockdown, sbHint)
		}
		sysctlData, _ := os.ReadFile("/proc/sys/kernel/kexec_load_disabled")
		if strings.TrimSpace(string(sysctlData)) == "1" {
			return errors.New("kexec is disabled via sysctl. Run: sudo sysctl -w kernel.kexec_load_disabled=0")
		}
		return errors.New("kexec blocked: permission denied. Possible causes:\n  1. Kernel requires signed image (try booting with 'lockdown=none')\n  2. Secure Boot is enabled\n  3. Check /proc/sys/kernel/kexec_load_disabled")
	case unix.EBUSY:
		return errors.New("kexec is busy (another kexec may be in progress)")
	case syscall.Errno(129): // EKEYREJECTED = 129
		return errors.New("kernel signature verification failed (unsigned kernel with lockdown enabled)")
	case syscall.Errno(95): // ENOTSUP = 95
		return errors.New("kexec_file_load not supported (old kernel or missing CONFIG_KEXEC_FILE)")
	default:
		return errors.Newf("error loading kernel for kexec: %v (errno: %d). Check dmesg for details", errno, errno)
	}
}

// RunBootMode executes boot mode: shows summary, asks confirmation, loads kernel via kexec.
//
//nolint:forbidigo
func RunBootMode(source types.ImageSource, extraArgs []string) {
	// First show summary and ask for confirmation
	fmt.Println("\nBoot Summary:")
	fmt.Printf("  Image: %s\n", source.Reference())
	fmt.Printf("  Extra kernel args: %s\n",
		func() string {
			if len(extraArgs) == 0 {
				return "(none)"
			}
			return strings.Join(extraArgs, " ")
		}())
	fmt.Println()

	if !cli.AskYesNo("Continue with boot?", true) {
		log.Fatal("aborted by user")
	}
	fmt.Println()

	// Get boot assets from image source
	log.Printf("boot mode: extracting kernel and initramfs from image")

	assets, err := source.GetBootAssets()
	cli.Must("get boot assets", err)
	defer assets.Close()

	// Collect additional kernel arguments into a string
	extraCmdline := strings.Join(extraArgs, " ")

	log.Print("loading kernel with kexec")
	cli.Must("kexec", KexecLoadFromAssets(assets, extraCmdline))
}
