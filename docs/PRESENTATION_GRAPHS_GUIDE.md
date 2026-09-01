# eDAG-MEC — Results & Complexity: graph-by-graph explainer for the presenter

**Who this is for:** the person presenting *Complexity Analysis* and *Results & Discussion*
(slides 13–20 of `BTP_Presentation.pdf`) and fielding questions on them.

**How it was built (2026-09-01):** every number and every "why" below was re-derived from
the committed data in `results/`, the benchmark sources in `cmd/*/main.go`, the eBPF C in
`internal/ebpf/c/`, the model scripts in `analysis/`, and the figure images actually
embedded in the slide PDF. Where a slide says something the data does not support, this
guide says so and gives you the sentence to say instead. Nothing in the deck was changed.

It complements (and in five places corrects) the earlier `PRESENTATION_PREP.md`.

---

## 0. Read this first — five things the earlier prep notes missed

1. **Slide 18 shows a different figure than its bullets describe.** The image embedded on
   slide 18 is `results/edp_plot.png` (y-axis "Per-task delay (µs)", baseline reaching
   ~770 µs; EDP improvement 197×). The bullets on the same slide quote the *other* model
   (`energy_vs_basepaper_plot.png`): "22.2 ms vs ~4 ms, 5.6×", "~100× EDP". Both are real
   and both are in the paper, but they are different models and a panelist who reads the
   axis while you say "22 milliseconds" will stop you. See §5 for the bridging sentence.

2. **In the application-level model (slides 18–20) every subtask is a cache hit.** CachOf's
   published parameters set `cache_capacity = 7` and `num_task = 7`, so the popularity
   knapsack caches every subtask class. Verified by running the model: hit fraction = 1.0.
   Consequence: the offload term (kube-proxy vs connect4) **never enters** the makespan,
   deadline or ablation numbers. That is the *actual* reason "Without sender bypass" ≈ Full
   on slide 20 (see §7 for the honest answer).

3. **The eBPF arm of the cache benchmark is userspace map access, not packet delivery.**
   `cmd/cache_bench` times `m.Put` + δ × `m.Lookup` + `m.Delete` from Go through the `bpf()`
   syscall. It measures the cost of *accessing the retained copy*; the app arm measures
   *re-delivering it over the stack*. Kernel-side *delivery* is the separate Path-2
   experiment (`fanout.c`). If a kernel person asks "where is the network in your eBPF
   cache arm?", that is the answer — don't claim the green line includes delivery.

4. **Two unrelated numbers are both "13.8%".** Slide 14's "13.8% faster under load" is the
   1000-run chaos test (501.04 → 431.93 µs). The energy proxy's "+13.8% kernel system time"
   is eBPF costing *more* CPU in software. Same digits, opposite sign. Don't mix them.

5. **`docs/` contains numbers from an earlier dev-kernel run that differ from the slides**
   (crossover 62.8× and 8.8 ns/rule; single-host 7.4× at 40 µs; cache 22×). The slides use
   the AWS run in `results/*.csv` (37×, 4.97 ns/rule, 7.1× at 21 µs, 18×). If someone has
   read `docs/crossover_experiment.md`, say: "that write-up is the pre-AWS dev-kernel run;
   the deck and paper use the AWS cluster run in `results/`. Same shape, different constant."

---

## 1. How every measured number was produced (say this once, then never again)

All six measured rows on slide 14 come from the same harness pattern:

| | |
|---|---|
| Probe | one UDP datagram (4-byte task id) and its echo; wall-clock round trip in Go |
| Repetitions | 3000 per data point (crossover, MEC, cross-VM); 2000 (cache, Path-2); 1000/2000/3000/4000 (C3) |
| Statistic | **2 % trimmed mean** — samples sorted, top 2 % dropped, mean of the rest (kills scheduler spikes) |
| New flow per probe | fresh socket each time, so conntrack cannot amortise the iptables walk; this matches kube-proxy on every new connection |
| Worst-case placement | the real service rule is **last** in the chain; N dummy rules are walked first |
| Equal footprint | the eBPF map is pre-filled with the same N entries the baseline has rules, so memory is matched and only the lookup mechanism differs |
| A/B | identical binary and path; the only difference is whether the BPF program is attached |

Hardware: 3 × AWS EC2 `c7i-flex.large` (2 vCPU), Ubuntu 24.04, kernel 6.17-aws, cgroup v2,
clang 18, Go 1.22, cilium/ebpf. Run log: `results/run.log`, `results/xnode.log`.

---

## 2. Slide 13 — Complexity table, row by row (what the O() actually refers to)

| Row | Where the O(N)/O(δ) in the baseline comes from | Why ours is O(1) | Measured on |
|---|---|---|---|
| Sender offload (connect4) | kube-proxy = iptables `KUBE-SERVICES` chain: N rules tested in order, then DNAT + conntrack insert | `cgroup/connect4` fires inside `connect()`, one hash lookup in `vault`, rewrites `user_ip4` before any packet exists | slides 15, 16 |
| Receiver routing (tc) | receiver netfilter chain walk | `tc_bridge.c` looks up `vault_map[task_id]` (hash) and rewrites `daddr` | C3 bench routing-only path (9.4 µs) |
| Retention serve (per consumer) | app cache: one full stack round trip per consumer | one hash access on the single retained copy | slide 17 (a), C3 bench |
| Deterministic GC | n/a (LRU/TTL are heuristics) | `bpf_spin_lock` → decrement → `bpf_map_delete_elem` when zero: constant work per read | C3 bench 3000/3000 |
| Path-2 producer cost | producer does δ `sendto()` calls | producer does **one** `sendto()`; kernel clones with `bpf_clone_redirect` inside a loop unrolled to `MAX_FANOUT_P2 = 16` | slide 14 row 5 |
| Rule provisioning | n/a | Channeler writes one map entry per DAG edge before dispatch: O(\|E\|) writes total, each O(1), none on the hot path | design |

**Honest scope on row 2:** the tc program returns `TC_ACT_OK` after the rewrite, so the
receiver *decision* is O(1) but the packet still continues up the normal stack. The full
receiver-side bypass (`bpf_redirect_peer`) is listed as future work on slide 23. If asked
"is the receiver netfilter actually bypassed?", say exactly that.

**Bounded constants you must know (they are the C3 verifier obligation):**
`MAX_FANOUT = 4` in `tc_bridge.c` (retention rule destinations), `MAX_FANOUT_P2 = 16` in
`fanout.c`, `RETAIN_BYTES = 64`. If asked "what if fan-out exceeds 16?": the bound exists so
the verifier can prove termination; larger fan-out needs chained rules or a bigger constant,
both trivially changeable, and the measured trend up to δ = 64 (cache bench) is linear anyway.

C1 / C2 / C3 definitions are in `PRESENTATION_PREP.md` Part 2 and remain correct.

---

## 3. Slide 15 — Routing crossover (`crossover_results.csv`, `cmd/crossover_bench`)

**Exactly what is measured.** Loopback UDP echo. Arm A: a filter-table chain `DPLS_SVC`
holding N rules `-d 10.x.y.z/32 -j RETURN` that never match, walked on every request
packet. Arm B: `xdp_lookup.o` in generic XDP mode on `lo` doing exactly one hash lookup in
a map pre-filled with N entries. 500 warm-up pings before the sweep.

**Data:**

| N | 0 | 100 | 500 | 1k | 2k | 5k | 10k | 20k | 40k |
|---|---|---|---|---|---|---|---|---|---|
| iptables (µs) | 5.18 | 5.68 | 7.72 | 10.21 | 15.07 | 29.08 | 54.82 | 102.25 | 204.65 |
| XDP (µs) | 5.41 | 5.33 | 5.35 | 5.32 | 5.41 | 5.49 | 5.38 | 5.39 | 5.50 |
| ratio | 0.96 | 1.06 | 1.44 | 1.92 | 2.79 | 5.3 | 10.2 | 19.0 | 37.2 |

Fit: iptables = **4.97 ns/rule × N + 4.9 µs**. Segment slopes are 4.67–5.15 ns/rule at every
step, i.e. the line is linear to within 5 % across the whole sweep. XDP = 5.40 ± 0.06 µs.

**Why the red line is straight.** netfilter evaluates a chain as a linked list; a packet
to the last rule tests all N before it. One rule test ≈ one memory read plus a compare ≈
5 ns on this CPU. Latency = 5 ns × N. There is no super-linear knee here (the dev-kernel
run had one past 20k; the AWS run does not).

**Why the blue line is flat, and why it is 5.4 µs not nanoseconds.** A hash lookup does not
depend on how many keys the table holds. The 5.4 µs is the *harness*: two syscalls, two
loopback traversals, a Go goroutine wake-up. The lookup itself is tens of nanoseconds and is
invisible inside that. Say: "the blue line is the measurement floor; the lookup is below it."

**Why iptables is *faster* at N = 0 (5.18 vs 5.41).** With zero rules the chain is a single
jump, while generic XDP still runs a BPF program on every packet and (in generic mode)
linearises the skb. The crossover is therefore at roughly **N ≈ 100**: smaller than any
real cluster. This is the "crossover" in the experiment's name; use it.

**Scope to volunteer.** Routing decision only, loopback, worst-case rule placement. Not
end-to-end eDAG-MEC latency; slides 16–17 are.

**Line to say:** "One line has a slope of five nanoseconds per service and the other has
no slope. At 40,000 services that is 37×, and it keeps widening."

---

## 4. Slide 16 — Cross-VM offload hop (`mec_xnode_results.csv`, `cmd/mec_xnode`)

**Exactly what is measured.** Two real EC2 instances over the VPC ENA NIC. The scheduler
dials the virtual ClusterIP 10.96.0.10 with a connected UDP socket (so `connect()` fires the
cgroup hook). Arm A: `iptables -t nat` OUTPUT → `KUBE-SERVICES` with N dummy rules and a
final DNAT to the worker IP (real DNAT + conntrack). Arm B: `connect4.o` rewrites the
destination in the `vault` map before netfilter. No warm-up pings in this harness; the whole
10-point sweep ran in 10 s (`xnode.log` 17:05:45 → 17:05:55).

**Data:**

| N | 0 | 100 | 500 | 1k | 2k | 5k | 10k | 20k | 40k | 60k |
|---|---|---|---|---|---|---|---|---|---|---|
| kube-proxy (µs) | 100.6 | 99.1 | 98.6 | 101.1 | 101.6 | 107.6 | 116.9 | 141.1 | 186.7 | 211.2 |
| connect4 (µs) | 99.3 | 95.5 | 94.9 | 91.9 | 91.2 | 87.6 | 90.7 | 91.5 | 87.2 | 82.9 |
| speed-up | 1.01 | 1.04 | 1.04 | 1.10 | 1.11 | 1.23 | 1.29 | 1.54 | 2.14 | 2.55 |

Fit: kube-proxy = **1.97 ns/svc × N + 99.1 µs**. connect4 mean **91.3 µs**, std 4.4.

**The floor.** Both arms start at ~99–100 µs. That is the VM-to-VM wire round trip and it is
in the baseline too. It is why the ratio is 2.55× here and 7.1× single-host, while the
**absolute saving is the same**: 128.2 µs cross-VM vs 130.0 µs single-host at N = 60k.
Say: "the wire compresses the ratio, not the saving."

**Why the green line drifts *down* (99 → 83 µs) — the question you will get.** There is no
mechanism by which more services make a hash lookup faster, so this is not an effect of N.
Three observations support "drift, not mechanism":
- The **baseline also dips** over the first three points (100.6 → 99.1 → 98.6) before the
  slope takes over. Whatever moved the green line early moved the red line too.
- Std of the green line is 4.4 µs on a 99 µs floor, i.e. 4.5 %; cloud network RTT jitter
  and the burstable `c7i-flex` CPU frequency ramping over a 10-second run are both of that
  order, and this harness has no warm-up phase (the crossover harness does).
- The single-host version of the same experiment (`mec_results.csv`, std 0.6 µs on a 21 µs
  floor) is flat, 19.7–22.0 µs, across the same N sweep.

Say: "Flat within noise. If anything, treat 91 as the mean and 83 as a lucky end point;
that is why the slide says '~91 µs', and why the honest ratio at 60k is 2.55× using the
actual 82.9 µs point (211.2 / 82.9), or 2.3× using the mean." Do **not** claim it improves
with scale.

**Why the red slope is 2 ns/svc here but 5 ns/rule on slide 15.** Different table (nat vs
filter), different rule shape (`-d … -p udp --dport 9000` vs `-d …`), different path (real
NIC vs loopback), separate runs. The order of magnitude, a few nanoseconds per rule, is the
robust statement; the exact constant is not. Don't over-explain; say "a few ns per rule,
consistent across three independent harnesses (5.0, 2.2, 2.0)."

**Single-host backup (slide 14 row 1, `mec_results.csv`).** netns + veth on one host.
kube-proxy = 2.22 ns/svc × N + 25.9 µs; connect4 21.06 ± 0.63 µs; 151.2 vs 21.2 µs at 60k =
**7.13×**, gap 130 µs. The 25.9 − 21.1 ≈ 5 µs intercept difference is what DNAT + conntrack
cost per fresh UDP flow on this path.

**Risk from slide 4 (not your slide, but adjacent).** Slide 4 says DNAT + conntrack cost
≈ 407 µs (81.3 %) at 500 services. Your reproducible micro-benchmarks show the fixed
DNAT/conntrack cost is only ~5 µs on veth and ~1 µs cross-VM (compare the two arms at
N = 0). If challenged: the 407 µs figure is from an earlier end-to-end run of the full
scheduler harness whose raw captures are not in the repo (`docs/RECOVERY_STATUS.md` §2.1);
the micro-benchmarks isolate the per-hop routing cost and are what the O(1)-vs-O(N) claim
rests on. Lean on the **slope**, not the intercept.

---

## 5. Slide 17 — Cache fan-out, payload, C3 retention and GC

### 5a. Fan-out panel (a) — `cache_fanout.csv`, `cmd/cache_bench`

**Exactly what is measured**, per repetition, wall-clock around the whole block:
- **App arm:** copy the D-byte result into a userspace buffer, then for each of δ consumers
  open a UDP socket, send the bytes to a local echo server, wait for the echo, close. Free.
- **eBPF arm:** `Put` the result into a `BPF_MAP_TYPE_HASH` (value size 1408 B), then δ ×
  `Lookup` on that key, then `Delete` (the deterministic GC step). All via the `bpf()`
  syscall from Go. (See §0 item 3 for what this does and does not include.)

**Data and fits:**

| δ | 1 | 2 | 4 | 8 | 16 | 32 | 64 |
|---|---|---|---|---|---|---|---|
| app (µs) | 11.2 | 20.1 | 40.2 | 80.2 | 160.6 | 322.9 | 651.9 |
| eBPF (µs) | 1.53 | 2.05 | 3.11 | 5.24 | 9.69 | 18.25 | 35.92 |
| ratio | 7.3 | 9.8 | 13.0 | 15.3 | 16.6 | 17.7 | 18.1 |

app = **10.18 µs/consumer − 0.75**; eBPF = **0.546 µs/consumer + 0.92**.

**Why both lines are straight.** δ consumers need δ deliveries. Both arms are O(δ) in
*total*; the claim is O(1) *per access* and an 18× smaller constant. The app constant
(10 µs) is one loopback round trip plus socket create/close; the eBPF constant (0.55 µs)
is one `bpf()` syscall copying a 1408-byte value.

**Why the ratio climbs from 7× to 18×.** The eBPF arm has a ~0.9 µs fixed cost (Put +
Delete) that is amortised as δ grows; the asymptotic ratio is 10.18 / 0.546 ≈ 18.6×. At
δ = 1 the fixed cost is 60 % of the eBPF total, hence only 7.3×.

**The dashed line at zero** is CachOf's `exet = 0` on a hit. Point at it. Our green line
is not zero either; it is 18× closer than the substrate CachOf implicitly assumes.

### 5b. Payload panel (b) — `cache_payload.csv`

At δ = 8: app 79.2 / 79.8 / 81.0 / 80.6 / 80.7 µs and eBPF 5.33 / 5.30 / 5.34 / 5.25 /
5.35 µs at D = 64 / 256 / 512 / 1024 / 1400 B. **Both flat.**

- eBPF flat **by construction**: the map value slot is fixed at 1408 bytes and the full slot
  is copied regardless of D. Say it that way if asked; it is the same reason the kernel
  retention path copies a fixed `RETAIN_BYTES` buffer.
- App flat because every D fits in one datagram under the MTU; loopback UDP cost is per
  packet, not per byte.
- So the message of panel (b) is the **~15× ratio at any payload**, not "ours is flat and
  theirs is not" — theirs is flat too.

### 5c. C3 retention bench — `c3_results.txt`, `cmd/c3_bench`, `tc_bridge.c`

This is the real TC/TCX program on `lo`, exercised by UDP triggers to port 9000; each
trigger is timed round trip. Three sub-benchmarks:

| Path | mean | p50 | p95 | p99 | n |
|---|---|---|---|---|---|
| A. routing-only (ref_count = 1, no retention) | 9.41 µs | 8.95 | 10.53 | 18.8 | 1000 |
| B. retention store (ref_count = 4, first hit) | 10.11 µs | 9.77 | 11.19 | 20.2 | 1000 |
| C. serve path, δ = 2 | 6.95 µs | 6.62 | 8.32 | 12.8 | 2000 |
| C. serve path, δ = 3 | 6.89 µs | 6.49 | 9.51 | 15.1 | 3000 |
| C. serve path, δ = 4 | 6.57 µs | 6.24 | 7.69 | 11.9 | 4000 |

- **Retention overhead = 10.108 − 9.407 = 701 ns** per store. Volunteer it.
- **Serve-path spread δ = 2 → 4 = 374 ns**, no upward trend: per-access cost independent
  of δ. This is the direct evidence for the O(1)/access row and it is not on any slide.
- **GC: 3000/3000.** For each of 1000 tasks × δ ∈ {2,3,4}: program the rule, send one
  store trigger, confirm the entry exists, send δ serve triggers, confirm the entry is gone.
  Never early, never late, on a real kernel.

**Why the serve path (6.6–6.9 µs) is *faster* than routing-only (9.4 µs) — someone may
notice.** In A and B every trigger uses a fresh task id and the harness writes the vault
rule immediately before sending, so the map entry and cache lines are cold. In C the same
task id is hit δ+1 times in a row, so the entry is warm. The valid comparisons are A vs B
(overhead) and within C (scaling); do not compare C to A.

**Tails.** p99 of 12–20 µs on a 2-vCPU VM is scheduler jitter; the harness uses means, not
trimmed means, here, which is why means sit above p50.

**Caveat to hold:** δ = 2–4 is three points. If challenged, concede and point to panel (a),
which goes to δ = 64 with the same per-access constant.

### 5d. Path-2 kernel fan-out (slide 14 row 5, `path2_results.csv`, `fanout.c`)

**Exactly what is measured.** Time of the producer's `Write()` only. App arm: δ `sendto()`
calls. Kernel arm: one `sendto()` to a trigger address; the TCX ingress program on `lo`
rewrites `daddr` and `dport`, fixes the IP checksum, zeroes the UDP checksum, and calls
`bpf_clone_redirect` once per consumer, then drops the original (`TC_ACT_SHOT`). Both arms
verified to deliver to all δ consumer sockets.

| δ | 1 | 2 | 4 | 8 | 16 |
|---|---|---|---|---|---|
| app (µs) | 1.89 | 3.65 | 6.88 | 14.71 | 29.39 |
| kernel (µs) | 2.32 | 3.07 | 4.33 | 7.13 | 13.12 |
| speed-up | 0.81 | 1.19 | 1.59 | 2.06 | 2.24 |

app = 1.84 µs/consumer; kernel = **0.72 µs/consumer + 1.54 µs**. Asymptotic ratio 2.56×.

**Why the kernel line still rises if the producer is O(1).** On loopback the ingress hook
runs inside the sender's own `sendto()` call path (loopback transmit → backlog → softirq
before the syscall returns), so the δ clones are performed *before* `Write()` returns and
are counted in the producer's time. The producer issues one syscall (O(1) syscalls) but the
kernel does O(δ) clone work synchronously. The win is the constant: 0.72 vs 1.84 µs per
consumer, because a clone-and-reinject is cheaper than a full socket send.

**Why δ = 1 is slower (0.81×).** One consumer means no fan-out, so the clone, header
rewrites, checksum fix and drop-the-original are pure overhead against a single plain
`sendto()`. It crosses over at δ = 2. Say this before it is asked.

---

## 6. Slide 18 — the figure vs the bullets (read §0 item 1 first)

### What the *figure* on the slide is (`edp_plot.png`, `analysis/analyze_energy_edp.py`)

A per-task model with the measured fits plugged in:

```
delay_baseline(N) = (2.22 ns × N + 25.9 µs)  +  (10.18 µs × F − 0.75)     kube-proxy + app serve
delay_eDAG(N)     =  21.06 µs                +  (0.546 µs × F + 0.92)     connect4 + eBPF serve
F = fan-out = max(1, min(64, N / 1000))        modelling assumption, ties δ to N
Energy  = 15 W × delay             (MODELED; EC2 blocks RAPL)
EDP     = Energy × delay = 15 × delay²
HW projection: 85 % of the eBPF data-plane CPU time moves to a SmartNIC
```

At N = 60k (F = 60): baseline 768.8 µs vs eDAG 54.7 µs = **14.05×**; EDP **197×** software
(= 14.05², because EDP = P·t² with constant P), **1316×** projected hardware (= 197 / 0.15).

**Why the left panel is straight** — it is literally the sum of two measured straight lines.
**Why the right panel is log** — EDP is quadratic in delay, so the baseline spans three
decades; on a log axis read the *gap*, not the steepness. **Why the green EDP line still
rises** — the eBPF serve term 0.546 × F grows with F.

**Be ready for:** "your EDP is just delay squared." Answer: yes, under a constant-power
model EDP = P·t², and we label it modeled. The only place the energy axis carries
information beyond delay is the hardware-offload projection, which we also label projected.

### What the *bullets* describe (`energy_vs_basepaper_fine.csv`, `analysis/energy_vs_basepaper.py`)

CachOf's own per-subtask cost model and parameters (8 apps × 7 subtasks, `cn/fn_bs` compute,
`dn/rn` transfer, popularity-knapsack cache), with subtask compute scaled ×1e-3 for the
dense-edge regime, and only the two assumed-zero terms replaced by our measured fits.
Monte-Carlo over 300 workloads per N. Every subtask is a cache hit (§0 item 2), so the
makespan is: transfer floor (~2.9 ms, present in all arms) + serve costs along the chain.

| N | 100 | 1k | 10k | 20k | 40k | 60k |
|---|---|---|---|---|---|---|
| baseline makespan (ms) | 3.23 | 3.21 | 6.13 | 9.34 | 15.78 | 22.24 |
| eDAG makespan (ms) | 2.96 | 2.93 | 3.12 | 3.28 | 3.63 | 3.95 |
| EDP improvement sw / HW | 7.0× / 47× | 7.0× / 47× | 31× / 207× | 49× / 326× | 78× / 518× | **102× / 680×** |

At 60k: 22.24 / 3.95 = **5.6×** delay; energy ratio 18.1× (= the serve ratio at F = 60);
EDP = 5.6 × 18.1 ≈ **102×**.

**Why the baseline is straight in N** — F = N/1000, and the per-hit app serve cost is
10.18 µs × F, paid on every subtask; a chain of hits adds up linearly. **Why eDAG is nearly
flat** — 0.546 µs × F is 34 µs per hit at F = 60, small against the 2.9 ms transfer floor.
**Why 5.6× here but 14× in the figure** — the 2.9 ms transfer floor is in both arms of the
workload model, exactly as the 99 µs wire floor was on slide 16. Same argument.

**The bridging sentence to say on slide 18:**
> "The plot is the per-task view: one offload plus one fan-out serve, 769 versus 55
> microseconds, fourteen times. Fed into CachOf's full workload model, where the transfer
> floor is common to both arms, the end-to-end makespan gain is 5.6×, 22.2 versus 3.95 ms,
> and EDP improves about 100× in software. All of it is modeled on measured per-hop costs;
> we never deployed 60,000 services."

**Be ready for:** "why does fan-out grow with cluster size?" It is a modelling assumption
(dense edge clusters have more consumers per result), capped at 64 to stay inside the
measured range. Without it the baseline would still be O(N) from the offload term, but
that term does not enter this particular model (all hits), so the coupling is what makes
the baseline grow here. Say: "F tied to N is an assumption; the per-consumer costs it
multiplies are measured."

---

## 7. Slide 19 — Deadline satisfaction (`deadline_satisfaction.csv`, `analysis/deadline_satisfaction.py`)

Same model as §6's bullets, but keeping the full Monte-Carlo distribution of makespans
(1000 workloads per N) and counting the fraction ≤ T_max = 10 ms. **Model-based; say so.**

| N | 100 | 1k | 10k | 40k | 60k |
|---|---|---|---|---|---|
| baseline mean (ms) | 3.22 | 3.20 | 6.10 | 15.78 | 22.15 |
| eDAG mean (ms) | 2.94 | 2.96 | 3.11 | 3.63 | 3.96 |
| baseline / eDAG satisfaction | 100/100 | 100/100 | 100/100 | 0/100 | 0/100 |

**Why 100 % → 0 % with nothing in between.** A deadline is a step function applied to a
narrow distribution. At 60k the baseline makespans span roughly 19–27 ms (std ≈ 1.5 ms)
against a 10 ms deadline: every sample misses. At 10k they span roughly 4.5–8 ms: every
sample meets. The cliff sits between the sampled points: re-running the model gives
**≈ 88 % at N = 20k, ≈ 54 % at 22k, ≈ 10 % at 25k, 0 % at 30k** — the baseline mean crosses
10 ms at about N ≈ 22,000. We sampled 10k and 40k, so the table shows a cliff. Say:
"the crossing is around 22,000 services; the table brackets it."

**The "you picked a flattering deadline" attack.** The paper's Fig. 10(b) (in
`results/deadline_satisfaction.png`, not on the slide) sweeps T_max at N = 60k: eDAG reaches
100 % at about 5 ms, the baseline needs about 25 ms. Any deadline between 5 and 25 ms gives
the same conclusion.

**eDAG is not perfectly flat either** (2.94 → 3.96 ms): that is the 0.546 µs × F serve term
growing with F. It is honest and small.

---

## 8. Slide 20 — Ablation (`ablation_results.csv`, `analysis/ablation.py`)

| configuration | mean makespan | deadline sat |
|---|---|---|
| Full (connect4 + eBPF cache) | 3.951 ms | 100 % |
| Without sender bypass (kube-proxy + eBPF cache) | 3.975 ms | 100 % |
| Without cache (connect4 + app serve) | 22.218 ms | 0 % |
| Without DAG-GC | — | Fails (Proposition 1) |

**"Without sender bypass" ≈ Full — the honest mechanism.** In this model every subtask is a
cache hit (§0 item 2), so the offload substrate is never exercised; the 24 µs difference is
Monte-Carlo seed noise, not offload cost. The slide wisely prints no number for that row
("offload reverts to O(N)"). If pressed:
> "At this workload's cache-hit rate the offload path is not on the critical path, so the
> bypass cannot show up in makespan. Where it is measured on its own — slides 15 and 16 —
> it is 37× and 2.55×. The ablation shows what dominates *this* metric: the cache."

**"Without cache" = 22.2 ms = the baseline row.** Same number because, with all hits, the
baseline's only extra cost *is* the app-level serve.

**"Without deterministic GC" has no number by design.** LRU/TTL can evict a result a later
consumer still needs; in a DAG that stalls the graph rather than costing a re-fetch. It is
a correctness failure, so it is reported as "Fails", not as a latency. Land the talk here.

---

## 9. Trend questions you will get, with the short answer

- **"Why does the eBPF line on slide 16 go down?"** Noise and warm-up, not N; the baseline
  dips early too; the single-host twin is flat to 0.6 µs. Treat it as flat at ~91 µs.
- **"Why is iptables faster than XDP at N = 0?"** Empty chain vs running a BPF program per
  packet in generic mode; crossover ≈ 100 services.
- **"Your cache line grows with δ, so how is it O(1)?"** O(1) per access; total delivery is
  necessarily O(δ). The constant fell 18×.
- **"Why does the ratio on the cache plot rise from 7× to 18×?"** A ~0.9 µs fixed Put+Delete
  cost in the eBPF arm is amortised; the asymptote is 10.18/0.546 ≈ 18.6×.
- **"Why is the Path-2 kernel line not flat?"** The clones run synchronously inside the
  producer's `sendto()` on loopback; one syscall, O(δ) kernel work at 0.72 µs/consumer.
- **"Why is Path-2 slower at δ = 1?"** No fan-out to amortise the clone machinery; crosses
  over at δ = 2.
- **"Why is the C3 serve path faster than routing-only?"** Warm vs cold map entry; compare
  A to B and C to C only.
- **"Why is EDP 197× when delay is only 14×?"** EDP = P·t²; 14.05² = 197. Modeled.
- **"Why 5.6× on the bullets but 14× on the plot?"** Workload model has a 2.9 ms transfer
  floor in both arms; per-task model does not. Same floor argument as slide 16.
- **"Why 100 % → 0 % with no middle?"** Step function on a narrow distribution; crossing at
  N ≈ 22k, which we did not sample.
- **"Why does the bypass do nothing in the ablation?"** All-hit workload; offload is off the
  critical path; measured on its own on slides 15–16.
- **"Did you deploy 60,000 services?"** No. Per-hop costs are measured; the cluster and the
  workload are CachOf's model. Say "modeled" on slides 18, 19, 20 without being asked.
- **"Where is the energy measurement?"** None; EC2 blocks RAPL. CPU proxy result is
  *negative* for software eBPF (+0.11 % task-clock, +13.8 % sys time). Energy win needs
  hardware offload, which is projected, not built.

---

## 10. Numbers card

**Measured (real hardware):**
- Crossover: 4.97 ns/rule; XDP 5.40 ± 0.06 µs; 204.65 / 5.50 = **37.2×** at 40k; crossover ≈ N 100.
- Single-host offload: 2.22 ns/svc + 25.9 µs; connect4 21.06 ± 0.63; 151.2 / 21.2 = **7.13×**; gap 130 µs.
- Cross-VM offload: 1.97 ns/svc + 99.1 µs; connect4 91.3 ± 4.4 (82.9–99.3); 211.2 / 82.9 = **2.55×**; gap 128 µs.
- Cache: app 10.18 µs/consumer, eBPF 0.546 µs/consumer (+0.92); **18.1×** at δ = 64; payload-flat 5.25–5.35 µs.
- C3: routing 9.407, store 10.108 → **+701 ns**; serve 6.95 / 6.89 / 6.57 µs (spread 374 ns); GC **3000/3000**.
- Path-2: app 1.84 µs/consumer; kernel 0.72 µs/consumer + 1.54; **2.24×** at δ = 16; 0.81× at δ = 1.
- Robustness: 1000 chaos runs 501.0 → 431.9 µs (**−13.8 %**); raw captures not in repo.

**Modeled (always say the word):**
- Per-task (slide 18 figure): 768.8 vs 54.7 µs = 14.05×; EDP **197× / 1316×**; F = N/1000.
- Workload (slide 18 bullets, 19, 20): 22.24 vs 3.95 ms = **5.6×**; EDP **102× / 680×**;
  deadline 100 → 0 % between 10k and 40k (crossing ≈ 22k); eDAG 100 % throughout;
  tolerance sweep: eDAG meets at ~5 ms, baseline needs ~25 ms.
- Knobs: P_active = 15 W; HW offload fraction 0.85; cn × 1e-3 fine-grained regime;
  CachOf params verbatim (8 apps, 7 subtasks, rn = 5e8 b/s, cache_capacity 7).

**Projected (not built):** SmartNIC hardware offload.
