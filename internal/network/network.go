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
	"strings"

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
func GenerateBondCmdline(info *NetworkInfo, bond *LinkInfo, bondName string) string {
	if bond == nil || !bond.IsBond() || bond.BondMaster == nil {
		return ""
	}

	// Get slave interfaces
	slaves := info.GetBondSlaves(bond.Index)
	if len(slaves) == 0 {
		return ""
	}

	// Build slave list using predictable names
	var slaveNames []string
	for _, slave := range slaves {
		slaveNames = append(slaveNames, PrettyName(slave.Name))
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

// PrettyName returns the predictable network interface name.
func PrettyName(name string) string {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return name
	}
	p := fmt.Sprintf("/run/udev/data/n%d", ifc.Index)

	data, err := os.ReadFile(p)
	if err != nil {
		return name
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.IndexByte(line, ':'); i >= 0 {
			line = line[i+1:]
		}
		switch {
		case strings.HasPrefix(line, "ID_NET_NAME_ONBOARD="):
			return strings.TrimPrefix(line, "ID_NET_NAME_ONBOARD=")
		case strings.HasPrefix(line, "ID_NET_NAME_PATH="):
			return strings.TrimPrefix(line, "ID_NET_NAME_PATH=")
		case strings.HasPrefix(line, "ID_NET_NAME_SLOT="):
			return strings.TrimPrefix(line, "ID_NET_NAME_SLOT=")
		}
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

// CollectKernelArgs collects kernel arguments for network configuration.
func CollectKernelArgs() []string {
	// Try netlink-based detection first (supports bond/bridge)
	if args := collectKernelArgsNetlink(); args != nil {
		return args
	}

	// Fallback to simple detection
	return collectKernelArgsSimple()
}

//nolint:gocognit,forbidigo,funlen
func collectKernelArgsNetlink() []string {
	// Try to collect network info via netlink
	netInfo, err := CollectNetworkInfo()
	if err != nil {
		log.Printf("warning: failed to collect network info via netlink: %v", err)
		log.Printf("falling back to simple detection")
		return nil // Will use fallback
	}

	// Find default route interface
	dev, gw, err := DefaultRoute()
	if err != nil {
		log.Printf("warning: no default route found: %v", err)
		return nil
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
	ip, mask, err := IfaceAddr(dev)
	if err != nil {
		log.Printf("warning: failed to get IP address for %s: %v", dev, err)
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
	bondName := "bond0"

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
				fmt.Printf("%s (%s)", s.Name, PrettyName(s.Name))
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
		bondCmdline := GenerateBondCmdline(netInfo, actualDevice, bondName)
		if bondCmdline != "" {
			out = append(out, bondCmdline)
		}
		ipDevice = bondName
	} else {
		// Regular interface
		ipDevice = PrettyName(actualDevice.Name)
		fmt.Printf("\nDetected interface: %s (%s)\n", actualDevice.Name, ipDevice)
	}

	// Handle VLANs
	if len(vlans) > 0 {
		fmt.Printf("\nDetected VLAN configuration:\n")
		for _, vlan := range vlans {
			if vlan.VLAN != nil {
				parent := netInfo.GetLinkByIndex(vlan.LinkIndex)
				parentName := "unknown"
				if parent != nil {
					if parent.IsBond() && actualDevice.IsBond() {
						parentName = bondName
					} else {
						parentName = PrettyName(parent.Name)
					}
				}
				fmt.Printf("  VLAN %d on %s (interface: %s)\n", vlan.VLAN.VID, parentName, vlan.Name)
			}
		}
		fmt.Println()

		// Generate VLAN cmdlines (in reverse order - from lowest to topmost)
		// This ensures parent interfaces are created before child VLANs
		for i := len(vlans) - 1; i >= 0; i-- {
			vlan := vlans[i]
			if vlan.VLAN == nil {
				continue
			}

			// Determine VLAN device name for Talos
			// Format: <parent>.<vid>
			parent := netInfo.GetLinkByIndex(vlan.LinkIndex)
			var parentName string
			if parent != nil {
				if parent.IsBond() && actualDevice.IsBond() {
					parentName = bondName
				} else if parent.IsVLAN() {
					// Nested VLAN - find the previous VLAN's name
					// For now, use predictable name
					parentName = PrettyName(parent.Name)
				} else {
					parentName = PrettyName(parent.Name)
				}
			}

			vlanName := fmt.Sprintf("%s.%d", parentName, vlan.VLAN.VID)
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

func collectKernelArgsSimple() []string {
	dev, gw, _ := DefaultRoute()
	ip, mask, _ := IfaceAddr(dev)
	dev = PrettyName(dev)
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
