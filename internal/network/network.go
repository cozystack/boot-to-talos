//go:build linux

package network

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"unsafe"

	"github.com/cockroachdb/errors"
	"github.com/jsimonetti/rtnetlink/v2"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"

	"github.com/cozystack/boot-to-talos/internal/cli"
)

// Bond mode constants.
const (
	BondModeBalanceRR    uint8 = 0 // balance-rr
	BondModeActiveBackup uint8 = 1 // active-backup
	BondModeBalanceXOR   uint8 = 2 // balance-xor
	BondModeBroadcast    uint8 = 3 // broadcast
	BondMode8023AD       uint8 = 4 // 802.3ad (LACP)
	BondModeBalanceTLB   uint8 = 5 // balance-tlb
	BondModeBalanceALB   uint8 = 6 // balance-alb
)

// LACP rate constants.
const (
	LACPRateSlow uint8 = 0
	LACPRateFast uint8 = 1
)

// Hash policy constants.
const (
	BondXmitHashPolicyLayer2     uint8 = 0
	BondXmitHashPolicyLayer34    uint8 = 1
	BondXmitHashPolicyLayer23    uint8 = 2
	BondXmitHashPolicyEncap23    uint8 = 3
	BondXmitHashPolicyEncap34    uint8 = 4
	BondXmitHashPolicyVlanSrcMAC uint8 = 5
)

// LinkInfo represents network interface information.
type LinkInfo struct {
	Name             string
	Index            uint32
	Type             uint16
	LinkIndex        uint32 // Parent interface index (for VLAN, etc.)
	Flags            uint32
	HardwareAddr     net.HardwareAddr
	MTU              uint32
	MasterIndex      uint32
	OperationalState rtnetlink.OperationalState
	Kind             string
	SlaveKind        string
	BondMaster       *BondMasterSpec
	VLAN             *VLANSpec
}

// VLANSpec represents VLAN configuration.
type VLANSpec struct {
	VID      uint16 // VLAN ID (1-4094)
	Protocol uint16 // VLAN protocol (0x8100 for 802.1Q, 0x88a8 for 802.1ad)
}

// BondMasterSpec represents bond master configuration.
type BondMasterSpec struct {
	Mode         uint8
	HashPolicy   uint8
	LACPRate     uint8
	MIIMon       uint32
	UpDelay      uint32
	DownDelay    uint32
	ARPInterval  uint32
	ARPIPTargets []netip.Addr
	PrimaryIndex *uint32
	UseCarrier   bool
}

// NetworkInfo contains all collected network information.
type NetworkInfo struct {
	Links     []LinkInfo
	linkIndex map[uint32]*LinkInfo
	linkName  map[string]*LinkInfo
}

// IsBond returns true if the link is a bond interface.
func (l *LinkInfo) IsBond() bool {
	return l.Kind == "bond"
}

// IsBondSlave returns true if the link is a bond slave.
func (l *LinkInfo) IsBondSlave() bool {
	return l.SlaveKind == "bond"
}

// IsBridge returns true if the link is a bridge interface.
func (l *LinkInfo) IsBridge() bool {
	return l.Kind == "bridge"
}

// IsBridgeSlave returns true if the link is a bridge port.
func (l *LinkInfo) IsBridgeSlave() bool {
	return l.SlaveKind == "bridge"
}

// IsVLAN returns true if the link is a VLAN interface.
func (l *LinkInfo) IsVLAN() bool {
	return l.Kind == "vlan"
}

// IsPhysical returns true if the link is a physical ethernet interface.
func (l *LinkInfo) IsPhysical() bool {
	return l.Kind == "" && l.Type == 1 && !l.IsBondSlave() && !l.IsBridgeSlave()
}

// GetLinkByIndex returns link by index.
func (n *NetworkInfo) GetLinkByIndex(index uint32) *LinkInfo {
	return n.linkIndex[index]
}

// GetLinkByName returns link by name.
func (n *NetworkInfo) GetLinkByName(name string) *LinkInfo {
	return n.linkName[name]
}

// GetBondSlaves returns slave interfaces for a bond.
func (n *NetworkInfo) GetBondSlaves(masterIndex uint32) []*LinkInfo {
	var slaves []*LinkInfo
	for i := range n.Links {
		l := &n.Links[i]
		if l.IsBondSlave() && l.MasterIndex == masterIndex {
			slaves = append(slaves, l)
		}
	}
	return slaves
}

// GetBridgePorts returns port interfaces for a bridge.
func (n *NetworkInfo) GetBridgePorts(masterIndex uint32) []*LinkInfo {
	var ports []*LinkInfo
	for i := range n.Links {
		l := &n.Links[i]
		if l.MasterIndex == masterIndex {
			ports = append(ports, l)
		}
	}
	return ports
}

// GetVLANChain returns all VLAN interfaces in the path from link to physical/bond.
// Returns VLANs in order from topmost to lowest (closest to physical).
func (n *NetworkInfo) GetVLANChain(link *LinkInfo) []*LinkInfo {
	var vlans []*LinkInfo
	current := link

	for current != nil {
		if current.IsVLAN() {
			vlans = append(vlans, current)
			if current.LinkIndex > 0 {
				current = n.GetLinkByIndex(current.LinkIndex)
			} else {
				break
			}
		} else {
			break
		}
	}

	return vlans
}

// CollectNetworkInfo gathers all network interface information via netlink.
//
//nolint:gocognit
func CollectNetworkInfo() (*NetworkInfo, error) {
	conn, err := rtnetlink.Dial(nil)
	if err != nil {
		return nil, errors.Wrap(err, "error dialing rtnetlink socket")
	}
	defer conn.Close()

	links, err := conn.Link.List()
	if err != nil {
		return nil, errors.Wrap(err, "error listing links")
	}

	info := &NetworkInfo{
		linkIndex: make(map[uint32]*LinkInfo),
		linkName:  make(map[string]*LinkInfo),
	}

	for _, link := range links {
		li := LinkInfo{
			Name:             link.Attributes.Name,
			Index:            link.Index,
			Type:             link.Type,
			Flags:            link.Flags,
			HardwareAddr:     link.Attributes.Address,
			MTU:              link.Attributes.MTU,
			OperationalState: link.Attributes.OperationalState,
		}

		// Get parent interface index (used by VLAN and other stacked interfaces)
		if link.Attributes.Type != 0 {
			li.LinkIndex = link.Attributes.Type
		}

		if link.Attributes.Master != nil {
			li.MasterIndex = *link.Attributes.Master
		}

		if link.Attributes.Info != nil {
			li.Kind = link.Attributes.Info.Kind
			li.SlaveKind = link.Attributes.Info.SlaveKind

			if link.Attributes.Info.Data != nil {
				if linkData, ok := link.Attributes.Info.Data.(*rtnetlink.LinkData); ok {
					switch li.Kind {
					case "bond":
						bondSpec, err := decodeBondMasterSpec(linkData.Data)
						if err != nil {
							log.Printf("warning: failed to decode bond master spec for %s: %v", link.Attributes.Name, err)
						} else {
							li.BondMaster = bondSpec
						}
					case "vlan":
						vlanSpec, err := decodeVLANSpec(linkData.Data)
						if err != nil {
							log.Printf("warning: failed to decode VLAN spec for %s: %v", link.Attributes.Name, err)
						} else {
							li.VLAN = vlanSpec
						}
					}
				}
			}
		}

		info.Links = append(info.Links, li)
	}

	// Build indexes
	for i := range info.Links {
		l := &info.Links[i]
		info.linkIndex[l.Index] = l
		info.linkName[l.Name] = l
	}

	return info, nil
}

func decodeBondMasterSpec(data []byte) (*BondMasterSpec, error) {
	spec := &BondMasterSpec{}
	decoder, err := netlink.NewAttributeDecoder(data)
	if err != nil {
		return nil, err
	}

	for decoder.Next() {
		switch decoder.Type() {
		case unix.IFLA_BOND_MODE:
			spec.Mode = decoder.Uint8()
		case unix.IFLA_BOND_MIIMON:
			spec.MIIMon = decoder.Uint32()
		case unix.IFLA_BOND_UPDELAY:
			spec.UpDelay = decoder.Uint32()
		case unix.IFLA_BOND_DOWNDELAY:
			spec.DownDelay = decoder.Uint32()
		case unix.IFLA_BOND_USE_CARRIER:
			spec.UseCarrier = decoder.Uint8() == 1
		case unix.IFLA_BOND_ARP_INTERVAL:
			spec.ARPInterval = decoder.Uint32()
		case unix.IFLA_BOND_ARP_IP_TARGET:
			decoder.Nested(func(nad *netlink.AttributeDecoder) error {
				for nad.Next() {
					addr, ok := netip.AddrFromSlice(nad.Bytes())
					if ok {
						spec.ARPIPTargets = append(spec.ARPIPTargets, addr)
					}
				}
				return nil
			})
		case unix.IFLA_BOND_PRIMARY:
			val := decoder.Uint32()
			spec.PrimaryIndex = &val
		case unix.IFLA_BOND_XMIT_HASH_POLICY:
			spec.HashPolicy = decoder.Uint8()
		case unix.IFLA_BOND_AD_LACP_RATE:
			spec.LACPRate = decoder.Uint8()
		}
	}

	return spec, decoder.Err()
}

func decodeVLANSpec(data []byte) (*VLANSpec, error) {
	spec := &VLANSpec{}
	decoder, err := netlink.NewAttributeDecoder(data)
	if err != nil {
		return nil, err
	}

	for decoder.Next() {
		switch decoder.Type() {
		case unix.IFLA_VLAN_ID:
			spec.VID = decoder.Uint16()
		case unix.IFLA_VLAN_PROTOCOL:
			// Protocol is stored in network byte order (big-endian)
			b := decoder.Bytes()
			if len(b) >= 2 {
				spec.Protocol = uint16(b[0])<<8 | uint16(b[1])
			}
		}
	}

	return spec, decoder.Err()
}

// ResolveNetworkDevice finds the actual device to use for network configuration.
// If the device is a bridge, it finds the underlying physical interface or bond.
// If the device is a bond, it returns the bond itself.
// If the device is a VLAN, it recursively resolves the parent interface.
//
//nolint:gocognit
func ResolveNetworkDevice(info *NetworkInfo, link *LinkInfo) *LinkInfo {
	if link == nil {
		return nil
	}

	// If it's a VLAN, recursively resolve the parent interface
	if link.IsVLAN() && link.LinkIndex > 0 {
		parent := info.GetLinkByIndex(link.LinkIndex)
		if parent != nil {
			return ResolveNetworkDevice(info, parent)
		}
	}

	// If it's a bridge, find the underlying device
	if link.IsBridge() {
		ports := info.GetBridgePorts(link.Index)
		for _, port := range ports {
			// Prefer bond over physical interface
			if port.IsBond() {
				return port
			}
			// Also check for VLAN on bond
			if port.IsVLAN() {
				resolved := ResolveNetworkDevice(info, port)
				if resolved != nil && resolved.IsBond() {
					return resolved
				}
			}
		}
		// Fall back to first port that is physical or bond
		for _, port := range ports {
			if port.IsPhysical() || port.IsBond() {
				return port
			}
		}
		// No suitable port found, maybe bridge has bond slave
		for _, port := range ports {
			if port.MasterIndex > 0 {
				master := info.GetLinkByIndex(port.MasterIndex)
				if master != nil && master.IsBond() {
					return master
				}
			}
		}
	}

	// If it's a bond, return it
	if link.IsBond() {
		return link
	}

	// If it's a bond slave, return the bond master
	if link.IsBondSlave() && link.MasterIndex > 0 {
		master := info.GetLinkByIndex(link.MasterIndex)
		if master != nil && master.IsBond() {
			return master
		}
	}

	// Return the link as is (physical interface)
	return link
}

// BondModeToString converts bond mode constant to kernel string.
func BondModeToString(mode uint8) string {
	switch mode {
	case BondModeBalanceRR:
		return "balance-rr"
	case BondModeActiveBackup:
		return "active-backup"
	case BondModeBalanceXOR:
		return "balance-xor"
	case BondModeBroadcast:
		return "broadcast"
	case BondMode8023AD:
		return "802.3ad"
	case BondModeBalanceTLB:
		return "balance-tlb"
	case BondModeBalanceALB:
		return "balance-alb"
	default:
		return fmt.Sprintf("mode%d", mode)
	}
}

// HashPolicyToString converts hash policy constant to kernel string.
func HashPolicyToString(policy uint8) string {
	switch policy {
	case BondXmitHashPolicyLayer2:
		return "layer2"
	case BondXmitHashPolicyLayer34:
		return "layer3+4"
	case BondXmitHashPolicyLayer23:
		return "layer2+3"
	case BondXmitHashPolicyEncap23:
		return "encap2+3"
	case BondXmitHashPolicyEncap34:
		return "encap3+4"
	case BondXmitHashPolicyVlanSrcMAC:
		return "vlan+srcmac"
	default:
		return "layer2"
	}
}

// LACPRateToString converts LACP rate to kernel string.
func LACPRateToString(rate uint8) string {
	if rate == LACPRateFast {
		return "fast"
	}
	return "slow"
}

// GenerateBondCmdline generates kernel cmdline for bond configuration.
// Format: bond=<bondname>:<slaves>:<options>
//
// When verbatim is true, slave names are emitted as-is (skipping the
// perm_addr-based PrettyName rewrite) — used on the --override-interface path
// so the entire cmdline carries user-supplied names instead of partially
// rewriting slaves into enx<mac>-style names.
func GenerateBondCmdline(info *NetworkInfo, bond *LinkInfo, bondName string, verbatim bool) string {
	if bond == nil || !bond.IsBond() || bond.BondMaster == nil {
		return ""
	}

	// Get slave interfaces
	slaves := info.GetBondSlaves(bond.Index)
	if len(slaves) == 0 {
		return ""
	}

	// Build slave list. Under verbatim mode the user has signed off on the
	// link names already (override is active), so do not run them through
	// PrettyName.
	var slaveNames []string
	for _, slave := range slaves {
		if verbatim {
			slaveNames = append(slaveNames, slave.Name)
		} else {
			slaveNames = append(slaveNames, prettyNameFn(slave.Name))
		}
	}

	// Build options
	var options []string

	// Mode
	options = append(options, fmt.Sprintf("mode=%s", BondModeToString(bond.BondMaster.Mode)))

	// Hash policy (for modes that use it)
	if bond.BondMaster.Mode == BondMode8023AD ||
		bond.BondMaster.Mode == BondModeBalanceXOR ||
		bond.BondMaster.Mode == BondModeBalanceTLB ||
		bond.BondMaster.Mode == BondModeBalanceALB {
		options = append(options, fmt.Sprintf("xmit_hash_policy=%s", HashPolicyToString(bond.BondMaster.HashPolicy)))
	}

	// LACP rate (only for 802.3ad)
	if bond.BondMaster.Mode == BondMode8023AD {
		options = append(options, fmt.Sprintf("lacp_rate=%s", LACPRateToString(bond.BondMaster.LACPRate)))
	}

	// MII monitoring
	if bond.BondMaster.MIIMon > 0 {
		options = append(options, fmt.Sprintf("miimon=%d", bond.BondMaster.MIIMon))
	}

	// Updelay (only if miimon is set)
	if bond.BondMaster.MIIMon > 0 && bond.BondMaster.UpDelay > 0 {
		options = append(options, fmt.Sprintf("updelay=%d", bond.BondMaster.UpDelay))
	}

	// Downdelay (only if miimon is set)
	if bond.BondMaster.MIIMon > 0 && bond.BondMaster.DownDelay > 0 {
		options = append(options, fmt.Sprintf("downdelay=%d", bond.BondMaster.DownDelay))
	}

	return fmt.Sprintf("bond=%s:%s:%s",
		bondName,
		strings.Join(slaveNames, ","),
		strings.Join(options, ","))
}

// GenerateVLANCmdline generates kernel cmdline for VLAN configuration.
// Format: vlan=<vlandev>:<parent>
func GenerateVLANCmdline(info *NetworkInfo, vlan *LinkInfo, vlanName string) string {
	if vlan == nil || !vlan.IsVLAN() || vlan.VLAN == nil {
		return ""
	}

	// Get parent interface
	parent := info.GetLinkByIndex(vlan.LinkIndex)
	if parent == nil {
		return ""
	}

	parentName := PrettyName(parent.Name)
	return fmt.Sprintf("vlan=%s:%s", vlanName, parentName)
}

// GenerateIPCmdline generates kernel cmdline for IP configuration.
// Format: ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>
func GenerateIPCmdline(ip, gateway, netmask, hostname, device string) string {
	// Format: ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>
	// server-ip is empty, autoconf is "none" for static
	return fmt.Sprintf("ip=%s::%s:%s:%s:%s:none", ip, gateway, netmask, hostname, device)
}

// DefaultRoute returns the default route interface and gateway.
func DefaultRoute() (iface, gw string, err error) {
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
	if err = sc.Err(); err != nil {
		err = errors.Wrap(err, "read /proc/net/route")
		return
	}
	err = errors.New("no default route")
	return
}

// IfaceAddr returns the IPv4 address and netmask of the named interface.
func IfaceAddr(name string) (ip, mask string, err error) {
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
	err = errors.Newf("IPv4 not found on %s", name)
	return
}

// getPermanentMAC returns the permanent (hardware) MAC address of the interface.
//
// Primary source is /sys/class/net/<iface>/perm_addr, which contains the original
// hardware MAC that does not change when the active MAC is rewritten (most
// notably by Linux bonding, which replaces slave MACs with the master MAC).
//
// Some drivers/kernels do not expose perm_addr in sysfs, or expose it as the
// all-zero address. In both cases we fall back to the ethtool ETHTOOL_GPERMADDR
// ioctl, which is the canonical netlink-independent way to obtain the burned-in
// hardware address. Without this fallback, every slave of an active bond is
// reported with the bond master's MAC, which collapses the predictable
// enx<mac> name space and breaks the kernel bond= cmdline (both slaves end up
// with identical names).
func getPermanentMAC(name string) (net.HardwareAddr, error) {
	if mac, err := readSysfsPermAddr(name); err == nil && !isZeroMAC(mac) {
		return mac, nil
	}

	return ethtoolPermAddr(name)
}

// readSysfsPermAddr reads /sys/class/net/<name>/perm_addr. Returns the parsed
// MAC, which may be the all-zero address when the driver does not populate it.
func readSysfsPermAddr(name string) (net.HardwareAddr, error) {
	path := fmt.Sprintf("/sys/class/net/%s/perm_addr", name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return net.ParseMAC(strings.TrimSpace(string(data)))
}

// isZeroMAC reports whether the address is empty or all zero bytes.
func isZeroMAC(mac net.HardwareAddr) bool {
	if len(mac) == 0 {
		return true
	}
	for _, b := range mac {
		if b != 0 {
			return false
		}
	}
	return true
}

// ethtoolIfreq mirrors struct ifreq with the ifr_data union pointer used by
// SIOCETHTOOL. The Name field is null-terminated and bounded by IFNAMSIZ.
type ethtoolIfreq struct {
	Name [unix.IFNAMSIZ]byte
	Data unsafe.Pointer
}

// ethtoolPermAddrCmd mirrors struct ethtool_perm_addr from <linux/ethtool.h>.
// MaxAddrLen (32) is the kernel-side MAX_ADDR_LEN upper bound; for Ethernet
// interfaces Size comes back as 6.
type ethtoolPermAddrCmd struct {
	Cmd  uint32
	Size uint32
	Data [32]byte
}

// ethtoolPermAddr issues ETHTOOL_GPERMADDR via SIOCETHTOOL to fetch the
// burned-in hardware address of the interface, bypassing any active MAC
// override (e.g. one imposed by bonding).
func ethtoolPermAddr(name string) (net.HardwareAddr, error) {
	if len(name) >= unix.IFNAMSIZ {
		return nil, errors.Newf("interface name %q exceeds IFNAMSIZ", name)
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, errors.Wrap(err, "ethtool: open socket")
	}
	defer unix.Close(fd)

	cmd := ethtoolPermAddrCmd{
		Cmd:  unix.ETHTOOL_GPERMADDR,
		Size: uint32(len(ethtoolPermAddrCmd{}.Data)),
	}
	ifr := ethtoolIfreq{Data: unsafe.Pointer(&cmd)}
	copy(ifr.Name[:], name)

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCETHTOOL, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		return nil, errors.Wrapf(errno, "ethtool: SIOCETHTOOL ETHTOOL_GPERMADDR on %s", name)
	}

	if cmd.Size == 0 || cmd.Size > uint32(len(cmd.Data)) {
		return nil, errors.Newf("ethtool: implausible permanent address size %d on %s", cmd.Size, name)
	}
	mac := make(net.HardwareAddr, cmd.Size)
	copy(mac, cmd.Data[:cmd.Size])
	if isZeroMAC(mac) {
		return nil, errors.Newf("ethtool: permanent address on %s is all zero", name)
	}
	return mac, nil
}

// macToInterfaceName converts a MAC address to a predictable interface name.
// Format: enx<mac_without_colons_lowercase>
// Example: e8:eb:d3:7e:38:3a -> enxe8ebd37e383a
func macToInterfaceName(mac net.HardwareAddr) string {
	// Convert MAC to hex string without colons
	macHex := strings.ReplaceAll(mac.String(), ":", "")
	return "enx" + strings.ToLower(macHex)
}

// PrettyName returns the predictable network interface name based on permanent MAC address.
// Format: enx<mac> where mac is the permanent hardware MAC address without colons.
// This ensures the interface name remains consistent across reboots even if
// the user has modified the active MAC address.
func PrettyName(name string) string {
	// Try to get permanent MAC address first
	mac, err := getPermanentMAC(name)
	if err == nil && len(mac) > 0 {
		return macToInterfaceName(mac)
	}

	// Fallback: try to get current MAC from interface
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return name
	}
	if len(ifc.HardwareAddr) > 0 {
		return macToInterfaceName(ifc.HardwareAddr)
	}

	return name
}

func hexIPLittle(h string) string {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 4 {
		log.Printf("warning: invalid hex IP %q: err=%v len=%d", h, err, len(b))
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", b[3], b[2], b[1], b[0])
}

// GetHostname returns the current system hostname.
func GetHostname() string {
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

// Injected helpers — package-level vars so tests can swap real OS-backed
// lookups for stubs without standing up a netlink runtime. fatalf wraps
// log.Fatalf so tests can intercept process-terminating failures (e.g. the
// fail-loud guard on -yes + override + missing-IPv4).
//
//nolint:gochecknoglobals
var (
	defaultRouteFn       = DefaultRoute
	ifaceAddrFn          = IfaceAddr
	prettyNameFn         = PrettyName
	collectNetworkInfoFn = CollectNetworkInfo
	fatalf               = log.Fatalf
)

// CollectKernelArgs collects networking-related kernel cmdline arguments.
// overrideIface, when non-empty, replaces the auto-detected default-route
// device — the escape hatch for VLAN / bond topologies where the netlink
// or /proc/net/route probe does not converge on the right child interface.
// Both detection paths honour the override.
func CollectKernelArgs(overrideIface string) []string {
	// Try netlink-based detection first (supports bond/bridge)
	if args := collectKernelArgsNetlink(overrideIface); args != nil {
		return args
	}

	// Fallback to simple detection
	return collectKernelArgsSimple(overrideIface)
}

// pickInterface returns the network device to use, plus a bool indicating
// whether the override path was taken. Extracted so the override-vs-detect
// decision is unit-testable without a netlink runtime.
func pickInterface(override, detected string) (dev string, fromOverride bool) {
	if override != "" {
		return override, true
	}
	return detected, false
}

// vlanParentName resolves the cmdline name of a VLAN's parent link. When
// fromOverride is true, raw link names are used (skipping the perm_addr-based
// PrettyName rewrite) so a user override reaches the kernel cmdline verbatim.
// When the parent is a bond that matches the active actualDevice, the
// caller-supplied bondName is preferred so the cmdline references whatever
// the bond block named the master.
func vlanParentName(parent, actualDevice *LinkInfo, bondName string, fromOverride bool) string {
	if parent == nil {
		return "unknown"
	}
	if parent.IsBond() && actualDevice != nil && actualDevice.IsBond() {
		return bondName
	}
	if fromOverride {
		return parent.Name
	}
	return prettyNameFn(parent.Name)
}

//nolint:gocognit,forbidigo,funlen
func collectKernelArgsNetlink(overrideIface string) []string {
	// Try to collect network info via netlink
	netInfo, err := collectNetworkInfoFn()
	if err != nil {
		log.Printf("warning: failed to collect network info via netlink: %v", err)
		log.Printf("falling back to simple detection")
		return nil // Will use fallback
	}

	// Find default route interface, honouring the override when set.
	detectedDev, gw, drErr := defaultRouteFn()
	dev, fromOverride := pickInterface(overrideIface, detectedDev)
	if !fromOverride && drErr != nil {
		log.Printf("warning: no default route found: %v", drErr)
		return nil
	}
	if fromOverride {
		log.Printf("using override interface %s (auto-detected was %q, default route err=%v)",
			dev, detectedDev, drErr)
		if drErr != nil {
			log.Printf("warning: no default route — gateway will be empty unless answered interactively")
		}
	}

	// Get link info for the interface
	link := netInfo.GetLinkByName(dev)
	if link == nil {
		log.Printf("warning: interface %s not found in netlink", dev)
		return nil
	}

	// Get VLAN chain if any (from topmost to lowest)
	vlans := netInfo.GetVLANChain(link)

	// Resolve to actual device (handle bridge/vlan -> bond/physical)
	actualDevice := ResolveNetworkDevice(netInfo, link)
	if actualDevice == nil {
		actualDevice = link
	}

	// Get IP address and mask
	ip, mask, err := ifaceAddrFn(dev)
	if err != nil {
		if fromOverride {
			log.Printf("warning: failed to read IPv4 address for override interface %q: %v "+
				"— falling back to simple detection", dev, err)
		} else {
			log.Printf("warning: failed to get IP address for %s: %v", dev, err)
		}
		return nil
	}

	// Ask user if they want networking
	netOn := cli.AskYesNo("Add networking configuration?", true)
	if !netOn {
		return nil
	}

	var out []string

	// Determine the final device name for IP configuration
	// If there's a VLAN, the IP goes on the VLAN interface
	// If there's a bond, the IP goes on the bond (or VLAN on bond)
	var ipDevice string
	// Default bond master name in the kernel cmdline. When fromOverride is
	// true and the override resolves to a bond master, replace this with
	// the override so the bond=/ip= lines stay internally consistent
	// (otherwise the kernel would create "bond0" from the bond= line while
	// the ip= line points at a non-existent override name).
	bondName := "bond0"
	if fromOverride && actualDevice.IsBond() {
		bondName = actualDevice.Name
	}

	// Handle bond
	if actualDevice.IsBond() {
		fmt.Printf("\nDetected bond interface: %s\n", actualDevice.Name)
		slaves := netInfo.GetBondSlaves(actualDevice.Index)
		if len(slaves) > 0 {
			fmt.Printf("  Slaves: ")
			for i, s := range slaves {
				if i > 0 {
					fmt.Printf(", ")
				}
				fmt.Printf("%s (%s)", s.Name, prettyNameFn(s.Name))
			}
			fmt.Println()
		}
		if actualDevice.BondMaster != nil {
			fmt.Printf("  Mode: %s\n", BondModeToString(actualDevice.BondMaster.Mode))
			if actualDevice.BondMaster.Mode == BondMode8023AD {
				fmt.Printf("  Hash policy: %s\n", HashPolicyToString(actualDevice.BondMaster.HashPolicy))
				fmt.Printf("  LACP rate: %s\n", LACPRateToString(actualDevice.BondMaster.LACPRate))
			}
		}

		// Generate bond cmdline
		bondCmdline := GenerateBondCmdline(netInfo, actualDevice, bondName, fromOverride)
		if bondCmdline != "" {
			out = append(out, bondCmdline)
		}
		if fromOverride {
			ipDevice = overrideIface
			fmt.Printf("Bond IP device: %s (override active, bond master = %s)\n", ipDevice, bondName)
		} else {
			ipDevice = bondName
		}
	} else {
		// Regular interface. Skip the perm_addr-based rewrite for an
		// explicit override so the user-supplied name (often a VLAN child)
		// reaches the kernel cmdline verbatim.
		if fromOverride {
			ipDevice = overrideIface
			fmt.Printf("\nDetected interface: %s (override active, using %s verbatim)\n",
				actualDevice.Name, overrideIface)
		} else {
			ipDevice = prettyNameFn(actualDevice.Name)
			fmt.Printf("\nDetected interface: %s (%s)\n", actualDevice.Name, ipDevice)
		}
	}

	// Handle VLANs
	if len(vlans) > 0 {
		fmt.Printf("\nDetected VLAN configuration:\n")
		for _, vlan := range vlans {
			if vlan.VLAN != nil {
				parent := netInfo.GetLinkByIndex(vlan.LinkIndex)
				// Display name matches what the cmdline will carry — route
				// through the same vlanParentName helper used below so the
				// human-readable preamble does not diverge from the emitted
				// vlan=/ip= lines under override.
				parentName := vlanParentName(parent, actualDevice, bondName, fromOverride)
				fmt.Printf("  VLAN %d on %s (interface: %s)\n", vlan.VLAN.VID, parentName, vlan.Name)
			}
		}
		fmt.Println()

		// Generate VLAN cmdlines (in reverse order - from lowest to topmost)
		// This ensures parent interfaces are created before child VLANs
		for i, vlan := range slices.Backward(vlans) {
			if vlan.VLAN == nil {
				continue
			}

			// Determine VLAN device name for Talos.
			// When fromOverride, names go through verbatim (no PrettyName
			// rewrite) so the user-supplied leaf reaches the kernel cmdline
			// unchanged. Format: <parent>.<vid>.
			parent := netInfo.GetLinkByIndex(vlan.LinkIndex)
			parentName := vlanParentName(parent, actualDevice, bondName, fromOverride)

			vlanName := fmt.Sprintf("%s.%d", parentName, vlan.VLAN.VID)
			// The topmost VLAN (i == 0 after Backward) is the leaf — if an
			// override is active, honour the user-supplied name verbatim
			// rather than the derived <parent>.<vid> form. For the common
			// case where the user passes "eth0.10" the two forms agree, but
			// this branch covers the rarer case where the runtime link name
			// does not follow the dotted convention (e.g. ifrename'd VLAN
			// interfaces) and the user is telling us the exact kernel name
			// they expect on the cmdline.
			if i == 0 && fromOverride {
				vlanName = overrideIface
			}
			vlanCmdline := fmt.Sprintf("vlan=%s:%s", vlanName, parentName)
			out = append(out, vlanCmdline)

			// The topmost VLAN is where we put the IP
			if i == 0 {
				ipDevice = vlanName
			}
		}
	}

	// Ask for IP configuration
	ipDevice = cli.Ask("Network device for IP", ipDevice)
	ip = cli.Ask("IP address", ip)
	mask = cli.Ask("Netmask", mask)
	gw = cli.Ask("Gateway (or 'none')", gw)
	if strings.EqualFold(gw, "none") {
		gw = ""
	}
	hostname := cli.Ask("Hostname", GetHostname())

	// Generate IP cmdline
	ipCmdline := GenerateIPCmdline(ip, gw, mask, hostname, ipDevice)
	out = append(out, ipCmdline)

	// Serial console
	console := cli.Ask("Configure serial console? (or 'no')", "ttyS0")
	if console == "" {
		console = "ttyS0"
	}
	if !strings.EqualFold(console, "no") && !strings.EqualFold(console, "none") {
		out = append(out, "console="+console)
	}

	return out
}

func collectKernelArgsSimple(overrideIface string) []string {
	detectedDev, gw, drErr := defaultRouteFn()
	dev, fromOverride := pickInterface(overrideIface, detectedDev)
	if fromOverride {
		log.Printf("using override interface %s (auto-detected was %q)", dev, detectedDev)
		if drErr != nil {
			log.Printf("warning: no default route — gateway will be empty unless answered interactively")
		}
	}
	ip, mask, ifErr := ifaceAddrFn(dev)
	if fromOverride && ifErr != nil {
		if cli.YesFlag {
			// Non-interactive caller asked for an override interface that
			// has no IPv4 address; emitting an ip= line with empty fields
			// would silently produce a broken Talos install. Fail loudly
			// so the operator sees the problem now, not after a reboot
			// into a node without networking.
			fatalf("override interface %q has no IPv4 address: %v "+
				"(refusing to emit a broken ip= line under -yes)", dev, ifErr)
		}
		log.Printf("warning: failed to read IPv4 address for override interface %q: %v "+
			"— ip/mask will be empty unless answered interactively", dev, ifErr)
	}
	// Skip the perm_addr-based rewrite for an explicit override: the user
	// passed a specific interface name (often a VLAN child like eth0.10
	// that inherits its parent's MAC) and the kernel cmdline must carry
	// that exact name.
	if !fromOverride {
		dev = prettyNameFn(dev)
	}
	hostname := GetHostname()

	netOn := cli.AskYesNo("Add networking configuration?", true)
	var out []string
	if netOn {
		dev = cli.Ask("Interface", dev)
		ip = cli.Ask("IP address", ip)
		mask = cli.Ask("Netmask", mask)
		gw = cli.Ask("Gateway (or 'none')", gw)
		if strings.EqualFold(gw, "none") {
			gw = ""
		}
		hostname = cli.Ask("Hostname", hostname)
		out = append(out, fmt.Sprintf("ip=%s::%s:%s:%s:%s:none", ip, gw, mask, hostname, dev))
	}

	console := cli.Ask("Configure serial console? (or 'no')", "ttyS0")
	if console == "" {
		console = "ttyS0"
	}
	if !strings.EqualFold(console, "no") && !strings.EqualFold(console, "none") {
		out = append(out, "console="+console)
	}
	return out
}
