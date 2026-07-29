//go:build linux

package server

import (
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
)

func applyIdentity(req identityRequest) error {
	if req.Hostname == "" || strings.ContainsAny(req.Hostname, "/\x00\n") {
		return fmt.Errorf("invalid hostname")
	}
	if err := syscall.Sethostname([]byte(req.Hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	if err := os.WriteFile("/etc/hostname", []byte(req.Hostname+"\n"), 0o644); err != nil {
		return fmt.Errorf("persist hostname: %w", err)
	}
	return applyNetworkIdentity(req.Interfaces)
}

// applyNetworkIdentity changes networkd state only when the clone has guest
// NICs. A networkless clone still needs a unique hostname, but reloading
// networkd in that case is unrelated work during the restore critical path.
func applyNetworkIdentity(identities []interfaceIdentity) error {
	if len(identities) == 0 {
		return nil
	}
	interfaceNames, err := persistNetworkdIdentity(identities)
	if err != nil {
		return fmt.Errorf("persist network identity: %w", err)
	}
	if err := reloadNetworkd(); err != nil {
		return fmt.Errorf("reload network identity: %w", err)
	}
	for index, identity := range identities {
		if err := configureInterface(index, identity); err != nil {
			return err
		}
	}
	if err := reconfigureNetworkd(interfaceNames); err != nil {
		return fmt.Errorf("reconfigure network identity: %w", err)
	}
	return nil
}

func configureInterface(index int, identity interfaceIdentity) error {
	link, err := linkByMAC(identity.MAC)
	if err != nil {
		return fmt.Errorf("configure interface %d: %w", index, err)
	}
	if identity.Name != "" && link.Attrs().Name != identity.Name {
		if err := netlink.LinkSetDown(link); err != nil {
			return fmt.Errorf("set %s down: %w", link.Attrs().Name, err)
		}
		if err := netlink.LinkSetName(link, identity.Name); err != nil {
			return fmt.Errorf("rename %s to %s: %w", link.Attrs().Name, identity.Name, err)
		}
		link, err = netlink.LinkByName(identity.Name)
		if err != nil {
			return fmt.Errorf("resolve renamed interface %s: %w", identity.Name, err)
		}
	}
	if err := flushAddresses(link); err != nil {
		return err
	}
	if identity.IP != "" {
		address, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", identity.IP, identity.Prefix))
		if err != nil {
			return fmt.Errorf("parse address for %s: %w", link.Attrs().Name, err)
		}
		if err := netlink.AddrReplace(link, address); err != nil {
			return fmt.Errorf("set address on %s: %w", link.Attrs().Name, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set %s up: %w", link.Attrs().Name, err)
	}
	if identity.Gateway != "" {
		gateway := net.ParseIP(identity.Gateway)
		if gateway == nil {
			return fmt.Errorf("invalid gateway %s", identity.Gateway)
		}
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Gw: gateway, Priority: 100 + index}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("set default route on %s: %w", link.Attrs().Name, err)
		}
	}
	return nil
}

func linkByMAC(mac string) (netlink.Link, error) {
	if _, err := net.ParseMAC(mac); err != nil {
		return nil, fmt.Errorf("invalid MAC %s", mac)
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	for _, link := range links {
		if strings.EqualFold(link.Attrs().HardwareAddr.String(), mac) {
			return link, nil
		}
	}
	return nil, fmt.Errorf("interface with MAC %s not found", mac)
}

func flushAddresses(link netlink.Link) error {
	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("list addresses on %s: %w", link.Attrs().Name, err)
	}
	for i := range addresses {
		if err := netlink.AddrDel(link, &addresses[i]); err != nil {
			return fmt.Errorf("remove address from %s: %w", link.Attrs().Name, err)
		}
	}
	return nil
}
