package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxc/incus/v6/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/api"
)

// networkdConfigFile represents a given filename and its contents.
type networkdConfigFile struct {
	Name     string
	Contents string
}

// generateNetworkConfiguration clears any existing configuration from /run/systemd/network/ and generates
// new config files from the supplied NetworkConfig struct.
func generateNetworkConfiguration(_ context.Context, networkCfg *api.SystemNetworkConfig) error {
	// Remove any existing configuration.
	err := os.RemoveAll(SystemdNetworkConfigPath)
	if err != nil {
		return err
	}

	err = os.Mkdir(SystemdNetworkConfigPath, 0o755)
	if err != nil {
		return err
	}

	// Generate .link files.
	for _, cfg := range generateLinkFileContents(*networkCfg) {
		err := os.WriteFile(filepath.Join(SystemdNetworkConfigPath, cfg.Name), []byte(cfg.Contents), 0o644)
		if err != nil {
			return err
		}
	}

	// Generate .netdev files.
	for _, cfg := range generateNetdevFileContents(*networkCfg) {
		err := os.WriteFile(filepath.Join(SystemdNetworkConfigPath, cfg.Name), []byte(cfg.Contents), 0o644)
		if err != nil {
			return err
		}
	}

	// Generate .network files.
	for _, cfg := range generateNetworkFileContents(*networkCfg) {
		err := os.WriteFile(filepath.Join(SystemdNetworkConfigPath, cfg.Name), []byte(cfg.Contents), 0o644)
		if err != nil {
			return err
		}
	}

	// Generate systemd-timesyncd configuration if any timeservers are defined.
	ntpCfg := ""
	if networkCfg.NTP != nil {
		ntpCfg = generateTimesyncContents(*networkCfg.NTP)

		if ntpCfg != "" {
			err := os.WriteFile(SystemdTimesyncConfigFile, []byte(ntpCfg), 0o644)
			if err != nil {
				return err
			}
		}
	}

	// If there's no NTP configuration, remove the old config file that might exist.
	if networkCfg.NTP == nil || ntpCfg == "" {
		_ = os.Remove(SystemdTimesyncConfigFile)
	}

	return nil
}

// ApplyNetworkConfiguration instructs systemd-networkd to apply the supplied network configuration.
func ApplyNetworkConfiguration(ctx context.Context, networkCfg *api.SystemNetworkConfig, timeout time.Duration) error {
	if networkCfg == nil {
		return errors.New("no network configuration provided")
	}

	// Get hostname and domain from network config, if defined.
	hostname := ""
	if networkCfg.DNS != nil && networkCfg.DNS.Hostname != "" {
		hostname = networkCfg.DNS.Hostname
		if networkCfg.DNS.Domain != "" {
			hostname += "." + networkCfg.DNS.Domain
		}
	}

	// Apply the configured hostname, or reset back to default if not set.
	err := SetHostname(ctx, hostname)
	if err != nil {
		return err
	}

	// Set proxy environment variables, or clear existing ones if none are defined.
	err = UpdateEnvironment(networkCfg.Proxy)
	if err != nil {
		return err
	}

	err = generateNetworkConfiguration(ctx, networkCfg)
	if err != nil {
		return err
	}

	err = waitForUdevInterfaceRename(ctx, 5*time.Second)
	if err != nil {
		return err
	}

	// Restart networking after new config files have been generated.
	err = RestartUnit(ctx, "systemd-networkd")
	if err != nil {
		return err
	}

	// (Re)start NTP time synchronization. Since we might be overriding the default fallback NTP servers,
	// the service is disabled by default and only started once we have performed the network (re)configuration.
	err = RestartUnit(ctx, "systemd-timesyncd")
	if err != nil {
		return err
	}

	// Wait for the network to apply.
	return waitForNetworkOnline(ctx, networkCfg, timeout)
}

// waitForUdevInterfaceRename waits up to a provided timeout for udev to pickup and process
// the renaming of interfaces. At system startup there's a small race between udev being fully
// started and our reconfiguring of the network, so we poll in a loop until we see the kernel
// has been notified of the rename.
func waitForUdevInterfaceRename(ctx context.Context, timeout time.Duration) error {
	endTime := time.Now().Add(timeout)

	for {
		if time.Now().After(endTime) {
			return errors.New("timed out waiting for udev to rename interface(s)")
		}

		// Trigger udev rule update to pickup device names.
		_, err := subprocess.RunCommandContext(ctx, "udevadm", "trigger", "--action=add")
		if err != nil {
			return err
		}

		// Wait for udev to be done processing the events.
		_, err = subprocess.RunCommandContext(ctx, "udevadm", "settle")
		if err != nil {
			return err
		}

		// Check if the kernel has noticed the renaming of (at least) one interface to
		// the expected "en<MAC address>" format.
		_, err = subprocess.RunCommandContext(ctx, "journalctl", "-t", "kernel", "-g", "en[[:xdigit:]]{12}: renamed from ")
		if err == nil {
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// waitForNetworkOnline waits up to a provided timeout for configured network interfaces,
// bonds, and vlans to configure their IP address(es) and come online.
func waitForNetworkOnline(ctx context.Context, networkCfg *api.SystemNetworkConfig, timeout time.Duration) error {
	isOnline := func(name string) bool {
		output, err := subprocess.RunCommandContext(ctx, "networkctl", "status", name)
		if err != nil {
			return false
		}

		return strings.Contains(output, "Online state: online")
	}

	getNumberOfIPs := func(name string) int {
		ipAddressRegex := regexp.MustCompile(`inet6? (.+)/\d+ `)

		output, err := subprocess.RunCommandContext(ctx, "ip", "address", "show", name)
		if err != nil {
			return -1
		}

		numIPs := 0
		matches := ipAddressRegex.FindAllStringSubmatch(output, -1)

		for _, addr := range matches {
			// Don't count link-local address.
			if strings.HasPrefix(addr[1], "fe80:") {
				continue
			}

			numIPs++
		}

		return numIPs
	}

	endTime := time.Now().Add(timeout)

	devicesToCheck := make(map[string]int)

	for _, i := range networkCfg.Interfaces {
		if len(i.Addresses) == 0 {
			continue
		}

		devicesToCheck[i.Name] = len(i.Addresses)
	}

	for _, b := range networkCfg.Bonds {
		if len(b.Addresses) == 0 {
			continue
		}

		devicesToCheck[b.Name] = len(b.Addresses)
	}

	for _, v := range networkCfg.VLANs {
		if len(v.Addresses) == 0 {
			continue
		}

		devicesToCheck[v.Name] = len(v.Addresses)
	}

	for {
		if time.Now().After(endTime) {
			return errors.New("timed out waiting for network to come online")
		}

		allDevicesOnline := true
		for name, numIPs := range devicesToCheck {
			if !isOnline(name) || getNumberOfIPs(name) != numIPs {
				allDevicesOnline = false

				break
			}
		}

		if allDevicesOnline {
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// generateLinkFileContents generates the contents of systemd.link files. Returns an array of ConfigFile structs.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.link.html
func generateLinkFileContents(networkCfg api.SystemNetworkConfig) []networkdConfigFile {
	ret := []networkdConfigFile{}

	for _, i := range networkCfg.Interfaces {
		strippedHwaddr := strings.ToLower(strings.ReplaceAll(i.Hwaddr, ":", ""))
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("00-en%s.link", strippedHwaddr),
			Contents: fmt.Sprintf(`[Match]
PermanentMACAddress=%s

[Link]
NamePolicy=
Name=en%s
`, i.Hwaddr, strippedHwaddr),
		})
	}

	for _, b := range networkCfg.Bonds {
		for _, member := range b.Members {
			strippedHwaddr := strings.ToLower(strings.ReplaceAll(member, ":", ""))
			ret = append(ret, networkdConfigFile{
				Name: fmt.Sprintf("01-en%s.link", strippedHwaddr),
				Contents: fmt.Sprintf(`[Match]
PermanentMACAddress=%s

[Link]
NamePolicy=
Name=en%s
`, member, strippedHwaddr),
			})
		}
	}

	return ret
}

// generateNetdevFileContents generates the contents of systemd.netdev files. Returns an array of networkdConfigFile structs.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.netdev.html
func generateNetdevFileContents(networkCfg api.SystemNetworkConfig) []networkdConfigFile {
	ret := []networkdConfigFile{}

	// Create a bridge device for each interface.
	for _, i := range networkCfg.Interfaces {
		strippedHwaddr := strings.ToLower(strings.ReplaceAll(i.Hwaddr, ":", ""))
		mtuString := ""
		if i.MTU != 0 {
			mtuString = fmt.Sprintf("MTUBytes=%d", i.MTU)
		}
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("10-br%s.netdev", strippedHwaddr),
			Contents: fmt.Sprintf(`[NetDev]
Name=%s
Kind=bridge
MACAddress=%s
%s

[Bridge]
VLANFiltering=true
`, i.Name, i.Hwaddr, mtuString),
		})
	}

	// Create bond and bridge devices for each bond.
	for _, b := range networkCfg.Bonds {
		bondMacAddr := b.Hwaddr
		if bondMacAddr == "" {
			bondMacAddr = b.Members[0]
		}

		strippedHwaddr := strings.ToLower(strings.ReplaceAll(bondMacAddr, ":", ""))

		mtuString := ""
		if b.MTU != 0 {
			mtuString = fmt.Sprintf("MTUBytes=%d", b.MTU)
		}

		// Bond.
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("11-bn%s.netdev", strippedHwaddr),
			Contents: fmt.Sprintf(`[NetDev]
Name=bn%s
Kind=bond
MACAddress=%s
%s

[Bond]
Mode=%s
`, strippedHwaddr, bondMacAddr, mtuString, b.Mode),
		})

		// Bridge.
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("11-br%s.netdev", strippedHwaddr),
			Contents: fmt.Sprintf(`[NetDev]
Name=%s
Kind=bridge
MACAddress=%s
%s

[Bridge]
VLANFiltering=true
`, b.Name, bondMacAddr, mtuString),
		})
	}

	// Create vlans.
	for _, v := range networkCfg.VLANs {
		parentMACAddress := ""
		for _, i := range networkCfg.Interfaces {
			if i.Name == v.Parent {
				parentMACAddress = i.Hwaddr

				break
			}
		}
		if parentMACAddress == "" {
			for _, b := range networkCfg.Bonds {
				if b.Name == v.Parent {
					if b.Hwaddr != "" {
						parentMACAddress = b.Hwaddr
					} else {
						parentMACAddress = b.Members[0]
					}

					break
				}
			}
		}

		mtuString := ""
		if v.MTU != 0 {
			mtuString = fmt.Sprintf("MTUBytes=%d", v.MTU)
		}

		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("12-%s.netdev", v.Name),
			Contents: fmt.Sprintf(`[NetDev]
Name=%s
Kind=veth
MACAddress=%s
%s

[Peer]
Name=vl%s
`, v.Name, parentMACAddress, mtuString, v.Name),
		})
	}

	return ret
}

// generateNetworkFileContents generates the contents of systemd.network files. Returns an array of networkdConfigFile structs.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.network.html
func generateNetworkFileContents(networkCfg api.SystemNetworkConfig) []networkdConfigFile {
	ret := []networkdConfigFile{}

	// Create networks for each interface.
	for _, i := range networkCfg.Interfaces {
		strippedHwaddr := strings.ToLower(strings.ReplaceAll(i.Hwaddr, ":", ""))
		cfgString := fmt.Sprintf(`[Match]
Name=%s

[Link]
%s

[DHCP]
ClientIdentifier=mac
RouteMetric=100
UseMTU=true

[Network]
%s`, i.Name, generateLinkSectionContents(i.Addresses), generateNetworkSectionContents(networkCfg.DNS, networkCfg.NTP))

		cfgString += processAddresses(i.Addresses)

		if len(i.Routes) > 0 {
			cfgString += processRoutes(i.Routes)
		}

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("20-%s.network", i.Name),
			Contents: cfgString,
		})

		cfgString = fmt.Sprintf(`[Match]
Name=en%s

[Network]
Bridge=%s
LLDP=%s
EmitLLDP=%s
`, strippedHwaddr, i.Name, strconv.FormatBool(i.LLDP), strconv.FormatBool(i.LLDP))

		cfgString += generateBridgeVLANContents(i.Name, i.VLAN, i.VLANTags, networkCfg.VLANs)

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("20-en%s.network", strippedHwaddr),
			Contents: cfgString,
		})
	}

	// Create networks for each bond and its member(s).
	for _, b := range networkCfg.Bonds {
		bondMacAddr := b.Hwaddr
		if bondMacAddr == "" {
			bondMacAddr = b.Members[0]
		}

		strippedHwaddr := strings.ToLower(strings.ReplaceAll(bondMacAddr, ":", ""))

		// Bond.
		cfgString := fmt.Sprintf(`[Match]
Name=%s

[Link]
%s

[DHCP]
ClientIdentifier=mac
RouteMetric=100
UseMTU=true

[Network]
%s`, b.Name, generateLinkSectionContents(b.Addresses), generateNetworkSectionContents(networkCfg.DNS, networkCfg.NTP))

		cfgString += processAddresses(b.Addresses)

		if len(b.Routes) > 0 {
			cfgString += processRoutes(b.Routes)
		}

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("21-%s.network", b.Name),
			Contents: cfgString,
		})

		// Bridge.
		cfgString = fmt.Sprintf(`[Match]
Name=bn%s

[Network]
Bridge=%s
`, strippedHwaddr, b.Name)

		cfgString += generateBridgeVLANContents(b.Name, b.VLAN, b.VLANTags, networkCfg.VLANs)

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("21-bn%s.network", strippedHwaddr),
			Contents: cfgString,
		})

		// Bond members.
		for index, member := range b.Members {
			memberStrippedHwaddr := strings.ToLower(strings.ReplaceAll(member, ":", ""))

			ret = append(ret, networkdConfigFile{
				Name: fmt.Sprintf("21-bn%s-dev%d.network", strippedHwaddr, index),
				Contents: fmt.Sprintf(`[Match]
Name=en%s

[Network]
Bond=bn%s
LLDP=%s
EmitLLDP=%s
`, memberStrippedHwaddr, strippedHwaddr, strconv.FormatBool(b.LLDP), strconv.FormatBool(b.LLDP)),
			})
		}
	}

	// Create networks for each VLAN.
	for _, v := range networkCfg.VLANs {
		cfgString := fmt.Sprintf(`[Match]
Name=vl%s

[Network]
Bridge=%s

[BridgeVLAN]
VLAN=%d
PVID=%d
EgressUntagged=%d
`, v.Name, v.Parent, v.ID, v.ID, v.ID)

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("22-vl%s.network", v.Name),
			Contents: cfgString,
		})

		cfgString = fmt.Sprintf(`[Match]
Name=%s

[Link]
%s

[DHCP]
ClientIdentifier=mac
RouteMetric=100
UseMTU=true

[Network]
%s`, v.Name, generateLinkSectionContents(v.Addresses), generateNetworkSectionContents(networkCfg.DNS, networkCfg.NTP))

		cfgString += processAddresses(v.Addresses)

		if len(v.Routes) > 0 {
			cfgString += processRoutes(v.Routes)
		}

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("22-%s.network", v.Name),
			Contents: cfgString,
		})
	}

	return ret
}

func processAddresses(addresses []string) string {
	ret := ""
	if len(addresses) != 0 {
		ret += "LinkLocalAddressing=ipv6\n"
	} else {
		ret += "LinkLocalAddressing=no\n"
		ret += "ConfigureWithoutCarrier=yes\n"
	}

	hasDHCP4 := false
	hasDHCP6 := false
	acceptIPv6RA := false
	for _, addr := range addresses {
		switch addr {
		case "dhcp4": //nolint:goconst
			hasDHCP4 = true
		case "dhcp6":
			hasDHCP6 = true
		case "slaac": //nolint:goconst
			acceptIPv6RA = true

		default:
			ret += fmt.Sprintf("Address=%s\n", addr)
		}
	}

	if acceptIPv6RA {
		ret += "IPv6AcceptRA=true\n"
	} else {
		ret += "IPv6AcceptRA=false\n"
	}

	if hasDHCP4 && hasDHCP6 { //nolint:gocritic
		ret += "DHCP=yes\n"
	} else if hasDHCP4 {
		ret += "DHCP=ipv4\n"
	} else if hasDHCP6 {
		ret += "DHCP=ipv6\n"
	}

	return ret
}

func processRoutes(routes []api.SystemNetworkRoute) string {
	ret := ""

	for _, route := range routes {
		ret += "\n[Route]\n"

		switch route.Via {
		case "dhcp4":
			ret += "Gateway=_dhcp4\n"
		case "slaac":
			ret += "Gateway=_ipv6ra\n"
		default:
			ret += fmt.Sprintf("Gateway=%s\n", route.Via)
		}

		ret += fmt.Sprintf("Destination=%s\n", route.To)
	}

	return ret
}

func generateNetworkSectionContents(dns *api.SystemNetworkDNS, ntp *api.SystemNetworkNTP) string {
	ret := ""

	// If there are search domains or name servers, add those to the config.
	if dns != nil {
		if len(dns.SearchDomains) > 0 {
			ret += fmt.Sprintf("Domains=%s\n", strings.Join(dns.SearchDomains, " "))
		}

		for _, ns := range dns.Nameservers {
			ret += fmt.Sprintf("DNS=%s\n", ns)
		}
	}

	// If there are time servers defined, add them to the config.
	if ntp != nil {
		for _, ts := range ntp.Timeservers {
			ret += fmt.Sprintf("NTP=%s\n", ts)
		}
	}

	return ret
}

func generateTimesyncContents(ntp api.SystemNetworkNTP) string {
	if len(ntp.Timeservers) == 0 {
		return ""
	}

	return "[Time]\nFallbackNTP=" + strings.Join(ntp.Timeservers, " ") + "\n"
}

func generateBridgeVLANContents(bridgeName string, specificVLAN int, additionalVLANTags []int, vlans []api.SystemNetworkVLAN) string {
	vlanTags := []int{}

	// Add specific VLAN tag, if configured.
	if specificVLAN != 0 {
		vlanTags = append(vlanTags, specificVLAN)
	}

	// Add any additional VLAN tags.
	vlanTags = append(vlanTags, additionalVLANTags...)

	// Grab any relevant tags for this bridge from VLAN definitions.
	for _, vlan := range vlans {
		if vlan.Parent == bridgeName {
			vlanTags = append(vlanTags, vlan.ID)
		}
	}

	// Sort and remove any duplicate tags.
	slices.Sort(vlanTags)
	vlanTags = slices.Compact(vlanTags)

	ret := ""

	if len(vlanTags) > 0 {
		if specificVLAN != 0 {
			ret += "\n[BridgeVLAN]\n"
			ret += fmt.Sprintf("PVID=%d\n", specificVLAN)
			ret += fmt.Sprintf("EgressUntagged=%d\n", specificVLAN)
		}

		for _, tag := range vlanTags {
			ret += "\n[BridgeVLAN]\n"
			ret += fmt.Sprintf("VLAN=%d\n", tag)
		}
	}

	return ret
}

func generateLinkSectionContents(addresses []string) string {
	if len(addresses) == 0 {
		return "RequiredForOnline=no"
	}

	expectsIPv4 := false
	expectsIPv6 := false
	for _, addr := range addresses {
		switch addr {
		case "dhcp4":
			expectsIPv4 = true
		case "dhcp6", "slaac":
			expectsIPv6 = true

		default:
			if strings.Contains(addr, ".") {
				expectsIPv4 = true
			}

			if strings.Contains(addr, ":") {
				expectsIPv6 = true
			}
		}
	}

	if expectsIPv4 && expectsIPv6 {
		return "RequiredForOnline=yes\nRequiredFamilyForOnline=both"
	} else if expectsIPv4 {
		return "RequiredForOnline=yes\nRequiredFamilyForOnline=ipv4"
	}

	return "RequiredForOnline=yes\nRequiredFamilyForOnline=ipv6"
}
