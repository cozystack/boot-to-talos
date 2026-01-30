//go:build !linux

package source

import (
	"testing"

	"github.com/cozystack/boot-to-talos/internal/types"
)

func TestContainerSource_NewContainerSource(t *testing.T) {
	ref := "ghcr.io/test/image:v1.0"
	source := NewContainerSource(ref)
	if source == nil {
		t.Fatal("NewContainerSource returned nil")
	}
	if source.ref != ref {
		t.Errorf("ref = %q, want %q", source.ref, ref)
	}
}

func TestContainerSource_Type(t *testing.T) {
	source := NewContainerSource("test:latest")
	if source.Type() != types.ImageSourceContainer {
		t.Errorf("Type() = %v, want %v", source.Type(), types.ImageSourceContainer)
	}
}

func TestContainerSource_Reference(t *testing.T) {
	ref := "ghcr.io/cozystack/talos:v1.11"
	source := NewContainerSource(ref)
	if source.Reference() != ref {
		t.Errorf("Reference() = %q, want %q", source.Reference(), ref)
	}
}

func TestContainerSource_GetBootAssets_ReturnsError(t *testing.T) {
	source := NewContainerSource("test:latest")
	assets, err := source.GetBootAssets()
	if err == nil {
		t.Error("GetBootAssets should return error on non-Linux platform")
	}
	if assets != nil {
		t.Error("GetBootAssets should return nil assets on non-Linux platform")
	}
}

func TestContainerSource_GetInstallAssets_ReturnsError(t *testing.T) {
	source := NewContainerSource("test:latest")
	assets, err := source.GetInstallAssets("/tmp", 10)
	if err == nil {
		t.Error("GetInstallAssets should return error on non-Linux platform")
	}
	if assets != nil {
		t.Error("GetInstallAssets should return nil assets on non-Linux platform")
	}
}

func TestContainerSource_Close(t *testing.T) {
	source := NewContainerSource("test:latest")
	err := source.Close()
	if err != nil {
		t.Errorf("Close() returned error: %v", err)
	}
}
