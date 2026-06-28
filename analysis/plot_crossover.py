import csv, numpy as np
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import os as _os
R = _os.path.join(_os.path.dirname(_os.path.dirname(_os.path.abspath(__file__))), "results")

N, ipt, xdp = [], [], []
with open(_os.path.join(R,"crossover_results.csv")) as f:
    for r in csv.DictReader(f):
        N.append(int(r["N"])); ipt.append(float(r["iptables_us"])); xdp.append(float(r["xdp_us"]))
N=np.array(N); ipt=np.array(ipt); xdp=np.array(xdp)

# Linear fit on iptables vs N (per-rule cost)
A=np.polyfit(N, ipt, 1)
slope_ns = A[0]*1000
print(f"iptables fit: {A[0]*1000:.3f} ns/rule + {A[1]:.2f} us base")
print(f"xdp mean: {xdp.mean():.2f} us (std {xdp.std():.2f})")

fig,(ax1,ax2)=plt.subplots(1,2,figsize=(13,5))

for ax in (ax1,ax2):
    ax.plot(N, ipt, 'o-', color="#c0392b", lw=2, ms=6, label="netfilter / iptables  O(N)")
    ax.plot(N, xdp, 's-', color="#2980b9", lw=2, ms=6, label="eBPF / XDP  O(1)")
    ax.set_xlabel("Cluster size  N  (number of services / endpoints)")
    ax.set_ylabel("Per-packet routing-decision latency  (µs)")
    ax.grid(True, alpha=0.3); ax.legend(loc="upper left")
ax1.set_title("Linear scale — O(N) vs O(1) crossover")
ax2.set_yscale("log"); ax2.set_xscale("log")
ax2.set_title("Log-log — XDP stays flat as iptables diverges")

# annotate fit + speedup
ax1.annotate(f"iptables ≈ {slope_ns:.1f} ns/rule × N\nXDP ≈ {xdp.mean():.1f} µs (flat)\n{ipt[-1]/xdp[-1]:.0f}× faster at N={N[-1]:,}",
             xy=(N[-1], ipt[-1]), xytext=(N[-1]*0.30, ipt[-1]*0.75),
             fontsize=10, bbox=dict(boxstyle="round", fc="#fdf2e9", ec="#c0392b"))
fig.suptitle("eDAG-MEC: O(1) XDP data plane vs O(N) iptables — where eBPF prevails at the edge", fontsize=13, fontweight="bold")
fig.tight_layout()
fig.savefig(_os.path.join(R,"crossover_plot.png"), dpi=140)
print("saved", _os.path.join(R,"crossover_plot.png"))
