import csv, numpy as np, matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import os as _os
R = _os.path.join(_os.path.dirname(_os.path.dirname(_os.path.abspath(__file__))), "results")

N, base, ebpf = [], [], []
with open(_os.path.join(R,"mec_results.csv")) as f:
    for r in csv.DictReader(f):
        N.append(int(r["N"])); base.append(float(r["kubeproxy_us"])); ebpf.append(float(r["ebpf_connect4_us"]))
N=np.array(N); base=np.array(base); ebpf=np.array(ebpf)
A=np.polyfit(N, base, 1)
print(f"kube-proxy fit: {A[0]*1000:.3f} ns/rule + {A[1]:.1f} us  | eBPF mean {ebpf.mean():.1f}us std {ebpf.std():.1f}")

fig,(ax1,ax2)=plt.subplots(1,2,figsize=(13,5))
for ax in (ax1,ax2):
    ax.plot(N, base,'o-',color="#c0392b",lw=2,ms=6,label="kube-proxy: iptables DNAT + conntrack  O(N)")
    ax.plot(N, ebpf,'s-',color="#27ae60",lw=2,ms=6,label="eDAG-MEC: cgroup/connect4 bypass  O(1)")
    ax.set_xlabel("Cluster size  N  (services in KUBE-SERVICES)")
    ax.set_ylabel("Cross-node task RTT  (µs)")
    ax.grid(True,alpha=0.3); ax.legend(loc="upper left")
ax1.set_title("Multi-node (veth) — real sender-side connect4 vs kube-proxy")
ax2.set_yscale("log"); ax2.set_xscale("symlog")
ax2.set_title("Log scale — connect4 stays flat as kube-proxy diverges")
ax1.annotate(f"kube-proxy ≈ {A[0]*1000:.1f} ns/svc × N + DNAT\nconnect4 ≈ {ebpf.mean():.0f} µs (flat, O(1))\n{base[-1]/ebpf[-1]:.1f}× faster at N={N[-1]:,}",
    xy=(N[-1],base[-1]), xytext=(N[-1]*0.18,base[-1]*0.72),
    fontsize=10, bbox=dict(boxstyle="round",fc="#eafaf1",ec="#27ae60"))
fig.suptitle("eDAG-MEC: emulated multi-node edge cluster — connect4 O(1) bypass vs kube-proxy O(N) DNAT",
    fontsize=13, fontweight="bold")
fig.tight_layout(); fig.savefig(_os.path.join(R,"mec_plot.png"),dpi=140); print("saved", _os.path.join(R,"mec_plot.png"))
