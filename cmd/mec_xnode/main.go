//go:build linux
// +build linux

// mec_xnode — REAL cross-VM multi-node MEC benchmark (HANDOFF §6-D).
// ===================================================================
// Unlike cmd/mec_bench (which emulates the worker with a veth + netns on ONE
// host), this dials a *real remote worker* on another EC2 instance across the
// VPC NIC. It lifts the "emulated, not physical, multi-node" caveat (§5.3):
// the round-trip now traverses two separate kernels and the actual network.
//
//   [ this node = SCHEDULER ]  --- VPC (ENA NIC) --->  [ remote node = WORKER ]
//        dials VIP 10.96.0.10                           worker_listener UDP :9000
//
// Two arms route the virtual ClusterIP (10.96.0.10) to the real worker IP:
//   ARM A (kube-proxy):  iptables nat OUTPUT DNAT 10.96.0.10 -> WORKER, sitting
//        behind N dummy KUBE-SERVICES rules => O(N) chain walk + conntrack.
//   ARM B (eDAG-MEC):    cgroup/connect4 rewrites the dest to WORKER before
//        netfilter via one O(1) vault lookup => no DNAT, no conntrack.
//
// Run worker_listener on the remote node first, then:
//   sudo ./mec_xnode --worker 172.31.4.205 --pings 3000 --csv mec_xnode_results.csv
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
	svcIP   = "10.96.0.10" // virtual ClusterIP the scheduler dials
	port    = 9000
	cgPath  = "/tmp/cg2/dpls"
	connElf = "internal/ebpf/c/connect4.o"
)

var workerIP string

func main() {
	w := flag.String("worker", "", "remote worker private IP (required)")
	pings := flag.Int("pings", 3000, "round-trips per measurement")
	csvPath := flag.String("csv", "mec_xnode_results.csv", "output CSV")
	flag.Parse()
	log.SetFlags(log.Ltime)
	if *w == "" {
		log.Fatal("must pass --worker <remote IP>")
	}
	workerIP = *w

	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("memlock: %v", err)
	}
	setup()
	defer teardown()

	// sanity: confirm the remote worker echoes BEFORE measuring
	if _, ok := pingTo(workerIP); !ok {
		log.Fatalf("[sanity] remote worker %s:%d unreachable — start worker_listener there", workerIP, port)
	}
	log.Printf("[sanity] remote worker %s reachable across VPC ✓", workerIP)

	Ns := []int{0, 100, 500, 1000, 2000, 5000, 10000, 20000, 40000, 60000}
	type row struct {
		N                int
		kube, ebpf, gap  float64
		speedup          float64
	}
	rows := make([]row, 0, len(Ns))
	for _, n := range Ns {
		kube := measureKubeProxy(n, *pings)
		ebpfv := measureEBPF(n, *pings)
		gap := kube - ebpfv
		rows = append(rows, row{n, kube, ebpfv, gap, kube / ebpfv})
		log.Printf("N=%-6d  kube-proxy(DNAT+O(N))=%8.2f us   eBPF connect4 O(1)=%8.2f us   gap=%+8.2f us  (%.2fx)",
			n, kube, ebpfv, gap, kube/ebpfv)
	}

	var b strings.Builder
	b.WriteString("N,kubeproxy_us,ebpf_connect4_us,gap_us,speedup\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("%d,%.3f,%.3f,%.3f,%.3f\n", r.N, r.kube, r.ebpf, r.gap, r.speedup))
	}
	if err := os.WriteFile(*csvPath, []byte(b.String()), 0644); err != nil {
		log.Fatalf("write csv: %v", err)
	}
	log.Printf("wrote %s", *csvPath)
}

// ── ARM A: kube-proxy baseline (iptables nat DNAT behind N service rules) ─────
func measureKubeProxy(n, pings int) float64 {
	loadNAT(n)
	defer clearNAT()
	return measure(svcIP, pings)
}

func loadNAT(n int) {
	var b strings.Builder
	b.WriteString("*nat\n:PREROUTING ACCEPT [0:0]\n:INPUT ACCEPT [0:0]\n")
	b.WriteString(":OUTPUT ACCEPT [0:0]\n:POSTROUTING ACCEPT [0:0]\n:KUBE-SERVICES - [0:0]\n")
	b.WriteString("-A OUTPUT -j KUBE-SERVICES\n")
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
	_ = vault.Put(ipBE(svcIP), ipBE(workerIP))
	for i := 0; i < n; i++ {
		k := beUint32(0x0a630000 + uint32(i)) // 10.99.x.x dummies, same footprint as ARM A
		_ = vault.Put(k, ipBE(workerIP))
	}
	l, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgPath,
		Attach:  ebpf.AttachCGroupInet4Connect,
		Program: coll.Programs["connect4"],
	})
	if err != nil {
		log.Fatalf("connect4 attach: %v", err)
	}
	defer l.Close()
	return measure(svcIP, pings)
}

// ── shared cross-node UDP RTT measurement (98th-pct trimmed mean) ─────────────
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
	c.SetDeadline(time.Now().Add(300 * time.Millisecond)) // wider: real network
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, 1)
	t := time.Now()
	if _, err := c.Write(p); err != nil {
		return 0, false
	}
	buf := make([]byte, 32)
	if _, err := c.Read(buf); err != nil {
		return 0, false
	}
	return time.Since(t), true
}

// ── setup/teardown (no veth — worker is a real remote host) ───────────────────
func setup() {
	mustRun("mount", "-t", "cgroup2", "none", "/tmp/cg2") // idempotent-ish
	os.MkdirAll(cgPath, 0755)
	// put THIS process into the dpls cgroup so connect4 applies to its sockets
	// (HANDOFF §3). Best-effort: on this kernel the attach also covers descendants.
	_ = os.WriteFile(cgPath+"/cgroup.procs", []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
	// route the ClusterIP range toward the worker so ARM-A connect()/DNAT works.
	sh("ip route replace 10.96.0.0/16 via " + workerIP)
}

func teardown() {
	clearNAT()
	sh("ip route del 10.96.0.0/16 via " + workerIP)
}

// ── helpers (same as mec_bench) ───────────────────────────────────────────────
func ipBE(s string) uint32 {
	ip := net.ParseIP(s).To4()
	return binary.LittleEndian.Uint32(ip)
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
func mustRun(args ...string) { exec.Command(args[0], args[1:]...).Run() }
func sh(cmdline string) {
	parts := strings.Fields(cmdline)
	exec.Command(parts[0], parts[1:]...).Run()
}
