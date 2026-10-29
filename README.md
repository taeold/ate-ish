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

`demo/egress-policy.yaml` is the file from pi-durable-substrate. `demo/template.yaml` is that repository's template with two changes. The command is `node /actor/server.ts`, because the agent is baked into the image at `/actor`. The agent listens on `PORT=8080` and the probe uses port 8080, because a sandbox cannot bind a port below 1024. The `image` field names the public image and is ignored.

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

To start from an image you already have, add the binary and the entrypoint. The binary is static and carries its own root certificates. [Running the Agent Substrate demos](#running-the-agent-substrate-demos) does this for three images.

```Dockerfile
FROM <your image>
COPY --from=us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest /usr/local/bin/ate /usr/local/bin/ate
ENTRYPOINT ["/usr/local/bin/ate", "serve"]
```

Deploy your image with `IMAGE=<your image> ./deploy.sh play <secret>`, then run `ate create actor-template -f template.yaml`.

The images are public in Artifact Registry today. The intended distribution is GitHub Container Registry, with these commands:

```bash
docker build --platform linux/amd64 -t ghcr.io/taeold/ate-ish:latest .
docker build --platform linux/amd64 --build-arg BASE=ghcr.io/taeold/ate-ish:latest -t ghcr.io/taeold/ate-ish/pi-durable:latest demo/pi-durable
docker push ghcr.io/taeold/ate-ish:latest
docker push ghcr.io/taeold/ate-ish/pi-durable:latest
```

Cloud Run does not deploy from GHCR directly. Deploy from Artifact Registry, or from an Artifact Registry remote repository for `ghcr.io`.

## Running the Agent Substrate demos

[taeold/coding-agents-on-substrate](https://github.com/taeold/coding-agents-on-substrate) has three coding agents written for Agent Substrate on kind. Each one runs on ate-ish from the demo's own image with `ate` added. `demo/coding-agents/<demo>/` holds a Dockerfile and a template for each demo. The demos' `egress-policy.yaml` files are used unchanged. `demo/coding-agents/journey.sh <demo>` runs a demo's README walkthrough and prints each step with its time. The results below are from `runs/coding-agents-*.txt`, recorded on 2026-10-29 in us-west1 with one instance per demo.

Every demo needs three changes:

- The Dockerfile starts from the demo's image, copies `/usr/local/bin/ate` from the base image, and sets `ENTRYPOINT ["/usr/local/bin/ate", "serve"]`. `ate` carries its own root certificates, because the demo images are built on `node:24-slim`, which has none.
- The template sets `command`. The demos' templates leave the command to the image's `CMD`, and ate-ish does not read the image's configuration.
- The server listens on port 8080 and the wakeup probe uses port 8080, because a sandbox cannot bind a port below 1024 (see [Limits](#limits)).

Each demo gets its own instance, because one deployment runs one template. `INSTANCE` names the instance, and its bucket is `gs://$INSTANCE-$PROJECT`. `SECRET_URI` is the `credentialUri` in the demo's egress policy, and `deploy.sh` maps it to the secret it is given.

The walkthroughs read the egress policies from a clone of the demos:

```bash
git clone https://github.com/taeold/coding-agents-on-substrate ../coding-agents-on-substrate
DEMOS=$PWD/../coding-agents-on-substrate
```

The images `us-central1-docker.pkg.dev/daniel-junk/ate-ish/coding-agents-<demo>:latest` are public. To build your own instead, build the demo's image and then the ate-ish layer, with `REPO` set to your Artifact Registry repository:

```bash
docker build --platform linux/amd64 -t coding-agents/libfx $DEMOS/libfx/agent
docker build --platform linux/amd64 -t $REPO/coding-agents-libfx demo/coding-agents/libfx
docker push $REPO/coding-agents-libfx
```

The walkthroughs below are the demos' READMEs with `kubectl ate` replaced by `ate` and `localhost:8000` replaced by `$ATE_URL`. Each request needs an identity token:

```bash
TOKEN=$(gcloud auth print-identity-token)
```

### pi-durable

[pi-durable](https://github.com/taeold/coding-agents-on-substrate/tree/main/pi-durable) uses a Gemini key.

| File | Change |
|---|---|
| `demo/coding-agents/pi-durable/Dockerfile` | `FROM coding-agents/pi-durable`, the `ate` binary and the entrypoint. One `sed` changes `server.listen(80, ...)` in `/app/server.ts` to `server.listen(Number(process.env.PORT ?? 80), ...)`. |
| `demo/coding-agents/pi-durable/template.yaml` | `command: [node, /app/server.ts]`, `PORT=8080`, and wakeup probe port 8080. |
| `egress-policy.yaml` | None. |
| Secret | The Gemini key in Secret Manager. `deploy.sh` maps the policy's `credentialUri` to it by default. |

```bash
INSTANCE=ate-node-pi-durable IMAGE=us-central1-docker.pkg.dev/daniel-junk/ate-ish/coding-agents-pi-durable:latest \
  ./deploy.sh play gemini-api-key
export ATE_URL=$(INSTANCE=ate-node-pi-durable ./deploy.sh url play) PATH=$PWD/bin:$PATH

ate create atespace ate-demo-pi
ate create actor-template -f demo/coding-agents/pi-durable/template.yaml
ate create actor demo-agent --template pi-durable-data -a ate-demo-pi
ate create egress-policy demo-agent -a ate-demo-pi -f $DEMOS/pi-durable/egress-policy.yaml

curl -sS -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-pi/demo-agent" $ATE_URL/submit \
  -H "Content-Type: application/json" \
  -d '{"prompt": "Create app.js with add(a, b) and a test.js that verifies it."}'

ate suspend actor demo-agent -a ate-demo-pi
ate create tag checkpoint-v1 --actor demo-agent -a ate-demo-pi
ate create actor demo-agent-fork --template pi-durable-data --tag checkpoint-v1 -a ate-demo-pi
ate create egress-policy demo-agent-fork -a ate-demo-pi -f $DEMOS/pi-durable/egress-policy.yaml

curl -sS -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-pi/demo-agent" $ATE_URL/submit \
  -H "Content-Type: application/json" -d '{"prompt": "Add multiply(a, b) and structured logging to app.js."}'
curl -sS -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-pi/demo-agent-fork" $ATE_URL/submit \
  -H "Content-Type: application/json" -d '{"prompt": "Add divide(a, b) and edge-case tests to test.js."}'
```

The Gemini key in the test project is revoked, so no turn answered on this image (`runs/coding-agents-pi-durable-gemini.txt`). Every turn settled `unanswered` with `model_error` and Google's 401 reason `ACCESS_TOKEN_TYPE_UNSUPPORTED`. Google gives that reason for the revoked key and `API_KEY_INVALID` for the placeholder, so the proxy did replace the key. The other steps ran. The fork's history held the Turn 1 prompt followed by its own.

The full walkthrough ran on the Gateway variant, the image from `demo/pi-durable` with `demo/template-gateway.yaml` and `demo/egress-policy-gateway.yaml` (see [Use Vercel AI Gateway instead](#use-vercel-ai-gateway-instead)). That image has the same `server.ts` with the same port change, and `server-gateway.ts`, which changes the provider and the model. Results from `runs/coding-agents-pi-durable.txt`:

| Step | Result | Time at the client |
|---|---|---|
| Create atespace, template, actor, egress policy | ok | 1.0 to 1.1 s each |
| Turn 1 | `done`, wrote `app.js` and `test.js` | 8.7 s, resume 3.3 s (first sandbox on a new container) |
| Suspend | ok, 141,824-byte snapshot, 516 ms on the worker | 1.6 s |
| Tag, fork | ok | 1.1 and 1.2 s |
| Turn 2 on `demo-agent` | `done`, entry 18, history: Turn 1 prompt, multiply prompt | 11.4 s, resume 1.25 s |
| Turn 2 on `demo-agent-fork` | `done`, entry 18, history: Turn 1 prompt, divide prompt | 10.7 s, resume 1.34 s |

The demo's README does not exercise an interrupted turn, and this run did not either.

### libfx

[libfx](https://github.com/taeold/coding-agents-on-substrate/tree/main/libfx) uses a Vercel AI Gateway key with the model `google/gemini-3.8-flash`.

| File | Change |
|---|---|
| `demo/coding-agents/libfx/Dockerfile` | `FROM coding-agents/libfx`, the `ate` binary and the entrypoint, and the same `sed` on `/app/server.ts` as pi-durable. |
| `demo/coding-agents/libfx/template.yaml` | `command: [node, /app/server.ts]`, `PORT=8080`, and wakeup probe port 8080. |
| `egress-policy.yaml` | None. |
| Secret | The policy replaces the `authorization` header, and the demo stores `Bearer $AI_GATEWAY_API_KEY` in its secret. Store the same value in Secret Manager. |

```bash
printf 'Bearer %s' "$AI_GATEWAY_API_KEY" | gcloud secrets create ai-gateway-bearer --data-file=-
INSTANCE=ate-node-libfx IMAGE=us-central1-docker.pkg.dev/daniel-junk/ate-ish/coding-agents-libfx:latest \
  SECRET_URI=ate-secret://k8s.io/default/ate-demo-fx/llm-credentials/ai-gateway-api-key \
  ./deploy.sh play ai-gateway-bearer
export ATE_URL=$(INSTANCE=ate-node-libfx ./deploy.sh url play) PATH=$PWD/bin:$PATH

ate create atespace ate-demo-fx
ate create actor-template -f demo/coding-agents/libfx/template.yaml
ate create actor demo-agent --template fx-durable-data -a ate-demo-fx
ate create egress-policy demo-agent -a ate-demo-fx -f $DEMOS/libfx/egress-policy.yaml

curl -sS -N -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-fx/demo-agent" $ATE_URL/submit \
  -H "Content-Type: application/json" \
  -d '{"prompt": "Create app.js with add(a, b) and a test.js that verifies it."}'

ate suspend actor demo-agent -a ate-demo-fx
ate create tag checkpoint-v1 --actor demo-agent -a ate-demo-fx
ate create actor demo-agent-fork --template fx-durable-data --tag checkpoint-v1 -a ate-demo-fx
ate create egress-policy demo-agent-fork -a ate-demo-fx -f $DEMOS/libfx/egress-policy.yaml

curl -sS -N -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-fx/demo-agent" $ATE_URL/submit \
  -H "Content-Type: application/json" -d '{"prompt": "Add multiply(a, b) and structured logging to app.js."}'
curl -sS -N -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-fx/demo-agent-fork" $ATE_URL/submit \
  -H "Content-Type: application/json" -d '{"prompt": "Add divide(a, b) and edge-case tests to test.js."}'

curl -sS -N -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-fx/demo-agent-fork" "$ATE_URL/events?cursor=9"
```

Results from `runs/coding-agents-libfx.txt`:

| Step | Result | Time at the client |
|---|---|---|
| Create atespace, template, actor, egress policy | ok | 1.0 to 1.1 s each |
| Turn 1 | `turn_end` `end_turn` at cursor 39, wrote `app.js` and `test.js` and ran `node test.js` | 38.0 s, resume 1.06 s |
| Suspend | ok, 694,272-byte snapshot, 506 ms on the worker | 1.7 s |
| Tag, fork | ok | 1.0 and 1.2 s |
| Turn 2 on `demo-agent` | `end_turn`, cursors 41 to 120, added `multiply` and logging | 68.0 s, resume 0.80 s |
| Turn 2 on `demo-agent-fork` | `end_turn`, cursors 41 to 120, added `divide` and tests | 136.6 s, resume 0.85 s |
| `/events?cursor=9` on the fork | replayed cursors 10 to 120, Turn 1 and the fork's Turn 2, and stayed open | stopped by curl after 5 s |
| Suspend after `/events`, from `runs/coding-agents-libfx-suspend-after-events.txt` | fails with `actor did not exit 8s after SIGTERM`, and the actor stays running | 9.1 s |

Both branches continued from cursor 41 after the shared Turn 1. The model calls went through the egress proxy, because the sandbox holds only the placeholder key and the turns answered. A turn's time includes every model call and tool run in it, and the split was not measured. The demo's README does not exercise an interrupted turn, and this run did not either.

In the suspend check, a new actor forked from `checkpoint-v1` suspended in 1.5 s after a `/readyz` request. The same actor, woken again by `/events?cursor=0` that curl dropped after 5 s, did not exit within 8 s of `SIGTERM`, and `ate delete actor` failed the same way. The demo's `SIGTERM` handler closes the agent and then the HTTP server, and the dropped stream is likely still open at the actor (see [Limits](#limits)). Which of the two calls waits was not measured.

### cursor-agent-sdk

[cursor-agent-sdk](https://github.com/taeold/coding-agents-on-substrate/tree/main/cursor-agent-sdk) needs a Cursor API key. This project has none, so the walkthrough ran with the demo's placeholder key and stops at the first model call. Nothing past that point is tested on ate-ish.

| File | Change |
|---|---|
| `demo/coding-agents/cursor-agent-sdk/Dockerfile` | `FROM coding-agents/cursor-agent-sdk`, the `ate` binary and the entrypoint. |
| `demo/coding-agents/cursor-agent-sdk/template.yaml` | `command` and `args` from the image's `CMD` with `--port 8080`, and wakeup probe port 8080. |
| `egress-policy.yaml` | None. |
| Secret | None. The demo puts the real `CURSOR_API_KEY` in the template, because its egress policy injects nothing. Replace the placeholder `crsr_000...` in the template with your key. |

```bash
INSTANCE=ate-node-cursor-agent-sdk IMAGE=us-central1-docker.pkg.dev/daniel-junk/ate-ish/coding-agents-cursor-agent-sdk:latest \
  ./deploy.sh play
export ATE_URL=$(INSTANCE=ate-node-cursor-agent-sdk ./deploy.sh url play) PATH=$PWD/bin:$PATH

ate create atespace ate-demo-cursor
ate create actor-template -f demo/coding-agents/cursor-agent-sdk/template.yaml
ate create actor demo-agent --template cursor-agent -a ate-demo-cursor
ate create egress-policy demo-agent -a ate-demo-cursor -f $DEMOS/cursor-agent-sdk/egress-policy.yaml

curl -sS -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-cursor/demo-agent" $ATE_URL/v1/session \
  -H "Content-Type: application/json" \
  -d '{"message": "Create hello.js that prints hi, run it with node, and report the output."}'
curl -sS -N -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-cursor/demo-agent" \
  "$ATE_URL/v1/session/<sessionId>/stream?startIndex=0"

ate suspend actor demo-agent -a ate-demo-cursor
curl -sS -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-cursor/demo-agent" "$ATE_URL/v1/session/<sessionId>" \
  -H "Content-Type: application/json" \
  -d '{"continuationToken": "<token>", "message": "Change hello.js so it also prints the current year, run it again, and tell me what it printed."}'

ate suspend actor demo-agent -a ate-demo-cursor
ate create tag checkpoint-v1 --actor demo-agent -a ate-demo-cursor
ate create actor demo-agent-fork --template cursor-agent --tag checkpoint-v1 -a ate-demo-cursor
ate create egress-policy demo-agent-fork -a ate-demo-cursor -f $DEMOS/cursor-agent-sdk/egress-policy.yaml

curl -sS -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-cursor/demo-agent" "$ATE_URL/v1/session/<sessionId>" \
  -H "Content-Type: application/json" -d '{"continuationToken": "<token>", "message": "Add add(a, b), call it with 2 and 3"}'
curl -sS -H "Authorization: Bearer $TOKEN" -H "ate-target-actor: ate-demo-cursor/demo-agent-fork" "$ATE_URL/v1/session/<sessionId>" \
  -H "Content-Type: application/json" -d '{"continuationToken": "<token>", "message": "Add multiply(a, b), call it with 6 and 7"}'
```

Results from `runs/coding-agents-cursor-agent-sdk.txt`:

| Step | Result | Time at the client |
|---|---|---|
| Create atespace, template, actor, egress policy | ok | 1.0 to 1.1 s each |
| `POST /v1/session` | 200 with `sessionId` and `continuationToken` | 4.2 s, resume 3.1 s |
| Stream | `turn.started`, then `turn.failed` "Invalid User API Key" 0.23 s later | open until curl stopped it |
| Suspend, then Turn 2 with the token | 200 and a new token, then `turn.failed` "Invalid User API Key" | 1.7 s, then 4.2 s with resume 3.2 s |
| Suspend, tag, fork | ok, 207,872-byte snapshot | 1.6, 1.1 and 1.1 s |
| Turns 3 on `demo-agent` and `demo-agent-fork` | 200 each, then `turn.failed` "Invalid User API Key" | 4.0 and 4.3 s, resume 3.1 and 3.3 s |

The SDK's requests reached `api2.cursor.sh` and `api.cursor.com` through the egress proxy, according to the worker's `egress ... allowed=true` log lines. Each actor's event log has the same events 0 to 8, from Turns 1 and 2, and its own message at event 9. No turn ran a tool. The demo's interrupted-turn test needs a working key and did not run. A boot of this actor, from `sandbox run` to the wakeup probe, took 2.79 to 2.98 s (n=7), against 0.49 to 1.0 s for pi-durable and libfx (n=6), first boots on a new container left out. The cause was not measured.

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

Of these templates, only pi-durable has run on ate-ish. The other rows follow from the snapshot fidelity each template asks for in `substrate/demos`, because ate-ish delivers ROOTFS fidelity.

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

- **No ports below 1024.** In a sandbox, root holds `CAP_NET_BIND_SERVICE` and binding port 80 or 1023 still fails with `EACCES`. Ports 1024 and 8080 bind. gVisor's `/proc` has no `net/ipv4/ip_unprivileged_port_start` to lower (measured on 2026-10-29 with the probe port published as 80 and as 8080). An actor listens on port 1024 or above, and its wakeup probe names that port.
- **A request stays open after its client leaves.** A probe actor held a streaming response open. After curl dropped the connection, the actor saw no `close` event in 30 s. The request ended when the actor was deleted, about 80 s after curl left, and Cloud Run then logged `Truncated response body`. The cause is likely Cloud Run's front end not closing the request to the container, but that was not confirmed. An actor that waits for its open connections on `SIGTERM` then does not exit, as libfx does after `/events`.
- **No forced stop.** If an actor does not exit within 8 s of `SIGTERM`, suspend fails and the actor keeps running. `ate delete actor` suspends first, so it fails the same way, and the actor keeps its slot until the worker stops.
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
INSTANCE=ate-node-libfx ./deploy.sh down play    # and each demo instance, then its bucket gs://ate-node-libfx-$PROJECT
gcloud storage rm -r gs://ate-node-$PROJECT gs://ate-node-scale-$PROJECT
gcloud secrets delete gemini-api-key
```
