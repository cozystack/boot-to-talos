package uki

import (
	"debug/pe"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cozystack/boot-to-talos/internal/testutil"
)

func TestReadCmdline(t *testing.T) {
	// Create test UKI file
	tmpDir := t.TempDir()
	ukiPath := filepath.Join(tmpDir, "test.efi")
	expectedCmdline := "console=ttyS0 talos.config=http://example.com/config"

	if err := createTestUKIFile(ukiPath, expectedCmdline); err != nil {
		t.Fatalf("Failed to create test UKI: %v", err)
	}

	cmdline, err := ReadCmdline(ukiPath)
	if err != nil {
		t.Fatalf("ReadCmdline error: %v", err)
	}

	if cmdline != expectedCmdline {
		t.Errorf("cmdline = %q, want %q", cmdline, expectedCmdline)
	}
}

func TestPatchCmdline(t *testing.T) {
	tmpDir := t.TempDir()
	ukiPath := filepath.Join(tmpDir, "test.efi")
	originalCmdline := "console=ttyS0 original=true"
	newCmdline := "console=ttyS0 patched=true"

	// Create UKI with enough space for patched cmdline
	paddedOriginal := originalCmdline + strings.Repeat("\x00", 100)
	if err := createTestUKIFile(ukiPath, paddedOriginal); err != nil {
		t.Fatalf("Failed to create test UKI: %v", err)
	}

	// Patch cmdline
	if err := PatchCmdline(ukiPath, newCmdline); err != nil {
		t.Fatalf("PatchCmdline error: %v", err)
	}

	// Read back and verify
	cmdline, err := ReadCmdline(ukiPath)
	if err != nil {
		t.Fatalf("ReadCmdline error: %v", err)
	}

	// cmdline might have trailing nulls, trim them
	cmdline = strings.TrimRight(cmdline, "\x00")
	if cmdline != newCmdline {
		t.Errorf("cmdline = %q, want %q", cmdline, newCmdline)
	}

	// Verify PE file is still valid
	peFile, err := pe.Open(ukiPath)
	if err != nil {
		t.Fatalf("PE file invalid after patch: %v", err)
	}
	peFile.Close()
}

func TestPatchCmdline_TooLong(t *testing.T) {
	tmpDir := t.TempDir()
	ukiPath := filepath.Join(tmpDir, "test.efi")
	shortCmdline := "short"

	if err := createTestUKIFile(ukiPath, shortCmdline); err != nil {
		t.Fatalf("Failed to create test UKI: %v", err)
	}

	// Try to patch with longer cmdline than available space
	longCmdline := strings.Repeat("x", 1000)
	err := PatchCmdline(ukiPath, longCmdline)
	if err == nil {
		t.Error("Expected error when cmdline is too long")
	}
}

func TestExtract(t *testing.T) {
	tmpDir := t.TempDir()
	ukiPath := filepath.Join(tmpDir, "test.efi")
	expectedCmdline := "console=ttyS0"
	expectedKernel := "test-kernel-data"
	expectedInitrd := "test-initrd-data"

	if err := createTestUKIFile(ukiPath, expectedCmdline); err != nil {
		t.Fatalf("Failed to create test UKI: %v", err)
	}

	assets, err := Extract(ukiPath)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	defer assets.Close()

	// Read and verify kernel
	kernelData, err := io.ReadAll(assets.Kernel)
	if err != nil {
		t.Fatalf("Read kernel error: %v", err)
	}
	if string(kernelData) != expectedKernel {
		t.Errorf("kernel = %q, want %q", string(kernelData), expectedKernel)
	}

	// Read and verify initrd
	initrdData, err := io.ReadAll(assets.Initrd)
	if err != nil {
		t.Fatalf("Read initrd error: %v", err)
	}
	if string(initrdData) != expectedInitrd {
		t.Errorf("initrd = %q, want %q", string(initrdData), expectedInitrd)
	}

	// Read and verify cmdline
	cmdlineData, err := io.ReadAll(assets.Cmdline)
	if err != nil {
		t.Fatalf("Read cmdline error: %v", err)
	}
	if string(cmdlineData) != expectedCmdline {
		t.Errorf("cmdline = %q, want %q", string(cmdlineData), expectedCmdline)
	}
}

func TestExtract_InvalidFile(t *testing.T) {
	tmpDir := t.TempDir()
	ukiPath := filepath.Join(tmpDir, "invalid.efi")

	// Create invalid PE file
	if err := os.WriteFile(ukiPath, []byte("not a PE file"), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	_, err := Extract(ukiPath)
	if err == nil {
		t.Error("Expected error for invalid PE file")
	}
}

func TestExtract_MissingSection(t *testing.T) {
	tmpDir := t.TempDir()
	ukiPath := filepath.Join(tmpDir, "incomplete.efi")

	// Create PE file with only cmdline section (missing .linux and .initrd)
	sections := map[string][]byte{
		".cmdline": []byte("test-cmdline"),
	}
	if err := createMinimalPEFile(ukiPath, sections); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	_, err := Extract(ukiPath)
	if err == nil {
		t.Error("Expected error for missing sections")
	}
}

func TestReadCmdline_InvalidFile(t *testing.T) {
	tmpDir := t.TempDir()
	ukiPath := filepath.Join(tmpDir, "invalid.efi")

	// Create invalid PE file
	if err := os.WriteFile(ukiPath, []byte("not a PE file"), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	_, err := ReadCmdline(ukiPath)
	if err == nil {
		t.Error("Expected error for invalid PE file")
	}
}

func TestReadCmdline_NoCmdlineSection(t *testing.T) {
	tmpDir := t.TempDir()
	ukiPath := filepath.Join(tmpDir, "no-cmdline.efi")

	// Create PE file without .cmdline section
	sections := map[string][]byte{
		".linux":  []byte("test-kernel-data"),
		".initrd": []byte("test-initrd-data"),
	}
	if err := createMinimalPEFile(ukiPath, sections); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	_, err := ReadCmdline(ukiPath)
	if err == nil {
		t.Error("Expected error for missing .cmdline section")
	}
}

func TestPatchCmdline_InvalidFile(t *testing.T) {
	tmpDir := t.TempDir()
	ukiPath := filepath.Join(tmpDir, "invalid.efi")

	// Create invalid PE file
	if err := os.WriteFile(ukiPath, []byte("not a PE file"), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	err := PatchCmdline(ukiPath, "new cmdline")
	if err == nil {
		t.Error("Expected error for invalid PE file")
	}
}

func TestPatchCmdline_NoCmdlineSection(t *testing.T) {
	tmpDir := t.TempDir()
	ukiPath := filepath.Join(tmpDir, "no-cmdline.efi")

	// Create PE file without .cmdline section
	sections := map[string][]byte{
		".linux":  []byte("test-kernel-data"),
		".initrd": []byte("test-initrd-data"),
	}
	if err := createMinimalPEFile(ukiPath, sections); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	err := PatchCmdline(ukiPath, "new cmdline")
	if err == nil {
		t.Error("Expected error for missing .cmdline section")
	}
}

func TestExtract_NonExistentFile(t *testing.T) {
	_, err := Extract("/nonexistent/path/to/file.efi")
	if err == nil {
		t.Error("Expected error for nonexistent file")
	}
}

func TestReadCmdline_NonExistentFile(t *testing.T) {
	_, err := ReadCmdline("/nonexistent/path/to/file.efi")
	if err == nil {
		t.Error("Expected error for nonexistent file")
	}
}

func TestPatchCmdline_NonExistentFile(t *testing.T) {
	err := PatchCmdline("/nonexistent/path/to/file.efi", "new cmdline")
	if err == nil {
		t.Error("Expected error for nonexistent file")
	}
}

// createTestUKIFile creates a minimal PE file with .cmdline section for testing.
func createTestUKIFile(path, cmdline string) error {
	return testutil.CreateTestUKIFile(path, cmdline, "test-kernel-data", "test-initrd-data")
}

// createMinimalPEFile creates a PE file with given sections.
func createMinimalPEFile(path string, sections map[string][]byte) error {
	return testutil.CreateMinimalPEFile(path, sections)
}
