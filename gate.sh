#!/usr/bin/env bash
# Runs the pi-durable-substrate README sequence against $ATE_URL and prints
# what each step returned and how long it took.
#
#   ATE_URL=$(./deploy.sh url play) ./gate.sh
set -euo pipefail
cd "$(dirname "$0")"

AS=${AS:-ate-demo-pi}
TEMPLATE=${TEMPLATE:-demo/template.yaml}
POLICY=${POLICY:-demo/egress-policy.yaml}
: "${ATE_URL:?set ATE_URL}"
export ATE_URL
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
sed "s|atespace: ate-demo-pi|atespace: $AS|" "$TEMPLATE" > "$work/template.yaml"

ms() { echo $(( ($(date +%s%N) - $1) / 1000000 )); }

ate-ish() {
  echo "\$ ate-ish $*"
  local t0
  t0=$(date +%s%N)
  bin/ate-ish "$@" | sed 's/^/  /'
  echo "  [$(ms "$t0") ms]"
}

# turn ACTOR PROMPT: one POST /submit through the ingress.
turn() {
  echo "\$ curl -X POST \$ATE_URL/submit -H 'ate-target-actor: $AS/$1' -d '{\"prompt\": \"$2\"}'"
  local t0
  t0=$(date +%s%N)
  curl -sS -X POST "$ATE_URL/submit" -D "$work/headers" -o "$work/out.json" \
    -H "Authorization: Bearer $(gcloud auth print-identity-token)" \
    -H "ate-target-actor: $AS/$1" -H "Content-Type: application/json" \
    -d "$(jq -n --arg p "$2" '{prompt: $p}')"
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

# Enforcement by convention: the proxy refuses an unlisted host, and a
# process that ignores the proxy variables reaches it directly.
fetch="node -e \"fetch('https://example.com').then(r => console.log('status', r.status), e => console.log('failed:', e.cause?.message || e.message))\""
turn demo-agent-fork "Run this exact command with the bash tool and reply with its output only: $fetch"
expect '^failed:' "through the proxy, example.com is refused"
turn demo-agent-fork "Run this exact command with the bash tool and reply with its output only: NODE_USE_ENV_PROXY=0 $fetch"
expect '^status 200' "without the proxy, example.com answers"

ate-ish get actors -a "$AS"
ate-ish get tags -a "$AS"
ate-ish get workers
