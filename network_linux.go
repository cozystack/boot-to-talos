//go:build linux

package main

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/jsimonetti/rtnetlink/v2"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// getHostname returns the current system hostname
func getHostname() string {
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

// Bond mode constants
const (
	BondModeBalanceRR    uint8 = 0 // balance-rr
	BondModeActiveBackup uint8 = 1 // active-backup
	BondModeBalanceXOR   uint8 = 2 // balance-xor
	BondModeBroadcast    uint8 = 3 // broadcast
	BondMode8023AD       uint8 = 4 // 802.3ad (LACP)
	BondModeBalanceTLB   uint8 = 5 // balance-tlb
	BondModeBalanceALB   uint8 = 6 // balance-alb
)

// LACP rate constants
const (
	LACPRateSlow uint8 = 0
	LACPRateFast uint8 = 1
)

// Hash policy constants
const (
	BondXmitHashPolicyLayer2     uint8 = 0
	BondXmitHashPolicyLayer34    uint8 = 1
	BondXmitHashPolicyLayer23    uint8 = 2
	BondXmitHashPolicyEncap23    uint8 = 3
	BondXmitHashPolicyEncap34    uint8 = 4
	BondXmitHashPolicyVlanSrcMAC uint8 = 5
)

// LinkInfo represents network interface information
type LinkInfo struct {
	Name             string
	Index            uint32
	Type             uint16
	Flags            uint32
	HardwareAddr     net.HardwareAddr
	MTU              uint32
	MasterIndex      uint32
	OperationalState rtnetlink.OperationalState
	Kind             string
	SlaveKind        string
	BondMaster       *BondMasterSpec
}

// BondMasterSpec represents bond master configuration
type BondMasterSpec struct {
	Mode            uint8
	HashPolicy      uint8
	LACPRate        uint8
	MIIMon          uint32
	UpDelay         uint32
	DownDelay       uint32
	ARPInterval     uint32
	ARPIPTargets    []netip.Addr
	PrimaryIndex    *uint32
	UseCarrier      bool
}

// NetworkInfo contains all collected network information
type NetworkInfo struct {
	Links     []LinkInfo
	linkIndex map[uint32]*LinkInfo
	linkName  map[string]*LinkInfo
}

// IsBond returns true if the link is a bond interface
func (l *LinkInfo) IsBond() bool {
	return l.Kind == "bond"
}

// IsBondSlave returns true if the link is a bond slave
func (l *LinkInfo) IsBondSlave() bool {
	return l.SlaveKind == "bond"
}

// IsBridge returns true if the link is a bridge interface
func (l *LinkInfo) IsBridge() bool {
	return l.Kind == "bridge"
}

// IsBridgeSlave returns true if the link is a bridge port
func (l *LinkInfo) IsBridgeSlave() bool {
	return l.SlaveKind == "bridge"
}

// IsPhysical returns true if the link is a physical ethernet interface
func (l *LinkInfo) IsPhysical() bool {
	return l.Kind == "" && l.Type == 1 && !l.IsBondSlave() && !l.IsBridgeSlave()
}

// GetLinkByIndex returns link by index
func (n *NetworkInfo) GetLinkByIndex(index uint32) *LinkInfo {
	return n.linkIndex[index]
}

// GetLinkByName returns link by name
func (n *NetworkInfo) GetLinkByName(name string) *LinkInfo {
	return n.linkName[name]
}

// GetBondSlaves returns slave interfaces for a bond
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

// GetBridgePorts returns port interfaces for a bridge
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

// collectNetworkInfo gathers all network interface information via netlink
func collectNetworkInfo() (*NetworkInfo, error) {
	conn, err := rtnetlink.Dial(nil)
	if err != nil {
		return nil, fmt.Errorf("error dialing rtnetlink socket: %w", err)
	}
	defer conn.Close()

	links, err := conn.Link.List()
	if err != nil {
		return nil, fmt.Errorf("error listing links: %w", err)
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

		if link.Attributes.Master != nil {
			li.MasterIndex = *link.Attributes.Master
		}

		if link.Attributes.Info != nil {
			li.Kind = link.Attributes.Info.Kind
			li.SlaveKind = link.Attributes.Info.SlaveKind

			if li.Kind == "bond" && link.Attributes.Info.Data != nil {
				if linkData, ok := link.Attributes.Info.Data.(*rtnetlink.LinkData); ok {
					bondSpec, err := decodeBondMasterSpec(linkData.Data)
					if err != nil {
						log.Printf("warning: failed to decode bond master spec for %s: %v", link.Attributes.Name, err)
					} else {
						li.BondMaster = bondSpec
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

// resolveNetworkDevice finds the actual device to use for network configuration
// If the device is a bridge, it finds the underlying physical interface or bond
// If the device is a bond, it returns the bond itself
func resolveNetworkDevice(info *NetworkInfo, link *LinkInfo) *LinkInfo {
	if link == nil {
		return nil
	}

	// If it's a bridge, find the underlying device
	if link.IsBridge() {
		ports := info.GetBridgePorts(link.Index)
		for _, port := range ports {
			// Prefer bond over physical interface
			if port.IsBond() {
				return port
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

// bondModeToString converts bond mode constant to kernel string
func bondModeToString(mode uint8) string {
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

// hashPolicyToString converts hash policy constant to kernel string
func hashPolicyToString(policy uint8) string {
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

// lacpRateToString converts LACP rate to kernel string
func lacpRateToString(rate uint8) string {
	if rate == LACPRateFast {
		return "fast"
	}
	return "slow"
}

// generateBondCmdline generates kernel cmdline for bond configuration
// Format: bond=<bondname>:<slaves>:<options>
// Example: bond=bond0:eth0,eth1:mode=802.3ad,xmit_hash_policy=layer3+4,miimon=100
func generateBondCmdline(info *NetworkInfo, bond *LinkInfo, bondName string) string {
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
		slaveNames = append(slaveNames, prettyName(slave.Name))
	}

	// Build options
	var options []string

	// Mode
	options = append(options, fmt.Sprintf("mode=%s", bondModeToString(bond.BondMaster.Mode)))

	// Hash policy (for modes that use it)
	if bond.BondMaster.Mode == BondMode8023AD ||
		bond.BondMaster.Mode == BondModeBalanceXOR ||
		bond.BondMaster.Mode == BondModeBalanceTLB ||
		bond.BondMaster.Mode == BondModeBalanceALB {
		options = append(options, fmt.Sprintf("xmit_hash_policy=%s", hashPolicyToString(bond.BondMaster.HashPolicy)))
	}

	// LACP rate (only for 802.3ad)
	if bond.BondMaster.Mode == BondMode8023AD {
		options = append(options, fmt.Sprintf("lacp_rate=%s", lacpRateToString(bond.BondMaster.LACPRate)))
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

// generateIPCmdline generates kernel cmdline for IP configuration
// Format: ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>
// Example: ip=10.200.16.12::10.200.16.1:255.255.240.0:hostname:bond0:none
func generateIPCmdline(ip, gateway, netmask, hostname, device string) string {
	// Format: ip=<client-ip>:<server-ip>:<gw-ip>:<netmask>:<hostname>:<device>:<autoconf>
	// server-ip is empty, autoconf is "none" for static
	return fmt.Sprintf("ip=%s::%s:%s:%s:%s:none", ip, gateway, netmask, hostname, device)
}

// collectKernelArgsNetlink collects kernel arguments using netlink for better bond/bridge detection
func collectKernelArgsNetlink() []string {
	// Try to collect network info via netlink
	netInfo, err := collectNetworkInfo()
	if err != nil {
		log.Printf("warning: failed to collect network info via netlink: %v", err)
		log.Printf("falling back to simple detection")
		return nil // Will use fallback
	}

	// Find default route interface
	dev, gw, err := defaultRoute()
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

	// Resolve to actual device (handle bridge -> bond/physical)
	actualDevice := resolveNetworkDevice(netInfo, link)
	if actualDevice == nil {
		actualDevice = link
	}

	// Get IP address and mask
	ip, mask, err := ifaceAddr(dev)
	if err != nil {
		log.Printf("warning: failed to get IP address for %s: %v", dev, err)
		return nil
	}

	// Ask user if they want networking
	netOn := askYesNo("Add networking configuration?", true)
	if !netOn {
		return nil
	}

	var out []string

	// Determine if we're dealing with a bond
	if actualDevice.IsBond() {
		fmt.Printf("\nDetected bond interface: %s\n", actualDevice.Name)
		slaves := netInfo.GetBondSlaves(actualDevice.Index)
		if len(slaves) > 0 {
			fmt.Printf("  Slaves: ")
			for i, s := range slaves {
				if i > 0 {
					fmt.Printf(", ")
				}
				fmt.Printf("%s (%s)", s.Name, prettyName(s.Name))
			}
			fmt.Println()
		}
		if actualDevice.BondMaster != nil {
			fmt.Printf("  Mode: %s\n", bondModeToString(actualDevice.BondMaster.Mode))
			if actualDevice.BondMaster.Mode == BondMode8023AD {
				fmt.Printf("  Hash policy: %s\n", hashPolicyToString(actualDevice.BondMaster.HashPolicy))
				fmt.Printf("  LACP rate: %s\n", lacpRateToString(actualDevice.BondMaster.LACPRate))
			}
		}
		fmt.Println()

		// For Talos, we use standard bond0 name
		bondName := "bond0"

		// Generate bond cmdline
		bondCmdline := generateBondCmdline(netInfo, actualDevice, bondName)
		if bondCmdline != "" {
			out = append(out, bondCmdline)
		}

		// Ask for IP configuration
		ip = ask("IP address", ip)
		mask = ask("Netmask", mask)
		gw = ask("Gateway (or 'none')", gw)
		if strings.EqualFold(gw, "none") {
			gw = ""
		}
		hostname := ask("Hostname", getHostname())

		// Generate IP cmdline with bond0 as device
		ipCmdline := generateIPCmdline(ip, gw, mask, hostname, bondName)
		out = append(out, ipCmdline)
	} else {
		// Regular interface
		devPretty := prettyName(actualDevice.Name)
		fmt.Printf("\nDetected interface: %s (%s)\n\n", actualDevice.Name, devPretty)

		devPretty = ask("Interface", devPretty)
		ip = ask("IP address", ip)
		mask = ask("Netmask", mask)
		gw = ask("Gateway (or 'none')", gw)
		if strings.EqualFold(gw, "none") {
			gw = ""
		}
		hostname := ask("Hostname", getHostname())

		// Standard ip= format with hostname
		ipCmdline := generateIPCmdline(ip, gw, mask, hostname, devPretty)
		out = append(out, ipCmdline)
	}

	// Serial console
	console := ask("Configure serial console? (or 'no')", "ttyS0")
	if console == "" {
		console = "ttyS0"
	}
	if !strings.EqualFold(console, "no") && !strings.EqualFold(console, "none") {
		out = append(out, "console="+console)
	}

	return out
}
