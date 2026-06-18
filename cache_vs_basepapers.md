# eDAG-MEC vs the Base Papers — Experimental Evidence

This document positions the thesis against its two base papers and presents the graphs that
support the novelty claim. All results are **measured on a real Linux 6.18 kernel** (real
`iptables`/netfilter, real `cgroup/connect4`, real BPF maps, emulated multi-node via
netns+veth). The base papers are **pure simulations**; this work is the **real data-plane
substrate** that realises and improves the assumptions they only stipulate.

## The two base papers

| | **CachOf** (IEEE ToC 2025) | **DPLS** (IEEE IoT-J 2026) |
|---|---|---|
| Problem | Cache subtask **results** to assist DAG offloading | Online multi-DAG **scheduling** (subtask order) |
| Mechanism | popularity analysis → 0-1 knapsack → DDPG (DRL) | dynamic priority list: up/down rank, volume, contention |
| Cache cost model | **hit ⇒ `texe = 0`, downlink ignored** (Eq. 5) | n/a |
| Implementation | Python 3.7 simulation | simulation |
| Metrics | Total Reward, Avg Delay, Success Rate | mean/max completion time, load balance |

**Their shared blind spot:** both operate in the **control plane / simulation**. CachOf
*decides what to cache* and then assumes serving a cached result is **free** — no lookup cost,
no delivery (downlink) cost, coarse per-time-slot updates. DPLS decides *order* but routes
through whatever stack exists.

## The novelty (one sentence)

> The base papers assume a cache hit and an offload hop are nearly free; this thesis builds the
> **kernel-resident, dependency-aware cache + sender-side bypass** that makes those assumptions
> *physically true and O(1)* on real hardware — without replacing their DRL/priority decision
> logic.

## Evidence

### Graph 1 — Cache: eBPF kernel cache vs CachOf's application cache  (`cache_plot.png`)

The operation CachOf prices at zero: make a producer's D-byte result available to **N
downstream DAG consumers**, then reclaim it.

- **App-level edge cache** (the substrate CachOf implies): result in userspace memory,
  delivered to each consumer over the network stack → **~11.7 µs per consumer, O(N) downlink**.
- **eBPF kernel cache** (this thesis): stored once in a kernel hash map, served by an O(1)
  kernel access per consumer, GC'd on the last read → **~0.5 µs per consumer**.

| Fan-out N | App cache (µs) | eBPF cache (µs) | speed-up |
|--:|--:|--:|--:|
| 1  | 18.9  | 1.4  | 13.6× |
| 8  | 99.4  | 5.2  | 19.1× |
| 32 | 381.8 | 17.5 | 21.9× |
| 64 | 753.5 | 34.0 | **22.1×** |

Payload sweep (N=8): app ≈ 87 µs, eBPF ≈ 5 µs — **~17× cheaper, payload-independent**.

**Defence:** CachOf's `texe=0` is only achievable if the cache substrate is essentially free.
An application cache is **not** free — it pays the full downlink per consumer (the term CachOf
drops). The eBPF kernel cache is the substrate that gets within ~0.5 µs/consumer of the ideal,
**validating CachOf's assumption while exposing that an app-level cache violates it by 20×.**
Harness: `cmd/cache_bench`; data: `cache_fanout.csv`, `cache_payload.csv`.

### Graph 2 — Offload path: connect4 bypass vs kube-proxy  (`mec_plot.png`)

Emulated two-node edge cluster (netns + veth, distinct IPs, real DNAT/conntrack). The real
`cgroup/connect4` sender-side rewrite (O(1) vault lookup) vs kube-proxy iptables DNAT (O(N)):
connect4 stays **flat ~40 µs** while kube-proxy grows to **301 µs at 60 000 services (7.4×)**.
This is the data-plane realisation of the offload hop that DPLS schedules but assumes cheap.
Harness: `cmd/mec_bench`; data: `mec_results.csv`.

### Graph 3 — Routing-decision scaling: XDP O(1) vs iptables O(N)  (`crossover_plot.png`)

Micro-isolation of the routing decision: iptables ≈ 8.8 ns/rule (O(N)); XDP/eBPF map lookup
flat ~5.6 µs (O(1)) — the crossover and asymptotic divergence. Harness: `cmd/crossover_bench`.

## Honest scope (for the defence)

- **Substrate, not algorithm.** This work does not beat CachOf's DRL accuracy or DPLS's
  priority quality; it replaces the **cache/offload substrate beneath them**. Combine: their
  decision logic + this kernel data plane.
- **Per-access O(1), total still scales with fan-out.** The kernel cache costs ~0.5 µs *per
  consumer access* (flat in N); total fan-out is N × that constant. True N-independent delivery
  needs kernel-side `bpf_clone_redirect` (C3 "Path 2", future work). The measured win is the
  ~20× lower per-consumer constant and elimination of the userspace re-serve loop.
- **Emulated multi-node + software data plane.** netns+veth give real separate stacks on one
  host; absolute latencies on physical nodes/NICs differ, but the O(1)-vs-O(N) *shape* is an
  algorithmic property and carries over. The **energy** win still requires hardware offload
  (XDP on SmartNIC), as established in `comprehensive_experiments.md` §4.

**Bottom line:** against CachOf, the eBPF cache makes the "free hit" assumption real and is
~20× cheaper than the application cache it implies; against DPLS, the connect4 bypass makes the
offload hop O(1) in cluster size. The novelty is a real, dependency-aware kernel data plane
under the base papers' simulated decision logic.
