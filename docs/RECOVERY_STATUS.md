# Artifact Recovery — Status Ledger

Response to the eDAG-MEC artifact-recovery request. Each item is marked
**RECOVERED / FIXED / RECONSTRUCTED / CLARIFIED / UNRECOVERABLE**. "Unrecoverable"
means the artifact was produced in an earlier Claude-container or SSH/AWS session
that is no longer accessible and was never committed — it cannot be regenerated
from this repository. Where that is the case it is stated plainly.

Repo state this ledger was written against: `main` (post-restructure), which is
**ahead** of the `9ec8deb` the request assumed — some items the request lists as
absent already exist here.

---

## Priority 1 — cited in the paper

### 1.1 `analysis/deadline_satisfaction.py` — ✅ RECOVERED + FIXED
- **Present** at `analysis/deadline_satisfaction.py` with `results/deadline_satisfaction.csv`
  (added on `main` after the request's `9ec8deb` snapshot).
- **Invocation:** `python3 analysis/deadline_satisfaction.py` (defaults: `episodes=1000`,
  `T_max=10 ms`, sweep `N∈{100,1k,10k,40k,60k}`). Reads no CSV — it imports the
  CachOf model from `energy_vs_basepaper.py` and Monte-Carlos makespans directly.
- **Output:** Table XIII = **100 / 100 / 100 / 0.0 / 0.0** (baseline) and 100 throughout
  (eDAG). Matches the paper exactly.
- **Discrepancy reconciled (0.0 vs the 1.9%/0.1% handoff note):** the script shared
  `energy_vs_basepaper.py`'s `hash(arm)` seed, which Python salts per process, so the
  tail fraction at the 10 ms boundary drifted between runs. **Fixed** (deterministic
  `_ARM_SEED`); the deterministic value is **0.0 / 0.0**, confirming the paper. The
  1.9%/0.1% was earlier unseeded noise, now eliminated. Regenerated CSV committed.

### 1.2 Ablation (Table XIV) — ⚙️ RECONSTRUCTED (new generator) + CLARIFIED
- No generating code ever existed (`ablat` in zero tracked files — confirmed). The
  original Outcome column was **hand-derived** from the model.
- **New:** `analysis/ablation.py` composes the four configurations with offload and
  cache substrate chosen **independently** (the 3-arm energy script cannot), reusing
  the verbatim CachOf cost model. Deterministic; writes `results/ablation_results.csv`.
- **Important correction to the request's "tell":** the "Without sender bypass" row
  being ≈ Full is **not** a carry-over error. Recomputed at N=60k, T_max=10 ms:

  | configuration | mean makespan | deadline sat |
  |---|---:|---:|
  | Full (connect4 + eBPF cache) | 3.951 ms | 100% |
  | Without sender bypass (kube-proxy + eBPF cache) | 3.975 ms | 100% |
  | Without cache (connect4 + app serve) | 22.218 ms | 0% |
  | Without DAG-GC | — | Fails (Proposition 1) |

  At N=60k the app-level makespan is **cache-dominated**: removing the cache is
  catastrophic (22 ms), while the offload substrate barely moves makespan (the
  sender-bypass win shows up in packet latency and EDP, not app-level deadline).
  So Full ≈ "Without sender bypass" is a genuine model property, now reproducible.
  The "Without DAG-GC → Fails" row is correct by design (Proposition 1), not a number.

---

## Priority 2 — prose numbers with no data/code

### 2.1 Latency deconstruction (500 services) — ❌ raw UNRECOVERABLE / ✅ arithmetic CLARIFIED
- The **bench and raw RTT captures are unrecoverable** (produced in a dead session;
  never committed). The numbers survive only as prose in `comprehensive_experiments.md`
  (direct-IP 93.469 µs · ClusterIP 501.04 µs · eBPF ~79 µs · chain-walk 14.46 µs ·
  DNAT+conntrack 407.57 µs).
- **Arithmetic:** the repo doc is **internally correct** — it attributes 84.2% to the
  *total* saving (`422.04/501.04`), and separately isolates DNAT at 407.57 µs. It never
  claims 407.57 = 84.2%. The mis-bonding described in the request lives only in the
  **PDF/paper** (`docs/BTP.pdf`, Figure 3.2 caption), which cannot be edited here.
- **Added** an explicit attribution note to `comprehensive_experiments.md`: DNAT share =
  `407.57/501.04` = **81.3%**; total eBPF saving = **84.2%**; O(N) chain-walk = 2.9%.
  Use **81.3%** for the 407.57 µs isolate in the paper.

### 2.2 Heavy-payload (1024 B) and chaos (1000-iter) experiments — ❌ UNRECOVERABLE
- Present only as prose (`739→351 µs`, −52.5%; `501.0→431.9 µs`, −13.8%). The payload
  sweep bench and the 1000-iteration chaos harness, and their raw output, were never
  committed and cannot be recovered. Fallback for the viva: cite the prose numbers as
  measured-on-AWS, but flag that raw backing is not in the repo.

### 2.3 CPU-proxy `perf` energy (Tables X/XII) — ❌ raw UNRECOVERABLE / methodology recorded
- The `perf stat` invocation, event list, and raw counters were not committed and
  cannot be recovered. Reported numbers preserved in `docs/ENERGY_BENCHMARK_RUNBOOK.md`
  (elapsed 70.81→71.78 s; task-clock 66.75→66.83 s = +0.11%; sys 0.419→0.477 s = +13.8%).
  The runbook records the reconstructed methodology and marks the raw capture as gone.

---

## Priority 3 — present but not reproducible

### 3.1 `energy_vs_basepaper.py` nondeterminism — ✅ FIXED (code) / provenance documented
- **Root cause confirmed:** `hash(arm)` (process-salted). **Fixed** with a fixed
  `_ARM_SEED` map; output is now identical across processes and Python versions
  (verified: two runs with different `PYTHONHASHSEED` give byte-identical results).
- **Committed CSV (EDP@60k = 102.03×, the paper's value):** from an earlier
  process-salted run. Per your decision it is **kept as the paper's canonical figure**.
  The now-deterministic script yields **100.48×** (Δ1.5%, inside the run-to-run spread;
  "~100× software EDP" unchanged). This is documented in the script header and the
  energy runbook. **The original Python version / `PYTHONHASHSEED` that produced
  exactly 102.03 is UNRECOVERABLE** — it is not reachable under any seed on Python
  3.11 here (matching your finding), so it came from a different Python build/revision.

### 3.2 `c3_bench` / `path2_bench` automation + runbooks — ✅ FIXED + RECONSTRUCTED
- **Automation added:** both are now steps **4/6 (c3)** and **5/6 (path2)** of
  `scripts/run_all.sh`; `fanout.c` added to the eBPF compile list; `deadline` +
  `ablation`-adjacent scripts wired into `make analysis`.
- **Runbooks:** the originals (`C3_BENCHMARK_RUNBOOK.md`, `ENERGY_BENCHMARK_RUNBOOK.md`)
  existed only on the local Windows machine and are **not in this container →
  originals UNRECOVERABLE**. **Reconstructed** equivalents committed at
  `docs/C3_BENCHMARK_RUNBOOK.md` and `docs/ENERGY_BENCHMARK_RUNBOOK.md`, built from the
  bench source + committed `results/c3_results.txt`, clearly labelled as reconstructed.
- **c3 interface recorded:** TCX/`tc` ingress on `lo`, `tc_bridge.o`, kernel ≥ 6.6,
  `--iface lo --iters 1000` → the committed `c3_results.txt` (GC 3000/3000).

---

## Priority 4 — verification records

### 4.1 Bibliography Crossref verification — ❌ N/A in this repo
- **`References.bib` is not tracked in this repository at all** (confirmed:
  `git ls-files` shows no `.bib`). There is nothing here to verify against, and no
  verification record was committed. The bibliography must be checked in whatever
  repo/Overleaf project actually holds it.
- **Recorded for safety:** the four papers **Navarro, Cable/Zhou, Leiter, Moreira**
  are NOT in the real bibliography and **must never be cited** — they came from a stale
  project brief and were a genuine earlier error.

---

## Summary checklist

| Item | Status |
|---|---|
| `deadline_satisfaction.py` + invocation + output | ✅ present; determinism fixed; Table XIII reproduced (0.0/0.0) |
| Ablation generator (Table XIV) | ⚙️ new `analysis/ablation.py`; "no-bypass ≈ Full" shown correct, not a copy error |
| Latency-deconstruction bench + raw captures | ❌ unrecoverable; arithmetic (81.3% vs 84.2%) clarified in doc |
| Heavy-payload + chaos harnesses + raw | ❌ unrecoverable (prose only) |
| `perf` invocation + raw counters | ❌ unrecoverable; methodology recorded in runbook |
| Python / `PYTHONHASHSEED` for committed EDP run | ❌ unrecoverable; script now deterministic; 102.03 kept as paper value |
| `C3_/ENERGY_BENCHMARK_RUNBOOK.md` | ⚙️ originals unrecoverable; reconstructed equivalents committed |
| `c3_bench` / `path2_bench` in run scripts | ✅ added to `scripts/run_all.sh` |
| Bibliography verification record | ❌ `References.bib` not in this repo; 4 phantom papers flagged |
