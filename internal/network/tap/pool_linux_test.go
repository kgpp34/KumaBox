//go:build linux && integration

package tap

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/kumabox/kumabox/internal/network"
	"github.com/kumabox/kumabox/internal/types"
)

func kernelPool(t *testing.T) *Provider {
	t.Helper()
	p := testProvider(t)
	p.options.PinDir = "/sys/fs/bpf/kb-pool-" + p.owner
	p.options.PoolSize, p.options.PoolQueues = 4, []int{2, 4}
	t.Cleanup(func() {
		_ = p.Close()
		links, _ := netlink.LinkList()
		for _, link := range links {
			if _, _, owned := p.parsePoolAlias(link.Attrs().Alias); owned {
				if err := netlink.LinkDel(link); err != nil {
					t.Error(err)
				}
			}
		}
		if err := os.RemoveAll(p.options.PinDir); err != nil {
			t.Error(err)
		}
	})
	if err := p.StartPool(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Tests below exercise allocation/recovery without a background refill
	// changing which idle device is selected. Production keeps this worker on.
	p.taps.cancel()
	<-p.taps.done
	return p
}

func addPoolNIC(t *testing.T, p *Provider, id types.SandboxID, slot, queues int) lease {
	t.Helper()
	devices, err := p.Add(t.Context(), id, "ebpf", network.AddSpec{Index: slot, Queues: queues})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatal("missing assigned NIC")
	}
	item, err := p.readLease(p.indexPath(id, slot))
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func monitorQueue(t *testing.T, name string, queues int) int {
	t.Helper()
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	request, err := unix.NewIfreq(name)
	if err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	flags := uint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	if queues > 2 {
		flags |= unix.IFF_MULTI_QUEUE
	}
	request.SetUint16(flags)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, request); err != nil {
		_ = unix.Close(fd)
		t.Fatal(err)
	}
	return fd
}

func TestPoolReusesTAPWithFreshAddressAndOwnershipEpoch(t *testing.T) {
	p := kernelPool(t)
	a, _ := types.NewSandboxID()
	first := addPoolNIC(t, p, a, 0, 4)
	link, err := p.ownedLink(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	b, _ := types.NewSandboxID()
	second := addPoolNIC(t, p, b, 0, 4)
	current, err := p.ownedLink(second)
	if err != nil {
		t.Fatal(err)
	}
	if second.Interface.TAP != first.Interface.TAP || current.Attrs().Index != link.Attrs().Index || second.Epoch != first.Epoch+1 || second.Interface.HostAddress == first.Interface.HostAddress {
		t.Fatalf("reuse changed device or retained address/epoch: before=%+v after=%+v", first, second)
	}
	if index, ok := p.IndexForTAP(b, second.Interface.TAP); !ok || index != 0 {
		t.Fatal("hotplug lookup lost pooled identity")
	}
	d, release, err := p.dataPlane(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := p.releasePooledTAP(t.Context(), first, d); err == nil {
		t.Fatal("stale lifecycle reclaimed another sandbox's TAP")
	}
	if err := p.Verify(t.Context(), b, []types.NetworkInterface{second.Interface}); err != nil {
		t.Fatalf("stale cleanup damaged live owner: %v", err)
	}
	packet := udpPacket(net.IPv4(1, 1, 1, 1), net.ParseIP(first.Interface.HostAddress), [6]byte{})
	// BPF_PROG_TEST_RUN uses ifindex 0/1. The separate kernel tests exercise
	// attached filters; here we check the new endpoint rejects the old address.
	value, _ := p.endpoint(second.Interface, true)
	for _, key := range []uint32{0, 1} {
		if err := d.endpoints.Update(key, value, ebpf.UpdateAny); err != nil {
			t.Fatal(err)
		}
	}
	code, _, err := d.egress.Test(packet)
	if err != nil || code != 2 {
		t.Fatalf("old address reached new owner: %d %v", code, err)
	}
}

func TestPoolFencesBusyQueuesAndDiscardsBufferedFrames(t *testing.T) {
	for _, queues := range []int{2, 4} {
		t.Run(string(rune('0'+queues)), func(t *testing.T) {
			p := kernelPool(t)
			id, _ := types.NewSandboxID()
			first := addPoolNIC(t, p, id, 0, queues)
			fd := monitorQueue(t, first.Interface.TAP, queues)
			closed := false
			defer func() {
				if !closed {
					_ = unix.Close(fd)
				}
			}()
			peer, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(first.Interface.HostAddress), Port: 9987})
			if err != nil {
				t.Fatal(err)
			}
			pending := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // test process descriptor
			// Queue attachment changes carrier state asynchronously. Retry the
			// datagram within a bound, but require proof that a frame is pending
			// before testing disposal; one early dropped packet proves nothing.
			queued := false
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				if _, err := peer.Write([]byte("previous owner")); err != nil {
					_ = peer.Close()
					t.Fatal(err)
				}
				if _, err := unix.Poll(pending, 10); err != nil && !errors.Is(err, unix.EINTR) {
					_ = peer.Close()
					t.Fatal(err)
				}
				if pending[0].Revents&unix.POLLIN != 0 {
					queued = true
					break
				}
			}
			_ = peer.Close()
			if !queued {
				t.Fatal("old-owner frame was not actually queued")
			}
			if err := p.Delete(t.Context(), id); err == nil {
				t.Fatal("returned a TAP still held by the monitor")
			}
			if _, err := p.readLease(p.indexPath(id, 0)); err != nil {
				t.Fatal("busy cleanup lost durable ownership")
			}
			if err := unix.Close(fd); err != nil {
				t.Fatal(err)
			}
			closed = true
			if err := p.Delete(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			other, _ := types.NewSandboxID()
			second := addPoolNIC(t, p, other, 0, queues)
			if first.Interface.TAP != second.Interface.TAP {
				t.Fatal("did not reuse the cleaned device")
			}
			fd = monitorQueue(t, second.Interface.TAP, queues)
			defer unix.Close(fd) //nolint:errcheck // test queue
			var frame [4096]byte
			if n, err := unix.Read(fd, frame[:]); n > 0 || !errors.Is(err, unix.EAGAIN) {
				t.Fatalf("new owner inherited old frames: n=%d err=%v", n, err)
			}
		})
	}
}

func TestPoolEpochFencesRebindingOfTheSameSandboxSlot(t *testing.T) {
	p := kernelPool(t)
	clock := time.Now().UTC()
	p.now = func() time.Time { return clock }
	id, _ := types.NewSandboxID()
	first := addPoolNIC(t, p, id, 0, 4)
	if err := p.Remove(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(p.options.ReuseDelay + time.Second)
	second := addPoolNIC(t, p, id, 0, 4)
	if first.Interface.TAP != second.Interface.TAP || first.Interface.HostAddress != second.Interface.HostAddress || first.Epoch+1 != second.Epoch {
		t.Fatalf("did not rebind same resource with a fresh epoch: %+v %+v", first, second)
	}
	d, release, err := p.dataPlane(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := p.releasePooledTAP(t.Context(), first, d); err == nil {
		t.Fatal("stale cleanup reclaimed the new binding")
	}
	if err := p.retireAddress(first); err == nil {
		t.Fatal("stale retirement overwrote the new address lease")
	}
	if err := p.Verify(t.Context(), id, []types.NetworkInterface{second.Interface}); err != nil {
		t.Fatal(err)
	}
}

func TestPoolRecoversLiveAndUnindexedReservations(t *testing.T) {
	p := kernelPool(t)
	id, _ := types.NewSandboxID()
	first := addPoolNIC(t, p, id, 0, 4)
	fd := monitorQueue(t, first.Interface.TAP, 4)
	defer unix.Close(fd) //nolint:errcheck // live queue spans provider restart
	if err := os.Remove(p.indexPath(id, 0)); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(p.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.RecoverLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.StartPool(t.Context()); err != nil {
		t.Fatal(err)
	}
	other, _ := types.NewSandboxID()
	second := addPoolNIC(t, reopened, other, 0, 4)
	if second.Interface.TAP == first.Interface.TAP {
		t.Fatal("recovery assigned a live orphan to another owner")
	}
	// Rebuild the missing hard-link index without changing host identity or
	// taking a second TAP. The active monitor queue must remain valid.
	recovered := addPoolNIC(t, reopened, id, 0, 4)
	if recovered.Epoch != first.Epoch || recovered.Interface.TAP != first.Interface.TAP {
		t.Fatal("recovery replaced active identity")
	}
	if err := reopened.Verify(context.Background(), id, []types.NetworkInterface{first.Interface}); err != nil {
		t.Fatal(err)
	}
}

func TestPoolKeepsQueueShapesSeparateAndRecoversLegacyTAPs(t *testing.T) {
	p := kernelPool(t)
	a, _ := types.NewSandboxID()
	first := addPoolNIC(t, p, a, 0, 2)
	if err := p.Delete(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	b, _ := types.NewSandboxID()
	second := addPoolNIC(t, p, b, 0, 4)
	if first.Interface.TAP == second.Interface.TAP {
		t.Fatal("reused an incompatible queue shape")
	}
	// Legacy leases keep their deterministic names when pooling is enabled.
	c, _ := types.NewSandboxID()
	legacy, err := p.reserve(t.Context(), c, network.AddSpec{Queues: 2})
	if err != nil {
		t.Fatal(err)
	}
	d, release, err := p.dataPlane(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := p.ensureTAP(legacy, d); err != nil {
		t.Fatal(err)
	}
	result, err := p.Recover(t.Context(), c, "ebpf", []types.NetworkInterface{legacy.Interface})
	if err != nil || result[0].TAP != legacy.Interface.TAP {
		t.Fatalf("legacy recovery changed identity: %v", err)
	}
	if err := p.Delete(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(legacy.Interface.TAP); err == nil {
		t.Fatal("legacy TAP was not destroyed")
	}
}

func TestDisablingPoolRetiresIdleDevicesWithoutTouchingActiveOwner(t *testing.T) {
	p := kernelPool(t)
	id, _ := types.NewSandboxID()
	active := addPoolNIC(t, p, id, 0, 4)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	options := p.options
	options.PoolSize = 0
	disabled, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disabled.Close() })
	if err := disabled.StartPool(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := disabled.Verify(t.Context(), id, []types.NetworkInterface{active.Interface}); err != nil {
		t.Fatal(err)
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if _, _, owned := p.parsePoolAlias(link.Attrs().Alias); owned && link.Attrs().Name != active.Interface.TAP {
			t.Fatalf("disabled pool retained idle device %s", link.Attrs().Name)
		}
	}
	if err := disabled.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(active.Interface.TAP); err == nil {
		t.Fatal("disabled pool retained released TAP")
	}
}

func TestPoolReturnsUnpublishedReservationAfterAddressExhaustion(t *testing.T) {
	p := kernelPool(t)
	p.pool = netip.MustParsePrefix("10.89.0.0/28")
	for range 14 {
		id, _ := types.NewSandboxID()
		addPoolNIC(t, p, id, 0, 4)
	}
	id, _ := types.NewSandboxID()
	if _, err := p.Add(t.Context(), id, "ebpf", network.AddSpec{Queues: 4}); err == nil {
		t.Fatal("expected address exhaustion after borrowing a TAP")
	}
	d, release, err := p.dataPlane(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	p.maintainPool(t.Context(), d, false)
	p.taps.mu.Lock()
	defer p.taps.mu.Unlock()
	for _, entry := range p.taps.entries {
		if entry.owner == id {
			t.Fatal("failed publication retained an undiscoverable owner")
		}
		if entry.owner == "" && entry.phase != poolReady {
			t.Fatalf("unpublished TAP was not cleaned: %+v", entry)
		}
	}
}
