#!/usr/bin/env python3
"""
deadline_satisfaction.py — application-level MEC metric on top of the CachOf
cost model (advisor request: end-to-end DAG completion time + deadline
satisfaction, not just packet-level latency).

WHY THIS EXISTS
---------------
`energy_vs_basepaper.py` reproduces CachOf's per-subtask cost model and injects
our MEASURED data-plane fits, but it only keeps the *mean* makespan per cluster
size N. A MEC deployment cares about a deadline: what fraction of DAG requests
finish within a tolerance T_max. This script reuses that exact model (imported,
not re-derived) and, instead of averaging, keeps the full Monte-Carlo
distribution of per-workload makespans, so the deadline-satisfaction ratio is
the EMPIRICAL fraction of instances meeting T_max -- no distribution assumption.

  satisfaction(arm, N, T_max) = #{instances : makespan <= T_max} / #instances

Two views are emitted (mirroring CachOf Fig. 6c / DPLS success-rate figures):
  (a) satisfaction vs cluster size N, at a fixed deadline T_max
  (b) satisfaction vs latency tolerance T_max, at a fixed N

DELAY provenance is identical to energy_vs_basepaper.py: CachOf compute/transfer
model + our measured data-plane fits. This is the fine-grained (dense-edge)
regime (cn x 1e-3), the regime the thesis targets.

USAGE
-----
  python3 analysis/deadline_satisfaction.py                 # CSV + PNG (episodes=1000)
  python3 analysis/deadline_satisfaction.py --episodes 2000
  python3 analysis/deadline_satisfaction.py --no-plot
"""
import os, csv, random, argparse
import energy_vs_basepaper as E   # reuse the verbatim CachOf model + measured fits

ARMS = ("baseline", "edag")       # ideal omitted: it is the unreachable lower bound
CN_SCALE = 1e-3                   # dense-edge fine-grained regime (as in Table VIII)


def collect_makespans_ms(episodes, N):
    """Return {arm: [makespan_ms, ...]} — the full Monte-Carlo distribution at N.
    Reuses energy_vs_basepaper.makespan_and_energy verbatim; same seed stream per
    arm so all arms see identical random workloads (paired comparison)."""
    F = E.fanout_for_N(N)
    E._SWEEP_CTX["N"], E._SWEEP_CTX["F"] = N, F
    out = {a: [] for a in ARMS}
    for ep in range(episodes):
        for arm in ARMS:
            rng = random.Random((N << 20) ^ (ep << 4) ^ E._ARM_SEED[arm])
            ms_s, _, _ = E.makespan_and_energy(rng, arm, CN_SCALE)
            out[arm].append(ms_s * 1e3)   # seconds -> milliseconds
    return out


def satisfaction(ms_list, tmax_ms):
    return 100.0 * sum(1 for m in ms_list if m <= tmax_ms) / len(ms_list)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--episodes", type=int, default=1000)
    ap.add_argument("--no-plot", action="store_true")
    args = ap.parse_args()

    SWEEP_N = [100, 1000, 10000, 40000, 60000]
    T_FIXED = 10.0                              # 10 ms edge-inference deadline
    N_FIXED = 60000
    T_SWEEP = [round(0.5 * k, 1) for k in range(0, 61)]   # 0..30 ms

    # distributions per N (reused for both views)
    dist = {N: collect_makespans_ms(args.episodes, N) for N in SWEEP_N}

    # (a) satisfaction vs N at fixed deadline
    rows_vsN = []
    print(f"=== Deadline satisfaction vs N  (T_max = {T_FIXED} ms, "
          f"episodes = {args.episodes}) ===")
    for N in SWEEP_N:
        b = satisfaction(dist[N]["baseline"], T_FIXED)
        e = satisfaction(dist[N]["edag"], T_FIXED)
        mb = sum(dist[N]["baseline"]) / len(dist[N]["baseline"])
        me = sum(dist[N]["edag"]) / len(dist[N]["edag"])
        rows_vsN.append(dict(N=N, Tmax_ms=T_FIXED, baseline_sat=round(b, 1),
                             edag_sat=round(e, 1), base_mean_ms=round(mb, 2),
                             edag_mean_ms=round(me, 2)))
        print(f"  N={N:>6}  baseline={b:6.1f}%  eDAG={e:6.1f}%  "
              f"(mean makespan {mb:6.2f} / {me:5.2f} ms)")

    # (b) satisfaction vs T_max at fixed N
    distN = dist[N_FIXED]
    rows_vsT = [dict(Tmax_ms=t, N=N_FIXED,
                     baseline_sat=round(satisfaction(distN["baseline"], t), 1),
                     edag_sat=round(satisfaction(distN["edag"], t), 1))
                for t in T_SWEEP]

    with open(os.path.join(E.HERE, "deadline_satisfaction.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(rows_vsN[0].keys()))
        w.writeheader(); w.writerows(rows_vsN)
    print(f"\nwrote {os.path.join(E.HERE, 'deadline_satisfaction.csv')}")

    if args.no_plot:
        return
    try:
        import matplotlib; matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError:
        print("[matplotlib missing -> CSV written, plot skipped]"); return

    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(7.2, 3.0))
    ax1.plot([r["N"] for r in rows_vsN], [r["baseline_sat"] for r in rows_vsN],
             "o-", color="#b5432f", label="Baseline (kube-proxy+app)")
    ax1.plot([r["N"] for r in rows_vsN], [r["edag_sat"] for r in rows_vsN],
             "s-", color="#1f6f4f", label="eDAG-MEC")
    ax1.set_xscale("log"); ax1.set_ylim(-3, 103); ax1.grid(alpha=.3)
    ax1.set_xlabel("Cluster size $N$"); ax1.set_ylabel("Deadline satisfaction (%)")
    ax1.set_title(f"(a) vs. load ($T_{{\\max}}={int(T_FIXED)}$ ms)", fontsize=9)
    ax1.legend(fontsize=7, loc="center left")

    ax2.plot(T_SWEEP, [r["baseline_sat"] for r in rows_vsT], "-",
             color="#b5432f", label=f"Baseline @{N_FIXED//1000}k")
    ax2.plot(T_SWEEP, [r["edag_sat"] for r in rows_vsT], "-",
             color="#1f6f4f", label=f"eDAG-MEC @{N_FIXED//1000}k")
    ax2.set_ylim(-3, 103); ax2.grid(alpha=.3)
    ax2.set_xlabel("Latency tolerance $T_{\\max}$ (ms)")
    ax2.set_ylabel("Deadline satisfaction (%)")
    ax2.set_title(f"(b) vs. tolerance ($N{{=}}{N_FIXED//1000}$k)", fontsize=9)
    ax2.legend(fontsize=7, loc="lower right")
    fig.tight_layout()
    out = os.path.join(E.HERE, "deadline_satisfaction.png")
    fig.savefig(out, dpi=150, bbox_inches="tight")
    print(f"saved {out}")


if __name__ == "__main__":
    main()
