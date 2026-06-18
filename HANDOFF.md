# HANDOFF — eDAG-MEC eBPF Thesis: State, How-To, and Remaining Work

> **Audience:** a future Claude Code (or human) continuing this thesis project.
> **Read this first**, then `comprehensive_experiments.md` (older energy/C3 work) and the
> three new experiment write-ups (`crossover_experiment.md`, `mec_multinode_experiment.md`,
> `cache_vs_basepapers.md`). This file is the single source of truth for *what was done, why,
> what the environment supports, and what is left to do.*

---

## 1. The thesis in one paragraph

**eDAG-MEC**: accelerate dependency-aware (DAG) task offloading in Mobile Edge Computing by
moving the **routing and caching data plane into the Linux kernel via eBPF**. Two base papers
define the problem; this work builds the *real kernel substrate* underneath their *simulated*
control logic:

- **Sender side** — `cgroup/connect4` rewrites a connection's destination (O(1) `vault` map
  lookup) **before netfilter**, bypassing kube-proxy's O(N) iptables DNAT + conntrack.
- **Receiver side** — a `tc`/TCX program retains a producer subtask's output in a kernel hash
  map (`retention_map`) and serves it to multiple DAG consumers with **ref-counted
  deterministic GC** (the C3 "fan-out cache").

The central claim: routing/cache cost becomes **O(1) in cluster size and fan-out** instead of
O(N), which is the regime dense edge clusters actually operate in.

---

## 2. Base papers (the baselines to beat)

| | **CachOf** (IEEE Trans. Computers 2025) | **DPLS** (IEEE IoT-J 2026) |
|---|---|---|
| Topic | Dynamic **caching** of subtask results to assist offloading | Online **multi-DAG scheduling** (subtask order) |
| Method | popularity analysis → 0-1 knapsack → **DDPG (DRL)** | dynamic priority list: upward/downward rank, volume, contention |
| Key cost model | **cache hit ⇒ `texe = 0`, downlink ignored** (their Eq. 5) | offload hop assumed cheap |
| Implementation | **Python 3.7 simulation** (github.com/NetworkCommunication/CachOf) | simulation |
| Metrics | Total Reward, Average Delay, Success Rate (vs episodes / delay constraint / #tasks) | mean & max completion time, server load balance |
| PDFs | stored locally, **gitignored** (IEEE copyright) — do not commit | same |

**Our novelty vs them:** we do **not** replace their DRL/priority decision logic. We replace
the **substrate they assume is free**: a kernel-resident, *dependency-aware* cache that makes
"hit serve ≈ 0" physically true and O(1), and a sender-side bypass that makes the offload hop
O(1) in cluster size. Frame every comparison as **"they decide policy in simulation; we provide
the real O(1) data plane and measure what they assumed."**

---

## 3. Environment & kernel capabilities (CRITICAL — saves hours)

Verified on the dev kernel used for these experiments (**Linux 6.18**, root, `cilium/ebpf`
v0.13.2, Go 1.24). What works and what does NOT in this container/kernel:

| Capability | Status | Notes |
|---|---|---|
| `iptables` / netfilter (filter + nat/DNAT/conntrack) | ✅ works | used by crossover + mec benches |
| BPF map create + program load | ✅ works | |
| **XDP attach** (generic, on `lo`/veth) | ✅ works | crossover bench uses `link.AttachXDP(... XDPGenericMode)` |
| **`cgroup/connect4` attach** | ✅ works **only on a cgroup v2 mount** | host root `/sys/fs/cgroup` is **cgroup v1** → attach fails `bad file descriptor`. Fix: `mount -t cgroup2 none /tmp/cg2 && mkdir /tmp/cg2/dpls`, attach there, and put the sender PID in `/tmp/cg2/dpls/cgroup.procs`. |
| **TCX** (modern tc, `link.AttachTCX`) | ✅ works | kernel ≥ 6.6. Use this for receiver-side, NOT classic tc. |
| classic `tc` **clsact qdisc** | ❌ FAILS | `tc qdisc add dev lo clsact` → "unknown qdisc". The legacy `cmd/c3_bench` uses `AttachTC`(clsact) and will NOT run here — port it to TCX. |
| network namespaces + veth | ✅ works | real multi-node emulation; `ping` binary may be absent — use UDP probes. |

**Toolchain install (Ubuntu/noble) that was needed:**
```bash
apt-get update
apt-get install -y libbpf-dev libelf-dev iproute2 poppler-utils
pip3 install matplotlib numpy        # for plotting
# clang/llvm/go already present
```

**Compile the eBPF objects** (regenerate `*.o`; they are gitignored):
```bash
ARCH=$(uname -m)
clang -target bpf -O2 -g -I /usr/include/${ARCH}-linux-gnu -c internal/ebpf/c/connect4.c   -o internal/ebpf/c/connect4.o
clang -target bpf -O2 -g -I /usr/include/${ARCH}-linux-gnu -c internal/ebpf/c/xdp_lookup.c -o internal/ebpf/c/xdp_lookup.o
clang -target bpf -O2 -g -I /usr/include/${ARCH}-linux-gnu -c internal/ebpf/c/tc_bridge.c  -o internal/ebpf/c/tc_bridge.o
```

---

## 4. What was built (this work) — files, how to run, results

All benchmarks live under `cmd/`. PNGs/CSVs are committed; `*.o` are regenerated (see above).
Run everything as **root**.

### 4.1 O(N) crossover — `cmd/crossover_bench` → `crossover_plot.png`
Isolates the **routing-decision cost** vs cluster size N: real iptables chain walk (O(N)) vs a
single XDP hash-map lookup (O(1)) on `lo`.
```bash
go build -o /tmp/crossover ./cmd/crossover_bench
/tmp/crossover --pings 3000 --csv crossover_results.csv
python3 plot_crossover.py
```
**Result:** iptables ≈ **8.8 ns/rule** (O(N)); XDP flat **~5.6 µs** (O(1)); **63× at N=40k**.
*Caveat:* loopback + XDP-on-lo is a micro-isolation, not the real architecture (see 4.2).

### 4.2 Multi-node offload — `cmd/mec_bench` → `mec_plot.png` (the REAL architecture)
Emulated 2-node cluster (root netns ↔ `mec-worker` netns over veth, distinct IPs, real
DNAT/conntrack). Real `cgroup/connect4` sender bypass vs kube-proxy iptables DNAT, sweeping N
services. Self-contained (sets up/tears down topology; `--role worker` is the echo node).
```bash
mount -t cgroup2 none /tmp/cg2 2>/dev/null; mkdir -p /tmp/cg2/dpls
go build -o /tmp/mec ./cmd/mec_bench
/tmp/mec --pings 3000 --csv mec_results.csv
python3 plot_mec.py
```
**Result:** connect4 flat **~40 µs (O(1))**; kube-proxy **4.6 ns/svc + 48 µs DNAT floor (O(N))**,
reaching **301 µs at N=60k → 7.4×**, gap widening. This is the headline, defensible result.

### 4.3 Cache vs CachOf — `cmd/cache_bench` → `cache_plot.png`
Targets CachOf's "free cache hit." Serve a producer's D-byte result to **N DAG consumers**:
app-level cache (userspace store + per-consumer delivery over the stack = real O(N) downlink)
vs eBPF kernel cache (store once + O(1) kernel access per consumer + GC), with CachOf's
assumed-0 as a reference line.
```bash
go build -o /tmp/cache ./cmd/cache_bench
/tmp/cache --reps 2000
python3 plot_cache.py
```
**Result:** app ≈ **11.7 µs/consumer**, kernel ≈ **0.5 µs/consumer** → **22× at N=64**, ~17×
and payload-independent. Validates CachOf's assumption while showing an *app* cache violates it.

### 4.4 Pre-existing work (older commits, see `comprehensive_experiments.md`)
- **C3 fan-out retention** (`cmd/c3_bench`, `internal/ebpf/c/tc_bridge.c`): store overhead ≈ 0,
  O(1) fan-out per access, deterministic GC 3000/3000. **Uses classic tc/clsact → must be
  ported to TCX to run on this kernel** (see §3).
- **Energy / EDP proxy** (perf `task-clock`, see commits `04425ec`, `62f49f5`, `7c46f99`):
  software eBPF on loopback was **energy-neutral-to-negative** (+13.8% kernel sys time). The
  conclusion: energy win requires **hardware offload (XDP on SmartNIC)**. This is a real,
  honest limitation that bounds the energy claims.

---

## 5. Honest scope (carry these caveats into the defense — do NOT overclaim)

1. **Substrate, not algorithm.** We don't beat CachOf's DRL accuracy or DPLS's priority
   quality — we replace the cache/offload substrate beneath them. The right story is *combine*.
2. **Per-access O(1), total fan-out still linear.** The kernel cache is ~0.5 µs *per consumer
   access* (flat in N), so total = N × constant. True N-independent *total* delivery needs
   kernel-side `bpf_clone_redirect`/`bpf_redirect` (the C3 "Path 2", still future work).
3. **Emulated, not physical, multi-node.** netns+veth = real separate stacks on one host/CPU →
   isolates stack/routing cost, not NIC/wire. The O(1)-vs-O(N) *shape* is algorithmic and
   carries over; absolute numbers will differ on real hardware.
4. **Latency/scalability axis only.** The **energy** advantage is NOT demonstrated in software
   (see §4.4) and requires hardware offload.
5. **iptables O(N) is per-flow in reality.** We deliberately use fresh flows so the walk is paid
   per task (matches kube-proxy on new connections); with long-lived flows conntrack amortizes
   it. State this so an examiner can't ambush you.

---

## 6. REMAINING WORK (prioritized backlog for the next session)

### A. Energy comparison against the base papers ⭐ — ✅ DONE (see `energy_vs_basepaper.md`)
> Implemented in `energy_vs_basepaper.py`: reproduces CachOf's `ddpg/env.py` cost
> model with their verbatim parameters, replaces their `exet=0` cache hit and
> cheap-offload assumptions with our measured eBPF/connect4 costs, and emits
> delay+energy+EDP across N for CachOf-ideal vs baseline vs eDAG-MEC
> (`energy_vs_basepaper_plot.png`). Coarse regime: assumption safe; fine-grained
> dense-edge regime: EDP 15×→141× (sw), up to ~941× (projected HW). The
> sub-items below are the original plan, now satisfied:
The base papers report *delay/reward/success-rate*, not real energy. Our existing energy proxy
(`task-clock`) only compares eBPF-vs-iptables in software. **To do:**
1. **CachOf-style energy/delay reproduction:** clone `github.com/NetworkCommunication/CachOf`
   (Python sim), run it to get their Average-Delay / Success-Rate curves, and place our measured
   kernel-cache serve costs into the *same delay model* (replace their `texe=0`-on-hit and
   ignored-downlink with our **measured** ~0.5 µs/consumer serve + real downlink). Show
   end-to-end DAG makespan with a *realistic* (non-zero) cache cost — ours still wins because the
   substrate is 20× cheaper than an app cache.
2. **Hardware-offload energy estimate:** since real RAPL/PMU is blocked in cloud VMs (see §4.4),
   either (a) run the energy proxy on bare metal / a VM that exposes `perf` RAPL, or (b) model
   per-packet Joules from `task-clock` × measured package power, and project the SmartNIC-offload
   case. Clearly label measured vs projected.
3. **EDP (Energy-Delay Product) head-to-head table:** baseline (kube-proxy/app-cache) vs
   eDAG-MEC, for both software and projected-hardware, across N. This is the figure the user
   asked for ("energy check from baseline paper").

### B. Close the per-consumer→total O(1) gap (Path 2)
Implement receiver-side **TCX** (not clsact) fan-out delivery using `bpf_clone_redirect` so one
retained copy is delivered to N consumers from the kernel without N userspace re-serves. Then
re-run §4.3 to show **total** delivery flat in N. Start by porting `cmd/c3_bench`'s `AttachTC`
to `link.AttachTCX(... AttachTCXIngress)`.

### C. More/stronger baselines
- **DPLS scheduling baseline:** the repo's scheduler is named DPLS; wire `cmd/mec_bench`'s
  offload path under an actual DPLS priority schedule vs a naive FCFS to show scheduling × data
  plane interaction. Compare mean/max completion time (DPLS paper's metrics).
- **Cilium / eBPF-kube-proxy-replacement** as a *third* line in §4.2 (kube-proxy vs Cilium vs
  our connect4) — positions us against the state of the art, not just legacy iptables.
- **Service-mesh sidecar (Envoy/iptables-redirect)** as an upper-bound-cost baseline for the
  cache/offload path.

### D. Realism upgrades
- **Physical / cross-VM multi-node** run of `cmd/mec_bench` (two hosts, real NIC) to lift the
  caveat in §5.3 — report absolute RTT and confirm the O(1)/O(N) shape holds.
- **Payload scaling to MTU+ (fragmentation)** and **concurrent multi-tenant load** in the cache
  bench, matching CachOf's 0.8–1.2 MB subtask data sizes (note: current cache cap is 1408 B —
  raise `valCap` and chunk, or store an offset/handle).

### E. Hygiene
- Port `c3_bench` to TCX so the whole suite runs on one kernel.
- Add a `Makefile` / `make bench` that compiles the `.o`s, builds all four benches, runs them,
  and regenerates all PNGs. Add a top-level `README` linking the four experiment docs.
- CI is on **PR #1** (`github.com/akashtripathi12/dpls-xdp/pull/1`). Push to branch
  `claude/affectionate-gauss-bfcb5z` to update it; do **not** open a new PR.

---

## 7. Repo map (new artifacts from this work)

```
cmd/crossover_bench/main.go     O(N) crossover (iptables vs XDP)         -> crossover_*.{csv,png}, plot_crossover.py
cmd/mec_bench/main.go           multi-node connect4 vs kube-proxy        -> mec_results.csv, mec_plot.png, plot_mec.py
cmd/cache_bench/main.go         eBPF cache vs app cache (CachOf)         -> cache_*.csv, cache_plot.png, plot_cache.py
internal/ebpf/c/connect4.c      sender-side connect4 dest-rewrite (vault)
internal/ebpf/c/xdp_lookup.c    one-lookup XDP probe (O(1))
internal/ebpf/c/tc_bridge.c     (pre-existing) C3 retention data plane — classic tc, port to TCX
crossover_experiment.md         write-up + honest scope
mec_multinode_experiment.md     write-up + honest scope
cache_vs_basepapers.md          write-up tying all 3 to the base papers
comprehensive_experiments.md    (pre-existing) energy/EDP + C3 results
HANDOFF.md                      <-- this file
```

**Note on `.gitignore`:** it ignores `*.md` except `comprehensive_experiments.md`, and ignores
`*.o`. New `.md` files must be added with `git add -f`. Commit author email is
`noreply@anthropic.com`. The stop-hook "Unverified" warning is only about GPG signing (not
configured in this env) and is harmless.
```
