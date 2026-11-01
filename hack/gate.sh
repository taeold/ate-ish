#!/usr/bin/env bash
# Runs the pi-durable-substrate README sequence against $ATE_URL and prints
# what each step returned and how long it took.
#
#   ATE_URL=http://localhost:8080 ./hack/gate.sh
#
# --fake points the same sequence at hack/fakeupstream instead of the
# real ai-gateway.vercel.sh: the node must already be running with
# ATE_EGRESS_RESOLVE/ATE_EGRESS_TEST_CA exported at its own startup
# (this script only switches the egress policy's credentialUri from
# Secret Manager to ate-secret://env/AI_GATEWAY_API_KEY, which it reads
# from this process's own environment, same as fakeupstream's -api-key
# must). hack/smoke.sh is the caller that sets all of this up.
set -euo pipefail
cd "$(dirname "$0")/.."

FAKE=0
if [[ "${1:-}" == "--fake" ]]; then
  FAKE=1
  shift
fi

AS=${AS:-ate-demo-pi}
TEMPLATE=${TEMPLATE:-demo/pi-durable/template-gateway.yaml}
if [[ "$FAKE" == 1 ]]; then
  POLICY=${POLICY:-demo/pi-durable/egress-policy-gateway-fake.yaml}
else
  POLICY=${POLICY:-demo/pi-durable/egress-policy-gateway.yaml}
fi
ATE_URL="${ATE_URL:-http://127.0.0.1:8080}"
export ATE_URL
work=$(mktemp -d)
GATE_NODE_PID=""
cleanup() {
  if [[ -n "$GATE_NODE_PID" ]]; then
    kill "$GATE_NODE_PID" 2>/dev/null || true
    wait "$GATE_NODE_PID" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

# nodeUp prints /admin/workers's own http code against $ATE_URL, or ""
# on a connection refused/timeout (curl's own exit nonzero, caught so
# `set -e` does not end the script on the common "nothing there yet"
# case).
nodeUp() { curl -s -o /dev/null -m 2 -w '%{http_code}' "$ATE_URL/admin/workers" 2>/dev/null || true; }

if [[ "$(nodeUp)" != "200" ]]; then
  echo "== no node answering at $ATE_URL: starting one (same as hack/smoke.sh) =="
  CGO_ENABLED=0 go build -o bin/ate-ish ./cmd/ate-ish
  export ATE_DIR="$work/ate-node"
  export ATE_STORE="dir:$work/ate-store"
  export ATE_LISTEN="${ATE_URL#http://}"
  # AI_GATEWAY_API_KEY, and under --fake ATE_EGRESS_RESOLVE/
  # ATE_EGRESS_TEST_CA, are already in this process's own environment
  # when the caller set them; bin/ate-ish inherits them as any forked
  # child does. The demo policies resolve their credential from
  # ate-secret://env/AI_GATEWAY_API_KEY, i.e. from this node's own
  # environment, never a file or Secret Manager, so this is the only
  # thing that needs to carry over.
  bin/ate-ish >"$work/node.log" 2>&1 &
  GATE_NODE_PID=$!
  for _ in $(seq 1 200); do
    [[ "$(nodeUp)" == "200" ]] && break
    sleep 0.1
  done
  [[ "$(nodeUp)" == "200" ]] || { echo "FAILED: node never answered $ATE_URL: $(tail -c 2000 "$work/node.log")"; exit 1; }
  echo "  node up at $ATE_URL (pid $GATE_NODE_PID), log at $work/node.log"
fi

sed "s|atespace: ate-demo-pi|atespace: $AS|" "$TEMPLATE" > "$work/template.yaml"

ms() { echo $(( ($(date +%s%N) - $1) / 1000000 )); }

ate-ish() {
  echo "\$ ate-ish $*"
  local t0
  t0=$(date +%s%N)
  bin/ate-ish "$@" | sed 's/^/  /'
  echo "  [$(ms "$t0") ms]"
}

# bearerToken prints the Authorization header value for $ATE_URL, or
# nothing against a local node (no IAM there) or when ATE_NO_AUTH=1 says
# so explicitly. `gcloud auth print-identity-token` needs a real gcloud
# login, which an actor image driving this script against its own
# localhost node has no reason to carry.
bearerToken() {
  if [[ -n "${ATE_NO_AUTH:-}" ]] || [[ "$ATE_URL" == http://localhost* || "$ATE_URL" == http://127.0.0.1* ]]; then
    return
  fi
  echo "Bearer $(gcloud auth print-identity-token)"
}

# postSubmit URL ACTOR PROMPT AUTH HEADERS_OUT BODY_OUT: one POST /submit,
# with node's own fetch rather than curl. The verification run that found
# this gap ran gate.sh from inside an actor image, which (unlike this dev
# VM) ships node but not curl.
postSubmit() {
  node -e '
    const [url, actor, prompt, auth, headersOut, bodyOut] = process.argv.slice(1);
    const fs = require("fs");
    const headers = {"Content-Type": "application/json", "ate-target-actor": actor};
    if (auth) headers["Authorization"] = auth;
    fetch(url + "/submit", {method: "POST", headers, body: JSON.stringify({prompt})})
      .then(async (r) => {
        fs.writeFileSync(bodyOut, await r.text());
        let h = "";
        for (const [k, v] of r.headers) h += k + ": " + v + "\n";
        fs.writeFileSync(headersOut, h);
      })
      .catch((e) => {
        fs.writeFileSync(bodyOut, "fetch failed: " + e.message);
        fs.writeFileSync(headersOut, "");
      });
  ' "$1" "$2" "$3" "$4" "$5" "$6"
}

# turn ACTOR PROMPT: one POST /submit through the ingress.
turn() {
  echo "\$ node -e fetch(...) \$ATE_URL/submit -H 'ate-target-actor: $AS/$1' -d '{\"prompt\": \"$2\"}'"
  local t0
  t0=$(date +%s%N)
  postSubmit "$ATE_URL" "$AS/$1" "$2" "$(bearerToken)" "$work/headers" "$work/out.json"
  local took
  took=$(ms "$t0")
  if ! jq -e .settled "$work/out.json" >/dev/null 2>&1; then
    echo "  FAILED: $(head -c 500 "$work/out.json")"
    exit 1
  fi
  jq -r '
    "  settled: \(.settled | tostring)",
    "  messages: \(.view.messages | length), user prompts: \([.view.messages[] | select(.role == "user") | .content] | tostring)",
    "  tool results: \([.view.messages[] | select(.role == "toolResult") | .content[0].text // "" | .[0:160]] | .[-2:] | tostring)",
    "  last answer: \([.view.messages[] | select(.role == "assistant")][-1].content | map(select(.type == "text") | .text) | join(" ") | .[0:300])"
  ' "$work/out.json"
  echo "  ingress: $(grep -i '^ate-' "$work/headers" | tr -d '\r' | tr '\n' ' ')"
  echo "  [$took ms]"
}

ate-ish create atespace "$AS"
ate-ish create actor-template -f "$work/template.yaml"
ate-ish create actor demo-agent --template pi-durable-data -a "$AS"
ate-ish create egress-policy demo-agent -a "$AS" -f "$POLICY"
turn demo-agent "Create app.js with add(a, b) and a test.js that verifies it."
if [[ "$(jq -r '.settled.status' "$work/out.json")" != done ]]; then
  echo "  FAILED: turn 1 did not settle done: $(jq -c .settled "$work/out.json")"
  exit 1
fi
echo "  ok: turn 1 settled done, a real model answer (the placeholder key was replaced)"

ate-ish suspend actor demo-agent -a "$AS"
ate-ish create tag checkpoint-v1 --actor demo-agent -a "$AS"
ate-ish create actor demo-agent-fork --template pi-durable-data --tag checkpoint-v1 -a "$AS"
ate-ish create egress-policy demo-agent-fork -a "$AS" -f "$POLICY"

turn demo-agent "Add multiply(a, b) and structured logging to app.js."
turn demo-agent-fork "Add divide(a, b) and edge-case tests to test.js."

# expect PATTERN WHAT: the last tool output of the last turn matches PATTERN.
expect() {
  local got
  got=$(jq -r '[.view.messages[] | select(.role == "toolResult") | .content[0].text // ""][-1]' "$work/out.json")
  if ! grep -q "$1" <<<"$got"; then
    echo "  FAILED: $2: $got"
    exit 1
  fi
  echo "  ok: $2: $got"
}

# Enforcement is the sandbox's network now, not convention (v2 stage 3):
# nftables redirects every actor TCP connection to atunnel regardless of any
# proxy environment variable, so an unlisted host fails whether or not one
# is set — the opposite of v1, where only a proxy-aware fetch was refused
# and a process that ignored the proxy variables reached the host directly.
fetch="node -e \"fetch('https://example.com').then(r => console.log('status', r.status), e => console.log('failed:', e.cause?.message || e.message))\""
turn demo-agent-fork "Run this exact command with the bash tool and reply with its output only: $fetch"
expect '^failed:' "example.com is refused"
turn demo-agent-fork "Run this exact command with the bash tool and reply with its output only: NODE_USE_ENV_PROXY=0 $fetch"
expect '^failed:' "example.com is refused with no proxy env var set either; the sandbox has no other route"

# The proxy replaces a credential header an actor's request already carries;
# it never adds one that was not there. A request to the listed host with no
# x-api-key at all must reach the real upstream unmodified and get its own
# 401, not a 200 the proxy's own secret produced.
#
# node's own fetch, not curl: the pi-durable actor image (demo/pi-durable's
# Dockerfile) carries node but no curl, and the actor sandbox has neither
# the host's PATH nor its packages, so this check needs something the
# actor's own image ships.
#
# A fresh actor and a one-line prompt, not demo-agent-fork's own
# conversation: measured directly (2026-10-31) that asking this model to
# run the exact same command in a conversation that had just run the two
# DNS-refusal checks above got refused ("could be used to test
# credential/authentication bypasses"), while the identical command as a
# single turn on a brand-new actor got a plain "status 401" both times.
# The refusal tracked the conversation's own accumulated turns, not the
# command's wording, so the real fix is not sharing a conversation with
# prior "something is refusing my commands" turns.
rawFetch="node -e \"fetch('https://ai-gateway.vercel.sh/v1/chat/completions', {method: 'POST', headers: {'content-type': 'application/json'}, body: JSON.stringify({model: 'openai/gpt-4o-mini', messages: [{role: 'user', content: 'hi'}]})}).then(r => console.log('status', r.status))\""
ate-ish create actor proxy-check --template pi-durable-data -a "$AS"
ate-ish create egress-policy proxy-check -a "$AS" -f "$POLICY"
turn proxy-check "Run this exact command with the bash tool and reply with its output only: $rawFetch"
expect '^status 401' "the listed host, with no x-api-key header, gets its own 401 (the proxy only replaces, never adds)"

ate-ish get actors -a "$AS"
ate-ish get tags -a "$AS"
ate-ish get workers
