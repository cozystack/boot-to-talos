//go:build linux

package network

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"strings"
	"testing"
	"unsafe"

	"github.com/cockroachdb/errors"
	"golang.org/x/sys/unix"

	"github.com/cozystack/boot-to-talos/internal/cli"
)

// TestEthtoolIfreqSizeMatchesKernel guards against the OOB-read class of bugs
// where Go's ethtoolIfreq is smaller than the kernel's struct ifreq. The
// SIOCETHTOOL ioctl path copies sizeof(struct ifreq) bytes out of userspace,
// so any future field rearrangement that shrinks ethtoolIfreq below
// unix.Ifreq's size would let the kernel read past the allocation boundary.
func TestEthtoolIfreqSizeMatchesKernel(t *testing.T) {
	got := unsafe.Sizeof(ethtoolIfreq{})
	want := unsafe.Sizeof(unix.Ifreq{})
	if got != want {
		t.Errorf("ethtoolIfreq size = %d, want %d (struct ifreq size); SIOCETHTOOL would read out of bounds", got, want)
	}
}

func TestIsZeroMAC(t *testing.T) {
	tests := []struct {
		name string
		mac  net.HardwareAddr
		want bool
	}{
		{"nil", nil, true},
		{"empty", net.HardwareAddr{}, true},
		{"all zero", net.HardwareAddr{0, 0, 0, 0, 0, 0}, true},
		{"first byte set", net.HardwareAddr{0x10, 0, 0, 0, 0, 0}, false},
		{"last byte set", net.HardwareAddr{0, 0, 0, 0, 0, 0x01}, false},
		{"non-zero throughout", net.HardwareAddr{0x10, 0xff, 0xe0, 0x3a, 0xd6, 0x86}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isZeroMAC(tc.mac); got != tc.want {
				t.Errorf("isZeroMAC(%v) = %v, want %v", tc.mac, got, tc.want)
			}
		})
	}
}

func TestPickInterface(t *testing.T) {
	tests := []struct {
		name             string
		override         string
		detected         string
		wantDev          string
		wantFromOverride bool
	}{
		{
			name:             "no override, detected used",
			override:         "",
			detected:         "eth0",
			wantDev:          "eth0",
			wantFromOverride: false,
		},
		{
			name:             "override wins over detected",
			override:         "eth0.4000",
			detected:         "eth0",
			wantDev:          "eth0.4000",
			wantFromOverride: true,
		},
		{
			name:             "override wins even when detection failed",
			override:         "bond0",
			detected:         "",
			wantDev:          "bond0",
			wantFromOverride: true,
		},
		{
			name:             "empty override, empty detected, both empty",
			override:         "",
			detected:         "",
			wantDev:          "",
			wantFromOverride: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dev, fromOverride := pickInterface(tt.override, tt.detected)
			if dev != tt.wantDev {
				t.Errorf("dev = %q, want %q", dev, tt.wantDev)
			}
			if fromOverride != tt.wantFromOverride {
				t.Errorf("fromOverride = %v, want %v", fromOverride, tt.wantFromOverride)
			}
		})
	}
}

// swapFns swaps the package-level injected helpers for the duration of the
// test, restoring originals via t.Cleanup. Keeps individual tests focused on
// the override behaviour rather than the injection mechanics. NOT safe under
// t.Parallel — the helpers mutate package globals; if a future test enables
// parallelism, gate the swaps behind a sync.Mutex.
func swapFns(t *testing.T, dr func() (string, string, error), ia func(string) (string, string, error), pn func(string) string) {
	t.Helper()
	origDR, origIA, origPN := defaultRouteFn, ifaceAddrFn, prettyNameFn
	defaultRouteFn = dr
	ifaceAddrFn = ia
	prettyNameFn = pn
	t.Cleanup(func() {
		defaultRouteFn = origDR
		ifaceAddrFn = origIA
		prettyNameFn = origPN
	})
}

// swapNetInfo swaps collectNetworkInfoFn for the duration of the test.
func swapNetInfo(t *testing.T, fn func() (*NetworkInfo, error)) {
	t.Helper()
	orig := collectNetworkInfoFn
	collectNetworkInfoFn = fn
	t.Cleanup(func() { collectNetworkInfoFn = orig })
}

// buildNetInfo constructs a NetworkInfo from a flat list of LinkInfo values,
// initialising the private linkIndex / linkName maps that GetLinkByIndex /
// GetLinkByName rely on. Tests use this to assemble VLAN / bond topologies
// without standing up real netlink.
func buildNetInfo(links []LinkInfo) *NetworkInfo {
	info := &NetworkInfo{
		Links:     links,
		linkIndex: make(map[uint32]*LinkInfo),
		linkName:  make(map[string]*LinkInfo),
	}
	for i := range info.Links {
		l := &info.Links[i]
		info.linkIndex[l.Index] = l
		info.linkName[l.Name] = l
	}
	return info
}

// withYes flips cli.YesFlag for the duration of the test and restores it.
// Required because collectKernelArgsSimple consults cli.AskYesNo / cli.Ask,
// which only return defaults when YesFlag is set.
func withYes(t *testing.T) {
	t.Helper()
	orig := cli.YesFlag
	cli.YesFlag = true
	t.Cleanup(func() { cli.YesFlag = orig })
}

// captureLog redirects the standard logger to a buffer for assertion on
// log output (used by the empty-gateway warning test).
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})
	return &buf
}

func TestCollectKernelArgsSimple_OverrideBypassesPrettyName(t *testing.T) {
	withYes(t)
	swapFns(t,
		// DefaultRoute reports the parent NIC as default.
		func() (string, string, error) { return "eth0", "192.168.1.1", nil },
		// IfaceAddr returns an address only for the VLAN child.
		func(dev string) (string, string, error) {
			if dev == "eth0.10" {
				return "10.0.0.5", "255.255.255.0", nil
			}
			return "", "", errors.New("no IPv4")
		},
		// PrettyName must NOT be called for an override; if it is, force a
		// visibly-wrong result so the test fails loudly.
		func(string) string { return "POISONED" },
	)

	out := collectKernelArgsSimple("eth0.10")

	var ipLine string
	for _, a := range out {
		if strings.HasPrefix(a, "ip=") {
			ipLine = a
		}
	}
	if ipLine == "" {
		t.Fatalf("collectKernelArgsSimple returned no ip= line; got %v", out)
	}
	if strings.Contains(ipLine, "POISONED") {
		t.Errorf("ip= line was rewritten by PrettyName despite override: %q", ipLine)
	}
	if !strings.Contains(ipLine, ":eth0.10:") {
		t.Errorf("ip= line missing override device name: %q", ipLine)
	}
	if !strings.Contains(ipLine, "10.0.0.5") {
		t.Errorf("ip= line missing override-interface address: %q", ipLine)
	}
}

func TestCollectKernelArgsSimple_NoOverrideUsesPrettyName(t *testing.T) {
	withYes(t)
	swapFns(t,
		func() (string, string, error) { return "eth0", "192.168.1.1", nil },
		func(string) (string, string, error) { return "192.168.1.42", "255.255.255.0", nil },
		// Standard predictable-name rewrite for non-override path.
		func(string) string { return "enp0s31f6" },
	)

	out := collectKernelArgsSimple("")

	var ipLine string
	for _, a := range out {
		if strings.HasPrefix(a, "ip=") {
			ipLine = a
		}
	}
	if !strings.Contains(ipLine, ":enp0s31f6:") {
		t.Errorf("ip= line missing predictable name: %q", ipLine)
	}
}

// TestCollectKernelArgsNetlink_OverrideVLANChildBypassesPrettyName proves that
// when the user passes a VLAN-child override, the netlink path emits the
// override verbatim in both the leaf ip=...:<dev>:none and the vlan= cmdline,
// and crucially does NOT route the parent name through PrettyName (which would
// rewrite eth0 → enx<mac> and break the kernel's link lookup).
func TestCollectKernelArgsNetlink_OverrideVLANChildBypassesPrettyName(t *testing.T) {
	withYes(t)
	eth0 := LinkInfo{Name: "eth0", Index: 1, Kind: ""}
	vlan := LinkInfo{
		Name:      "eth0.10",
		Index:     2,
		LinkIndex: 1,
		Kind:      "vlan",
		VLAN:      &VLANSpec{VID: 10, Protocol: 0x8100},
	}
	swapNetInfo(t, func() (*NetworkInfo, error) {
		return buildNetInfo([]LinkInfo{eth0, vlan}), nil
	})
	swapFns(t,
		func() (string, string, error) { return "eth0", "192.168.1.1", nil },
		func(dev string) (string, string, error) {
			if dev == "eth0.10" {
				return "10.0.0.5", "255.255.255.0", nil
			}
			return "", "", errors.New("no IPv4")
		},
		func(string) string { return "POISONED" },
	)

	out := collectKernelArgsNetlink("eth0.10")
	if out == nil {
		t.Fatal("collectKernelArgsNetlink returned nil for override; expected cmdline")
	}
	joined := strings.Join(out, " ")
	if strings.Contains(joined, "POISONED") {
		t.Errorf("netlink path used PrettyName on override path: %q", joined)
	}
	if !strings.Contains(joined, "ip=") || !strings.Contains(joined, ":eth0.10:none") {
		t.Errorf("ip= line missing override leaf %q in: %q", "eth0.10", joined)
	}
	if !strings.Contains(joined, "vlan=eth0.10:eth0") {
		t.Errorf("vlan= cmdline missing expected eth0.10:eth0 in: %q", joined)
	}
}

// TestCollectKernelArgsNetlink_OverrideBondNameStaysConsistent proves the
// emitted bond= and ip= lines reference the same master name when the bond
// happens to be called something other than "bond0" and the user overrides
// to that real name. With actual slaves present GenerateBondCmdline emits a
// non-empty bond= line, so this test validates the full internal-consistency
// invariant: whatever appears in :DEV:none of ip= must also appear as the
// master name in the bond= line.
func TestCollectKernelArgsNetlink_OverrideBondNameStaysConsistent(t *testing.T) {
	withYes(t)
	bond := LinkInfo{
		Name:       "team0",
		Index:      1,
		Kind:       "bond",
		BondMaster: &BondMasterSpec{Mode: BondModeBalanceRR, MIIMon: 100},
	}
	slave1 := LinkInfo{
		Name:        "eth0",
		Index:       2,
		MasterIndex: 1,
		SlaveKind:   "bond",
	}
	slave2 := LinkInfo{
		Name:        "eth1",
		Index:       3,
		MasterIndex: 1,
		SlaveKind:   "bond",
	}
	swapNetInfo(t, func() (*NetworkInfo, error) {
		return buildNetInfo([]LinkInfo{bond, slave1, slave2}), nil
	})
	swapFns(t,
		func() (string, string, error) { return "team0", "192.168.1.1", nil },
		func(dev string) (string, string, error) {
			if dev == "team0" {
				return "10.0.0.5", "255.255.255.0", nil
			}
			return "", "", errors.New("no IPv4")
		},
		func(string) string { return "POISONED" },
	)

	out := collectKernelArgsNetlink("team0")
	if out == nil {
		t.Fatal("collectKernelArgsNetlink returned nil; expected cmdline")
	}
	joined := strings.Join(out, " ")

	var bondLine, ipLine string
	for _, a := range out {
		switch {
		case strings.HasPrefix(a, "bond="):
			bondLine = a
		case strings.HasPrefix(a, "ip="):
			ipLine = a
		}
	}
	if bondLine == "" {
		t.Fatalf("no bond= line emitted; got %v", out)
	}
	if !strings.HasPrefix(bondLine, "bond=team0:") {
		t.Errorf("bond= master name not overridden: %q", bondLine)
	}
	if !strings.Contains(ipLine, ":team0:none") {
		t.Errorf("ip= line did not carry override device name: %q", ipLine)
	}
	if strings.Contains(joined, "bond=bond0") || strings.Contains(joined, ":bond0:") {
		t.Errorf("hardcoded bond0 leaked into cmdline despite override: %q", joined)
	}
	if strings.Contains(joined, "POISONED") {
		t.Errorf("PrettyName leaked into cmdline on override path: %q", joined)
	}
}

// TestCollectKernelArgsNetlink_OverrideRenamedVLANUsesUserName proves the
// fromOverride leaf-name branch: when the live link name does not follow the
// <parent>.<vid> convention (ifrename'd VLAN), the cmdline must carry the
// user-supplied override verbatim, not a derived <parent>.<vid> form.
func TestCollectKernelArgsNetlink_OverrideRenamedVLANUsesUserName(t *testing.T) {
	withYes(t)
	eth0 := LinkInfo{Name: "eth0", Index: 1, Kind: ""}
	vlan := LinkInfo{
		Name:      "myvlan",
		Index:     2,
		LinkIndex: 1,
		Kind:      "vlan",
		VLAN:      &VLANSpec{VID: 10, Protocol: 0x8100},
	}
	swapNetInfo(t, func() (*NetworkInfo, error) {
		return buildNetInfo([]LinkInfo{eth0, vlan}), nil
	})
	swapFns(t,
		func() (string, string, error) { return "eth0", "192.168.1.1", nil },
		func(dev string) (string, string, error) {
			if dev == "myvlan" {
				return "10.0.0.5", "255.255.255.0", nil
			}
			return "", "", errors.New("no IPv4")
		},
		func(string) string { return "POISONED" },
	)

	out := collectKernelArgsNetlink("myvlan")
	if out == nil {
		t.Fatal("collectKernelArgsNetlink returned nil; expected cmdline")
	}
	joined := strings.Join(out, " ")
	if !strings.Contains(joined, "vlan=myvlan:eth0") {
		t.Errorf("vlan= line did not use override name verbatim: %q", joined)
	}
	if strings.Contains(joined, "eth0.10") {
		t.Errorf("derived <parent>.<vid> name leaked into cmdline despite ifrename override: %q", joined)
	}
	if !strings.Contains(joined, ":myvlan:none") {
		t.Errorf("ip= line did not carry override leaf: %q", joined)
	}
}

// TestCollectKernelArgsNetlink_OverrideBondSlaveNamesAreVerbatim pins the
// bond= line slave-naming invariant on the override path: slaves go through
// the injected prettyNameFn under no-override, but stay raw under override so
// the entire emitted cmdline is internally consistent.
func TestCollectKernelArgsNetlink_OverrideBondSlaveNamesAreVerbatim(t *testing.T) {
	withYes(t)
	bond := LinkInfo{
		Name:       "team0",
		Index:      1,
		Kind:       "bond",
		BondMaster: &BondMasterSpec{Mode: BondModeBalanceRR, MIIMon: 100},
	}
	s1 := LinkInfo{Name: "eth0", Index: 2, MasterIndex: 1, SlaveKind: "bond"}
	s2 := LinkInfo{Name: "eth1", Index: 3, MasterIndex: 1, SlaveKind: "bond"}
	swapNetInfo(t, func() (*NetworkInfo, error) {
		return buildNetInfo([]LinkInfo{bond, s1, s2}), nil
	})
	swapFns(t,
		func() (string, string, error) { return "team0", "192.168.1.1", nil },
		func(dev string) (string, string, error) {
			if dev == "team0" {
				return "10.0.0.5", "255.255.255.0", nil
			}
			return "", "", errors.New("no IPv4")
		},
		// PrettyName must not appear in the bond= line under override.
		func(string) string { return "POISONED" },
	)

	out := collectKernelArgsNetlink("team0")
	if out == nil {
		t.Fatal("collectKernelArgsNetlink returned nil; expected cmdline")
	}
	joined := strings.Join(out, " ")
	if !strings.Contains(joined, "bond=team0:eth0,eth1:") {
		t.Errorf("bond= line did not emit raw slave names under override: %q", joined)
	}
	if strings.Contains(joined, "POISONED") {
		t.Errorf("PrettyName leaked into bond slave names despite override: %q", joined)
	}
}

// TestCollectKernelArgsSimple_FailLoudUnderYesOverrideMissingIP pins the
// non-interactive-install safety guard: -yes + override + missing IPv4 must
// surface a fatal error rather than silently emit ip=::<gw>:::<dev>:none.
func TestCollectKernelArgsSimple_FailLoudUnderYesOverrideMissingIP(t *testing.T) {
	withYes(t)
	swapFns(t,
		func() (string, string, error) { return "eth0", "192.168.1.1", nil },
		func(string) (string, string, error) { return "", "", errors.New("no IPv4") },
		func(string) string { return "POISONED" },
	)

	origFatal := fatalf
	var fatalMsg string
	fatalf = func(format string, args ...any) {
		fatalMsg = sprintf(format, args...)
		panic(fatalSentinel{})
	}
	defer func() { fatalf = origFatal }()

	defer func() {
		r := recover()
		if _, ok := r.(fatalSentinel); !ok {
			t.Fatalf("expected fatalSentinel panic; got recover()=%v", r)
		}
		if !strings.Contains(fatalMsg, `override interface "eth0.999"`) {
			t.Errorf("fatal message missing override identifier: %q", fatalMsg)
		}
	}()

	_ = collectKernelArgsSimple("eth0.999")
}

// fatalSentinel is the panic value emitted by the test's fatalf stub. Using
// a typed sentinel (rather than an error) lets the fail-loud test
// distinguish its own panic from any unrelated runtime panic without
// tripping err113.
type fatalSentinel struct{}

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// TestCollectKernelArgsNetlink_OverrideMissingInterfaceWarns mirrors the
// simple-path test of the same shape: when the override interface has no
// IPv4 address, the netlink path must surface a warning that names the
// override (not the generic "failed to get IP address" message) before
// returning nil to fall back to the simple path.
func TestCollectKernelArgsNetlink_OverrideMissingInterfaceWarns(t *testing.T) {
	withYes(t)
	buf := captureLog(t)
	link := LinkInfo{Name: "eth0.999", Index: 1, Kind: "vlan", VLAN: &VLANSpec{VID: 999}}
	swapNetInfo(t, func() (*NetworkInfo, error) {
		return buildNetInfo([]LinkInfo{link}), nil
	})
	swapFns(t,
		func() (string, string, error) { return "eth0", "192.168.1.1", nil },
		func(string) (string, string, error) { return "", "", errors.New("no IPv4") },
		func(string) string { return "POISONED" },
	)

	if out := collectKernelArgsNetlink("eth0.999"); out != nil {
		t.Errorf("netlink path returned %v under missing-IP override; expected nil so simple path runs", out)
	}
	if !strings.Contains(buf.String(), `override interface "eth0.999"`) {
		t.Errorf("expected override-specific warning naming eth0.999; got %q", buf.String())
	}
}

// TestVLANParentName covers the helper directly: bond match wins over
// override; otherwise override returns the raw parent name while no-override
// goes through the injected PrettyName.
func TestVLANParentName(t *testing.T) {
	swapFns(t,
		func() (string, string, error) { return "", "", nil },
		func(string) (string, string, error) { return "", "", nil },
		func(string) string { return "PRETTY" },
	)
	bond := &LinkInfo{Name: "bond0", Kind: "bond"}
	eth := &LinkInfo{Name: "eth0"}

	if got := vlanParentName(bond, bond, "bond0", false); got != "bond0" {
		t.Errorf("bond parent: got %q, want bond0", got)
	}
	if got := vlanParentName(eth, eth, "bond0", true); got != "eth0" {
		t.Errorf("override path returns raw name: got %q, want eth0", got)
	}
	if got := vlanParentName(eth, eth, "bond0", false); got != "PRETTY" {
		t.Errorf("non-override path goes through PrettyName: got %q, want PRETTY", got)
	}
	if got := vlanParentName(nil, eth, "bond0", false); got != "unknown" {
		t.Errorf("nil parent: got %q, want unknown", got)
	}
}

// TestCollectKernelArgsSimple_InteractiveOverrideMissingIPWarns covers the
// non-fatal path: with YesFlag disabled the simple path must still log a
// specific warning naming the override interface (operator answers ip/mask
// interactively).
func TestCollectKernelArgsSimple_InteractiveOverrideMissingIPWarns(t *testing.T) {
	// Note: cli.YesFlag stays false here — the fail-loud guard only fires
	// under -yes; in interactive mode we want a warning and a prompt.
	buf := captureLog(t)
	swapFns(t,
		func() (string, string, error) { return "eth0", "192.168.1.1", nil },
		func(string) (string, string, error) { return "", "", errors.New("no IPv4") },
		func(string) string { return "POISONED" },
	)

	// Override fatalf so the interactive path that drains stdin via cli.Ask
	// does not block the test; the helper returns nothing on EOF.
	origFatal := fatalf
	fatalf = func(string, ...any) {}
	defer func() { fatalf = origFatal }()

	_ = collectKernelArgsSimple("eth0.999")

	if !strings.Contains(buf.String(), "failed to read IPv4 address for override interface") {
		t.Errorf("expected warning about override interface; got %q", buf.String())
	}
}

func TestCollectKernelArgsSimple_OverrideEmptyGatewayWarns(t *testing.T) {
	withYes(t)
	buf := captureLog(t)
	swapFns(t,
		func() (string, string, error) {
			return "", "", errors.New("no default route")
		},
		func(string) (string, string, error) { return "10.0.0.5", "255.255.255.0", nil },
		func(string) string { return "POISONED" },
	)

	out := collectKernelArgsSimple("eth0.10")

	logged := buf.String()
	if !strings.Contains(logged, "gateway will be empty") {
		t.Errorf("expected empty-gateway warning in log; got %q", logged)
	}
	var ipLine string
	for _, a := range out {
		if strings.HasPrefix(a, "ip=") {
			ipLine = a
		}
	}
	if !strings.Contains(ipLine, ":eth0.10:") {
		t.Errorf("override device missing from ip= line: %q", ipLine)
	}
}
