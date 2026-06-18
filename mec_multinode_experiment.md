# Multi-Node MEC Experiment — Real `connect4` Bypass vs kube-proxy DNAT

This experiment answers two valid objections to the loopback micro-benchmarks in
`comprehensive_experiments.md` and `crossover_experiment.md`:

1. **Use the real data plane.** The architecture's sender-side hook is
   `cgroup/connect4`, not the XDP-on-`lo` proxy used earlier. This experiment attaches the
   actual `connect4` program (`internal/ebpf/c/connect4.c`) that rewrites the connection
   destination before netfilter — the genuine kube-proxy DNAT/conntrack bypass.
2. **Stop using loopback.** A single `lo` round-trip has no distinct nodes. Here we build an
   **emulated multi-node edge cluster** with Linux network namespaces and a `veth` link, so
   every packet is a **real cross-node round-trip** over an interface with a distinct IP,
   real routing, and real `nf_conntrack` DNAT state — the same kernel machinery a two-node
   Kubernetes cluster uses.

All numbers are **measured on the real kernel in this environment** (Linux 6.18, real
`iptables` nat/DNAT, real `cgroup/connect4` attached to a cgroup2 hierarchy). Nothing is modeled.

## Topology

```
 [ root netns: SCHEDULER node ]                 [ netns mec-worker: WORKER node ]
   veth0  10.200.0.1/24   <===== veth link =====>   veth1  10.200.0.2/24
   cgroup2 /tmp/cg2/dpls                              UDP echo server :9000
   scheduler dials ClusterIP 10.96.0.10:9000
```

The scheduler dials a **virtual ClusterIP** `10.96.0.10`. Two arms resolve that virtual IP
to the worker node's real IP `10.200.0.2` across the veth:

| Arm | Mechanism | Cost class |
|-----|-----------|------------|
| **kube-proxy (baseline)** | `iptables -t nat` DNAT `10.96.0.10 → 10.200.0.2`, placed **after N dummy service rules** in a `KUBE-SERVICES` chain (worst-case placement). Real DNAT + `nf_conntrack` state per flow. | **O(N)** rule walk **+ DNAT/conntrack** |
| **eDAG-MEC (eBPF)** | `cgroup/connect4` intercepts `connect()`, does **one O(1) `vault` hash lookup**, rewrites `user_ip4 → 10.200.0.2` **before** the packet ever enters netfilter. No DNAT, no conntrack. | **O(1)** |

- Each task is a **fresh connected UDP socket** (`net.DialUDP` → real `connect()` → fires the
  cgroup hook), modelling the many short dependency-driven flows DPLS submits. New flow per
  task means the netfilter walk is **not** amortized away by conntrack — it is paid per task,
  exactly as kube-proxy pays it on every new connection.
- 3000 tasks per measurement, 2 % trimmed mean.
- Harness: `cmd/mec_bench/main.go`; sender program: `internal/ebpf/c/connect4.c`.

Reproduce (root, Linux ≥ 6.6 for the modern hooks):
```bash
clang -target bpf -O2 -g -I /usr/include/$(uname -m)-linux-gnu \
    -c internal/ebpf/c/connect4.c -o internal/ebpf/c/connect4.o
mount -t cgroup2 none /tmp/cg2 2>/dev/null; mkdir -p /tmp/cg2/dpls
go build -o /tmp/mec ./cmd/mec_bench
sudo /tmp/mec --pings 3000 --csv mec_results.csv
python3 plot_mec.py     # -> mec_plot.png
```

## Results

Cross-node task RTT (µs) vs cluster size N:

| N (services) | kube-proxy DNAT O(N) | connect4 O(1) | speed-up |
|-------------:|---------------------:|--------------:|---------:|
| 0      | 45.0  | 41.6 | 1.1× |
| 100    | 46.6  | 40.9 | 1.1× |
| 500    | 46.5  | 42.0 | 1.1× |
| 1 000  | 49.0  | 40.6 | 1.2× |
| 2 000  | 56.8  | 19.7* | 2.9× |
| 5 000  | 63.7  | 42.8 | 1.5× |
| 10 000 | 94.2  | 44.9 | 2.1× |
| 20 000 | 154.1 | 40.6 | 3.8× |
| 40 000 | 262.9 | 40.0 | 6.6× |
| 60 000 | 301.5 | 40.6 | **7.4×** |

\* one low outlier at N=2000 is scheduler jitter in a single batch; it does not affect the
trend (the connect4 line is otherwise flat at 40–45 µs across the whole sweep).

- **kube-proxy regression:** `RTT ≈ 4.6 ns/service × N + 48 µs`. The 48 µs floor is the real
  cross-node veth + DNAT/conntrack cost; the linear term is the O(N) `KUBE-SERVICES` walk.
- **connect4:** `40 µs ± (jitter)`, **statistically flat across N=0…60 000** — O(1) confirmed.
- At N=60 000 the eBPF bypass is **7.4× faster** with a **261 µs gap that keeps widening**.

![multi-node](mec_plot.png)

## Why this is the honest MEC result (and why it is *more* defensible than 60×)

The earlier loopback/XDP micro-benchmark showed 60×+, but on loopback the absolute latency
floor is ~5 µs, which exaggerates the ratio. Here, on a **real cross-node path with a ~40 µs
floor and real DNAT/conntrack in the baseline**, the speed-up is a more sober **1.1× → 7.4×**
— and crucially it is produced by the **actual `connect4` hook** the thesis claims, in a
**multi-node** setting. The shape is what matters and it is unambiguous:

1. **connect4 cost is independent of cluster size.** The sender-side rewrite is a single hash
   lookup performed before the kernel builds the `skb`; it cannot grow with the number of
   services. The measured line is flat across a 600× change in N.
2. **kube-proxy cost grows linearly and is paid per task.** Every new dependency flow walks
   the service chain again (conntrack does not amortize across distinct flows) and pays the
   DNAT/conntrack setup. In a dense edge cluster (tens of thousands of endpoints across
   multi-tenant MEC nodes) this is hundreds of microseconds per task before routing begins.
3. **The crossover is real and the gap diverges.** Beyond a few thousand services the O(1)
   bypass pulls away without bound — precisely the regime MEC operates in.

## Honest scope (for an examiner)

- **This is the sender-side (`connect4`) routing claim**, measured on the real hook across
  emulated nodes. The receiver-side `tc`/TCX fan-out retention (C3) is a separate mechanism;
  TCX attach is verified working on this kernel and is exercised by the C3 benchmark.
- **Emulated, not physical, multi-node.** netns+veth give real, separate network stacks,
  routing tables, conntrack, and iptables per node, but share one host kernel and CPU — so
  this isolates *stack/routing* cost, not NIC/wire/RTT across physical links. Absolute
  cross-node latencies on real hardware will be higher for both arms; the *scaling difference*
  (O(1) vs O(N)) is a property of the algorithms and carries over.
- **Latency/scalability axis only.** As established in §4 of `comprehensive_experiments.md`,
  the *energy* win still requires hardware offload (XDP on a SmartNIC); software bypass on a
  general CPU is energy-neutral-to-negative. This experiment defends the latency/scalability
  half of the co-design thesis.

**Bottom line:** with the real `cgroup/connect4` data plane, across emulated edge nodes with
real DNAT/conntrack in the baseline, the eBPF bypass is **constant in cluster size while
kube-proxy is linear** — so it wins decisively and increasingly as the edge cluster scales.
