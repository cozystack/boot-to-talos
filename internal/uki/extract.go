package uki

import (
	"debug/pe"
	"io"
	"strings"

	"github.com/cockroachdb/errors"
)

// AssetInfo contains kernel, initrd and cmdline from UKI file.
type AssetInfo struct {
	io.Closer

	Kernel  io.Reader
	Initrd  io.Reader
	Cmdline io.Reader
}

// Extract extracts kernel, initrd and cmdline from UKI file.
func Extract(ukiPath string) (*AssetInfo, error) {
	peFile, err := pe.Open(ukiPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open PE file")
	}

	assetInfo := &AssetInfo{
		Closer: peFile,
	}

	sectionMap := map[string]*io.Reader{
		".initrd":  &assetInfo.Initrd,
		".cmdline": &assetInfo.Cmdline,
		".linux":   &assetInfo.Kernel,
	}

	for _, section := range peFile.Sections {
		// Remove null bytes from section name
		sectionName := strings.TrimRight(section.Name, "\x00")

		if reader, exists := sectionMap[sectionName]; exists && *reader == nil {
			// Use VirtualSize instead of Size to exclude alignment
			*reader = io.LimitReader(section.Open(), int64(section.VirtualSize))
		}
	}

	// Check that all required sections are found
	for name, reader := range sectionMap {
		if *reader == nil {
			peFile.Close()
			return nil, errors.Newf("%s not found in PE file", name)
		}
	}

	return assetInfo, nil
}
