package main

import (
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
	"regexp"
	"strconv"
	"strings"
	"unsafe"

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
	modeFlag  string
)

func init() {
	flag.StringVar(&imageFlag, "image",
		"ghcr.io/cozystack/cozystack/talos:v1.11.3", "Talos installer image")
	flag.StringVar(&diskFlag, "disk", "", "target disk (will be wiped)")
	flag.BoolVar(&yesFlag, "yes", false, "automatic yes to prompts")
	flag.StringVar(&modeFlag, "mode", "", "mode: boot or install")
	flag.StringVar(&modeFlag, "m", "", "mode: boot or install (shorthand)")
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

func askMode() string {
	modeOptions := "Mode:\n" +
		"  1. boot – extract the kernel and initrd from the Talos installer and boot them directly using the kexec mechanism.\n" +
		"  2. install – prepare the environment, run the Talos installer, and then overwrite the system disk with the installed image."

	if yesFlag {
		fmt.Println(modeOptions)
		fmt.Println("Mode [1]: boot")
		return "boot"
	}
	for {
		fmt.Println(modeOptions)
		fmt.Print("Mode [1]: ")
		in, _ := reader.ReadString('\n')
		in = strings.TrimSpace(strings.ToLower(in))
		if in == "" || in == "1" || in == "boot" || in == "kexec" {
			return "boot"
		}
		if in == "2" || in == "install" {
			return "install"
		}
		fmt.Println("Please enter '1' or '2' (or 'boot'/'install').")
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

var re = regexp.MustCompile(`^([a-z0-9]+)\.(\d+)$`)

func getVID(name string) (int, string, error) {
	matches := re.FindStringSubmatch(name)
	phy := name
	if len(matches) == 3 {
		vid, err := strconv.Atoi(matches[2])
		phy = matches[1]
		if err != nil || vid < 1 || vid > 4094 {
			return 0, phy, fmt.Errorf("invalid VID: %s", matches[2])
		}
		return vid, phy, nil
	}
	return 0, phy, nil
}

/* ---------------- collect kernel arguments -------------------------------- */

func collectKernelArgs() []string {
	// Try netlink-based detection first (supports bond/bridge)
	if args := collectKernelArgsNetlink(); args != nil {
		return args
	}

	// Fallback to simple detection
	return collectKernelArgsSimple()
}

func collectKernelArgsSimple() []string {
	dev, gw, _ := defaultRoute()
	ip, mask, _ := ifaceAddr(dev)
	dev = prettyName(dev)
	hostname := getHostnameSimple()

	netOn := askYesNo("Add networking configuration?", true)
	var out []string
	if netOn {
		dev = ask("Interface", dev)
		vid, phy, err := getVID(dev)
		if err != nil {
			log.Fatalf("%s", err)
		}
		ip = ask("IP address", ip)
		mask = ask("Netmask", mask)
		gw = ask("Gateway (or 'none')", gw)
		if strings.EqualFold(gw, "none") {
			gw = ""
		}
		hostname = ask("Hostname", hostname)
		out = append(out, fmt.Sprintf("ip=%s::%s:%s:%s:%s:none", ip, gw, mask, hostname, dev))
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

func getHostnameSimple() string {
	hostname, err := os.Hostname()
	if err != nil {
		return ""
	}
	// Remove domain part if present
	if idx := strings.IndexByte(hostname, '.'); idx > 0 {
		hostname = hostname[:idx]
	}
	return hostname
}

/* ------------------------------ main -------------------------------------- */

func main() {
	var extra multiFlag
	sizeGiB := flag.Uint64("image-size-gib", 3, "image.raw size (GiB)")
	flag.Var(&extra, "extra-kernel-arg", "extra kernel arg (repeatable)")
	flag.Parse()

	// If mode is not specified, ask as first question
	if modeFlag == "" {
		modeFlag = askMode()
	} else {
		// Check validity of specified mode
		if modeFlag != "boot" && modeFlag != "install" {
			log.Fatalf("invalid mode: %s (must be 'boot' or 'install')", modeFlag)
		}
	}

	if imageFlag == flag.Lookup("image").DefValue {
		imageFlag = ask("Talos installer image", imageFlag)
	}

	// For install mode, ask for target disk after image selection
	if modeFlag == "install" && diskFlag == "" {
		def := firstDisk()
		if def == "" {
			diskFlag = askRequired("Target disk")
		} else {
			diskFlag = ask("Target disk", def)
		}
	}

	// Collect kernel args for both modes
	for _, e := range collectKernelArgs() {
		extra = append(extra, e)
	}

	// If not installation mode, use boot
	if modeFlag == "boot" {
		runBootMode(imageFlag, extra)
		return
	}

	// Installation mode
	runInstallMode(imageFlag, diskFlag, extra, *sizeGiB)
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
