package source

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem"

	"github.com/cozystack/boot-to-talos/internal/types"
	"github.com/cozystack/boot-to-talos/internal/uki"
)

// Common paths where kernel and initrd are located in ISO images.
//
//nolint:gochecknoglobals
var kernelPaths = []string{
	"boot/vmlinuz",
	"boot/vmlinuz-linux",
	"boot/kernel",
	"EFI/BOOT/vmlinuz",
	"isolinux/vmlinuz",
	"vmlinuz",
}

//nolint:gochecknoglobals
var initrdPaths = []string{
	"boot/initramfs.xz",
	"boot/initrd.xz",
	"boot/initrd.img",
	"boot/initrd",
	"boot/initramfs-linux.img",
	"EFI/BOOT/initrd.img",
	"isolinux/initrd.img",
	"initrd.img",
	"initrd",
}

// ISOSource implements ImageSource for ISO files.
type ISOSource struct {
	path string
}

// NewISOSource creates a new ISOSource.
func NewISOSource(path string) *ISOSource {
	return &ISOSource{path: path}
}

func (s *ISOSource) Type() types.ImageSourceType {
	return types.ImageSourceISO
}

func (s *ISOSource) Reference() string {
	return s.path
}

// FindKernelPath searches for kernel in common locations.
func FindKernelPath(mountPoint string) string {
	for _, relPath := range kernelPaths {
		fullPath := filepath.Join(mountPoint, relPath)
		if _, err := os.Stat(fullPath); err == nil {
			return fullPath
		}
	}

	// Also try to find any file starting with "vmlinuz"
	bootDir := filepath.Join(mountPoint, "boot")
	if entries, err := os.ReadDir(bootDir); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "vmlinuz") {
				return filepath.Join(bootDir, entry.Name())
			}
		}
	}

	return ""
}

// FindInitrdPath searches for initrd in common locations.
func FindInitrdPath(mountPoint string) string {
	for _, relPath := range initrdPaths {
		fullPath := filepath.Join(mountPoint, relPath)
		if _, err := os.Stat(fullPath); err == nil {
			return fullPath
		}
	}

	// Also try to find any file starting with "initr" or "initramfs"
	bootDir := filepath.Join(mountPoint, "boot")
	if entries, err := os.ReadDir(bootDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, "initrd") || strings.HasPrefix(name, "initramfs") {
				return filepath.Join(bootDir, entry.Name())
			}
		}
	}

	return ""
}

// GetBootAssets extracts kernel and initrd from ISO.
func (s *ISOSource) GetBootAssets() (*types.BootAssets, error) {
	// Open ISO file
	disk, err := diskfs.Open(s.path, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, errors.Wrap(err, "open ISO")
	}
	defer disk.Close()

	// Get ISO9660 filesystem (partition 0 for ISO)
	fs, err := disk.GetFilesystem(0)
	if err != nil {
		return nil, errors.Wrap(err, "get ISO filesystem")
	}

	// Try to find UKI first (Talos uses UKI)
	ukiPath, err := findUKIInISO(fs)
	if err == nil {
		return s.extractUKIFromISO(fs, ukiPath)
	}

	// Fall back to separate kernel/initrd
	return s.extractKernelInitrdFromISO(fs)
}

// findUKIInISO searches for UKI file in ISO filesystem.
func findUKIInISO(fs filesystem.FileSystem) (string, error) {
	searchPaths := []string{
		"/EFI/BOOT",
		"/efi/boot",
	}

	for _, dir := range searchPaths {
		entries, err := fs.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			name := strings.ToLower(entry.Name())
			if strings.HasSuffix(name, ".efi") && strings.Contains(name, "vmlinuz") {
				return filepath.Join(dir, entry.Name()), nil
			}
		}
	}

	return "", errors.New("UKI not found in ISO")
}

// extractUKIFromISO extracts boot assets from UKI file in ISO.
func (s *ISOSource) extractUKIFromISO(fs filesystem.FileSystem, ukiPath string) (*types.BootAssets, error) {
	// Copy UKI to temp file
	ukiFile, err := fs.OpenFile(ukiPath, os.O_RDONLY)
	if err != nil {
		return nil, errors.Wrap(err, "open UKI in ISO")
	}
	defer ukiFile.Close()

	tmpDir, err := os.MkdirTemp("", "iso-uki-*")
	if err != nil {
		return nil, errors.Wrap(err, "create temp dir")
	}

	ukiTempPath := filepath.Join(tmpDir, "boot.efi")
	ukiOut, err := os.Create(ukiTempPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, errors.Wrap(err, "create temp UKI file")
	}

	if _, err := io.Copy(ukiOut, ukiFile); err != nil {
		ukiOut.Close()
		os.RemoveAll(tmpDir)
		return nil, errors.Wrap(err, "copy UKI")
	}
	ukiOut.Close()

	// Extract UKI sections
	ukiAssets, err := uki.Extract(ukiTempPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, errors.Wrap(err, "extract UKI")
	}

	// Read cmdline
	cmdlineBytes, err := io.ReadAll(ukiAssets.Cmdline)
	ukiAssets.Close()
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, errors.Wrap(err, "read cmdline")
	}
	cmdline := strings.TrimSpace(strings.TrimRight(string(cmdlineBytes), "\x00"))

	// Reopen for readers
	ukiAssets2, err := uki.Extract(ukiTempPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, errors.Wrap(err, "extract UKI for readers")
	}

	shared := newSharedCloser(ukiAssets2, tmpDir)

	return &types.BootAssets{
		Kernel:  &readerCloser{reader: ukiAssets2.Kernel, closer: shared},
		Initrd:  &readerCloser{reader: ukiAssets2.Initrd, closer: shared},
		Cmdline: cmdline,
	}, nil
}

// extractKernelInitrdFromISO extracts separate kernel and initrd from ISO.
func (s *ISOSource) extractKernelInitrdFromISO(fs filesystem.FileSystem) (*types.BootAssets, error) {
	// Find kernel
	kernelPath := findFileInISO(fs, kernelPaths)
	if kernelPath == "" {
		return nil, errors.New("kernel not found in ISO")
	}

	// Find initrd
	initrdPath := findFileInISO(fs, initrdPaths)
	if initrdPath == "" {
		return nil, errors.New("initrd not found in ISO")
	}

	// Copy to temp files
	tmpDir, err := os.MkdirTemp("", "iso-boot-*")
	if err != nil {
		return nil, errors.Wrap(err, "create temp dir")
	}

	kernelTempPath := filepath.Join(tmpDir, "kernel")
	if err := copyFileFromISO(fs, kernelPath, kernelTempPath); err != nil {
		os.RemoveAll(tmpDir)
		return nil, errors.Wrap(err, "copy kernel")
	}

	initrdTempPath := filepath.Join(tmpDir, "initrd")
	if err := copyFileFromISO(fs, initrdPath, initrdTempPath); err != nil {
		os.RemoveAll(tmpDir)
		return nil, errors.Wrap(err, "copy initrd")
	}

	// Open files for reading
	kernelFile, err := os.Open(kernelTempPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, errors.Wrap(err, "open kernel")
	}

	initrdFile, err := os.Open(initrdTempPath)
	if err != nil {
		kernelFile.Close()
		os.RemoveAll(tmpDir)
		return nil, errors.Wrap(err, "open initrd")
	}

	shared := newFilesCloser([]*os.File{kernelFile, initrdFile}, tmpDir)

	return &types.BootAssets{
		Kernel:  &readerCloser{reader: kernelFile, closer: shared},
		Initrd:  &readerCloser{reader: initrdFile, closer: shared},
		Cmdline: "", // No cmdline for separate kernel/initrd
	}, nil
}

// findFileInISO searches for files from a list of paths.
func findFileInISO(fs filesystem.FileSystem, paths []string) string {
	for _, path := range paths {
		f, err := fs.OpenFile("/"+path, os.O_RDONLY)
		if err == nil {
			f.Close()
			return "/" + path
		}
	}
	return ""
}

// copyFileFromISO copies a file from ISO filesystem to local path.
func copyFileFromISO(fs filesystem.FileSystem, srcPath, dstPath string) error {
	src, err := fs.OpenFile(srcPath, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err
}

// GetInstallAssets extracts the rootfs from ISO for chroot installation.
func (s *ISOSource) GetInstallAssets(tmpDir string, sizeGiB uint64) (*types.InstallAssets, error) {
	// ISO install mode is not supported - Talos ISOs don't contain installer rootfs
	return nil, errors.New("ISO source install mode not supported - use container or RAW image")
}

func (s *ISOSource) Close() error {
	return nil
}
