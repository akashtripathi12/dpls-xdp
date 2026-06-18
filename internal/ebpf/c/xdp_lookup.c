// xdp_lookup.c — minimal O(1) data-plane probe for the crossover benchmark.
// Performs exactly ONE hash-map lookup per packet (the "routing decision"),
// independent of how many entries (N services) the map holds. This is the
// XDP analogue of the eDAG-MEC vault_map lookup that replaces iptables.
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1 << 20);
    __type(key, __u32);
    __type(value, __u32);
} svc_map SEC(".maps");

SEC("xdp")
int xdp_lookup(struct xdp_md *ctx) {
    __u32 key = 0;                         // fixed key: O(1) lookup
    __u32 *v = bpf_map_lookup_elem(&svc_map, &key);
    if (v)
        return XDP_PASS;
    return XDP_PASS;
}
char _license[] SEC("license") = "GPL";
