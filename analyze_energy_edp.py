#!/usr/bin/env python3
"""
analyze_energy_edp.py — HANDOFF §6-A deliverable: Energy / EDP head-to-head.

WHAT THIS DOES
--------------
The base papers (CachOf, DPLS) report delay/reward/success-rate, never real
energy, and CachOf assumes a cache hit costs ZERO (texe=0, downlink ignored).
This script turns our REAL measured kernel-data-plane costs into an
Energy-Delay-Product (EDP) comparison across cluster size N, for three arms:

  1. baseline      = kube-proxy O(N) offload  +  app-level cache (real O(N) serve)
  2. eDAG-MEC      = connect4 O(1) offload     +  eBPF kernel cache (O(1) serve)
  3. CachOf-ideal  = connect4 O(1) offload     +  ZERO cache cost (their assumption)

DELAY is built from MEASURED data (mec_results.csv, cache_fanout.csv) fitted to
their O(N)/O(1) models. ENERGY is MODELED: software network path is CPU-bound,
so E ≈ P_active × delay; the hardware-offload projection moves the eBPF
data-plane work off the CPU. EDP = E × delay.

Everything MEASURED vs MODELED is labelled. Tune the two model knobs below.

USAGE
-----
  python3 analyze_energy_edp.py            # writes edp_results.csv + edp_plot.png
  python3 analyze_energy_edp.py --no-plot  # CSV only (no matplotlib needed)
"""
import csv, argparse, math, os

# ----------------------------------------------------------------------------
# MODEL KNOBS (the only assumptions; everything else is measured) -------------
# ----------------------------------------------------------------------------
# Representative active package power while the network/data-plane path runs.
# Measure on your AWS box with:  perf stat -a -e power/energy-pkg/ -- sleep 10
# then divide Joules/sec. Default is a placeholder for a single busy core.
P_ACTIVE_W = 15.0          # watts, active package power (MODELED — replace w/ RAPL)

# Fraction of the eBPF data-plane delay whose CPU work would move to a SmartNIC
# under hardware XDP offload (HANDOFF §4.4 / §5.4: energy win needs offload).
HW_OFFLOAD_DATAPLANE_FRAC = 0.85   # 85% of dp CPU cycles leave the host (MODELED)

# How many DAG consumers a produced subtask result is served to (cache fan-out)
# is tied to cluster size in dense edge clusters; here fan-out F = scaled N.
def fanout_for_N(N):       # consumers per produced result at cluster size N
    return max(1, min(64, N // 1000))   # 1..64, matches cache_fanout.csv range


# ----------------------------------------------------------------------------
# Load MEASURED data and fit the O(N)/O(1) models ----------------------------
# ----------------------------------------------------------------------------
HERE = os.path.dirname(os.path.abspath(__file__))

def _read_csv(name):
    with open(os.path.join(HERE, name)) as f:
        return list(csv.DictReader(f))

def fit_line(xs, ys):
    """least-squares y = m*x + b -> (m, b)"""
    n = len(xs); sx = sum(xs); sy = sum(ys)
    sxx = sum(x*x for x in xs); sxy = sum(x*y for x, y in zip(xs, ys))
    m = (n*sxy - sx*sy) / (n*sxx - sx*sx)
    b = (sy - m*sx) / n
    return m, b

# --- offload hop: mec_results.csv (RTT in µs vs cluster size N) ---
mec = _read_csv("mec_results.csv")
mec_N   = [int(r["N"])               for r in mec]
mec_kp  = [float(r["kubeproxy_us"])  for r in mec]
mec_e4  = [float(r["ebpf_connect4_us"]) for r in mec]
kp_m, kp_b = fit_line(mec_N, mec_kp)        # kube-proxy O(N): m µs/svc + b floor
e4_flat    = sum(mec_e4) / len(mec_e4)      # connect4 O(1): flat mean

# --- cache serve: cache_fanout.csv (µs to serve result to M consumers) ---
cf = _read_csv("cache_fanout.csv")
cf_M   = [int(r["N"])             for r in cf]
cf_app = [float(r["app_cache_us"])  for r in cf]
cf_ker = [float(r["ebpf_cache_us"]) for r in cf]
app_per, app_b = fit_line(cf_M, cf_app)     # app cache ~ per-consumer µs
ker_per, ker_b = fit_line(cf_M, cf_ker)     # eBPF cache ~ per-consumer µs

def offload_kubeproxy(N): return kp_m * N + kp_b
def offload_connect4(N):  return e4_flat
def cache_app(M):         return app_per * M + app_b
def cache_kernel(M):      return ker_per * M + ker_b


# ----------------------------------------------------------------------------
# Build the per-task delay / energy / EDP for each arm -----------------------
# ----------------------------------------------------------------------------
def energy_uJ(delay_us, dataplane_frac_offloaded=0.0):
    """Modeled energy (µJ) = P_active(W) × active_time(s) × 1e6.
    dataplane_frac_offloaded shifts that fraction of delay's CPU work off-host."""
    active_us = delay_us * (1.0 - dataplane_frac_offloaded)
    return P_ACTIVE_W * (active_us * 1e-6) * 1e6   # = P_ACTIVE_W * active_us

def edp(delay_us, energy_uj):                       # Energy-Delay Product
    return energy_uj * delay_us

SWEEP = [100, 500, 1000, 2000, 5000, 10000, 20000, 40000, 60000]

rows = []
for N in SWEEP:
    F = fanout_for_N(N)
    # ---- per-task DELAY (measured-derived) ----
    d_base = offload_kubeproxy(N) + cache_app(F)        # kube-proxy + app cache
    d_edag = offload_connect4(N)  + cache_kernel(F)     # connect4 + eBPF cache
    d_ideal = offload_connect4(N) + 0.0                 # CachOf assumption (hit=0)
    # ---- ENERGY (modeled): software ----
    e_base_sw = energy_uJ(d_base)                       # all on CPU
    e_edag_sw = energy_uJ(d_edag)                       # all on CPU (software eBPF)
    # ---- ENERGY (modeled): projected hardware offload (eDAG dp -> SmartNIC) ----
    e_edag_hw = energy_uJ(d_edag, HW_OFFLOAD_DATAPLANE_FRAC)
    rows.append(dict(
        N=N, fanout=F,
        delay_base_us=round(d_base, 2),
        delay_edag_us=round(d_edag, 2),
        delay_ideal_us=round(d_ideal, 2),
        delay_speedup=round(d_base / d_edag, 2),
        energy_base_sw_uj=round(e_base_sw, 2),
        energy_edag_sw_uj=round(e_edag_sw, 2),
        energy_edag_hw_uj=round(e_edag_hw, 2),
        edp_base_sw=round(edp(d_base, e_base_sw), 1),
        edp_edag_sw=round(edp(d_edag, e_edag_sw), 1),
        edp_edag_hw=round(edp(d_edag, e_edag_hw), 1),
        edp_improve_sw=round(edp(d_base, e_base_sw) / edp(d_edag, e_edag_sw), 2),
        edp_improve_hw=round(edp(d_base, e_base_sw) / edp(d_edag, e_edag_hw), 2),
    ))

# ----------------------------------------------------------------------------
# Write CSV + console summary -------------------------------------------------
# ----------------------------------------------------------------------------
out_csv = os.path.join(HERE, "edp_results.csv")
with open(out_csv, "w", newline="") as f:
    w = csv.DictWriter(f, fieldnames=list(rows[0].keys()))
    w.writeheader(); w.writerows(rows)

print("=== MODEL FITS (from measured CSVs) ===")
print(f"  kube-proxy offload : {kp_m*1000:.2f} ns/svc * N + {kp_b:.1f} us   [O(N), measured]")
print(f"  connect4  offload  : {e4_flat:.1f} us flat                       [O(1), measured]")
print(f"  app cache serve    : {app_per:.2f} us/consumer (+{app_b:.1f})    [measured]")
print(f"  eBPF cache serve   : {ker_per:.2f} us/consumer (+{ker_b:.1f})    [measured]")
print(f"  P_active           : {P_ACTIVE_W} W  | HW offload dp frac {HW_OFFLOAD_DATAPLANE_FRAC} [MODELED]")
print()
print(f"=== EDP HEAD-TO-HEAD (wrote {out_csv}) ===")
print(f"{'N':>7} {'F':>3} {'delay base':>11} {'delay eDAG':>11} {'EDP imp sw':>11} {'EDP imp hw':>11}")
for r in rows:
    print(f"{r['N']:>7} {r['fanout']:>3} {r['delay_base_us']:>9.1f}us {r['delay_edag_us']:>9.1f}us "
          f"{r['edp_improve_sw']:>10.2f}x {r['edp_improve_hw']:>10.2f}x")

# ----------------------------------------------------------------------------
# Plot (optional) ------------------------------------------------------------
# ----------------------------------------------------------------------------
ap = argparse.ArgumentParser()
ap.add_argument("--no-plot", action="store_true")
args = ap.parse_args()
if args.no_plot:
    raise SystemExit(0)
try:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
except ImportError:
    print("\n[matplotlib not installed -> skipped plot; CSV is written]")
    raise SystemExit(0)

N   = [r["N"] for r in rows]
fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(13, 5))

ax1.plot(N, [r["delay_base_us"] for r in rows], "o-", color="#c0392b", lw=2,
         label="baseline: kube-proxy + app cache  (O(N))")
ax1.plot(N, [r["delay_edag_us"] for r in rows], "s-", color="#27ae60", lw=2,
         label="eDAG-MEC: connect4 + eBPF cache  (O(1))")
ax1.plot(N, [r["delay_ideal_us"] for r in rows], "--", color="#7f8c8d", lw=1.5,
         label="CachOf assumption: hit cost = 0")
ax1.set_xlabel("Cluster size N"); ax1.set_ylabel("Per-task delay (µs)")
ax1.set_title("Delay (measured-derived)"); ax1.grid(alpha=0.3); ax1.legend()

ax2.plot(N, [r["edp_base_sw"] for r in rows], "o-", color="#c0392b", lw=2,
         label="baseline EDP (software)")
ax2.plot(N, [r["edp_edag_sw"] for r in rows], "s-", color="#27ae60", lw=2,
         label="eDAG-MEC EDP (software)")
ax2.plot(N, [r["edp_edag_hw"] for r in rows], "^-", color="#2980b9", lw=2,
         label="eDAG-MEC EDP (projected HW offload)")
ax2.set_yscale("log"); ax2.set_xlabel("Cluster size N")
ax2.set_ylabel("Energy-Delay Product (µJ·µs, log)")
ax2.set_title("EDP — lower is better"); ax2.grid(alpha=0.3, which="both"); ax2.legend()

fig.suptitle("eDAG-MEC vs base-paper assumptions — Energy-Delay Product across cluster size",
             fontsize=13, fontweight="bold")
fig.tight_layout()
out_png = os.path.join(HERE, "edp_plot.png")
fig.savefig(out_png, dpi=140)
print(f"\nsaved {out_png}")
