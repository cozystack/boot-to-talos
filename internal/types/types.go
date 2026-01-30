package types

import (
	"io"

	"github.com/cockroachdb/errors"
)

// ImageSourceType represents the type of image source.
type ImageSourceType int

const (
	ImageSourceContainer ImageSourceType = iota // Container registry image (e.g., ghcr.io/...)
	ImageSourceISO                              // ISO file (local or HTTP)
	ImageSourceRAW                              // RAW disk image (local or HTTP), possibly XZ compressed
)

func (t ImageSourceType) String() string {
	switch t {
	case ImageSourceContainer:
		return "container"
	case ImageSourceISO:
		return "iso"
	case ImageSourceRAW:
		return "raw"
	default:
		return "unknown"
	}
}

// BootAssets contains kernel, initrd, and cmdline for kexec boot.
type BootAssets struct {
	Kernel  io.ReadCloser
	Initrd  io.ReadCloser
	Cmdline string
}

// Close releases all resources.
func (a *BootAssets) Close() error {
	var errs []error
	if a.Kernel != nil {
		if err := a.Kernel.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if a.Initrd != nil {
		if err := a.Initrd.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Newf("close boot assets: %v", errs)
	}
	return nil
}

// InstallAssets contains data needed for installation.
type InstallAssets struct {
	// For container/ISO: path to extracted rootfs with installer
	RootfsPath string

	// For RAW: reader for disk image (possibly decompressed)
	DiskImage     io.ReadCloser
	DiskImageSize int64

	// Cleanup function to call after installation
	Cleanup func() error
}

// Close releases all resources.
func (a *InstallAssets) Close() error {
	var errs []error
	if a.DiskImage != nil {
		if err := a.DiskImage.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if a.Cleanup != nil {
		if err := a.Cleanup(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Newf("close install assets: %v", errs)
	}
	return nil
}

// ImageSource is an abstraction over different image sources.
type ImageSource interface {
	// Type returns the type of image source.
	Type() ImageSourceType

	// Reference returns the original image reference (path, URL, or container ref).
	Reference() string

	// GetBootAssets returns kernel, initrd, and cmdline for kexec boot.
	GetBootAssets() (*BootAssets, error)

	// GetInstallAssets returns data needed for installation.
	GetInstallAssets(tmpDir string, sizeGiB uint64) (*InstallAssets, error)

	// Close releases any resources held by the source.
	Close() error
}
