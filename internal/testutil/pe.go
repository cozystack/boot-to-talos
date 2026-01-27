package testutil

import (
	"encoding/binary"
	"os"
	"sort"
)

// CreateTestUKIFile creates a minimal PE file with .linux, .initrd, .cmdline sections for testing.
func CreateTestUKIFile(path, cmdline, kernel, initrd string) error {
	sections := map[string][]byte{
		".linux":   []byte(kernel),
		".initrd":  []byte(initrd),
		".cmdline": []byte(cmdline),
	}
	return CreateMinimalPEFile(path, sections)
}

// CreateMinimalPEFile creates a PE file with given sections.
// This is a helper for testing - creates valid PE structure.
func CreateMinimalPEFile(path string, sections map[string][]byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// DOS header (64 bytes)
	dosHeader := make([]byte, 64)
	dosHeader[0] = 'M'
	dosHeader[1] = 'Z'
	binary.LittleEndian.PutUint32(dosHeader[60:], 64) // PE offset at 0x3C

	// PE signature (4 bytes)
	peSignature := []byte{'P', 'E', 0, 0}

	// COFF header (20 bytes)
	numSections := uint16(len(sections))
	coffHeader := make([]byte, 20)
	binary.LittleEndian.PutUint16(coffHeader[0:], 0x8664)      // AMD64
	binary.LittleEndian.PutUint16(coffHeader[2:], numSections) // Number of sections
	binary.LittleEndian.PutUint16(coffHeader[16:], 112)        // Optional header size
	binary.LittleEndian.PutUint16(coffHeader[18:], 0x22)       // Characteristics

	// Optional header (112 bytes for PE32+)
	optHeader := make([]byte, 112)
	binary.LittleEndian.PutUint16(optHeader[0:], 0x20b) // PE32+ magic
	optHeader[2] = 1                                    // Major linker version

	// Calculate data start (aligned to 512)
	headerSize := 64 + 4 + 20 + 112 + int(numSections)*40
	dataStart := ((headerSize + 511) / 512) * 512

	// Section headers (40 bytes each)
	sectionHeaders := make([]byte, 0, int(numSections)*40)
	sectionNames := make([]string, 0, len(sections))
	for name := range sections {
		sectionNames = append(sectionNames, name)
	}
	sort.Strings(sectionNames) // Ensure deterministic order

	currentOffset := dataStart
	for _, name := range sectionNames {
		data := sections[name]
		hdr := make([]byte, 40)

		// Name (8 bytes, null-padded)
		copy(hdr[0:8], name)

		// VirtualSize
		binary.LittleEndian.PutUint32(hdr[8:], uint32(len(data)))
		// VirtualAddress
		binary.LittleEndian.PutUint32(hdr[12:], uint32(currentOffset))
		// SizeOfRawData (aligned to 512)
		rawSize := ((len(data) + 511) / 512) * 512
		binary.LittleEndian.PutUint32(hdr[16:], uint32(rawSize))
		// PointerToRawData
		binary.LittleEndian.PutUint32(hdr[20:], uint32(currentOffset))

		sectionHeaders = append(sectionHeaders, hdr...)
		currentOffset += rawSize
	}

	// Write everything
	if _, err := f.Write(dosHeader); err != nil {
		return err
	}
	if _, err := f.Write(peSignature); err != nil {
		return err
	}
	if _, err := f.Write(coffHeader); err != nil {
		return err
	}
	if _, err := f.Write(optHeader); err != nil {
		return err
	}
	if _, err := f.Write(sectionHeaders); err != nil {
		return err
	}

	// Pad to data start
	padding := make([]byte, dataStart-headerSize)
	if _, err := f.Write(padding); err != nil {
		return err
	}

	// Write section data
	for _, name := range sectionNames {
		data := sections[name]
		if _, err := f.Write(data); err != nil {
			return err
		}
		// Pad to 512 boundary
		rawSize := ((len(data) + 511) / 512) * 512
		sectionPadding := make([]byte, rawSize-len(data))
		if _, err := f.Write(sectionPadding); err != nil {
			return err
		}
	}

	return nil
}
