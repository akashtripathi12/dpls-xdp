import csv, numpy as np, matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

def load(p):
    X,a,e=[],[],[]
    with open(p) as f:
        for r in csv.DictReader(f):
            k=list(r.values())
            X.append(int(k[0])); a.append(float(k[1])); e.append(float(k[2]))
    return np.array(X),np.array(a),np.array(e)

N,appN,ebN=load("cache_fanout.csv")
D,appD,ebD=load("cache_payload.csv")
print(f"app per-consumer ~{(appN[-1]-appN[0])/(N[-1]-N[0]):.2f} us  | ebpf per-consumer ~{(ebN[-1]-ebN[0])/(N[-1]-N[0]):.2f} us")

fig,(ax1,ax2)=plt.subplots(1,2,figsize=(13,5))

# (a) fan-out
ax1.plot(N,appN,'o-',color="#c0392b",lw=2,ms=6,label="App-level edge cache (CachOf substrate): O(N) downlink")
ax1.plot(N,ebN,'s-',color="#27ae60",lw=2,ms=6,label="eDAG-MEC eBPF kernel cache: O(1)/consumer")
ax1.axhline(0,color="#7f8c8d",ls="--",lw=1.5,label="CachOf model assumption: hit serve = 0")
ax1.set_xlabel("DAG fan-out  N  (downstream consumers of one result)")
ax1.set_ylabel("Total serve+deliver latency (µs)")
ax1.set_title("(a) Fan-out delivery — the cost CachOf sets to zero")
ax1.grid(True,alpha=0.3); ax1.legend(loc="upper left",fontsize=8.5)
ax1.annotate(f"{appN[-1]/ebN[-1]:.0f}× cheaper at N={N[-1]}\n(app ≈ {(appN[-1]-appN[0])/(N[-1]-N[0]):.0f} µs/consumer,\n kernel ≈ {(ebN[-1]-ebN[0])/(N[-1]-N[0]):.1f} µs/consumer)",
    xy=(N[-1],appN[-1]),xytext=(N[-1]*0.30,appN[-1]*0.72),fontsize=9,
    bbox=dict(boxstyle="round",fc="#fdf2e9",ec="#c0392b"))

# (b) payload
ax2.plot(D,appD,'o-',color="#c0392b",lw=2,ms=6,label="App-level edge cache (CachOf substrate)")
ax2.plot(D,ebD,'s-',color="#27ae60",lw=2,ms=6,label="eDAG-MEC eBPF kernel cache")
ax2.axhline(0,color="#7f8c8d",ls="--",lw=1.5,label="CachOf model assumption: hit serve = 0")
ax2.set_xlabel("Result payload size  D  (bytes)")
ax2.set_ylabel("Serve latency at fan-out N=8 (µs)")
ax2.set_title("(b) Hit-serve cost vs payload size")
ax2.grid(True,alpha=0.3); ax2.legend(loc="center right",fontsize=8.5)
ax2.set_ylim(bottom=-10)

fig.suptitle("eDAG-MEC kernel cache vs CachOf-style application cache — realising the 'free cache hit' assumption",
    fontsize=13,fontweight="bold")
fig.tight_layout(); fig.savefig("cache_plot.png",dpi=140); print("saved cache_plot.png")
