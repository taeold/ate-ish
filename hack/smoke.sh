#!/usr/bin/env bash
# One command, no network, no Secret Manager: build, start a node, run
# the counter demo (two requests, suspend, a third), then run one
# pi-durable turn through hack/gate.sh --fake against hack/fakeupstream.
# Exits nonzero on any failure, and always tears down what it started.
#
#   hack/smoke.sh
set -euo pipefail
cd "$(dirname "$0")/.."

t_start=$(date +%s%N)
ms() { echo $(( ($(date +%s%N) - $1) / 1000000 )); }

work=$(mktemp -d)
NODE_PID=""
FAKE_PID=""
cleanup() {
  [[ -n "$NODE_PID" ]] && kill "$NODE_PID" 2>/dev/null || true
  [[ -n "$FAKE_PID" ]] && kill "$FAKE_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

echo "== build =="
t0=$(date +%s%N)
CGO_ENABLED=0 go build -o bin/ate-ish ./cmd/ate-ish
CGO_ENABLED=0 go build -o bin/fakeupstream ./hack/fakeupstream
echo "  [$(ms "$t0") ms]"

echo "== fakeupstream =="
t0=$(date +%s%N)
FAKE_KEY="smoke-$(date +%s)"
bin/fakeupstream -listen 127.0.0.1:0 -hostname ai-gateway.vercel.sh -api-key "$FAKE_KEY" -ready-file "$work/fakeupstream.ready" >"$work/fakeupstream.log" 2>&1 &
FAKE_PID=$!
for _ in $(seq 1 50); do
  [[ -s "$work/fakeupstream.ready" ]] && break
  sleep 0.1
done
[[ -s "$work/fakeupstream.ready" ]] || { echo "FAILED: fakeupstream never became ready: $(cat "$work/fakeupstream.log")"; exit 1; }
read -r FAKE_ADDR FAKE_CA < "$work/fakeupstream.ready"
echo "  $FAKE_ADDR, ca $FAKE_CA [$(ms "$t0") ms]"

echo "== node =="
t0=$(date +%s%N)
export ATE_DIR="$work/ate-node"
export ATE_STORE="dir:$work/ate-store"
export ATE_EGRESS_RESOLVE="ai-gateway.vercel.sh=$FAKE_ADDR"
export ATE_EGRESS_TEST_CA="$FAKE_CA"
export AI_GATEWAY_API_KEY="$FAKE_KEY"
bin/ate-ish >"$work/node.log" 2>&1 &
NODE_PID=$!
export ATE_URL=http://127.0.0.1:8080
for _ in $(seq 1 200); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$ATE_URL/admin/workers" || true)
  [[ "$code" == "200" ]] && break
  sleep 0.1
done
[[ "$code" == "200" ]] || { echo "FAILED: node never answered /admin/workers: $(tail -c 2000 "$work/node.log")"; exit 1; }
echo "  up [$(ms "$t0") ms]"

echo "== counter demo =="
t0=$(date +%s%N)
bin/ate-ish create atespace ate-demo-counter
bin/ate-ish create actor-template -f demo/counter/template.yaml
bin/ate-ish create actor counter1 --template counter -a ate-demo-counter
memoryCount() { grep -oE 'memory count: [0-9]+' | grep -oE '[0-9]+'; }
for want in 1 2; do
  got=$(curl -s -H "ate-target-actor: ate-demo-counter/counter1" "$ATE_URL/" | memoryCount)
  [[ "$got" == "$want" ]] || { echo "FAILED: counter request $want returned '$got'"; exit 1; }
done
bin/ate-ish suspend actor counter1 -a ate-demo-counter
got=$(curl -s -H "ate-target-actor: ate-demo-counter/counter1" "$ATE_URL/" | memoryCount)
[[ "$got" == "3" ]] || { echo "FAILED: counter request after suspend returned '$got', want 3"; exit 1; }
echo "  ok: 1, 2, suspend, 3 [$(ms "$t0") ms]"

echo "== pi-durable turn against fakeupstream (hack/gate.sh --fake) =="
t0=$(date +%s%N)
hack/gate.sh --fake
echo "  [$(ms "$t0") ms]"

echo "== total [$(ms "$t_start") ms] =="
