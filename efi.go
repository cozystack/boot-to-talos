//go:build linux
// +build linux

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"golang.org/x/text/encoding/unicode"
)

/* -------------------- EFI variables update helpers ------------------------------- */

// isUEFIBoot returns true if the system is booted using UEFI.
func isUEFIBoot() bool {
	_, err := os.Stat("/sys/firmware/efi")
	return err == nil
}

// getUKIAndPartitionInfo reads UKI file name and partition info from installed image
// Returns UKI file name and blkid info from raw image file
func getUKIAndPartitionInfo(loopDevice, rawImage string) (string, interface{}, error) {
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
					needUnmount = false
					log.Printf("using existing mount point %s for EFI partition", mp)
					break
				}
			}
		}
	}

	// If not found, try to mount it ourselves
	if loopEfiMountPoint == "" {
		loopEfiMountPoint = "/tmp/loop-efi-mount-boot-to-talos"
		os.MkdirAll(loopEfiMountPoint, 0o755)
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
					unix.Unmount(loopEfiMountPoint, 0)
				} else if err == unix.EBUSY {
					// Partition is already mounted, try to find where
					mounts, err := os.ReadFile("/proc/mounts")
					if err == nil {
						lines := strings.Split(string(mounts), "\n")
						for _, line := range lines {
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
			return "", nil, fmt.Errorf("failed to find EFI partition on loop device %s", loopDevice)
		}

		if needUnmount {
			defer func() {
				unix.Unmount(loopEfiMountPoint, 0)
				os.RemoveAll(loopEfiMountPoint)
			}()
		}
	}

	// Find UKI files in the installed image - same logic as sdboot.go
	ukiFiles, err := filepath.Glob(filepath.Join(loopEfiMountPoint, "EFI", "Linux", "Talos-*.efi"))
	if err != nil {
		return "", nil, fmt.Errorf("failed to find UKI files: %w", err)
	}

	if len(ukiFiles) == 0 {
		return "", nil, fmt.Errorf("no UKI files found in %s", filepath.Join(loopEfiMountPoint, "EFI", "Linux"))
	}

	// Use the latest UKI file (assuming it's the one just installed)
	// In sdboot.go, this would be ukiPath from generateNextUKIName
	ukiPath := filepath.Base(ukiFiles[len(ukiFiles)-1])
	log.Printf("found UKI file in installed image: %s", ukiPath)

	// For now, return nil for rawBlkidInfo as it's not needed for basic functionality
	// In full implementation, we would use blkid.ProbePath(rawImage, ...) here
	return ukiPath, nil, nil
}

// updateEFIVariables updates EFI variables after installation
// This finds the Talos boot entry created by installer and updates BootOrder to put it first
func updateEFIVariables(disk, ukiPath string, rawBlkidInfo interface{}) error {
	// Create efivarfs reader/writer
	efiRW, err := newEFIReaderWriter(true)
	if err != nil {
		return fmt.Errorf("failed to create efivarfs reader/writer: %w", err)
	}
	defer efiRW.Close() //nolint:errcheck

	// Get current BootOrder
	bootOrder, err := getBootOrder(efiRW)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("failed to get BootOrder: %w", err)
		}
		bootOrder = bootOrderType{}
	}

	log.Printf("Current BootOrder: %v", bootOrder)

	// List all boot entries to find Talos entry
	bootEntries, err := listBootEntries(efiRW)
	if err != nil {
		return fmt.Errorf("failed to list boot entries: %w", err)
	}

	// Find Talos boot entry index
	var talosBootEntryIndex int = -1
	for idx, entry := range bootEntries {
		if entry.Description == "Talos Linux UKI" {
			talosBootEntryIndex = idx
			log.Printf("Found Talos boot entry at index %d", idx)
			break
		}
	}

	if talosBootEntryIndex == -1 {
		return fmt.Errorf("Talos boot entry not found")
	}

	// Update BootOrder: put Talos entry first, then all others (excluding Talos entries)
	newBootOrder := bootOrderType{uint16(talosBootEntryIndex)}

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
		return fmt.Errorf("failed to set BootOrder: %w", err)
	}

	log.Printf("BootOrder updated successfully, Talos entry %d is now first", talosBootEntryIndex)
	return nil
}

/* -------------------- Local EFI variables implementation ------------------------------- */
// These are local copies of efivarfs types and functions to avoid dependency on internal packages

const (
	efiVarsMountPoint = "/sys/firmware/efi/efivars"
	efiVarsPath       = "/sys/firmware/efi/efivars"
)

var (
	scopeSystemd = uuid.MustParse("4a67b082-0a4c-41cf-b6c7-440b29bb8c4f")
	scopeGlobal  = uuid.MustParse("8be4df61-93ca-11d2-aa0d-00e098032b8c")
)

var efiEncoding = unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)

type efiAttribute uint32

const (
	attrNonVolatile efiAttribute = 1 << iota
	attrBootserviceAccess
	attrRuntimeAccess
)

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
			return nil, fmt.Errorf("failed to remount efivarfs in read-write mode: %w", err)
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
			f.Close() //nolint:errcheck
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
		return fmt.Errorf("writing %q in scope %s: %w", varName, scope, e)
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
			var flags uint32 = 0x10                                                            // FS_IMMUTABLE_FL
			unix.Syscall(unix.SYS_IOCTL, f2.Fd(), 0x40086602, uintptr(unsafe.Pointer(&flags))) //nolint:errcheck
			f2.Close()                                                                         //nolint:errcheck
		}
	}

	return err
}

func (rw *efiFilesystemReaderWriter) Read(scope uuid.UUID, varName string) ([]byte, efiAttribute, error) {
	val, err := os.ReadFile(varPath(scope, varName))
	if err != nil {
		return nil, 0, fmt.Errorf("reading %q in scope %s: %w", varName, scope, err)
	}
	if len(val) < 4 {
		return nil, 0, fmt.Errorf("reading %q in scope %s: malformed, less than 4 bytes long", varName, scope)
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
		return nil, fmt.Errorf("failed to list variable directory: %w", err)
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

type bootOrderType []uint16

func unmarshalBootOrder(d []byte) (bootOrderType, error) {
	if len(d)%2 != 0 {
		return nil, fmt.Errorf("invalid length: %v bytes", len(d))
	}
	l := len(d) / 2
	out := make(bootOrderType, l)
	for i := range l {
		out[i] = binary.LittleEndian.Uint16(d[i*2:])
	}
	return out, nil
}

func (bo bootOrderType) marshal() []byte {
	var out []byte
	for _, v := range bo {
		out = binary.LittleEndian.AppendUint16(out, v)
	}
	return out
}

func getBootOrder(rw efiReadWriter) (bootOrderType, error) {
	raw, _, err := rw.Read(scopeGlobal, "BootOrder")
	if err != nil {
		return nil, err
	}
	return unmarshalBootOrder(raw)
}

func setBootOrder(rw efiReadWriter, ord bootOrderType) error {
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
		return nil, fmt.Errorf("failed to list EFI variables: %w", err)
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
		return nil, fmt.Errorf("invalid load option: minimum 6 bytes are required, got %d", len(data))
	}
	nullIdx := bytes.Index(data[6:], []byte{0x00, 0x00})
	if nullIdx == -1 {
		return nil, errors.New("no null code point marking end of Description found")
	}
	descriptionEnd := 6 + nullIdx + 1
	descriptionRaw := data[6:descriptionEnd]
	description, err := efiEncoding.NewDecoder().Bytes(descriptionRaw)
	if err != nil {
		return nil, fmt.Errorf("error decoding UTF-16 in Description: %w", err)
	}
	descriptionEnd += 2
	opt := &loadOption{
		Description: string(bytes.TrimSuffix(description, []byte{0})),
	}
	return opt, nil
}
