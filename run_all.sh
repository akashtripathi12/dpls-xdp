#!/usr/bin/env bash
# run_all.sh — turnkey: compile eBPF, build+run all benches, regenerate all plots.
# HANDOFF §6-E ("make bench"). Run as ROOT on Ubuntu 24.04 (kernel >=6.6, cgroup v2).
#
#   sudo bash run_all.sh            # full suite
#   sudo bash run_all.sh --setup    # also apt-install the toolchain first
#
set -euo pipefail
cd "$(dirname "$0")"
HR() { printf '\n========== %s ==========\n' "$1"; }

if [[ "${1:-}" == "--setup" ]]; then
  HR "INSTALL TOOLCHAIN"
  apt-get update
  apt-get install -y clang llvm libbpf-dev libelf-dev iproute2 git golang-go python3-pip poppler-utils
  pip3 install --break-system-packages matplotlib numpy
fi

[[ $EUID -eq 0 ]] || { echo "ERROR: must run as root (eBPF attach needs CAP_SYS_ADMIN)"; exit 1; }

HR "ENV CHECK"
uname -r
mount | grep -q 'cgroup2' || echo "WARN: no cgroup2 mount visible (connect4 may need /tmp/cg2)"
go version; clang --version | head -1

HR "COMPILE eBPF OBJECTS"
ARCH=$(uname -m)
for c in connect4 xdp_lookup tc_bridge; do
  clang -target bpf -O2 -g -I "/usr/include/${ARCH}-linux-gnu" \
    -c "internal/ebpf/c/$c.c" -o "internal/ebpf/c/$c.o"
  echo "  built internal/ebpf/c/$c.o"
done

HR "1/4 CROSSOVER  (iptables O(N) vs XDP O(1))"
go build -o /tmp/crossover ./cmd/crossover_bench
/tmp/crossover --pings 3000 --csv crossover_results.csv
python3 plot_crossover.py

HR "2/4 MEC MULTI-NODE  (connect4 O(1) vs kube-proxy O(N))"
mount -t cgroup2 none /tmp/cg2 2>/dev/null || true
mkdir -p /tmp/cg2/dpls
go build -o /tmp/mec ./cmd/mec_bench
/tmp/mec --pings 3000 --csv mec_results.csv
python3 plot_mec.py

HR "3/4 CACHE vs CachOf  (eBPF kernel cache vs app cache)"
go build -o /tmp/cache ./cmd/cache_bench
/tmp/cache --reps 2000
python3 plot_cache.py

HR "4/4 ENERGY / EDP  (HANDOFF §6-A — derived from measured CSVs)"
python3 analyze_energy_edp.py

HR "DONE — artifacts"
ls -la *_plot.png *_results.csv edp_*.csv crossover_results.csv 2>/dev/null || true
echo "Pull these PNGs back with scp to view."
