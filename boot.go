package main

import (
	"archive/tar"
	"debug/pe"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

/* -------------------- kernel module loading --------------------------------- */

// findModuleInLibModules searches for a module file in /lib/modules directory
// Returns path to module file (may be .ko, .ko.zst, .ko.xz, .ko.gz)
func findModuleInLibModules(moduleName string) (string, error) {
	libModulesDir := "/lib/modules"
	
	// Check if /lib/modules exists
	if _, err := os.Stat(libModulesDir); os.IsNotExist(err) {
		return "", fmt.Errorf("/lib/modules directory not found")
	}
	
	// Try different compression formats
	possibleNames := []string{
		moduleName + ".ko",
		moduleName + ".ko.zst",
		moduleName + ".ko.xz",
		moduleName + ".ko.gz",
	}
	
	// Recursively search for the module file
	var foundPath string
	err := filepath.Walk(libModulesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors, continue searching
		}
		if !info.IsDir() {
			for _, name := range possibleNames {
				if info.Name() == name {
					foundPath = path
					return filepath.SkipAll // Found it, stop searching
				}
			}
		}
		return nil
	})
	
	if err != nil && foundPath == "" {
		return "", fmt.Errorf("error searching for module: %w", err)
	}
	
	if foundPath == "" {
		return "", fmt.Errorf("module %s (or compressed variants) not found in /lib/modules", moduleName)
	}
	
	return foundPath, nil
}

// loadKernelModule loads a kernel module by name using finit_module syscall
func loadKernelModule(moduleName string) error {
	// Check if module is already loaded
	if data, err := os.ReadFile("/proc/modules"); err == nil {
		modulePattern := moduleName + " "
		if strings.Contains(string(data), modulePattern) {
			log.Printf("module %s is already loaded", moduleName)
			return nil
		}
	}

	// Find module in /lib/modules
	modulePath, err := findModuleInLibModules(moduleName)
	if err != nil {
		return fmt.Errorf("failed to find module: %w", err)
	}

	// Check if module is compressed and needs decompression
	needsDecompress := strings.HasSuffix(modulePath, ".zst") || 
		strings.HasSuffix(modulePath, ".xz") || 
		strings.HasSuffix(modulePath, ".gz")
	
	var moduleFile *os.File
	if needsDecompress {
		// Decompress module to a temporary file
		tmpModulePath := modulePath + ".decompressed"
		defer os.Remove(tmpModulePath) // Clean up temporary file
		
		if strings.HasSuffix(modulePath, ".zst") {
			zstdCmd := exec.Command("zstd", "-dc", modulePath)
			outFile, err := os.Create(tmpModulePath)
			if err != nil {
				return fmt.Errorf("failed to create temp file: %w", err)
			}
			zstdCmd.Stdout = outFile
			zstdCmd.Stderr = os.Stderr
			if err := zstdCmd.Run(); err != nil {
				outFile.Close()
				return fmt.Errorf("failed to decompress module: %w", err)
			}
			outFile.Close()
		} else {
			return fmt.Errorf("unsupported compression format for module %s", modulePath)
		}
		
		moduleFile, err = os.Open(tmpModulePath)
		if err != nil {
			return fmt.Errorf("failed to open decompressed module file: %w", err)
		}
	} else {
		moduleFile, err = os.Open(modulePath)
		if err != nil {
			return fmt.Errorf("failed to open module file %s: %w", modulePath, err)
		}
	}
	defer moduleFile.Close()

	// Use finit_module syscall
	// SYS_FINIT_MODULE = 313 on x86_64
	// long finit_module(int fd, const char *param_values, int flags);
	const SYS_FINIT_MODULE = 313
	var flags uintptr = 0

	_, _, errno := unix.Syscall(
		SYS_FINIT_MODULE,
		uintptr(moduleFile.Fd()),
		0, // param_values (empty string pointer, use "" for no params)
		flags,
	)

	if errno != 0 {
		// Check if module is already loaded
		if errno == unix.EEXIST || errno == unix.EBUSY {
			return nil // Module already loaded, that's fine
		}
		return fmt.Errorf("finit_module failed: %v", errno)
	}

	log.Printf("successfully loaded module %s from %s", moduleName, modulePath)
	return nil
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

func kexecLoadFromUKI(ukiPath string, extraCmdline string) error {
	// Use KexecFileLoad - it's simpler and more correct
	log.Printf("using KexecFileLoad")

	// Extract assets from UKI
	assets, err := ExtractUKI(ukiPath)
	if err != nil {
		return fmt.Errorf("failed to extract UKI: %w", err)
	}
	defer assets.Close()

	// Create memfd for kernel from reader
	kernelFile, err := createMemfdFromReader("kernel", assets.Kernel)
	if err != nil {
		return fmt.Errorf("failed to create kernel memfd: %w", err)
	}
	defer kernelFile.Close()

	// Create memfd for initramfs from reader
	initrdFile, err := createMemfdFromReader("initramfs", assets.Initrd)
	if err != nil {
		return fmt.Errorf("failed to create initramfs memfd: %w", err)
	}
	defer initrdFile.Close()
	initrdFD := int(initrdFile.Fd())

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
	cmdline := strings.Join(cmdlineParts, " ")

	log.Printf("cmdline: %s", cmdline)

	// Call kexec_file_load via syscall
	// SYS_KEXEC_FILE_LOAD = 320 on x86_64
	// long kexec_file_load(int kernel_fd, int initrd_fd, unsigned long cmdline_len, const char *cmdline, unsigned long flags)
	const SYS_KEXEC_FILE_LOAD = 320
	// KEXEC_FILE_LOAD_UNSAFE = 0x00000001 - skip signature verification (if lockdown is not enabled)
	// KEXEC_FILE_LOAD_NO_VERIFY_SIG = 0x00000002 - skip signature verification
	const KEXEC_FILE_LOAD_UNSAFE = 0x00000001
	const KEXEC_FILE_LOAD_NO_VERIFY_SIG = 0x00000002

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
		SYS_KEXEC_FILE_LOAD,
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
			SYS_KEXEC_FILE_LOAD,
			uintptr(kernelFile.Fd()),   // kernel_fd
			uintptr(initrdFD),          // initrd_fd (-1 if none)
			uintptr(len(cmdlineBytes)), // cmdline_len
			cmdlinePtr,                 // cmdline
			flags,                      // flags
			0,                          // unused
		)
	}

	if errno != 0 {
		switch errno {
		case unix.ENOSYS:
			return fmt.Errorf("kexec support is disabled in the kernel (CONFIG_KEXEC not enabled)")
		case unix.EPERM:
			// EPERM can mean several things:
			// 1. sysctl is disabled
			// 2. lockdown mode is enabled
			// 3. kernel signature is required
			lockdownData, _ := os.ReadFile("/sys/kernel/security/lockdown")
			lockdown := strings.TrimSpace(string(lockdownData))
			if strings.Contains(lockdown, "[confidentiality]") || strings.Contains(lockdown, "[integrity]") {
				return fmt.Errorf("kexec blocked: kernel is in lockdown mode (%s). Solutions:\n  1. Boot with 'lockdown=none' kernel parameter\n  2. Use signed UKI kernel\n  3. Disable Secure Boot", lockdown)
			}
			sysctlData, _ := os.ReadFile("/proc/sys/kernel/kexec_load_disabled")
			if strings.TrimSpace(string(sysctlData)) == "1" {
				return fmt.Errorf("kexec is disabled via sysctl. Run: sudo sysctl -w kernel.kexec_load_disabled=0")
			}
			return fmt.Errorf("kexec blocked: permission denied. Possible causes:\n  1. Kernel requires signed image (try booting with 'lockdown=none')\n  2. Secure Boot is enabled\n  3. Check /proc/sys/kernel/kexec_load_disabled")
		case unix.EBUSY:
			return fmt.Errorf("kexec is busy (another kexec may be in progress)")
		case syscall.Errno(129): // EKEYREJECTED = 129
			return fmt.Errorf("kernel signature verification failed (unsigned kernel with lockdown enabled)")
		case syscall.Errno(95): // ENOTSUP = 95
			return fmt.Errorf("kexec_file_load not supported (old kernel or missing CONFIG_KEXEC_FILE)")
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

/* -------------------- kexec with kernel and initrd ------------------------ */

func kexecLoadWithInitrd(kernelPath string, initrdPath string, extraCmdline string) error {
	// Use KexecFileLoad
	log.Printf("using KexecFileLoad with kernel: %s, initrd: %s", kernelPath, initrdPath)

	// Open kernel file
	kernelFile, err := os.Open(kernelPath)
	if err != nil {
		return fmt.Errorf("failed to open kernel: %w", err)
	}
	defer kernelFile.Close()

	// Open initrd file
	initrdFile, err := os.Open(initrdPath)
	if err != nil {
		return fmt.Errorf("failed to open initrd: %w", err)
	}
	defer initrdFile.Close()
	initrdFD := int(initrdFile.Fd())

	// Read current cmdline from /proc/cmdline
	currentCmdlineBytes, _ := os.ReadFile("/proc/cmdline")
	currentCmdline := strings.TrimSpace(string(currentCmdlineBytes))

	// Combine cmdlines
	cmdlineParts := []string{}
	if currentCmdline != "" {
		cmdlineParts = append(cmdlineParts, currentCmdline)
	}
	// Add talos.platform=metal if not already present
	if !strings.Contains(currentCmdline, "talos.platform=") && !strings.Contains(extraCmdline, "talos.platform=") {
		cmdlineParts = append(cmdlineParts, "talos.platform=metal")
	}
	if extraCmdline != "" {
		cmdlineParts = append(cmdlineParts, extraCmdline)
	}
	cmdline := strings.Join(cmdlineParts, " ")

	log.Printf("cmdline: %s", cmdline)

	// Call kexec_file_load via syscall
	const SYS_KEXEC_FILE_LOAD = 320
	const KEXEC_FILE_LOAD_UNSAFE = 0x00000001
	const KEXEC_FILE_LOAD_NO_VERIFY_SIG = 0x00000002

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
		SYS_KEXEC_FILE_LOAD,
		uintptr(kernelFile.Fd()),   // kernel_fd
		uintptr(initrdFD),          // initrd_fd
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
			SYS_KEXEC_FILE_LOAD,
			uintptr(kernelFile.Fd()),   // kernel_fd
			uintptr(initrdFD),          // initrd_fd
			uintptr(len(cmdlineBytes)), // cmdline_len
			cmdlinePtr,                 // cmdline
			flags,                      // flags
			0,                          // unused
		)
	}

	if errno != 0 {
		switch errno {
		case unix.ENOSYS:
			return fmt.Errorf("kexec support is disabled in the kernel (CONFIG_KEXEC not enabled)")
		case unix.EPERM:
			lockdownData, _ := os.ReadFile("/sys/kernel/security/lockdown")
			lockdown := strings.TrimSpace(string(lockdownData))
			if strings.Contains(lockdown, "[confidentiality]") || strings.Contains(lockdown, "[integrity]") {
				return fmt.Errorf("kexec blocked: kernel is in lockdown mode (%s). Solutions:\n  1. Boot with 'lockdown=none' kernel parameter\n  2. Use signed kernel\n  3. Disable Secure Boot", lockdown)
			}
			sysctlData, _ := os.ReadFile("/proc/sys/kernel/kexec_load_disabled")
			if strings.TrimSpace(string(sysctlData)) == "1" {
				return fmt.Errorf("kexec is disabled via sysctl. Run: sudo sysctl -w kernel.kexec_load_disabled=0")
			}
			return fmt.Errorf("kexec blocked: permission denied. Possible causes:\n  1. Kernel requires signed image (try booting with 'lockdown=none')\n  2. Secure Boot is enabled\n  3. Check /proc/sys/kernel/kexec_load_disabled")
		case unix.EBUSY:
			return fmt.Errorf("kexec is busy (another kexec may be in progress)")
		case syscall.Errno(129): // EKEYREJECTED = 129
			return fmt.Errorf("kernel signature verification failed (unsigned kernel with lockdown enabled)")
		case syscall.Errno(95): // ENOTSUP = 95
			return fmt.Errorf("kexec_file_load not supported (old kernel or missing CONFIG_KEXEC_FILE)")
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

/* -------------------- initramfs mode ------------------------------------------- */

// checkRequiredSyscalls checks if required syscalls are available for initramfs mode
func checkRequiredSyscalls() error {
	var warnings []string
	
	// Check for /proc/modules (indicates kernel module support, needed for finit_module)
	if _, err := os.Stat("/proc/modules"); os.IsNotExist(err) {
		warnings = append(warnings, "/proc/modules not available (module loading may not work)")
	}
	
	// Check for kexec_file_load support
	if _, err := os.Stat("/sys/kernel/kexec_load_disabled"); os.IsNotExist(err) {
		warnings = append(warnings, "/sys/kernel/kexec_load_disabled not found (kexec support may not be available)")
	}
	
	// Check for /proc/mounts (needed for mount operations that Talos init will perform)
	if _, err := os.Stat("/proc/mounts"); os.IsNotExist(err) {
		warnings = append(warnings, "/proc/mounts not available (mount operations may not work)")
	}
	
	// Check if we can read /proc/self (basic proc filesystem check)
	if _, err := os.Stat("/proc/self"); os.IsNotExist(err) {
		warnings = append(warnings, "/proc/self not available (proc filesystem may not be mounted)")
	}
	
	// Note: We can't easily test open_tree, move_mount, mount_setattr syscalls without actual mount operations.
	// These syscalls are used by Talos init (from mount/v3 package) and will fail at runtime if the kernel
	// doesn't support them. Older kernels (< 5.2) may not have open_tree/move_mount support.
	// The Talos initramfs expects these syscalls to be available.
	
	if len(warnings) > 0 {
		log.Printf("warning: syscall availability checks found issues:")
		for _, w := range warnings {
			log.Printf("  - %s", w)
		}
		log.Printf("  Note: Talos init requires open_tree, move_mount, mount_setattr syscalls (kernel >= 5.2)")
		log.Printf("  If these are not available, Talos init may fail to mount filesystems")
		return fmt.Errorf("some required syscalls/filesystems may not be available")
	}
	
	return nil
}

func runInitramfsMode(extra multiFlag) {
	// Check required syscalls availability
	if err := checkRequiredSyscalls(); err != nil {
		log.Printf("warning: syscall check failed: %v (continuing anyway)", err)
	}
	
	// First show summary and ask for confirmation
	fmt.Println("\nInitramfs Boot Summary:")
	fmt.Printf("  Will use current kernel with Talos initramfs\n")
	fmt.Printf("  Extra kernel args: %s\n",
		func() string {
			if len(extra) == 0 {
				return "(none)"
			}
			return strings.Join(extra, " ")
		}())
	fmt.Println()

	if !askYesNo("Continue with initramfs boot?", true) {
		log.Fatal("aborted by user")
	}
	fmt.Println()

	tmpDir, _ := os.MkdirTemp("", "initramfs-*")
	log.Printf("created temporary directory %s", tmpDir)
	defer os.RemoveAll(tmpDir)

	// Get current kernel version
	log.Print("getting current kernel version")
	unameCmd := exec.Command("uname", "-r")
	unameOutput, err := unameCmd.Output()
	must("uname", err)
	kernelVersion := strings.TrimSpace(string(unameOutput))
	log.Printf("current kernel version: %s", kernelVersion)

	// Find current kernel path
	kernelPath := fmt.Sprintf("/boot/vmlinuz-%s", kernelVersion)
	if _, err := os.Stat(kernelPath); os.IsNotExist(err) {
		// Try alternative locations
		altPaths := []string{
			fmt.Sprintf("/boot/vmlinux-%s", kernelVersion),
			"/boot/vmlinuz",
			"/boot/vmlinux",
			"/vmlinuz",
		}
		found := false
		for _, alt := range altPaths {
			if _, err := os.Stat(alt); err == nil {
				kernelPath = alt
				found = true
				break
			}
		}
		if !found {
			log.Fatalf("kernel not found (tried %s and alternatives)", kernelPath)
		}
	}
	log.Printf("using kernel: %s", kernelPath)

	// Check for required tools
	requiredTools := []string{"zstd", "cpio", "unsquashfs", "mksquashfs"}
	for _, tool := range requiredTools {
		if _, err := exec.LookPath(tool); err != nil {
			log.Fatalf("required tool not found: %s (please install it)", tool)
		}
	}

	// Download initramfs
	initramfsURL := "https://github.com/siderolabs/talos/releases/download/v1.12.0/initramfs-amd64.xz"
	log.Printf("downloading initramfs from %s", initramfsURL)
	initramfsCompressedPath := filepath.Join(tmpDir, "initramfs-compressed")
	resp, err := http.Get(initramfsURL)
	must("download initramfs", err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("failed to download initramfs: HTTP %d", resp.StatusCode)
	}
	initramfsCompressedFile, err := os.Create(initramfsCompressedPath)
	must("create initramfs compressed file", err)
	_, err = io.Copy(initramfsCompressedFile, resp.Body)
	initramfsCompressedFile.Close()
	must("write initramfs compressed file", err)

	// Decompress (actually zstd, despite .xz extension)
	log.Print("decompressing initramfs (zstd format)")
	initramfsPath := filepath.Join(tmpDir, "initramfs")
	zstdCmd := exec.Command("zstd", "-dc", initramfsCompressedPath)
	zstdOutput, err := os.Create(initramfsPath)
	must("create initramfs file", err)
	zstdCmd.Stdout = zstdOutput
	zstdCmd.Stderr = os.Stderr
	must("zstd decompress", zstdCmd.Run())
	zstdOutput.Close()

	// Extract cpio
	log.Print("extracting cpio archive")
	cpioDir := filepath.Join(tmpDir, "cpio")
	os.MkdirAll(cpioDir, 0o755)
	cpioCmd := exec.Command("cpio", "-id")
	cpioInput, err := os.Open(initramfsPath)
	must("open initramfs", err)
	cpioCmd.Stdin = cpioInput
	cpioCmd.Dir = cpioDir
	cpioCmd.Stderr = os.Stderr
	must("cpio extract", cpioCmd.Run())
	cpioInput.Close()

	// Find and extract rootfs.sqsh
	rootfsSqshPath := filepath.Join(cpioDir, "rootfs.sqsh")
	if _, err := os.Stat(rootfsSqshPath); os.IsNotExist(err) {
		log.Fatalf("rootfs.sqsh not found in initramfs (expected at %s)", rootfsSqshPath)
	}

	// Extract squashfs
	log.Print("extracting rootfs.sqsh (squashfs)")
	squashfsDir := filepath.Join(tmpDir, "squashfs")
	unsquashfsCmd := exec.Command("unsquashfs", "-d", squashfsDir, rootfsSqshPath)
	unsquashfsCmd.Stderr = os.Stderr
	must("unsquashfs extract", unsquashfsCmd.Run())

	// Replace kernel modules
	log.Print("replacing kernel modules")
	oldModulesPath := filepath.Join(squashfsDir, "lib", "modules", "6.18.1-talos")
	newModulesPath := filepath.Join(squashfsDir, "lib", "modules", kernelVersion)
	currentModulesPath := fmt.Sprintf("/lib/modules/%s", kernelVersion)

	if _, err := os.Stat(currentModulesPath); os.IsNotExist(err) {
		log.Fatalf("current kernel modules not found at %s", currentModulesPath)
	}

	// Remove old modules directory if it exists
	if _, err := os.Stat(oldModulesPath); err == nil {
		must("remove old modules", os.RemoveAll(oldModulesPath))
	}

	// Create lib/modules directory if it doesn't exist
	libModulesDir := filepath.Join(squashfsDir, "lib", "modules")
	os.MkdirAll(libModulesDir, 0o755)

	// Copy current kernel modules and decompress if needed
	log.Printf("copying modules from %s to %s", currentModulesPath, newModulesPath)
	os.MkdirAll(newModulesPath, 0o755)
	
	// Copy all module files, decompressing .zst files if needed
	err = filepath.Walk(currentModulesPath, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if info.IsDir() {
			return nil
		}
		
		relPath, err := filepath.Rel(currentModulesPath, srcPath)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(newModulesPath, relPath)
		os.MkdirAll(filepath.Dir(dstPath), 0o755)
		
		// If it's a .zst file, decompress it to .ko
		if strings.HasSuffix(srcPath, ".ko.zst") {
			dstPath = strings.TrimSuffix(dstPath, ".zst")
			log.Printf("decompressing %s to %s", srcPath, dstPath)
			zstdCmd := exec.Command("zstd", "-dc", srcPath)
			outFile, err := os.Create(dstPath)
			if err != nil {
				return fmt.Errorf("failed to create output file: %w", err)
			}
			zstdCmd.Stdout = outFile
			zstdCmd.Stderr = os.Stderr
			if err := zstdCmd.Run(); err != nil {
				outFile.Close()
				return fmt.Errorf("failed to decompress: %w", err)
			}
			outFile.Close()
			// Preserve permissions
			os.Chmod(dstPath, info.Mode())
		} else {
			// Regular copy
			must("copy file", copyFile(srcPath, dstPath))
			os.Chmod(dstPath, info.Mode())
		}
		return nil
	})
	must("copy modules tree", err)

	// Repack squashfs
	log.Print("repacking rootfs.sqsh")
	newRootfsSqshPath := filepath.Join(tmpDir, "rootfs.sqsh")
	mksquashfsCmd := exec.Command("mksquashfs", squashfsDir, newRootfsSqshPath, "-comp", "xz", "-no-progress")
	mksquashfsCmd.Stderr = os.Stderr
	must("mksquashfs", mksquashfsCmd.Run())

	// Replace rootfs.sqsh in cpio directory
	must("replace rootfs.sqsh", os.Remove(rootfsSqshPath))
	must("copy new rootfs.sqsh", copyFile(newRootfsSqshPath, rootfsSqshPath))

	// Copy modules to initramfs root so they're available before rootfs is mounted
	// Decompress .zst files during copy
	log.Print("copying modules to initramfs root (decompressing .zst files)")
	initramfsModulesDir := filepath.Join(cpioDir, "lib", "modules")
	os.MkdirAll(initramfsModulesDir, 0o755)
	initramfsModulesPath := filepath.Join(initramfsModulesDir, kernelVersion)
	os.MkdirAll(initramfsModulesPath, 0o755)
	
	// Copy and decompress modules
	err = filepath.Walk(currentModulesPath, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if info.IsDir() {
			return nil
		}
		
		relPath, err := filepath.Rel(currentModulesPath, srcPath)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(initramfsModulesPath, relPath)
		os.MkdirAll(filepath.Dir(dstPath), 0o755)
		
		// If it's a .zst file, decompress it to .ko
		if strings.HasSuffix(srcPath, ".ko.zst") {
			dstPath = strings.TrimSuffix(dstPath, ".zst")
			zstdCmd := exec.Command("zstd", "-dc", srcPath)
			outFile, err := os.Create(dstPath)
			if err != nil {
				return fmt.Errorf("failed to create output file: %w", err)
			}
			zstdCmd.Stdout = outFile
			zstdCmd.Stderr = os.Stderr
			if err := zstdCmd.Run(); err != nil {
				outFile.Close()
				return fmt.Errorf("failed to decompress: %w", err)
			}
			outFile.Close()
			os.Chmod(dstPath, info.Mode())
		} else {
			// Regular copy
			must("copy file", copyFile(srcPath, dstPath))
			os.Chmod(dstPath, info.Mode())
		}
		return nil
	})
	must("copy modules to initramfs", err)

	// Copy current binary as init and save original init as init.talos
	log.Print("setting up init binary")
	initPath := filepath.Join(cpioDir, "init")
	initTalosPath := filepath.Join(cpioDir, "init.talos")
	
	// Check if init exists
	if _, err := os.Stat(initPath); err != nil {
		log.Fatalf("init not found in initramfs: %v", err)
	}
	
	// Get init file info to preserve permissions
	initInfo, err := os.Stat(initPath)
	must("stat init", err)
	
	// Rename original init to init.talos
	must("rename init to init.talos", os.Rename(initPath, initTalosPath))
	
	// Get current executable path
	currentExec, err := os.Executable()
	must("get executable path", err)
	
	// Copy current binary as init
	must("copy binary as init", copyFile(currentExec, initPath))
	
	// Make init executable with original init permissions
	must("chmod init", os.Chmod(initPath, initInfo.Mode()))

	// Repack cpio
	log.Print("repacking initramfs")
	newInitramfsPath := filepath.Join(tmpDir, "initramfs-new")
	newInitramfsFile, err := os.Create(newInitramfsPath)
	must("create new initramfs", err)
	cpioPackCmd := exec.Command("sh", "-c", "find . | cpio -o -H newc")
	cpioPackCmd.Dir = cpioDir
	cpioPackCmd.Stdout = newInitramfsFile
	cpioPackCmd.Stderr = os.Stderr
	must("repack cpio", cpioPackCmd.Run())
	newInitramfsFile.Close()

	// Collect additional kernel arguments into a string
	extraCmdline := strings.Join(extra, " ")

	log.Print("loading kernel with kexec using modified initramfs")
	must("kexec", kexecLoadWithInitrd(kernelPath, newInitramfsPath, extraCmdline))
}

// Helper function to copy file
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
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
