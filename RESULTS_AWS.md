# eDAG-MEC — Final Results on Real AWS Hardware

> Independent reproduction + extension of the eDAG-MEC eBPF data-plane benchmarks
> on a **real 3-node AWS EC2 cluster**, plus the new physical cross-VM experiment
> (HANDOFF §6-D) and the energy/EDP analysis (§6-A). All numbers below are
> **measured on real hardware** unless explicitly marked *modeled*.

## Test environment
- **Cluster:** 3 × EC2 `c7i-flex.large` (2 vCPU each), ap-south-1b, same VPC subnet.
  - node-b `172.31.3.35` = scheduler · node-c `172.31.4.205` = worker.
- **OS / kernel:** Ubuntu 24.04.4 LTS, **kernel 6.17-aws**, cgroup v2 unified.
- **Toolchain:** clang 18, Go 1.22.2, cilium/ebpf v0.13.2, libbpf 1.3.
- All three eBPF objects (`connect4.o`, `xdp_lookup.o`, `tc_bridge.o`) compile clean.
- Reproduce: `sudo bash run_all.sh` (single-host suite) and
  `bash run_xnode.sh <worker_ip>` (cross-VM). Both are detached + self-cleaning.

## Headline results

| Experiment | Baseline (O(N)) | eDAG-MEC (O(1)) | Speed-up @ max N | Source |
|---|---|---|---|---|
| **§6-D Cross-VM MEC** (real 2-node) | kube-proxy 1.97 ns/svc·N + **99 µs** floor → 211 µs @60k | connect4 **flat 91 µs** | **2.55× @ N=60k**, gap widening | `mec_xnode_results.csv` |
| **MEC** (single-host veth) | kube-proxy → 151 µs @60k | connect4 **flat ~21 µs** | **7.1× @ N=60k** | `mec_results.csv` |
| **Crossover** (routing cost only) | iptables → 205 µs @40k | XDP **flat ~5.4 µs** | **~37× @ N=40k** | `crossover_results.csv` |
| **Cache fan-out** vs CachOf | app cache **~10 µs/consumer** | eBPF **~0.55 µs/consumer** | **~18× @ N=64** | `cache_fanout.csv` |
| **§6-B C3 fan-out cache** (NEW, TCX port) | routing-only 9.4 µs | retention store 10.1 µs | **+0.7 µs overhead; O(1) in fan-out; GC 3000/3000** | `c3_results.txt` |
| **§6-B Path-2 kernel fan-out** (NEW) | app: N sends 29.4 µs @16 | 1 send → kernel clone_redirect 13.1 µs | **2.24× @ N=16; all N delivered; O(1) syscalls** | `path2_results.csv` |
| **EDP** (energy×delay) | kube-proxy + app cache | connect4 + eBPF cache | **197× (sw) / 1316× (HW*)** @60k | `edp_results.csv` |

\* HW-offload energy is *modeled* (SmartNIC projection); see caveats.

## What each experiment shows

### §6-D Cross-VM MEC — the defensible headline (NEW)
Two **separate EC2 kernels** over the real ENA NIC. The scheduler dials a virtual
ClusterIP; one arm routes it via kube-proxy iptables DNAT behind N dummy services
(O(N)), the other via a `cgroup/connect4` O(1) vault rewrite. Result: connect4 stays
**flat at ~91 µs** across a 600× change in N, while kube-proxy climbs to **211 µs**.
The absolute floor (~99 µs) is higher than the veth version because it now includes
**real network RTT**, and the speed-up is correspondingly lower (2.55× vs 7.1×) — this
is honest: the fixed wire latency dilutes the routing-cost gap. The **O(1)-vs-O(N)
shape is confirmed on physical multi-node**, lifting the emulation caveat (§5.3).

### MEC (single-host) & Crossover — isolate the mechanism
Single-host veth (mec) and loopback (crossover) strip out wire latency to expose the
pure routing-decision cost: iptables chain-walk is linear (~2 ns/rule here), the
eBPF map lookup is constant. Crossover's 37× is the cleanest illustration of O(1) vs O(N).

### Cache fan-out — validates CachOf's core assumption, refutes app caches
Serving one producer's result to N DAG consumers: an **application** cache pays
~10 µs/consumer through the stack (O(N) downlink); the **kernel** eBPF cache pays
~0.55 µs/consumer (O(1) per access), payload-independent. This is the substrate that
makes CachOf's "cache hit ⇒ serve ≈ 0" physically near-true — an app cache misses it by ~18×.

### §6-B C3 fan-out retention cache — now runs on kernel 6.17 (NEW, TCX port)
The C3 retention bench (`cmd/c3_bench`) previously used classic `tc`/clsact, which
**fails on kernel ≥6.6** ("unknown qdisc") — so this result could not be reproduced.
I ported the attach path in `internal/ebpf/loader_linux.go` from clsact to **`link.AttachTCX`**
(modern, link-owned, no qdisc). The bench now runs and confirms all three C3 claims on
real hardware: **retention overhead ≈ 0.7–1.0 µs** over the plain routing path (cheap);
**per-access latency flat in fan-out degree** (N=2/3/4 within ~350 ns → O(1)); and
**deterministic GC 3000/3000** (atomic ref-count + delete-on-zero reclaims every entry).

### §6-B Path-2 — kernel-side fan-out DELIVERY (NEW, closes caveat #2)
Path-1 proved retention is cheap but *total* delivery to N consumers was still N
userspace re-serves. `internal/ebpf/c/fanout.c` (a SEC("tc") program attached to
`lo` ingress via TCX) makes the producer send **one** trigger packet; the kernel
then uses **`bpf_clone_redirect`** to clone it to N consumers (rewriting dest IP +
port, zeroing the UDP checksum, re-injecting on ingress). `cmd/path2_bench` verifies
**all N consumers actually receive** the payload while measuring producer-side cost:
app fan-out is linear (~1.8 µs/consumer → 29.4 µs @N=16), Path-2 is sub-linear
(13.1 µs @N=16) → **2.24× cheaper at N=16 with a single producer syscall** regardless
of N. (Requires `net.ipv4.conf.all.accept_local=1`, set automatically by the bench,
so re-injected clones with local source IPs are delivered.)

### EDP (§6-A) — energy-delay product vs the base-paper assumptions
`analyze_energy_edp.py` combines the measured offload + cache costs into a per-task
Energy-Delay Product across N, against three arms: baseline (kube-proxy+app cache),
eDAG-MEC (connect4+eBPF cache), and CachOf's assumed-free-hit reference. Delay is
measured-derived; energy is modeled (E ≈ P_active × active_time), with a projected
hardware-offload case. EDP improves up to **197× (software)** at N=60k.

## Honest caveats (carry into the defense)
1. **Substrate, not algorithm** — we replace the cache/offload data plane the base
   papers assume is free; we do not beat their DRL/priority decision logic. Story = *combine*.
2. **Per-access O(1), total fan-out still linear** — kernel cache is flat *per consumer*;
   true N-independent *total* delivery needs `bpf_clone_redirect` (§6-B, future work).
3. **Energy is modeled, not measured** — EC2 blocks RAPL `power/energy-pkg`. Latency/
   scalability are measured; energy/EDP is a clearly-labeled model + HW projection.
4. **iptables O(N) is per-flow** — fresh flows used so the walk is paid per task (matches
   kube-proxy on new connections); long-lived flows amortize via conntrack.

## Artifacts (in results_aws_nodeb/)
`mec_xnode_results.csv` + `mec_xnode_plot.png` (cross-VM, NEW) · `mec_results.csv`/`mec_plot.png`
· `crossover_results.csv`/`crossover_plot.png` · `cache_fanout.csv`/`cache_plot.png`
· `edp_results.csv`/`edp_plot.png` · `run.log`, `xnode.log` (full execution traces).

## Remaining backlog (not yet done)
- **§6-A full** — clone & run the CachOf Python sim, overlay our serve cost on its delay model.
- **§6-C** — Cilium (eBPF kube-proxy replacement) and DPLS-schedule baselines.

*(§6-B done: C3 on TCX + Path-2 kernel fan-out delivery.)*
