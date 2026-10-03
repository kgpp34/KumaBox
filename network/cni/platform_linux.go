//go:build linux

package cni

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"

	cns "github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	tapTXQueueLength = 10000
	tapGROMaxSize    = 65536
)

type linuxPlatform struct{}

func newPlatform() platform { return linuxPlatform{} }

func (linuxPlatform) EnsureNamespace(name, path string) (_ bool, returnErr error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	if err != nil {
		return false, fmt.Errorf("get current network namespace: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, netns.Set(original), original.Close())
	}()
	created, err := netns.NewNamed(name)
	if err != nil {
		return false, fmt.Errorf("create named network namespace %s: %w", name, err)
	}
	if err := created.Close(); err != nil {
		return false, fmt.Errorf("close network namespace %s: %w", name, err)
	}
	return true, nil
}

func (linuxPlatform) RemoveNamespace(ctx context.Context, name string) error {
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := netns.DeleteNamed(name)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return err
		case <-ticker.C:
		}
	}
}

func (linuxPlatform) NamespaceExists(path string) error {
	_, err := os.Stat(path)
	return err
}

func (linuxPlatform) VerifyTAP(namespacePath, tapName string) error {
	return cns.WithNetNSPath(namespacePath, func(_ cns.NetNS) error {
		_, err := netlink.LinkByName(tapName)
		return err
	})
}

func (linuxPlatform) SetupRedirect(namespacePath, interfaceName, tapName string, queues int, overrideMAC string) (string, error) {
	var mac string
	err := cns.WithNetNSPath(namespacePath, func(_ cns.NetNS) error {
		var err error
		mac, err = setupRedirect(interfaceName, tapName, queues, overrideMAC)
		return err
	})
	return mac, err
}

func setupRedirect(interfaceName, tapName string, queues int, overrideMAC string) (string, error) {
	source, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return "", fmt.Errorf("find CNI link %s: %w", interfaceName, err)
	}
	if overrideMAC != "" {
		hardwareAddress, err := net.ParseMAC(overrideMAC)
		if err != nil {
			return "", fmt.Errorf("parse MAC %s: %w", overrideMAC, err)
		}
		if err := netlink.LinkSetHardwareAddr(source, hardwareAddress); err != nil {
			return "", fmt.Errorf("set MAC on %s: %w", interfaceName, err)
		}
	}
	mac := cmp.Or(overrideMAC, source.Attrs().HardwareAddr.String())
	addresses, err := netlink.AddrList(source, netlink.FAMILY_ALL)
	if err != nil {
		return "", fmt.Errorf("list addresses on %s: %w", interfaceName, err)
	}
	for _, address := range addresses {
		if err := netlink.AddrDel(source, &address); err != nil {
			return "", fmt.Errorf("remove address %s from %s: %w", address.IPNet, interfaceName, err)
		}
	}
	tap, err := createTAP(tapName, queues)
	if err != nil {
		return "", err
	}
	if source.Attrs().MTU > 0 {
		if err := netlink.LinkSetMTU(tap, source.Attrs().MTU); err != nil {
			return "", fmt.Errorf("set TAP %s MTU: %w", tapName, err)
		}
	}
	for _, link := range []netlink.Link{source, tap} {
		if err := netlink.LinkSetUp(link); err != nil {
			return "", fmt.Errorf("set link %s up: %w", link.Attrs().Name, err)
		}
		qdisc := &netlink.Ingress{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Parent: netlink.HANDLE_INGRESS}}
		if err := netlink.QdiscAdd(qdisc); err != nil {
			return "", fmt.Errorf("add ingress qdisc to %s: %w", link.Attrs().Name, err)
		}
	}
	if err := redirect(source, tap); err != nil {
		return "", fmt.Errorf("redirect %s to %s: %w", interfaceName, tapName, err)
	}
	if err := redirect(tap, source); err != nil {
		return "", fmt.Errorf("redirect %s to %s: %w", tapName, interfaceName, err)
	}
	return mac, nil
}

func createTAP(name string, queues int) (netlink.Link, error) {
	queuePairs := max(1, queues/2)
	flags := netlink.TUNTAP_VNET_HDR | netlink.TUNTAP_NO_PI
	if queuePairs == 1 {
		flags |= netlink.TUNTAP_ONE_QUEUE
	} else {
		flags |= netlink.TUNTAP_MULTI_QUEUE_DEFAULTS
	}
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		Mode:      netlink.TUNTAP_MODE_TAP,
		Queues:    queuePairs,
		Flags:     flags,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return nil, fmt.Errorf("create TAP %s: %w", name, err)
	}
	for _, descriptor := range tap.Fds {
		_ = descriptor.Close()
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("resolve TAP %s: %w", name, err)
	}
	// Queue and GRO tuning improve throughput but are not supported by every
	// kernel. The functional network path must remain available in that case.
	_ = netlink.LinkSetTxQLen(link, tapTXQueueLength)
	_ = netlink.LinkSetGROMaxSize(link, tapGROMaxSize)
	return link, nil
}

func redirect(source, target netlink.Link) error {
	return netlink.FilterAdd(&netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: source.Attrs().Index, Parent: netlink.HANDLE_INGRESS,
			Priority: 1, Protocol: syscall.ETH_P_ALL,
		},
		Sel: &netlink.TcU32Sel{
			Flags: netlink.TC_U32_TERMINAL,
			Keys:  []netlink.TcU32Key{{Mask: 0, Val: 0, Off: 0, OffMask: 0}},
		},
		Actions: []netlink.Action{&netlink.MirredAction{
			ActionAttrs:  netlink.ActionAttrs{Action: netlink.TC_ACT_STOLEN},
			MirredAction: netlink.TCA_EGRESS_REDIR, Ifindex: target.Attrs().Index,
		}},
	})
}

func (linuxPlatform) DeleteTAP(namespacePath, tapName string) error {
	err := cns.WithNetNSPath(namespacePath, func(_ cns.NetNS) error {
		link, err := netlink.LinkByName(tapName)
		if err != nil {
			var notFound netlink.LinkNotFoundError
			if errors.As(err, &notFound) {
				return nil
			}
			return err
		}
		return netlink.LinkDel(link)
	})
	var namespaceMissing cns.NSPathNotExistErr
	if errors.As(err, &namespaceMissing) {
		return nil
	}
	return err
}

func (linuxPlatform) SetLinkState(namespacePath string, names []string, up bool) error {
	err := cns.WithNetNSPath(namespacePath, func(_ cns.NetNS) error {
		for _, name := range names {
			link, err := netlink.LinkByName(name)
			if err != nil {
				var notFound netlink.LinkNotFoundError
				if errors.As(err, &notFound) {
					continue
				}
				return err
			}
			if up {
				err = netlink.LinkSetUp(link)
			} else {
				err = netlink.LinkSetDown(link)
			}
			if err != nil {
				return fmt.Errorf("set link %s state: %w", name, err)
			}
		}
		return nil
	})
	var namespaceMissing cns.NSPathNotExistErr
	if errors.As(err, &namespaceMissing) {
		return nil
	}
	return err
}
