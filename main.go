package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"time"

	"dpls-xdp/internal/ebpf"
	"dpls-xdp/internal/graph"
	"dpls-xdp/internal/scheduler"
	"dpls-xdp/internal/state"
	"dpls-xdp/pkg/api"
)

func main() {
	mode := flag.String("mode", "mock", "Execution mode: mock or ebpf")
	iface := flag.String("interface", "lo", "Network interface for eBPF attachment")
	elfPath := flag.String("ebpf-elf", "internal/ebpf/c/tc_bridge.o", "Path to compiled eBPF ELF file")
	flag.Parse()

	// Seed the random number generator
	rand.Seed(time.Now().UnixNano())

	log.Printf("[DPLS Main] Starting in %s mode", *mode)

	if *mode == "ebpf" {
		// eBPF Setup
		log.Printf("[DPLS Main] Loading eBPF objects from %s", *elfPath)
		if err := ebpf.LoadBPFObjects(*elfPath); err != nil {
			log.Fatalf("Failed to load eBPF objects: %v", err)
		}

		// log.Printf("[DPLS Main] Attaching TC to interface %s", *iface)
		// if err := ebpf.AttachTC(*iface); err != nil {
		// 	log.Fatalf("Failed to attach TC: %v", err)
		// }
		
		log.Printf("[DPLS Main] Attaching CGROUP hooks to /sys/fs/cgroup")
		if err := ebpf.AttachCgroup(); err != nil {
			log.Printf("Warning: failed to attach cgroup hook: %v", err)
		}

		defer func() {
			log.Printf("[DPLS Main] Detaching TC from %s", *iface)
			ebpf.DetachTC()
			ebpf.DetachCgroup()
		}()
	}

	// Initialize Scheduler components
	graphEng := graph.NewEngine()
	stateMgr := state.NewManager()

	workers := []*api.Worker{
		{ID: "worker-1", IP: "172.31.3.35", ComputeMultiplier: 1.5, NetworkBandwidth: 100},
		{ID: "worker-2", IP: "172.31.3.35", ComputeMultiplier: 1.0, NetworkBandwidth: 50},
	}

	for _, w := range workers {
		_ = stateMgr.UpdateWorkerState(w.ID, "IDLE")
	}

	sched := scheduler.NewScheduler(graphEng, stateMgr, workers, 0.5)

	// 5-minute timeout to allow 1000 iterations safely over real network
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	log.Printf("[DPLS Main] Starting Scheduler...")
	go sched.Run(ctx)

	start := time.Now()
	
	iterations := 50000
	for i := 0; i < iterations; i++ {
		// Create a unique DAG for each iteration so state isn't confused
		dagID := fmt.Sprintf("testdag-%d", i)
		task0ID := fmt.Sprintf("task0-%d", i)
		task1ID := fmt.Sprintf("task1-%d", i)
		
		// Randomize payload size between 64 and 1400 bytes
		randomDataSize := int64(rand.Intn(1400-64+1) + 64)
		
		dag := &api.DAG{
			ID: dagID,
			Tasks: map[string]*api.TaskNode{
				task0ID: {
					ID:              task0ID,
					BaseComputation: 0, // Removed for pure network CPU benchmarking
					Predecessors:    []string{},
					Successors: []api.Dependency{
						{TargetTaskID: task1ID, DataSize: randomDataSize}, // Randomized realistic payload size
					},
				},
				task1ID: {
					ID:              task1ID,
					BaseComputation: 0, // Removed for pure network CPU benchmarking
					Predecessors:    []string{task0ID},
					Successors:      []api.Dependency{},
				},
			},
		}
		
		sched.SubmitDAG(dag)

		// Wait for this DAG's completion
		for {
			state, exists := stateMgr.GetTaskState(task1ID)
			if exists && state == api.StateCompleted {
				break
			}
			if ctx.Err() != nil {
				log.Printf("Timeout waiting for DAG %s to complete | Mean Network RTT so far: %v", dagID, sched.RTTMean())
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	
	elapsed := time.Since(start)
	log.Printf("==========================================================================")
	log.Printf("=== COMPLETED %d ITERATIONS | Total Duration: %v", iterations, elapsed)
	log.Printf("=== STATISTICAL MEAN RTT: %v", sched.RTTMean())
	log.Printf("==========================================================================")
}
