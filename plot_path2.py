import csv, sys, numpy as np, matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

src = sys.argv[1] if len(sys.argv) > 1 else "path2_results.csv"
N, app, ker = [], [], []
with open(src) as f:
    for r in csv.DictReader(f):
        N.append(int(r["N"])); app.append(float(r["app_producer_us"])); ker.append(float(r["kernel_producer_us"]))
N=np.array(N); app=np.array(app); ker=np.array(ker)

fig, ax = plt.subplots(figsize=(8,5))
ax.plot(N, app,'o-',color="#c0392b",lw=2,ms=7,label="app fan-out: N userspace sends  O(N)")
ax.plot(N, ker,'s-',color="#27ae60",lw=2,ms=7,label="eDAG Path-2: 1 send → kernel clone_redirect")
ax.set_xlabel("Fan-out degree  N  (DAG consumers)")
ax.set_ylabel("Producer-side fan-out cost  (µs)")
ax.set_title("C3 Path-2 (§6-B): kernel-side fan-out delivery — all N consumers received")
ax.grid(True,alpha=0.3); ax.legend(loc="upper left")
ax.annotate(f"app ≈ {(app[-1]-app[0])/(N[-1]-N[0]):.2f} µs/consumer (linear)\n"
            f"Path-2 {app[-1]/ker[-1]:.1f}× cheaper at N={N[-1]} (1 syscall)",
    xy=(N[-1],app[-1]), xytext=(2, app[-1]*0.62),
    fontsize=10, bbox=dict(boxstyle="round",fc="#eafaf1",ec="#27ae60"))
fig.tight_layout(); fig.savefig("path2_plot.png",dpi=140); print("saved path2_plot.png")
