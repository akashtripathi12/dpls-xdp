// SPDX-License-Identifier: GPL-2.0
//
// fanout.c — C3 Path-2 data plane: kernel-side fan-out DELIVERY.
// ===============================================================
// HANDOFF §6-B Path-2. Path-1 (cmd/c3_bench, tc_bridge.c) proved the retention
// mechanism is cheap + O(1) per access, but TOTAL delivery to N consumers was
// still N userspace re-serves. This program closes that gap: a SINGLE producer
// trigger packet is fanned out to N distinct consumers entirely in the kernel
// via bpf_clone_redirect — the producer pays O(1) (one send) regardless of N.
//
// MECHANISM (attached to `lo` ingress via TCX):
//   producer --1 pkt--> 127.0.0.1:9000  (the "trigger")
//      TC ingress hook reads task_id, looks up fanout_map[task] = {n, dst[]}
//      for each consumer i:  rewrite daddr=dst[i], dport=9001, fix IP csum,
//                            zero UDP csum, bpf_clone_redirect(lo, INGRESS)
//      drop the original (TC_ACT_SHOT)
//   => kernel emits N copies; consumers bound to 127.0.0.{2+i}:9001 receive them.
//
// Clones carry dport 9001 (not 9000) so they pass straight through this hook on
// re-entry — no infinite fan-out. UDP checksum is zeroed (legal for IPv4 UDP),
// which avoids all L4 checksum math.
#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/udp.h>
#include <linux/pkt_cls.h>
#include <linux/in.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define TRIGGER_PORT  9000
#define CONSUMER_PORT 9001
#define MAX_FANOUT_P2 16

// Fixed offsets (standard IPv4 header, IHL=5, no options — we verify ihl==5).
#define ETH_LEN        14
#define IP_CHECK_OFF   (ETH_LEN + __builtin_offsetof(struct iphdr, check))
#define IP_DADDR_OFF   (ETH_LEN + __builtin_offsetof(struct iphdr, daddr))
#define UDP_OFF        (ETH_LEN + 20)
#define UDP_DEST_OFF   (UDP_OFF + __builtin_offsetof(struct udphdr, dest))
#define UDP_CHECK_OFF  (UDP_OFF + __builtin_offsetof(struct udphdr, check))

struct fanout_rule {
    __u32 n;                       // number of consumers
    __u32 dst[MAX_FANOUT_P2];      // consumer IPs, network byte order
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u32);
    __type(value, struct fanout_rule);
} fanout_map SEC(".maps");

SEC("tc")
int dpls_fanout(struct __sk_buff *skb)
{
    void *data     = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;

    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return TC_ACT_OK;
    if (eth->h_proto != bpf_htons(ETH_P_IP))
        return TC_ACT_OK;

    struct iphdr *ip = (struct iphdr *)(eth + 1);
    if ((void *)(ip + 1) > data_end)
        return TC_ACT_OK;
    if (ip->protocol != IPPROTO_UDP)
        return TC_ACT_OK;
    if (ip->ihl != 5)              // only standard headers (fixed offsets)
        return TC_ACT_OK;

    struct udphdr *udp = (struct udphdr *)((void *)ip + 20);
    if ((void *)(udp + 1) > data_end)
        return TC_ACT_OK;
    if (udp->dest != bpf_htons(TRIGGER_PORT))
        return TC_ACT_OK;          // not a trigger (incl. our own clones on 9001)

    __u32 *payload = (void *)(udp + 1);
    if ((void *)(payload + 1) > data_end)
        return TC_ACT_OK;
    __u32 task_id = *payload;

    struct fanout_rule *fo = bpf_map_lookup_elem(&fanout_map, &task_id);
    if (!fo)
        return TC_ACT_OK;

    __u32 cur = ip->daddr;         // read BEFORE any skb mutation

    // Rewrite dport -> consumer port and disable UDP checksum (once).
    __u16 np = bpf_htons(CONSUMER_PORT);
    bpf_skb_store_bytes(skb, UDP_DEST_OFF, &np, sizeof(np), 0);
    __u16 zero = 0;
    bpf_skb_store_bytes(skb, UDP_CHECK_OFF, &zero, sizeof(zero), 0);

    #pragma clang loop unroll(full)
    for (int i = 0; i < MAX_FANOUT_P2; i++) {
        if ((__u32)i >= fo->n)
            break;
        __u32 nd = fo->dst[i];
        // fix IP header checksum for the daddr change, then write daddr
        bpf_l3_csum_replace(skb, IP_CHECK_OFF, cur, nd, sizeof(nd));
        bpf_skb_store_bytes(skb, IP_DADDR_OFF, &nd, sizeof(nd), 0);
        // clone the current packet and inject it on lo's ingress -> delivered
        bpf_clone_redirect(skb, skb->ifindex, BPF_F_INGRESS);
        cur = nd;
    }

    return TC_ACT_SHOT;            // drop the original trigger; clones carry it
}

char _license[] SEC("license") = "GPL";
