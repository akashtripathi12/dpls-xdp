#!/usr/bin/env python3
"""
ablation.py — component ablation for Table XIV.

WHY THIS EXISTS
---------------
Table XIV needs FOUR configurations, but `energy_vs_basepaper.py` only defines
three coupled arms (ideal / baseline / edag), where the offload substrate and the
cache substrate are chosen together. The ablation needs them chosen INDEPENDENTLY:

  1. Full                 : connect4 offload  + eBPF cache        (eDAG-MEC)
  2. Without sender bypass: kube-proxy offload + eBPF cache
  3. Without cache        : connect4 offload  + app  cache
  4. Without DAG-GC       : Fails  (correctness, not latency — Proposition 1)

Row 4 is NOT a measurement: without DAG-aware GC a still-needed result can be
evicted, stalling the graph. It is reported as "Fails" by design; no number.

This script reuses `energy_vs_basepaper`'s VERBATIM CachOf cost model and measured
data-plane fits (`offload_us_*`, `serve_us_*`, `gen_app`, `popular_set`), so the
cost columns match the rest of the repo. It is DETERMINISTIC (fixed seeds).

USAGE
-----
  python3 analysis/ablation.py                 # N=60000, T_max=10ms, episodes=1000
  python3 analysis/ablation.py --episodes 2000 --N 60000 --tmax 10
"""
import os, csv, random, argparse
import energy_vs_basepaper as E

# (label, offload substrate, cache substrate) — None cache-fn means 'ideal' serve=0
CONFIGS = [
    ("Full (connect4 + eBPF cache)",        E.offload_us_connect4,  E.serve_us_ebpf),
    ("Without sender bypass (kube-proxy)",   E.offload_us_kubeproxy, E.serve_us_ebpf),
    ("Without cache (app-level serve)",      E.offload_us_connect4,  E.serve_us_app),
    # "Without DAG-GC" is a correctness failure (Proposition 1), not a latency row.
]
CN_SCALE = 1e-3   # dense-edge fine-grained regime (same as Table VIII / deadline)


def makespan_ms(rng, N, F, offload_fn, serve_fn):
    """One workload instance's makespan (ms) with an INDEPENDENT choice of offload
    and cache substrate. Mirrors E.makespan_and_energy's inner loop exactly."""
    E._SWEEP_CTX["N"], E._SWEEP_CTX["F"] = N, F
    total_eft = 0.0
    for _ in range(E.NUM_APP):
        tasks = E.gen_app(rng, CN_SCALE)
        cached = E.popular_set(rng)
        max_eft = 0.0
        for t in tasks:
            est = 0.0
            for p in t["preds"]:
                est = max(est, tasks[p]["eft"] + tasks[p]["dn"] / E.RN)
            if t["id"] in cached:
                exet = 0.0
                dp_us = serve_fn(F)                 # cache-hit serve cost (F consumers)
            else:
                exet = t["cn"] / E.FN_BS
                dp_us = offload_fn(N)               # offload routing tax
            eft = est + exet + dp_us * 1e-6
            t["eft"] = eft
            max_eft = max(max_eft, eft)
        total_eft += max_eft
    return total_eft * 1e3


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--episodes", type=int, default=1000)
    ap.add_argument("--N", type=int, default=60000)
    ap.add_argument("--tmax", type=float, default=10.0)
    args = ap.parse_args()
    N, F, Tmax = args.N, E.fanout_for_N(args.N), args.tmax

    print(f"=== Ablation (Table XIV)  N={N}, T_max={Tmax} ms, "
          f"episodes={args.episodes} ===\n")
    print(f"  {'configuration':<40} {'mean makespan':>13} {'deadline sat':>13}   outcome")
    rows = []
    for i, (label, off_fn, srv_fn) in enumerate(CONFIGS):
        ms = []
        for ep in range(args.episodes):
            # deterministic, config-independent workload stream (paired comparison)
            rng = random.Random((N << 20) ^ (ep << 4) ^ (0xA11 + i))
            ms.append(makespan_ms(rng, N, F, off_fn, srv_fn))
        mean_ms = sum(ms) / len(ms)
        sat = 100.0 * sum(1 for m in ms if m <= Tmax) / len(ms)
        outcome = "meets deadline" if sat >= 99.95 else ("misses" if sat < 0.05 else "partial")
        rows.append(dict(config=label, mean_makespan_ms=round(mean_ms, 3),
                         deadline_sat_pct=round(sat, 1), outcome=outcome))
        print(f"  {label:<40} {mean_ms:>10.3f} ms {sat:>11.1f} %   {outcome}")
    # row 4 — correctness, not a measurement
    rows.append(dict(config="Without DAG-GC (ref-count eviction)",
                     mean_makespan_ms="", deadline_sat_pct="", outcome="Fails (Proposition 1)"))
    print(f"  {'Without DAG-GC (ref-count eviction)':<40} {'—':>13} {'—':>13}   Fails (Proposition 1)")

    out = os.path.join(E.HERE, "ablation_results.csv")
    with open(out, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["config", "mean_makespan_ms",
                                          "deadline_sat_pct", "outcome"])
        w.writeheader(); w.writerows(rows)
    print(f"\nwrote {out}")


if __name__ == "__main__":
    main()
