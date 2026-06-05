package source

import (
	iofs "io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cozystack/boot-to-talos/internal/testutil"
	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

type fakeDirEntry struct {
	name string
	dir  bool
}

func (e fakeDirEntry) Name() string { return e.name }
func (e fakeDirEntry) IsDir() bool  { return e.dir }
func (e fakeDirEntry) Type() iofs.FileMode {
	if e.dir {
		return iofs.ModeDir
	}
	return 0
}

// Info returns a minimal in-memory FileInfo so the DirEntry contract holds
// even for callers that decide to consult it. findUKIFile only needs Name(),
// but future call-site additions should not silently fail.
func (e fakeDirEntry) Info() (iofs.FileInfo, error) { return fakeFileInfo(e), nil }

type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string { return f.name }
func (f fakeFileInfo) Size() int64  { return 0 }
func (f fakeFileInfo) Mode() iofs.FileMode {
	if f.dir {
		return iofs.ModeDir
	}
	return 0
}
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

type fakeReadDirFS struct {
	dirs map[string][]iofs.DirEntry
}

func (f fakeReadDirFS) ReadDir(path string) ([]iofs.DirEntry, error) {
	if entries, ok := f.dirs[path]; ok {
		return entries, nil
	}
	return nil, iofs.ErrNotExist
}

func TestFindUKIFile(t *testing.T) {
	tests := []struct {
		name    string
		dirs    map[string][]iofs.DirEntry
		want    string
		wantErr bool
	}{
		{
			name: "UKI in EFI/BOOT",
			dirs: map[string][]iofs.DirEntry{
				"EFI/BOOT": {fakeDirEntry{name: "BOOTX64.EFI"}},
			},
			want: filepath.Join("EFI/BOOT", "BOOTX64.EFI"),
		},
		{
			name: "UKI in mixed-case EFI/boot",
			dirs: map[string][]iofs.DirEntry{
				"EFI/boot": {fakeDirEntry{name: "BOOTX64.efi"}},
			},
			want: filepath.Join("EFI/boot", "BOOTX64.efi"),
		},
		{
			name: "UKI in lowercase efi/boot",
			dirs: map[string][]iofs.DirEntry{
				"efi/boot": {fakeDirEntry{name: "bootx64.efi"}},
			},
			want: filepath.Join("efi/boot", "bootx64.efi"),
		},
		{
			name: "directory exists but no EFI file",
			dirs: map[string][]iofs.DirEntry{
				"EFI/BOOT": {fakeDirEntry{name: "readme.txt"}},
			},
			wantErr: true,
		},
		{
			name:    "no directories at all",
			dirs:    map[string][]iofs.DirEntry{},
			wantErr: true,
		},
		{
			// Directory entries whose name happens to end in ".efi" must be
			// skipped — a directory cannot be opened as a UKI by the caller.
			// The fake's Type()/IsDir() report ModeDir for directory entries
			// so this is the property the finder relies on.
			name: "directory named with .efi suffix is skipped",
			dirs: map[string][]iofs.DirEntry{
				"EFI/BOOT": {fakeDirEntry{name: "subdir.efi", dir: true}},
			},
			wantErr: true,
		},
		{
			// Directory entry plus a regular .efi file in the same directory:
			// the regular file wins, confirming the directory was filtered
			// rather than merely deprioritised.
			name: "regular .efi alongside a directory with .efi suffix",
			dirs: map[string][]iofs.DirEntry{
				"EFI/BOOT": {
					fakeDirEntry{name: "subdir.efi", dir: true},
					fakeDirEntry{name: "BOOTX64.EFI"},
				},
			},
			want: filepath.Join("EFI/BOOT", "BOOTX64.EFI"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findUKIFile(fakeReadDirFS{dirs: tt.dirs})
			if (err != nil) != tt.wantErr {
				t.Fatalf("findUKIFile err=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("findUKIFile = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFindUKIInISO exercises the ISO UKI lookup against a real go-diskfs ISO
// produced by testutil.CreateTestISOImage. This is the regression guard for
// the io/fs.ValidPath path-stripping introduced for go-diskfs v1.9.
func TestFindUKIInISO(t *testing.T) {
	tmpDir := t.TempDir()
	isoPath := filepath.Join(tmpDir, "test.iso")

	files := map[string][]byte{
		"/EFI/BOOT/vmlinuz.efi": []byte("uki-bytes"),
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
	if _, ok := fs.(*iso9660.FileSystem); !ok {
		t.Fatalf("expected iso9660 filesystem, got %T", fs)
	}

	got, err := findUKIInISO(fs)
	if err != nil {
		t.Fatalf("findUKIInISO: %v", err)
	}
	// ISO9660 uppercases names. The lookup is case-insensitive on the suffix,
	// so just confirm the result lives under EFI/BOOT and ends in .efi.
	lower := strings.ToLower(got)
	if !strings.Contains(lower, "efi/boot") || !strings.HasSuffix(lower, ".efi") {
		t.Errorf("findUKIInISO = %q, expected EFI/BOOT/...efi", got)
	}
}

// TestFindUKIInISO_NonVmlinuzEFI documents the deliberate divergence from
// findUKIFile: in an ISO the UKI lookup additionally requires the filename to
// contain "vmlinuz", so a generic .efi file under EFI/BOOT must not be
// returned. If this assertion ever flips, the two finders are out of sync and
// callers may pick up the wrong file (e.g. a shim) as the kernel UKI.
func TestFindUKIInISO_NonVmlinuzEFI(t *testing.T) {
	tmpDir := t.TempDir()
	isoPath := filepath.Join(tmpDir, "test.iso")

	files := map[string][]byte{
		"/EFI/BOOT/BOOTX64.EFI": []byte("not-a-uki"),
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

	got, err := findUKIInISO(fs)
	if err == nil {
		t.Errorf("findUKIInISO accepted %q without 'vmlinuz' in name; expected not-found error", got)
	}
	if err != nil && !strings.Contains(err.Error(), "UKI not found") {
		t.Errorf("findUKIInISO err = %v, expected to mention 'UKI not found'", err)
	}
}

// TestFindUKIInISO_PicksVmlinuzAmongstOthers proves that when both a generic
// .efi file (e.g. shim) and a vmlinuz.efi UKI live in the same EFI/BOOT
// directory, the lookup picks the vmlinuz one. Catches a regression where the
// filter is loosened to "any .efi file" and silently starts returning the shim
// in production.
func TestFindUKIInISO_PicksVmlinuzAmongstOthers(t *testing.T) {
	tmpDir := t.TempDir()
	isoPath := filepath.Join(tmpDir, "test.iso")

	files := map[string][]byte{
		"/EFI/BOOT/BOOTX64.EFI": []byte("shim-bytes"),
		"/EFI/BOOT/vmlinuz.efi": []byte("uki-bytes"),
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

	got, err := findUKIInISO(fs)
	if err != nil {
		t.Fatalf("findUKIInISO: %v", err)
	}
	if !strings.Contains(strings.ToLower(got), "vmlinuz") {
		t.Errorf("findUKIInISO = %q, expected the vmlinuz UKI rather than the shim", got)
	}
}

// TestFindUKIInISO_PlainISO covers the upper-case directory fallback in
// findUKIInISO: on a plain ISO9660 image without Rock Ridge, the lower-case
// "efi/boot" candidate cannot resolve directly, so the finder must also try
// "EFI/BOOT" before declaring not-found.
func TestFindUKIInISO_PlainISO(t *testing.T) {
	tmpDir := t.TempDir()
	isoPath := filepath.Join(tmpDir, "plain.iso")

	files := map[string][]byte{
		"/efi/boot/vmlinuz.efi": []byte("uki-bytes"),
	}
	if err := testutil.CreateTestISOImagePlain(isoPath, files); err != nil {
		t.Fatalf("CreateTestISOImagePlain: %v", err)
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

	got, err := findUKIInISO(fs)
	if err != nil {
		t.Fatalf("findUKIInISO on plain ISO: %v", err)
	}
	if !strings.HasSuffix(strings.ToLower(got), ".efi") {
		t.Errorf("findUKIInISO = %q, expected a .efi path", got)
	}
}

// TestFindFileInISO_PlainISO covers the case-folding fallback added to
// findFileInISO: when the ISO is finalised without Rock Ridge, ISO9660 forces
// filenames to upper-case on disk, so a lower-case look-up path must still
// resolve via the upper-case retry.
func TestFindFileInISO_PlainISO(t *testing.T) {
	tmpDir := t.TempDir()
	isoPath := filepath.Join(tmpDir, "plain.iso")

	files := map[string][]byte{
		"/boot/vmlinuz": []byte("kernel"),
	}
	if err := testutil.CreateTestISOImagePlain(isoPath, files); err != nil {
		t.Fatalf("CreateTestISOImagePlain: %v", err)
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

	got := findFileInISO(fs, []string{"boot/vmlinuz"})
	if got == "" {
		t.Fatal("findFileInISO returned empty for lower-case path on plain ISO; case fallback missing")
	}
	if got != strings.ToUpper("boot/vmlinuz") {
		t.Errorf("findFileInISO = %q, want %q (upper-case fallback)", got, strings.ToUpper("boot/vmlinuz"))
	}
}

// TestFindUKIFile_RealFAT32 drives findUKIFile against a real go-diskfs FAT32
// filesystem built via testutil.CreateTestRAWImage, locking in the contract
// that go-diskfs v1.9 satisfies the in-package interface (ReadDir returning
// []iofs.DirEntry).
func TestFindUKIFile_RealFAT32(t *testing.T) {
	tmpDir := t.TempDir()
	rawPath := filepath.Join(tmpDir, "test.raw")

	files := map[string][]byte{
		"/EFI/BOOT/BOOTX64.EFI": []byte("uki-bytes"),
	}
	if err := testutil.CreateTestRAWImage(rawPath, 64, files); err != nil {
		t.Fatalf("CreateTestRAWImage: %v", err)
	}

	d, err := diskfs.Open(rawPath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	fs, err := d.GetFilesystem(1)
	if err != nil {
		t.Fatalf("GetFilesystem: %v", err)
	}

	got, err := findUKIFile(fs)
	if err != nil {
		t.Fatalf("findUKIFile: %v", err)
	}
	if !strings.HasSuffix(strings.ToLower(got), ".efi") {
		t.Errorf("findUKIFile = %q, expected a .efi path", got)
	}
}

// TestFindFileInISO checks the kernel/initrd fallback path: existing files are
// returned with no leading slash, missing files return empty.
func TestFindFileInISO(t *testing.T) {
	tmpDir := t.TempDir()
	isoPath := filepath.Join(tmpDir, "test.iso")

	files := map[string][]byte{
		"/boot/vmlinuz": []byte("kernel"),
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

	if got := findFileInISO(fs, []string{"boot/vmlinuz"}); got == "" {
		t.Error("findFileInISO returned empty for existing file")
	} else if strings.HasPrefix(got, "/") {
		t.Errorf("findFileInISO returned %q with leading slash; expected no leading slash", got)
	}

	if got := findFileInISO(fs, []string{"nonexistent/file"}); got != "" {
		t.Errorf("findFileInISO returned %q for missing file, want empty", got)
	}
}
