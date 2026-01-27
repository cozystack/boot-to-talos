//go:build linux && arm64

package boot

// Architecture-specific syscall numbers for arm64.
const (
	// SYS_MEMFD_CREATE is the syscall number for memfd_create on arm64.
	// int memfd_create(const char *name, unsigned int flags);
	sysMemfdCreate = 279

	// SYS_KEXEC_FILE_LOAD is the syscall number for kexec_file_load on arm64.
	// long kexec_file_load(int kernel_fd, int initrd_fd, unsigned long cmdline_len,
	//                      const char *cmdline, unsigned long flags);
	sysKexecFileLoad = 294

	// SYS_REBOOT is the syscall number for reboot on arm64.
	// int reboot(int magic, int magic2, int cmd, void *arg);
	sysReboot = 142
)
