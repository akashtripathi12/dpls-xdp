// connect4.c — eDAG-MEC SENDER-SIDE data plane (the real architecture).
// Attaches to cgroup/connect4: intercepts the scheduler's connect() to a
// virtual service IP BEFORE the packet enters netfilter, looks the service up
// in the vault map (O(1) hash), and rewrites the destination to the chosen
// worker node's real IP. This is the kube-proxy DNAT/conntrack bypass.
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

// key   = virtual service IP (network byte order, 4 bytes)
// value = real worker node IP (network byte order, 4 bytes)
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1 << 20);
    __type(key, __u32);
    __type(value, __u32);
} vault SEC(".maps");

SEC("cgroup/connect4")
int connect4(struct bpf_sock_addr *ctx) {
    __u32 svc = ctx->user_ip4;                 // virtual service IP (net order)
    __u32 *real = bpf_map_lookup_elem(&vault, &svc);
    if (real)
        ctx->user_ip4 = *real;                 // O(1) redirect to worker node
    return 1;                                   // allow the connect()
}
char _license[] SEC("license") = "GPL";
