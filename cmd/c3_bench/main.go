//go:build linux
// +build linux

// cmd/c3_bench — Configuration C3 (DAG-Aware Fan-Out Retention) Benchmark
// =======================================================================
// Project: eBPF-Accelerated Dependency-Aware Task Offloading in MEC
//
// WHAT THIS PROVES (Path 1 — "cheap & correct"):
//  1. OVERHEAD:   The kernel retention path (ref_count > 1: store payload,
//     ref-count, serve, GC) adds negligible latency versus the
//     plain routing path (ref_count == 1, no retention).
//  2. O(1) SCALE: Per-access latency is independent of the fan-out degree N
//     (N = 2, 3, 4) — retention is a single hash-map operation,
//     not a per-consumer loop through the stack.
//  3. CORRECTNESS:The atomic ref-count countdown + delete-on-zero performs
//     deterministic kernel garbage collection — retention_map
//     occupancy returns to zero after the last consumer is served.
//
// WHAT THIS DELIBERATELY DOES NOT CLAIM:
//
//	It does not claim measured "N-1 re-transmissions avoided." That requires
//	real kernel-side DELIVERY of the retained bytes to each consumer
//	(bpf_clone_redirect / bpf_redirect), which is Path 2 / future work. See
//	DEFENSE_DOSSIER.md §5 and §6. This harness measures the COST and
//	CORRECTNESS of the retention mechanism, which is what Path 1 sets out to do.
//
// WHY LOOPBACK (lo):
//
//	We send triggers to 127.0.0.1:9000 and run an in-process echo server there.
//	The TC ingress program is attached to `lo`, so every trigger traverses the
//	eBPF retention code. Loopback removes cross-node network variance so the
//	measured delta is the eBPF processing cost itself — exactly what the
//	"retention is cheap" claim needs. Absolute cross-node latency is already
//	established by Experiments 1-3 in comprehensive_experiments.md.
//
// REQUIREMENTS:
//   - Linux with a real kernel (>= 5.15). Run as root (BPF attach + map writes).
//   - tc_bridge.o compiled:
//     clang -target bpf -O2 -I /usr/include/$(uname -m)-linux-gnu \
//     -c internal/ebpf/c/tc_bridge.c -o internal/ebpf/c/tc_bridge.o
//
// USAGE:
//
//	go build -o c3_bench ./cmd/c3_bench
//	sudo ./c3_bench --iface lo --iters 1000
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"sort"
	"time"

	"dpls-xdp/internal/ebpf"
	"dpls-xdp/pkg/api"
)

const triggerPort = 9000

func main() {
	elfPath := flag.String("elf", "internal/ebpf/c/tc_bridge.o", "Path to compiled tc_bridge.o")
	iface := flag.String("iface", "lo", "Interface to attach the TC program to")
	iters := flag.Int("iters", 1000, "Iterations per sub-benchmark")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// ── Load + attach the eBPF data plane ────────────────────────────────────
	log.Printf("[C3 Bench] Loading BPF objects from %s", *elfPath)
	if err := ebpf.LoadBPFObjects(*elfPath); err != nil {
		log.Fatalf("[C3 Bench] load failed (compile tc_bridge.o first?): %v", err)
	}
	log.Printf("[C3 Bench] Attaching TC ingress to %s", *iface)
	if err := ebpf.AttachTC(*iface); err != nil {
		log.Fatalf("[C3 Bench] TC attach failed (run as root?): %v", err)
	}
	defer ebpf.DetachTC()

	// ── In-process echo server on :9000 so every trigger gets an ACK ──────────
	// This stands in for worker_listener; keeping it in-process makes the
	// benchmark single-node and reproducible.
	echo, err := startEchoServer()
	if err != nil {
		log.Fatalf("[C3 Bench] could not start echo server on :%d: %v", triggerPort, err)
	}
	defer echo.Close()
	time.Sleep(100 * time.Millisecond) // let the listener settle

	log.Printf("[C3 Bench] ───────────────────────────────────────────────────")
	log.Printf("[C3 Bench] Sub-benchmark A: ROUTING-ONLY path (ref_count = 1)")
	routing := runPath(*iters, 1 /*refCount*/, 0 /*idBase*/)
	report("A. Routing-only (ref_count=1)", routing)

	log.Printf("[C3 Bench] Sub-benchmark B: RETENTION STORE path (ref_count = 4, first hit)")
	store := runPath(*iters, 4 /*refCount*/, 1_000_000 /*idBase*/)
	report("B. Retention store (ref_count=4)", store)

	log.Printf("[C3 Bench] Sub-benchmark C: O(1) fan-out scaling + GC correctness")
	scaling := map[int]stats{}
	gcOK, gcTotal := 0, 0
	for _, n := range []int{2, 3, 4} {
		s, ok, total := runFanout(*iters, n, 10_000_000+n*1_000_000)
		scaling[n] = s
		gcOK += ok
		gcTotal += total
		report(fmt.Sprintf("C. Fan-out serve path (N=%d)", n), s)
	}

	// ── Final summary ─────────────────────────────────────────────────────────
	log.Printf("[C3 Bench] ===========================================================")
	log.Printf("[C3 Bench] SUMMARY")
	log.Printf("[C3 Bench] -----------------------------------------------------------")
	overhead := store.mean - routing.mean
	log.Printf("[C3 Bench] Retention overhead vs routing-only: %v (store %v - routing %v)",
		overhead, store.mean, routing.mean)
	log.Printf("[C3 Bench] O(1) fan-out scaling (serve-path mean RTT):")
	for _, n := range []int{2, 3, 4} {
		log.Printf("[C3 Bench]    N=%d  mean=%v  p95=%v  p99=%v",
			n, scaling[n].mean, scaling[n].p95, scaling[n].p99)
	}
	spread := maxMean(scaling) - minMean(scaling)
	log.Printf("[C3 Bench] Scaling spread across N=2..4: %v (smaller => more O(1))", spread)
	log.Printf("[C3 Bench] Deterministic GC: %d/%d fan-out tasks fully reclaimed "+
		"(retention_map empty after last consumer)", gcOK, gcTotal)
	log.Printf("[C3 Bench] ===========================================================")
	log.Printf("[C3 Bench] Interpretation:")
	log.Printf("[C3 Bench]  - overhead ~ tens of microseconds or less => retention is cheap (C5).")
	log.Printf("[C3 Bench]  - scaling spread small => per-access cost is O(1) in fan-out degree.")
	log.Printf("[C3 Bench]  - GC = %d/%d => atomic ref-count + delete-on-zero works on a real kernel.",
		gcOK, gcTotal)
}

// ─────────────────────────────────────────────────────────────────────────────
// runPath: program a rule with the given refCount, then send `iters` triggers
// (each a fresh task ID) and record per-trigger RTT. Used for the routing-only
// and retention-store sub-benchmarks.
// ─────────────────────────────────────────────────────────────────────────────
func runPath(iters, refCount, idBase int) stats {
	samples := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		taskID := uint32(idBase + i)
		programRule(taskID, refCount)
		if d, ok := sendTrigger(taskID); ok {
			samples = append(samples, d)
		}
	}
	return summarize(samples)
}

// ─────────────────────────────────────────────────────────────────────────────
// runFanout: for each task, send 1 store trigger (warm the retention entry) then
// N-1 serve triggers (each decrements the kernel ref-count). After the Nth
// access, verify retention_map[task] has been deleted (deterministic GC).
// Returns serve-path latency stats, GC-correct count, and total tasks.
// ─────────────────────────────────────────────────────────────────────────────
func runFanout(iters, n, idBase int) (stats, int, int) {
	samples := make([]time.Duration, 0, iters*n)
	gcOK := 0
	for i := 0; i < iters; i++ {
		taskID := uint32(idBase + i)
		programRule(taskID, n)

		// Producer transmits once → kernel stores payload, remaining = n
		// (the C code sets remaining_consumers = ref_count on the first hit).
		sendTrigger(taskID)

		// Confirm the entry was actually POPULATED by the TC hook. This rules out
		// a false GC pass where the entry simply never existed (e.g. the hook
		// didn't fire) — GetRetainedPayloadCount returns 0 for "absent" too.
		populated, _ := ebpf.GetRetainedPayloadCount(taskID)

		// N consumers access → each decrements remaining; the n-th decrement
		// hits 0 and the C code deletes the entry (deterministic GC).
		for c := 0; c < n; c++ {
			if d, ok := sendTrigger(taskID); ok {
				samples = append(samples, d)
			}
		}

		// GC success = we saw it populated (remaining > 0) AND it is now gone
		// (remaining == 0 / key deleted) after exactly N serves.
		reclaimed, _ := ebpf.GetRetainedPayloadCount(taskID)
		if populated > 0 && reclaimed == 0 {
			gcOK++
		}
	}
	return summarize(samples), gcOK, iters
}

// programRule writes vault_map[taskID] = {refCount, dests=[127.0.0.1 ...]}.
// All destinations are loopback so the C daddr-rewrite is a no-op (old==new)
// and the trigger always reaches the local echo server.
func programRule(taskID uint32, refCount int) {
	dests := make([]string, refCount)
	for i := range dests {
		dests[i] = "127.0.0.1"
	}
	_ = ebpf.WriteDependencyRuleToKernel(api.DependencyRule{
		SubtaskID:    taskID,
		RefCount:     uint32(refCount),
		Destinations: dests,
	})
}

// sendTrigger sends one UDP trigger (payload = little-endian taskID) to
// 127.0.0.1:9000 and blocks for the echoed ACK, returning the round-trip time.
// The packet traverses the TC ingress hook on lo, exercising the retention code.
func sendTrigger(taskID uint32) (time.Duration, bool) {
	dst, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("127.0.0.1:%d", triggerPort))
	if err != nil {
		return 0, false
	}
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(50 * time.Millisecond))

	payload := make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, taskID)

	start := time.Now()
	if _, err := conn.WriteTo(payload, dst); err != nil {
		return 0, false
	}
	ack := make([]byte, 64)
	if _, _, err := conn.ReadFrom(ack); err != nil {
		return 0, false
	}
	return time.Since(start), true
}

// ─────────────────────────────────────────────────────────────────────────────
// Echo server
// ─────────────────────────────────────────────────────────────────────────────
func startEchoServer() (*net.UDPConn, error) {
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", triggerPort))
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		buf := make([]byte, 512)
		for {
			n, remote, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // closed on shutdown
			}
			_, _ = conn.WriteToUDP(buf[:n], remote)
		}
	}()
	return conn, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Stats helpers
// ─────────────────────────────────────────────────────────────────────────────
type stats struct {
	n    int
	mean time.Duration
	p50  time.Duration
	p95  time.Duration
	p99  time.Duration
}

func summarize(samples []time.Duration) stats {
	if len(samples) == 0 {
		return stats{}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var total time.Duration
	for _, d := range samples {
		total += d
	}
	pct := func(p float64) time.Duration {
		idx := int(p * float64(len(samples)))
		if idx >= len(samples) {
			idx = len(samples) - 1
		}
		return samples[idx]
	}
	return stats{
		n:    len(samples),
		mean: total / time.Duration(len(samples)),
		p50:  pct(0.50),
		p95:  pct(0.95),
		p99:  pct(0.99),
	}
}

func report(label string, s stats) {
	log.Printf("[C3 Bench]   %-34s n=%-5d mean=%-10v p50=%-10v p95=%-10v p99=%v",
		label, s.n, s.mean, s.p50, s.p95, s.p99)
}

func maxMean(m map[int]stats) time.Duration {
	var mx time.Duration
	for _, s := range m {
		if s.mean > mx {
			mx = s.mean
		}
	}
	return mx
}

func minMean(m map[int]stats) time.Duration {
	mn := time.Duration(1<<62 - 1)
	for _, s := range m {
		if s.mean < mn {
			mn = s.mean
		}
	}
	if mn == time.Duration(1<<62-1) {
		return 0
	}
	return mn
}
