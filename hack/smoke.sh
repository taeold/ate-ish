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
FAKE2_PID=""
cleanup() {
  [[ -n "$NODE_PID" ]] && kill "$NODE_PID" 2>/dev/null || true
  [[ -n "$FAKE_PID" ]] && kill "$FAKE_PID" 2>/dev/null || true
  [[ -n "$FAKE2_PID" ]] && kill "$FAKE2_PID" 2>/dev/null || true
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

# A second stub stands in for a plain host-side test server behind a
# non-443 https rule (G4): same binary, a made-up hostname, its own key.
INTERNAL_KEY="internal-$(date +%s)"
bin/fakeupstream -listen 127.0.0.1:0 -hostname internal.test -api-key "$INTERNAL_KEY" -ready-file "$work/internal.ready" >"$work/internal.log" 2>&1 &
FAKE2_PID=$!
for _ in $(seq 1 50); do
  [[ -s "$work/internal.ready" ]] && break
  sleep 0.1
done
[[ -s "$work/internal.ready" ]] || { echo "FAILED: internal.test stub never became ready: $(cat "$work/internal.log")"; exit 1; }
read -r INTERNAL_ADDR INTERNAL_CA < "$work/internal.ready"
cat "$FAKE_CA" "$INTERNAL_CA" > "$work/combined-ca.pem"
echo "  ai-gateway.vercel.sh stub $FAKE_ADDR, internal.test stub $INTERNAL_ADDR [$(ms "$t0") ms]"

echo "== node =="
t0=$(date +%s%N)
export ATE_DIR="$work/ate-node"
export ATE_STORE="dir:$work/ate-store"
export ATE_EGRESS_RESOLVE="ai-gateway.vercel.sh=$FAKE_ADDR,internal.test=$INTERNAL_ADDR"
export ATE_EGRESS_TEST_CA="$work/combined-ca.pem"
export AI_GATEWAY_API_KEY="$FAKE_KEY"
export INTERNAL_TEST_KEY="$INTERNAL_KEY"
export ATE_CAPACITY=8 # counter + gate.sh's 3 pi-durable actors + this script's 2 more
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

echo "== G4 rule kinds =="
t0=$(date +%s%N)
# Reuses the pi-durable-data template and ate-demo-pi atespace
# hack/gate.sh --fake already registered just above.
bin/ate-ish create actor g4-rules --template pi-durable-data -a ate-demo-pi
bin/ate-ish create egress-policy g4-rules -a ate-demo-pi -f demo/pi-durable/egress-policy-g4.yaml
bin/ate-ish create actor g4-star --template pi-durable-data -a ate-demo-pi
bin/ate-ish create egress-policy g4-star -a ate-demo-pi -f demo/pi-durable/egress-policy-g4-star.yaml

run() { # actor prompt
  node -e '
    const [url, actor, prompt] = process.argv.slice(1);
    fetch(url + "/submit", {method: "POST", headers: {"Content-Type": "application/json", "ate-target-actor": actor}, body: JSON.stringify({prompt})})
      .then(async r => process.stdout.write(await r.text()))
      .catch(e => process.stdout.write(JSON.stringify({error: e.message})));
  ' "$ATE_URL" "ate-demo-pi/$1" "$2"
}
lastResult() { jq -r '[.view.messages[] | select(.role == "toolResult") | .content[0].text // ""][-1]'; }
cmd() { echo "Run this exact command with the bash tool and reply with its output only: $1"; }

out=$(run g4-rules "$(cmd "node -e \"fetch('http://example.com/').then(r => console.log('status', r.status))\"")")
echo "$out" | grep -q '"status":"done"' || { echo "FAILED: http rule turn did not settle: $out"; exit 1; }
got=$(echo "$out" | lastResult)
echo "$got" | grep -q '^status 200' || { echo "FAILED: http rule to example.com: $got"; exit 1; }
echo "  ok: http rule reaches http://example.com/: $got"

out=$(run g4-rules "$(cmd "node -e \"fetch('https://internal.test:8443/v1/chat/completions', {method: 'POST', headers: {'x-api-key': 'placeholder', 'content-type': 'application/json'}, body: '{}'}).then(r => console.log('status', r.status), e => console.log('failed:', e.cause?.message || e.message))\"")")
got=$(echo "$out" | lastResult)
echo "$got" | grep -q '^status 200' || { echo "FAILED: https non-443 rule to internal.test:8443: $got"; exit 1; }
echo "  ok: https rule on a non-443 port reaches internal.test:8443, key replaced: $got"

out=$(run g4-rules "$(cmd "node -e \"fetch('https://ai-gateway.vercel.sh/v1/chat/completions', {method: 'POST', headers: {'x-api-key': 'ate-placeholder-key', 'content-type': 'application/json'}, body: JSON.stringify({model: 'x', messages: []})}).then(r => console.log('status', r.status))\"")")
got=$(echo "$out" | lastResult)
echo "$got" | grep -q '^status 200' || { echo "FAILED: *.vercel.sh wildcard with key replaced: $got"; exit 1; }
echo "  ok: *.vercel.sh matches ai-gateway.vercel.sh, placeholder key replaced: $got"

out=$(run g4-star "$(cmd "node -e \"fetch('https://example.com').then(r => console.log('status', r.status))\"")")
got=$(echo "$out" | lastResult)
echo "$got" | grep -q '^status 200' || { echo "FAILED: bare * to example.com: $got"; exit 1; }
out=$(run g4-star "$(cmd "node -e \"fetch('https://example.org').then(r => console.log('status', r.status))\"")")
got=$(echo "$out" | lastResult)
echo "$got" | grep -q '^status 200' || { echo "FAILED: bare * to example.org: $got"; exit 1; }
echo "  ok: bare * reaches example.com and example.org with no key"

out=$(run g4-rules "$(cmd "node -e \"fetch('https://example.org').then(r => console.log('status', r.status), e => console.log('failed:', e.cause?.message || e.message))\"")")
got=$(echo "$out" | lastResult)
echo "$got" | grep -q '^failed' || { echo "FAILED: example.org should be unlisted under g4-rules's policy: $got"; exit 1; }
echo "  ok: under g4-rules's specific policy (no bare *), an unlisted name still fails: $got"
echo "  [$(ms "$t0") ms]"

echo "== total [$(ms "$t_start") ms] =="
