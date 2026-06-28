#!/usr/bin/env bash
# run_xnode.sh — detached, self-cleaning runner for the cross-VM bench (§6-D).
# Usage: bash run_xnode.sh <worker_private_ip>
set -uo pipefail
cd "$(dirname "$0")/.."                 # repo root (script lives in scripts/)
mkdir -p results
WORKER="${1:?need worker IP}"

cleanup() {
  echo "=== TRAP: clean nat + route ==="
  sudo iptables -t nat -F 2>/dev/null; sudo iptables -t nat -X 2>/dev/null
  sudo ip route del 10.96.0.0/16 via "$WORKER" 2>/dev/null
  echo "nat lines now: $(sudo iptables -t nat -S 2>/dev/null | wc -l)"
}
trap cleanup EXIT INT TERM

echo "########## XNODE START $(date -u) worker=$WORKER ##########"
go build -o /tmp/mec_xnode ./cmd/mec_xnode
echo "build OK"
sudo timeout 1200 /tmp/mec_xnode --worker "$WORKER" --pings 3000 --csv results/mec_xnode_results.csv
echo "===== mec_xnode exit: $? ====="
python3 analysis/plot_xnode.py || true
echo "########## XNODE DONE $(date -u) ##########"
