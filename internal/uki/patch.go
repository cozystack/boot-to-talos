package uki

import (
	"debug/pe"
	"io"
	"os"
	"strings"

	"github.com/cockroachdb/errors"
)

// ReadCmdline reads the kernel command line from a UKI PE file.
func ReadCmdline(ukiPath string) (string, error) {
	peFile, err := pe.Open(ukiPath)
	if err != nil {
		return "", errors.Wrap(err, "open PE file")
	}
	defer peFile.Close()

	for _, section := range peFile.Sections {
		if section.Name == ".cmdline" {
			data, err := io.ReadAll(io.LimitReader(section.Open(), int64(section.VirtualSize)))
			if err != nil {
				return "", errors.Wrap(err, "read .cmdline section")
			}
			// Trim null bytes
			return strings.TrimRight(string(data), "\x00"), nil
		}
	}

	return "", errors.New(".cmdline section not found in PE file")
}

// PatchCmdline patches the kernel command line in a UKI PE file.
// The new cmdline must fit within the existing section size.
func PatchCmdline(ukiPath, newCmdline string) error {
	// First, read PE to find the .cmdline section
	peFile, err := pe.Open(ukiPath)
	if err != nil {
		return errors.Wrap(err, "open PE file")
	}

	var cmdlineSection *pe.Section
	for _, section := range peFile.Sections {
		if section.Name == ".cmdline" {
			cmdlineSection = section
			break
		}
	}

	if cmdlineSection == nil {
		peFile.Close()
		return errors.New(".cmdline section not found in PE file")
	}

	// Check if new cmdline fits
	maxSize := int(cmdlineSection.Size)
	if maxSize == 0 {
		maxSize = int(cmdlineSection.VirtualSize)
	}

	if len(newCmdline) > maxSize {
		peFile.Close()
		return errors.Newf("new cmdline too long: %d bytes, max %d bytes", len(newCmdline), maxSize)
	}

	offset := int64(cmdlineSection.Offset)
	peFile.Close()

	// Open file for writing
	file, err := os.OpenFile(ukiPath, os.O_RDWR, 0)
	if err != nil {
		return errors.Wrap(err, "open file for writing")
	}
	defer file.Close()

	// Seek to section
	if _, err := file.Seek(offset, 0); err != nil {
		return errors.Wrap(err, "seek to .cmdline")
	}

	// Prepare data (null-padded to original size)
	data := make([]byte, maxSize)
	copy(data, newCmdline)

	// Write
	if _, err := file.Write(data); err != nil {
		return errors.Wrap(err, "write .cmdline")
	}

	return nil
}
