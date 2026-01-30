//go:build linux

package boot

import (
	"bytes"
	"io"
	"testing"
)

func TestCreateMemfdFromReader(t *testing.T) {
	testData := []byte("Hello, memfd!")
	reader := bytes.NewReader(testData)

	file, err := CreateMemfdFromReader("test-memfd", reader)
	if err != nil {
		t.Fatalf("CreateMemfdFromReader() error: %v", err)
	}
	defer file.Close()

	// Verify file is readable
	readData, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("Failed to read from memfd: %v", err)
	}

	if !bytes.Equal(readData, testData) {
		t.Errorf("Read data = %q, want %q", string(readData), string(testData))
	}
}

func TestCreateMemfdFromReader_LargeData(t *testing.T) {
	// Test with larger data (1 MB)
	testData := make([]byte, 1024*1024)
	for i := range testData {
		testData[i] = byte(i % 256)
	}
	reader := bytes.NewReader(testData)

	file, err := CreateMemfdFromReader("large-memfd", reader)
	if err != nil {
		t.Fatalf("CreateMemfdFromReader() error: %v", err)
	}
	defer file.Close()

	readData, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("Failed to read from memfd: %v", err)
	}

	if !bytes.Equal(readData, testData) {
		t.Errorf("Large data mismatch, got %d bytes, want %d bytes", len(readData), len(testData))
	}
}

func TestCreateMemfdFromReader_EmptyData(t *testing.T) {
	reader := bytes.NewReader([]byte{})

	file, err := CreateMemfdFromReader("empty-memfd", reader)
	if err != nil {
		t.Fatalf("CreateMemfdFromReader() error: %v", err)
	}
	defer file.Close()

	readData, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("Failed to read from memfd: %v", err)
	}

	if len(readData) != 0 {
		t.Errorf("Expected empty data, got %d bytes", len(readData))
	}
}
