//go:build linux

package efi

import (
	"bytes"
	"encoding/binary"
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
