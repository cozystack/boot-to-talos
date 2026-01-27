package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cozystack/boot-to-talos/internal/types"
)

func TestDetectImageSourceType(t *testing.T) {
	// Create temp directory for test files
	tmpDir := t.TempDir()

	// Create test files
	isoFile := filepath.Join(tmpDir, "talos.iso")
	rawFile := filepath.Join(tmpDir, "talos.raw")
	rawXZFile := filepath.Join(tmpDir, "talos.raw.xz")
	rawGZFile := filepath.Join(tmpDir, "talos.raw.gz")
	rawZSTFile := filepath.Join(tmpDir, "talos.raw.zst")

	for _, f := range []string{isoFile, rawFile, rawXZFile, rawGZFile, rawZSTFile} {
		if err := os.WriteFile(f, []byte("test"), 0o644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", f, err)
		}
	}

	tests := []struct {
		name     string
		input    string
		wantType types.ImageSourceType
		wantErr  bool
	}{
		// Container references (default fallback)
		{
			name:     "container registry ref",
			input:    "ghcr.io/cozystack/cozystack/talos:v1.11.6",
			wantType: types.ImageSourceContainer,
		},
		{
			name:     "docker hub ref",
			input:    "docker.io/library/alpine:latest",
			wantType: types.ImageSourceContainer,
		},
		{
			name:     "simple container ref",
			input:    "nginx:latest",
			wantType: types.ImageSourceContainer,
		},

		// Local ISO files
		{
			name:     "local ISO lowercase",
			input:    isoFile,
			wantType: types.ImageSourceISO,
		},

		// Local RAW files
		{
			name:     "local RAW",
			input:    rawFile,
			wantType: types.ImageSourceRAW,
		},
		{
			name:     "local RAW.xz",
			input:    rawXZFile,
			wantType: types.ImageSourceRAW,
		},
		{
			name:     "local RAW.gz",
			input:    rawGZFile,
			wantType: types.ImageSourceRAW,
		},
		{
			name:     "local RAW.zst",
			input:    rawZSTFile,
			wantType: types.ImageSourceRAW,
		},

		// HTTP/HTTPS URLs
		{
			name:     "HTTP ISO",
			input:    "https://factory.talos.dev/image/xxx/v1.11.0/metal-amd64.iso",
			wantType: types.ImageSourceISO,
		},
		{
			name:     "HTTP RAW.xz",
			input:    "https://factory.talos.dev/image/xxx/v1.11.0/metal-amd64.raw.xz",
			wantType: types.ImageSourceRAW,
		},
		{
			name:     "HTTP RAW.zst",
			input:    "https://factory.talos.dev/image/xxx/v1.11.0/metal-amd64.raw.zst",
			wantType: types.ImageSourceRAW,
		},
		{
			name:     "HTTP RAW",
			input:    "http://example.com/talos.raw",
			wantType: types.ImageSourceRAW,
		},
		{
			name:     "HTTP unknown extension defaults to container",
			input:    "https://registry.example.com/talos:v1.11",
			wantType: types.ImageSourceContainer,
		},

		// Edge cases
		{
			name:     "nonexistent file without extension is container",
			input:    "some-image:tag",
			wantType: types.ImageSourceContainer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := DetectImageSource(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("DetectImageSource() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			defer source.Close()

			if got := source.Type(); got != tt.wantType {
				t.Errorf("DetectImageSource().Type() = %v, want %v", got, tt.wantType)
			}
		})
	}
}

func TestDetectImageSource_LocalFileError(t *testing.T) {
	// Test that a local file with unknown extension returns error
	tmpDir := t.TempDir()
	unknownFile := filepath.Join(tmpDir, "unknown.xyz")
	if err := os.WriteFile(unknownFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	_, err := DetectImageSource(unknownFile)
	if err == nil {
		t.Error("DetectImageSource() should return error for unknown local file extension")
	}
}

func TestImageSource_Reference(t *testing.T) {
	tmpDir := t.TempDir()
	isoFile := filepath.Join(tmpDir, "test.iso")
	if err := os.WriteFile(isoFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	tests := []struct {
		name  string
		input string
	}{
		{"container ref", "ghcr.io/test/image:v1"},
		{"local file", isoFile},
		{"http url", "https://example.com/image.iso"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, err := DetectImageSource(tt.input)
			if err != nil {
				t.Fatalf("DetectImageSource() error: %v", err)
			}
			defer source.Close()

			if got := source.Reference(); got != tt.input {
				t.Errorf("Reference() = %q, want %q", got, tt.input)
			}
		})
	}
}
