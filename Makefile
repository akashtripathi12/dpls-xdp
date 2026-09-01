# eDAG-MEC — build & reproduce.  Run benches as root (eBPF attach needs CAP_SYS_ADMIN).
ARCH    := $(shell uname -m)
BPF_SRC := $(wildcard internal/ebpf/c/*.c)
BPF_OBJ := $(BPF_SRC:.c=.o)
BENCHES := crossover_bench mec_bench mec_xnode cache_bench c3_bench path2_bench worker_listener

.PHONY: all ebpf build bench xnode analysis clean help

help:
	@echo "make ebpf      compile eBPF objects (*.o)"
	@echo "make build     go build all cmd/ benches into ./bin"
	@echo "make bench     sudo: compile, run full single-host suite -> results/  (scripts/run_all.sh)"
	@echo "make xnode IP=<worker_ip>   cross-VM run -> results/  (scripts/run_xnode.sh)"
	@echo "make analysis  regenerate plots + energy/EDP from existing results/ CSVs"
	@echo "make clean     remove *.o and ./bin"

all: ebpf build

ebpf: $(BPF_OBJ)
internal/ebpf/c/%.o: internal/ebpf/c/%.c
	clang -target bpf -O2 -g -I /usr/include/$(ARCH)-linux-gnu -c $< -o $@
	@echo "  built $@"

build: ebpf
	@mkdir -p bin
	@for b in $(BENCHES); do go build -o bin/$$b ./cmd/$$b && echo "  built bin/$$b"; done

bench:
	sudo bash scripts/run_all.sh

xnode:
	bash scripts/run_xnode.sh $(IP)

# Regenerate every figure + the energy/EDP analyses from the committed results/ CSVs.
analysis:
	python3 analysis/plot_crossover.py
	python3 analysis/plot_mec.py
	python3 analysis/plot_cache.py
	python3 analysis/plot_path2.py
	python3 analysis/plot_xnode.py
	python3 analysis/analyze_energy_edp.py
	python3 analysis/energy_vs_basepaper.py
	python3 analysis/deadline_satisfaction.py

clean:
	rm -f $(BPF_OBJ)
	rm -rf bin
