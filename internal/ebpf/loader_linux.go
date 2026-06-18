//go:build linux
// +build linux

// Package ebpf provides the real Linux control plane bridge between the DPLS
// scheduler and the TC-BPF kernel program. This file is compiled ONLY on Linux.
//
// On non-Linux systems, loader.go (the mock) is compiled instead, which uses
// an in-memory Go map to simulate the kernel map — enabling cross-platform
// development and testing without root access.
//
// DEPENDENCY INSTALLATION (run on Ubuntu 22.04 before building):
//   go get github.com/cilium/ebpf@v0.13.2
//   go get golang.org/x/sys
//   go mod tidy
//
// USAGE:
//   Must be run as root (or with CAP_BPF + CAP_NET_ADMIN capabilities):
//   sudo ./dpls-scheduler --mode ebpf --interface eth0
package ebpf

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"dpls-xdp/pkg/api"
)

// ─────────────────────────────────────────────────────────────────────────────
// KERNEL MAP VALUE STRUCTS
// These structs must match EXACTLY the C structs in tc_bridge.c.
// Field order, sizes, and padding are critical — the Go struct is serialised
// into a byte slice and written into the kernel map via the bpf() syscall.
// ─────────────────────────────────────────────────────────────────────────────

// maxFanout must match MAX_FANOUT in tc_bridge.c
const maxFanout = 4

// workerTriggerPort must match WORKER_TRIGGER_PORT in tc_bridge.c
const workerTriggerPort = 9000

// retainBytes must match RETAIN_BYTES in tc_bridge.c — the number of real
// subtask-output bytes held in the kernel retention_map for fan-out serving.
const retainBytes = 64

// kernelDependencyRule mirrors `struct dependency_rule` in tc_bridge.c.
// Total size: 4 + 4*4 = 20 bytes (no padding due to all uint32 alignment).
type kernelDependencyRule struct {
	RefCount uint32         // Number of downstream consumers (fan-out degree)
	DestIPs  [maxFanout]uint32 // Successor node IPs in NETWORK byte order (big-endian)
}

// ─────────────────────────────────────────────────────────────────────────────
// LOADER STATE
// ─────────────────────────────────────────────────────────────────────────────

// loader holds handles to the loaded BPF objects.
// These are package-level so WriteDependencyRuleToKernel() can access them
// without re-opening the maps on every call.
var (
	vaultMap     *ebpf.Map       // Handle to vault_map in the kernel
	retentionMap *ebpf.Map       // Handle to retention_map in the kernel
	tcIngressProg *ebpf.Program  // Handle to the dpls_tc_ingress BPF program
	tcEgressProg  *ebpf.Program  // Handle to the dpls_tc_egress BPF program
	sendmsg4Prog  *ebpf.Program  // Handle to the dpls_cgroup_connect4 BPF program
	attachedIface string         // Name of the interface TC programs are attached to

	// TCX link handles (modern kernel >= 6.6 attach path; replaces clsact qdisc).
	tcxIngressLink link.Link
	tcxEgressLink  link.Link
)

// ─────────────────────────────────────────────────────────────────────────────
// LoadBPFObjects loads the compiled tc_bridge.o ELF file into the kernel.
//
// This function:
//   1. Removes the RLIMIT_MEMLOCK constraint (required for BPF map creation)
//   2. Opens the compiled ELF file
//   3. Creates the vault_map and retention_map in kernel memory
//   4. Loads the TC-BPF program bytecode into the kernel verifier
//   5. Stores handles for later use by WriteDependencyRuleToKernel()
//
// elfPath: absolute or relative path to the compiled tc_bridge.o file
// ─────────────────────────────────────────────────────────────────────────────
func LoadBPFObjects(elfPath string) error {
	// Step 1: Remove memory lock limit.
	// By default, non-root processes have a tiny RLIMIT_MEMLOCK that prevents
	// creating BPF maps. This call raises it to unlimited (requires root or CAP_BPF).
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("[eBPF Loader] failed to remove memlock rlimit: %w", err)
	}

	// Step 2: Resolve the ELF file path.
	absPath, err := filepath.Abs(elfPath)
	if err != nil {
		return fmt.Errorf("[eBPF Loader] invalid ELF path %q: %w", elfPath, err)
	}
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		return fmt.Errorf("[eBPF Loader] tc_bridge.o not found at %q — compile it first:\n"+
			"  clang -target bpf -O2 -I /usr/include/x86_64-linux-gnu "+
			"-c internal/ebpf/c/tc_bridge.c -o internal/ebpf/c/tc_bridge.o", absPath)
	}

	// Step 3: Load the compiled ELF file.
	// cilium/ebpf parses the ELF sections, extracts map definitions, and creates
	// kernel maps. It also loads the BPF bytecode through the kernel verifier.
	spec, err := ebpf.LoadCollectionSpec(absPath)
	if err != nil {
		return fmt.Errorf("[eBPF Loader] failed to parse ELF %q: %w", absPath, err)
	}

	// Step 4: Instantiate the collection (creates maps + loads programs).
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("[eBPF Loader] kernel rejected BPF program (verifier error): %w\n"+
			"  Common fixes: reduce stack usage, add bounds checks, remove unbounded loops", err)
	}

	// Step 5: Extract map and program handles for later use.
	var ok bool

	vaultMap, ok = coll.Maps["vault_map"]
	if !ok {
		coll.Close()
		return fmt.Errorf("[eBPF Loader] vault_map not found in ELF — check tc_bridge.c SEC(\".maps\")")
	}

	retentionMap, ok = coll.Maps["retention_map"]
	if !ok {
		coll.Close()
		return fmt.Errorf("[eBPF Loader] retention_map not found in ELF")
	}

	// 2. Extract Programs (Functions)
	tcIngressProg, ok = coll.Programs["dpls_tc_ingress"]
	if !ok || tcIngressProg == nil {
		// Cleanup maps if program extraction fails
		vaultMap.Close()
		retentionMap.Close()
		return fmt.Errorf("[eBPF Loader] dpls_tc_ingress program not found in ELF — check SEC(\"tc\") in tc_bridge.c")
	}

	tcEgressProg, ok = coll.Programs["dpls_tc_egress"]
	if !ok || tcEgressProg == nil {
		log.Printf("[eBPF Loader] Warning: dpls_tc_egress not found — egress filtering disabled")
	}

	sendmsg4Prog, ok = coll.Programs["dpls_cgroup_connect4"]
	if !ok || sendmsg4Prog == nil {
		log.Printf("[eBPF Loader] Warning: dpls_cgroup_connect4 not found — sender bypass disabled")
	}

	log.Printf("[eBPF Loader] Successfully loaded BPF maps and programs into kernel memory")
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// AttachTC attaches the loaded TC-BPF programs to a network interface.
//
// This uses the standard Linux `tc` command (iproute2) to:
//   1. Create the clsact qdisc on the target interface
//   2. Attach our dpls_tc_ingress program to the ingress hook
//   3. Attach our dpls_tc_egress program to the egress hook (if available)
//
// interfaceName: the network interface to attach to (e.g., "eth0", "lo")
//
// The clsact qdisc is idempotent — if it already exists, the `add` is a no-op.
// ─────────────────────────────────────────────────────────────────────────────
func AttachTC(interfaceName string) error {
	if tcIngressProg == nil {
		return fmt.Errorf("[eBPF Loader] BPF program not loaded — call LoadBPFObjects() first")
	}

	// Modern TCX attach (kernel >= 6.6). Replaces the legacy clsact qdisc + tc
	// filter path, which FAILS on this kernel ("unknown qdisc"). TCX is the
	// link-based, ownership-tracked successor and needs no qdisc.
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return fmt.Errorf("[eBPF Loader] interface %q not found: %w", interfaceName, err)
	}

	// Ingress
	il, err := link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Program:   tcIngressProg,
		Attach:    ebpf.AttachTCXIngress,
	})
	if err != nil {
		return fmt.Errorf("[eBPF Loader] TCX ingress attach failed (kernel >= 6.6?): %w", err)
	}
	tcxIngressLink = il

	// Egress (optional)
	if tcEgressProg != nil {
		el, err := link.AttachTCX(link.TCXOptions{
			Interface: iface.Index,
			Program:   tcEgressProg,
			Attach:    ebpf.AttachTCXEgress,
		})
		if err != nil {
			log.Printf("[eBPF Loader] Warning: TCX egress attach failed: %v", err)
		} else {
			tcxEgressLink = el
		}
	}

	attachedIface = interfaceName
	log.Printf("[eBPF Loader] Attached TC-BPF via TCX to interface: %s", interfaceName)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DetachTC removes the TC BPF filters and clsact qdisc from the interface.
// Called during graceful shutdown to clean up kernel resources.
// ─────────────────────────────────────────────────────────────────────────────
func DetachTC() error {
	if attachedIface == "" {
		return nil
	}

	// Detach TCX links (modern attach path) — closing the link removes the hook.
	if tcxIngressLink != nil {
		_ = tcxIngressLink.Close()
		tcxIngressLink = nil
	}
	if tcxEgressLink != nil {
		_ = tcxEgressLink.Close()
		tcxEgressLink = nil
	}

	// Close kernel object handles
	if vaultMap != nil {
		vaultMap.Close()
	}
	if retentionMap != nil {
		retentionMap.Close()
	}
	if tcIngressProg != nil {
		tcIngressProg.Close()
	}
	if tcEgressProg != nil {
		tcEgressProg.Close()
	}

	log.Printf("[eBPF Loader] Successfully detached and closed BPF maps")
	attachedIface = ""
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// WriteDependencyRuleToKernel is the CORE FUNCTION of the Channeler.
//
// This is called by the DPLS scheduler (in scheduler/core.go DispatchTask())
// BEFORE a task begins executing — the "Golden Rule" of this architecture.
//
// It converts the Go api.DependencyRule into the binary layout expected by the
// C struct dependency_rule in tc_bridge.c, then writes it into vault_map via
// the bpf() syscall (wrapped by cilium/ebpf's Map.Update()).
//
// When the worker goroutine later sends a UDP trigger packet, the TC-BPF
// program intercepts it and looks up this rule to know where to route the data.
// ─────────────────────────────────────────────────────────────────────────────
func WriteDependencyRuleToKernel(rule api.DependencyRule) error {
	if vaultMap == nil {
		return fmt.Errorf("[eBPF Real Bridge] vault_map not initialised — call LoadBPFObjects first")
	}

	// Convert Go DependencyRule to kernel-compatible binary struct
	kernelRule := kernelDependencyRule{
		RefCount: rule.RefCount,
	}

	// Pack destination IP strings into network-byte-order uint32 values.
	// The C program reads these with bpf_ntohl() for display, and uses them
	// directly for bpf_skb_store_bytes() (which expects network byte order).
	for i, ipStr := range rule.Destinations {
		if i >= maxFanout {
			log.Printf("[eBPF Real Bridge] Warning: more than %d destinations for task %d — truncating",
				maxFanout, rule.SubtaskID)
			break
		}
		if ipStr == "" {
			continue
		}
		parsed := net.ParseIP(ipStr)
		if parsed == nil {
			return fmt.Errorf("[eBPF Real Bridge] invalid IP %q for task %d", ipStr, rule.SubtaskID)
		}
		ipv4 := parsed.To4()
		if ipv4 == nil {
			return fmt.Errorf("[eBPF Real Bridge] %q is not an IPv4 address", ipStr)
		}
		// BPF maps on x86 store values in HOST byte order (little-endian).
		// The C struct field `__u32 dest_ips[MAX_FANOUT]` is host-order.
		// bpf_skb_store_bytes() writes it directly into the packet's daddr field,
		// which the kernel also holds in network byte order (big-endian).
		// HOWEVER: bpf_l3_csum_replace() and bpf_skb_store_bytes() work with the
		// raw packet bytes, so new_daddr must be in NETWORK byte order (big-endian).
		// The C code does: __u32 new_daddr = rule->dest_ips[0]
		//   then:          bpf_skb_store_bytes(skb, IP_DADDR_OFF, &new_daddr, ...)
		// This means dest_ips must contain the IP in NETWORK byte order.
		// cilium/ebpf marshals Go uint32 fields using the native (little-endian) layout,
		// so we must store the big-endian bytes AS a little-endian uint32, which means
		// reading the 4-byte big-endian slice as little-endian:
		kernelRule.DestIPs[i] = binary.LittleEndian.Uint32(ipv4)
	}

	// Write into vault_map: key=SubtaskID, value=kernelDependencyRule.
	// cilium/ebpf v0.13 Map.Update accepts typed Go values directly —
	// it uses its internal marshal layer to serialise them into the kernel map.
	// Do NOT use unsafe.Pointer here; the library handles the byte layout.
	if err := vaultMap.Update(rule.SubtaskID, &kernelRule, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("[eBPF Real Bridge] map update failed for task %d: %w", rule.SubtaskID, err)
	}

	log.Printf("[eBPF Real Bridge] Successfully wrote DependencyRule to Kernel vault_map: %+v", rule)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GetRetainedPayloadCount returns the number of remaining consumers for a task.
// Used by the scheduler to confirm fan-out retention state.
// ─────────────────────────────────────────────────────────────────────────────
func GetRetainedPayloadCount(taskID uint32) (uint32, error) {
	if retentionMap == nil {
		return 0, fmt.Errorf("retention_map not initialised")
	}

	// retained_payload mirrors `struct retained_payload` in tc_bridge.c EXACTLY:
	//   task_id(4) + remaining_consumers(4) + payload_len(4) + payload[64] + lock(4)
	// Field order and sizes are critical — cilium/ebpf marshals by struct layout.
	var payload struct {
		TaskID             uint32
		RemainingConsumers uint32
		PayloadLen         uint32
		Payload            [retainBytes]byte
		Lock               uint32 // bpf_spin_lock padding
	}
	if err := retentionMap.Lookup(taskID, &payload); err != nil {
		return 0, nil // Not retained = fully consumed (GC'd) or not a fan-out task
	}
	return payload.RemainingConsumers, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GetMockRule is a stub for Linux builds — on Linux the real vault_map is used.
// This exists solely to satisfy the test file (loader_test.go) which calls it.
// On Linux, tests should use the real map via WriteDependencyRuleToKernel.
// ─────────────────────────────────────────────────────────────────────────────
func GetMockRule(subtaskID uint32) (api.DependencyRule, bool) {
	// On Linux: read from actual vault_map if it's loaded
	if vaultMap != nil {
		var kernelRule kernelDependencyRule
		if err := vaultMap.Lookup(subtaskID, &kernelRule); err == nil {
			dests := make([]string, 0, maxFanout)
			for _, ip := range kernelRule.DestIPs {
				if ip == 0 {
					continue
				}
				b := make([]byte, 4)
				binary.BigEndian.PutUint32(b, ip)
				dests = append(dests, net.IP(b).String())
			}
			return api.DependencyRule{
				SubtaskID:    subtaskID,
				RefCount:     kernelRule.RefCount,
				Destinations: dests,
			}, true
		}
	}
	return api.DependencyRule{}, false
}

// ─────────────────────────────────────────────────────────────────────────────
// runTC executes a `tc` (iproute2) command with the given arguments.
// This is the simplest way to manipulate TC qdiscs and filters without
// adding a heavy netlink dependency to the project.
// ─────────────────────────────────────────────────────────────────────────────
func runTC(args ...string) error {
	cmd := exec.Command("tc", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Some errors (e.g., "already exists") are acceptable — log but don't fail
		return fmt.Errorf("tc %v: %w", args, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Compile-time safety check: this file is only compiled on Linux (build tag).
// Panicking here catches any accidental cross-compilation without the tag.
// ─────────────────────────────────────────────────────────────────────────────
func init() {
	if runtime.GOOS != "linux" {
		panic("loader_linux.go: must only compile on Linux — check //go:build linux tag")
	}
}
