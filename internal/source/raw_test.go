package source

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cozystack/boot-to-talos/internal/testutil"
	"github.com/cozystack/boot-to-talos/internal/types"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

func TestDetectCompression(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"file.raw.xz", "xz"},
		{"file.raw.gz", "gz"},
		{"file.raw.zst", "zst"},
		{"file.raw", ""},
		{"file.RAW.XZ", "xz"},
		{"file.RAW.GZ", "gz"},
		{"file.RAW.ZST", "zst"},
		{"/path/to/talos-v1.11.0-metal-amd64.raw.xz", "xz"},
		{"/path/to/talos-v1.11.0-metal-amd64.raw.zst", "zst"},
		{"/path/to/talos-v1.11.0-metal-amd64.raw", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := DetectCompression(tt.path)
			if got != tt.want {
				t.Errorf("DetectCompression(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestOpenDecompressed_Uncompressed(t *testing.T) {
	// Create uncompressed test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.raw")
	testContent := []byte("uncompressed raw content")

	if err := os.WriteFile(testFile, testContent, 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	reader, size, err := OpenDecompressed(testFile)
	if err != nil {
		t.Fatalf("OpenDecompressed error: %v", err)
	}
	defer reader.Close()

	if size != int64(len(testContent)) {
		t.Errorf("size = %d, want %d", size, len(testContent))
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}

	if !bytes.Equal(data, testContent) {
		t.Errorf("content mismatch: got %q, want %q", data, testContent)
	}
}

func TestOpenDecompressed_XZ(t *testing.T) {
	// Create XZ compressed test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.raw.xz")
	testContent := []byte("compressed raw content for xz test")

	// Compress with XZ
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatalf("xz.NewWriter error: %v", err)
	}
	if _, err := w.Write(testContent); err != nil {
		t.Fatalf("xz write error: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("xz close error: %v", err)
	}

	if err := os.WriteFile(testFile, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	reader, size, err := OpenDecompressed(testFile)
	if err != nil {
		t.Fatalf("OpenDecompressed error: %v", err)
	}
	defer reader.Close()

	// Size is unknown for compressed files (-1)
	if size != -1 {
		t.Logf("size = %d (compressed files may report -1 or actual size)", size)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}

	if !bytes.Equal(data, testContent) {
		t.Errorf("content mismatch: got %q, want %q", data, testContent)
	}
}

func TestOpenDecompressed_GZ(t *testing.T) {
	// Create GZ compressed test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.raw.gz")
	testContent := []byte("compressed raw content for gzip test")

	// Compress with gzip
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(testContent); err != nil {
		t.Fatalf("gzip write error: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close error: %v", err)
	}

	if err := os.WriteFile(testFile, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	reader, size, err := OpenDecompressed(testFile)
	if err != nil {
		t.Fatalf("OpenDecompressed error: %v", err)
	}
	defer reader.Close()

	// Size is unknown for compressed files (-1)
	if size != -1 {
		t.Logf("size = %d (compressed files may report -1 or actual size)", size)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}

	if !bytes.Equal(data, testContent) {
		t.Errorf("content mismatch: got %q, want %q", data, testContent)
	}
}

func TestOpenDecompressed_ZST(t *testing.T) {
	// Create ZST compressed test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.raw.zst")
	testContent := []byte("compressed raw content for zstd test")

	// Compress with zstd
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter error: %v", err)
	}
	if _, err := w.Write(testContent); err != nil {
		t.Fatalf("zstd write error: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zstd close error: %v", err)
	}

	if err := os.WriteFile(testFile, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	reader, size, err := OpenDecompressed(testFile)
	if err != nil {
		t.Fatalf("OpenDecompressed error: %v", err)
	}
	defer reader.Close()

	// Size is unknown for compressed files (-1)
	if size != -1 {
		t.Logf("size = %d (compressed files may report -1 or actual size)", size)
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}

	if !bytes.Equal(data, testContent) {
		t.Errorf("content mismatch: got %q, want %q", data, testContent)
	}
}

func TestRAWSource_Type(t *testing.T) {
	source := NewRAWSource("/path/to/test.raw")
	if source.Type() != types.ImageSourceRAW {
		t.Errorf("Type() = %v, want %v", source.Type(), types.ImageSourceRAW)
	}
}

func TestRAWSource_Reference(t *testing.T) {
	path := "/path/to/test.raw.xz"
	source := NewRAWSource(path)
	if source.Reference() != path {
		t.Errorf("Reference() = %v, want %v", source.Reference(), path)
	}
}

func TestRAWSource_GetInstallAssets(t *testing.T) {
	// Create test RAW file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.raw")
	testContent := []byte("raw disk image content")

	if err := os.WriteFile(testFile, testContent, 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	source := NewRAWSource(testFile)
	defer source.Close()

	assets, err := source.GetInstallAssets(tmpDir, 10)
	if err != nil {
		t.Fatalf("GetInstallAssets error: %v", err)
	}
	defer assets.Close()

	if assets.DiskImage == nil {
		t.Fatal("DiskImage should not be nil")
	}

	data, err := io.ReadAll(assets.DiskImage)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}

	if !bytes.Equal(data, testContent) {
		t.Errorf("content mismatch: got %q, want %q", data, testContent)
	}
}

func TestRAWSource_GetBootAssets(t *testing.T) {
	tmpDir := t.TempDir()
	rawPath := filepath.Join(tmpDir, "test.raw")

	// Create test UKI file content
	ukiPath := filepath.Join(tmpDir, "test.efi")
	expectedCmdline := "console=ttyS0 talos.platform=metal"
	expectedKernel := "test-kernel-data-12345"
	expectedInitrd := "test-initrd-data-67890"

	if err := testutil.CreateTestUKIFile(ukiPath, expectedCmdline, expectedKernel, expectedInitrd); err != nil {
		t.Fatalf("Failed to create test UKI: %v", err)
	}

	ukiContent, err := os.ReadFile(ukiPath)
	if err != nil {
		t.Fatalf("Failed to read UKI: %v", err)
	}

	// Create RAW image with EFI partition containing UKI
	files := map[string][]byte{
		"/EFI/BOOT/BOOTX64.EFI": ukiContent,
	}
	if err := testutil.CreateTestRAWImage(rawPath, 64, files); err != nil {
		t.Fatalf("Failed to create test RAW image: %v", err)
	}

	// Test GetBootAssets
	source := NewRAWSource(rawPath)
	defer source.Close()

	assets, err := source.GetBootAssets()
	if err != nil {
		t.Fatalf("GetBootAssets error: %v", err)
	}
	defer assets.Close()

	// Verify kernel
	kernelData, err := io.ReadAll(assets.Kernel)
	if err != nil {
		t.Fatalf("Read kernel error: %v", err)
	}
	if string(kernelData) != expectedKernel {
		t.Errorf("kernel = %q, want %q", string(kernelData), expectedKernel)
	}

	// Verify initrd
	initrdData, err := io.ReadAll(assets.Initrd)
	if err != nil {
		t.Fatalf("Read initrd error: %v", err)
	}
	if string(initrdData) != expectedInitrd {
		t.Errorf("initrd = %q, want %q", string(initrdData), expectedInitrd)
	}

	// Verify cmdline
	if assets.Cmdline != expectedCmdline {
		t.Errorf("cmdline = %q, want %q", assets.Cmdline, expectedCmdline)
	}
}

func TestRAWSource_GetBootAssets_InvalidFile(t *testing.T) {
	// Test that GetBootAssets returns error for invalid disk image
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.raw")

	// Create minimal file (not a valid disk image)
	if err := os.WriteFile(testFile, []byte("dummy"), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	source := NewRAWSource(testFile)
	defer source.Close()

	_, err := source.GetBootAssets()
	if err == nil {
		t.Error("Expected error for invalid disk image")
	}
}

func TestRAWSource_GetInstallAssets_XZ(t *testing.T) {
	// Create XZ compressed test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.raw.xz")
	testContent := []byte("compressed disk image content")

	// Compress with XZ
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatalf("xz.NewWriter error: %v", err)
	}
	if _, err := w.Write(testContent); err != nil {
		t.Fatalf("xz write error: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("xz close error: %v", err)
	}

	if err := os.WriteFile(testFile, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	source := NewRAWSource(testFile)
	defer source.Close()

	assets, err := source.GetInstallAssets(tmpDir, 10)
	if err != nil {
		t.Fatalf("GetInstallAssets error: %v", err)
	}
	defer assets.Close()

	if assets.DiskImage == nil {
		t.Fatal("DiskImage should not be nil")
	}

	data, err := io.ReadAll(assets.DiskImage)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}

	if !bytes.Equal(data, testContent) {
		t.Errorf("content mismatch: got %q, want %q", data, testContent)
	}
}
