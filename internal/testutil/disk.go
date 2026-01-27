package testutil

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// EFISystemPartitionGUID is the GUID for EFI System Partition.
const EFISystemPartitionGUID = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"

// CreateTestRAWImage creates a RAW disk image with GPT partition table and EFI partition.
// The EFI partition contains the provided files map (path -> content).
func CreateTestRAWImage(path string, sizeMB int64, files map[string][]byte) error {
	// Create disk image
	diskImg, err := diskfs.Create(path, sizeMB*1024*1024, diskfs.SectorSizeDefault)
	if err != nil {
		return err
	}

	// Create GPT partition table
	table := &gpt.Table{
		ProtectiveMBR: true,
		Partitions: []*gpt.Partition{
			{
				Start: 2048,
				End:   uint64(sizeMB*1024*1024/512) - 34,
				Type:  gpt.EFISystemPartition,
				Name:  "EFI",
			},
		},
	}

	if err := diskImg.Partition(table); err != nil {
		return err
	}

	// Create FAT32 filesystem on partition 1
	spec := disk.FilesystemSpec{
		Partition:   1,
		FSType:      filesystem.TypeFat32,
		VolumeLabel: "EFI",
	}

	fs, err := diskImg.CreateFilesystem(spec)
	if err != nil {
		return err
	}

	// Write files to filesystem
	for filePath, content := range files {
		// Create parent directories recursively
		dir := filepath.Dir(filePath)
		if dir != "/" && dir != "." {
			// Split path and create each directory
			parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
			currentPath := ""
			for _, part := range parts {
				currentPath = currentPath + "/" + part
				_ = fs.Mkdir(currentPath) // Ignore error if exists
			}
		}

		f, err := fs.OpenFile(filePath, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return err
		}
		if _, err := f.Write(content); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}

	return nil
}
