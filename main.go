package main

import (
	"archive/tar"
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/google/go-containerregistry/pkg/crane"
	"golang.org/x/sys/unix"
)

/* ------------------------------ flags ------------------------------------- */

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

var (
	imageFlag string
	diskFlag  string
	yesFlag   bool
)

func init() {
	flag.StringVar(&imageFlag, "image",
		"ghcr.io/cozystack/cozystack/talos:v1.10.5", "Talos installer image")
	flag.StringVar(&diskFlag, "disk", "", "target disk (will be wiped)")
	flag.BoolVar(&yesFlag, "yes", false, "automatic yes to prompts")
}

/* ------------------------------ helpers ----------------------------------- */

func must(msg string, err error) {
	if err != nil {
		log.Fatalf("%s: %v", msg, err)
	}
}

func mountBind(src, dst string) {
	os.MkdirAll(dst, 0o755)
	must("bind "+src, unix.Mount(src, dst, "", unix.MS_BIND, ""))
}

func mountBindRecursive(src, dst string) {
	os.MkdirAll(dst, 0o755)
	must("bind recursive "+src, unix.Mount(src, dst, "", unix.MS_BIND|unix.MS_REC, ""))
}

func overrideCmdline(root, content string) {
	tmp := filepath.Join(root, "tmp", "cmdline")
	os.MkdirAll(filepath.Dir(tmp), 0o755)
	must("write cmdline", os.WriteFile(tmp, []byte(content), 0o644))
	must("bind cmdline", unix.Mount(tmp, filepath.Join(root, "proc/cmdline"), "", unix.MS_BIND, ""))
}

func copyWithFsync(src, dst string) {
	log.Printf("copy %s → %s", src, dst)
	in, err := os.Open(src)
	must("open src", err)
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY, 0)
	must("open dst", err)
	defer out.Close()
	buf := make([]byte, 4<<20)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			_, werr := out.Write(buf[:n])
			must("write", werr)
			out.Sync()
		}
		if err == io.EOF {
			break
		}
		must("read", err)
	}
}

func fakeCert() string {
	r := make([]byte, 256)
	rand.Read(r)
	return base64.StdEncoding.EncodeToString(r)
}

/* -------------------- interactive questions ------------------------------- */

var reader = bufio.NewReader(os.Stdin)

func ask(msg, def string) string {
	if yesFlag {
		fmt.Printf("%s [%s]: %s\n", msg, def, def)
		return def
	}
	fmt.Printf("%s [%s]: ", msg, def)
	t, _ := reader.ReadString('\n')
	t = strings.TrimSpace(t)
	if t == "" {
		return def
	}
	return t
}

func askRequired(msg string) string {
	if yesFlag {
		log.Fatalf("missing required input for: %s (cannot auto-fill)", msg)
	}
	for {
		fmt.Printf("%s: ", msg)
		t, _ := reader.ReadString('\n')
		t = strings.TrimSpace(t)
		if t != "" {
			return t
		}
	}
}

func askYesNo(msg string, def bool) bool {
	if yesFlag {
		fmt.Printf("%s [%s]: %v\n", msg, map[bool]string{true: "yes", false: "no"}[def], def)
		return def
	}
	defStr := "yes"
	if !def {
		defStr = "no"
	}
	for {
		fmt.Printf("%s [%s]: ", msg, defStr)
		in, _ := reader.ReadString('\n')
		in = strings.TrimSpace(strings.ToLower(in))
		if in == "" {
			return def
		}
		if in == "y" || in == "yes" {
			return true
		}
		if in == "n" || in == "no" {
			return false
		}
		fmt.Println("Please answer 'yes' or 'no'.")
	}
}

// ------------------- auto-detection helper -------------------
func firstDisk() string {
	entries, err := ioutil.ReadDir("/sys/block")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "loop") ||
			strings.HasPrefix(name, "ram") ||
			strings.HasPrefix(name, "fd") {
			continue
		}
		base := filepath.Join("/sys/block", name)
		if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
			continue
		}
		if b, _ := os.ReadFile(filepath.Join(base, "removable")); strings.TrimSpace(string(b)) != "0" {
			continue
		}
		return "/dev/" + name
	}
	return ""
}

/* -------------------- networking autodetect ------------------------------- */

func defaultRoute() (iface, gw string, err error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Scan()
	for sc.Scan() {
		flds := strings.Fields(sc.Text())
		if len(flds) >= 3 && flds[1] == "00000000" {
			iface = flds[0]
			gw = hexIPLittle(flds[2])
			return
		}
	}
	err = fmt.Errorf("no default route")
	return
}

func ifaceAddr(name string) (ip, mask string, err error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			ip = n.IP.String()
			mask = net.IP(n.Mask).String()
			return
		}
	}
	err = fmt.Errorf("IPv4 not found on %s", name)
	return
}

func prettyName(name string) string {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return name
	}
	p := fmt.Sprintf("/run/udev/data/n%d", ifc.Index)

	data, err := os.ReadFile(p)
	if err != nil {
		return name
	}
	for _, l := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(l, ':'); i >= 0 {
			l = l[i+1:]
		}
		switch {
		case strings.HasPrefix(l, "ID_NET_NAME_ONBOARD="):
			return strings.TrimPrefix(l, "ID_NET_NAME_ONBOARD=")
		case strings.HasPrefix(l, "ID_NET_NAME_PATH="):
			return strings.TrimPrefix(l, "ID_NET_NAME_PATH=")
		case strings.HasPrefix(l, "ID_NET_NAME_SLOT="):
			return strings.TrimPrefix(l, "ID_NET_NAME_SLOT=")
		}
	}
	return name
}

func hexIPLittle(h string) string {
	b, _ := hex.DecodeString(h)
	if len(b) != 4 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", b[3], b[2], b[1], b[0])
}

/* ---------------- collect kernel arguments -------------------------------- */

func collectKernelArgs() []string {
	dev, gw, _ := defaultRoute()
	ip, mask, _ := ifaceAddr(dev)
	dev = prettyName(dev)

	netOn := askYesNo("Add networking configuration?", true)
	var out []string
	if netOn {
		dev = ask("Interface", dev)
		ip = ask("IP address", ip)
		mask = ask("Netmask", mask)
		gw = ask("Gateway (or 'none')", gw)
		if strings.EqualFold(gw, "none") {
			gw = ""
		}
		out = append(out, fmt.Sprintf("ip=%s::%s:%s::%s:::::", ip, gw, mask, dev))
	}

	console := ask("Configure serial console? (or 'no')", "ttyS0")
	if console == "" {
		console = "ttyS0"
	}
	if !strings.EqualFold(console, "no") && !strings.EqualFold(console, "none") {
		out = append(out, "console="+console)
	}
	return out
}

/* ------------------------------ main -------------------------------------- */

func main() {
	var extra multiFlag
	sizeGiB := flag.Uint64("image-size-gib", 2, "image.raw size (GiB)")
	flag.Var(&extra, "extra-kernel-arg", "extra kernel arg (repeatable)")
	flag.Parse()

	if diskFlag == "" {
		def := firstDisk()
		if def == "" {
			diskFlag = askRequired("Target disk")
		} else {
			diskFlag = ask("Target disk", def)
		}
	}
	if imageFlag == flag.Lookup("image").DefValue {
		imageFlag = ask("Talos installer image", imageFlag)
	}

	for _, e := range collectKernelArgs() {
		extra = append(extra, e)
	}

	fmt.Println("\nSummary:")
	fmt.Printf("  Image: %s\n", imageFlag)
	fmt.Printf("  Disk:  %s\n", diskFlag)
	fmt.Printf("  Extra kernel args: %s\n",
		func() string {
			if len(extra) == 0 {
				return "(none)"
			}
			return strings.Join(extra, " ")
		}())
	fmt.Printf("\nWARNING: ALL DATA ON %s WILL BE ERASED!\n\n", diskFlag)
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

	log.Printf("pulling image %s", imageFlag)
	img, err := crane.Pull(imageFlag, opts)
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
				os.MkdirAll(filepath.Dir(target), 0o755)
				f, _ := os.Create(target)
				io.Copy(f, tr)
				f.Close()
				os.Chmod(target, os.FileMode(h.Mode))
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
	log.Printf("creating raw disk %s (%d GiB)", raw, *sizeGiB)
	f, _ := os.Create(raw)
	f.Truncate(int64(*sizeGiB) << 30)
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

	copyWithFsync(raw, diskFlag)
	log.Printf("installation image copied to %s", diskFlag)

	log.Print("rebooting system")
	os.WriteFile("/proc/sysrq-trigger", []byte("b"), 0)
}

/* ---------------- loop util (local) -------------------------------------- */

func setupLoop(path string) (string, *os.File) {
	ctrl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	must("open loop-control", err)
	num, _, errno := unix.Syscall(unix.SYS_IOCTL, ctrl.Fd(), unix.LOOP_CTL_GET_FREE, 0)
	if errno != 0 {
		log.Fatalf("LOOP_CTL_GET_FREE: %v", errno)
	}
	loop := fmt.Sprintf("/dev/loop%d", num)
	lf, err := os.OpenFile(loop, os.O_RDWR, 0)
	must("open loop", err)
	bf, err := os.OpenFile(path, os.O_RDWR, 0)
	must("open backing", err)
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

/* ---------------- setup transport that respects proxy settings -------------------------------------- */
func setupTransportWithProxy() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		proxyURL, err := http.ProxyFromEnvironment(req)
		if err != nil {
			log.Printf("Warning: error reading proxy settings: %v", err)
			return nil, nil // Fallback to direct connection on error.
		}

		if proxyURL != nil {
			log.Printf("Using proxy: %s", proxyURL.String())
		} else {
			log.Printf("No proxy configured, using direct connection")
		}
		return proxyURL, nil
	}
	return transport
}
