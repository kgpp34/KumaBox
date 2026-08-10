//go:build linux

package network

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kumabox/kumabox/internal/fileutil"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const cniPollInterval = 100 * time.Millisecond

const netnsDir = "/var/run/netns"

func NetNSPath(vmID string) string {
	return filepath.Join(netnsDir, vmID)
}

func prepareCNINetnsLinux(vmID, requestedPath string) (string, bool, error) {
	nsPath := NetNSPath(vmID)
	if requestedPath != "" {
		nsPath = requestedPath
	}
	if _, err := os.Stat(nsPath); err == nil {
		return nsPath, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, fmt.Errorf("stat netns %s: %w", nsPath, err)
	}
	if nsPath != NetNSPath(vmID) {
		return "", false, fmt.Errorf("missing CNI netns path %s is not managed by VM %s", nsPath, vmID)
	}
	if err := os.MkdirAll(netnsDir, 0o755); err != nil {
		return "", false, fmt.Errorf("create netns dir: %w", err)
	}
	if err := createNamedNetns(vmID); err != nil {
		return "", false, err
	}
	return nsPath, true, nil
}

func setupCNIDatapathLinux(nsPath, ifName, tapName string, queues int, overrideMAC string) (string, error) {
	var mac string
	err := withNetNSPath(nsPath, func() error {
		var err error
		mac, err = setupCNIDatapathInNS(ifName, tapName, queues, overrideMAC)
		return err
	})
	return mac, err
}

func deleteCNIDatapathLinux(nsPath, tapName string) error {
	if nsPath == "" || tapName == "" {
		return nil
	}
	if _, err := os.Stat(nsPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return withNetNSPath(nsPath, func() error {
		return DeleteHostTap(tapName)
	})
}

func deleteCNINetnsLinux(vmID, nsPath string) error {
	if vmID == "" || nsPath == "" || nsPath != NetNSPath(vmID) {
		return nil
	}
	deadline := time.Now().Add(time.Second)
	for {
		err := netns.DeleteNamed(vmID)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(cniPollInterval)
	}
}

func createNamedNetns(name string) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer fileutil.CloseAndJoin(&err, &origNS, "close original network namespace")

	ns, err := netns.NewNamed(name)
	if err != nil {
		return fmt.Errorf("create netns %s: %w", name, err)
	}
	fileutil.CloseAndJoin(&err, &ns, "close created network namespace")
	if err := netns.Set(origNS); err != nil {
		return fmt.Errorf("restore netns: %w", err)
	}
	return nil
}

func withNetNSPath(path string, fn func() error) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer fileutil.CloseAndJoin(&err, &origNS, "close original network namespace")

	targetNS, err := netns.GetFromPath(path)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", path, err)
	}
	defer fileutil.CloseAndJoin(&err, &targetNS, "close target network namespace")

	if err := netns.Set(targetNS); err != nil {
		return fmt.Errorf("enter netns %s: %w", path, err)
	}
	defer func() {
		_ = netns.Set(origNS)
	}()
	return fn()
}

func setupCNIDatapathInNS(ifName, tapName string, queues int, overrideMAC string) (string, error) {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return "", fmt.Errorf("find cni link %s: %w", ifName, err)
	}
	if overrideMAC != "" {
		hwAddr, parseErr := net.ParseMAC(overrideMAC)
		if parseErr != nil {
			return "", fmt.Errorf("parse MAC %s: %w", overrideMAC, parseErr)
		}
		if err := netlink.LinkSetHardwareAddr(link, hwAddr); err != nil {
			return "", fmt.Errorf("set MAC on %s: %w", ifName, err)
		}
	}
	mac := strings.ToLower(link.Attrs().HardwareAddr.String())
	if overrideMAC != "" {
		mac = strings.ToLower(overrideMAC)
	}
	if err := flushLinkAddresses(link); err != nil {
		return "", err
	}

	tap, created, err := ensureTap(Record{TAP: tapName, NumQueues: queues})
	if err != nil {
		return "", err
	}
	if created {
		defer func() {
			if err != nil {
				_ = netlink.LinkDel(tap)
			}
		}()
	}
	if mtu := link.Attrs().MTU; mtu > 0 {
		if err := netlink.LinkSetMTU(tap, mtu); err != nil {
			return "", fmt.Errorf("set tap %s mtu %d: %w", tapName, mtu, err)
		}
	}
	for _, l := range []netlink.Link{link, tap} {
		if err := netlink.LinkSetUp(l); err != nil {
			return "", fmt.Errorf("set %s up: %w", l.Attrs().Name, err)
		}
	}
	for _, l := range []netlink.Link{link, tap} {
		if err := ensureIngressQdisc(l); err != nil {
			return "", err
		}
	}
	if err := addTCRedirect(link, tap); err != nil {
		return "", fmt.Errorf("redirect %s -> %s: %w", ifName, tapName, err)
	}
	if err := addTCRedirect(tap, link); err != nil {
		return "", fmt.Errorf("redirect %s -> %s: %w", tapName, ifName, err)
	}
	return mac, nil
}

func flushLinkAddresses(link netlink.Link) error {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("list addrs on %s: %w", link.Attrs().Name, err)
	}
	for _, addr := range addrs {
		if err := netlink.AddrDel(link, &addr); err != nil {
			return fmt.Errorf("flush addr %s on %s: %w", addr.IPNet, link.Attrs().Name, err)
		}
	}
	return nil
}

func ensureIngressQdisc(link netlink.Link) error {
	qdisc := &netlink.Ingress{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.HANDLE_INGRESS,
		},
	}
	if err := netlink.QdiscAdd(qdisc); err != nil && !os.IsExist(err) {
		return fmt.Errorf("add ingress qdisc on %s: %w", link.Attrs().Name, err)
	}
	return nil
}

func addTCRedirect(from, to netlink.Link) error {
	filter := &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: from.Attrs().Index,
			Parent:    netlink.HANDLE_INGRESS,
			Priority:  1,
			Protocol:  syscall.ETH_P_ALL,
		},
		Sel: &netlink.TcU32Sel{
			Flags: netlink.TC_U32_TERMINAL,
			Keys: []netlink.TcU32Key{
				{Mask: 0x0, Val: 0x0, Off: 0, OffMask: 0x0},
			},
		},
		Actions: []netlink.Action{
			&netlink.MirredAction{
				ActionAttrs:  netlink.ActionAttrs{Action: netlink.TC_ACT_STOLEN},
				MirredAction: netlink.TCA_EGRESS_REDIR,
				Ifindex:      to.Attrs().Index,
			},
		},
	}
	return netlink.FilterAdd(filter)
}
