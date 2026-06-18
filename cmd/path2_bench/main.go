//go:build linux
// +build linux

// path2_bench — C3 Path-2: kernel-side fan-out DELIVERY (HANDOFF §6-B Path-2).
// ===========================================================================
// Compares the PRODUCER-SIDE cost of delivering one subtask result to N DAG
// consumers:
//   APP path     : producer issues N userspace sendto() calls            => O(N)
//   eDAG Path-2  : producer issues ONE sendto(); the TC/eBPF fanout.c hook
//                  clones it to N consumers inside the kernel             => O(1)
// Both are verified to actually DELIVER to all N consumers (correctness), so the
// O(1) producer cost is not free-lunch accounting — the fan-out really happens.
//
//   sudo ./path2_bench --reps 2000 --csv path2_results.csv
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sort"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

const (
	triggerPort  = 9000
	consumerPort = 9001
	maxFanout    = 16
	elf          = "internal/ebpf/c/fanout.o"
)

type fanoutRule struct {
	N   uint32
	Dst [maxFanout]uint32
}

func main() {
	reps := flag.Int("reps", 2000, "repetitions per fan-out degree")
	csvPath := flag.String("csv", "path2_results.csv", "output CSV")
	flag.Parse()
	log.SetFlags(log.Ltime)

	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("memlock: %v", err)
	}
	// Clones are re-injected on lo ingress carrying a local source IP; the kernel
	// drops such "martian" packets unless accept_local is enabled. Required for
	// kernel-side fan-out delivery to reach the consumer sockets.
	_ = exec.Command("sysctl", "-w", "net.ipv4.conf.all.accept_local=1").Run()
	spec, err := ebpf.LoadCollectionSpec(elf)
	if err != nil {
		log.Fatalf("load spec (compile fanout.o?): %v", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		log.Fatalf("verifier rejected fanout.o: %v", err)
	}
	defer coll.Close()
	fmap := coll.Maps["fanout_map"]

	lo, err := net.InterfaceByName("lo")
	if err != nil {
		log.Fatalf("lo: %v", err)
	}
	l, err := link.AttachTCX(link.TCXOptions{
		Interface: lo.Index,
		Program:   coll.Programs["dpls_fanout"],
		Attach:    ebpf.AttachTCXIngress,
	})
	if err != nil {
		log.Fatalf("TCX attach: %v", err)
	}
	defer l.Close()
	log.Printf("fanout.o attached to lo ingress ✓")

	Ns := []int{1, 2, 4, 8, 16}
	type row struct {
		N                      int
		appUs, kerUs           float64
		appRecv, kerRecv       int
	}
	rows := make([]row, 0, len(Ns))

	for _, n := range Ns {
		// consumer sockets bound to 127.0.0.{2+i}:9001
		cons := make([]*net.UDPConn, n)
		dsts := make([]*net.UDPAddr, n)
		var rule fanoutRule
		rule.N = uint32(n)
		for i := 0; i < n; i++ {
			ipStr := fmt.Sprintf("127.0.0.%d", 2+i)
			ca, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", ipStr, consumerPort))
			c, err := net.ListenUDP("udp4", ca)
			if err != nil {
				log.Fatalf("bind %s: %v", ca, err)
			}
			cons[i] = c
			dsts[i], _ = net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", ipStr, consumerPort))
			rule.Dst[i] = ipBE(ipStr)
		}

		taskID := uint32(1000 + n)
		_ = fmap.Put(taskID, &rule)
		trig, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("127.0.0.1:%d", triggerPort))
		pay := make([]byte, 4)
		binary.LittleEndian.PutUint32(pay, taskID)

		// ---- KERNEL Path-2: one send, kernel clones to N ----
		kerSamples := make([]time.Duration, 0, *reps)
		kerRecv := 0
		for r := 0; r < *reps; r++ {
			pc, _ := net.DialUDP("udp4", nil, trig)
			t := time.Now()
			pc.Write(pay) // ONE send — O(1) producer cost
			kerSamples = append(kerSamples, time.Since(t))
			pc.Close()
			kerRecv += drain(cons, 5*time.Millisecond)
		}

		// ---- APP path: producer sends N times directly ----
		appSamples := make([]time.Duration, 0, *reps)
		appRecv := 0
		for r := 0; r < *reps; r++ {
			pc, _ := net.ListenUDP("udp4", nil)
			t := time.Now()
			for i := 0; i < n; i++ {
				pc.WriteToUDP(pay, dsts[i]) // N sends — O(N) producer cost
			}
			appSamples = append(appSamples, time.Since(t))
			pc.Close()
			appRecv += drain(cons, 5*time.Millisecond)
		}

		for _, c := range cons {
			c.Close()
		}
		fmap.Delete(taskID)

		kr := row{n, mean(appSamples), mean(kerSamples), appRecv / *reps, kerRecv / *reps}
		rows = append(rows, kr)
		log.Printf("N=%-2d  app(O(N))=%7.2f us [recv %d/%d]   kernel-Path2(O(1))=%7.2f us [recv %d/%d]   %.1fx",
			n, kr.appUs, kr.appRecv, n, kr.kerUs, kr.kerRecv, n, kr.appUs/kr.kerUs)
	}

	var b []byte
	b = append(b, []byte("N,app_producer_us,kernel_producer_us,app_delivered,kernel_delivered,speedup\n")...)
	for _, r := range rows {
		b = append(b, []byte(fmt.Sprintf("%d,%.3f,%.3f,%d,%d,%.3f\n",
			r.N, r.appUs, r.kerUs, r.appRecv, r.kerRecv, r.appUs/r.kerUs))...)
	}
	if err := os.WriteFile(*csvPath, b, 0644); err != nil {
		log.Fatalf("csv: %v", err)
	}
	log.Printf("wrote %s", *csvPath)
}

// drain reads all currently-available datagrams across the consumer sockets,
// returning how many were received within the deadline.
func drain(cons []*net.UDPConn, d time.Duration) int {
	got := 0
	buf := make([]byte, 64)
	for _, c := range cons {
		c.SetReadDeadline(time.Now().Add(d))
		if _, _, err := c.ReadFromUDP(buf); err == nil {
			got++
		}
	}
	return got
}

func mean(s []time.Duration) float64 {
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

func ipBE(s string) uint32 {
	ip := net.ParseIP(s).To4()
	return binary.LittleEndian.Uint32(ip)
}
