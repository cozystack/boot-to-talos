package testutil_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cozystack/boot-to-talos/internal/testutil"
	"github.com/diskfs/go-diskfs"
)

// TestCreateTestISOImage_LowerCaseRoundTrip locks in the RockRidge: true
// finalize option. ISO9660 without Rock Ridge upper-cases every filename, so a
// case-sensitive fs.OpenFile look-up against the lower-cased input path would
// fail. If a future contributor toggles RockRidge off, this test fails before
// it can break the case-sensitive UKI / kernel / initrd look-ups in
// internal/source.
func TestCreateTestISOImage_LowerCaseRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	isoPath := filepath.Join(tmpDir, "lowercase.iso")

	const payload = "lowercase-payload"
	files := map[string][]byte{
		"/efi/boot/lowercase.efi": []byte(payload),
	}
	if err := testutil.CreateTestISOImage(isoPath, files); err != nil {
		t.Fatalf("CreateTestISOImage: %v", err)
	}

	d, err := diskfs.Open(isoPath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	fs, err := d.GetFilesystem(0)
	if err != nil {
		t.Fatalf("GetFilesystem: %v", err)
	}

	f, err := fs.OpenFile("efi/boot/lowercase.efi", os.O_RDONLY)
	if err != nil {
		t.Fatalf("OpenFile lower-case path: %v", err)
	}
	defer f.Close()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != payload {
		t.Errorf("payload = %q, want %q", string(got), payload)
	}
}
