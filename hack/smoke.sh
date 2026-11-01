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
# Extra scratch nodes a later section starts in their own ATE_DIR
# (the orphan repro, G5's own fallback/multi-ID runs): track_node
# appends to these so one cleanup trap can reap all of them if the
# script exits before that section's own graceful shutdown runs.
EXTRA_PIDS=()
EXTRA_DIRS=()
track_node() { EXTRA_PIDS+=("$1"); EXTRA_DIRS+=("$2"); } # pid dir
# Plain helper processes (an extra fakeupstream, etc.) with no runsc
# state of their own to reap, just a kill.
EXTRA_SIMPLE_PIDS=()
track_pid() { EXTRA_SIMPLE_PIDS+=("$1"); }

# privileged_rm removes dir even when a multi-ID mapping (idmap.go) left
# some of it owned by a subuid/subgid id a plain host rm can't touch: it
# tries the easy way first, and only when that leaves something behind
# does it spin up one throwaway mapped namespace (the same newuidmap/
# newgidmap this script already requires for that path) to finish the
# job as that mapping's own root.
privileged_rm() { # dir
  local dir="$1"
  [[ -e "$dir" ]] || return 0
  rm -rf "$dir" 2>/dev/null && return 0
  [[ -e "$dir" ]] || return 0
  command -v newuidmap >/dev/null 2>&1 && command -v newgidmap >/dev/null 2>&1 || return 0
  local subuid subgid me
  me=$(id -u)
  subuid=$(awk -F: -v u="$(id -un)" -v i="$me" '$1==u || $1==i {print $2" "$3; exit}' /etc/subuid 2>/dev/null)
  subgid=$(awk -F: -v u="$(id -un)" -v i="$me" '$1==u || $1==i {print $2" "$3; exit}' /etc/subgid 2>/dev/null)
  [[ -n "$subuid" && -n "$subgid" ]] || return 0
  unshare --user --net --mount --pid --fork --propagation=private sleep 10 &
  local hpid=$! child=""
  for _ in $(seq 1 20); do
    child=$(awk '{print $1}' "/proc/$hpid/task/$hpid/children" 2>/dev/null)
    [[ -n "$child" ]] && break
    sleep 0.05
  done
  if [[ -n "$child" ]]; then
    newuidmap "$child" 0 "$me" 1 1 $subuid 2>/dev/null || true
    newgidmap "$child" 0 "$(id -g)" 1 1 $subgid 2>/dev/null || true
    nsenter --user="/proc/$child/ns/user" --mount="/proc/$child/ns/mnt" --preserve-credentials -- rm -rf "$dir" 2>/dev/null || true
  fi
  kill -9 "$hpid" 2>/dev/null || true
  wait "$hpid" 2>/dev/null || true
}

cleanup() {
  [[ -n "$NODE_PID" ]] && kill "$NODE_PID" 2>/dev/null || true
  [[ -n "$FAKE_PID" ]] && kill "$FAKE_PID" 2>/dev/null || true
  [[ -n "$FAKE2_PID" ]] && kill "$FAKE2_PID" 2>/dev/null || true
  for pid in "${EXTRA_SIMPLE_PIDS[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  local i
  for i in "${!EXTRA_PIDS[@]}"; do
    pid="${EXTRA_PIDS[$i]}" dir="${EXTRA_DIRS[$i]}"
    [[ -n "$pid" ]] && kill -9 "$pid" 2>/dev/null || true
    # A hard-killed scratch node leaves exactly the orphan the orphan
    # section tests for; a failure partway through this script must not
    # leave it running past the script's own exit.
    runsc_bin="${ATE_RUNSC_DIR:-${ATE_CACHE_DIR:-$HOME/.cache/ate-ish}/runsc}/runsc"
    if [[ -n "$dir" && -x "$runsc_bin" ]]; then
      for id in $("$runsc_bin" "--root=$dir/runsc-state" list -quiet -sandbox 2>/dev/null); do
        "$runsc_bin" "--root=$dir/runsc-state" delete -force "$id" 2>/dev/null || true
      done
      [[ -f "$dir/holder.pid" ]] && kill -9 "$(cat "$dir/holder.pid")" 2>/dev/null || true
    fi
  done
  wait 2>/dev/null || true
  privileged_rm "$work"
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

echo "== orphaned sandboxes =="
t0=$(date +%s%N)

assert_no_runsc() { # state_dir
  if pgrep -af 'runsc-sandbox|runsc-gofer' | grep -qF -- "$1"; then
    echo "FAILED: a runsc process still has root dir $1"
    pgrep -af 'runsc-sandbox|runsc-gofer' | grep -F -- "$1"
    exit 1
  fi
}
assert_runsc_present() { # state_dir, message
  pgrep -af 'runsc-sandbox|runsc-gofer' | grep -qF -- "$1" || { echo "FAILED: $2"; exit 1; }
}

echo "-- SIGINT: every sandbox this node runs stops, node exits with none left --"
kill -INT "$NODE_PID"
wait "$NODE_PID" 2>/dev/null || true
NODE_PID=""
assert_no_runsc "$work/ate-node/runsc-state"
echo "  ok: SIGINT suspended/stopped every sandbox, no runsc process remains"

echo "-- SIGKILL: the next node start with the same state dir reaps what it left behind --"
g2c_dir="$work/ate-g2c"
G2C_URL=http://127.0.0.1:8082
ATE_DIR="$g2c_dir" ATE_STORE="dir:$g2c_dir-store" ATE_LISTEN=127.0.0.1:8082 ATE_CAPACITY=2 bin/ate-ish >"$work/g2c-1.log" 2>&1 &
G2C_PID=$!
track_node "$G2C_PID" "$g2c_dir"
for _ in $(seq 1 100); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$G2C_URL/admin/workers" || true)
  [[ "$code" == "200" ]] && break
  sleep 0.1
done
[[ "$code" == "200" ]] || { echo "FAILED: g2c node never came up: $(tail -c 1000 "$work/g2c-1.log")"; exit 1; }
# demo/counter/template.yaml's own metadata.atespace is
# ate-demo-counter; the actor must be created under that atespace, not
# a new one, or create actor 404s looking for the template there.
ATE_URL="$G2C_URL" bin/ate-ish create atespace ate-demo-counter >/dev/null
ATE_URL="$G2C_URL" bin/ate-ish create actor-template -f demo/counter/template.yaml >/dev/null
ATE_URL="$G2C_URL" bin/ate-ish create actor g2c1 --template counter -a ate-demo-counter >/dev/null
curl -s -H "ate-target-actor: ate-demo-counter/g2c1" "$G2C_URL/" >/dev/null
assert_runsc_present "$g2c_dir/runsc-state" "expected a running sandbox under $g2c_dir/runsc-state before SIGKILL"
kill -KILL "$G2C_PID"
wait "$G2C_PID" 2>/dev/null || true
assert_runsc_present "$g2c_dir/runsc-state" "the sandbox should still be running with no ate-ish process alive right after SIGKILL (the G2 bug, reproduced)"
echo "  ok: SIGKILL leaves the sandbox running with no ate-ish process alive"

ATE_DIR="$g2c_dir" ATE_STORE="dir:$g2c_dir-store" ATE_LISTEN=127.0.0.1:8082 ATE_CAPACITY=2 bin/ate-ish >"$work/g2c-2.log" 2>&1 &
G2C_PID=$!
track_node "$G2C_PID" "$g2c_dir"
for _ in $(seq 1 100); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$G2C_URL/admin/workers" || true)
  [[ "$code" == "200" ]] && break
  sleep 0.1
done
[[ "$code" == "200" ]] || { echo "FAILED: g2c restart never came up: $(tail -c 1000 "$work/g2c-2.log")"; exit 1; }
grep -q "reaped orphaned sandbox" "$work/g2c-2.log" || { echo "FAILED: restart did not log reaping the orphan: $(tail -c 1000 "$work/g2c-2.log")"; exit 1; }
assert_no_runsc "$g2c_dir/runsc-state"
echo "  ok: restart with the same state dir found, killed and deleted the orphan, logged one line: $(grep "reaped orphaned sandbox" "$work/g2c-2.log")"

kill -TERM "$G2C_PID"
wait "$G2C_PID" 2>/dev/null || true
assert_no_runsc "$g2c_dir/runsc-state"
echo "  [$(ms "$t0") ms]"

echo "== G5 non-root images =="
t0=$(date +%s%N)
docker build -q -t ate-ish-g5-nonroot:latest demo/g5-nonroot >/dev/null
docker build -q -t ate-ish-g5-gosu:latest demo/g5-gosu >/dev/null

echo "-- root-only fallback (ATE_SANDBOX_FORCE_ROOT_ONLY_MAPPING): the log line, then the USER node image fails --"
g5_dir="$work/ate-g5a"
G5_URL=http://127.0.0.1:8083
ATE_SANDBOX_FORCE_ROOT_ONLY_MAPPING=1 ATE_DIR="$g5_dir" ATE_STORE="dir:$g5_dir-store" ATE_LISTEN=127.0.0.1:8083 ATE_CAPACITY=2 bin/ate-ish >"$work/g5a.log" 2>&1 &
G5_PID=$!
track_node "$G5_PID" "$g5_dir"
for _ in $(seq 1 100); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$G5_URL/admin/workers" || true)
  [[ "$code" == "200" ]] && break
  sleep 0.1
done
[[ "$code" == "200" ]] || { echo "FAILED: g5 root-only node never came up: $(tail -c 1000 "$work/g5a.log")"; exit 1; }
grep -q "non-root images will fail" "$work/g5a.log" || { echo "FAILED: missing the root-only fallback log line: $(tail -c 1000 "$work/g5a.log")"; exit 1; }
echo "  ok: $(grep "held namespace maps" "$work/g5a.log")"
ATE_URL="$G5_URL" bin/ate-ish create atespace ate-demo-nonroot >/dev/null
ATE_URL="$G5_URL" bin/ate-ish create actor-template -f demo/g5-nonroot/template.yaml >/dev/null
ATE_URL="$G5_URL" bin/ate-ish create actor nonroot1 --template nonroot -a ate-demo-nonroot >/dev/null
got=$(curl -s -m 15 -H "ate-target-actor: ate-demo-nonroot/nonroot1" "$G5_URL/")
echo "$got" | grep -q '"write":"failed' || { echo "FAILED: expected the USER node image's write to fail under root-only mapping: $got"; exit 1; }
echo "  ok: the USER node image boots but its own write fails under root-only mapping: $got"
kill -TERM "$G5_PID"
wait "$G5_PID" 2>/dev/null || true
assert_no_runsc "$g5_dir/runsc-state"

if command -v newuidmap >/dev/null 2>&1 && command -v newgidmap >/dev/null 2>&1; then
  echo "-- multi-ID mapping (newuidmap/newgidmap plus a subuid/subgid range on this host): the same image works --"
  g5_dir2="$work/ate-g5b"
  G5_URL2=http://127.0.0.1:8084
  ATE_DIR="$g5_dir2" ATE_STORE="dir:$g5_dir2-store" ATE_LISTEN=127.0.0.1:8084 ATE_CAPACITY=4 bin/ate-ish >"$work/g5b.log" 2>&1 &
  G5_PID=$!
  track_node "$G5_PID" "$g5_dir2"
  for _ in $(seq 1 100); do
    code=$(curl -s -o /dev/null -w '%{http_code}' "$G5_URL2/admin/workers" || true)
    [[ "$code" == "200" ]] && break
    sleep 0.1
  done
  [[ "$code" == "200" ]] || { echo "FAILED: g5 multi-ID node never came up: $(tail -c 1000 "$work/g5b.log")"; exit 1; }
  grep -q "non-root actor images work" "$work/g5b.log" || { echo "FAILED: missing the multi-ID mapping log line: $(tail -c 1000 "$work/g5b.log")"; exit 1; }
  echo "  ok: $(grep "held namespace maps" "$work/g5b.log")"

  ATE_URL="$G5_URL2" bin/ate-ish create atespace ate-demo-nonroot >/dev/null
  ATE_URL="$G5_URL2" bin/ate-ish create actor-template -f demo/g5-nonroot/template.yaml >/dev/null
  ATE_URL="$G5_URL2" bin/ate-ish create actor nonroot1 --template nonroot -a ate-demo-nonroot >/dev/null
  got=$(curl -s -m 15 -H "ate-target-actor: ate-demo-nonroot/nonroot1" "$G5_URL2/")
  echo "$got" | grep -q '"write":"ok"' || { echo "FAILED: USER node image should work under multi-ID mapping: $got"; exit 1; }
  echo "$got" | grep -q 'uid=1000(node)' || { echo "FAILED: id should report uid=1000(node): $got"; exit 1; }
  echo "  ok: the USER node image writes its own home and reports its own uid: $got"

  ATE_URL="$G5_URL2" bin/ate-ish create actor-template -f demo/g5-gosu/template.yaml >/dev/null
  ATE_URL="$G5_URL2" bin/ate-ish create actor gosu1 --template gosu-nobody -a ate-demo-nonroot >/dev/null
  got=$(curl -s -m 15 -H "ate-target-actor: ate-demo-nonroot/gosu1" "$G5_URL2/")
  echo "$got" | grep -q 'uid=65534(nobody)' || { echo "FAILED: gosu to nobody should report uid=65534: $got"; exit 1; }
  echo "  ok: a root-starting actor's own gosu drops to nobody: $got"

  ATE_URL="$G5_URL2" bin/ate-ish create atespace ate-demo-counter >/dev/null
  ATE_URL="$G5_URL2" bin/ate-ish create actor-template -f demo/counter/template.yaml >/dev/null
  ATE_URL="$G5_URL2" bin/ate-ish create actor counter1 --template counter -a ate-demo-counter >/dev/null
  c1=$(curl -s -m 15 -H "ate-target-actor: ate-demo-counter/counter1" "$G5_URL2/" | memoryCount)
  c2=$(curl -s -m 15 -H "ate-target-actor: ate-demo-counter/counter1" "$G5_URL2/" | memoryCount)
  [[ "$c1" == "1" && "$c2" == "2" ]] || { echo "FAILED: counter under multi-ID mapping returned '$c1' then '$c2'"; exit 1; }
  echo "  ok: the counter (a root-run image) still runs under multi-ID mapping"

  kill -TERM "$G5_PID"
  wait "$G5_PID" 2>/dev/null || true
  assert_no_runsc "$g5_dir2/runsc-state"
  privileged_rm "$g5_dir2"
else
  echo "  (newuidmap/newgidmap not installed on this host: skipping the multi-ID mapping success path; see docs/ROOTLESS.md's G5)"
fi
echo "  [$(ms "$t0") ms]"

echo "== G6 in-flight suspend =="
t0=$(date +%s%N)
g6_dir="$work/ate-g6"
G6_URL=http://127.0.0.1:8087
bin/fakeupstream -listen 127.0.0.1:0 -hostname ai-gateway.vercel.sh -api-key g6key -delay 20s -ready-file "$work/g6-fake.ready" >"$work/g6-fake.log" 2>&1 &
G6_FAKE_PID=$!
track_pid "$G6_FAKE_PID"
for _ in $(seq 1 50); do [[ -s "$work/g6-fake.ready" ]] && break; sleep 0.1; done
[[ -s "$work/g6-fake.ready" ]] || { echo "FAILED: G6 fakeupstream never became ready: $(cat "$work/g6-fake.log")"; exit 1; }
read -r G6_FAKE_ADDR G6_FAKE_CA < "$work/g6-fake.ready"
ATE_DIR="$g6_dir" ATE_STORE="dir:$g6_dir-store" ATE_LISTEN=127.0.0.1:8087 ATE_CAPACITY=2 \
  ATE_EGRESS_RESOLVE="ai-gateway.vercel.sh=$G6_FAKE_ADDR" ATE_EGRESS_TEST_CA="$G6_FAKE_CA" \
  AI_GATEWAY_API_KEY=g6key \
  bin/ate-ish >"$work/g6-node.log" 2>&1 &
G6_PID=$!
track_node "$G6_PID" "$g6_dir"
for _ in $(seq 1 100); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$G6_URL/admin/workers" || true)
  [[ "$code" == "200" ]] && break
  sleep 0.1
done
[[ "$code" == "200" ]] || { echo "FAILED: G6 node never came up: $(tail -c 1000 "$work/g6-node.log")"; exit 1; }
sed "s|atespace: ate-demo-pi|atespace: ate-demo-g6|" demo/pi-durable/template-gateway.yaml > "$work/g6-template.yaml"
ATE_URL="$G6_URL" bin/ate-ish create atespace ate-demo-g6 >/dev/null
ATE_URL="$G6_URL" bin/ate-ish create actor-template -f "$work/g6-template.yaml" >/dev/null
ATE_URL="$G6_URL" bin/ate-ish create actor g6agent --template pi-durable-data -a ate-demo-g6 >/dev/null
ATE_URL="$G6_URL" bin/ate-ish create egress-policy g6agent -a ate-demo-g6 -f demo/pi-durable/egress-policy-gateway-fake.yaml >/dev/null

node -e '
  const fs = require("fs");
  fetch("http://127.0.0.1:8087/submit", {method: "POST", headers: {"Content-Type": "application/json", "ate-target-actor": "ate-demo-g6/g6agent"}, body: JSON.stringify({prompt: "hello"})})
    .then(async r => fs.writeFileSync(process.argv[1], JSON.stringify({status: r.status})))
    .catch(e => fs.writeFileSync(process.argv[1], JSON.stringify({error: e.message})));
' "$work/g6-submit.json" &
G6_SUBMIT_PID=$!
# Wait for the actor's own outbound call to actually start (the egress
# listener log line) before suspending, so the suspend genuinely lands
# mid-flight rather than before the actor's own process is even in
# flight on it.
for _ in $(seq 1 100); do
  grep -q "egress: listening actor=ate-demo-g6/g6agent" "$work/g6-node.log" && break
  sleep 0.1
done
sleep 1
tc0=$(date +%s%N)
set +e
ATE_URL="$G6_URL" bin/ate-ish suspend actor g6agent -a ate-demo-g6
checkpoint_rc=$?
set -e
echo "  checkpoint while the actor's own outbound call was in flight: rc=$checkpoint_rc [$(( ($(date +%s%N)-tc0)/1000000 )) ms]"
wait "$G6_SUBMIT_PID" 2>/dev/null || true
echo "  the original in-flight request: $(cat "$work/g6-submit.json" 2>/dev/null)"
kill "$G6_PID" 2>/dev/null || true
wait "$G6_PID" 2>/dev/null || true
assert_no_runsc "$g6_dir/runsc-state"
kill "$G6_FAKE_PID" 2>/dev/null || true
echo "  [$(ms "$t0") ms]"

echo "== G7 restore latency =="
t0=$(date +%s%N)
g7_dir="$work/ate-g7"
G7_URL=http://127.0.0.1:8088
bin/fakeupstream -listen 127.0.0.1:0 -hostname ai-gateway.vercel.sh -api-key g7key -ready-file "$work/g7-fake.ready" >"$work/g7-fake.log" 2>&1 &
G7_FAKE_PID=$!
track_pid "$G7_FAKE_PID"
for _ in $(seq 1 50); do [[ -s "$work/g7-fake.ready" ]] && break; sleep 0.1; done
[[ -s "$work/g7-fake.ready" ]] || { echo "FAILED: G7 fakeupstream never became ready: $(cat "$work/g7-fake.log")"; exit 1; }
read -r G7_FAKE_ADDR G7_FAKE_CA < "$work/g7-fake.ready"
ATE_DIR="$g7_dir" ATE_STORE="dir:$g7_dir-store" ATE_LISTEN=127.0.0.1:8088 ATE_CAPACITY=2 \
  ATE_EGRESS_RESOLVE="ai-gateway.vercel.sh=$G7_FAKE_ADDR" ATE_EGRESS_TEST_CA="$G7_FAKE_CA" \
  AI_GATEWAY_API_KEY=g7key \
  bin/ate-ish >"$work/g7-node.log" 2>&1 &
G7_PID=$!
track_node "$G7_PID" "$g7_dir"
for _ in $(seq 1 100); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$G7_URL/admin/workers" || true)
  [[ "$code" == "200" ]] && break
  sleep 0.1
done
[[ "$code" == "200" ]] || { echo "FAILED: G7 node never came up: $(tail -c 1000 "$work/g7-node.log")"; exit 1; }
sed "s|atespace: ate-demo-pi|atespace: ate-demo-g7|" demo/pi-durable/template-gateway.yaml > "$work/g7-template.yaml"
ATE_URL="$G7_URL" bin/ate-ish create atespace ate-demo-g7 >/dev/null
ATE_URL="$G7_URL" bin/ate-ish create actor-template -f "$work/g7-template.yaml" >/dev/null
ATE_URL="$G7_URL" bin/ate-ish create actor g7agent --template pi-durable-data -a ate-demo-g7 >/dev/null
ATE_URL="$G7_URL" bin/ate-ish create egress-policy g7agent -a ate-demo-g7 -f demo/pi-durable/egress-policy-gateway-fake.yaml >/dev/null
curl -s -m 15 -X POST -H 'Content-Type: application/json' -H "ate-target-actor: ate-demo-g7/g7agent" -d '{"prompt":"hello"}' "$G7_URL/submit" -o /dev/null
ATE_URL="$G7_URL" bin/ate-ish suspend actor g7agent -a ate-demo-g7 >/dev/null
tr0=$(date +%s%N)
code=$(curl -s -m 15 -X POST -H 'Content-Type: application/json' -H "ate-target-actor: ate-demo-g7/g7agent" -d '{"prompt":"hello again"}' "$G7_URL/submit" -o /dev/null -w '%{http_code}')
restore_ms=$(( ($(date +%s%N)-tr0)/1000000 ))
[[ "$code" == "200" ]] || { echo "FAILED: first request after restore returned $code"; exit 1; }
echo "  first request after restore: ${restore_ms} ms total (restore + boot + the turn itself), code $code"
# Before the SIGUSR2 redial fix (init.go's outforwarder.restart), this
# measured 10000+ ms: the sandboxed ingress outforwarder's restored
# connection was stale and the bridge's own 10s wait for a fresh one
# from it always expired. 5000 ms here is a regression guard, not the
# precise bar (docs/ROOTLESS.md's own 1s is about the redial itself,
# not this full round trip's fake-model turn on top of it); see
# docs/PROGRESS-ROOTLESS.md's stage C section for the measured number.
[[ "$restore_ms" -lt 5000 ]] || { echo "FAILED: first request after restore took ${restore_ms} ms"; exit 1; }
kill "$G7_PID" 2>/dev/null || true
wait "$G7_PID" 2>/dev/null || true
assert_no_runsc "$g7_dir/runsc-state"
kill "$G7_FAKE_PID" 2>/dev/null || true
echo "  [$(ms "$t0") ms]"

echo "== total [$(ms "$t_start") ms] =="
