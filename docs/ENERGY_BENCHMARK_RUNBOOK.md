# Energy Benchmark Runbook — Modeled EDP + CPU-Proxy Measurement

> **Provenance.** The original `ENERGY_BENCHMARK_RUNBOOK.md` existed only on the
> author's local Windows machine and was never committed; it could not be recovered
> from the repository. This runbook is **reconstructed** from the committed analysis
> scripts and documentation. It has two parts: (1) the **modeled energy/EDP**
> pipeline, which is fully in the repo and reproducible; and (2) the **CPU-proxy
> `perf` measurement**, whose driver and raw counters were produced in an
> SSH/AWS session that is no longer accessible — that raw capture is
> **UNRECOVERABLE**; only its methodology and reported numbers are recorded here.

---

## Why energy is modeled, not measured directly

AWS EC2 blocks the RAPL `power/energy-pkg` MSR, so watt-level energy cannot be read
on the testbed. Two honest substitutes are used:

1. **Modeled EDP** — `E = P_active(15 W) × active_CPU_time`, with a projected
   SmartNIC/hardware-offload case that moves `HW_OFFLOAD_DATAPLANE_FRAC = 0.85` of
   the data-plane CPU work off-host. `EDP = E × makespan`. Delay/makespan are
   measured-derived (real CSV fits); the energy scalar is the only modeled input.
2. **CPU-proxy** — `perf stat` `task-clock` and kernel `sys` time as a proxy for
   relative CPU energy between the eBPF and baseline arms (Part 2 below).

---

## Part 1 — Modeled energy / EDP  (fully reproducible, no root)

```bash
make analysis      # runs the two scripts below (plus the plots)
# or individually:
python3 analysis/analyze_energy_edp.py     # -> results/edp_results.csv, edp_plot.png
python3 analysis/energy_vs_basepaper.py    # -> results/energy_vs_basepaper_*.{csv,png}
python3 analysis/deadline_satisfaction.py  # -> results/deadline_satisfaction.{csv,png}
```

- `analyze_energy_edp.py` — combines the measured offload + cache costs into a
  per-task EDP across N. Headline: **197× (sw) / 1316× (HW*)** at N=60k. Deterministic.
- `energy_vs_basepaper.py` — reproduces CachOf's `ddpg/env.py` cost model verbatim
  and overlays our measured data-plane costs. Headline (fine-grained regime):
  **7× → ~100× (sw), up to ~680× (HW*)** at N=60k.
  - **Determinism:** the Monte-Carlo stream is seeded from a fixed `_ARM_SEED` map
    (not `hash(arm)`), so it is stable across processes and Python versions.
  - **Provenance:** the committed `results/energy_vs_basepaper_fine.csv` (EDP@60k =
    **102.03×**, the paper's printed value) is from an earlier process-salted run,
    preserved intentionally. A fresh deterministic run yields **100.48×** (~1.5%
    lower, inside the run-to-run spread; the "~100×" claim is unchanged).
- `deadline_satisfaction.py` — deadline-satisfaction ratio vs N at `T_max = 10 ms`
  (Table XIII). Deterministic; reproduces **100/100/100/0.0/0.0** exactly.

\* Hardware-offload energy is projected/modeled.

---

## Part 2 — CPU-proxy `perf` measurement  (raw capture UNRECOVERABLE)

**Status:** the loopback driver and the raw `perf` counters were generated in an
SSH/AWS session that no longer exists. The `perf stat` invocation, event list,
system-wide-vs-per-process flag, and repetition count were **not committed** and
**cannot be recovered**. Only the reported results survive:

| metric | baseline | eDAG (eBPF) | delta |
|---|---|---|---|
| elapsed | 70.81 s | 71.78 s | — |
| CPU task-clock | 66.75 s | 66.83 s | **+0.11%** |
| kernel sys time | 0.419 s | 0.477 s | **+13.8%** |

**Honest reading (the paper's negative result):** in *software* on loopback the
eBPF path is energy-neutral-to-slightly-negative — the +13.8% kernel `sys` time is
the cost of the extra in-kernel work. The energy *win* genuinely requires hardware
offload (Part 1's projected case), and the report states this plainly.

**To regenerate equivalent data** (methodology reconstruction — exact original
flags unknown), the shape was: a ~4000-packet loopback driver exercising each arm,
wrapped in `perf stat` capturing `task-clock` and `context-switches`, e.g.:

```bash
# NOTE: reconstructed shape, not the original invocation (which is unrecoverable)
perf stat -r 5 -e task-clock,context-switches,cpu-clock \
    -- <loopback-driver> --packets 4000 --arm {baseline|ebpf}
```

If this measurement is re-run for the viva, commit the exact command and the raw
`perf` output alongside the numbers so it is reproducible next time.
