#!/usr/bin/env bash
# Parking on the scale shape: 10 actors, 2 workers with 4 slots each.
#
#   ATE_URL=$(./deploy.sh url scale) ./park.sh
set -euo pipefail
cd "$(dirname "$0")"

AS=${AS:-park}
TEMPLATE=${TEMPLATE:-demo/template.yaml}
: "${ATE_URL:?set ATE_URL}"
export ATE_URL
TOKEN=$(gcloud auth print-identity-token)
sed "s|atespace: ate-demo-pi|atespace: $AS|" "$TEMPLATE" > "/tmp/$AS-template.yaml"

# wake ACTOR: one GET /readyz through the ingress.
wake() {
  local line
  line=$(curl -sS -o /dev/null -D "/tmp/$AS-$1.h" -w "$1 http=%{http_code} %{time_total}s" \
    -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: $AS/$1" "$ATE_URL/readyz")
  echo "$line $(grep -i '^ate-' "/tmp/$AS-$1.h" | tr -d '\r' | sort | tr '\n' ' ')"
}

bin/ate create atespace "$AS"
bin/ate create actor-template -f "/tmp/$AS-template.yaml"
for i in $(seq 10); do bin/ate create actor "p$i" --template pi-durable-data -a "$AS" >/dev/null; done

echo "== p1..p8 at once fill the 8 slots"
for i in $(seq 8); do wake "p$i" & done
wait
bin/ate get workers

echo "== p9 parks; p1 is suspended 2 s into the wait"
wake p9 &
sleep 2
bin/ate suspend actor p1 -a "$AS" >/dev/null
echo "suspended p1"
wait

echo "== p10 parks and nothing frees a slot"
wake p10
