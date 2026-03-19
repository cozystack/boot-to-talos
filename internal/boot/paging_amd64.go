//go:build linux && amd64

package boot

import "golang.org/x/sys/unix"

// Is5LevelPagingActive detects if the host kernel uses 5-level page tables (LA57).
//
// 5-level paging extends the virtual address space from 48-bit (4-level) to 57-bit.
// Detection works by attempting mmap at an address above the 4-level canonical limit (2^48).
// If the mapping succeeds, the kernel is using 5-level page tables.
//
// This matters for kexec: if the host runs 5-level paging but the target kernel
// (Talos) is compiled without CONFIG_X86_5LEVEL, the kexec handoff will triple-fault
// because the target decompressor cannot handle the 5→4 level paging transition.
func Is5LevelPagingActive() bool {
	// Address above 4-level canonical limit (2^48).
	// In 4-level paging, userspace addresses are limited to 0 - 0x7FFFFFFFFFFF (2^47 - 1).
	// Address 0x1000000000000 (2^48) is only valid with 5-level paging.
	const targetAddr = 0x1000000000000
	const pageSize = 4096

	addr, _, errno := unix.Syscall6(
		unix.SYS_MMAP,
		targetAddr,
		pageSize,
		unix.PROT_READ,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_FIXED_NOREPLACE,
		^uintptr(0), // fd = -1
		0,
	)
	if errno != 0 {
		return false
	}

	// mmap succeeded at the high address — 5-level paging is active.
	unix.Syscall(unix.SYS_MUNMAP, addr, pageSize, 0) //nolint:errcheck

	return addr == targetAddr
}
