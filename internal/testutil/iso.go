package testutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// errISOUnexpectedFSType is returned when disk.CreateFilesystem yields a
// concrete filesystem implementation other than *iso9660.FileSystem. Defined
// as a sentinel so callers can match it with errors.Is in tests.
var errISOUnexpectedFSType = errors.New("expected *iso9660.FileSystem from CreateFilesystem")

// CreateTestISOImage creates an ISO image with the provided files using Rock
// Ridge so input filenames round-trip in their original case.
func CreateTestISOImage(path string, files map[string][]byte) error {
	return createTestISOImage(path, files, true)
}

// CreateTestISOImagePlain creates an ISO image with the provided files without
// Rock Ridge. Filenames are upper-cased on disk per the bare ISO9660 spec; use
// this variant to exercise case-folding fallbacks in callers that consume the
// image.
func CreateTestISOImagePlain(path string, files map[string][]byte) error {
	return createTestISOImage(path, files, false)
}

// createTestISOImage is the shared implementation. ISO9660 requires a 2048-byte
// sector size (enforced since go-diskfs v1.9), so the disk is created with that
// sector size explicitly rather than SectorSizeDefault.
func createTestISOImage(path string, files map[string][]byte, rockRidge bool) error {
	// Create disk image - ISO needs minimum size
	diskSize := int64(10 * 1024 * 1024) // 10MB minimum for ISO
	const isoSectorSize diskfs.SectorSize = 2048
	diskImg, err := diskfs.Create(path, diskSize, isoSectorSize)
	if err != nil {
		return err
	}

	// Create ISO9660 filesystem
	spec := disk.FilesystemSpec{
		Partition:   0, // ISO doesn't use partitions
		FSType:      filesystem.TypeISO9660,
		VolumeLabel: "TEST",
	}

	fs, err := diskImg.CreateFilesystem(spec)
	if err != nil {
		return err
	}

	isoFS, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return fmt.Errorf("%w, got %T", errISOUnexpectedFSType, fs)
	}

	// Write files. go-diskfs v1.9+ follows io/fs.ValidPath and rejects paths
	// with a leading slash; strip it before every Mkdir/OpenFile.
	for filePath, content := range files {
		relPath := strings.TrimPrefix(filePath, "/")
		if dir := filepath.Dir(relPath); dir != "." && dir != "" {
			// Ignore mkdir errors - directory may exist
			_ = isoFS.Mkdir(dir)
		}

		f, err := isoFS.OpenFile(relPath, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return err
		}
		if _, err := f.Write(content); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}

	// Finalize ISO. With Rock Ridge enabled, input filenames round-trip in
	// their original case; without it ISO9660 forces names to upper-case and
	// case-sensitive look-ups via fs.OpenFile must compensate via the
	// case-folding fallback in findFileInISO.
	if err := isoFS.Finalize(iso9660.FinalizeOptions{RockRidge: rockRidge}); err != nil {
		return err
	}

	return nil
}
