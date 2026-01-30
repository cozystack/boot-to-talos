//go:build linux

package install

import (
	"archive/tar"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/cockroachdb/errors"
	"golang.org/x/sys/unix"

	"github.com/cozystack/boot-to-talos/internal/cli"
	"github.com/cozystack/boot-to-talos/internal/efi"
	"github.com/cozystack/boot-to-talos/internal/types"
)

// MountBind performs a bind mount.
func MountBind(src, dst string) {
	_ = os.MkdirAll(dst, 0o755)
	cli.Must("bind "+src, unix.Mount(src, dst, "", unix.MS_BIND, ""))
}

// MountBindRecursive performs a recursive bind mount.
func MountBindRecursive(src, dst string) {
	_ = os.MkdirAll(dst, 0o755)
	cli.Must("bind recursive "+src, unix.Mount(src, dst, "", unix.MS_BIND|unix.MS_REC, ""))
}

// OverrideCmdline overrides /proc/cmdline in a chroot environment.
func OverrideCmdline(root, content string) {
	tmp := filepath.Join(root, "tmp", "cmdline")
	_ = os.MkdirAll(filepath.Dir(tmp), 0o755)
	cli.Must("write cmdline", os.WriteFile(tmp, []byte(content), 0o644))
	cli.Must("bind cmdline", unix.Mount(tmp, filepath.Join(root, "proc/cmdline"), "", unix.MS_BIND, ""))
}

// CopyWithFsync copies a file from src to dst with fsync after each write.
func CopyWithFsync(src, dst string) {
	log.Printf("copy %s → %s", src, dst)
	in, err := os.Open(src)
	cli.Must("open src", err)
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY, 0)
	cli.Must("open dst", err)
	defer out.Close()
	buf := make([]byte, 4<<20)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			_, werr := out.Write(buf[:n])
			cli.Must("write", werr)
			_ = out.Sync()
		}
		if err == io.EOF {
			break
		}
		cli.Must("read", err)
	}
}

// FakeCert generates a fake certificate for installer.
func FakeCert() string {
	r := make([]byte, 256)
	_, _ = rand.Read(r)
	return base64.StdEncoding.EncodeToString(r)
}

// SetupLoop sets up a loop device for the given file path.
// Returns the loop device path and the file handle.
func SetupLoop(path string) (string, *os.File) {
	ctrl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	cli.Must("open loop-control", err)
	num, _, errno := unix.Syscall(unix.SYS_IOCTL, ctrl.Fd(), unix.LOOP_CTL_GET_FREE, 0)
	if errno != 0 {
		log.Fatalf("LOOP_CTL_GET_FREE: %v", errno)
	}
	loop := fmt.Sprintf("/dev/loop%d", num)
	lf, err := os.OpenFile(loop, os.O_RDWR, 0)
	cli.Must("open loop", err)
	bf, err := os.OpenFile(path, os.O_RDWR, 0)
	cli.Must("open backing", err)
	_, _, errno = unix.Syscall(unix.SYS_IOCTL, lf.Fd(), unix.LOOP_SET_FD, bf.Fd())
	if errno != 0 {
		log.Fatalf("LOOP_SET_FD: %v", errno)
	}
	var info unix.LoopInfo64
	info.Flags = unix.LO_FLAGS_AUTOCLEAR
	_, _, errno = unix.Syscall(unix.SYS_IOCTL, lf.Fd(), unix.LOOP_SET_STATUS64, uintptr(unsafe.Pointer(&info)))
	if errno != 0 {
		log.Fatalf("LOOP_SET_STATUS64: %v", errno)
	}
	return loop, lf
}

// RunInstallMode executes install mode: extracts image, runs installer, copies to disk.
//
//nolint:forbidigo
func RunInstallMode(source types.ImageSource, disk string, extraArgs []string, sizeGiB uint64) {
	// Check Secure Boot state on UEFI systems
	if efi.IsUEFIBoot() {
		sbState, err := efi.GetSecureBootState()
		if err == nil && sbState.Enabled && !sbState.SetupMode {
			fmt.Println("\nWARNING: Secure Boot is enabled!")
			fmt.Println("Talos UKI is signed with Sidero Labs keys, which are not in your UEFI db.")
			fmt.Println("The installed system will likely fail to boot.")
			fmt.Println("")
			fmt.Println("Options:")
			fmt.Println("  1. Disable Secure Boot in BIOS/UEFI settings")
			fmt.Println("  2. Put UEFI in Setup Mode to allow automatic key enrollment")
			fmt.Println("")
			if !cli.AskYesNo("Proceed anyway (not recommended)?", false) {
				log.Fatal("aborted: Secure Boot is enabled")
			}
			fmt.Println("")
		}
	}

	fmt.Println("\nSummary:")
	fmt.Printf("  Image: %s\n", source.Reference())
	fmt.Printf("  Disk:  %s\n", disk)
	fmt.Printf("  Extra kernel args: %s\n",
		func() string {
			if len(extraArgs) == 0 {
				return "(none)"
			}
			return strings.Join(extraArgs, " ")
		}())
	fmt.Printf("\nWARNING: ALL DATA ON %s WILL BE ERASED!\n\n", disk)
	if !cli.AskYesNo("Continue?", true) {
		log.Fatal("aborted by user")
	}
	fmt.Println()

	// Get install assets from source
	tmpDir, err := os.MkdirTemp("", "installer-*")
	if err != nil {
		log.Fatalf("create temporary directory: %v", err)
	}
	log.Printf("created temporary directory %s", tmpDir)

	mounted := false
	defer func() {
		if mounted {
			_ = unix.Unmount(tmpDir, 0)
		}
		os.RemoveAll(tmpDir)
	}()

	cli.Must("mount tmpfs", unix.Mount("tmpfs", tmpDir, "tmpfs", 0, ""))
	mounted = true

	assets, err := source.GetInstallAssets(tmpDir, sizeGiB)
	if err != nil {
		log.Fatalf("failed to get install assets from %s source: %v", source.Type(), err)
	}
	defer assets.Close()

	// Use disk image from assets
	if assets.DiskImage != nil {
		runDiskImageInstall(assets, disk, extraArgs)
	} else if assets.RootfsPath != "" {
		runChrootInstall(assets, disk, extraArgs, sizeGiB, tmpDir)
	} else {
		log.Fatal("install assets contain neither disk image nor rootfs path")
	}
}

// runDiskImageInstall installs using a pre-built disk image (RAW).
func runDiskImageInstall(assets *types.InstallAssets, disk string, extraArgs []string) {
	log.Printf("installing from disk image to %s", disk)

	// Copy disk image to target disk
	out, err := os.OpenFile(disk, os.O_WRONLY, 0)
	cli.Must("open disk", err)
	defer out.Close()

	buf := make([]byte, 4<<20) // 4MB buffer
	for {
		n, err := assets.DiskImage.Read(buf)
		if n > 0 {
			_, werr := out.Write(buf[:n])
			cli.Must("write", werr)
			_ = out.Sync()
		}
		if err == io.EOF {
			break
		}
		cli.Must("read", err)
	}

	log.Printf("disk image copied to %s", disk)

	// If extra args provided, we need to patch the UKI cmdline
	if len(extraArgs) > 0 {
		log.Printf("extra kernel args provided but UKI patching for installed image is not implemented yet")
	}

	log.Print("rebooting system")
	_ = os.WriteFile("/proc/sysrq-trigger", []byte("b"), 0)
}

// runChrootInstall installs using chroot installer.
func runChrootInstall(assets *types.InstallAssets, disk string, extraArgs []string, sizeGiB uint64, tmpDir string) {
	instDir := assets.RootfsPath

	raw := filepath.Join(tmpDir, "image.raw")
	log.Printf("creating raw disk %s (%d GiB)", raw, sizeGiB)
	f, err := os.Create(raw)
	cli.Must("create raw disk image", err)
	cli.Must("truncate raw disk image", f.Truncate(int64(sizeGiB)<<30))
	f.Close()

	loop, lf := SetupLoop(raw)
	log.Printf("attached %s to %s", raw, loop)
	defer func() {
		_, _, _ = unix.Syscall(unix.SYS_IOCTL, lf.Fd(), unix.LOOP_CLR_FD, 0)
		lf.Close()
	}()

	MountBind("/proc", filepath.Join(instDir, "proc"))
	MountBindRecursive("/sys", filepath.Join(instDir, "sys"))
	MountBind("/dev", filepath.Join(instDir, "dev"))
	OverrideCmdline(instDir, "talos.platform=metal "+strings.Join(extraArgs, " "))

	execPath := "/usr/bin/installer"
	args := []string{execPath, "install", "--platform", "metal", "--disk", loop, "--force"}
	for _, a := range extraArgs {
		args = append(args, "--extra-kernel-arg", a)
	}

	stdinR, stdinW, err := os.Pipe()
	cli.Must("create stdin pipe", err)
	go func() {
		fmt.Fprintf(stdinW, `version: v1alpha1
machine:
  ca: {crt: %s}
  install: {disk: /dev/sda}
cluster:
  controlPlane: {endpoint: https://localhost:6443}
`, FakeCert())
		stdinW.Close()
	}()

	attr := &syscall.ProcAttr{
		Dir:   "/",
		Env:   []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		Files: []uintptr{stdinR.Fd(), os.Stdout.Fd(), os.Stderr.Fd()},
		Sys:   &syscall.SysProcAttr{Chroot: instDir},
	}

	log.Print("starting Talos installer")
	pid, err := syscall.ForkExec(execPath, args, attr)
	cli.Must("forkexec", err)
	var ws syscall.WaitStatus
	_, err = syscall.Wait4(pid, &ws, 0, nil)
	cli.Must("wait", err)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		log.Fatalf("installer exited %d", ws.ExitStatus())
	}
	log.Print("Talos installer finished successfully")

	// Get UKI file name and partition info from installed image (loop device) before copying
	// We need this info to update EFI variables after copying
	var ukiPath string
	var rawBlkidInfo any
	if efi.IsUEFIBoot() {
		var err error
		ukiPath, rawBlkidInfo, err = efi.GetUKIAndPartitionInfo(loop, raw)
		if err != nil {
			log.Printf("warning: failed to get UKI and partition info: %v", err)
		}
	}

	log.Print("remounting all filesystems read-only")
	_ = os.WriteFile("/proc/sysrq-trigger", []byte("u"), 0)

	CopyWithFsync(raw, disk)
	log.Printf("installation image copied to %s", disk)

	// Update EFI variables AFTER copying image
	// Update BootOrder to put Talos boot entry first (created by installer)
	if efi.IsUEFIBoot() && ukiPath != "" {
		log.Print("updating EFI variables")
		if err := efi.UpdateEFIVariables(disk, ukiPath, rawBlkidInfo); err != nil {
			log.Printf("warning: failed to update EFI variables: %v", err)
		}
	}

	log.Print("rebooting system")
	_ = os.WriteFile("/proc/sysrq-trigger", []byte("b"), 0)
}

// ExtractContainerLayers extracts container image layers to a directory.
// This is exported for use by sources that need to extract containers.
//
//nolint:gocognit
func ExtractContainerLayers(layers []io.ReadCloser, destDir string) error {
	for _, layer := range layers {
		tr := tar.NewReader(layer)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				layer.Close()
				return errors.Wrap(err, "read tar")
			}

			if suffix, found := strings.CutPrefix(filepath.Base(h.Name), ".wh."); found {
				os.RemoveAll(filepath.Join(destDir, filepath.Dir(h.Name), suffix))
				continue
			}

			target := filepath.Join(destDir, h.Name)

			// Security: prevent path traversal attacks
			cleanTarget := filepath.Clean(target)
			cleanDest := filepath.Clean(destDir)
			if !strings.HasPrefix(cleanTarget, cleanDest+string(os.PathSeparator)) && cleanTarget != cleanDest {
				continue // skip files that would escape destDir
			}

			switch h.Typeflag {
			case tar.TypeDir:
				_ = os.MkdirAll(target, os.FileMode(h.Mode))
			case tar.TypeReg:
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					return errors.Wrap(err, "create directory")
				}
				f, err := os.Create(target)
				if err != nil {
					return errors.Wrap(err, "create file")
				}
				if _, err := io.Copy(f, tr); err != nil {
					f.Close()
					os.Remove(target)
					return errors.Wrap(err, "extract file")
				}
				if err := f.Close(); err != nil {
					os.Remove(target)
					return errors.Wrap(err, "close file")
				}
				if err := os.Chmod(target, os.FileMode(h.Mode)); err != nil {
					return errors.Wrap(err, "chmod")
				}
			case tar.TypeSymlink:
				// Validate symlink target doesn't escape destDir
				linkTarget := h.Linkname
				if !filepath.IsAbs(linkTarget) {
					linkTarget = filepath.Join(filepath.Dir(target), linkTarget)
				}
				cleanLink := filepath.Clean(linkTarget)
				if !strings.HasPrefix(cleanLink, cleanDest+string(os.PathSeparator)) && cleanLink != cleanDest {
					continue // skip symlink escape attempt
				}
				_ = os.MkdirAll(filepath.Dir(target), 0o755)
				_ = os.Symlink(h.Linkname, target)
			case tar.TypeLink:
				// Validate hardlink source doesn't escape destDir
				linkSource := filepath.Join(destDir, h.Linkname)
				cleanSource := filepath.Clean(linkSource)
				if !strings.HasPrefix(cleanSource, cleanDest+string(os.PathSeparator)) && cleanSource != cleanDest {
					continue // skip hardlink escape attempt
				}
				_ = os.Link(linkSource, target)
			case tar.TypeChar, tar.TypeBlock:
				_ = os.MkdirAll(filepath.Dir(target), 0o755)
				dev := int(unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor)))
				mode := uint32(h.Mode)
				if h.Typeflag == tar.TypeChar {
					mode |= unix.S_IFCHR
				} else {
					mode |= unix.S_IFBLK
				}
				_ = unix.Mknod(target, mode, dev)
			}
		}
		layer.Close()
	}
	return nil
}
