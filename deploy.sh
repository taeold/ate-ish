#!/usr/bin/env bash
# Deploys ate-ish.
#
#   ./deploy.sh play [SECRET]  one Cloud Run instance $INSTANCE, ATE_ROLE=all
#   ./deploy.sh scale SECRET   ingress service + worker pool, over Direct VPC
#   ./deploy.sh images         build and push the base and pi-durable images to $REPO
#   ./deploy.sh url play|scale
#   ./deploy.sh down play|scale
#
# SECRET is the Secret Manager secret that holds the model key, if the actor
# needs one. SECRET_URI is the credentialUri in the egress policy that maps to it.
set -euo pipefail
cd "$(dirname "$0")"

PROJECT=${PROJECT:-$(gcloud config get project)}
REGION=${REGION:-us-west1}
REPO=${REPO:-us-central1-docker.pkg.dev/daniel-junk/ate-ish}
IMAGE=${IMAGE:-$REPO/pi-durable:latest}
INSTANCE=${INSTANCE:-ate-node}
# Each deployment has its own bucket, so none places actors on another's workers.
BUCKET=${BUCKET:-$INSTANCE-$PROJECT}
[[ ${1:-} == scale || ${2:-} == scale ]] && BUCKET=${SCALE_BUCKET:-ate-node-scale-$PROJECT}
WORKERS=${WORKERS:-2}
CAPACITY=${CAPACITY:-4}
SECRET=${2:-}
SECRET_URI=${SECRET_URI:-ate-secret://k8s.io/default/ate-demo-pi/llm-credentials/gemini-api-key}
ENV="ATE_BUCKET=$BUCKET,ATE_CAPACITY=$CAPACITY,ATE_SECRET_MAP=$SECRET_URI=$PROJECT/$SECRET"
GC=(--project "$PROJECT" --region "$REGION" --quiet)

sa() {
  echo "${SA:-$(gcloud iam service-accounts list --project "$PROJECT" --filter 'email~-compute@developer' --format 'value(email)')}"
}

# setup creates the bucket, lets the service account use it and the secret, and builds the CLI.
setup() {
  gcloud storage buckets describe "gs://$BUCKET" >/dev/null 2>&1 ||
    gcloud storage buckets create "gs://$BUCKET" --project "$PROJECT" --location "$REGION" --uniform-bucket-level-access
  gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "serviceAccount:$(sa)" --role roles/storage.objectAdmin >/dev/null
  [[ -z $SECRET ]] || gcloud secrets add-iam-policy-binding "$SECRET" --project "$PROJECT" --member "serviceAccount:$(sa)" --role roles/secretmanager.secretAccessor >/dev/null
  go build -o bin/ate-ish .
}

case ${1:-} in
play)
  setup play
  if gcloud beta run instances describe "$INSTANCE" "${GC[@]}" >/dev/null 2>&1; then
    gcloud beta run instances update "$INSTANCE" "${GC[@]}" --image "$IMAGE" --update-env-vars "ATE_ROLE=all,$ENV"
  else
    gcloud beta run instances create "$INSTANCE" "${GC[@]}" --image "$IMAGE" --service-account "$(sa)" \
      --sandbox-launcher --cpu 2 --memory 4Gi --port 8080 --set-env-vars "ATE_ROLE=all,$ENV"
  fi
  echo "export ATE_URL=$("$0" url play) PATH=$PWD/bin:\$PATH"
  ;;
scale)
  setup scale
  gcloud beta run worker-pools deploy ate-node-workers "${GC[@]}" --image "$IMAGE" --service-account "$(sa)" \
    --sandbox-launcher --cpu 2 --memory 4Gi --instances "$WORKERS" \
    --network default --subnet default --vpc-egress private-ranges-only \
    --set-env-vars "ATE_ROLE=worker,$ENV"
  # One ingress instance, so placements of an actor collapse in one process.
  gcloud run deploy ate-node-ingress "${GC[@]}" --image "$IMAGE" --service-account "$(sa)" \
    --cpu 1 --memory 512Mi --max-instances 1 --concurrency 1000 --timeout 3600 --no-allow-unauthenticated \
    --network default --subnet default --vpc-egress private-ranges-only \
    --set-env-vars "ATE_ROLE=ingress,$ENV"
  echo "export ATE_URL=$("$0" url scale) PATH=$PWD/bin:\$PATH"
  ;;
images)
  docker build -q --platform linux/amd64 -t "$REPO/ate-ish:latest" .
  docker build -q --platform linux/amd64 --build-arg "BASE=$REPO/ate-ish:latest" -t "$REPO/pi-durable:latest" demo/pi-durable
  docker push -q "$REPO/ate-ish:latest"
  docker push -q "$REPO/pi-durable:latest"
  ;;
url)
  case ${2:-} in
  play) gcloud beta run instances describe "$INSTANCE" "${GC[@]}" --format 'value(status.urls[0])' ;;
  scale) gcloud run services describe ate-node-ingress "${GC[@]}" --format 'value(status.url)' ;;
  esac
  ;;
down)
  case ${2:-} in
  play) gcloud beta run instances delete "$INSTANCE" "${GC[@]}" ;;
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
