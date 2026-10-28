// SPDX-License-Identifier: MIT
// Fixed guest identities are private to one TAP. Translation happens before
// host routing/conntrack on ingress, and after reverse NAT on TAP egress.
#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/if_packet.h>
#include <linux/ip.h>
#include <linux/pkt_cls.h>
#include <linux/in.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

struct endpoint {
    __be32 guest, host, gateway, pool, mask;
    __u32 enabled;
    __u8 guest_mac[6], host_mac[6];
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 65536);
    __uint(map_flags, BPF_F_NO_PREALLOC);
    __type(key, __u32);
    __type(value, struct endpoint);
} endpoints SEC(".maps");

static __always_inline int mac_equal(const __u8 *a, const __u8 *b)
{
    return a[0] == b[0] && a[1] == b[1] && a[2] == b[2] &&
           a[3] == b[3] && a[4] == b[4] && a[5] == b[5];
}

// Change an IPv4 address and its transport pseudoheader checksum. outer_csum
// additionally repairs an ICMP error containing the translated packet header.
static __always_inline int translate(struct __sk_buff *skb, __u32 offset,
                                     int source, __be32 old, __be32 next,
                                     __u32 outer_csum)
{
    struct iphdr ip;
    if (bpf_skb_load_bytes(skb, offset, &ip, sizeof(ip))) return -1;
    if (ip.version != 4 || ip.ihl < 5) return -1;
    __u32 address = offset + (source ? 12 : 16);
    __u32 checksum = offset + 10;
    __be16 old_ip_sum = ip.check, new_ip_sum;
    if (bpf_l3_csum_replace(skb, checksum, old, next, 4)) return -1;
    if (bpf_skb_store_bytes(skb, address, &next, 4, 0)) return -1;
    if (outer_csum) {
        if (bpf_skb_load_bytes(skb, checksum, &new_ip_sum, 2)) return -1;
        if (bpf_l4_csum_replace(skb, outer_csum, old, next, 4)) return -1;
        if (bpf_l4_csum_replace(skb, outer_csum, old_ip_sum, new_ip_sum, 2)) return -1;
    }
    // Non-initial fragments contain no transport header. Linux conntrack
    // handles reassembly; every fragment still needs its IPv4 address changed.
    if (ip.frag_off & bpf_htons(0x1fff)) return 0;
    __u32 l4 = offset + (__u32)ip.ihl * 4;
    __u32 l4_checksum;
    if (ip.protocol == IPPROTO_TCP) l4_checksum = l4 + 16;
    else if (ip.protocol == IPPROTO_UDP) l4_checksum = l4 + 6;
    else return 0;
    __be16 before, after;
    if (bpf_skb_load_bytes(skb, l4_checksum, &before, 2)) {
        // ICMP errors may quote only eight bytes of the transport header.
        return outer_csum ? 0 : -1;
    }
    if (ip.protocol == IPPROTO_UDP && !before) return 0;
    if (bpf_l4_csum_replace(skb, l4_checksum, old, next,
                          BPF_F_PSEUDO_HDR | BPF_F_MARK_MANGLED_0 | 4)) return -1;
    if (outer_csum) {
        if (bpf_skb_load_bytes(skb, l4_checksum, &after, 2)) return -1;
        if (bpf_l4_csum_replace(skb, outer_csum, before, after, 2)) return -1;
    }
    return 0;
}

static __always_inline int packet(struct __sk_buff *skb, int outgoing)
{
    __u32 index = skb->ifindex;
    struct endpoint *e = bpf_map_lookup_elem(&endpoints, &index);
    if (!e || !e->enabled) return TC_ACT_SHOT;
    struct ethhdr eth;
    if (bpf_skb_load_bytes(skb, 0, &eth, sizeof(eth))) return TC_ACT_SHOT;
    if (outgoing && !mac_equal(eth.h_source, e->guest_mac)) return TC_ACT_SHOT;
    if (eth.h_proto == bpf_htons(ETH_P_ARP)) {
        // Linux answers only this TAP's local gateway. Do not let a guest
        // advertise or resolve other guests' unique host endpoints.
        struct { __be16 hardware, protocol; __u8 hlen, plen; __be16 operation;
                 __u8 sender_mac[6]; __be32 sender_ip;
                 __u8 target_mac[6]; __be32 target_ip; } __attribute__((packed)) arp;
        if (bpf_skb_load_bytes(skb, ETH_HLEN, &arp, sizeof(arp))) return TC_ACT_SHOT;
        if (outgoing && (arp.operation != bpf_htons(1) || arp.target_ip != e->gateway ||
                        (arp.sender_ip != e->guest && arp.sender_ip != 0) ||
                        !mac_equal(arp.sender_mac, e->guest_mac))) return TC_ACT_SHOT;
        return TC_ACT_OK;
    }
    // This backend has an explicit IPv4 contract; IPv6 cannot bypass policy.
    if (eth.h_proto != bpf_htons(ETH_P_IP)) return TC_ACT_SHOT;
    struct iphdr ip;
    if (bpf_skb_load_bytes(skb, ETH_HLEN, &ip, sizeof(ip))) return TC_ACT_SHOT;
    if (ip.version != 4 || ip.ihl < 5) return TC_ACT_SHOT;
    if (ip.protocol != IPPROTO_TCP && ip.protocol != IPPROTO_UDP && ip.protocol != IPPROTO_ICMP) return TC_ACT_SHOT;
    if (outgoing) {
        if (ip.saddr != e->guest || (ip.daddr & e->mask) == e->pool) return TC_ACT_SHOT;
        if (translate(skb, ETH_HLEN, 1, e->guest, e->host, 0)) return TC_ACT_SHOT;
        // This TAP is an L3 endpoint. A restored neighbour cache can still
        // address the source host's gateway MAC. After validating the guest,
        // normalize delivery to this host; otherwise IPv4 drops OTHERHOST
        // frames even though TC has translated their source successfully.
        if (bpf_skb_store_bytes(skb, 0, e->host_mac, ETH_ALEN, 0)) return TC_ACT_SHOT;
        if (bpf_skb_change_type(skb, PACKET_HOST)) return TC_ACT_SHOT;
    } else {
        if (ip.daddr != e->host) return TC_ACT_SHOT;
        if (translate(skb, ETH_HLEN, 0, e->host, e->guest, 0)) return TC_ACT_SHOT;
    }
    if (ip.protocol == IPPROTO_ICMP && !(ip.frag_off & bpf_htons(0x1fff))) {
        __u32 icmp = ETH_HLEN + (__u32)ip.ihl * 4;
        __u8 type;
        if (bpf_skb_load_bytes(skb, icmp, &type, 1)) return TC_ACT_SHOT;
        if (type == 3 || type == 11 || type == 12) {
            struct iphdr quoted;
            if (bpf_skb_load_bytes(skb, icmp + 8, &quoted, sizeof(quoted))) return TC_ACT_SHOT;
            if (outgoing) {
                if (quoted.daddr != e->guest || translate(skb, icmp+8, 0, e->guest, e->host, icmp+2)) return TC_ACT_SHOT;
            } else {
                if (quoted.saddr != e->host || translate(skb, icmp+8, 1, e->host, e->guest, icmp+2)) return TC_ACT_SHOT;
            }
        }
    }
    return TC_ACT_OK;
}

SEC("tc/ingress") int from_guest(struct __sk_buff *skb) { return packet(skb, 1); }
SEC("tc/egress") int to_guest(struct __sk_buff *skb) { return packet(skb, 0); }
char LICENSE[] SEC("license") = "MIT";
