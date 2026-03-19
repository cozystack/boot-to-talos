//go:build linux

package efi

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestUnmarshalBootOrder(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		want    BootOrderType
		wantErr bool
	}{
		{
			name: "single entry",
			data: []byte{0x00, 0x00},
			want: BootOrderType{0},
		},
		{
			name: "multiple entries",
			data: []byte{0x01, 0x00, 0x02, 0x00, 0x03, 0x00},
			want: BootOrderType{1, 2, 3},
		},
		{
			name: "high values",
			data: []byte{0xFF, 0xFF, 0x00, 0x80},
			want: BootOrderType{0xFFFF, 0x8000},
		},
		{
			name:    "invalid odd length",
			data:    []byte{0x00, 0x00, 0x01},
			wantErr: true,
		},
		{
			name: "empty",
			data: []byte{},
			want: BootOrderType{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := unmarshalBootOrder(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("unmarshalBootOrder() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("unmarshalBootOrder() len = %d, want %d", len(got), len(tt.want))
					return
				}
				for i := range got {
					if got[i] != tt.want[i] {
						t.Errorf("unmarshalBootOrder()[%d] = %d, want %d", i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}

func TestBootOrderMarshal(t *testing.T) {
	tests := []struct {
		name string
		bo   BootOrderType
		want []byte
	}{
		{
			name: "single entry",
			bo:   BootOrderType{0},
			want: []byte{0x00, 0x00},
		},
		{
			name: "multiple entries",
			bo:   BootOrderType{1, 2, 3},
			want: []byte{0x01, 0x00, 0x02, 0x00, 0x03, 0x00},
		},
		{
			name: "high values",
			bo:   BootOrderType{0xFFFF, 0x8000},
			want: []byte{0xFF, 0xFF, 0x00, 0x80},
		},
		{
			name: "empty",
			bo:   BootOrderType{},
			want: []byte{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.bo.marshal()
			if !bytes.Equal(got, tt.want) {
				t.Errorf("BootOrderType.marshal() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBootOrderRoundTrip(t *testing.T) {
	// Test that marshal -> unmarshal preserves data
	original := BootOrderType{1, 2, 3, 0x1234, 0xABCD}
	marshaled := original.marshal()
	unmarshaled, err := unmarshalBootOrder(marshaled)
	if err != nil {
		t.Fatalf("unmarshalBootOrder() error: %v", err)
	}

	if len(unmarshaled) != len(original) {
		t.Fatalf("Round trip changed length: got %d, want %d", len(unmarshaled), len(original))
	}

	for i := range original {
		if unmarshaled[i] != original[i] {
			t.Errorf("Round trip changed value at %d: got %d, want %d", i, unmarshaled[i], original[i])
		}
	}
}

func TestVarPath(t *testing.T) {
	testUUID := uuid.MustParse("8be4df61-93ca-11d2-aa0d-00e098032b8c")

	tests := []struct {
		name    string
		scope   uuid.UUID
		varName string
		want    string
	}{
		{
			name:    "BootOrder",
			scope:   testUUID,
			varName: "BootOrder",
			want:    "/sys/firmware/efi/efivars/BootOrder-8be4df61-93ca-11d2-aa0d-00e098032b8c",
		},
		{
			name:    "Boot0000",
			scope:   testUUID,
			varName: "Boot0000",
			want:    "/sys/firmware/efi/efivars/Boot0000-8be4df61-93ca-11d2-aa0d-00e098032b8c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := varPath(tt.scope, tt.varName)
			if got != tt.want {
				t.Errorf("varPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBootVarRegexp(t *testing.T) {
	tests := []struct {
		input string
		match bool
		num   string
	}{
		{"Boot0000", true, "0000"},
		{"Boot0001", true, "0001"},
		{"BootFFFF", true, "FFFF"},
		{"Boot00ff", true, "00ff"},
		{"Boot1234", true, "1234"},
		{"BootOrder", false, ""},
		{"Boot", false, ""},
		{"Boot12345", false, ""},
		{"Boot123", false, ""},
		{"boot0000", false, ""},
		{"NotBoot0000", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			matches := bootVarRegexp.FindStringSubmatch(tt.input)
			if tt.match {
				if matches == nil {
					t.Errorf("bootVarRegexp should match %q", tt.input)
				} else if len(matches) > 1 && matches[1] != tt.num {
					t.Errorf("bootVarRegexp captured %q, want %q", matches[1], tt.num)
				}
			} else {
				if matches != nil {
					t.Errorf("bootVarRegexp should not match %q", tt.input)
				}
			}
		})
	}
}

func TestEfiAttributes(t *testing.T) {
	// Test attribute combinations
	attrs := attrNonVolatile | attrRuntimeAccess

	// When runtime access is set, boot service access should also be set
	// (this is handled in Write, but we test the constants)
	if attrNonVolatile != 1 {
		t.Errorf("attrNonVolatile = %d, want 1", attrNonVolatile)
	}
	if attrBootserviceAccess != 2 {
		t.Errorf("attrBootserviceAccess = %d, want 2", attrBootserviceAccess)
	}
	if attrRuntimeAccess != 4 {
		t.Errorf("attrRuntimeAccess = %d, want 4", attrRuntimeAccess)
	}

	// Combined value
	if attrs != 5 {
		t.Errorf("attrNonVolatile | attrRuntimeAccess = %d, want 5", attrs)
	}
}

func TestEfiAttributeFormat(t *testing.T) {
	// Test that attributes are correctly encoded in the 4-byte prefix
	attrs := efiAttribute(attrNonVolatile | attrBootserviceAccess | attrRuntimeAccess)
	value := []byte("test")

	// Build buffer as in Write()
	buf := make([]byte, len(value)+4)
	binary.LittleEndian.PutUint32(buf[:4], uint32(attrs))
	copy(buf[4:], value)

	// Verify structure
	gotAttrs := binary.LittleEndian.Uint32(buf[:4])
	if gotAttrs != 7 { // 1 + 2 + 4
		t.Errorf("Encoded attributes = %d, want 7", gotAttrs)
	}

	gotValue := buf[4:]
	if string(gotValue) != "test" {
		t.Errorf("Encoded value = %q, want %q", string(gotValue), "test")
	}
}

func TestScopeGlobalUUID(t *testing.T) {
	// Verify the global scope UUID is correct
	// This is the EFI Global Variable GUID from UEFI spec
	expected := "8be4df61-93ca-11d2-aa0d-00e098032b8c"
	if scopeGlobal.String() != expected {
		t.Errorf("scopeGlobal = %q, want %q", scopeGlobal.String(), expected)
	}
}

func TestGUIDToMixedEndian(t *testing.T) {
	// UUID from Talos boot_test.go: 15e39a00-1dd2-1000-8d7f-00a0c92408fc
	// Expected mixed-endian bytes (from the test hex dump):
	// 00 9A E3 15 D2 1D 00 10 8D 7F 00 A0 C9 24 08 FC
	id := uuid.MustParse("15e39a00-1dd2-1000-8d7f-00a0c92408fc")
	got := guidToMixedEndian(id)
	want := []byte{
		0x00, 0x9A, 0xE3, 0x15, // time_low reversed
		0xD2, 0x1D, // time_mid reversed
		0x00, 0x10, // time_hi reversed
		0x8D, 0x7F, 0x00, 0xA0, 0xC9, 0x24, 0x08, 0xFC, // as-is
	}

	if !bytes.Equal(got, want) {
		t.Errorf("guidToMixedEndian() =\n  %X\nwant:\n  %X", got, want)
	}
}

func TestEndOfDevicePathData(t *testing.T) {
	e := &endOfDevicePath{}
	got, err := e.data()
	if err != nil {
		t.Fatalf("data() error: %v", err)
	}

	want := []byte{0x7F, 0xFF, 0x04, 0x00}
	if !bytes.Equal(got, want) {
		t.Errorf("endOfDevicePath.data() = %X, want %X", got, want)
	}
}

func TestHardDrivePathData(t *testing.T) {
	// Values from Talos boot_test.go example
	h := &hardDrivePath{
		PartitionNumber:    1,
		PartitionStart:     5,
		PartitionSize:      8,
		PartitionSignature: uuid.MustParse("15e39a00-1dd2-1000-8d7f-00a0c92408fc"),
	}

	got, err := h.data()
	if err != nil {
		t.Fatalf("data() error: %v", err)
	}

	if len(got) != hardDrivePathLen {
		t.Fatalf("len = %d, want %d", len(got), hardDrivePathLen)
	}

	// Check header
	if got[0] != 0x04 || got[1] != 0x01 {
		t.Errorf("type/subtype = %02X/%02X, want 04/01", got[0], got[1])
	}

	if binary.LittleEndian.Uint16(got[2:4]) != hardDrivePathLen {
		t.Errorf("length = %d, want %d", binary.LittleEndian.Uint16(got[2:4]), hardDrivePathLen)
	}

	// Check partition number
	if binary.LittleEndian.Uint32(got[4:8]) != 1 {
		t.Errorf("partition number = %d, want 1", binary.LittleEndian.Uint32(got[4:8]))
	}

	// Check start/size
	if binary.LittleEndian.Uint64(got[8:16]) != 5 {
		t.Errorf("start = %d, want 5", binary.LittleEndian.Uint64(got[8:16]))
	}

	if binary.LittleEndian.Uint64(got[16:24]) != 8 {
		t.Errorf("size = %d, want 8", binary.LittleEndian.Uint64(got[16:24]))
	}

	// Check GUID in mixed-endian
	wantGUID := []byte{0x00, 0x9A, 0xE3, 0x15, 0xD2, 0x1D, 0x00, 0x10, 0x8D, 0x7F, 0x00, 0xA0, 0xC9, 0x24, 0x08, 0xFC}
	if !bytes.Equal(got[24:40], wantGUID) {
		t.Errorf("GUID = %X, want %X", got[24:40], wantGUID)
	}

	// Check GPT signature type
	if got[40] != 0x02 || got[41] != 0x02 {
		t.Errorf("MBR/sig type = %02X/%02X, want 02/02", got[40], got[41])
	}
}

func TestFilePathElemData(t *testing.T) {
	f := &filePathElem{Path: `\EFI\boot\BOOTX64.efi`}
	got, err := f.data()
	if err != nil {
		t.Fatalf("data() error: %v", err)
	}

	// Check header
	if got[0] != 0x04 || got[1] != 0x04 {
		t.Errorf("type/subtype = %02X/%02X, want 04/04", got[0], got[1])
	}

	totalLen := binary.LittleEndian.Uint16(got[2:4])
	if int(totalLen) != len(got) {
		t.Errorf("length field = %d, actual = %d", totalLen, len(got))
	}

	// Check that the path ends with null terminator (0x00 0x00)
	if got[len(got)-1] != 0x00 || got[len(got)-2] != 0x00 {
		t.Errorf("missing null terminator at end: %X", got[len(got)-2:])
	}
}

func TestFilePathForwardSlashConversion(t *testing.T) {
	f := &filePathElem{Path: `/EFI/boot/BOOTX64.efi`}
	got, err := f.data()
	if err != nil {
		t.Fatalf("data() error: %v", err)
	}

	// Decode the path back and verify backslashes
	pathBytes := got[4 : len(got)-2] // skip header and null terminator
	decoded, err := efiEncoding.NewDecoder().Bytes(pathBytes)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if string(decoded) != `\EFI\boot\BOOTX64.efi` {
		t.Errorf("decoded path = %q, want %q", string(decoded), `\EFI\boot\BOOTX64.efi`)
	}
}

func TestLoadOptionMarshal(t *testing.T) {
	opt := &loadOption{
		Description: "Test",
		FilePath: devicePath{
			&hardDrivePath{
				PartitionNumber:    1,
				PartitionStart:     2048,
				PartitionSize:      614400,
				PartitionSignature: uuid.MustParse("fa7141e7-c13a-4788-acd6-8a841eeca4e1"),
			},
			&filePathElem{Path: `\EFI\boot\BOOTX64.efi`},
			&endOfDevicePath{},
		},
	}

	data, err := opt.marshal()
	if err != nil {
		t.Fatalf("marshal() error: %v", err)
	}

	// Verify attributes (LOAD_OPTION_ACTIVE = 0x01)
	attrs := binary.LittleEndian.Uint32(data[0:4])
	if attrs != 0x01 {
		t.Errorf("attributes = 0x%08X, want 0x00000001", attrs)
	}

	// Verify FilePathListLength at bytes 4-5
	fpLen := binary.LittleEndian.Uint16(data[4:6])
	if fpLen == 0 {
		t.Error("FilePathListLength = 0, expected non-zero")
	}

	// Round-trip: unmarshal should recover the description
	unmarshaled, err := unmarshalLoadOption(data)
	if err != nil {
		t.Fatalf("unmarshalLoadOption() error: %v", err)
	}

	if unmarshaled.Description != "Test" {
		t.Errorf("round-trip description = %q, want %q", unmarshaled.Description, "Test")
	}
}

// mockEFIReadWriter is an in-memory efiReadWriter for testing.
type mockEFIReadWriter struct {
	vars map[string]mockVar
}

type mockVar struct {
	data  []byte
	attrs efiAttribute
}

func newMockEFIReadWriter() *mockEFIReadWriter {
	return &mockEFIReadWriter{vars: make(map[string]mockVar)}
}

func (m *mockEFIReadWriter) Write(scope uuid.UUID, varName string, attrs efiAttribute, value []byte) error {
	key := varName + "-" + scope.String()
	m.vars[key] = mockVar{data: append([]byte(nil), value...), attrs: attrs}

	return nil
}

func (m *mockEFIReadWriter) Read(scope uuid.UUID, varName string) ([]byte, efiAttribute, error) {
	key := varName + "-" + scope.String()
	v, ok := m.vars[key]
	if !ok {
		return nil, 0, &fakeNotExistError{name: key}
	}

	return v.data, v.attrs, nil
}

func (m *mockEFIReadWriter) Delete(scope uuid.UUID, varName string) error {
	key := varName + "-" + scope.String()
	delete(m.vars, key)

	return nil
}

func (m *mockEFIReadWriter) List(scope uuid.UUID) ([]string, error) {
	suffix := "-" + scope.String()
	var names []string

	for key := range m.vars {
		if name, ok := strings.CutSuffix(key, suffix); ok {
			names = append(names, name)
		}
	}

	return names, nil
}

type fakeNotExistError struct{ name string }

func (e *fakeNotExistError) Error() string { return "not found: " + e.name }
func (e *fakeNotExistError) Is(target error) bool {
	return target == fs.ErrNotExist //nolint:errorlint // intentional sentinel comparison
}

func TestFindFreeBootIndex(t *testing.T) {
	mock := newMockEFIReadWriter()

	// Write some existing entries
	_ = mock.Write(scopeGlobal, "Boot0000", 0, []byte("x"))
	_ = mock.Write(scopeGlobal, "Boot0001", 0, []byte("x"))
	_ = mock.Write(scopeGlobal, "Boot0003", 0, []byte("x"))

	idx, err := findFreeBootIndex(mock)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if idx != 2 {
		t.Errorf("findFreeBootIndex() = %d, want 2", idx)
	}
}

func TestFindFreeBootIndexEmpty(t *testing.T) {
	mock := newMockEFIReadWriter()

	idx, err := findFreeBootIndex(mock)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if idx != 0 {
		t.Errorf("findFreeBootIndex() = %d, want 0", idx)
	}
}

func TestSetBootEntry(t *testing.T) {
	mock := newMockEFIReadWriter()

	opt := &loadOption{
		Description: talosBootEntryDescription,
		FilePath: devicePath{
			&endOfDevicePath{},
		},
	}

	err := setBootEntry(mock, 5, opt)
	if err != nil {
		t.Fatalf("setBootEntry() error: %v", err)
	}

	// Verify the variable was written
	key := "Boot0005-" + scopeGlobal.String()
	v, ok := mock.vars[key]
	if !ok {
		t.Fatal("Boot0005 was not written")
	}

	// Unmarshal and check description
	parsed, err := unmarshalLoadOption(v.data)
	if err != nil {
		t.Fatalf("unmarshalLoadOption() error: %v", err)
	}

	if parsed.Description != talosBootEntryDescription {
		t.Errorf("description = %q, want %q", parsed.Description, talosBootEntryDescription)
	}
}

// TestLoadOptionMarshalMatchesTalosFormat verifies our marshaling against the
// known hex dump from Talos boot_test.go.
func TestLoadOptionMarshalMatchesTalosFormat(t *testing.T) {
	opt := &loadOption{
		Description: "Example",
		FilePath: devicePath{
			&hardDrivePath{
				PartitionNumber:    1,
				PartitionStart:     5,
				PartitionSize:      8,
				PartitionSignature: uuid.MustParse("15e39a00-1dd2-1000-8d7f-00a0c92408fc"),
			},
			&filePathElem{Path: `\test\a.efi`},
			&endOfDevicePath{},
		},
	}

	data, err := opt.marshal()
	if err != nil {
		t.Fatalf("marshal() error: %v", err)
	}

	// Known expected bytes from Talos boot_test.go
	expected := []byte{
		// Attributes: LOAD_OPTION_ACTIVE
		0x01, 0x00, 0x00, 0x00,
		// FilePathListLength
		0x4A, 0x00,
		// Description "Example" in UTF-16LE + null
		0x45, 0x00, 0x78, 0x00, 0x61, 0x00, 0x6D, 0x00,
		0x70, 0x00, 0x6C, 0x00, 0x65, 0x00,
		0x00, 0x00,
		// HardDrivePath
		0x04, 0x01, 0x2A, 0x00,
		0x01, 0x00, 0x00, 0x00,
		0x05, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x9A, 0xE3, 0x15, 0xD2, 0x1D, 0x00, 0x10,
		0x8D, 0x7F, 0x00, 0xA0, 0xC9, 0x24, 0x08, 0xFC,
		0x02, 0x02,
		// FilePath "\test\a.efi"
		0x04, 0x04, 0x1C, 0x00,
		0x5C, 0x00, 0x74, 0x00, 0x65, 0x00, 0x73, 0x00,
		0x74, 0x00, 0x5C, 0x00, 0x61, 0x00, 0x2E, 0x00,
		0x65, 0x00, 0x66, 0x00, 0x69, 0x00, 0x00, 0x00,
		// End of Device Path
		0x7F, 0xFF, 0x04, 0x00,
	}

	if !bytes.Equal(data, expected) {
		t.Errorf("marshal() output does not match Talos format\ngot:\n  %X\nwant:\n  %X", data, expected)
	}
}
