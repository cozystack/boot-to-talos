package main

import (
	"archive/tar"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/google/go-containerregistry/pkg/crane"
	"golang.org/x/sys/unix"
)

/* -------------------- install mode ------------------------------------------- */

func runInstallMode(image string, disk string, extra multiFlag, sizeGiB uint64) {
	fmt.Println("\nSummary:")
	fmt.Printf("  Image: %s\n", image)
	fmt.Printf("  Disk:  %s\n", disk)
	fmt.Printf("  Extra kernel args: %s\n",
		func() string {
			if len(extra) == 0 {
				return "(none)"
			}
			return strings.Join(extra, " ")
		}())
	fmt.Printf("\nWARNING: ALL DATA ON %s WILL BE ERASED!\n\n", disk)
	if !askYesNo("Continue?", true) {
		log.Fatal("aborted by user")
	}
	fmt.Println()

	/* ---------- heavy work (logs will show progress) ---------- */

	tmpDir, _ := os.MkdirTemp("", "installer-*")
	log.Printf("created temporary directory %s", tmpDir)
	defer os.RemoveAll(tmpDir)
	must("mount tmpfs", unix.Mount("tmpfs", tmpDir, "tmpfs", 0, ""))

	instDir := filepath.Join(tmpDir, "installer")
	os.MkdirAll(instDir, 0o755)

	transport := setupTransportWithProxy()
	opts := crane.WithTransport(transport)

	log.Printf("pulling image %s", image)
	img, err := crane.Pull(image, opts)
	must("pull image", err)

	log.Print("extracting image layers")
	layers, _ := img.Layers()
	for _, l := range layers {
		r, _ := l.Uncompressed()
		defer r.Close()
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			must("tar", err)
			if strings.HasPrefix(filepath.Base(h.Name), ".wh.") {
				os.RemoveAll(filepath.Join(instDir,
					filepath.Dir(h.Name),
					strings.TrimPrefix(filepath.Base(h.Name), ".wh.")))
				continue
			}
			target := filepath.Join(instDir, h.Name)
			switch h.Typeflag {
			case tar.TypeDir:
				os.MkdirAll(target, os.FileMode(h.Mode))
			case tar.TypeReg:
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					log.Fatalf("failed to create directory: %v (check available disk space)", err)
				}
				f, err := os.Create(target)
				if err != nil {
					log.Fatalf("failed to create file: %v (check available disk space)", err)
				}
				if _, err := io.Copy(f, tr); err != nil {
					f.Close()
					os.Remove(target)
					log.Fatalf("failed to extract file: %v (check available disk space)", err)
				}
				if err := f.Close(); err != nil {
					os.Remove(target)
					log.Fatalf("failed to close file: %v", err)
				}
				if err := os.Chmod(target, os.FileMode(h.Mode)); err != nil {
					log.Fatalf("failed to set file permissions: %v", err)
				}
			case tar.TypeSymlink:
				os.MkdirAll(filepath.Dir(target), 0o755)
				os.Symlink(h.Linkname, target)
			case tar.TypeLink:
				os.Link(filepath.Join(instDir, h.Linkname), target)
			case tar.TypeChar, tar.TypeBlock:
				os.MkdirAll(filepath.Dir(target), 0o755)
				dev := int(unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor)))
				mode := uint32(h.Mode)
				if h.Typeflag == tar.TypeChar {
					mode |= unix.S_IFCHR
				} else {
					mode |= unix.S_IFBLK
				}
				unix.Mknod(target, mode, dev)
			}
		}
	}

	raw := filepath.Join(tmpDir, "image.raw")
	log.Printf("creating raw disk %s (%d GiB)", raw, sizeGiB)
	f, _ := os.Create(raw)
	f.Truncate(int64(sizeGiB) << 30)
	f.Close()

	loop, lf := setupLoop(raw)
	log.Printf("attached %s to %s", raw, loop)
	defer func() {
		unix.Syscall(unix.SYS_IOCTL, lf.Fd(), unix.LOOP_CLR_FD, 0)
		lf.Close()
	}()

	mountBind("/proc", filepath.Join(instDir, "proc"))
	mountBindRecursive("/sys", filepath.Join(instDir, "sys"))
	mountBind("/dev", filepath.Join(instDir, "dev"))
	overrideCmdline(instDir, "talos.platform=metal "+strings.Join(extra, " "))

	execPath := "/usr/bin/installer"
	args := []string{execPath, "install", "--platform", "metal", "--disk", loop, "--force"}
	for _, a := range extra {
		args = append(args, "--extra-kernel-arg", a)
	}

	stdinR, stdinW, _ := os.Pipe()
	go func() {
		fmt.Fprintf(stdinW, `version: v1alpha1
machine:
  ca: {crt: %s}
  install: {disk: /dev/sda}
cluster:
  controlPlane: {endpoint: https://localhost:6443}
`, fakeCert())
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
	must("forkexec", err)
	var ws syscall.WaitStatus
	_, err = syscall.Wait4(pid, &ws, 0, nil)
	must("wait", err)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		log.Fatalf("installer exited %d", ws.ExitStatus())
	}
	log.Print("Talos installer finished successfully")

	log.Print("remounting all filesystems read-only")
	os.WriteFile("/proc/sysrq-trigger", []byte("u"), 0)

	copyWithFsync(raw, disk)
	log.Printf("installation image copied to %s", disk)

	log.Print("rebooting system")
	os.WriteFile("/proc/sysrq-trigger", []byte("b"), 0)
}

