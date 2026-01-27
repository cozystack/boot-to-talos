package testutil

import (
	"os"
	"path/filepath"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// CreateTestISOImage creates an ISO image with the provided files.
// Files map is path -> content.
func CreateTestISOImage(path string, files map[string][]byte) error {
	// Create disk image - ISO needs minimum size
	diskSize := int64(10 * 1024 * 1024) // 10MB minimum for ISO
	diskImg, err := diskfs.Create(path, diskSize, diskfs.SectorSizeDefault)
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
		return err
	}

	// Write files
	for filePath, content := range files {
		if dir := filepath.Dir(filePath); dir != "." && dir != "/" {
			// Ignore mkdir errors - directory may exist
			_ = isoFS.Mkdir(dir)
		}

		f, err := isoFS.OpenFile(filePath, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return err
		}
		if _, err := f.Write(content); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}

	// Finalize ISO
	if err := isoFS.Finalize(iso9660.FinalizeOptions{}); err != nil {
		return err
	}

	return nil
}
