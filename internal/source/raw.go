package source

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/diskfs/go-diskfs"
	diskType "github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/cozystack/boot-to-talos/internal/types"
	"github.com/cozystack/boot-to-talos/internal/uki"
)

// RAWSource implements ImageSource for RAW disk images.
type RAWSource struct {
	path string
}

// NewRAWSource creates a new RAWSource.
func NewRAWSource(path string) *RAWSource {
	return &RAWSource{path: path}
}

func (s *RAWSource) Type() types.ImageSourceType {
	return types.ImageSourceRAW
}

func (s *RAWSource) Reference() string {
	return s.path
}

// DetectCompression returns the compression type based on file extension.
func DetectCompression(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".xz"):
		return "xz"
	case strings.HasSuffix(lower, ".gz"):
		return "gz"
	case strings.HasSuffix(lower, ".zst"):
		return "zst"
	default:
		return ""
	}
}

// OpenDecompressed opens a file and returns a decompressed reader.
// Returns the reader, uncompressed size (-1 if unknown), and error.
func OpenDecompressed(path string) (io.ReadCloser, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, errors.Wrapf(err, "open %s", path)
	}

	compression := DetectCompression(path)
	switch compression {
	case "xz":
		reader, err := xz.NewReader(file)
		if err != nil {
			file.Close()
			return nil, 0, errors.Wrap(err, "xz reader")
		}
		// Wrap to close underlying file when done
		return &xzReadCloser{reader: reader, file: file}, -1, nil

	case "gz":
		reader, err := gzip.NewReader(file)
		if err != nil {
			file.Close()
			return nil, 0, errors.Wrap(err, "gzip reader")
		}
		// gzip.Reader.Close() only closes the gzip reader, not the file
		return &gzReadCloser{reader: reader, file: file}, -1, nil

	case "zst":
		reader, err := zstd.NewReader(file)
		if err != nil {
			file.Close()
			return nil, 0, errors.Wrap(err, "zstd reader")
		}
		return &zstReadCloser{reader: reader, file: file}, -1, nil

	default:
		// Uncompressed - return file directly with its size
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, 0, errors.Wrapf(err, "stat %s", path)
		}
		return file, info.Size(), nil
	}
}

// xzReadCloser wraps xz.Reader to close underlying file.
type xzReadCloser struct {
	reader *xz.Reader
	file   *os.File
}

func (r *xzReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *xzReadCloser) Close() error {
	return r.file.Close()
}

// gzReadCloser wraps gzip.Reader to close underlying file.
type gzReadCloser struct {
	reader *gzip.Reader
	file   *os.File
}

func (r *gzReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *gzReadCloser) Close() error {
	r.reader.Close()
	return r.file.Close()
}

// zstReadCloser wraps zstd.Decoder to close underlying file.
type zstReadCloser struct {
	reader *zstd.Decoder
	file   *os.File
}

func (r *zstReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *zstReadCloser) Close() error {
	r.reader.Close()
	return r.file.Close()
}

// GetBootAssets extracts kernel and initrd from UKI in RAW image.
func (s *RAWSource) GetBootAssets() (*types.BootAssets, error) {
	// Prepare image path (decompress if needed)
	imagePath, tempImageDir, err := s.prepareImagePath()
	if err != nil {
		return nil, err
	}

	cleanup := func() {
		if tempImageDir != "" {
			os.RemoveAll(tempImageDir)
		}
	}

	// Extract UKI from disk image to temp file
	ukiTempPath, ukiTempDir, err := extractUKIFromDisk(imagePath)
	if err != nil {
		cleanup()
		return nil, err
	}

	// Build boot assets from UKI
	assets, err := buildBootAssetsFromUKI(ukiTempPath, ukiTempDir, tempImageDir)
	if err != nil {
		os.RemoveAll(ukiTempDir)
		cleanup()
		return nil, err
	}

	return assets, nil
}

// prepareImagePath returns path to uncompressed image, decompressing if needed.
// Returns: imagePath, tempDir (empty if no temp created), error.
func (s *RAWSource) prepareImagePath() (string, string, error) {
	if DetectCompression(s.path) == "" {
		return s.path, "", nil
	}

	tmpDir, err := os.MkdirTemp("", "raw-source-*")
	if err != nil {
		return "", "", errors.Wrap(err, "create temp dir")
	}

	tempFile := filepath.Join(tmpDir, "image.raw")
	if err := decompressFile(s.path, tempFile); err != nil {
		os.RemoveAll(tmpDir)
		return "", "", err
	}

	return tempFile, tmpDir, nil
}

// decompressFile decompresses src to dst.
func decompressFile(src, dst string) error {
	reader, _, err := OpenDecompressed(src)
	if err != nil {
		return errors.Wrap(err, "open compressed image")
	}
	defer reader.Close()

	out, err := os.Create(dst)
	if err != nil {
		return errors.Wrap(err, "create temp file")
	}
	defer out.Close()

	if _, err := io.Copy(out, reader); err != nil {
		return errors.Wrap(err, "decompress image")
	}

	return nil
}

// extractUKIFromDisk opens disk image, finds EFI partition, extracts UKI to temp.
// Returns: ukiTempPath, ukiTempDir, error.
func extractUKIFromDisk(imagePath string) (string, string, error) {
	disk, err := diskfs.Open(imagePath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return "", "", errors.Wrap(err, "open disk image")
	}
	defer disk.Close()

	// Find EFI partition
	efiPartNum, err := findEFIPartition(disk)
	if err != nil {
		return "", "", err
	}

	// Get filesystem and find UKI
	fs, err := disk.GetFilesystem(efiPartNum)
	if err != nil {
		return "", "", errors.Wrap(err, "get EFI filesystem")
	}

	ukiPath, err := findUKIFile(fs)
	if err != nil {
		return "", "", errors.Wrap(err, "find UKI file")
	}

	// Copy UKI to temp file
	return copyUKIToTemp(fs, ukiPath)
}

// findEFIPartition finds EFI System Partition number in GPT table.
func findEFIPartition(disk *diskType.Disk) (int, error) {
	table, err := disk.GetPartitionTable()
	if err != nil {
		return 0, errors.Wrap(err, "get partition table")
	}

	gptTable, ok := table.(*gpt.Table)
	if !ok {
		return 0, errors.New("disk does not have GPT partition table")
	}

	for i, part := range gptTable.Partitions {
		if part == nil {
			continue
		}
		if part.Type == gpt.EFISystemPartition {
			return i + 1, nil
		}
	}

	return 0, errors.New("EFI partition not found")
}

// copyUKIToTemp copies UKI file from filesystem to temp directory.
func copyUKIToTemp(fs filesystem.FileSystem, ukiPath string) (string, string, error) {
	ukiFile, err := fs.OpenFile(ukiPath, os.O_RDONLY)
	if err != nil {
		return "", "", errors.Wrapf(err, "open UKI file %s", ukiPath)
	}
	defer ukiFile.Close()

	ukiTempDir, err := os.MkdirTemp("", "uki-extract-*")
	if err != nil {
		return "", "", errors.Wrap(err, "create UKI temp dir")
	}

	ukiTempPath := filepath.Join(ukiTempDir, "boot.efi")
	ukiOut, err := os.Create(ukiTempPath)
	if err != nil {
		os.RemoveAll(ukiTempDir)
		return "", "", errors.Wrap(err, "create UKI temp file")
	}
	defer ukiOut.Close()

	if _, err := io.Copy(ukiOut, ukiFile); err != nil {
		os.RemoveAll(ukiTempDir)
		return "", "", errors.Wrap(err, "copy UKI file")
	}

	return ukiTempPath, ukiTempDir, nil
}

// buildBootAssetsFromUKI extracts UKI and creates BootAssets.
func buildBootAssetsFromUKI(ukiTempPath, ukiTempDir, imageDir string) (assets *types.BootAssets, err error) {
	// Extract to read cmdline
	ukiAssets, err := uki.Extract(ukiTempPath)
	if err != nil {
		return nil, errors.Wrap(err, "extract UKI")
	}

	cmdlineBytes, err := io.ReadAll(ukiAssets.Cmdline)
	ukiAssets.Close()
	if err != nil {
		return nil, errors.Wrap(err, "read cmdline")
	}
	cmdline := strings.TrimSpace(strings.TrimRight(string(cmdlineBytes), "\x00"))

	// Reopen for readers
	ukiAssets2, err := uki.Extract(ukiTempPath)
	if err != nil {
		return nil, errors.Wrap(err, "extract UKI for readers")
	}
	// Close ukiAssets2 if we return an error after this point
	defer func() {
		if err != nil {
			ukiAssets2.Close()
		}
	}()

	cleanupDirs := []string{ukiTempDir}
	if imageDir != "" {
		cleanupDirs = append(cleanupDirs, imageDir)
	}
	shared := newSharedCloserMulti(ukiAssets2, cleanupDirs)

	return &types.BootAssets{
		Kernel:  &readerCloser{reader: ukiAssets2.Kernel, closer: shared},
		Initrd:  &readerCloser{reader: ukiAssets2.Initrd, closer: shared},
		Cmdline: cmdline,
	}, nil
}

// findUKIFile searches for UKI file in common locations.
func findUKIFile(fs interface {
	ReadDir(path string) ([]os.FileInfo, error)
},
) (string, error) {
	// Common UKI locations
	searchPaths := []string{
		"/EFI/BOOT",
		"/EFI/boot",
		"/efi/boot",
	}

	for _, dir := range searchPaths {
		entries, err := fs.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			name := strings.ToLower(entry.Name())
			if strings.HasSuffix(name, ".efi") {
				return filepath.Join(dir, entry.Name()), nil
			}
		}
	}

	return "", errors.New("UKI file not found in EFI partition")
}

// rawSharedCloser handles cleanup for RAW source boot assets.

// GetInstallAssets returns the RAW image for direct writing to disk.
func (s *RAWSource) GetInstallAssets(tmpDir string, sizeGiB uint64) (*types.InstallAssets, error) {
	reader, size, err := OpenDecompressed(s.path)
	if err != nil {
		return nil, errors.Wrap(err, "open RAW image")
	}

	return &types.InstallAssets{
		DiskImage:     reader,
		DiskImageSize: size,
		Cleanup:       nil, // reader.Close() handles cleanup
	}, nil
}

func (s *RAWSource) Close() error {
	return nil
}
