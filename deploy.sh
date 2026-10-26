#!/usr/bin/env bash
# Builds and deploys ate-node.
#
#   ./deploy.sh setup         bucket and IAM for the service account
#   ./deploy.sh image         build bin/ate and push the image
#   ./deploy.sh play          one Cloud Run instance, ATE_ROLE=all
#   ./deploy.sh scale         ingress service + worker pool, over Direct VPC
#   ./deploy.sh url play|scale
#   ./deploy.sh down play|scale
set -euo pipefail
cd "$(dirname "$0")"

PROJECT=${PROJECT:-daniel-junk}
REGION=${REGION:-us-west1}
# Each shape has its own bucket, so neither places actors on the other's workers.
BUCKET=${BUCKET:-ate-node-$PROJECT}
[[ ${1:-} == scale || ${2:-} == scale ]] && BUCKET=${SCALE_BUCKET:-ate-node-scale-$PROJECT}
REPO=${REPO:-us-central1-docker.pkg.dev/$PROJECT/cloud-run-source-deploy/ate-node}
WORKERS=${WORKERS:-2}
CAPACITY=${CAPACITY:-4}
# The demo's k8s-style credentialUri, mapped to a Secret Manager secret.
SECRET_MAP=${SECRET_MAP:-ate-secret://k8s.io/default/ate-demo-pi/llm-credentials/gemini-api-key=$PROJECT/opencode-gemini-key}
ENV="ATE_BUCKET=$BUCKET,ATE_CAPACITY=$CAPACITY,ATE_SECRET_MAP=$SECRET_MAP"
GC=(--project "$PROJECT" --region "$REGION" --quiet)

sa() {
  echo "${SA:-$(gcloud iam service-accounts list --project "$PROJECT" --filter 'email~-compute@developer' --format 'value(email)')}"
}

digest() {
  echo "$REPO@$(gcloud artifacts docker images describe "$REPO:latest" --format 'value(image_summary.digest)')"
}

case ${1:-} in
setup)
  gcloud storage buckets describe "gs://$BUCKET" >/dev/null 2>&1 ||
    gcloud storage buckets create "gs://$BUCKET" --project "$PROJECT" --location "$REGION" --uniform-bucket-level-access
  gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "serviceAccount:$(sa)" --role roles/storage.objectAdmin >/dev/null
  for s in opencode-gemini-key ai-gateway-token; do
    gcloud secrets add-iam-policy-binding "$s" --project "$PROJECT" --member "serviceAccount:$(sa)" --role roles/secretmanager.secretAccessor >/dev/null
  done
  ;;
image)
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/ate .
  docker build -q --platform linux/amd64 -t "$REPO:latest" . >/dev/null
  docker push -q "$REPO:latest" >/dev/null
  digest
  ;;
play)
  if gcloud beta run instances describe ate-node "${GC[@]}" >/dev/null 2>&1; then
    gcloud beta run instances update ate-node "${GC[@]}" --image "$(digest)"
    exit
  fi
  gcloud beta run instances create ate-node "${GC[@]}" --image "$(digest)" --service-account "$(sa)" \
    --sandbox-launcher --cpu 2 --memory 4Gi --port 8080 --set-env-vars "ATE_ROLE=all,$ENV"
  ;;
scale)
  gcloud beta run worker-pools deploy ate-node-workers "${GC[@]}" --image "$(digest)" --service-account "$(sa)" \
    --sandbox-launcher --cpu 2 --memory 4Gi --instances "$WORKERS" \
    --network default --subnet default --vpc-egress private-ranges-only \
    --set-env-vars "ATE_ROLE=worker,$ENV"
  # One ingress instance, so placements of an actor collapse in one process.
  gcloud run deploy ate-node-ingress "${GC[@]}" --image "$(digest)" --service-account "$(sa)" \
    --cpu 1 --memory 512Mi --max-instances 1 --concurrency 1000 --timeout 3600 --no-allow-unauthenticated \
    --network default --subnet default --vpc-egress private-ranges-only \
    --set-env-vars "ATE_ROLE=ingress,$ENV"
  ;;
url)
  case ${2:-} in
  play) gcloud beta run instances describe ate-node "${GC[@]}" --format 'value(status.urls[0])' ;;
  scale) gcloud run services describe ate-node-ingress "${GC[@]}" --format 'value(status.url)' ;;
  esac
  ;;
down)
  case ${2:-} in
  play) gcloud beta run instances delete ate-node "${GC[@]}" ;;
  scale)
    gcloud run services delete ate-node-ingress "${GC[@]}"
    gcloud beta run worker-pools delete ate-node-workers "${GC[@]}"
    ;;
  esac
  ;;
*)
  sed -n '2,10p' "$0"
  exit 1
  ;;
esac
