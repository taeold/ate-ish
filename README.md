# ate-node: a small Agent Substrate for Cloud Run

ate-node runs [Agent Substrate](https://github.com/agent-substrate/substrate) actors on Cloud Run sandboxes with one program and one Cloud Storage bucket.

It takes the same `template.yaml`, `egress-policy.yaml`, `ate-target-actor` header and CLI verbs as Substrate. It has no database, no ateapi and no gRPC. It trades latency and scale for that: it targets tens of workers and thousands of actors.

---

## Quickstart

Run everything on one [Cloud Run instance](https://docs.cloud.google.com/run/docs/instances/create-and-manage-instances).

Prerequisites:

- A billed GCP project and an authenticated Google Cloud CLI with `gcloud beta run instances`.
- Go 1.25 or later and Docker, to build the image.
- An Artifact Registry repository for the image (`REPO` in `deploy.sh`).
- The default compute service account needs Storage Object Admin on the bucket and Secret Accessor on the model key secret. `./deploy.sh setup` grants both.
- Your gcloud account needs permission to invoke the instance, such as the Cloud Run Invoker role.

### 1. Create the bucket and build the image

```bash
git clone <this repository> ate-node && cd ate-node
export PROJECT=your-project-id REGION=us-west1
./deploy.sh setup   # bucket gs://ate-node-$PROJECT, IAM for the service account
./deploy.sh image   # builds bin/ate (server, sandbox init and CLI in one binary) and pushes it
```

### 2. Create the instance

```bash
./deploy.sh play
export ATE_URL=$(./deploy.sh url play)
```

This runs:

```bash
gcloud beta run instances create ate-node --image ... --sandbox-launcher \
  --cpu 2 --memory 4Gi --port 8080 --set-env-vars ATE_ROLE=all,ATE_BUCKET=ate-node-$PROJECT,...
```

The instance returned in 16 s. The CLI is `bin/ate`. It calls `$ATE_URL` with `gcloud auth print-identity-token`, and Cloud Run IAM checks the token.

### 3. Try it

Follow the walkthrough below. `./gate.sh` runs all of it and prints each result with its time. `runs/play.txt` is the output of the last run.

## The pi-durable walkthrough

This is the README of [pi-durable-substrate](https://github.com/taeold/pi-durable-substrate) with `kubectl ate` replaced by `ate` and the kind cluster replaced by the instance above.

The agent is a pi-durable coding agent. It keeps its conversation in SQLite under `/workspace`, so a suspended actor resumes with its history and files.

### Model key

The demo's Gemini key was revoked on 2026-10-26, so the measured runs use Vercel AI Gateway. That change lives in its own files:

| File | Change from the demo |
|---|---|
| `demo/gateway/server.ts` | `vercelAIGatewayProvider()` and model `anthropic/claude-haiku-4.5` instead of Google and Gemini. pi-ai 1.0.4 has no `claude-haiku-5.5` in its gateway catalog (the turn settles `unanswered` with reason `no_model`). It listens on `$PORT`. |
| `demo/template-gateway.yaml` | That image, and `AI_GATEWAY_API_KEY=ate-placeholder-key`. |
| `demo/egress-policy-gateway.yaml` | Host `ai-gateway.vercel.sh`, header `x-api-key` (what pi-ai sends, measured), secret `ate-secret://secretmanager.googleapis.com/<project>/ai-gateway-token`. |

`demo/template.yaml` and `demo/egress-policy.yaml` are the Gemini originals. The template uses Substrate HEAD's schema and port 8080, because a sandbox cannot publish a port below 1024. The policy's `ate-secret://k8s.io/...` URI maps to the Secret Manager secret `opencode-gemini-key` through `ATE_SECRET_MAP`. That path was not run.

### 1. Create the atespace, template and actor

```bash
ate create atespace ate-demo-pi
ate create actor-template -f demo/template-gateway.yaml
ate create actor demo-agent --template pi-durable-data -a ate-demo-pi
ate create egress-policy demo-agent -a ate-demo-pi -f demo/egress-policy-gateway.yaml
```

```
ATESPACE      NAME         TEMPLATE          STATE       WORKER   GENERATION   EGRESS POLICY   AGE
ate-demo-pi   demo-agent   pi-durable-data   suspended   <none>   0            false           0s
```

### 2. Turn 1

```bash
curl -X POST $ATE_URL/submit \
  -H "Authorization: Bearer $(gcloud auth print-identity-token)" \
  -H "ate-target-actor: ate-demo-pi/demo-agent" \
  -H "Content-Type: application/json" \
  -d '{"prompt": "Create app.js with add(a, b) and a test.js that verifies it."}'
```

```
settled: {"conversationId":1,"type":"input","status":"done","entry":7,"id":8,"answer":17}
tool results: ["Successfully wrote to app.js","Successfully wrote to test.js"]
ingress: ate-parked-ms: 0 ate-resume-ms: 9768 ate-worker: w-bb302aae
```

The first request for a suspended actor places it on a worker and starts it. Here that took 9.8 s, because the worker pulled the image first.

### 3. Suspend, tag and fork

```bash
ate suspend actor demo-agent -a ate-demo-pi
ate create tag checkpoint-v1 --actor demo-agent -a ate-demo-pi
ate create actor demo-agent-fork --template pi-durable-data --tag checkpoint-v1 -a ate-demo-pi
ate create egress-policy demo-agent-fork -a ate-demo-pi -f demo/egress-policy-gateway.yaml
```

Suspend sends SIGTERM, waits for the agent to exit, tars the sandbox's writable layer and uploads it as the actor's snapshot. A tag is a copy of that snapshot. A fork starts from a copy of the tag.

### 4. Divergent turns

```bash
curl ... -H "ate-target-actor: ate-demo-pi/demo-agent" -d '{"prompt": "Add multiply(a, b) and structured logging to app.js."}'
curl ... -H "ate-target-actor: ate-demo-pi/demo-agent-fork" -d '{"prompt": "Add divide(a, b) and edge-case tests to test.js."}'
```

```
demo-agent       settled entry 18, prompts ["Create app.js ...", "Add multiply(a, b) and structured logging to app.js."]  resume 1720 ms
demo-agent-fork  settled entry 18, prompts ["Create app.js ...", "Add divide(a, b) and edge-case tests to test.js."]     resume 1464 ms
```

Both actors restored from the same turn 1 history and continued on their own branch.

### 5. Egress

The real key never enters the sandbox. The actor has the placeholder, and the worker's proxy replaces `x-api-key` on requests to `ai-gateway.vercel.sh`. Any other host is refused:

```
node -e "fetch('https://example.com')..."                       failed: Request was cancelled.   (proxy: allowed=false)
NODE_USE_ENV_PROXY=0 node -e "fetch('https://example.com')..."  status 200
```

The second line is the limit described under [Limits](#limits). The actor image has no `curl`, so the check uses Node's `fetch`, run by the agent's bash tool.

## How it works

One binary, `ate`, has three roles, set by `ATE_ROLE`:

- `worker` runs actors as sandboxes. It unpacks each template image once and starts `sandbox run --rootfs <image> --import-tar <snapshot> -p <port>:8080`. Inside, pid 1 is `ate init`. It reads the environment from a 0600 file (the launcher logs `-e` values), starts the container command, forwards SIGTERM, and stays alive so the sandbox can be tarred after the agent exits. Each actor gets a host port and an egress proxy port.
- `ingress` routes requests by `ate-target-actor` and serves the admin API the CLI calls.
- `all` is both, in one process. That is the quickstart.

All state is in the bucket. Every record write is conditional on the generation read (`ifGenerationMatch`):

```
atespaces/<as>/atespace.json
atespaces/<as>/templates/<t>.json      the template as given
atespaces/<as>/actors/<a>.json         {template, status, owner, generation, egressPolicy, created}
atespaces/<as>/snapshots/<a>.tar       the last snapshot
atespaces/<as>/tags/<t>.tar
workers/<id>.json                      lease: {addr, capacity, hosted, expires}
```

The actor record is the only truth about placement. A request for an actor reads its record. If the actor runs on a worker with a live lease, the ingress proxies to it. Otherwise the ingress asks live workers with room, fewest actors first, to resume it. The worker that has a free slot claims the record with a conditional write (`running`, owner, generation + 1), then boots the actor. If no worker has room, the request waits up to 5 s for a slot and then gets `503 no worker has room for the actor`. The worker refuses proxied requests whose generation is not the one it runs.

A worker writes its lease every 10 s and after each change, at most once a second. On SIGTERM it suspends every actor and deletes its lease. In role `all`, a timer suspends actors idle for `ATE_IDLE_SECONDS` (600). `POST /admin/reap?idleSeconds=N` does the same on demand.

## Scale-out

```bash
./deploy.sh scale     # worker pool ate-node-workers (2 x 4 slots) + service ate-node-ingress, Direct VPC
export ATE_URL=$(./deploy.sh url scale)
./gate.sh             # the walkthrough again: runs/scale.txt
./park.sh             # 10 actors on 8 slots: runs/park.txt
```

The same image runs as a worker pool with `ATE_ROLE=worker` and as a Cloud Run service with `ATE_ROLE=ingress`. Workers register their Direct VPC address from the metadata server. The ingress reaches them over the VPC, and Cloud Run IAM protects the ingress. The scale shape uses its own bucket, `ate-node-scale-$PROJECT`, so it never places actors on the quickstart instance. The ingress runs with `--max-instances 1`, so concurrent placements of one actor collapse in one process.

The walkthrough passed on this shape too. Turn 1 ran on one worker, and the fork restored on the other worker.

Parking, from `runs/park.txt`:

```
== p1..p8 at once fill the 8 slots
p1 http=200 10.49s ate-resume-ms: 10308 ate-worker: w-127d32fc
...
p7 http=200 10.60s ate-resume-ms: 10434 ate-worker: w-127d32fc
== p9 parks; p1 is suspended 2 s into the wait
p9 http=200 5.61s ate-parked-ms: 4313 ate-resume-ms: 5525 ate-worker: w-127d32fc
== p10 parks and nothing frees a slot
p10 http=503 5.39s
actor park/p10: no worker has room for the actor
```

p9 waited 4.3 s. The suspend request left the CLI 2 s in and took about 1.5 s with the token. The freed slot reached the worker's lease within a second, and the ingress checks every 250 ms.

## Measured

All runs were on 2026-10-26 in us-west1, with 2 vCPU and 4 GiB per worker. n is the number of runs.

| Step | Measured | n |
|---|---|---|
| `instances create` | 16 s | 1 |
| `instances update --image` (old container suspends its actors) | 94 and 128 s | 2 |
| Image pull and unpack, first actor of an image on a worker (515 MB from us-central1) | 7.5 to 8.6 s | 5 |
| Boot, image cached, one at a time (sandbox run to wakeup probe) | 0.76 to 1.03 s | 9 |
| Boot, four at once on one worker | 2.08 to 2.32 s | 16 |
| Suspend on the worker (stop, tar, upload of 120 to 140 KB, record) | 0.47 to 0.56 s alone, 0.70 to 0.82 s for eight at once | 15 |
| Request to a suspended actor at the ingress (restore and boot) | 0.99 to 1.72 s | 8 |
| Request to a running actor at the ingress (two GCS reads) | 45 to 107 ms | 18 |
| Request to a running actor from a VM in us-west1, end to end | 123 to 166 ms | 10 |
| `ate` CLI call (0.76 s of it is `gcloud auth print-identity-token`) | 0.88 to 1.19 s, suspend 1.49 to 1.63 s | 42 |
| Turn at the client, any resume included (claude-haiku-4.5 through the egress proxy) | 2.9 to 21.1 s | 15 |

What bounds them. Booting is Node starting pi-durable inside gVisor, and four boots on two vCPUs take about twice as long each. Suspend and restore are dominated by GCS round trips to a same-region bucket. A request to a running actor pays two GCS reads at the ingress: the actor record and the owner's lease. The CLI pays a gcloud process for its token on every call.

## Estimated cost

These prices are an estimate. They use Cloud Run's instance-based list prices for tier 1 regions as I know them ($0.000018 per vCPU-second and $0.000002 per GiB-second). I did not confirm them on the pricing page for this README.

| Shape | Always on | Per hour | Per month |
|---|---|---|---|
| Quickstart: one instance, 2 vCPU, 4 GiB | yes | about $0.16 | about $115 |
| Scale-out: two workers like the instance, plus an ingress billed per request | workers yes | about $0.32 | about $230 |

The bucket holds kilobytes of JSON and a 120 to 140 KB tar per actor. Model calls through the gateway cost about $0.002 per turn here. All runs for this README cost well under a dollar in model calls.

## Limits

- **Rootfs fidelity only.** A snapshot is the sandbox's writable layer. Memory and processes are not captured, so an actor restarts from its files on every resume.
- **The workspace is RAM.** The launcher keeps the writable layer in memory (`--overlay2=root:memory`). A 20 MB workspace costs 20 MB of worker memory. Unpacked images and snapshot tars sit in the worker's in-memory `/tmp` too.
- **Egress is enforced by convention.** A sandbox without `--allow-egress` cannot reach the worker (measured: connection refused at 169.254.8.1 and 172.20.0.1). So actors run with egress allowed, and `HTTPS_PROXY`, `HTTP_PROXY` and `NODE_USE_ENV_PROXY=1` point them at their proxy. A process that ignores those variables reaches the internet directly (`status 200` above). The metadata server was not reachable from a sandbox in either mode.
- **Per-actor identity at the proxy is the listener port.** Another actor on the same worker could use a neighbor's port and its credentials.
- **Actors can reach the worker's address.** ate-node refuses requests from the sandbox network (172.20.0.0/16) on its own port. Actor connections came from 172.20.0.N in every run.
- **The launcher sets no per-sandbox CPU or memory limit.** `resources` in the template is stored and ignored. A worker counts actor slots (`ATE_CAPACITY`, 4).
- **Hot actors cannot be split.** One actor runs on one worker.
- **A worker that Cloud Run replaces loses actors** that did not finish suspending in the shutdown window. They come back from their last snapshot. Records survive anything, because the bucket is the truth.
- **Instances cannot be stopped.** `restart` and `update` replace the container. The actors then suspend and resume on the next request.
- **GCS takes about one write per second per object.** The first placement design wrote the actor record before asking a worker for a slot. Eight concurrent wakes then failed with `429 rateLimitExceeded` on the actor records. Workers now claim the record only after they reserve a slot.

## Cleanup

```bash
./deploy.sh down play
./deploy.sh down scale    # only if you deployed it
gcloud storage rm -r gs://ate-node-$PROJECT gs://ate-node-scale-$PROJECT
```
