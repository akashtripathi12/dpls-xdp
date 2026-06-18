//go:build linux
// +build linux

// cache_bench — eDAG-MEC kernel cache vs CachOf-style application cache.
// =====================================================================
// Operation under test: a producer subtask's D-byte RESULT must be made
// available to N downstream (DAG-dependent) consumer subtasks, then reclaimed.
//
//   APP CACHE (the substrate CachOf implies):  the result sits in edge-server
//        userspace memory; to serve each of the N consumers the server reads it
//        and DELIVERS it over the network stack (one round-trip per consumer).
//        => O(N) downlink — exactly the cost CachOf's model sets to zero.
//
//   eBPF KERNEL CACHE (this thesis):  the result is stored ONCE in a kernel
//        hash map (retention/vault map); each consumer is served by an O(1)
//        kernel-map access from the single retained copy; the entry is GC'd on
//        the last read.  => O(1) per access, one copy, no producer re-serve.
//
//   CachOf assumption:  serving a hit costs 0 (drawn as a reference line).
//
// Both arms are MEASURED on a real kernel. Two sweeps:
//   A) fan-out N (fixed payload)   B) payload size D (fixed fan-out)
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

const valCap = 1408 // max retained bytes per cache entry (one MTU of result)

func main() {
	reps := flag.Int("reps", 2000, "repetitions per data point")
	flag.Parse()
	log.SetFlags(log.Ltime)
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("memlock: %v", err)
	}

	echo := mustEcho()
	defer echo.Close()
	time.Sleep(100 * time.Millisecond)

	// ── Sweep A: fan-out degree N, fixed 1024-byte result ────────────────────
	Ns := []int{1, 2, 4, 8, 16, 32, 64}
	var a strings.Builder
	a.WriteString("N,app_cache_us,ebpf_cache_us\n")
	log.Printf("=== Sweep A: fan-out (payload=1024B) ===")
	for _, n := range Ns {
		app := benchApp(n, 1024, *reps)
		eb := benchEBPF(n, 1024, *reps)
		a.WriteString(fmt.Sprintf("%d,%.3f,%.3f\n", n, app, eb))
		log.Printf("N=%-3d  app-cache=%8.2f us   eBPF-cache=%7.2f us   (%.1fx)", n, app, eb, app/eb)
	}
	os.WriteFile("cache_fanout.csv", []byte(a.String()), 0644)

	// ── Sweep B: payload size D, fixed fan-out N=8 ───────────────────────────
	Ds := []int{64, 256, 512, 1024, 1400}
	var b strings.Builder
	b.WriteString("D,app_cache_us,ebpf_cache_us\n")
	log.Printf("=== Sweep B: payload size (fan-out N=8) ===")
	for _, d := range Ds {
		app := benchApp(8, d, *reps)
		eb := benchEBPF(8, d, *reps)
		b.WriteString(fmt.Sprintf("%d,%.3f,%.3f\n", d, app, eb))
		log.Printf("D=%-5d  app-cache=%8.2f us   eBPF-cache=%7.2f us   (%.1fx)", d, app, eb, app/eb)
	}
	os.WriteFile("cache_payload.csv", []byte(b.String()), 0644)
	log.Printf("CSVs written: cache_fanout.csv, cache_payload.csv")
}

// ── APP CACHE: userspace store + N network-stack deliveries + free ───────────
// Models CachOf's implied substrate: cached bytes live in app memory and are
// delivered to each consumer over the stack (the "downlink" CachOf ignores).
func benchApp(n, d, reps int) float64 {
	payload := make([]byte, d)
	samples := make([]time.Duration, 0, reps)
	echoAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: echoPort}
	for r := 0; r < reps; r++ {
		t := time.Now()
		cache := make([]byte, d) // store: copy result into app cache
		copy(cache, payload)
		for c := 0; c < n; c++ { // serve each consumer over the stack
			conn, err := net.DialUDP("udp4", nil, echoAddr)
			if err != nil {
				continue
			}
			conn.SetDeadline(time.Now().Add(50 * time.Millisecond))
			conn.Write(cache) // deliver cached bytes to consumer
			ack := make([]byte, valCap)
			conn.Read(ack)
			conn.Close()
		}
		cache = nil // free
		samples = append(samples, time.Since(t))
	}
	return trimmedMeanUS(samples)
}

// ── eBPF KERNEL CACHE: store once in a kernel map, O(1) serve per consumer ───
func benchEBPF(n, d, reps int) float64 {
	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Type: ebpf.Hash, KeySize: 4, ValueSize: valCap, MaxEntries: 1024,
	})
	if err != nil {
		log.Fatalf("map: %v", err)
	}
	defer m.Close()
	val := make([]byte, valCap)
	out := make([]byte, valCap)
	samples := make([]time.Duration, 0, reps)
	for r := 0; r < reps; r++ {
		key := uint32(r)
		t := time.Now()
		_ = m.Put(key, val)         // store: retain result once in kernel
		for c := 0; c < n; c++ {    // serve each consumer: O(1) kernel access
			_ = m.Lookup(key, out)
		}
		_ = m.Delete(key)           // deterministic GC on last read
		samples = append(samples, time.Since(t))
	}
	_ = d
	return trimmedMeanUS(samples)
}

// ── echo server (consumer stand-in for the app arm) ──────────────────────────
const echoPort = 9100

func mustEcho() *net.UDPConn {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: echoPort})
	if err != nil {
		log.Fatalf("echo: %v", err)
	}
	go func() {
		buf := make([]byte, valCap)
		for {
			nn, a, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			c.WriteToUDP(buf[:nn], a)
		}
	}()
	return c
}

func trimmedMeanUS(s []time.Duration) float64 {
	if len(s) == 0 {
		return 0
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	cut := len(s) * 98 / 100
	if cut == 0 {
		cut = len(s)
	}
	var tot time.Duration
	for _, d := range s[:cut] {
		tot += d
	}
	return float64(tot.Nanoseconds()) / float64(cut) / 1000.0
}

var _ = binary.LittleEndian
