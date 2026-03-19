//go:build linux

package efi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"github.com/cockroachdb/errors"
	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"golang.org/x/text/encoding/unicode"
)

const (
	efiVarsMountPoint = "/sys/firmware/efi/efivars"
	efiVarsPath       = "/sys/firmware/efi/efivars"
)

//nolint:gochecknoglobals
var scopeGlobal = uuid.MustParse("8be4df61-93ca-11d2-aa0d-00e098032b8c")

//nolint:gochecknoglobals
var efiEncoding = unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)

type efiAttribute uint32

const (
	attrNonVolatile efiAttribute = 1 << iota
	attrBootserviceAccess
	attrRuntimeAccess
)

// IsUEFIBoot returns true if the system is booted using UEFI.
func IsUEFIBoot() bool {
	_, err := os.Stat("/sys/firmware/efi")
	return err == nil
}

// SecureBootState represents the current Secure Boot status.
type SecureBootState struct {
	Enabled   bool // SecureBoot variable is 1
	SetupMode bool // SetupMode variable is 1 (keys can be enrolled without authentication)
}

// GetSecureBootState reads the current Secure Boot state from UEFI variables.
// Returns error if not running on UEFI system or variables cannot be read.
func GetSecureBootState() (SecureBootState, error) {
	if !IsUEFIBoot() {
		return SecureBootState{}, errors.New("not a UEFI system")
	}

	state := SecureBootState{}

	// Read SecureBoot variable (1 = enabled, 0 = disabled)
	// Format: 4 bytes attributes + 1 byte value
	sbPath := fmt.Sprintf("%s/SecureBoot-%s", efiVarsPath, scopeGlobal.String())
	sbData, err := os.ReadFile(sbPath)
	if err != nil {
		return state, errors.Wrap(err, "failed to read SecureBoot variable")
	}
	if len(sbData) >= 5 {
		state.Enabled = sbData[4] == 1
	}

	// Read SetupMode variable (1 = setup mode, 0 = user mode)
	smPath := fmt.Sprintf("%s/SetupMode-%s", efiVarsPath, scopeGlobal.String())
	smData, err := os.ReadFile(smPath)
	if err != nil {
		// SetupMode might not exist on all systems, not critical
		return state, nil
	}
	if len(smData) >= 5 {
		state.SetupMode = smData[4] == 1
	}

	return state, nil
}

// GetUKIAndPartitionInfo reads UKI file name and partition info from installed image.
// Returns UKI file name and blkid info from raw image file.
//
//nolint:gocognit
func GetUKIAndPartitionInfo(loopDevice, rawImage string) (string, any, error) {
	// Try to find EFI partition on loop device
	// Usually it's the first partition (p1 for loop devices)
	var loopEfiPartition string
	var loopEfiMountPoint string
	var needUnmount bool

	// First, check if EFI partition is already mounted (Talos installer might have mounted it)
	// Check common mount points
	possibleMountPoints := []string{"/tmp/loop-efi-mount", "/boot/EFI", "/mnt"}
	for _, mp := range possibleMountPoints {
		if _, err := os.Stat(filepath.Join(mp, "EFI", "Linux")); err == nil {
			// Check if this mount point contains our loop device partition
			// Read /proc/mounts to verify
			mounts, err := os.ReadFile("/proc/mounts")
			if err == nil {
				if strings.Contains(string(mounts), loopDevice) && strings.Contains(string(mounts), mp) {
					loopEfiMountPoint = mp
					needUnmount = false //nolint:ineffassign // used after break
					log.Printf("using existing mount point %s for EFI partition", mp)
					break
				}
			}
		}
	}

	// If not found, try to mount it ourselves
	if loopEfiMountPoint == "" {
		loopEfiMountPoint = "/tmp/loop-efi-mount-boot-to-talos"
		_ = os.MkdirAll(loopEfiMountPoint, 0o755)
		needUnmount = true

		// Find EFI partition
		for i := 1; i <= 4; i++ {
			candidate := fmt.Sprintf("%sp%d", loopDevice, i)
			if _, err := os.Stat(candidate); err == nil {
				// Try to mount it to see if it's EFI partition
				// MS_RDONLY = 0x1
				if err := unix.Mount(candidate, loopEfiMountPoint, "vfat", 0x1, ""); err == nil {
					// Check if it has EFI directory
					if _, err := os.Stat(filepath.Join(loopEfiMountPoint, "EFI")); err == nil {
						loopEfiPartition = candidate
						break
					}
					_ = unix.Unmount(loopEfiMountPoint, 0)
				} else if errors.Is(err, unix.EBUSY) {
					// Partition is already mounted, try to find where
					mounts, err := os.ReadFile("/proc/mounts")
					if err == nil {
						for line := range strings.SplitSeq(string(mounts), "\n") {
							if strings.Contains(line, candidate) {
								fields := strings.Fields(line)
								if len(fields) >= 2 {
									loopEfiMountPoint = fields[1]
									needUnmount = false
									log.Printf("found already mounted EFI partition at %s", loopEfiMountPoint)
									break
								}
							}
						}
						if !needUnmount {
							break
						}
					}
				}
			}
		}

		if loopEfiPartition == "" && needUnmount {
			os.RemoveAll(loopEfiMountPoint)
			return "", nil, errors.Newf("failed to find EFI partition on loop device %s", loopDevice)
		}

		if needUnmount {
			defer func() {
				_ = unix.Unmount(loopEfiMountPoint, 0)
				_ = os.RemoveAll(loopEfiMountPoint)
			}()
		}
	}

	// Find UKI files in the installed image - same logic as sdboot.go
	ukiFiles, err := filepath.Glob(filepath.Join(loopEfiMountPoint, "EFI", "Linux", "Talos-*.efi"))
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to find UKI files")
	}

	if len(ukiFiles) == 0 {
		return "", nil, errors.Newf("no UKI files found in %s", filepath.Join(loopEfiMountPoint, "EFI", "Linux"))
	}

	// Use the latest UKI file (assuming it's the one just installed)
	// In sdboot.go, this would be ukiPath from generateNextUKIName
	ukiPath := filepath.Base(ukiFiles[len(ukiFiles)-1])
	log.Printf("found UKI file in installed image: %s", ukiPath)

	// For now, return nil for rawBlkidInfo as it's not needed for basic functionality
	// In full implementation, we would use blkid.ProbePath(rawImage, ...) here
	return ukiPath, nil, nil
}

// UpdateEFIVariables creates a Talos boot entry pointing to the target disk's ESP
// and updates BootOrder to put it first.
func UpdateEFIVariables(disk string) error {
	efiRW, err := newEFIReaderWriter(true)
	if err != nil {
		return errors.Wrap(err, "failed to create efivarfs reader/writer")
	}
	defer efiRW.Close()

	// Read GPT from target disk to find ESP
	esp, err := getESPInfo(disk)
	if err != nil {
		return errors.Wrap(err, "failed to get ESP info from target disk")
	}

	log.Printf("found ESP: partition %d, start LBA %d, size %d blocks, UUID %s",
		esp.PartitionNumber, esp.StartLBA, esp.SizeLBA, esp.PartitionGUID)

	// Determine EFI file path based on architecture
	efiFilePath, err := sdbootFilePath()
	if err != nil {
		return err
	}

	// List existing boot entries to find existing Talos entry
	bootEntries, err := listBootEntries(efiRW)
	if err != nil {
		return errors.Wrap(err, "failed to list boot entries")
	}

	// Find existing Talos entry or allocate new index
	targetIdx := -1
	for idx, entry := range bootEntries {
		if entry.Description == talosBootEntryDescription {
			targetIdx = idx
			log.Printf("found existing Talos boot entry at index %d, will overwrite", idx)

			break
		}
	}

	if targetIdx < 0 {
		targetIdx, err = findFreeBootIndex(efiRW)
		if err != nil {
			return errors.Wrap(err, "failed to find free boot index")
		}

		log.Printf("will create new boot entry at index %d", targetIdx)
	}

	// Build and write the boot entry
	opt := &loadOption{
		Description: talosBootEntryDescription,
		FilePath: devicePath{
			&hardDrivePath{
				PartitionNumber:    esp.PartitionNumber,
				PartitionStart:     esp.StartLBA,
				PartitionSize:      esp.SizeLBA,
				PartitionSignature: esp.PartitionGUID,
			},
			&filePathElem{Path: efiFilePath},
			&endOfDevicePath{},
		},
	}

	if err := setBootEntry(efiRW, targetIdx, opt); err != nil {
		return errors.Wrapf(err, "failed to write boot entry at index %d", targetIdx)
	}

	// Update BootOrder: put new entry first, keep others without duplicates
	bootOrder, err := getBootOrder(efiRW)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return errors.Wrap(err, "failed to get BootOrder")
		}

		bootOrder = BootOrderType{}
	}

	newBootOrder := BootOrderType{uint16(targetIdx)}

	for _, idx := range bootOrder {
		if idx != uint16(targetIdx) {
			newBootOrder = append(newBootOrder, idx)
		}
	}

	if err := setBootOrder(efiRW, newBootOrder); err != nil {
		return errors.Wrap(err, "failed to set BootOrder")
	}

	log.Printf("EFI boot entry %04X created, BootOrder: %v", targetIdx, newBootOrder)

	return nil
}

// EFI variables reader/writer interface.
type efiReadWriter interface {
	Write(scope uuid.UUID, varName string, attrs efiAttribute, value []byte) error
	Delete(scope uuid.UUID, varName string) error
	Read(scope uuid.UUID, varName string) ([]byte, efiAttribute, error)
	List(scope uuid.UUID) ([]string, error)
}

type efiFilesystemReaderWriter struct {
	write bool
}

func newEFIReaderWriter(write bool) (*efiFilesystemReaderWriter, error) {
	if write {
		// Remount efivarfs in read-write mode
		// MS_REMOUNT = 0x20
		if err := unix.Mount("efivarfs", efiVarsMountPoint, "efivarfs", 0x20, "rw"); err != nil {
			return nil, errors.Wrap(err, "failed to remount efivarfs in read-write mode")
		}
	}
	return &efiFilesystemReaderWriter{write: write}, nil
}

func (rw *efiFilesystemReaderWriter) Close() error {
	if rw.write {
		// MS_REMOUNT = 0x20, MS_RDONLY = 0x1
		return unix.Mount("efivarfs", efiVarsMountPoint, "efivarfs", 0x20|0x1, "")
	}
	return nil
}

func varPath(scope uuid.UUID, varName string) string {
	return fmt.Sprintf("%s/%s-%s", efiVarsPath, varName, scope.String())
}

func (rw *efiFilesystemReaderWriter) Write(scope uuid.UUID, varName string, attrs efiAttribute, value []byte) error {
	if !rw.write {
		return errors.New("efivarfs was opened read-only")
	}

	// Remove immutable attribute from the efivarfs file if it exists
	// Ref: https://docs.kernel.org/filesystems/efivarfs.html
	path := varPath(scope, varName)
	if _, err := os.Stat(path); err == nil {
		f, err := os.Open(path)
		if err == nil {
			// Try to remove immutable flag using ioctl
			// FS_IOC_SETFLAGS = 0x40086602, FS_IMMUTABLE_FL = 0x10
			var flags uint32
			_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0x80086601, uintptr(unsafe.Pointer(&flags))) // FS_IOC_GETFLAGS
			if errno == 0 {
				flags &^= 0x10                                                                                  // Clear FS_IMMUTABLE_FL
				_, _, errno = unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0x40086602, uintptr(unsafe.Pointer(&flags))) // FS_IOC_SETFLAGS
				if errno != 0 {
					log.Printf("warning: failed to clear immutable attribute: %v", errno)
				}
			}
			f.Close()
		}
	}

	// Required by UEFI 2.10 Section 8.2.3:
	// Runtime access to a data variable implies boot service access.
	if attrs&attrRuntimeAccess != 0 {
		attrs |= attrBootserviceAccess
	}

	// Write attributes, see Linux Documentation/filesystems/efivarfs.rst for format
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		e := err
		var perr *fs.PathError
		if errors.As(err, &perr) {
			e = perr.Err
		}
		return errors.Wrapf(e, "writing %q in scope %s", varName, scope)
	}

	// Linux wants everything in one write, so assemble an intermediate buffer
	buf := make([]byte, len(value)+4)
	binary.LittleEndian.PutUint32(buf[:4], uint32(attrs))
	copy(buf[4:], value)

	_, err = f.Write(buf)
	if err1 := f.Close(); err1 != nil && err == nil {
		err = err1
	}

	// Try to restore immutable flag
	if err == nil {
		if f2, err2 := os.Open(path); err2 == nil {
			var flags uint32 = 0x10 // FS_IMMUTABLE_FL
			_, _, _ = unix.Syscall(unix.SYS_IOCTL, f2.Fd(), 0x40086602, uintptr(unsafe.Pointer(&flags)))
			f2.Close()
		}
	}

	return err
}

func (rw *efiFilesystemReaderWriter) Read(scope uuid.UUID, varName string) ([]byte, efiAttribute, error) {
	val, err := os.ReadFile(varPath(scope, varName))
	if err != nil {
		return nil, 0, errors.Wrapf(err, "reading %q in scope %s", varName, scope)
	}
	if len(val) < 4 {
		return nil, 0, errors.Newf("reading %q in scope %s: malformed, less than 4 bytes long", varName, scope)
	}
	return val[4:], efiAttribute(binary.LittleEndian.Uint32(val[:4])), nil
}

func (rw *efiFilesystemReaderWriter) Delete(scope uuid.UUID, varName string) error {
	if !rw.write {
		return errors.New("efivarfs was opened read-only")
	}
	return os.Remove(varPath(scope, varName))
}

func (rw *efiFilesystemReaderWriter) List(scope uuid.UUID) ([]string, error) {
	vars, err := os.ReadDir(efiVarsPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list variable directory")
	}
	var outVarNames []string
	suffix := fmt.Sprintf("-%v", scope)
	for _, v := range vars {
		if v.IsDir() {
			continue
		}
		if !strings.HasSuffix(v.Name(), suffix) {
			continue
		}
		outVarNames = append(outVarNames, strings.TrimSuffix(v.Name(), suffix))
	}
	return outVarNames, nil
}

// BootOrderType represents the UEFI BootOrder variable.
type BootOrderType []uint16

func unmarshalBootOrder(d []byte) (BootOrderType, error) {
	if len(d)%2 != 0 {
		return nil, errors.Newf("invalid length: %v bytes", len(d))
	}
	l := len(d) / 2
	out := make(BootOrderType, l)
	for i := range l {
		out[i] = binary.LittleEndian.Uint16(d[i*2:])
	}
	return out, nil
}

func (bo BootOrderType) marshal() []byte {
	var out []byte
	for _, v := range bo {
		out = binary.LittleEndian.AppendUint16(out, v)
	}
	return out
}

func getBootOrder(rw efiReadWriter) (BootOrderType, error) {
	raw, _, err := rw.Read(scopeGlobal, "BootOrder")
	if err != nil {
		return nil, err
	}
	return unmarshalBootOrder(raw)
}

func setBootOrder(rw efiReadWriter, ord BootOrderType) error {
	return rw.Write(scopeGlobal, "BootOrder", attrNonVolatile|attrRuntimeAccess, ord.marshal())
}

var bootVarRegexp = regexp.MustCompile(`^Boot([0-9A-Fa-f]{4})$`)

type loadOption struct {
	Description string
	FilePath    devicePath
}

type devicePath []devicePathElem

type devicePathElem interface {
	typ() uint8
	subType() uint8
	data() ([]byte, error)
}

func listBootEntries(rw efiReadWriter) (map[int]*loadOption, error) {
	bootEntries := make(map[int]*loadOption)
	varNames, err := rw.List(scopeGlobal)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list EFI variables")
	}

	for _, varName := range varNames {
		s := bootVarRegexp.FindStringSubmatch(varName)
		if s == nil {
			continue
		}
		idx, err := strconv.ParseUint(s[1], 16, 16)
		if err != nil {
			continue
		}
		entry, err := getBootEntry(rw, int(idx))
		if err != nil {
			continue
		}
		bootEntries[int(idx)] = entry
	}
	return bootEntries, nil
}

func getBootEntry(rw efiReadWriter, idx int) (*loadOption, error) {
	raw, _, err := rw.Read(scopeGlobal, fmt.Sprintf("Boot%04X", idx))
	if errors.Is(err, fs.ErrNotExist) {
		raw, _, err = rw.Read(scopeGlobal, fmt.Sprintf("Boot%04x", idx))
	}
	if err != nil {
		return nil, err
	}
	return unmarshalLoadOption(raw)
}

func unmarshalLoadOption(data []byte) (*loadOption, error) {
	if len(data) < 6 {
		return nil, errors.Newf("invalid load option: minimum 6 bytes are required, got %d", len(data))
	}
	nullIdx := bytes.Index(data[6:], []byte{0x00, 0x00})
	if nullIdx == -1 {
		return nil, errors.New("no null code point marking end of Description found")
	}
	descriptionEnd := 6 + nullIdx + 1
	descriptionRaw := data[6:descriptionEnd]
	description, err := efiEncoding.NewDecoder().Bytes(descriptionRaw)
	if err != nil {
		return nil, errors.Wrap(err, "error decoding UTF-16 in Description")
	}
	opt := &loadOption{
		Description: string(bytes.TrimSuffix(description, []byte{0})),
	}
	return opt, nil
}

const talosBootEntryDescription = "Talos Linux UKI"

// loadOptionActive is the LOAD_OPTION_ACTIVE attribute bit.
const loadOptionActive = 0x00000001

func (lo *loadOption) marshal() ([]byte, error) {
	// Encode description to UTF-16LE with null terminator
	descBytes, err := efiEncoding.NewEncoder().Bytes([]byte(lo.Description))
	if err != nil {
		return nil, errors.Wrap(err, "encoding description to UTF-16LE")
	}

	descBytes = append(descBytes, 0x00, 0x00) // null terminator

	// Serialize device path elements
	var filePathList []byte

	for _, elem := range lo.FilePath {
		elemData, err := elem.data()
		if err != nil {
			return nil, errors.Wrap(err, "serializing device path element")
		}

		filePathList = append(filePathList, elemData...)
	}

	// Build load option: Attributes(4) + FilePathListLength(2) + Description + FilePathList
	buf := make([]byte, 4+2+len(descBytes)+len(filePathList))
	binary.LittleEndian.PutUint32(buf[0:4], loadOptionActive)
	binary.LittleEndian.PutUint16(buf[4:6], uint16(len(filePathList)))
	copy(buf[6:], descBytes)
	copy(buf[6+len(descBytes):], filePathList)

	return buf, nil
}

// hardDrivePath represents UEFI Hard Drive Media Device Path (Type 0x04, SubType 0x01).
type hardDrivePath struct {
	PartitionNumber    uint32
	PartitionStart     uint64    // in logical blocks
	PartitionSize      uint64    // in logical blocks
	PartitionSignature uuid.UUID // partition GUID
}

const (
	dpTypeMedia       = 0x04
	dpSubTypeHardDrive = 0x01
	dpSubTypeFilePath = 0x04
	dpTypeEnd         = 0x7F
	dpSubTypeEnd      = 0xFF
	hardDrivePathLen  = 42 // 4-byte header + 38-byte data

	gptMBRType       = 0x02
	gptSignatureType = 0x02
)

func (h *hardDrivePath) typ() uint8     { return dpTypeMedia }
func (h *hardDrivePath) subType() uint8 { return dpSubTypeHardDrive }

func (h *hardDrivePath) data() ([]byte, error) {
	buf := make([]byte, hardDrivePathLen)
	buf[0] = dpTypeMedia
	buf[1] = dpSubTypeHardDrive
	binary.LittleEndian.PutUint16(buf[2:4], hardDrivePathLen)
	binary.LittleEndian.PutUint32(buf[4:8], h.PartitionNumber)
	binary.LittleEndian.PutUint64(buf[8:16], h.PartitionStart)
	binary.LittleEndian.PutUint64(buf[16:24], h.PartitionSize)
	copy(buf[24:40], guidToMixedEndian(h.PartitionSignature))
	buf[40] = gptMBRType
	buf[41] = gptSignatureType

	return buf, nil
}

// filePathElem represents UEFI File Path Media Device Path (Type 0x04, SubType 0x04).
type filePathElem struct {
	Path string // e.g., `\EFI\boot\BOOTX64.efi`
}

func (f *filePathElem) typ() uint8     { return dpTypeMedia }
func (f *filePathElem) subType() uint8 { return dpSubTypeFilePath }

func (f *filePathElem) data() ([]byte, error) {
	// Convert forward slashes to backslashes
	path := strings.ReplaceAll(f.Path, "/", "\\")

	pathBytes, err := efiEncoding.NewEncoder().Bytes([]byte(path))
	if err != nil {
		return nil, errors.Wrap(err, "encoding file path to UTF-16LE")
	}

	pathBytes = append(pathBytes, 0x00, 0x00) // null terminator

	totalLen := 4 + len(pathBytes)
	buf := make([]byte, totalLen)
	buf[0] = dpTypeMedia
	buf[1] = dpSubTypeFilePath
	binary.LittleEndian.PutUint16(buf[2:4], uint16(totalLen))
	copy(buf[4:], pathBytes)

	return buf, nil
}

// endOfDevicePath represents UEFI End of Device Path (Type 0x7F, SubType 0xFF).
type endOfDevicePath struct{}

func (e *endOfDevicePath) typ() uint8     { return dpTypeEnd }
func (e *endOfDevicePath) subType() uint8 { return dpSubTypeEnd }

func (e *endOfDevicePath) data() ([]byte, error) {
	return []byte{dpTypeEnd, dpSubTypeEnd, 0x04, 0x00}, nil
}

// guidToMixedEndian converts a uuid.UUID (RFC 4122, big-endian time fields)
// to UEFI mixed-endian format (little-endian for first 3 groups).
func guidToMixedEndian(id uuid.UUID) []byte {
	out := make([]byte, 16)
	copy(out, id[:])
	// Reverse bytes 0-3 (time_low)
	out[0], out[1], out[2], out[3] = id[3], id[2], id[1], id[0]
	// Reverse bytes 4-5 (time_mid)
	out[4], out[5] = id[5], id[4]
	// Reverse bytes 6-7 (time_hi_and_version)
	out[6], out[7] = id[7], id[6]
	// Bytes 8-15 stay as-is

	return out
}

func setBootEntry(rw efiReadWriter, idx int, opt *loadOption) error {
	data, err := opt.marshal()
	if err != nil {
		return errors.Wrap(err, "marshaling load option")
	}

	return rw.Write(scopeGlobal, fmt.Sprintf("Boot%04X", idx), attrNonVolatile|attrRuntimeAccess, data)
}

func findFreeBootIndex(rw efiReadWriter) (int, error) {
	varNames, err := rw.List(scopeGlobal)
	if err != nil {
		return 0, errors.Wrap(err, "listing EFI variables")
	}

	usedIndices := make(map[int]bool)

	for _, varName := range varNames {
		s := bootVarRegexp.FindStringSubmatch(varName)
		if s == nil {
			continue
		}

		idx, err := strconv.ParseUint(s[1], 16, 16)
		if err != nil {
			continue
		}

		usedIndices[int(idx)] = true
	}

	for i := range 0x10000 {
		if !usedIndices[i] {
			return i, nil
		}
	}

	return 0, errors.New("no free boot entry index available")
}

// espInfo contains information about the EFI System Partition on a disk.
type espInfo struct {
	PartitionNumber uint32
	StartLBA        uint64
	SizeLBA         uint64
	PartitionGUID   uuid.UUID
}

func getESPInfo(diskPath string) (*espInfo, error) {
	d, err := diskfs.Open(diskPath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, errors.Wrapf(err, "opening disk %s", diskPath)
	}
	defer d.Close()

	table, err := d.GetPartitionTable()
	if err != nil {
		return nil, errors.Wrap(err, "reading partition table")
	}

	gptTable, ok := table.(*gpt.Table)
	if !ok {
		return nil, errors.New("disk does not have a GPT partition table")
	}

	for i, part := range gptTable.Partitions {
		if part == nil {
			continue
		}

		if part.Type != gpt.EFISystemPartition {
			continue
		}

		partGUID, err := uuid.Parse(part.GUID)
		if err != nil {
			return nil, errors.Wrapf(err, "parsing partition GUID %q", part.GUID)
		}

		sectorSize := uint64(gptTable.LogicalSectorSize)
		if sectorSize == 0 {
			sectorSize = 512
		}

		return &espInfo{
			PartitionNumber: uint32(i + 1),
			StartLBA:        part.Start,
			SizeLBA:         part.Size / sectorSize,
			PartitionGUID:   partGUID,
		}, nil
	}

	return nil, errors.Newf("EFI System Partition not found on %s", diskPath)
}

// sdbootFilePath returns the EFI file path for sd-boot based on architecture.
func sdbootFilePath() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return `\EFI\boot\BOOTX64.efi`, nil
	case "arm64":
		return `\EFI\boot\BOOTAA64.efi`, nil
	default:
		return "", errors.Newf("unsupported architecture: %s", runtime.GOARCH)
	}
}
