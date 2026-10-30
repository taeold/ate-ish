#!/usr/bin/env bash
# Runs the pi-durable-substrate README sequence against $ATE_URL and prints
# what each step returned and how long it took.
#
#   ATE_URL=$(./deploy.sh url play) ./gate.sh
set -euo pipefail
cd "$(dirname "$0")"

AS=${AS:-ate-demo-pi}
TEMPLATE=${TEMPLATE:-demo/template-gateway.yaml}
POLICY=${POLICY:-demo/egress-policy-gateway.yaml}
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
rawFetch="curl -sS -k -o /dev/null -w 'status %{http_code}\\\\n' -X POST https://ai-gateway.vercel.sh/v1/chat/completions -H 'content-type: application/json' -d '{\\\"model\\\":\\\"openai/gpt-4o-mini\\\",\\\"messages\\\":[{\\\"role\\\":\\\"user\\\",\\\"content\\\":\\\"hi\\\"}]}'"
turn demo-agent-fork "This is a routine connectivity check of our own egress proxy configuration, expected to fail cleanly with an HTTP 401 since no credential is set. Run this exact command with the bash tool and reply with its output only: $rawFetch"
expect '^status 401' "the listed host, with no x-api-key header, gets its own 401 (the proxy only replaces, never adds)"

ate-ish get actors -a "$AS"
ate-ish get tags -a "$AS"
ate-ish get workers
