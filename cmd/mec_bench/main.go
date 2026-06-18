//go:build linux
// +build linux

// mec_bench — Multi-node MEC benchmark using the REAL eDAG-MEC data plane.
// =======================================================================
// Topology (emulated edge cluster, NOT loopback):
//
//   [ root netns: SCHEDULER node ]        [ netns mec-worker: WORKER node ]
//        veth0  10.200.0.1/24  <==== veth link ====>  veth1  10.200.0.2/24
//        cgroup2 /tmp/cg2/dpls                         UDP echo :9000
//
// The scheduler dials a VIRTUAL service IP (10.96.0.10 — a kube ClusterIP).
// Two arms route that virtual IP to the real worker node across the veth:
//
//   BASELINE (kube-proxy):  iptables nat DNAT 10.96.0.10 -> 10.200.0.2, sitting
//        behind N dummy KUBE-SERVICES rules  =>  O(N) walk + conntrack DNAT.
//   eDAG-MEC (eBPF):        cgroup/connect4 rewrites the dest to 10.200.0.2
//        before netfilter, via one O(1) vault lookup  =>  no DNAT, no conntrack.
//
// Every packet is a real cross-node round-trip over the veth. Sweeping N shows
// where the O(1) sender-side bypass overtakes the O(N) netfilter path.
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

const (
	workerNS  = "mec-worker"
	schedIP   = "10.200.0.1"
	workerIP  = "10.200.0.2"
	svcIP     = "10.96.0.10" // virtual ClusterIP
	port      = 9000
	cgPath    = "/tmp/cg2/dpls"
	connElf   = "internal/ebpf/c/connect4.o"
)

func main() {
	role := flag.String("role", "orchestrator", "orchestrator | worker")
	pings := flag.Int("pings", 3000, "round-trips per measurement")
	csvPath := flag.String("csv", "mec_results.csv", "output CSV")
	flag.Parse()
	log.SetFlags(log.Ltime)

	if *role == "worker" {
		runWorker()
		return
	}
	orchestrate(*pings, *csvPath)
}

// ── WORKER node: echo server bound to the worker's real IP ───────────────────
func runWorker() {
	addr := &net.UDPAddr{IP: net.ParseIP(workerIP), Port: port}
	c, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Fatalf("[worker] listen %s: %v", workerIP, err)
	}
	log.Printf("[worker] echo up on %s:%d", workerIP, port)
	buf := make([]byte, 64)
	for {
		n, a, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		c.WriteToUDP(buf[:n], a)
	}
}

// ── ORCHESTRATOR: build topology, run both arms, sweep N ─────────────────────
func orchestrate(pings int, csvPath string) {
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("memlock: %v", err)
	}
	setupTopology()
	defer teardownTopology()

	self, _ := os.Executable()
	worker := exec.Command("ip", "netns", "exec", workerNS, self, "--role", "worker")
	worker.Stdout, worker.Stderr = os.Stderr, os.Stderr
	if err := worker.Start(); err != nil {
		log.Fatalf("start worker: %v", err)
	}
	defer worker.Process.Kill()
	time.Sleep(400 * time.Millisecond)

	// Put THIS process into the cgroup2 subtree connect4 will be attached to.
	if err := os.WriteFile(cgPath+"/cgroup.procs",
		[]byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		log.Fatalf("join cgroup: %v", err)
	}

	// sanity: confirm the worker is reachable directly first
	if d, ok := pingTo(workerIP); ok {
		log.Printf("[sanity] direct cross-node RTT to worker = %.2f us", us(d))
	} else {
		log.Fatalf("[sanity] worker unreachable across veth — topology broken")
	}

	Ns := []int{0, 100, 500, 1000, 2000, 5000, 10000, 20000, 40000, 60000}
	type row struct {
		n          int
		base, ebpf float64
	}
	var rows []row
	for _, n := range Ns {
		base := measureBaseline(n, pings)
		eb := measureEBPF(n, pings)
		rows = append(rows, row{n, base, eb})
		log.Printf("N=%-6d  kube-proxy(DNAT+O(N))=%8.2f us   eBPF connect4 O(1)=%8.2f us   gap=%+8.2f us  (%.1fx)",
			n, base, eb, base-eb, base/eb)
	}
	clearNAT()

	var b strings.Builder
	b.WriteString("N,kubeproxy_us,ebpf_connect4_us,gap_us,speedup\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("%d,%.3f,%.3f,%.3f,%.3f\n",
			r.n, r.base, r.ebpf, r.base-r.ebpf, r.base/r.ebpf))
	}
	os.WriteFile(csvPath, []byte(b.String()), 0644)
	last := rows[len(rows)-1]
	log.Printf("=========================================================")
	log.Printf("CSV -> %s", csvPath)
	log.Printf("At N=%d:  kube-proxy=%.1fus  eBPF=%.1fus  => %.1fx faster, gap=%.1fus",
		last.n, last.base, last.ebpf, last.base/last.ebpf, last.base-last.ebpf)
	log.Printf("=========================================================")
}

// ── ARM A: kube-proxy baseline (iptables nat DNAT behind N service rules) ────
func measureBaseline(n, pings int) float64 {
	loadNAT(n)
	defer clearNAT()
	return measure(svcIP, pings) // dial the ClusterIP; DNAT sends it to worker
}

func loadNAT(n int) {
	var b strings.Builder
	b.WriteString("*nat\n:PREROUTING ACCEPT [0:0]\n:INPUT ACCEPT [0:0]\n")
	b.WriteString(":OUTPUT ACCEPT [0:0]\n:POSTROUTING ACCEPT [0:0]\n:KUBE-SERVICES - [0:0]\n")
	b.WriteString("-A OUTPUT -j KUBE-SERVICES\n")
	// N dummy services walked first (worst case), real service rule last.
	for i := 0; i < n; i++ {
		ip := fmt.Sprintf("10.99.%d.%d", (i>>8)&0xff, i&0xff)
		b.WriteString(fmt.Sprintf("-A KUBE-SERVICES -d %s/32 -p udp --dport %d -j RETURN\n", ip, port))
	}
	b.WriteString(fmt.Sprintf("-A KUBE-SERVICES -d %s/32 -p udp --dport %d -j DNAT --to-destination %s:%d\n",
		svcIP, port, workerIP, port))
	b.WriteString("COMMIT\n")
	run("iptables-restore", b.String())
}

func clearNAT() {
	run("iptables-restore", "*nat\n:PREROUTING ACCEPT [0:0]\n:INPUT ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\n:POSTROUTING ACCEPT [0:0]\nCOMMIT\n")
}

// ── ARM B: eDAG-MEC eBPF (cgroup/connect4 sender-side rewrite, O(1)) ──────────
func measureEBPF(n, pings int) float64 {
	clearNAT()
	spec, err := ebpf.LoadCollectionSpec(connElf)
	if err != nil {
		log.Fatalf("spec: %v", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		log.Fatalf("coll: %v", err)
	}
	defer coll.Close()

	vault := coll.Maps["vault"]
	// Program the real service -> worker mapping, plus N-1 dummy services so the
	// map footprint matches the baseline's N rules (lookup stays O(1)).
	_ = vault.Put(ipBE(svcIP), ipBE(workerIP))
	for i := 0; i < n; i++ {
		k := beUint32(0x0a630000 + uint32(i)) // 10.99.x.x dummies
		_ = vault.Put(k, ipBE(workerIP))
	}
	l, err := link.AttachCgroup(link.CgroupOptions{
		Path:    "/tmp/cg2/dpls",
		Attach:  ebpf.AttachCGroupInet4Connect,
		Program: coll.Programs["connect4"],
	})
	if err != nil {
		log.Fatalf("connect4 attach: %v", err)
	}
	defer l.Close()
	return measure(svcIP, pings) // dial ClusterIP; connect4 rewrites to worker
}

// ── Shared cross-node UDP measurement ────────────────────────────────────────
func measure(dstIP string, pings int) float64 {
	samples := make([]time.Duration, 0, pings)
	for i := 0; i < pings; i++ {
		if d, ok := pingTo(dstIP); ok {
			samples = append(samples, d)
		}
	}
	if len(samples) == 0 {
		log.Fatalf("no samples to %s (path broken)", dstIP)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	cut := len(samples) * 98 / 100
	if cut == 0 {
		cut = len(samples)
	}
	var tot time.Duration
	for _, d := range samples[:cut] {
		tot += d
	}
	return float64(tot.Nanoseconds()) / float64(cut) / 1000.0
}

// pingTo dials dstIP:9000 (connected UDP => connect() fires the cgroup hook),
// sends 4 bytes, waits for the echo. Fresh socket per call = new flow.
func pingTo(dstIP string) (time.Duration, bool) {
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(dstIP), Port: port})
	if err != nil {
		return 0, false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(100 * time.Millisecond))
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, 1)
	t := time.Now()
	if _, err := c.Write(p); err != nil {
		return 0, false
	}
	buf := make([]byte, 16)
	if _, err := c.Read(buf); err != nil {
		return 0, false
	}
	return time.Since(t), true
}

// ── topology setup/teardown ──────────────────────────────────────────────────
func setupTopology() {
	teardownTopology() // clean slate
	mustRun("mount", "-t", "cgroup2", "none", "/tmp/cg2") // idempotent-ish
	os.MkdirAll("/tmp/cg2/dpls", 0755)
	sh("ip netns add " + workerNS)
	sh("ip link add veth0 type veth peer name veth1")
	sh("ip link set veth1 netns " + workerNS)
	sh("ip addr add " + schedIP + "/24 dev veth0")
	sh("ip link set veth0 up")
	sh("ip netns exec " + workerNS + " ip link set lo up")
	sh("ip netns exec " + workerNS + " ip addr add " + workerIP + "/24 dev veth1")
	sh("ip netns exec " + workerNS + " ip link set veth1 up")
	// route the ClusterIP range out the veth so OUTPUT routing succeeds pre-DNAT
	sh("ip route add 10.96.0.0/16 dev veth0")
}

func teardownTopology() {
	sh("ip link del veth0")
	sh("ip netns del " + workerNS)
}

// ── helpers ──────────────────────────────────────────────────────────────────
func us(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1000.0 }

func ipBE(s string) uint32 {
	ip := net.ParseIP(s).To4()
	return binary.LittleEndian.Uint32(ip) // in-memory net-order bytes == kernel user_ip4
}
func beUint32(host uint32) uint32 {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], host)
	return binary.LittleEndian.Uint32(b[:])
}

func run(bin, stdin string) {
	cmd := exec.Command(bin)
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Fatalf("%s: %v\n%s", bin, err, out)
	}
}
func mustRun(args ...string) {
	exec.Command(args[0], args[1:]...).Run() // best-effort (mount may already exist)
}
func sh(cmdline string) {
	parts := strings.Fields(cmdline)
	exec.Command(parts[0], parts[1:]...).Run() // best-effort; failures surface via sanity ping
}
