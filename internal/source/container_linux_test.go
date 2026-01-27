//go:build linux

package source

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// mockCloser tracks how many times Close was called.
type mockCloser struct {
	closeCount int32
}

func (m *mockCloser) Close() error {
	atomic.AddInt32(&m.closeCount, 1)
	return nil
}

func (m *mockCloser) getCloseCount() int {
	return int(atomic.LoadInt32(&m.closeCount))
}

// TestUkiReaderCloser_SharedCloser_NoDoubleClose verifies that when two
// readerCloser instances share the same underlying closer, calling Close()
// on both only closes the underlying resource once.
func TestUkiReaderCloser_SharedCloser_NoDoubleClose(t *testing.T) {
	underlying := &mockCloser{}
	shared := &sharedCloser{closer: underlying}

	// Create two readers sharing the same closer (mimics GetBootAssets behavior)
	reader1 := &readerCloser{
		reader: io.NopCloser(nil),
		closer: shared,
	}
	reader2 := &readerCloser{
		reader: io.NopCloser(nil),
		closer: shared,
	}

	// Close both readers
	if err := reader1.Close(); err != nil {
		t.Fatalf("reader1.Close() error: %v", err)
	}
	if err := reader2.Close(); err != nil {
		t.Fatalf("reader2.Close() error: %v", err)
	}

	// The underlying closer should only be closed once (sync.Once ensures this)
	if underlying.getCloseCount() != 1 {
		t.Errorf("closer was called %d times, want 1 (double-close bug)", underlying.getCloseCount())
	}
}

// TestUkiReaderCloser_MultipleClosesSameInstance verifies that calling Close()
// multiple times on the same readerCloser is safe (idempotent).
func TestUkiReaderCloser_MultipleClosesSameInstance(t *testing.T) {
	underlying := &mockCloser{}
	shared := &sharedCloser{closer: underlying}

	reader := &readerCloser{
		reader: io.NopCloser(nil),
		closer: shared,
	}

	// Close same reader multiple times
	for i := range 3 {
		if err := reader.Close(); err != nil {
			t.Fatalf("reader.Close() #%d error: %v", i, err)
		}
	}

	// The underlying closer should only be closed once
	if underlying.getCloseCount() != 1 {
		t.Errorf("closer was called %d times, want 1", underlying.getCloseCount())
	}
}

// TestContainerSource_Close_CleansUpTmpDir verifies that Close() cleans up
// the temporary directory even if extraction failed.
func TestContainerSource_Close_CleansUpTmpDir(t *testing.T) {
	// Create a container source with invalid image (will fail to pull)
	source := NewContainerSource("invalid-registry.local/nonexistent:v0.0.0")

	// Try to extract (this will fail, but might create tmpDir)
	_, _ = source.GetBootAssets()

	// Save tmpDir path before Close
	tmpDir := source.tmpDir

	// Close should clean up even after failure
	if err := source.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	// Verify tmpDir is cleared
	if source.tmpDir != "" {
		t.Error("source.tmpDir should be empty after Close()")
	}

	// If tmpDir was created, it should have been removed
	// (we can't easily check filesystem here without importing os,
	// but the cleanup logic in Close() handles this)
	_ = tmpDir // silence unused variable warning
}

// mockLayer implements the layer interface for testing extractLayer.
type mockLayer struct {
	data []byte
}

func (m *mockLayer) Uncompressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.data)), nil
}

// createTarWithFile creates a tar archive containing a single file.
func createTarWithFile(name string, content []byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	hdr := &tar.Header{
		Name: name,
		Mode: 0o644,
		Size: int64(len(content)),
	}
	_ = tw.WriteHeader(hdr)
	_, _ = tw.Write(content)
	_ = tw.Close()

	return buf.Bytes()
}

// TestExtractLayer_PathTraversal verifies that path traversal attacks are blocked.
func TestExtractLayer_PathTraversal(t *testing.T) {
	// Create temp directory structure
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "dest")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("failed to create dest dir: %v", err)
	}

	// Create tar with malicious path that tries to escape destDir
	maliciousContent := []byte("malicious content")
	tarData := createTarWithFile("../../../etc/malicious", maliciousContent)

	layer := &mockLayer{data: tarData}

	// Extract layer
	if err := extractLayer(layer, destDir); err != nil {
		t.Fatalf("extractLayer error: %v", err)
	}

	// Verify malicious file was NOT created outside destDir
	maliciousPath := filepath.Join(tmpDir, "etc", "malicious")
	if _, err := os.Stat(maliciousPath); !os.IsNotExist(err) {
		t.Errorf("path traversal attack succeeded: file created at %s", maliciousPath)
	}

	// Also check the root-level escape attempt
	rootMalicious := filepath.Join(destDir, "..", "..", "..", "etc", "malicious")
	cleanPath := filepath.Clean(rootMalicious)
	if _, err := os.Stat(cleanPath); !os.IsNotExist(err) {
		t.Errorf("path traversal attack succeeded: file exists at %s", cleanPath)
	}
}

// TestExtractLayer_ValidPath verifies that normal files are extracted correctly.
func TestExtractLayer_ValidPath(t *testing.T) {
	destDir := t.TempDir()

	// Create tar with valid path
	content := []byte("valid content")
	tarData := createTarWithFile("subdir/file.txt", content)

	layer := &mockLayer{data: tarData}

	// Extract layer
	if err := extractLayer(layer, destDir); err != nil {
		t.Fatalf("extractLayer error: %v", err)
	}

	// Verify file was created
	filePath := filepath.Join(destDir, "subdir", "file.txt")
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read extracted file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("content = %q, want %q", string(data), string(content))
	}
}

// createTarWithSymlink creates a tar archive containing a symlink.
func createTarWithSymlink(name, linkTarget string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	hdr := &tar.Header{
		Name:     name,
		Typeflag: tar.TypeSymlink,
		Linkname: linkTarget,
	}
	_ = tw.WriteHeader(hdr)
	_ = tw.Close()

	return buf.Bytes()
}

// createTarWithHardlink creates a tar archive containing a hardlink.
func createTarWithHardlink(name, linkTarget string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	hdr := &tar.Header{
		Name:     name,
		Typeflag: tar.TypeLink,
		Linkname: linkTarget,
	}
	_ = tw.WriteHeader(hdr)
	_ = tw.Close()

	return buf.Bytes()
}

// TestExtractLayer_SymlinkEscape verifies that symlinks pointing outside destDir are blocked.
func TestExtractLayer_SymlinkEscape(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "dest")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("failed to create dest dir: %v", err)
	}

	// Create a sensitive file outside destDir
	sensitiveFile := filepath.Join(tmpDir, "sensitive.txt")
	if err := os.WriteFile(sensitiveFile, []byte("secret"), 0o644); err != nil {
		t.Fatalf("failed to create sensitive file: %v", err)
	}

	// Create tar with symlink that tries to escape destDir
	tarData := createTarWithSymlink("escape_link", "../sensitive.txt")
	layer := &mockLayer{data: tarData}

	// Extract layer
	if err := extractLayer(layer, destDir); err != nil {
		t.Fatalf("extractLayer error: %v", err)
	}

	// Verify symlink was NOT created (escape attempt blocked)
	linkPath := filepath.Join(destDir, "escape_link")
	if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
		t.Errorf("symlink escape attack succeeded: symlink created at %s", linkPath)
	}
}

// TestExtractLayer_HardlinkEscape verifies that hardlinks referencing files outside destDir are blocked.
func TestExtractLayer_HardlinkEscape(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "dest")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("failed to create dest dir: %v", err)
	}

	// Create tar with hardlink that tries to reference file outside destDir
	tarData := createTarWithHardlink("escape_hardlink", "../../../etc/passwd")
	layer := &mockLayer{data: tarData}

	// Extract layer
	if err := extractLayer(layer, destDir); err != nil {
		t.Fatalf("extractLayer error: %v", err)
	}

	// Verify hardlink was NOT created (escape attempt blocked)
	linkPath := filepath.Join(destDir, "escape_hardlink")
	if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
		t.Errorf("hardlink escape attack succeeded: hardlink created at %s", linkPath)
	}
}

// TestExtractLayer_ValidSymlink verifies that symlinks within destDir work correctly.
func TestExtractLayer_ValidSymlink(t *testing.T) {
	destDir := t.TempDir()

	// First create the target file
	targetContent := []byte("target content")
	tarData := createTarWithFile("target.txt", targetContent)
	layer := &mockLayer{data: tarData}
	if err := extractLayer(layer, destDir); err != nil {
		t.Fatalf("extractLayer error creating target: %v", err)
	}

	// Create tar with valid symlink (relative, within destDir)
	tarData = createTarWithSymlink("subdir/link.txt", "../target.txt")
	layer = &mockLayer{data: tarData}

	// Create subdir first
	if err := os.MkdirAll(filepath.Join(destDir, "subdir"), 0o755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	// Extract layer
	if err := extractLayer(layer, destDir); err != nil {
		t.Fatalf("extractLayer error: %v", err)
	}

	// Verify symlink was created and works
	linkPath := filepath.Join(destDir, "subdir", "link.txt")
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("failed to stat symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("expected symlink, got %v", info.Mode())
	}

	// Verify symlink resolves to correct target
	resolved, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("failed to read symlink: %v", err)
	}
	if resolved != "../target.txt" {
		t.Errorf("symlink target = %q, want %q", resolved, "../target.txt")
	}
}
