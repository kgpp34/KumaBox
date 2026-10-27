//go:build linux && integration

package tap

import (
	"fmt"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// TestTAPQueueModeOpensForMonitor exercises the kernel's queue-mode check,
// rather than comparing flags against the same expression as production.
func TestTAPQueueModeOpensForMonitor(t *testing.T) {
	for _, queues := range []int{2, 4} {
		t.Run(fmt.Sprint(queues), func(t *testing.T) {
			device := &netlink.Tuntap{
				LinkAttrs: netlink.LinkAttrs{Name: fmt.Sprintf("kbtq%d-%d", os.Getpid(), queues)},
				Mode:      netlink.TUNTAP_MODE_TAP,
				Queues:    max(1, queues/2),
				Flags:     tapQueueFlags(queues),
			}
			if err := netlink.LinkAdd(device); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := netlink.LinkDel(device); err != nil {
					t.Errorf("remove temporary TAP: %v", err)
				}
			})
			// The provider closes its setup descriptors before the monitor
			// opens the persistent device. A single queue has only one owner.
			for _, descriptor := range device.Fds {
				if err := descriptor.Close(); err != nil {
					t.Fatal(err)
				}
			}
			request, err := unix.NewIfreq(device.Name)
			if err != nil {
				t.Fatal(err)
			}
			flags := uint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
			if queues > 2 {
				flags |= unix.IFF_MULTI_QUEUE
			}
			request.SetUint16(flags)
			fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd) //nolint:errcheck // temporary test descriptor
			if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, request); err != nil {
				t.Fatalf("monitor cannot reopen %d-queue TAP: %v", queues, err)
			}
		})
	}
}
