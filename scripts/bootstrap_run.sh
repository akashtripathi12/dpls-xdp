#!/usr/bin/env bash
# bootstrap_run.sh — detached, self-healing full-suite runner for a 2-vCPU AWS node.
# Launched via nohup so an SSH drop can't SIGHUP-kill it mid-run (that is what
# bricked nodes a & b: mec_bench left 60k nat rules in OUTPUT->KUBE-SERVICES,
# starving the box's own SSH egress). Here a trap + timeout guarantee nat is
# always flushed, so the box can never cut itself off permanently.
set -uo pipefail
cd "$(dirname "$0")/.."                 # repo root (script lives in scripts/)

cleanup() {
  echo "=== TRAP: restoring clean network state ==="
  sudo iptables -t nat -F  2>/dev/null; sudo iptables -t nat -X 2>/dev/null
  sudo iptables -F          2>/dev/null; sudo iptables -X 2>/dev/null
  sudo ip netns del mec-worker 2>/dev/null; sudo ip link del veth0 2>/dev/null
  echo "nat lines now: $(sudo iptables -t nat -S 2>/dev/null | wc -l)"
}
trap cleanup EXIT INT TERM

echo "########## BOOTSTRAP START $(date -u) ##########"

echo "===== 0. PRE-CLEAN (kill stray :9000 listeners + stale net state) ====="
sudo pkill -f ':9000' 2>/dev/null; sudo pkill -f worker_listener 2>/dev/null
sudo pkill -f 'mec --' 2>/dev/null; sudo pkill -f crossover 2>/dev/null
cleanup
sleep 1

echo "===== 1. INSTALL TOOLCHAIN ====="
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  golang-go clang llvm libbpf-dev libelf-dev make \
  python3-matplotlib python3-numpy poppler-utils 2>&1 | tail -2
echo "go: $(go version 2>&1) | clang: $(clang --version 2>&1 | head -1)"

echo "===== 2. UPDATE REPO TO LATEST ====="
git remote add upstream https://github.com/kavyarathod05/dpls-xdp.git 2>/dev/null || true
git fetch -q upstream
git checkout -B main upstream/main 2>&1 | tail -1
git log --oneline -1

echo "===== 3. RUN FULL SUITE (timeout-guarded) ====="
# 25-min ceiling: if any bench hangs at high N, timeout fires, trap flushes nat.
sudo timeout 1500 bash scripts/run_all.sh
RC=$?
echo "===== run_all.sh exit code: $RC ====="

echo "########## BOOTSTRAP DONE $(date -u) ##########"
