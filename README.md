# ate-ish

ate-ish is a small [Agent Substrate](https://github.com/agent-substrate/substrate) for Cloud Run: one binary, one bucket, no cluster.

```bash
./deploy.sh play gemini-api-key
```

Run it from a clone of this repository. The command creates the bucket `gs://ate-node-$PROJECT`, gives the default compute service account access to the bucket and to the secret, builds the `ate` CLI into `bin/`, and creates the Cloud Run instance `ate-node` from the public image `us-central1-docker.pkg.dev/daniel-junk/ate-ish/pi-durable:latest`. It prints the `export` line for `ATE_URL` and `PATH`.

Before you run it:

- Store your Gemini API key in Secret Manager. The secret name is the argument to `deploy.sh`: `printf %s "$GEMINI_API_KEY" | gcloud secrets create gemini-api-key --data-file=-`.
- Use a billed project, the Google Cloud CLI with `gcloud beta run instances`, and Go 1.27 or later. Cloud Run instances and sandboxes are in preview.
- `deploy.sh` uses your gcloud project and the region us-west1. Set `PROJECT` and `REGION` to change them.
- Your gcloud account needs permission to invoke the instance, such as the Cloud Run Invoker role.

## The pi-durable walkthrough

This is the README of [pi-durable-substrate](https://github.com/taeold/pi-durable-substrate) with `kubectl ate` replaced by `ate` and the kind cluster replaced by the instance from the quickstart. `./gate.sh` runs every step and fails if a turn or the egress check fails.

The outputs below are from `runs/play.txt`. That run used the Vercel AI Gateway variant (see [Use Vercel AI Gateway instead](#use-vercel-ai-gateway-instead)), because the Gemini key in the test project is revoked. The Gemini path has not run on ate-ish.

### How it works

`pi-durable` runs the coding agent loop and writes every conversation turn and tool checkpoint to a SQLite database at `/workspace/.pi/agent.sqlite`. ate-ish runs each agent as an actor in its own Cloud Run sandbox.

1. **Suspend and auto-resume.** `ate suspend actor` sends `SIGTERM` to the agent, waits for it to exit, tars the sandbox's writable layer with `sandbox tar`, and uploads the tar to the bucket as the actor's snapshot. When the next request arrives for that actor, the ingress asks a worker with a free slot to resume it. The worker starts a sandbox with `--import-tar` and waits for the wakeup probe before the ingress forwards the request.
2. **Checkpoint and fork.** A tag is a copy of a suspended actor's snapshot. An actor created with `--tag` starts from a copy of the tag. Both actors start from the same history and files, then diverge.
3. **Egress credential injection.** The sandbox runs with a placeholder `GEMINI_API_KEY`. Each actor's `HTTPS_PROXY` points at its own proxy in the worker. For `generativelanguage.googleapis.com`, the proxy replaces `x-goog-api-key` with the secret, so the key never enters the sandbox or its snapshots. `ATE_SECRET_MAP` maps the policy's `ate-secret://k8s.io/...` URI to the secret you passed to `deploy.sh`.

One binary has three roles, set by `ATE_ROLE`. The `worker` role runs actors. The `ingress` role routes requests by the `ate-target-actor` header and serves the admin API that the CLI calls. The `all` role is both, and the quickstart uses it. All state is in the bucket, and every record write is conditional on the generation read (`ifGenerationMatch`):

```
atespaces/<as>/atespace.json
atespaces/<as>/templates/<t>.json      the template as given
atespaces/<as>/actors/<a>.json         {template, status, owner, generation, egressPolicy, created}
atespaces/<as>/snapshots/<a>.tar       the last snapshot
atespaces/<as>/tags/<t>.tar
workers/<id>.json                      lease: {addr, capacity, hosted, expires}
```

### 1. Create the atespace, template and actor

```bash
ate create atespace ate-demo-pi
ate create actor-template -f demo/template.yaml
ate create actor demo-agent --template pi-durable-data -a ate-demo-pi
ate create egress-policy demo-agent -a ate-demo-pi -f demo/egress-policy.yaml
```

```
ATESPACE      NAME         TEMPLATE          STATE       WORKER   GENERATION   EGRESS POLICY   AGE
ate-demo-pi   demo-agent   pi-durable-data   suspended   <none>   0            false           0s
```

`demo/egress-policy.yaml` is the file from pi-durable-substrate. `demo/template.yaml` is that repository's template with two changes. The command is `node /actor/server.ts`, because the agent is baked into the image at `/actor`. The agent listens on `PORT=8080` and the probe uses port 8080, because a sandbox cannot publish a port below 1024. The `image` field names the public image and is ignored.

### 2. Run Turn 1

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
ingress: ate-parked-ms: 0 ate-resume-ms: 2971 ate-worker: w-e742006e
```

The first request for a suspended actor places it on a worker and starts it. The `ate-resume-ms` header is the time from the request reaching the ingress to the actor being ready.

### 3. Suspend, tag a checkpoint, and fork

```bash
ate suspend actor demo-agent -a ate-demo-pi
ate create tag checkpoint-v1 --actor demo-agent -a ate-demo-pi
ate create actor demo-agent-fork --template pi-durable-data --tag checkpoint-v1 -a ate-demo-pi
ate create egress-policy demo-agent-fork -a ate-demo-pi -f demo/egress-policy.yaml
```

### 4. Send divergent prompts to both branches

```bash
curl ... -H "ate-target-actor: ate-demo-pi/demo-agent" -d '{"prompt": "Add multiply(a, b) and structured logging to app.js."}'
curl ... -H "ate-target-actor: ate-demo-pi/demo-agent-fork" -d '{"prompt": "Add divide(a, b) and edge-case tests to test.js."}'
```

```
demo-agent       entry 18, prompts ["Create app.js ...", "Add multiply(a, b) and structured logging to app.js."]   resume 937 ms
demo-agent-fork  entry 18, prompts ["Create app.js ...", "Add divide(a, b) and edge-case tests to test.js."]       resume 989 ms
```

Both actors restored the Turn 1 history from the same snapshot and continued on their own branch.

### 5. Check egress

`gate.sh` asks the fork's agent to run `node -e "fetch('https://example.com')..."` with its bash tool, once as is and once with `NODE_USE_ENV_PROXY=0`:

```
ok: through the proxy, example.com is refused: failed: Request was cancelled.
ok: without the proxy, example.com answers: status 200
```

The second line is the limit described under [Limits](#limits). The image has no `curl`, so the check uses Node's `fetch`.

### Use Vercel AI Gateway instead

`demo/pi-durable/server-gateway.ts` is the same agent with `vercelAIGatewayProvider()` and the model `anthropic/claude-haiku-4.5`. The pi-durable image holds both servers, and the template picks one.

1. Store your AI Gateway key in Secret Manager, for example as `ai-gateway-token`.
2. Run `./deploy.sh play ai-gateway-token`.
3. In `demo/egress-policy-gateway.yaml`, replace `daniel-junk/ai-gateway-token` with your project and secret.
4. Use `demo/template-gateway.yaml` and `demo/egress-policy-gateway.yaml` in steps 1 and 3, or run `TEMPLATE=demo/template-gateway.yaml POLICY=demo/egress-policy-gateway.yaml ./gate.sh`.

The gateway policy replaces `x-api-key` on `ai-gateway.vercel.sh`, which is the header pi-ai sends.

## Build your own actor

One deployment runs one actor template, and the actor is baked into the node image. A sandbox's root is the node container's filesystem, read only, under a writable overlay. Build your image from the base image and copy the actor to `/actor`:

```Dockerfile
FROM us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest
COPY . /actor
```

The base image is Debian trixie-slim with `ca-certificates` and `/usr/local/bin/ate`. Add the runtime your command needs. `demo/pi-durable/Dockerfile` copies `node` from `node:24-slim`.

ate-ish reads these fields of `template.yaml`: `command`, `args`, `env`, `wakeupProbe`, `volumeMounts` and `volumes` (`durableDir` and the `systemInfo` trust bundle). It stores the other fields and ignores them. The `image` field is accepted and ignored.

Deploy your image with `IMAGE=<your image> ./deploy.sh play <secret>`, then run `ate create actor-template -f template.yaml`.

The images are public in Artifact Registry today. The intended distribution is GitHub Container Registry, with these commands:

```bash
docker build --platform linux/amd64 -t ghcr.io/taeold/ate-ish:latest .
docker build --platform linux/amd64 --build-arg BASE=ghcr.io/taeold/ate-ish:latest -t ghcr.io/taeold/ate-ish/pi-durable:latest demo/pi-durable
docker push ghcr.io/taeold/ate-ish:latest
docker push ghcr.io/taeold/ate-ish/pi-durable:latest
```

Cloud Run does not deploy from GHCR directly. Deploy from Artifact Registry, or from an Artifact Registry remote repository for `ghcr.io`.

## What Agent Substrate needs and what ate-ish needs

| Agent Substrate | ate-ish |
|---|---|
| A Kubernetes cluster, kind or GKE, with the `ate-system` components | One Cloud Run instance, or a Cloud Run worker pool and a service |
| ate-api-server with Postgres for actor records | JSON records in one Cloud Storage bucket, written with `ifGenerationMatch` |
| A `WorkerPool` of gVisor or microVM pods, run by atecontroller | Cloud Run sandboxes, started by the `ate` worker |
| atelet and ateworker for snapshots, and a golden snapshot per template | `sandbox tar` of the writable layer, uploaded to the bucket |
| atenet-router, an Envoy ext_proc, for `ate-target-actor` | The `ate` ingress, a Go reverse proxy |
| atenet-egress and a credential provider | One HTTPS proxy per actor in the worker, with secrets from Secret Manager |
| `kubectl-ate` over gRPC | `ate` over HTTP, with a gcloud identity token |
| A template image per actor template, pulled by the worker | The actor in the node image |

## Agent Substrate's demo templates

Only pi-durable has run on ate-ish. The other rows follow from the snapshot fidelity each template asks for in `substrate/demos`, because ate-ish delivers ROOTFS fidelity.

| Demo | Result | Reason |
|---|---|---|
| pi-durable (pi-durable-substrate) | Pass | Its state is SQLite and files in `/workspace`. `runs/play.txt` and `runs/scale.txt`. |
| `counter`, the file counter | Pass | The file is on a durable volume, and the writable layer keeps it. |
| `counter`, the in-memory counter | Fail | It asks for MEMORY. The process restarts on resume, so the count starts at 0. |
| `jupyter` | Fail | It asks for MEMORY. Kernel state is lost on resume. |
| `parking` | Fail | It asks for MEMORY for its counter. `park.sh` runs the parking behavior with pi-durable. |
| `egress` | Fail | It asks for MEMORY. It also expects transparent redirect and per-actor mTLS identity at a gateway. ate-ish uses `HTTPS_PROXY`. |
| `multi-template` | Fail | It asks for MEMORY. It also puts two templates with two binaries on one pool. |
| `sandbox` | Fail | It asks for MEMORY. Its client also calls ateapi over gRPC. |
| `claude-code-multiplex` | Fail | Its three templates ask for MEMORY. It also drives actors through ateapi, not `ate-target-actor`. |

## Measured

All runs were on 2026-10-28 in us-west1, on the published pi-durable image, with 2 vCPU and 4 GiB per worker. `runs/play.txt` is the walkthrough on the instance. `runs/scale.txt` is the walkthrough on the scale shape, an ingress service and two workers with four slots each (`./deploy.sh scale <secret>`). There, Turn 1 ran on one worker and the fork restored on the other. `runs/park.txt` is `./park.sh` on the scale shape. n is the number of runs.

| Step | Measured | n |
|---|---|---|
| `instances create` (2026-10-26) | 16 s | 1 |
| `deploy.sh play` on a running instance (`instances update --image`) | 92 and 103 s | 2 |
| `deploy.sh scale` | 31 and 41 s | 2 |
| Boot, first sandbox on a new container (sandbox run to wakeup probe) | 2.64 to 2.71 s | 3 |
| Boot, later sandboxes one at a time | 0.70 to 0.95 s | 4 |
| Boot, two at once on one worker | 1.15 to 1.22 s | 4 |
| Boot, four at once on one worker | 2.33 to 2.36 s | 4 |
| Suspend on the worker (stop, tar, upload of 122 to 164 KB, record) | 0.51 to 0.55 s | 5 |
| Request to a suspended actor at the ingress, worker already booted once | 0.94 to 1.24 s | 3 |
| Request to a running actor at the ingress (two GCS reads) | 47 to 77 ms | 4 |
| `ate` CLI call, `gcloud auth print-identity-token` included | 0.95 to 1.18 s, suspend 1.61 and 1.62 s | 22 |
| Turn at the client, resume included (claude-haiku-4.5 through the egress proxy) | 2.97 to 14.6 s | 10 |

Parking, from `runs/park.txt`. p1 to p8 at once filled the 8 slots, and all answered 200 in 1.6 to 4.4 s. p9 waited 3.9 s for a slot, which opened when p1 was suspended 2 s into the wait, and answered 200 at 5.2 s. p10 waited 5 s, no slot opened, and it got `503 no worker has room for the actor` at 5.5 s.

What bounds them. Booting is Node starting pi-durable inside gVisor, and four boots on two vCPUs take about three times as long as one. The first boot on a new container is about 2 s slower than later ones. The likely cause is Cloud Run loading the image's files on first read, but that was not measured. Suspend and restore are mostly GCS round trips to a bucket in the same region. A request to a running actor pays two GCS reads at the ingress, for the actor record and for the owner's lease. The CLI starts a gcloud process for its token on every call. In the parking run, p2 and p7 reached the ingress 2.7 s after the other six, according to the Cloud Run request log, and the ingress placed them in 1.5 s.

## Limits

- **ROOTFS fidelity only.** A snapshot is the sandbox's writable layer. Memory and processes are not in it, so an actor runs its command from the start on every resume.
- **The workspace is RAM on the worker.** The launcher keeps the writable layer in memory (`--overlay2=root:memory`). A 20 MB workspace uses 20 MB of worker memory, and the snapshot tar is in the worker's in-memory `/tmp` during a suspend.
- **Egress is enforced by convention.** A sandbox without `--allow-egress` cannot reach the worker, so it cannot reach its proxy (measured: connection refused at 169.254.8.1 and 172.20.0.1). Actors therefore run with egress allowed, and `HTTPS_PROXY`, `HTTP_PROXY` and `NODE_USE_ENV_PROXY=1` point them at their proxy. The proxy refuses hosts that are not in the policy (`failed: Request was cancelled.`). A process that ignores those variables reaches the internet directly (`status 200`). `gate.sh` asserts both results on every run.
- **The proxy knows an actor by its listener port.** Another actor on the same worker can use a neighbor's port and its credentials.
- **One ingress instance.** The scale shape runs the ingress with `--max-instances 1`, so concurrent placements of one actor share one process.
- **One template per deployment.** Every actor runs from the node image. Two templates can share a deployment only if the image holds both, as `demo/pi-durable` does for its two servers.
- **A worker has 10 s to drain when Cloud Run replaces it.** On `SIGTERM` the worker suspends every actor and deletes its lease. An actor whose suspend does not finish in 10 s comes back from its last snapshot.
- **No per-sandbox CPU or memory limit.** The launcher sets none. `resources` in the template is stored and ignored, and a worker counts slots (`ATE_CAPACITY`, 4).
- **Update a stopped instance after you start it.** On a stopped instance, `instances update --image` changes the spec but does not import the image, and `instances start` runs the previous image (measured once).

## Estimated cost

These prices are estimates from Cloud Run's instance-based list prices for tier 1 regions, $0.000018 per vCPU-second and $0.000002 per GiB-second. They are not confirmed against the pricing page or a bill.

| Shape | Always on | Per hour | Per month |
|---|---|---|---|
| Quickstart, one instance with 2 vCPU and 4 GiB | yes | about $0.16 | about $115 |
| Scale shape, two such workers and an ingress billed per request | workers yes | about $0.32 | about $230 |

The bucket holds a few KB of JSON and one 120 to 170 KB tar per actor. A Turn through the gateway with claude-haiku-4.5 cost about $0.002 in the 2026-10-26 runs.

## Cleanup

```bash
./deploy.sh down play
./deploy.sh down scale    # only if you deployed it
gcloud storage rm -r gs://ate-node-$PROJECT gs://ate-node-scale-$PROJECT
gcloud secrets delete gemini-api-key
```
