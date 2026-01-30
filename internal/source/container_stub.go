//go:build !linux

package source

import (
	"github.com/cockroachdb/errors"

	"github.com/cozystack/boot-to-talos/internal/types"
)

// ContainerSource implements ImageSource for container registry images.
// This is a stub for non-Linux platforms.
type ContainerSource struct {
	ref string
}

// NewContainerSource creates a new ContainerSource.
func NewContainerSource(ref string) *ContainerSource {
	return &ContainerSource{ref: ref}
}

func (s *ContainerSource) Type() types.ImageSourceType {
	return types.ImageSourceContainer
}

func (s *ContainerSource) Reference() string {
	return s.ref
}

// GetBootAssets extracts kernel and initrd from UKI in container image.
func (s *ContainerSource) GetBootAssets() (*types.BootAssets, error) {
	return nil, errors.New("container source not supported on this platform")
}

// GetInstallAssets extracts the full rootfs for chroot installation.
func (s *ContainerSource) GetInstallAssets(tmpDir string, sizeGiB uint64) (*types.InstallAssets, error) {
	return nil, errors.New("container source not supported on this platform")
}

func (s *ContainerSource) Close() error {
	return nil
}
