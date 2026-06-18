//go:build !linux
// +build !linux

// Package ebpf provides a MOCK implementation of the eBPF control plane for
// non-Linux systems (Windows, macOS). This is used for:
//   - Local development and compilation on developer machines
//   - Cross-platform unit testing (go test ./...)
//   - CI/CD pipelines without a Linux kernel
//
// On Linux, loader_linux.go compiles instead, providing the real bpf() syscall
// integration via the cilium/ebpf library.
package ebpf

import (
	"log"
	"sync"

	"dpls-xdp/pkg/api"
)

var (
	mockMap = make(map[uint32]api.DependencyRule)
	mockMu  sync.RWMutex
)

// WriteDependencyRuleToKernel mock implementation for non-Linux OS
func WriteDependencyRuleToKernel(rule api.DependencyRule) error {
	mockMu.Lock()
	defer mockMu.Unlock()
	mockMap[rule.SubtaskID] = rule
	log.Printf("[eBPF Mock Bridge] Map updated: SubtaskID=%d -> RefCount=%d, Destinations=%v\n",
		rule.SubtaskID, rule.RefCount, rule.Destinations)
	return nil
}

// AttachTC mock implementation
func AttachTC(interfaceName string) error {
	log.Printf("[eBPF Mock Bridge] Mock Attached TC program to: %s\n", interfaceName)
	return nil
}

// DetachTC mock implementation
func DetachTC() error {
	log.Println("[eBPF Mock Bridge] Mock Detached TC program")
	return nil
}

// AttachCgroup mock implementation (real one lives in cgroup_linux.go)
func AttachCgroup() error {
	log.Println("[eBPF Mock Bridge] Mock Attached cgroup/connect4 hook")
	return nil
}

// DetachCgroup mock implementation
func DetachCgroup() error {
	log.Println("[eBPF Mock Bridge] Mock Detached cgroup/connect4 hook")
	return nil
}

// LoadBPFObjects mock implementation
func LoadBPFObjects(elfPath string) error {
	log.Printf("[eBPF Mock Bridge] Mock Loaded BPF objects from: %s\n", elfPath)
	return nil
}

// GetMockRule helper for testing in non-Linux environments
func GetMockRule(subtaskID uint32) (api.DependencyRule, bool) {
	mockMu.RLock()
	defer mockMu.RUnlock()
	rule, exists := mockMap[subtaskID]
	return rule, exists
}
