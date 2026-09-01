# C3 Benchmark Runbook — Kernel Retention Cache + DAG-Aware Deterministic GC

> **Provenance.** The original `C3_BENCHMARK_RUNBOOK.md` existed only on the
> author's local Windows machine and was never committed; it could not be recovered
> from the repository. This runbook is **reconstructed from the bench source
> (`cmd/c3_bench/main.go`), the eBPF program (`internal/ebpf/c/tc_bridge.c`), and the
> committed run record (`results/c3_results.txt`)**. The numbers below are the
> committed ones; the invocation is read directly from the source flags.

## What this bench proves

`c3_bench` is the bench behind the project's **single novel claim** — DAG-aware
deterministic garbage collection of a kernel retention cache. It measures three
things on a real kernel:

- **A. Routing-only path** (`ref_count = 1`): plain forward, no retention.
- **B. Retention store path** (`ref_count = 4`, first hit): store the producer's
  payload in the kernel `retention_map`.
- **C. Fan-out serve path + GC correctness**: N consumers read the retained entry;
  the eBPF code sets `remaining_consumers = ref_count` on first hit and atomically
  decrements on each read, deleting the entry the instant it reaches zero.

## Requirements

- Linux **kernel ≥ 6.6** (TCX attach path used by the bench), cgroup v2.
- Root (TCX/`tc` ingress attach needs `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`).
- `clang`/`llvm` + `libbpf` headers to compile the eBPF object.

## Steps

```bash
# 1. compile the eBPF object the bench loads
make ebpf                    # or, just this one:
clang -target bpf -O2 -g -I /usr/include/$(uname -m)-linux-gnu \
      -c internal/ebpf/c/tc_bridge.c -o internal/ebpf/c/tc_bridge.o

# 2. build + run (attaches a TC ingress program to lo, port 9000)
go build -o /tmp/c3 ./cmd/c3_bench
sudo /tmp/c3 --iface lo --iters 1000 | tee results/c3_results.txt
```

Flags (from `cmd/c3_bench/main.go`):

| flag | default | meaning |
|---|---|---|
| `--elf` | `internal/ebpf/c/tc_bridge.o` | compiled eBPF object to load |
| `--iface` | `lo` | interface to attach TC ingress to |
| `--iters` | `1000` | iterations per sub-benchmark |

Automated as **step 4/6** of `scripts/run_all.sh` (`sudo make bench`).

## Expected result (committed `results/c3_results.txt`, kernel 6.x, iface `lo`)

```
A. Routing-only (ref_count=1)      mean = 9.407 µs
B. Retention store (ref_count=4)   mean = 10.108 µs      -> retention overhead = 701 ns
C. Fan-out serve path              N=2 6.946 µs · N=3 6.890 µs · N=4 6.572 µs
   scaling spread across N=2..4    = 374 ns   (small => O(1) in fan-out degree)
Deterministic GC                   = 3000 / 3000 fan-out tasks fully reclaimed
                                     (retention_map empty after the last consumer)
```

Interpretation: retention costs ~0.7 µs over plain routing; per-access cost is
O(1) in fan-out degree (374 ns spread); and the atomic ref-count + delete-on-zero
policy reclaims **every** entry with no leak and no premature eviction — the
correctness guarantee that distinguishes this from an LRU/TTL kernel cache.
