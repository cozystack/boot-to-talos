package source

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cozystack/boot-to-talos/internal/testutil"
	"github.com/cozystack/boot-to-talos/internal/types"
)

func TestISOSource_Type(t *testing.T) {
	source := NewISOSource("/path/to/test.iso")
	if source.Type() != types.ImageSourceISO {
		t.Errorf("Type() = %v, want %v", source.Type(), types.ImageSourceISO)
	}
}

func TestISOSource_Reference(t *testing.T) {
	path := "/path/to/talos.iso"
	source := NewISOSource(path)
	if source.Reference() != path {
		t.Errorf("Reference() = %v, want %v", source.Reference(), path)
	}
}

func TestFindKernelPath(t *testing.T) {
	// Test common kernel paths
	tmpDir := t.TempDir()

	tests := []struct {
		name       string
		createPath string
		wantFound  bool
	}{
		{
			name:       "boot/vmlinuz",
			createPath: "boot/vmlinuz",
			wantFound:  true,
		},
		{
			name:       "boot/vmlinuz-linux",
			createPath: "boot/vmlinuz-linux",
			wantFound:  true,
		},
		{
			name:       "EFI/BOOT/vmlinuz",
			createPath: "EFI/BOOT/vmlinuz",
			wantFound:  true,
		},
		{
			name:       "no kernel",
			createPath: "",
			wantFound:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fresh directory for each test
			testDir := filepath.Join(tmpDir, tt.name)
			if err := os.MkdirAll(testDir, 0o755); err != nil {
				t.Fatalf("Failed to create test dir: %v", err)
			}

			if tt.createPath != "" {
				fullPath := filepath.Join(testDir, tt.createPath)
				if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
					t.Fatalf("Failed to create dir: %v", err)
				}
				if err := os.WriteFile(fullPath, []byte("kernel"), 0o644); err != nil {
					t.Fatalf("Failed to create file: %v", err)
				}
			}

			path := FindKernelPath(testDir)
			found := path != ""

			if found != tt.wantFound {
				t.Errorf("FindKernelPath() found = %v, want %v", found, tt.wantFound)
			}
		})
	}
}

func TestFindInitrdPath(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name       string
		createPath string
		wantFound  bool
	}{
		{
			name:       "boot/initramfs.xz",
			createPath: "boot/initramfs.xz",
			wantFound:  true,
		},
		{
			name:       "boot/initrd.img",
			createPath: "boot/initrd.img",
			wantFound:  true,
		},
		{
			name:       "boot/initrd",
			createPath: "boot/initrd",
			wantFound:  true,
		},
		{
			name:       "no initrd",
			createPath: "",
			wantFound:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testDir := filepath.Join(tmpDir, tt.name)
			if err := os.MkdirAll(testDir, 0o755); err != nil {
				t.Fatalf("Failed to create test dir: %v", err)
			}

			if tt.createPath != "" {
				fullPath := filepath.Join(testDir, tt.createPath)
				if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
					t.Fatalf("Failed to create dir: %v", err)
				}
				if err := os.WriteFile(fullPath, []byte("initrd"), 0o644); err != nil {
					t.Fatalf("Failed to create file: %v", err)
				}
			}

			path := FindInitrdPath(testDir)
			found := path != ""

			if found != tt.wantFound {
				t.Errorf("FindInitrdPath() found = %v, want %v", found, tt.wantFound)
			}
		})
	}
}

func TestFindKernelPath_DynamicSearch(t *testing.T) {
	// Test the dynamic vmlinuz* search in boot dir
	tmpDir := t.TempDir()
	bootDir := filepath.Join(tmpDir, "boot")
	if err := os.MkdirAll(bootDir, 0o755); err != nil {
		t.Fatalf("Failed to create boot dir: %v", err)
	}

	// Create a kernel with non-standard name
	kernelPath := filepath.Join(bootDir, "vmlinuz-5.15.0-custom")
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0o644); err != nil {
		t.Fatalf("Failed to create kernel file: %v", err)
	}

	found := FindKernelPath(tmpDir)
	if found == "" {
		t.Error("FindKernelPath should find vmlinuz-* in boot dir")
	}
}

func TestFindInitrdPath_DynamicSearch(t *testing.T) {
	// Test the dynamic initrd*/initramfs* search in boot dir
	tmpDir := t.TempDir()
	bootDir := filepath.Join(tmpDir, "boot")
	if err := os.MkdirAll(bootDir, 0o755); err != nil {
		t.Fatalf("Failed to create boot dir: %v", err)
	}

	// Create an initrd with non-standard name
	initrdPath := filepath.Join(bootDir, "initramfs-5.15.0-custom.img")
	if err := os.WriteFile(initrdPath, []byte("initrd"), 0o644); err != nil {
		t.Fatalf("Failed to create initrd file: %v", err)
	}

	found := FindInitrdPath(tmpDir)
	if found == "" {
		t.Error("FindInitrdPath should find initramfs-* in boot dir")
	}
}

func TestISOSource_GetBootAssets_InvalidPath(t *testing.T) {
	source := NewISOSource("/nonexistent/path/to/test.iso")
	assets, err := source.GetBootAssets()
	if err == nil {
		t.Error("GetBootAssets should return error for invalid path")
	}
	if assets != nil {
		t.Error("GetBootAssets should return nil assets")
	}
}

// TestISOSource_GetBootAssets_UKI is the end-to-end regression guard for the
// io/fs.ValidPath path stripping adopted in go-diskfs v1.9: it builds an ISO
// containing a synthetic UKI under /EFI/BOOT/vmlinuz.efi, drives the public
// ISOSource.GetBootAssets path, and asserts the extracted kernel/initrd/cmdline
// match. If any step in findUKIInISO → extractUKIFromISO → uki.Extract regressed
// for slash-handling or case-handling, this test fails before merge.
func TestISOSource_GetBootAssets_UKI(t *testing.T) {
	tmpDir := t.TempDir()
	isoPath := filepath.Join(tmpDir, "test.iso")

	ukiPath := filepath.Join(tmpDir, "uki.efi")
	expectedCmdline := "console=ttyS0 talos.platform=metal"
	expectedKernel := "test-iso-kernel-data"
	expectedInitrd := "test-iso-initrd-data"

	if err := testutil.CreateTestUKIFile(ukiPath, expectedCmdline, expectedKernel, expectedInitrd); err != nil {
		t.Fatalf("CreateTestUKIFile: %v", err)
	}
	ukiContent, err := os.ReadFile(ukiPath)
	if err != nil {
		t.Fatalf("read UKI: %v", err)
	}

	files := map[string][]byte{
		"/EFI/BOOT/vmlinuz.efi": ukiContent,
	}
	if err := testutil.CreateTestISOImage(isoPath, files); err != nil {
		t.Fatalf("CreateTestISOImage: %v", err)
	}

	source := NewISOSource(isoPath)
	defer source.Close()

	assets, err := source.GetBootAssets()
	if err != nil {
		t.Fatalf("GetBootAssets: %v", err)
	}
	defer assets.Close()

	kernelData, err := io.ReadAll(assets.Kernel)
	if err != nil {
		t.Fatalf("read kernel: %v", err)
	}
	if string(kernelData) != expectedKernel {
		t.Errorf("kernel = %q, want %q", string(kernelData), expectedKernel)
	}

	initrdData, err := io.ReadAll(assets.Initrd)
	if err != nil {
		t.Fatalf("read initrd: %v", err)
	}
	if string(initrdData) != expectedInitrd {
		t.Errorf("initrd = %q, want %q", string(initrdData), expectedInitrd)
	}

	if assets.Cmdline != expectedCmdline {
		t.Errorf("cmdline = %q, want %q", assets.Cmdline, expectedCmdline)
	}
}

func TestISOSource_GetInstallAssets_NotSupported(t *testing.T) {
	source := NewISOSource("/path/to/test.iso")
	assets, err := source.GetInstallAssets("/tmp", 10)
	if err == nil {
		t.Error("GetInstallAssets should return error (not supported)")
	}
	if assets != nil {
		t.Error("GetInstallAssets should return nil assets")
	}
	// Verify error message indicates not supported
	if err != nil && !strings.Contains(err.Error(), "not supported") {
		t.Errorf("Error should indicate not supported: %v", err)
	}
}

func TestISOSource_Close(t *testing.T) {
	source := NewISOSource("/path/to/test.iso")
	err := source.Close()
	if err != nil {
		t.Errorf("Close() returned error: %v", err)
	}
}
