//go:build linux && amd64

package boot

import "testing"

func TestIs5LevelPagingActive(t *testing.T) {
	// Verify the detection function runs without panicking.
	// On most CI/development machines this returns false (no la57),
	// which is the expected safe result.
	active := Is5LevelPagingActive()
	t.Logf("5-level paging (LA57) active: %v", active)
}
