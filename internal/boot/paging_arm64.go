//go:build linux && arm64

package boot

// Is5LevelPagingActive always returns false on arm64.
// The 5-level paging (LA57) incompatibility only affects x86_64.
func Is5LevelPagingActive() bool { return false }
