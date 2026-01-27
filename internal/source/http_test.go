package source

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cozystack/boot-to-talos/internal/testutil"
	"github.com/cozystack/boot-to-talos/internal/types"
)

func TestProgressReader(t *testing.T) {
	content := "Hello, World!"
	reader := strings.NewReader(content)

	var totalRead int64
	pr := &progressReader{
		reader: reader,
		total:  int64(len(content)),
		onProgress: func(current, total int64) {
			atomic.StoreInt64(&totalRead, current)
		},
	}

	data, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}

	if string(data) != content {
		t.Errorf("Content mismatch: got %q, want %q", string(data), content)
	}

	if totalRead != int64(len(content)) {
		t.Errorf("Progress not reported correctly: got %d, want %d", totalRead, len(content))
	}
}

func TestDownloadToFile(t *testing.T) {
	// Create test server
	testContent := "test file content for download"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testContent))
	}))
	defer ts.Close()

	// Create temp file for download
	tmpFile, err := os.CreateTemp("", "download-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	// Download
	ctx := context.Background()
	err = DownloadToFile(ctx, ts.URL, tmpPath, nil)
	if err != nil {
		t.Fatalf("DownloadToFile error: %v", err)
	}

	// Verify content
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("Failed to read downloaded file: %v", err)
	}

	if string(data) != testContent {
		t.Errorf("Downloaded content mismatch: got %q, want %q", string(data), testContent)
	}
}

func TestDownloadToFileWithProgress(t *testing.T) {
	testContent := strings.Repeat("x", 1000)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte(testContent))
	}))
	defer ts.Close()

	tmpFile, err := os.CreateTemp("", "download-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	var progressCalled int64
	ctx := context.Background()
	err = DownloadToFile(ctx, ts.URL, tmpPath, func(current, total int64) {
		atomic.AddInt64(&progressCalled, 1)
	})
	if err != nil {
		t.Fatalf("DownloadToFile error: %v", err)
	}

	if progressCalled == 0 {
		t.Error("Progress callback was never called")
	}
}

func TestDownloadToFile_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	tmpFile, err := os.CreateTemp("", "download-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	ctx := context.Background()
	err = DownloadToFile(ctx, ts.URL, tmpPath, nil)
	if err == nil {
		t.Error("Expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("Error should mention 404: %v", err)
	}
}

func TestDownloadToFile_ContextCanceled(t *testing.T) {
	// Server that waits before sending data
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// Wait for context cancellation
		<-r.Context().Done()
	}))
	defer ts.Close()

	tmpFile, err := os.CreateTemp("", "download-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- DownloadToFile(ctx, ts.URL, tmpPath, nil)
	}()

	// Wait for request to start, then cancel
	<-started
	cancel()

	err = <-errCh
	if err == nil {
		t.Error("Expected error when context is canceled")
	}
}

func TestHTTPSource_Type(t *testing.T) {
	source := NewHTTPSource("https://example.com/test.iso", types.ImageSourceISO)
	if source.Type() != types.ImageSourceISO {
		t.Errorf("Type() = %v, want %v", source.Type(), types.ImageSourceISO)
	}

	source = NewHTTPSource("https://example.com/test.raw", types.ImageSourceRAW)
	if source.Type() != types.ImageSourceRAW {
		t.Errorf("Type() = %v, want %v", source.Type(), types.ImageSourceRAW)
	}
}

func TestHTTPSource_Reference(t *testing.T) {
	url := "https://example.com/test.iso"
	source := NewHTTPSource(url, types.ImageSourceISO)
	if source.Reference() != url {
		t.Errorf("Reference() = %v, want %v", source.Reference(), url)
	}
}

func TestHTTPSource_GetBootAssets_RAW(t *testing.T) {
	// Create test RAW image with UKI
	tmpDir := t.TempDir()
	rawPath := filepath.Join(tmpDir, "test.raw")

	// Create UKI
	ukiPath := filepath.Join(tmpDir, "test.efi")
	expectedCmdline := "console=ttyS0"
	expectedKernel := "test-kernel"
	expectedInitrd := "test-initrd"

	if err := testutil.CreateTestUKIFile(ukiPath, expectedCmdline, expectedKernel, expectedInitrd); err != nil {
		t.Fatalf("Failed to create test UKI: %v", err)
	}

	ukiContent, err := os.ReadFile(ukiPath)
	if err != nil {
		t.Fatalf("Failed to read UKI: %v", err)
	}

	// Create RAW image with UKI
	files := map[string][]byte{
		"/EFI/BOOT/BOOTX64.EFI": ukiContent,
	}
	if err := testutil.CreateTestRAWImage(rawPath, 64, files); err != nil {
		t.Fatalf("Failed to create test RAW image: %v", err)
	}

	rawContent, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatalf("Failed to read RAW: %v", err)
	}

	// Serve the RAW image via HTTP
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(rawContent)
	}))
	defer ts.Close()

	// Test HTTP source
	source := NewHTTPSource(ts.URL+"/test.raw", types.ImageSourceRAW)
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
}

func TestHTTPSource_GetInstallAssets_RAW(t *testing.T) {
	// Create test RAW content
	testContent := []byte("raw disk image content")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(testContent)
	}))
	defer ts.Close()

	source := NewHTTPSource(ts.URL+"/test.raw", types.ImageSourceRAW)
	defer source.Close()

	tmpDir := t.TempDir()
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

func TestHTTPSource_GetBootAssets_ISO_NotSupported(t *testing.T) {
	source := NewHTTPSource("https://example.com/test.iso", types.ImageSourceISO)
	_, err := source.GetBootAssets()
	if err == nil {
		t.Error("GetBootAssets for ISO should return error (not supported)")
	}
}

func TestHTTPSource_GetInstallAssets_ISO_NotSupported(t *testing.T) {
	source := NewHTTPSource("https://example.com/test.iso", types.ImageSourceISO)
	_, err := source.GetInstallAssets("/tmp", 10)
	if err == nil {
		t.Error("GetInstallAssets for ISO should return error (not supported)")
	}
}

func TestHTTPSource_Close(t *testing.T) {
	source := NewHTTPSource("https://example.com/test.iso", types.ImageSourceISO)
	err := source.Close()
	if err != nil {
		t.Errorf("Close() returned error: %v", err)
	}
}

func TestHTTPSource_Close_WithTempFile(t *testing.T) {
	// Create a temp file to simulate downloaded file
	tmpFile, err := os.CreateTemp("", "http-source-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	source := NewHTTPSource("https://example.com/test.iso", types.ImageSourceISO)
	source.tempFile = tmpPath

	// Verify file exists
	if _, err := os.Stat(tmpPath); os.IsNotExist(err) {
		t.Fatal("Temp file should exist before Close()")
	}

	err = source.Close()
	if err != nil {
		t.Errorf("Close() returned error: %v", err)
	}

	// Verify file was removed
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("Temp file should be removed after Close()")
		os.Remove(tmpPath) // cleanup
	}

	// Verify tempFile field was cleared
	if source.tempFile != "" {
		t.Error("tempFile field should be cleared after Close()")
	}
}
