package main

import (
	"archive/tar"
	"debug/pe"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/google/go-containerregistry/pkg/crane"
	"golang.org/x/sys/unix"
)

/* -------------------- UKI extraction helpers ------------------------------- */

// UKIAssetInfo contains kernel, initrd and cmdline from UKI file
type UKIAssetInfo struct {
	io.Closer
	Kernel  io.Reader
	Initrd  io.Reader
	Cmdline io.Reader
}

// ExtractUKI extracts kernel, initrd and cmdline from UKI file
func ExtractUKI(ukiPath string) (*UKIAssetInfo, error) {
	peFile, err := pe.Open(ukiPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open PE file: %w", err)
	}

	assetInfo := &UKIAssetInfo{
		Closer: peFile,
	}

	sectionMap := map[string]*io.Reader{
		".initrd":  &assetInfo.Initrd,
		".cmdline": &assetInfo.Cmdline,
		".linux":   &assetInfo.Kernel,
	}

	for _, section := range peFile.Sections {
		// Remove null bytes from section name
		sectionName := ""
		for _, b := range section.Name {
			if b != 0 {
				sectionName += string(b)
			}
		}

		if reader, exists := sectionMap[sectionName]; exists && *reader == nil {
			// Use VirtualSize instead of Size to exclude alignment
			*reader = io.LimitReader(section.Open(), int64(section.VirtualSize))
		}
	}

	// Check that all required sections are found
	for name, reader := range sectionMap {
		if *reader == nil {
			peFile.Close()
			return nil, fmt.Errorf("%s not found in PE file", name)
		}
	}

	return assetInfo, nil
}

/* -------------------- kexec helpers ----------------------------------------- */

// createMemfdFromReader creates an anonymous file in memory via memfd_create and copies data from reader
func createMemfdFromReader(name string, reader io.Reader) (*os.File, error) {
	// SYS_MEMFD_CREATE = 319 on x86_64
	// int memfd_create(const char *name, unsigned int flags);
	const SYS_MEMFD_CREATE = 319
	const MFD_CLOEXEC = 0x0001

	nameBytes := []byte(name + "\x00")
	fd, _, errno := unix.Syscall(SYS_MEMFD_CREATE, uintptr(unsafe.Pointer(&nameBytes[0])), MFD_CLOEXEC, 0)
	if errno != 0 {
		return nil, fmt.Errorf("memfd_create failed: %v", errno)
	}

	file := os.NewFile(fd, name)
	if file == nil {
		return nil, fmt.Errorf("failed to create file from fd")
	}

	// Copy data from reader to memfd
	if _, err := io.Copy(file, reader); err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to copy to memfd: %w", err)
	}

	// Reset position to beginning
	if _, err := file.Seek(0, 0); err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to seek memfd: %w", err)
	}

	return file, nil
}

// kexec_segment structure for kexec_load syscall
type kexecSegment struct {
	buf   uintptr
	bufsz uint64
	mem   uint64
	memsz uint64
}

func kexecLoadFromUKI(ukiPath string, extraCmdline string) error {
	// Use old kexec_load API - it doesn't require signature verification
	log.Printf("using kexec_load (legacy API without signature verification)")

	// Extract assets from UKI
	assets, err := ExtractUKI(ukiPath)
	if err != nil {
		return fmt.Errorf("failed to extract UKI: %w", err)
	}
	defer assets.Close()

	// Read kernel into memory
	kernelData, err := io.ReadAll(assets.Kernel)
	if err != nil {
		return fmt.Errorf("failed to read kernel: %w", err)
	}
	log.Printf("kernel size: %d bytes", len(kernelData))

	// Read initramfs into memory
	initrdData, err := io.ReadAll(assets.Initrd)
	if err != nil {
		return fmt.Errorf("failed to read initramfs: %w", err)
	}
	log.Printf("initramfs size: %d bytes", len(initrdData))

	// Read cmdline from UKI
	ukiCmdlineBytes, err := io.ReadAll(assets.Cmdline)
	if err != nil {
		return fmt.Errorf("failed to read cmdline from UKI: %w", err)
	}
	ukiCmdline := strings.TrimRight(string(ukiCmdlineBytes), "\x00")
	ukiCmdline = strings.TrimSpace(ukiCmdline)

	// Combine cmdline from UKI with additional arguments
	cmdlineParts := []string{}
	if ukiCmdline != "" {
		cmdlineParts = append(cmdlineParts, ukiCmdline)
	}
	if extraCmdline != "" {
		cmdlineParts = append(cmdlineParts, extraCmdline)
	}
	cmdline := strings.Join(cmdlineParts, " ") + "\x00" // null terminator
	cmdlineBytes := []byte(cmdline)

	log.Printf("cmdline: %s", strings.TrimRight(cmdline, "\x00"))

	// Prepare kexec segments
	// For x86_64, we use a simplified approach:
	// - Load entire bzImage at 2MB boundary (safe for modern systems)
	// - Load initrd at a high address (below 4GB boundary)
	// - Use zero entry point to let kernel auto-detect
	
	const KERNEL_LOAD_ADDR = 0x200000      // 2MB - safe alignment for modern kernels
	const INITRD_LOAD_ADDR = 0x7f000000    // ~2GB - safe for initrd
	const CMDLINE_LOAD_ADDR = 0x20000      // 128KB - traditional cmdline location

	segments := []kexecSegment{
		// Kernel image (entire bzImage)
		{
			buf:   uintptr(unsafe.Pointer(&kernelData[0])),
			bufsz: uint64(len(kernelData)),
			mem:   KERNEL_LOAD_ADDR,
			memsz: uint64(len(kernelData)),
		},
		// Initramfs
		{
			buf:   uintptr(unsafe.Pointer(&initrdData[0])),
			bufsz: uint64(len(initrdData)),
			mem:   INITRD_LOAD_ADDR,
			memsz: uint64(len(initrdData)),
		},
		// Command line
		{
			buf:   uintptr(unsafe.Pointer(&cmdlineBytes[0])),
			bufsz: uint64(len(cmdlineBytes)),
			mem:   CMDLINE_LOAD_ADDR,
			memsz: uint64(len(cmdlineBytes)),
		},
	}

	// Call kexec_load via syscall
	// SYS_KEXEC_LOAD = 246 on x86_64, 283 on ARM64
	// long kexec_load(unsigned long entry, unsigned long nr_segments, struct kexec_segment *segments, unsigned long flags)
	const SYS_KEXEC_LOAD = 246
	const KEXEC_ARCH_X86_64 = 62 << 16  // KEXEC_ARCH_X86_64

	// Entry point 0 means auto-detect from kernel header
	var entry uintptr = 0
	var flags uintptr = KEXEC_ARCH_X86_64

	_, _, errno := unix.Syscall6(
		SYS_KEXEC_LOAD,
		entry,                                 // entry (0 = auto-detect)
		uintptr(len(segments)),                // nr_segments
		uintptr(unsafe.Pointer(&segments[0])), // segments
		flags,                                 // flags
		0,                                     // unused
		0,                                     // unused
	)

	if errno != 0 {
		switch errno {
		case unix.ENOSYS:
			return fmt.Errorf("kexec support is disabled in the kernel (CONFIG_KEXEC not enabled)")
		case unix.EPERM:
			// EPERM can mean several things:
			// 1. sysctl is disabled
			// 2. lockdown mode is enabled
			lockdownData, _ := os.ReadFile("/sys/kernel/security/lockdown")
			lockdown := strings.TrimSpace(string(lockdownData))
			if strings.Contains(lockdown, "[confidentiality]") || strings.Contains(lockdown, "[integrity]") {
				return fmt.Errorf("kexec blocked: kernel is in lockdown mode (%s). Solutions:\n  1. Boot with 'lockdown=none' kernel parameter\n  2. Disable Secure Boot", lockdown)
			}
			sysctlData, _ := os.ReadFile("/proc/sys/kernel/kexec_load_disabled")
			if strings.TrimSpace(string(sysctlData)) == "1" {
				return fmt.Errorf("kexec is disabled via sysctl. Run: sudo sysctl -w kernel.kexec_load_disabled=0")
			}
			return fmt.Errorf("kexec blocked: permission denied. Check /proc/sys/kernel/kexec_load_disabled")
		case unix.EBUSY:
			return fmt.Errorf("kexec is busy (another kexec may be in progress)")
		case unix.EINVAL:
			return fmt.Errorf("invalid kexec parameters. Check dmesg for details (may need different load addresses or kernel format)")
		default:
			return fmt.Errorf("error loading kernel for kexec: %v (errno: %d). Check dmesg for details", errno, errno)
		}
	}

	log.Printf("kexec loaded successfully, rebooting...")

	// Call reboot with LINUX_REBOOT_CMD_KEXEC
	const LINUX_REBOOT_CMD_KEXEC = 0x45584543
	const LINUX_REBOOT_MAGIC1 = 0xfee1dead
	const LINUX_REBOOT_MAGIC2 = 672274793
	const SYS_REBOOT = 169
	_, _, errno2 := unix.Syscall6(
		SYS_REBOOT,
		LINUX_REBOOT_MAGIC1,    // magic1
		LINUX_REBOOT_MAGIC2,    // magic2
		LINUX_REBOOT_CMD_KEXEC, // cmd
		0,                      // arg (unused)
		0,                      // unused
		0,                      // unused
	)
	if errno2 != 0 {
		return fmt.Errorf("reboot with kexec failed: %v", errno2)
	}

	// Code should not reach here, as reboot restarts the system
	return nil
}

/* -------------------- boot mode ------------------------------------------- */

func runBootMode(image string, extra multiFlag) {
	// First show summary and ask for confirmation
	// (without loading the image)
	fmt.Println("\nBoot Summary:")
	fmt.Printf("  Image: %s\n", image)
	fmt.Printf("  Extra kernel args: %s\n",
		func() string {
			if len(extra) == 0 {
				return "(none)"
			}
			return strings.Join(extra, " ")
		}())
	fmt.Println()

	if !askYesNo("Continue with boot?", true) {
		log.Fatal("aborted by user")
	}
	fmt.Println()

	// Only after confirmation start loading the image
	log.Printf("boot mode: extracting kernel and initramfs from image")

	tmpDir, _ := os.MkdirTemp("", "boot-*")
	log.Printf("created temporary directory %s", tmpDir)
	defer os.RemoveAll(tmpDir)

	transport := setupTransportWithProxy()
	opts := crane.WithTransport(transport)

	log.Printf("pulling image %s", image)
	img, err := crane.Pull(image, opts)
	must("pull image", err)

	log.Print("extracting image layers")
	instDir := filepath.Join(tmpDir, "installer")
	os.MkdirAll(instDir, 0o755)

	var ukiPath string
	layers, _ := img.Layers()
	for _, l := range layers {
		r, _ := l.Uncompressed()
		defer r.Close()
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			must("tar", err)
			if strings.HasPrefix(filepath.Base(h.Name), ".wh.") {
				continue
			}
			target := filepath.Join(instDir, h.Name)
			name := strings.ToLower(h.Name)
			// Look for UKI kernel
			if strings.Contains(name, "install") && strings.Contains(name, "vmlinuz.efi") {
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					log.Fatalf("failed to create directory for UKI kernel: %v (check available disk space)", err)
				}
				f, err := os.Create(target)
				if err != nil {
					log.Fatalf("failed to create UKI kernel file: %v (check available disk space)", err)
				}
				if _, err := io.Copy(f, tr); err != nil {
					f.Close()
					os.Remove(target)
					log.Fatalf("failed to extract UKI kernel: %v (check available disk space)", err)
				}
				if err := f.Close(); err != nil {
					os.Remove(target)
					log.Fatalf("failed to close UKI kernel file: %v", err)
				}
				if err := os.Chmod(target, os.FileMode(h.Mode)); err != nil {
					log.Fatalf("failed to set permissions on UKI kernel file: %v", err)
				}
				ukiPath = target
			}
		}
	}

	if ukiPath == "" {
		log.Fatal("UKI kernel (vmlinuz.efi) not found in image")
	}

	log.Printf("found UKI kernel: %s", ukiPath)

	// Collect additional kernel arguments into a string
	extraCmdline := strings.Join(extra, " ")

	log.Print("loading kernel with kexec from UKI")
	must("kexec", kexecLoadFromUKI(ukiPath, extraCmdline))
}

