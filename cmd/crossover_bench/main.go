//go:build linux
// +build linux

// crossover_bench — The O(N) Crossover Experiment for eDAG-MEC.
// =============================================================
// Measures per-packet routing-decision cost as a function of cluster size N:
//
//   ARM A (netfilter / iptables):  a packet to the target service must walk
//       N service rules in a chain before it is routed  =>  O(N) per packet.
//   ARM B (XDP / eBPF):            the same packet triggers ONE hash-map
//       lookup in a map holding N entries                =>  O(1) per packet.
//
// Both arms carry an identical UDP echo round-trip on loopback; the ONLY
// difference is the routing-decision mechanism. Sweeping N exposes the
// crossover point beyond which the O(1) eBPF data plane wins, and shows the
// gap widening without bound — the core scalability claim of the thesis.
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
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

const port = 9000

func main() {
	pings := flag.Int("pings", 3000, "UDP echo round-trips per measurement")
	xdpObj := flag.String("xdp", "internal/ebpf/c/xdp_lookup.o", "compiled XDP object")
	csvPath := flag.String("csv", "crossover_results.csv", "output CSV")
	flag.Parse()
	log.SetFlags(log.Ltime)

	Ns := []int{0, 100, 500, 1000, 2000, 5000, 10000, 20000, 40000}

	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("memlock: %v", err)
	}
	echo := mustEcho()
	defer echo.Close()
	time.Sleep(100 * time.Millisecond)

	// Warm up the path so first-touch costs don't pollute N=0.
	for i := 0; i < 500; i++ {
		ping()
	}

	type row struct{ n int; ipt, xdp float64 }
	var rows []row

	for _, n := range Ns {
		ipt := measureIptables(n, *pings)
		xdp := measureXDP(*xdpObj, n, *pings)
		rows = append(rows, row{n, ipt, xdp})
		log.Printf("N=%-6d  iptables=%8.2f us  xdp=%8.2f us  gap=%+7.2f us",
			n, ipt, xdp, ipt-xdp)
	}
	clearIptables()

	// CSV
	var b strings.Builder
	b.WriteString("N,iptables_us,xdp_us,gap_us\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("%d,%.3f,%.3f,%.3f\n", r.n, r.ipt, r.xdp, r.ipt-r.xdp))
	}
	os.WriteFile(*csvPath, []byte(b.String()), 0644)

	// Crossover detection
	cross := -1
	for _, r := range rows {
		if r.ipt > r.xdp && cross == -1 {
			cross = r.n
		}
	}
	log.Printf("=========================================================")
	log.Printf("CSV written to %s", *csvPath)
	if cross >= 0 {
		log.Printf("CROSSOVER: eBPF/XDP O(1) overtakes iptables O(N) at N>=%d services", cross)
	}
	last := rows[len(rows)-1]
	log.Printf("At N=%d:  iptables=%.1fus  xdp=%.1fus  => XDP is %.1fx faster, gap=%.1fus",
		last.n, last.ipt, last.xdp, last.ipt/last.xdp, last.ipt-last.xdp)
	log.Printf("=========================================================")
}

// ── ARM A: netfilter O(N) ────────────────────────────────────────────────────
// Build a filter table where a packet to udp/9000 enters chain DPLS_SVC and
// walks N non-matching service rules (worst case) before RETURN. Plain rules,
// no conntrack state-match, fresh src port per ping => O(N) is paid per packet.
func measureIptables(n, pings int) float64 {
	loadIptables(n)
	return measure(pings)
}

func loadIptables(n int) {
	var b strings.Builder
	b.WriteString("*filter\n:INPUT ACCEPT [0:0]\n:FORWARD ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\n:DPLS_SVC - [0:0]\n")
	b.WriteString(fmt.Sprintf("-A OUTPUT -p udp --dport %d -j DPLS_SVC\n", port))
	for i := 0; i < n; i++ {
		// dummy ClusterIP-like dests in 10.0.0.0/8 — never match 127.0.0.1
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		b.WriteString(fmt.Sprintf("-A DPLS_SVC -d %s/32 -j RETURN\n", ip))
	}
	b.WriteString("COMMIT\n")
	cmd := exec.Command("iptables-restore")
	cmd.Stdin = strings.NewReader(b.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Fatalf("iptables-restore N=%d: %v\n%s", n, err, out)
	}
}

func clearIptables() {
	cmd := exec.Command("iptables-restore")
	cmd.Stdin = strings.NewReader("*filter\n:INPUT ACCEPT [0:0]\n:FORWARD ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\nCOMMIT\n")
	cmd.Run()
}

// ── ARM B: XDP O(1) ──────────────────────────────────────────────────────────
// Attach the one-lookup XDP program to lo with the svc_map pre-filled to N
// entries, so memory footprint matches arm A but the lookup stays O(1). No
// iptables rules present.
func measureXDP(obj string, n, pings int) float64 {
	clearIptables()
	spec, err := ebpf.LoadCollectionSpec(obj)
	if err != nil {
		log.Fatalf("load spec: %v", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		log.Fatalf("new coll: %v", err)
	}
	defer coll.Close()
	m := coll.Maps["svc_map"]
	for i := 0; i < n; i++ {
		k := uint32(i)
		v := uint32(i)
		_ = m.Put(k, v)
	}
	prog := coll.Programs["xdp_lookup"]
	l, err := link.AttachXDP(link.XDPOptions{Program: prog, Interface: 1, Flags: link.XDPGenericMode})
	if err != nil {
		log.Fatalf("xdp attach: %v", err)
	}
	defer l.Close()
	return measure(pings)
}

// ── Shared UDP echo measurement ──────────────────────────────────────────────
func measure(pings int) float64 {
	samples := make([]time.Duration, 0, pings)
	for i := 0; i < pings; i++ {
		if d, ok := ping(); ok {
			samples = append(samples, d)
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	// trimmed mean (drop top 2% to suppress scheduler jitter spikes)
	cut := len(samples) * 98 / 100
	var tot time.Duration
	for _, d := range samples[:cut] {
		tot += d
	}
	return float64(tot.Nanoseconds()) / float64(cut) / 1000.0 // microseconds
}

func ping() (time.Duration, bool) {
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	c, err := net.ListenPacket("udp4", "127.0.0.1:0") // fresh src port => new flow
	if err != nil {
		return 0, false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(50 * time.Millisecond))
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, 1)
	t := time.Now()
	if _, err := c.WriteTo(p, dst); err != nil {
		return 0, false
	}
	buf := make([]byte, 16)
	if _, _, err := c.ReadFrom(buf); err != nil {
		return 0, false
	}
	return time.Since(t), true
}

func mustEcho() *net.UDPConn {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil {
		log.Fatalf("echo listen: %v", err)
	}
	go func() {
		buf := make([]byte, 64)
		for {
			n, a, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			c.WriteToUDP(buf[:n], a)
		}
	}()
	return c
}
