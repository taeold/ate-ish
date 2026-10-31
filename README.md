# ate-ish

ate-ish is the easiest way to run [Agent Substrate](https://github.com/agent-substrate/substrate) actors: one binary, a directory or a bucket, your laptop, Docker, a VM, or Cloud Run — the same templates, egress policies, and CLI verbs as Agent Substrate. Write an actor (a Dockerfile, a `template.yaml`, and optionally an `egress-policy.yaml`) and run it locally in one command:

```bash
export AI_GATEWAY_API_KEY=...   # or a GEMINI_API_KEY, whatever your egress policy names

cd your-actor-directory   # holds Dockerfile, template.yaml, egress-policy.yaml
docker run --privileged -v "$PWD":/work -w /work -v /var/run/docker.sock:/var/run/docker.sock \
  -e AI_GATEWAY_API_KEY -p 8080:8080 \
  us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest \
  dev -f template.yaml -e egress-policy.yaml
```

or, with a local Go toolchain and no Docker:

```bash
export AI_GATEWAY_API_KEY=...   # or a GEMINI_API_KEY, whatever your egress policy names

cd your-actor-directory   # holds Dockerfile, template.yaml, egress-policy.yaml
sudo ate-ish dev -f template.yaml -e egress-policy.yaml
```

Run it from the actor's own directory (where its `Dockerfile` lives). `ate-ish dev` builds that Dockerfile, exports the resulting image's filesystem once as the actor's rootfs, fetches `runsc` if it is not already staged, starts one node with a local-directory store under `./.ate-ish/dev`, creates the atespace and one actor from the template, uploads the egress policy, and prints the `export ATE_URL=...` line and a ready-to-paste `curl` with the actor header. It keeps running in the foreground until Ctrl-C, which suspends the actor and exits cleanly. `--clean` wipes `./.ate-ish/dev` first; `--dir` and `--port` override the defaults.

Before you run it:

- A template that leaves `command` to the image's own `CMD`/`ENTRYPOINT` (as the upstream demos' templates do) gets it filled in automatically, read from the image `dev` just built; otherwise `ate-ish` does not read the image's configuration.
- An egress policy's `credentialUri` resolves two ways locally: `ate-secret://secretmanager.googleapis.com/<project>/<secret>`, read through `gcloud`'s Application Default Credentials if you have a GCP project set up, or `ate-secret://env/<VAR>`, read straight from an exported environment variable — no GCP project needed at all.
- `ate-ish dev` runs the vendored gVisor worker, which needs root (cgroup delegation, network namespaces, nftables): `sudo`, or `--privileged` through Docker.
- `ate-ish dev` also runs a plain `docker build` on the actor's own `Dockerfile` (no edits needed). The published image carries the `docker` CLI but no daemon of its own, so the Docker quickstart above mounts the host's own daemon socket (`-v /var/run/docker.sock:/var/run/docker.sock`); the actor rootfs export itself needs no daemon at all (`go-containerregistry`'s `crane`, used whenever `docker` is not reachable).
- The CLI's `get`/`suspend`/`create tag`/fork verbs (see below) work the same way against a local `ate-ish dev` node as against Cloud Run: `export ATE_URL=http://127.0.0.1:8080` and no `gcloud` login is required locally.

## Targets

1. **Laptop or dev box** (above): local directory store, the actor's rootfs exported from its own image. The primary target.
2. **A VM you own**: the same binary as a service (`ate-ish serve`), a GCS bucket store, two or three nodes over one bucket.
3. **Cloud Run gen2** (`ate-ish deploy`, below): one command, with its limits stated — in-memory rootfs copy at boot, no per-actor cgroup limits, single node.

Cloud Run is a target, not the design center. Its own findings are documented facts for the platform conversation, not constraints the rest of the design bends around.

## Deploy to Cloud Run

Write an Agent Substrate actor and deploy it to Cloud Run in one command:

```bash
go install github.com/taeold/ate-ish@latest   # or: download a release binary
printf %s "$AI_GATEWAY_API_KEY" | gcloud secrets create ai-gateway-token --data-file=-

cd your-actor-directory   # holds Dockerfile, template.yaml, egress-policy.yaml
ate-ish deploy -f template.yaml -e egress-policy.yaml
```

Run it from the actor's own directory (where its `Dockerfile` lives). `ate-ish deploy` builds that Dockerfile, adds `ate-ish` and its vendored gVisor worker on top, pushes the image to an Artifact Registry repo named `ate-ish` in your project (created if missing), creates the bucket `gs://ate-ish-$PROJECT` and grants it and every secret the egress policy's `credentialUri`s name to the default compute service account, creates or updates the Cloud Run gen2 instance (default name `ate-ish`), uploads the template and egress policy, creates one actor, and prints the URL, the `export` line, and a ready-to-paste `curl` with the actor header. It is idempotent: rerun it after a code change and it converges instead of failing on things that already exist. `ate-ish deploy --down` deletes the instance and keeps the bucket.

Before you run it:

- A template that leaves `command` to the image's own `CMD`/`ENTRYPOINT` (as the upstream demos' templates do) gets it filled in automatically, read from the image `deploy` just built; `ate-ish` does not otherwise read the image's configuration.
- Store the credential your egress policy's `credentialUri` names in Secret Manager, in the native `ate-secret://secretmanager.googleapis.com/<project>/<secret>` form. A policy using another form (such as a demo's `ate-secret://k8s.io/...`) gets a warning naming the `ATE_SECRET_MAP` line that would resolve it; rewriting the policy to the native form is simpler.
- Use a billed project, the Google Cloud CLI, Docker, and Go 1.27 or later (to build `ate-ish` itself, or to use `go install`). Cloud Run instances and sandboxes are in preview.
- `--project` and `--region` (default `us-west1`) override your gcloud project and region; `--name` (default `ate-ish`) sets the instance name, so more than one actor can run as separate instances.
- Your gcloud account needs permission to invoke the instance, such as the Cloud Run Invoker role.

## Manual deploy

The steps above as their own commands, `deploy.sh` and the CLI's own `create` verbs, useful when you want to see or change one step (a different bucket layout, an existing image, a template already uploaded):

```bash
./deploy.sh play gemini-api-key
```

Run it from a clone of this repository. The command creates the bucket `gs://ate-node-$PROJECT`, gives the default compute service account access to the bucket and to the secret, builds the `ate-ish` CLI into `bin/`, and creates the Cloud Run instance `ate-node` from the public image `us-central1-docker.pkg.dev/daniel-junk/ate-ish/pi-durable:latest`. It prints the `export` line for `ATE_URL` and `PATH`.

Before you run it:

- Store your Gemini API key in Secret Manager. The secret name is the argument to `deploy.sh`: `printf %s "$GEMINI_API_KEY" | gcloud secrets create gemini-api-key --data-file=-`.
- Use a billed project, the Google Cloud CLI with `gcloud beta run instances`, and Go 1.27 or later. Cloud Run instances and sandboxes are in preview.
- `deploy.sh` uses your gcloud project and the region us-west1. Set `PROJECT` and `REGION` to change them.
- Your gcloud account needs permission to invoke the instance, such as the Cloud Run Invoker role.

## The pi-durable walkthrough

This is the README of [pi-durable-substrate](https://github.com/taeold/pi-durable-substrate) with `kubectl ate X` replaced by `ate-ish X` and the kind cluster replaced by the instance from the quickstart. `./gate.sh` runs every step and fails if a turn or the egress check fails.

The outputs below are from `runs/play.txt`. That run used the Vercel AI Gateway variant (see [Use Vercel AI Gateway instead](#use-vercel-ai-gateway-instead)), because the Gemini key in the test project is revoked. The Gemini path has not run on ate-ish.

### How it works

`pi-durable` runs the coding agent loop and writes every conversation turn and tool checkpoint to a SQLite database at `/workspace/.pi/agent.sqlite`. ate-ish runs each agent as an actor in its own Cloud Run sandbox.

1. **Suspend and auto-resume.** `ate-ish suspend actor` sends `SIGTERM` to the agent, waits for it to exit, tars the sandbox's writable layer with `sandbox tar`, and uploads the tar to the bucket as the actor's snapshot. When the next request arrives for that actor, the ingress asks a worker with a free slot to resume it. The worker starts a sandbox with `--import-tar` and waits for the wakeup probe before the ingress forwards the request.
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
ate-ish create atespace ate-demo-pi
ate-ish create actor-template -f demo/template.yaml
ate-ish create actor demo-agent --template pi-durable-data -a ate-demo-pi
ate-ish create egress-policy demo-agent -a ate-demo-pi -f demo/egress-policy.yaml
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
ate-ish suspend actor demo-agent -a ate-demo-pi
ate-ish create tag checkpoint-v1 --actor demo-agent -a ate-demo-pi
ate-ish create actor demo-agent-fork --template pi-durable-data --tag checkpoint-v1 -a ate-demo-pi
ate-ish create egress-policy demo-agent-fork -a ate-demo-pi -f demo/egress-policy.yaml
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

The base image is Debian trixie-slim with `ca-certificates` and `/usr/local/bin/ate-ish`. Add the runtime your command needs. `demo/pi-durable/Dockerfile` copies `node` from `node:24-slim`.

ate-ish reads these fields of `template.yaml`: `command`, `args`, `env`, `wakeupProbe`, `volumeMounts` and `volumes` (`durableDir` and the `systemInfo` trust bundle). It stores the other fields and ignores them. The `image` field is accepted and ignored.

To start from an image you already have, add the binary and the entrypoint. The binary is static and carries its own root certificates. [Running the Agent Substrate demos](#running-the-agent-substrate-demos) does this for three images.

```Dockerfile
FROM <your image>
COPY --from=us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest /usr/local/bin/ate-ish /usr/local/bin/ate-ish
ENTRYPOINT ["/usr/local/bin/ate-ish", "serve"]
```

Deploy your image with `IMAGE=<your image> ./deploy.sh play <secret>`, then run `ate-ish create actor-template -f template.yaml`.

The images are public in Artifact Registry today. The intended distribution is GitHub Container Registry, with these commands:

```bash
docker build --platform linux/amd64 -t ghcr.io/taeold/ate-ish:latest .
docker build --platform linux/amd64 --build-arg BASE=ghcr.io/taeold/ate-ish:latest -t ghcr.io/taeold/ate-ish/pi-durable:latest demo/pi-durable
docker push ghcr.io/taeold/ate-ish:latest
docker push ghcr.io/taeold/ate-ish/pi-durable:latest
```

Cloud Run does not deploy from GHCR directly. Deploy from Artifact Registry, or from an Artifact Registry remote repository for `ghcr.io`.

## Running the Agent Substrate demos

[taeold/coding-agents-on-substrate](https://github.com/taeold/coding-agents-on-substrate) has three coding agents written for Agent Substrate on kind. v2 runs each one unchanged: its own `template.yaml` as written (port 80, no `PORT` env, no `sed`), listening on port 80 directly, because v2's worker runs the actor's command itself (no platform sandbox-launcher to route around). `demo/coding-agents/<demo>/` holds a Dockerfile and a template for each demo, built on `./deploy.sh images`'s base `ate-ish:latest` image (the binary plus its vendored `ateworker-gvisor`, `bin/runsc`, and the `kodata` runsc reads by relative path from `WORKDIR /ate-ish` — the final stage of each per-demo Dockerfile is `FROM $BASE`, not the demo's own node image, because v2 brings its own gVisor worker and that worker has to ship inside the actor image). Each demo's own `egress-policy.yaml` is used, with one change where the demo's own credential form (`ate-secret://k8s.io/...`) is not one ate-ish's egress proxy reads; see the per-demo notes below. `gate.sh`-style turns are run directly with `curl`, as these demos predate `gate.sh`.

Two deviations apply to every demo, forced by the mechanism, not by choice:

- **A `command` is added.** The demos' templates leave the command to the image's `CMD`; `ate-ish` does not read it (`records.go`'s `Template`/`Container` type has no image lookup).
- **The model key.** Gemini is revoked in this project. pi-durable runs the Vercel AI Gateway variant from `demo/pi-durable` (`server-gateway.ts`, `AI_GATEWAY_API_KEY`, `demo/egress-policy-gateway.yaml`) instead of its own `server.ts`/`GEMINI_API_KEY`. libfx already reads `AI_GATEWAY_API_KEY` itself; only its egress policy's `credentialUri` changes, to `ate-secret://secretmanager.googleapis.com/$PROJECT/ai-gateway-token` (the only form `egress.go` resolves) with a `Bearer ` prefix on the `authorization` header. cursor-agent-sdk needs no key change: it has none to give.

For a local run (one node, this VM, `sudo`), the actor's rootfs is the node's own filesystem (`bundle.go`'s overlay of `/`), so each demo's agent is installed directly on the node under `/actor-<demo>` (`npm ci --omit=dev` from the demo's own `package.json`/`package-lock.json`), and the template's `command` points there. For the two demos run on Cloud Run gen2 (pi-durable and libfx; cursor needs no key and ran locally only), the same agent is baked into a Docker image instead, and `command` points at the image's own path (`/app/server.ts`), with `./deploy.sh play` redeploying `ate-ish-v2` to that image one at a time.

### pi-durable

[pi-durable](https://github.com/taeold/coding-agents-on-substrate/tree/main/pi-durable) uses a Gemini key, revoked in this project; ran on the Gateway variant.

```bash
# Local: install the agent on the node, Gateway variant (same deps as pi-durable/agent).
sudo mkdir -p /actor-pi-durable && sudo chown "$(id -u):$(id -g)" /actor-pi-durable
cp demo/pi-durable/package.json demo/pi-durable/package-lock.json demo/pi-durable/server-gateway.ts /actor-pi-durable/
(cd /actor-pi-durable && npm ci --omit=dev)

ate-ish create atespace ate-demo-pi
ate-ish create actor-template -f /tmp/pi-durable-local.yaml   # upstream template.yaml, command: [node, /actor-pi-durable/server-gateway.ts]
ate-ish create actor demo-agent --template pi-durable-data -a ate-demo-pi
ate-ish create egress-policy demo-agent -a ate-demo-pi -f demo/egress-policy-gateway.yaml

curl -sS -X POST $ATE_URL/submit -H "ate-target-actor: ate-demo-pi/demo-agent" \
  -d '{"prompt": "Create app.js with add(a, b) and a test.js that verifies it."}'

ate-ish suspend actor demo-agent -a ate-demo-pi
ate-ish create tag checkpoint-v1 --actor demo-agent -a ate-demo-pi
ate-ish create actor demo-agent-fork --template pi-durable-data --tag checkpoint-v1 -a ate-demo-pi
ate-ish create egress-policy demo-agent-fork -a ate-demo-pi -f demo/egress-policy-gateway.yaml

curl -sS -X POST $ATE_URL/submit -H "ate-target-actor: ate-demo-pi/demo-agent" \
  -d '{"prompt": "Add multiply(a, b) and structured logging to app.js."}'
curl -sS -X POST $ATE_URL/submit -H "ate-target-actor: ate-demo-pi/demo-agent-fork" \
  -d '{"prompt": "Add divide(a, b) and edge-case tests to test.js."}'
```

For gen2, `demo/coding-agents/pi-durable/Dockerfile` builds the same `server-gateway.ts` into `/app` on top of `ate-ish:latest`, and `demo/coding-agents/pi-durable/template.yaml` sets `command: [node, /app/server.ts]`:

```bash
docker build --platform linux/amd64 -t $REPO/coding-agents-pi-durable demo/coding-agents/pi-durable
docker push $REPO/coding-agents-pi-durable
IMAGE=$REPO/coding-agents-pi-durable:latest ./deploy.sh play
# then the same create/submit/suspend/tag/fork sequence as above, against ATE_URL=$(./deploy.sh url play)
```

Both ran the full walkthrough: turn 1, suspend, tag, fork, turn 2 on both branches.

| Step | Local | gen2 |
|---|---|---|
| Turn 1, `done`, wrote `app.js` and `test.js` | 8.8 s | 6.6 s |
| Suspend, tag, fork | ok | ok, 1.4 s suspend |
| Turn 2 on `demo-agent` | `done` | 13.4 s | 7.2 s |
| Turn 2 on `demo-agent-fork` | `done` | 14.0 s | 11.5 s |

### libfx

[libfx](https://github.com/taeold/coding-agents-on-substrate/tree/main/libfx) uses a Vercel AI Gateway key with the model `google/gemini-3.8-flash`, unmodified.

```bash
# Local: install the agent on the node, unmodified.
sudo mkdir -p /actor-libfx && sudo chown "$(id -u):$(id -g)" /actor-libfx
cp /path/to/coding-agents-on-substrate/libfx/agent/{package.json,package-lock.json,server.ts,tools.ts} /actor-libfx/
(cd /actor-libfx && npm ci --omit=dev)

ate-ish create atespace ate-demo-fx
ate-ish create actor-template -f /tmp/libfx-local.yaml   # upstream template.yaml, command: [node, /actor-libfx/server.ts]
ate-ish create actor demo-agent --template fx-durable-data -a ate-demo-fx
ate-ish create egress-policy demo-agent -a ate-demo-fx -f demo/coding-agents/libfx/egress-policy.yaml

curl -sS -N -X POST $ATE_URL/submit -H "ate-target-actor: ate-demo-fx/demo-agent" \
  -d '{"prompt": "Create app.js with add(a, b) and a test.js that verifies it."}'
```

For gen2, `demo/coding-agents/libfx/Dockerfile` and `demo/coding-agents/libfx/template.yaml` follow the same `FROM $BASE` / `command: [node, /app/server.ts]` pattern as pi-durable's.

**Turn 1 completed on both hosts, model-bound (about 26 s locally).** The egress proxy allowed `ai-gateway.vercel.sh` and replaced the `authorization` header, and the ndjson stream showed every step of the turn, including `node test.js` and `node --test` passing. An earlier version of this section said the demo wedged after a dropped stream; that was a curl timeout during a long turn. The one real hang, seen only on ate-ish v1 on Cloud Run, is in the demo's own `server.ts`: its SIGTERM handler waits in `server.close()` while an `/events` stream is open, and `server.closeAllConnections()` after `server.close()` fixes it. v2 suspends through a filesystem checkpoint and never sends the actor SIGTERM for that.

### cursor-agent-sdk

[cursor-agent-sdk](https://github.com/taeold/coding-agents-on-substrate/tree/main/cursor-agent-sdk) needs a Cursor API key. This project has none, so the walkthrough ran with the demo's placeholder key and stops at the first model call, as written upstream (no port change either: its template already used port 80).

```bash
sudo mkdir -p /actor-cursor-agent-sdk && sudo chown "$(id -u):$(id -g)" /actor-cursor-agent-sdk
cp -r /path/to/coding-agents-on-substrate/cursor-agent-sdk/agent/{package.json,package-lock.json,bot} /actor-cursor-agent-sdk/
(cd /actor-cursor-agent-sdk && npm ci --omit=dev)

ate-ish create atespace ate-demo-cursor
ate-ish create actor-template -f /tmp/cursor-local.yaml   # upstream template.yaml, command pointed at /actor-cursor-agent-sdk/node_modules/.bin/agent-sdk
ate-ish create actor demo-agent --template cursor-agent -a ate-demo-cursor
ate-ish create egress-policy demo-agent -a ate-demo-cursor -f /path/to/coding-agents-on-substrate/cursor-agent-sdk/egress-policy.yaml

curl -sS -X POST $ATE_URL/v1/session -H "ate-target-actor: ate-demo-cursor/demo-agent" \
  -d '{"message": "Create hello.js that prints hi, run it with node, and report the output."}'
curl -sS -N $ATE_URL/v1/session/<sessionId>/stream?startIndex=0 -H "ate-target-actor: ate-demo-cursor/demo-agent"
```

`POST /v1/session` returned 200 with a `sessionId` in 5.8 s. The stream showed `turn.started`, then `turn.failed` with `"Invalid User API Key"` 1.27 s later — the documented stopping point with no key, same message as the demo's own README predicts for a bad key. Nothing past that point ran.

## What Agent Substrate needs and what ate-ish needs

| Agent Substrate | ate-ish |
|---|---|
| A Kubernetes cluster, kind or GKE, with the `ate-system` components | One Cloud Run instance, or a Cloud Run worker pool and a service |
| ate-api-server with Postgres for actor records | JSON records in one Cloud Storage bucket, written with `ifGenerationMatch` |
| A `WorkerPool` of gVisor or microVM pods, run by atecontroller | Cloud Run sandboxes, started by the `ate-ish` worker |
| atelet and ateworker for snapshots, and a golden snapshot per template | `sandbox tar` of the writable layer, uploaded to the bucket |
| atenet-router, an Envoy ext_proc, for `ate-target-actor` | The `ate-ish` ingress, a Go reverse proxy |
| atenet-egress and a credential provider | One HTTPS proxy per actor in the worker, with secrets from Secret Manager |
| `kubectl-ate` over gRPC | `ate-ish` over HTTP, with a gcloud identity token |
| A template image per actor template, pulled by the worker | The actor in the node image |

## Agent Substrate's demo templates

v2 runs substrate's own gVisor worker, so MEMORY fidelity (a runsc checkpoint, not just the durable dirs) works, and the `counter` demo's old failure mode is fixed. Results below are from real runs this round, not from the snapshot-fidelity table v1 shipped here.

| Demo | Fidelity asked | What ran | Changed from the demo as written | Result | Numbers |
|---|---|---|---|---|---|
| pi-durable (coding-agents-on-substrate) | VOLUMES (default) | Full README walkthrough: turn 1, suspend, tag, fork, turn 2 on both branches; locally and on gen2 | Gemini key revoked: ran the Gateway variant (`server-gateway.ts`, `AI_GATEWAY_API_KEY`) from `demo/pi-durable` instead of `server.ts`/`GEMINI_API_KEY`. `command` added (not in the upstream template). | Pass, locally and on gen2 | local: turn 1 8.8 s, turn 2 13.4/14.0 s. gen2: turn 1 6.6 s, turn 2 7.2/11.5 s |
| libfx (coding-agents-on-substrate) | VOLUMES (default) | Turn 1, locally and on gen2 | None to the agent. Egress policy's `credentialUri` rewritten from the demo's `ate-secret://k8s.io/...` form (unsupported) to ate-ish's `ate-secret://secretmanager.googleapis.com/...` form, with a `Bearer ` prefix added. `command` added. | Streams and completes turns normally under ate-ish v2 (a client drop reaches the actor in about 10 ms; v2 suspends through a filesystem checkpoint with no SIGTERM). The earlier "stuck after a dropped stream" result was a curl timeout during a long turn, not a wedge. The one real hang was on ate-ish v1 on Cloud Run and is the demo's own SIGTERM handler waiting in `server.close()` while an `/events` stream is open; `server.closeAllConnections()` after `server.close()` fixes it (see the libfx note in docs/PROGRESS.md). | turn 1 completes (26 s locally, model-bound) |
| cursor-agent-sdk (coding-agents-on-substrate) | VOLUMES (default) | Session create and stream, no key, locally only (no key to try on gen2) | None; ran as written, including port 80 | Pass, to the documented stopping point: session created, then `turn.failed` `"Invalid User API Key"` | 5.8 s create, 1.27 s to the failure |
| substrate `counter` (`/tmp/substrate/demos/counter`) | MEMORY | Built from source, run locally, two suspend/resume cycles | Template's nested `snapshotConfig.preferredFidelity` (a field ate-ish ignores) replaced with a top-level `preferredFidelity: MEMORY`; `--extra-port`/`--tcp-port` args dropped (not needed for this check) | **Pass — the fix for v1's known failure.** The file counter, the in-memory counter, and the rootfs counter all survived two suspend/resume cycles, staying in lockstep | 3 → suspend/resume → 4 → suspend/resume → 5, all three counters equal at each read |

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
| `ate-ish` CLI call, `gcloud auth print-identity-token` included | 0.95 to 1.18 s, suspend 1.61 and 1.62 s | 22 |
| Turn at the client, resume included (claude-haiku-4.5 through the egress proxy) | 2.97 to 14.6 s | 10 |

Parking, from `runs/park.txt`. p1 to p8 at once filled the 8 slots, and all answered 200 in 1.6 to 4.4 s. p9 waited 3.9 s for a slot, which opened when p1 was suspended 2 s into the wait, and answered 200 at 5.2 s. p10 waited 5 s, no slot opened, and it got `503 no worker has room for the actor` at 5.5 s.

What bounds them. Booting is Node starting pi-durable inside gVisor, and four boots on two vCPUs take about three times as long as one. The first boot on a new container is about 2 s slower than later ones. The likely cause is Cloud Run loading the image's files on first read, but that was not measured. Suspend and restore are mostly GCS round trips to a bucket in the same region. A request to a running actor pays two GCS reads at the ingress, for the actor record and for the owner's lease. The CLI starts a gcloud process for its token on every call. In the parking run, p2 and p7 reached the ingress 2.7 s after the other six, according to the Cloud Run request log, and the ingress placed them in 1.5 s.

## Limits

- **Port 80 works now.** v2 runs substrate's own gVisor worker directly, not a platform sandbox-launcher, so an actor can bind port 80 as root inside the sandbox. The three coding-agents demos (stage 6) all run their upstream templates unchanged on port 80, no `PORT` env, no `sed`.
- **A request stays open after its client leaves, and libfx's turns do not close their stream.** Confirmed again this round, locally and on Cloud Run gen2: a libfx turn's model and tool calls complete (seen in the ndjson itself) but the HTTP response never signals end of stream, so a client times out instead of seeing the turn finish. `ate-ish suspend actor` still works on the wedged actor (the lock is in the response, not the actor's own suspend path), but a client cannot tell a turn is done without a separate poll. See [Running the Agent Substrate demos](#running-the-agent-substrate-demos).
- **No forced stop.** If an actor does not exit within 8 s of `SIGTERM`, suspend fails and the actor keeps running. `ate-ish delete actor` suspends first, so it fails the same way, and the actor keeps its slot until the worker stops.
- **VOLUMES fidelity by default; MEMORY works when a template asks for it.** A template's `preferredFidelity: MEMORY` (top level; a nested `snapshotConfig.preferredFidelity` is ignored) gets a real runsc checkpoint of the sandbox's process memory, not just the durable dirs. Confirmed this round: substrate's own `counter` demo's in-memory and rootfs counters both survive suspend/resume under MEMORY, the exact case that failed on v1.
- **The workspace is RAM on the worker.** The launcher keeps the writable layer in memory (`--overlay2=root:memory`). A 20 MB workspace uses 20 MB of worker memory, and the snapshot tar is in the worker's in-memory `/tmp` during a suspend.
- **Egress is enforced by the actor's own network, not convention.** v2's worker puts every actor TCP connection through nftables to atunnel regardless of any proxy environment variable; a process that ignores `HTTPS_PROXY`/`NODE_USE_ENV_PROXY` reaches an unlisted host no more than one that respects it (the opposite of v1's `--allow-egress` convention). `gate.sh` asserts this with and without the proxy env var set.
- **The proxy knows an actor by its listener port.** Another actor on the same worker can use a neighbor's port and its credentials.
- **One ingress instance.** The scale shape runs the ingress with `--max-instances 1`, so concurrent placements of one actor share one process.
- **One template per deployment.** Every actor runs from the node image. Two templates can share a deployment only if the image holds both, as `demo/pi-durable` does for its two servers.
- **A worker has 10 s to drain when Cloud Run replaces it.** On `SIGTERM` the worker suspends every actor and deletes its lease. An actor whose suspend does not finish in 10 s comes back from its last snapshot.
- **No per-sandbox CPU or memory limit.** The launcher sets none. `resources` in the template is stored and ignored, and a worker counts slots (`ATE_CAPACITY`, 4).
- **On Cloud Run gen2, actors get no per-actor cgroup resource limits at all, by platform design, not by choice.** gen2 is a hybrid cgroup host: it exposes only the `hugetlb` controller to the container's own cgroup v2 root; `memory` and `cpu` are bound to cgroup v1 hierarchies this process cannot reach (measured: a 5-second, 20-attempt retry never saw them appear). ate-ish detects this at node start and runs the worker's own `runsc` with `--ignore-cgroups` plus a one-line patch (`third_party/substrate/VENDOR.md`'s "Patch 1") that skips assigning a per-container cgroup path; actors run with no cpu/memory/pids accounting or limit there, and usage stats report `source: unspecified` instead of real cgroup-backed numbers. On a host that does expose the controllers (this VM, GKE nodes), nothing changes. Cloud Run gen2's own "/" is also already overlayfs, so ate-ish copies the node filesystem once per instance onto an in-memory volume (`--add-volume=type=in-memory`, since `ephemeral-disk` is not supported for Cloud Run Instances) and uses that copy as every actor's rootfs lowerdir; measured 578-588 MB for the `pi-durable` image, comfortably inside a 2Gi volume, taking about 45-50 s once per instance start.
- **Update a stopped instance after you start it.** On a stopped instance, `instances update --image` changes the spec but does not import the image, and `instances start` runs the previous image (measured once).
- **`ate-ish dev`'s Docker path needs a docker daemon reachable from inside the container to build the actor's own Dockerfile.** The published `ate-ish` image has no docker daemon inside it (by design: it exports an already-built image's rootfs with `crane`, which needs none), so `docker run --privileged ... dev` can only run an actor whose template already names a pullable image; it cannot build a fresh Dockerfile the way `sudo ate-ish dev` can. See `docs/PROGRESS.md`'s stage 8 notes.

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
