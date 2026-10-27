//go:build linux

package tap

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/kumabox/kumabox/types"
)

//go:generate sh -c "clang -O2 -g -target bpfel -I/usr/include/$(uname -m)-linux-gnu -c bpf/datapath.c -o bpf/datapath_bpfel.o"

//go:embed bpf/datapath_bpfel.o
var datapathObject []byte

// endpoint mirrors the C map value. Addresses are byte arrays so the ABI is
// independent of host endianness; keys and Enabled use native byte order.
type endpoint struct {
	Guest, Host, Gateway, Pool, Mask [4]byte
	Enabled                          uint32
	GuestMAC, HostMAC                [6]byte
}

type datapath struct {
	endpoints       *ebpf.Map
	ingress, egress *ebpf.Program
}

func (d *datapath) close() error {
	return errors.Join(d.endpoints.Close(), d.ingress.Close(), d.egress.Close())
}

// openDatapath is called under the installation lock. Pins keep shared programs
// alive after CLI exit. A schema directory prevents reusing incompatible maps.
func (p *Provider) openDatapath() (*datapath, error) {
	directory := filepath.Join(p.options.PinDir, "v1")
	var fs unix.Statfs_t
	if err := unix.Statfs(filepath.Dir(p.options.PinDir), &fs); err != nil {
		return nil, fmt.Errorf("find bpffs mount: %w", err)
	}
	if fs.Type != unix.BPF_FS_MAGIC {
		return nil, errors.New("eBPF pin directory must be on bpffs")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	revision := sha256.Sum256(datapathObject)
	programDir := filepath.Join(directory, fmt.Sprintf("%x", revision[:8]))
	mapPath := filepath.Join(directory, "endpoints")
	mapping, err := ebpf.LoadPinnedMap(mapPath, nil)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if mapping != nil {
		ingress, inErr := ebpf.LoadPinnedProgram(filepath.Join(programDir, "from_guest"), nil)
		if inErr == nil {
			egress, outErr := ebpf.LoadPinnedProgram(filepath.Join(programDir, "to_guest"), nil)
			if outErr == nil {
				return &datapath{mapping, ingress, egress}, nil
			}
			_ = ingress.Close()
			if !errors.Is(outErr, os.ErrNotExist) {
				return nil, errors.Join(outErr, mapping.Close())
			}
		} else if !errors.Is(inErr, os.ErrNotExist) {
			return nil, errors.Join(inErr, mapping.Close())
		}
		defer mapping.Close() //nolint:errcheck // collection replacements clone this descriptor
	}
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(datapathObject))
	if err != nil {
		return nil, err
	}
	options := ebpf.CollectionOptions{}
	if mapping != nil {
		options.MapReplacements = map[string]*ebpf.Map{"endpoints": mapping}
	}
	objects, err := ebpf.NewCollectionWithOptions(spec, options)
	if err != nil {
		return nil, fmt.Errorf("load TAP data plane: %w", err)
	}
	defer objects.Close()
	// Program revisions share the schema-compatible endpoint map. A new binary
	// loads its own programs; Verify/Recover replaces old TAP filters safely.
	if err := os.MkdirAll(programDir, 0o700); err != nil {
		return nil, err
	}
	for _, name := range []string{"from_guest", "to_guest"} {
		path := filepath.Join(programDir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := objects.Programs[name].Pin(path); err != nil {
			return nil, err
		}
	}
	if mapping == nil {
		if err := objects.Maps["endpoints"].Pin(mapPath); err != nil {
			return nil, err
		}
	}
	mapping, err = objects.Maps["endpoints"].Clone()
	if err != nil {
		return nil, err
	}
	ingress, err := objects.Programs["from_guest"].Clone()
	if err != nil {
		return nil, errors.Join(err, mapping.Close())
	}
	egress, err := objects.Programs["to_guest"].Clone()
	if err != nil {
		return nil, errors.Join(err, mapping.Close(), ingress.Close())
	}
	return &datapath{mapping, ingress, egress}, nil
}

func (p *Provider) endpoint(item types.NetworkInterface, enabled bool) (endpoint, error) {
	var result endpoint
	if err := item.Validate(); err != nil {
		return result, err
	}
	if item.AddressMode != types.AddressModeFixed {
		return result, errors.New("TAP endpoint requires fixed addressing")
	}
	if item.Index >= types.MaxSandboxNICs {
		return result, errors.New("fixed NIC slot exceeds the supported layout")
	}
	copy(result.Guest[:], net.ParseIP(item.IPv4.Address).To4())
	copy(result.Host[:], net.ParseIP(item.HostAddress).To4())
	copy(result.Pool[:], p.pool.Addr().AsSlice())
	copy(result.Mask[:], net.CIDRMask(p.pool.Bits(), 32))
	// Each slot has a local host gateway, even when it has no default route.
	result.Gateway = [4]byte{169, 254, 240, byte(item.Index*4 + 1)} //nolint:gosec // bounded NIC slot maps inside one /24
	mac, _ := net.ParseMAC(item.MAC)
	copy(result.GuestMAC[:], mac)
	result.HostMAC = [6]byte{2, 0x4b, 0x42, 0, byte(item.Index), 1} //nolint:gosec // NIC slot is below MaxSandboxNICs
	if enabled {
		result.Enabled = 1
	}
	return result, nil
}

func attachFilters(link netlink.Link, d *datapath) error {
	qdisc := &netlink.Clsact{QdiscAttrs: netlink.QdiscAttrs{LinkIndex: link.Attrs().Index, Handle: netlink.MakeHandle(0xffff, 0), Parent: netlink.HANDLE_CLSACT}}
	if err := netlink.QdiscAdd(qdisc); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	for _, direction := range []struct {
		parent  uint32
		program *ebpf.Program
		name    string
	}{
		{netlink.HANDLE_MIN_INGRESS, d.ingress, "kb_from_guest"}, {netlink.HANDLE_MIN_EGRESS, d.egress, "kb_to_guest"},
	} {
		filter := &netlink.BpfFilter{FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index, Parent: direction.parent, Handle: 1, Priority: 1, Protocol: unix.ETH_P_ALL,
		}, Fd: direction.program.FD(), Name: direction.name, DirectAction: true}
		if err := netlink.FilterReplace(filter); err != nil {
			return fmt.Errorf("attach %s: %w", direction.name, err)
		}
	}
	return nil
}
