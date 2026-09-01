# eDAG-MEC — A Kernel eBPF Data Plane for Dependency-Aware MEC Task Offloading

eDAG-MEC accelerates **dependency-aware (DAG) task offloading** in Mobile Edge
Computing by moving the **routing and caching data plane into the Linux kernel
with eBPF**. Two base papers (CachOf, DPLS) decide *policy* in simulation and
**assume** the data plane is free — a cache hit costs zero, the offload hop is
cheap. This project builds the **real kernel substrate** beneath them and
**measures**, on a real AWS cluster, that those assumptions can be made
physically true and **O(1) in cluster size and fan-out** where the conventional
Linux/kube-proxy path is **O(N)**.

- **Sender side** — a `cgroup/connect4` program rewrites a connection's
  destination via a single O(1) map lookup **before** netfilter, bypassing
  kube-proxy's O(N) iptables DNAT + conntrack.
- **Receiver side** — a `tc`/TCX program retains a producer subtask's output in
  a kernel hash map and serves it to its DAG consumers without re-sending it
  through the full stack per consumer.

## Contribution & Novelty

This is a **systems contribution**: we did not invent a new offloading or caching
*algorithm*, and we do not claim to beat the base papers' decision logic. We
**re-purpose existing kernel mechanisms in a new arrangement** for a problem they
were not built for — dependency-aware MEC offloading — and we add **one new
policy**. A dedicated prior-art search (against Cilium, BMC, Electrode, cache_ext,
and the MEC literature) returned a verdict of **"partially anticipated,"** which
maps cleanly onto the three parts of the system:

| Part | Mechanism | Prior art? | Our contribution |
|---|---|---|---|
| **1. Sender bypass** (`cgroup/connect4` O(1) rewrite) | exists | **Already done** — Cilium/Calico use it for kube-proxy replacement | we apply + confirm it for the MEC offload hop |
| **2. Receiver retention + kernel fan-out** (`tc`/TCX map + `bpf_clone_redirect`) | exists | **Partially anticipated** — BMC (in-kernel cache), Electrode (clone-redirect) | first use as an *intermediate-result retention cache for dependent (DAG) subtasks* |
| **3. DAG-aware deterministic GC** | **new policy** | **Appears novel** — no precedent found | the genuine contribution (below) |

**The novel element — DAG-aware deterministic garbage collection.** Generic
in-kernel caches evict by least-recently-used or time-to-live. In a dependency
graph that is not merely suboptimal but **incorrect**: evicting a result a
downstream consumer still needs stalls or breaks the execution. Instead, each
cached entry carries a **reference count equal to its number of DAG consumers**;
the count is decremented atomically on each consumer read and the entry is deleted
the instant it reaches zero. This turns cache eviction from a *performance
heuristic* into a **correctness guarantee driven by the task topology** — measured
as deterministic GC reclaiming **3000/3000** entries with **no leaks and no
premature eviction**, all within the loop-bounded eBPF verifier.

Beyond the policy, the work also delivers the **first physical eBPF substrate** for
the CachOf/DPLS model: those papers *simulate* and assume the data plane is free;
we run it on real kernels and **measure** the gap they assumed away (§ Results).

**The single sharpest objection** is "BMC already proved eBPF can be a kernel
cache — you just swapped its LRU for a ref-count." The defense: BMC is a
*stateless* cache where a wrong eviction merely triggers a re-fetch; here a wrong
eviction is a *correctness failure*, so the DAG-driven, race-free, leak-free
lifecycle of variable-length payloads under concurrent reads is the substantive
work, not the atomic decrement itself.

> Honest scope: **substrate, not algorithm** — we run CachOf's *own* offload+cache
> policy on our data plane; we do not beat its DRL decision quality. The right
> story is *combine*. See [docs/](docs/) for full caveats.

## Results (measured on a real 3-node AWS EC2 cluster)

Kernel 6.17-aws, `c7i-flex.large`, cgroup v2. All latency/scalability numbers are
**measured**; energy is **modeled** and labelled as such.

| Experiment | Baseline (O(N)) | eDAG-MEC (O(1)) | Speed-up @ max N | Figure |
|---|---|---|---|---|
| **Cross-VM MEC** (real 2-node) | kube-proxy → 211 µs @60k | connect4 **flat 91 µs** | **2.55×** | `results/mec_xnode_plot.png` |
| **MEC** (single-host veth) | kube-proxy → 151 µs @60k | connect4 **flat ~21 µs** | **7.1×** | `results/mec_plot.png` |
| **Crossover** (routing cost) | iptables → 205 µs @40k | XDP **flat ~5.4 µs** | **~37×** | `results/crossover_plot.png` |
| **Cache fan-out** vs CachOf | app **~10 µs/consumer** | eBPF **~0.55 µs/consumer** | **~18× @64** | `results/cache_plot.png` |
| **C3 retention** (TCX) | routing-only 9.4 µs | store 10.1 µs (+0.7) | O(1) fan-out; **GC 3000/3000** | `results/c3_results.txt` |
| **Path-2 kernel fan-out** | app 29.4 µs @16 | clone_redirect 13.1 µs | **2.24× @16** | `results/path2_plot.png` |
| **EDP** (energy×delay) | kube-proxy + app cache | connect4 + eBPF cache | **197× sw / 1316× HW\*** @60k | `results/edp_plot.png` |
| **Energy vs CachOf model** | CachOf-on-real-stack | eDAG-MEC | **101× sw / 677× HW\*** @60k | `results/energy_vs_basepaper_plot.png` |

\* Hardware-offload (SmartNIC) energy is *modeled/projected*; EC2 blocks RAPL.
See [docs/RESULTS_AWS.md](docs/RESULTS_AWS.md) and
[docs/energy_vs_basepaper.md](docs/energy_vs_basepaper.md).

## Repository layout

```
cmd/                 Go benchmark binaries (one per experiment)
internal/            scheduler, graph engine, eBPF loader, and C programs:
  ebpf/c/connect4.c    sender-side connect4 destination rewrite (vault)
  ebpf/c/xdp_lookup.c  one-lookup XDP probe (O(1) routing)
  ebpf/c/tc_bridge.c   C3 retention cache + DAG-aware ref-counted GC
  ebpf/c/fanout.c      Path-2 bpf_clone_redirect kernel fan-out
analysis/            Python: plot_*.py + analyze_energy_edp.py + energy_vs_basepaper.py
scripts/             run_all.sh (single-host), run_xnode.sh (cross-VM), bootstrap_run.sh
results/             canonical AWS-hardware CSVs + PNGs   (dev-kernel runs in results/dev-kernel/)
docs/                experiment write-ups, RESULTS_AWS.md, HANDOFF.md, BTP.pdf
```

## Quickstart

**Prerequisites** (Ubuntu 24.04, root; kernel ≥ 6.6 for TCX, cgroup v2):
```bash
sudo apt-get install -y clang llvm libbpf-dev libelf-dev iproute2 poppler-utils
pip3 install matplotlib numpy            # Go 1.22+, clang 18 assumed present
```

**Reproduce everything:**
```bash
make ebpf            # compile the eBPF objects (*.o)
make build           # build all benches into ./bin
sudo make bench      # run the full single-host suite -> results/
make xnode IP=<worker_ip>   # (optional) cross-VM run on a 2-node cluster
```

**Just regenerate the figures / energy analysis** from the committed AWS CSVs
(no root, no kernel needed):
```bash
make analysis        # rewrites every results/*.png + edp/energy CSVs
```

See `make help` for individual targets.

## Documentation

- **[docs/FULL_REPORT.md](docs/FULL_REPORT.md) — single complete report** (all experiments, graphs, reproduction, caveats).
- [docs/RESULTS_AWS.md](docs/RESULTS_AWS.md) — final AWS-hardware results, every experiment.
- [docs/energy_vs_basepaper.md](docs/energy_vs_basepaper.md) — energy/EDP on CachOf's *own* cost model.
- [docs/comprehensive_experiments.md](docs/comprehensive_experiments.md) — energy proxy + C3 deep dive.
- [docs/crossover_experiment.md](docs/crossover_experiment.md) · [docs/mec_multinode_experiment.md](docs/mec_multinode_experiment.md) · [docs/cache_vs_basepapers.md](docs/cache_vs_basepapers.md) · [docs/scalability_benchmarks.md](docs/scalability_benchmarks.md)
- [docs/HANDOFF.md](docs/HANDOFF.md) — project state, environment notes, remaining backlog.
- [docs/RECOVERY_STATUS.md](docs/RECOVERY_STATUS.md) — artifact-recovery ledger (what's recovered / reconstructed / unrecoverable).
- [docs/C3_BENCHMARK_RUNBOOK.md](docs/C3_BENCHMARK_RUNBOOK.md) · [docs/ENERGY_BENCHMARK_RUNBOOK.md](docs/ENERGY_BENCHMARK_RUNBOOK.md) — how to run the C3/GC and energy benches.
