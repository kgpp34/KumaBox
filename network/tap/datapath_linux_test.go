//go:build linux && integration

package tap

import (
	"encoding/binary"
	"net"
	"os"
	"testing"

	"github.com/cilium/ebpf"
)

func checksum(data []byte) uint16 {
	var sum uint32
	for len(data) > 1 {
		sum += uint32(binary.BigEndian.Uint16(data))
		data = data[2:]
	}
	if len(data) > 0 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	return ^uint16(sum)
}

func udpPacket(source, destination net.IP, sourceMAC [6]byte) []byte {
	packet := make([]byte, 14+20+8+4)
	copy(packet[6:12], sourceMAC[:])
	packet[12], packet[13] = 8, 0
	ip := packet[14:]
	ip[0], ip[8], ip[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	copy(ip[12:16], source.To4())
	copy(ip[16:20], destination.To4())
	binary.BigEndian.PutUint16(ip[10:], checksum(ip[:20]))
	udp := ip[20:]
	binary.BigEndian.PutUint16(udp, 12345)
	binary.BigEndian.PutUint16(udp[2:], 53)
	binary.BigEndian.PutUint16(udp[4:], uint16(len(udp)))
	copy(udp[8:], []byte{1, 2, 3, 4})
	pseudo := append([]byte{}, ip[12:20]...)
	pseudo = append(pseudo, 0, 17, 0, byte(len(udp)))
	pseudo = append(pseudo, udp...)
	binary.BigEndian.PutUint16(udp[6:], checksum(pseudo))
	return packet
}

func TestKernelDataPlaneTranslatesAndRejectsSpoofing(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires CAP_BPF and CAP_NET_ADMIN")
	}
	p := testProvider(t)
	p.options.PinDir = "/sys/fs/bpf/kb-test-" + p.owner
	t.Cleanup(func() {
		if err := os.RemoveAll(p.options.PinDir); err != nil {
			t.Error(err)
		}
	})
	d, err := p.openDatapath()
	if err != nil {
		t.Fatalf("load BPF: %+v", err)
	}
	defer d.close() //nolint:errcheck // test cleanup
	value := endpoint{Guest: [4]byte{169, 254, 240, 2}, Host: [4]byte{10, 89, 0, 7}, Gateway: [4]byte{169, 254, 240, 1}, Pool: [4]byte{10, 89, 0, 0}, Mask: [4]byte{255, 255, 0, 0}, Enabled: 1, GuestMAC: [6]byte{2, 0x4b, 0x42, 0, 0, 2}}
	for _, key := range []uint32{0, 1} {
		if err := d.endpoints.Update(key, value, ebpf.UpdateAny); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name                string
		source, destination net.IP
		mac                 [6]byte
		program             *ebpf.Program
		drop                bool
	}{
		{"outbound UDP", net.IP(value.Guest[:]), net.IPv4(1, 1, 1, 1), value.GuestMAC, d.ingress, false},
		{"reply UDP", net.IPv4(1, 1, 1, 1), net.IP(value.Host[:]), value.HostMAC, d.egress, false},
		{"spoofed IP", net.IPv4(10, 89, 0, 8), net.IPv4(1, 1, 1, 1), value.GuestMAC, d.ingress, true},
		{"spoofed MAC", net.IP(value.Guest[:]), net.IPv4(1, 1, 1, 1), [6]byte{}, d.ingress, true},
		{"peer endpoint", net.IP(value.Guest[:]), net.IPv4(10, 89, 0, 8), value.GuestMAC, d.ingress, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, packet, err := test.program.Test(udpPacket(test.source, test.destination, test.mac))
			if err != nil {
				t.Fatal(err)
			}
			if test.drop {
				if result != 2 {
					t.Fatalf("spoof accepted: %d", result)
				}
				return
			}
			if result != 0 {
				t.Fatalf("packet dropped: %d", result)
			}
			ip := packet[14:]
			if checksum(ip[:20]) != 0 {
				t.Fatal("invalid IPv4 checksum")
			}
			udp := ip[20:]
			pseudo := append([]byte{}, ip[12:20]...)
			pseudo = append(pseudo, 0, 17, 0, byte(len(udp)))
			pseudo = append(pseudo, udp...)
			if checksum(pseudo) != 0 {
				t.Fatal("invalid UDP checksum")
			}
			if test.program == d.ingress && !net.IP(ip[12:16]).Equal(net.IP(value.Host[:])) {
				t.Fatal("host source address not rewritten")
			}
			if test.program == d.egress && !net.IP(ip[16:20]).Equal(net.IP(value.Guest[:])) {
				t.Fatal("guest destination address not rewritten")
			}
		})
	}
	for _, protocol := range []byte{6, 1} {
		t.Run(map[byte]string{6: "TCP checksums", 1: "ICMP echo checksums"}[protocol], func(t *testing.T) {
			for _, direction := range []struct {
				program  *ebpf.Program
				src, dst net.IP
			}{{d.ingress, net.IP(value.Guest[:]), net.IPv4(1, 1, 1, 1)}, {d.egress, net.IPv4(1, 1, 1, 1), net.IP(value.Host[:])}} {
				packet := udpPacket(direction.src, direction.dst, value.GuestMAC)
				packet = append(packet[:34], make([]byte, 20)...)
				ip := packet[14:]
				ip[9] = protocol
				binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
				ip[10], ip[11] = 0, 0
				binary.BigEndian.PutUint16(ip[10:], checksum(ip[:20]))
				transport := ip[20:]
				if protocol == 6 {
					binary.BigEndian.PutUint16(transport, 12345)
					binary.BigEndian.PutUint16(transport[2:], 443)
					transport[12] = 0x50
					pseudo := append([]byte{}, ip[12:20]...)
					pseudo = append(pseudo, 0, protocol, 0, byte(len(transport)))
					pseudo = append(pseudo, transport...)
					binary.BigEndian.PutUint16(transport[16:], checksum(pseudo))
				} else {
					transport[0] = 8
					binary.BigEndian.PutUint16(transport[2:], checksum(transport))
				}
				code, out, err := direction.program.Test(packet)
				if err != nil || code != 0 {
					t.Fatalf("translate: %d %v", code, err)
				}
				ip = out[14:]
				if checksum(ip[:20]) != 0 {
					t.Fatal("invalid IPv4 checksum")
				}
				transport = ip[20:]
				if protocol == 6 {
					pseudo := append([]byte{}, ip[12:20]...)
					pseudo = append(pseudo, 0, protocol, 0, byte(len(transport)))
					pseudo = append(pseudo, transport...)
					if checksum(pseudo) != 0 {
						t.Fatal("invalid TCP checksum")
					}
				} else if checksum(transport) != 0 {
					t.Fatal("invalid ICMP checksum")
				}
			}
		})
	}
	t.Run("ICMP error quoted UDP", func(t *testing.T) {
		quoted := udpPacket(net.IP(value.Host[:]), net.IPv4(1, 1, 1, 1), value.GuestMAC)[14:]
		packet := udpPacket(net.IPv4(1, 1, 1, 1), net.IP(value.Host[:]), value.HostMAC)
		icmp := make([]byte, 8)
		icmp[0], icmp[1] = 3, 4
		icmp = append(icmp, quoted...)
		binary.BigEndian.PutUint16(icmp[2:], checksum(icmp))
		packet = append(packet[:34], icmp...)
		ip := packet[14:]
		ip[9] = 1
		binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
		ip[10], ip[11] = 0, 0
		binary.BigEndian.PutUint16(ip[10:], checksum(ip[:20]))
		code, out, err := d.egress.Test(packet)
		if err != nil || code != 0 {
			t.Fatalf("translate ICMP error: %d %v", code, err)
		}
		ip = out[14:]
		if checksum(ip[:20]) != 0 || checksum(ip[20:]) != 0 {
			t.Fatal("invalid outer IPv4/ICMP checksum")
		}
		quoted = ip[28:]
		if !net.IP(quoted[12:16]).Equal(net.IP(value.Guest[:])) || checksum(quoted[:20]) != 0 {
			t.Fatal("quoted source identity was not repaired")
		}
		pseudo := append([]byte{}, quoted[12:20]...)
		pseudo = append(pseudo, 0, 17, 0, byte(len(quoted)-20))
		pseudo = append(pseudo, quoted[20:]...)
		if checksum(pseudo) != 0 {
			t.Fatal("quoted UDP checksum not repaired")
		}
	})
	t.Run("outgoing ICMP error quoted UDP", func(t *testing.T) {
		quoted := udpPacket(net.IPv4(1, 1, 1, 1), net.IP(value.Guest[:]), value.HostMAC)[14:]
		packet := udpPacket(net.IP(value.Guest[:]), net.IPv4(1, 1, 1, 1), value.GuestMAC)
		icmp := make([]byte, 8)
		icmp[0], icmp[1] = 3, 3
		icmp = append(icmp, quoted...)
		binary.BigEndian.PutUint16(icmp[2:], checksum(icmp))
		packet = append(packet[:34], icmp...)
		ip := packet[14:]
		ip[9] = 1
		binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
		ip[10], ip[11] = 0, 0
		binary.BigEndian.PutUint16(ip[10:], checksum(ip[:20]))
		code, out, err := d.ingress.Test(packet)
		if err != nil || code != 0 {
			t.Fatalf("translate outgoing ICMP error: %d %v", code, err)
		}
		ip = out[14:]
		if checksum(ip[:20]) != 0 || checksum(ip[20:]) != 0 {
			t.Fatal("invalid outer IPv4/ICMP checksum")
		}
		quoted = ip[28:]
		if !net.IP(quoted[16:20]).Equal(net.IP(value.Host[:])) || checksum(quoted[:20]) != 0 {
			t.Fatal("quoted destination identity was not repaired")
		}
		pseudo := append([]byte{}, quoted[12:20]...)
		pseudo = append(pseudo, 0, 17, 0, byte(len(quoted)-20))
		pseudo = append(pseudo, quoted[20:]...)
		if checksum(pseudo) != 0 {
			t.Fatal("quoted UDP checksum not repaired")
		}
	})
	t.Run("disabled endpoint", func(t *testing.T) {
		value.Enabled = 0
		for _, key := range []uint32{0, 1} {
			if err := d.endpoints.Update(key, value, ebpf.UpdateAny); err != nil {
				t.Fatal(err)
			}
		}
		code, _, err := d.ingress.Test(udpPacket(net.IP(value.Guest[:]), net.IPv4(1, 1, 1, 1), value.GuestMAC))
		if err != nil || code != 2 {
			t.Fatalf("disabled endpoint forwarded traffic: %d %v", code, err)
		}
	})
}
