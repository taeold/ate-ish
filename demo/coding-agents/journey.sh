#!/usr/bin/env bash
# Runs the README walkthrough of one coding-agents-on-substrate demo against
# $ATE_URL and prints what each step returned and how long it took.
#
#   DEMOS=~/coding-agents-on-substrate ATE_URL=... demo/coding-agents/journey.sh pi-durable|libfx|cursor-agent-sdk
set -euo pipefail
cd "$(dirname "$0")/../.."

DEMO=${1:?usage: journey.sh pi-durable|libfx|cursor-agent-sdk}
: "${ATE_URL:?set ATE_URL}" "${DEMOS:?set DEMOS to a clone of taeold/coding-agents-on-substrate}"
export ATE_URL
TEMPLATE=${TEMPLATE:-demo/coding-agents/$DEMO/template.yaml}
POLICY=${POLICY:-$DEMOS/$DEMO/egress-policy.yaml}
case $DEMO in
pi-durable) AS=ate-demo-pi T=pi-durable-data ;;
libfx) AS=ate-demo-fx T=fx-durable-data ;;
cursor-agent-sdk) AS=ate-demo-cursor T=cursor-agent ;;
esac
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

ms() { echo $(( ($(date +%s%N) - $1) / 1000000 )); }

ate() {
  echo "\$ ate $*"
  local t0
  t0=$(date +%s%N)
  bin/ate "$@" 2>&1 | sed 's/^/  /' || true
  echo "  [$(ms "$t0") ms]"
}

# req ACTOR METHOD PATH [BODY]: one request through the ingress, body in $work/out.
req() {
  local t0
  t0=$(date +%s%N)
  curl -sS -N --max-time "${MAX_TIME:-300}" -X "$2" "$ATE_URL$3" -D "$work/headers" -o "$work/out" \
    -H "Authorization: Bearer $(gcloud auth print-identity-token)" \
    -H "ate-target-actor: $AS/$1" -H "Content-Type: application/json" ${4:+-d "$4"} || true
  took=$(ms "$t0")
}

footer() {
  echo "  status: $(head -1 "$work/headers" | tr -d '\r')"
  echo "  ingress: $(grep -i '^ate-' "$work/headers" | tr -d '\r' | tr '\n' ' ')"
  echo "  [$took ms]"
}

# turn ACTOR PROMPT [SESSION TOKEN]: one turn the way the demo's README sends it.
turn() {
  case $DEMO in
  pi-durable)
    echo "\$ curl -X POST \$ATE_URL/submit -H 'ate-target-actor: $AS/$1' -d '{\"prompt\": \"$2\"}'"
    req "$1" POST /submit "$(jq -n --arg p "$2" '{prompt: $p}')"
    jq -r '
      "  settled: \(.settled | tostring)",
      "  user prompts: \([.view.messages[] | select(.role == "user") | .content] | tostring)",
      "  tool results: \([.view.messages[] | select(.role == "toolResult") | .content[0].text // "" | .[0:160]] | .[-2:] | tostring)",
      "  last answer: \(([.view.messages[] | select(.role == "assistant")][-1].content // []) | map(select(.type == "text") | .text) | join(" ") | .[0:300])"
    ' "$work/out" 2>/dev/null || echo "  body: $(head -c 600 "$work/out")"
    ;;
  libfx)
    echo "\$ curl -N -X POST \$ATE_URL/submit -H 'ate-target-actor: $AS/$1' -d '{\"prompt\": \"$2\"}'"
    req "$1" POST /submit "$(jq -n --arg p "$2" '{prompt: $p}')"
    events
    ;;
  cursor-agent-sdk)
    if [[ -z ${3:-} ]]; then
      echo "\$ curl -X POST \$ATE_URL/v1/session -H 'ate-target-actor: $AS/$1' -d '{\"message\": \"$2\"}'"
      req "$1" POST /v1/session "$(jq -n --arg m "$2" '{message: $m}')"
    else
      echo "\$ curl -X POST \$ATE_URL/v1/session/$3 -H 'ate-target-actor: $AS/$1' -d '{\"continuationToken\": ..., \"message\": \"$2\"}'"
      req "$1" POST "/v1/session/$3" "$(jq -n --arg m "$2" --arg t "$4" '{continuationToken: $t, message: $m}')"
    fi
    echo "  body: $(head -c 600 "$work/out")"
    ;;
  esac
  footer
}

# events: one line per NDJSON event, text deltas joined.
events() {
  jq -rc 'if .type == "text_delta" then empty else "  \(.cursor) \(.type) \(.name // "") \(.stopReason // "") \((.input // .content // .error // .message // "") | tostring | .[0:140])" end' "$work/out" 2>/dev/null ||
    echo "  body: $(head -c 600 "$work/out")"
  echo "  text: $(jq -rj 'select(.type == "text_delta") | .delta' "$work/out" 2>/dev/null | head -c 300)"
}

ate create atespace "$AS"
ate create actor-template -f "$TEMPLATE"
ate create actor demo-agent --template "$T" -a "$AS"
ate create egress-policy demo-agent -a "$AS" -f "$POLICY"

if [[ $DEMO == cursor-agent-sdk ]]; then
  turn demo-agent "Create hello.js that prints hi, run it with node, and report the output."
  session=$(jq -r '.sessionId // empty' "$work/out" 2>/dev/null || true)
  token=$(jq -r '.continuationToken // empty' "$work/out" 2>/dev/null || true)
  if [[ -n $session ]]; then
    echo "\$ curl -N \$ATE_URL/v1/session/$session/stream?startIndex=0"
    MAX_TIME=15 req demo-agent GET "/v1/session/$session/stream?startIndex=0"
    echo "  body: $(head -c 2000 "$work/out")"
    footer
  fi
  ate suspend actor demo-agent -a "$AS"
  if [[ -n $session ]]; then
    turn demo-agent "Change hello.js so it also prints the current year, run it again, and tell me what it printed." "$session" "$token"
    token=$(jq -r '.continuationToken // empty' "$work/out" 2>/dev/null || true)
  fi
else
  turn demo-agent "Create app.js with add(a, b) and a test.js that verifies it."
fi

ate suspend actor demo-agent -a "$AS"
ate create tag checkpoint-v1 --actor demo-agent -a "$AS"
ate create actor demo-agent-fork --template "$T" --tag checkpoint-v1 -a "$AS"
ate create egress-policy demo-agent-fork -a "$AS" -f "$POLICY"

if [[ $DEMO == cursor-agent-sdk ]]; then
  if [[ -n $session ]]; then
    turn demo-agent "Add add(a, b), call it with 2 and 3" "$session" "$token"
    turn demo-agent-fork "Add multiply(a, b), call it with 6 and 7" "$session" "$token"
  fi
else
  turn demo-agent "Add multiply(a, b) and structured logging to app.js."
  turn demo-agent-fork "Add divide(a, b) and edge-case tests to test.js."
fi

if [[ $DEMO == libfx ]]; then
  echo "\$ curl -N \$ATE_URL/events?cursor=9 -H 'ate-target-actor: $AS/demo-agent-fork'"
  MAX_TIME=5 req demo-agent-fork GET "/events?cursor=9"
  events
  footer
fi

ate get actors -a "$AS"
ate get tags -a "$AS"
ate get workers
