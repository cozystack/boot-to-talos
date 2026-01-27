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
	"strconv"
	"strings"
	"unsafe"

	"github.com/cockroachdb/errors"
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

// UpdateEFIVariables updates EFI variables after installation.
// This finds the Talos boot entry created by installer and updates BootOrder to put it first.
func UpdateEFIVariables(disk, ukiPath string, rawBlkidInfo any) error {
	// Create efivarfs reader/writer
	efiRW, err := newEFIReaderWriter(true)
	if err != nil {
		return errors.Wrap(err, "failed to create efivarfs reader/writer")
	}
	defer efiRW.Close()

	// Get current BootOrder
	bootOrder, err := getBootOrder(efiRW)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return errors.Wrap(err, "failed to get BootOrder")
		}
		bootOrder = BootOrderType{}
	}

	log.Printf("Current BootOrder: %v", bootOrder)

	// List all boot entries to find Talos entry
	bootEntries, err := listBootEntries(efiRW)
	if err != nil {
		return errors.Wrap(err, "failed to list boot entries")
	}

	// Find Talos boot entry index
	talosBootEntryIndex := -1
	for idx, entry := range bootEntries {
		if entry.Description == "Talos Linux UKI" {
			talosBootEntryIndex = idx
			log.Printf("Found Talos boot entry at index %d", idx)
			break
		}
	}

	if talosBootEntryIndex == -1 {
		return errors.New("Talos boot entry not found")
	}

	// Update BootOrder: put Talos entry first, then all others (excluding Talos entries)
	newBootOrder := BootOrderType{uint16(talosBootEntryIndex)}

	// Add other entries (excluding Talos entries)
	talosIndexSet := make(map[uint16]bool)
	for idx := range bootEntries {
		if bootEntries[idx].Description == "Talos Linux UKI" {
			talosIndexSet[uint16(idx)] = true
		}
	}

	for _, idx := range bootOrder {
		if !talosIndexSet[idx] {
			newBootOrder = append(newBootOrder, idx)
		}
	}

	log.Printf("New BootOrder: %v", newBootOrder)

	// Update BootOrder
	if err := setBootOrder(efiRW, newBootOrder); err != nil {
		return errors.Wrap(err, "failed to set BootOrder")
	}

	log.Printf("BootOrder updated successfully, Talos entry %d is now first", talosBootEntryIndex)
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
