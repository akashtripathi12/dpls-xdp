# §6-A (FULL) — Energy / EDP vs the CachOf base paper, on CachOf's own cost model

> Closes the last open item of HANDOFF §6-A.1 and the RESULTS_AWS.md backlog
> ("clone & run the CachOf sim, overlay our serve cost on its delay model").
> Script: `energy_vs_basepaper.py` → `energy_vs_basepaper_{coarse,fine}.csv` +
> `energy_vs_basepaper_plot.png`. Reproduce: `python3 energy_vs_basepaper.py`.

## What this adds over `analyze_energy_edp.py`

`analyze_energy_edp.py` compared our measured data-plane against a *synthetic*
baseline. This analysis instead anchors the comparison in the **actual base
paper**: it reproduces **CachOf's published per-subtask cost model verbatim**
(their `ddpg/env.py` `step()`), with **their parameters taken directly from
`ddpg/run.py` and `ddpg/other.py`**, and then replaces the two costs CachOf
assumes away with our **measured** kernel-data-plane numbers.

CachOf's repo (github.com/NetworkCommunication/CachOf) was cloned and read. Its
two free-lunch assumptions, quoted from `ddpg/env.py`:

| CachOf assumption | Where | What it ignores |
|---|---|---|
| cache hit ⇒ `self.exet = 0` | `env.py:216` | the cost to **serve** a cached result to its DAG consumers |
| offload hop = `dn / rn` only | `env.py` transfer term | the **O(N) kube-proxy / iptables DNAT** routing tax |

> *Note:* CachOf's `ddpg/DAGs_level.py` (their subtask-priority module) was never
> committed upstream, and their `DAGs_Generator.py` breaks under modern numpy.
> Since neither affects the **cost magnitudes** (only task ordering and which
> popularity slice a task lands in), we reproduce their cost equations and task
> distributions exactly and use a standard topological/upward-rank order. All
> compute (`cn/fn_app`, `cn/fn_bs`), transmission (`dn/rn`), and caching-policy
> terms are theirs, verbatim.

### CachOf parameters used (verbatim)
`NUM_APP=8`, `NUM_TASK=7`, `fn_app=0.27e8 Hz`, `fn_bs=0.6e8 Hz`, `rn=5e8 bit/s`,
`delay_constraint=1.5 s`, `cache_capacity=7`, `cn∈[1e7, 5e7]` cycles,
`dn = 2·cn`.

## The three arms (all run under CachOf's model)

1. **CachOf-ideal** — the paper as written: serve cost = 0, offload = `dn/rn` only.
2. **baseline** — CachOf's *exact policy* on a **real** Linux stack:
   kube-proxy O(N) DNAT per offload + app-level cache O(N) serve per hit.
3. **eDAG-MEC** — CachOf's exact policy on **our** substrate:
   `cgroup/connect4` O(1) offload + eBPF kernel cache O(1) serve.

Measured data-plane fits injected (from our real benchmark CSVs):
- offload: kube-proxy **4.62 ns/svc·N + 48 µs floor** (O(N)) vs connect4 **39.4 µs flat** (O(1)) — `mec_results.csv`
- cache serve: app **11.67 µs/consumer** (O(N)) vs eBPF **0.52 µs/consumer** (O(1)) — `cache_fanout.csv`

Energy is **MODELED**: `E = P_active(15 W) × active_CPU_time`; a projected
SmartNIC case moves `HW_OFFLOAD_DATAPLANE_FRAC = 0.85` of the data-plane CPU
work off-host. `EDP = E × makespan`.

## Headline finding — two regimes, one honest story

### 1. CachOf's own coarse-grained workload: their assumption is SAFE
With CachOf's published `cn∈[1e7,5e7]` cycles, each subtask is **seconds-scale
compute** (`cn/fn_bs ≈ 0.5 s`); a per-hit serve of a few µs is ~1e-5 of that.
The reproduced per-workload makespan is **≈ 2.9 s**, essentially identical
across all three arms. **At their granularity, CachOf is right to set hit≈0 —
and our kernel cache makes that ~0 physically true rather than assumed.**

### 2. Dense-edge fine-grained regime: the substrate decides
The thesis targets **dense edge clusters running many small (microservice-grained)
subtasks** at high fan-out. Scaling `cn` down by 1e-3 (sub-ms subtasks), the
per-task **O(N) offload tax + O(N) app-cache serve dominate**, and the arms
diverge sharply:

| N (services) | F | makespan baseline | makespan eDAG | EDP improvement (sw) | EDP (proj. HW) |
|---:|---:|---:|---:|---:|---:|
| 100 | 1 | 3.48 ms | 2.91 ms | **14.98×** | 99.8× |
| 1 000 | 1 | 3.47 ms | 2.94 ms | **14.80×** | 98.7× |
| 10 000 | 10 | 6.83 ms | 3.10 ms | **44.41×** | 296× |
| 40 000 | 40 | 17.94 ms | 3.58 ms | **109.52×** | 730× |
| 60 000 | 60 | 25.28 ms | 3.96 ms | **141.22×** | 941× |

eDAG-MEC tracks **CachOf-ideal** (flat) while the real baseline climbs O(N):
the kernel substrate is what keeps CachOf's "free hit / cheap offload"
assumption physically true as the cluster scales. See
`energy_vs_basepaper_plot.png` (makespan · energy · EDP).

## Honest caveats (unchanged from the rest of the thesis)
1. **Substrate, not algorithm** — we run *CachOf's own offload+cache policy*;
   we do not beat their DRL decision quality. We replace the data plane they
   assume is free. Story = *combine*.
2. **Energy is MODELED** — EC2 blocks RAPL `power/energy-pkg`. Latency/makespan
   are measured-derived (real CSV fits); energy/EDP is a clearly-labelled
   `P_active × time` model + a SmartNIC projection.
3. **Fine-grained regime is the relevant one** — at CachOf's coarse subtask
   size the data-plane is negligible (we say so plainly); the win is in the
   dense, high-fan-out edge regime, which is exactly what eDAG-MEC targets.
4. **iptables O(N) is per-flow** — fresh flows assumed (matches kube-proxy on
   new connections); long-lived flows amortize via conntrack.
