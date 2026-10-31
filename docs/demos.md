# Demo walkthroughs and journeys

A log of demo and deploy walkthroughs from earlier stages of the project,
including some that used commands (`ate-ish dev`, `ate-ish deploy`,
`deploy.sh`) that no longer exist in the current command surface. Kept
here for narrative detail the README cannot carry; not a source of truth
for today's commands or flags. See `internal/cli/cli.go` and the README
for what actually runs today.

## Targets (v1/v2-era)

1. **Laptop or dev box**: local directory store, the actor's rootfs exported from its own image. The primary target.
2. **A VM you own**: the same binary as a service (`ate-ish serve`), a GCS bucket store, two or three nodes over one bucket.
3. **Cloud Run gen2** (`ate-ish deploy`): one command, with its limits stated — in-memory rootfs copy at boot, no per-actor cgroup limits, single node.

Cloud Run was treated as a target, not the design center. Its own findings are documented facts for the platform conversation, not constraints the rest of the design bent around.

## Deploy to Cloud Run (via the now-removed `ate-ish deploy`)

The one-command Cloud Run deploy flow, from when `deploy` was a subcommand:

```bash
go install github.com/taeold/ate-ish@latest   # or: download a release binary
printf %s "$AI_GATEWAY_API_KEY" | gcloud secrets create ai-gateway-token --data-file=-

cd your-actor-directory   # holds Dockerfile, template.yaml, egress-policy.yaml
ate-ish deploy -f template.yaml -e egress-policy.yaml
```

Run from the actor's own directory (where its `Dockerfile` lives). `ate-ish deploy` built that Dockerfile, added `ate-ish` and its vendored gVisor worker on top, pushed the image to an Artifact Registry repo named `ate-ish` in the project (created if missing), created the bucket `gs://ate-ish-$PROJECT` and granted it and every secret the egress policy's `credentialUri`s name to the default compute service account, created or updated the Cloud Run gen2 instance (default name `ate-ish`), uploaded the template and egress policy, created one actor, and printed the URL, the `export` line, and a ready-to-paste `curl` with the actor header. It was idempotent: rerunning it after a code change converged instead of failing on things that already existed. `ate-ish deploy --down` deleted the instance and kept the bucket.

Notes from that era:

- A template that leaves `command` to the image's own `CMD`/`ENTRYPOINT` (as the upstream demos' templates do) got it filled in automatically, read from the image `deploy` just built; `ate-ish` did not otherwise read the image's configuration.
- The credential named by an egress policy's `credentialUri` had to live in Secret Manager, in the native `ate-secret://secretmanager.googleapis.com/<project>/<secret>` form, or as a plain environment variable with `ate-secret://env/<VAR>`. No other form resolved.
- This needed a billed project, the Google Cloud CLI, Docker, and Go 1.27 or later (to build `ate-ish` itself, or to use `go install`). Cloud Run instances and sandboxes were in preview.
- `--project` and `--region` (default `us-west1`) overrode the gcloud project and region; `--name` (default `ate-ish`) set the instance name, so more than one actor could run as separate instances.
- The gcloud account needed permission to invoke the instance, such as the Cloud Run Invoker role.

## Manual deploy (via the now-removed `deploy.sh`)

The steps above as their own commands, `deploy.sh` and the CLI's own `create` verbs, useful for seeing or changing one step (a different bucket layout, an existing image, a template already uploaded):

```bash
./deploy.sh play gemini-api-key
```

Run from a clone of the repository. The command created the bucket `gs://ate-node-$PROJECT`, gave the default compute service account access to the bucket and to the secret, built the `ate-ish` CLI into `bin/`, and created the Cloud Run instance `ate-node` from a public pi-durable image. It printed the `export` line for `ATE_URL` and `PATH`.

Notes from that era:

- The Gemini API key had to live in Secret Manager under the secret name passed to `deploy.sh`: `printf %s "$GEMINI_API_KEY" | gcloud secrets create gemini-api-key --data-file=-`.
- This needed a billed project, the Google Cloud CLI with `gcloud beta run instances`, and Go 1.27 or later. Cloud Run instances and sandboxes were in preview.
- `deploy.sh` used the gcloud project and the region us-west1 by default; `PROJECT` and `REGION` changed them.
- The gcloud account needed permission to invoke the instance, such as the Cloud Run Invoker role.

## The pi-durable walkthrough

This followed the README of [pi-durable-substrate](https://github.com/taeold/pi-durable-substrate) with `kubectl ate X` replaced by `ate-ish X` and the kind cluster replaced by the Cloud Run instance from the quickstart of that era. `./gate.sh` runs every step and fails if a turn or the egress check fails; it is still the current gate script (see `hack/gate.sh`), even though the walkthrough below used the older per-step CLI calls directly.

The outputs below are from `docs/log/play.txt`. That run used the Vercel AI Gateway variant (see "Use Vercel AI Gateway instead" below), because the Gemini key in the test project was revoked. The Gemini path never ran on ate-ish.

### How it works

`pi-durable` runs the coding agent loop and writes every conversation turn and tool checkpoint to a SQLite database at `/workspace/.pi/agent.sqlite`. ate-ish ran each agent as an actor in its own Cloud Run sandbox.

1. **Suspend and auto-resume.** `ate-ish suspend actor` sends `SIGTERM` to the agent, waits for it to exit, tars the sandbox's writable layer with `sandbox tar`, and uploads the tar to the bucket as the actor's snapshot. When the next request arrives for that actor, the ingress asks a worker with a free slot to resume it. The worker starts a sandbox with `--import-tar` and waits for the wakeup probe before the ingress forwards the request.
2. **Checkpoint and fork.** A tag is a copy of a suspended actor's snapshot. An actor created with `--tag` starts from a copy of the tag. Both actors start from the same history and files, then diverge.
3. **Egress credential injection.** The sandbox runs with a placeholder `GEMINI_API_KEY`. Each actor's `HTTPS_PROXY` points at its own proxy in the worker. For `generativelanguage.googleapis.com`, the proxy replaces `x-goog-api-key` with the secret, read from Secret Manager (or an environment variable) by the egress policy's `credentialUri`, so the key never enters the sandbox or its snapshots.

One binary had three roles then, set by `ATE_ROLE` (still true today: `all`, `ingress`, `worker`). The `worker` role runs actors. The `ingress` role routes requests by the `ate-target-actor` header and serves the admin API that the CLI calls. The `all` role is both, and the quickstart uses it. All state lived in the bucket, and every record write was conditional on the generation read (`ifGenerationMatch`):

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

`demo/egress-policy.yaml` was the file from pi-durable-substrate. `demo/template.yaml` was that repository's template with two changes. The command was `node /actor/server.ts`, because the agent was baked into the image at `/actor`. The agent listened on `PORT=8080` and the probe used port 8080, because the Cloud Run sandbox of that era could not bind a port below 1024 (today's worker has no such restriction; see the README's "What runs on it"). The `image` field named the public image and was ignored at the time (today it is read and used; see the README).

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

The second line was a limit of that era's proxy-convention enforcement (see docs/measurements.md). Today's worker enforces egress at the sandbox's network regardless of proxy environment variables (see the README). The image has no `curl`, so the check uses Node's `fetch`.

### Use Vercel AI Gateway instead

`demo/pi-durable/server-gateway.ts` is the same agent with `vercelAIGatewayProvider()` and the model `anthropic/claude-haiku-4.5`. The pi-durable image holds both servers, and the template picks one.

1. Store the AI Gateway key in Secret Manager, for example as `ai-gateway-token`.
2. Run `./deploy.sh play ai-gateway-token`.
3. In `demo/egress-policy-gateway.yaml`, replace the project and secret with your own.
4. Use `demo/template-gateway.yaml` and `demo/egress-policy-gateway.yaml` in steps 1 and 3, or run `TEMPLATE=demo/template-gateway.yaml POLICY=demo/egress-policy-gateway.yaml ./gate.sh`.

The gateway policy replaces `x-api-key` on `ai-gateway.vercel.sh`, which is the header pi-ai sends.

## Build your own actor (v1/v2-era instructions)

One deployment ran one actor template, and the actor was baked into the node image. A sandbox's root was the node container's filesystem, read only, under a writable overlay. The pattern was to build an image from the base image and copy the actor to `/actor`:

```Dockerfile
FROM <base image>
COPY . /actor
```

The base image was Debian trixie-slim with `ca-certificates` and `/usr/local/bin/ate-ish`. ate-ish read these fields of `template.yaml`: `command`, `args`, `env`, `wakeupProbe`, `volumeMounts` and `volumes` (`durableDir` and the `systemInfo` trust bundle); other fields were stored and ignored. The `image` field was accepted and ignored at the time.

To start from an image already built, the pattern added the binary and an entrypoint:

```Dockerfile
FROM <your image>
COPY --from=<base image> /usr/local/bin/ate-ish /usr/local/bin/ate-ish
ENTRYPOINT ["/usr/local/bin/ate-ish", "serve"]
```

Deploying that image used `IMAGE=<your image> ./deploy.sh play <secret>`, then `ate-ish create actor-template -f template.yaml`.

Distribution at the time used these commands:

```bash
docker build --platform linux/amd64 -t ghcr.io/taeold/ate-ish:latest .
docker build --platform linux/amd64 --build-arg BASE=ghcr.io/taeold/ate-ish:latest -t ghcr.io/taeold/ate-ish/pi-durable:latest demo/pi-durable
docker push ghcr.io/taeold/ate-ish:latest
docker push ghcr.io/taeold/ate-ish/pi-durable:latest
```

Cloud Run does not deploy from GHCR directly. Deploying from Artifact Registry, or from an Artifact Registry remote repository for `ghcr.io`, worked around that.

## Running the Agent Substrate demos

[taeold/coding-agents-on-substrate](https://github.com/taeold/coding-agents-on-substrate) has three coding agents written for Agent Substrate on kind. The v2 worker ran each one unchanged: its own `template.yaml` as written (port 80, no `PORT` env, no `sed`), listening on port 80 directly, because v2's worker runs the actor's command itself with no platform sandbox-launcher to route around. `demo/coding-agents/<demo>/` held a Dockerfile and a template for each demo, built on a base `ate-ish:latest` image (the binary plus its vendored `ateworker-gvisor`, `bin/runsc`, and the `kodata` runsc reads by relative path). Each demo's own `egress-policy.yaml` was used, with one change where the demo's own credential form (`ate-secret://k8s.io/...`) was not one ate-ish's egress proxy read.

Two deviations applied to every demo, forced by the mechanism, not by choice:

- **A `command` is added.** The demos' templates leave the command to the image's `CMD`; ate-ish's template record has no image lookup, so it needs an explicit command.
- **The model key.** Gemini was revoked in the test project. pi-durable ran the Vercel AI Gateway variant from `demo/pi-durable` (`server-gateway.ts`, `AI_GATEWAY_API_KEY`) instead of its own `server.ts`/`GEMINI_API_KEY`. libfx already read `AI_GATEWAY_API_KEY` itself; only its egress policy's `credentialUri` changed, to ate-ish's `ate-secret://secretmanager.googleapis.com/...` form with a `Bearer ` prefix on the `authorization` header. cursor-agent-sdk needed no key change: it had none to give.

For a local run, the actor's rootfs was the node's own filesystem, so each demo's agent was installed directly on the node under `/actor-<demo>` (`npm ci --omit=dev`), and the template's `command` pointed there. For the two demos run on Cloud Run gen2 (pi-durable and libfx; cursor needed no key and ran locally only), the same agent was baked into a Docker image instead, with `command` pointing at the image's own path.

### pi-durable

```bash
# Local: install the agent on the node, Gateway variant.
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

For gen2, a per-demo Dockerfile built the same `server-gateway.ts` into `/app` on top of `ate-ish:latest`, and the per-demo template set `command: [node, /app/server.ts]`. Both local and gen2 ran the full walkthrough: turn 1, suspend, tag, fork, turn 2 on both branches.

| Step | Local | gen2 |
|---|---|---|
| Turn 1, `done`, wrote `app.js` and `test.js` | 8.8 s | 6.6 s |
| Suspend, tag, fork | ok | ok, 1.4 s suspend |
| Turn 2 on `demo-agent`, `done` | 13.4 s | 7.2 s |
| Turn 2 on `demo-agent-fork`, `done` | 14.0 s | 11.5 s |

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

Turn 1 completed on both hosts, model-bound (about 26 s locally). The egress proxy allowed `ai-gateway.vercel.sh` and replaced the `authorization` header, and the ndjson stream showed every step of the turn, including `node test.js` and `node --test` passing. An earlier run reported the demo wedged after a dropped stream; that turned out to be a curl timeout during a long turn, not a wedge. The one real hang, seen only on ate-ish v1 on Cloud Run, was in the demo's own `server.ts`: its SIGTERM handler waits in `server.close()` while an `/events` stream is open, and `server.closeAllConnections()` after `server.close()` fixes it. v2 suspends through a filesystem checkpoint and never sends the actor SIGTERM for that.

### cursor-agent-sdk

[cursor-agent-sdk](https://github.com/taeold/coding-agents-on-substrate/tree/main/cursor-agent-sdk) needs a Cursor API key. The test project had none, so the walkthrough ran with the demo's placeholder key and stopped at the first model call, as written upstream (no port change either: its template already used port 80).

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

`POST /v1/session` returned 200 with a `sessionId` in 5.8 s. The stream showed `turn.started`, then `turn.failed` with `"Invalid User API Key"` 1.27 s later, the documented stopping point with no key, matching the demo's own README prediction for a bad key. Nothing past that point ran.

This same set of three demos was later re-run against the current `ate-ish` (no deploy.sh, no dev subcommand) directly from their own upstream clones; see `docs/CODING-AGENTS-PORTS.md` for that result.

## What Agent Substrate needs and what ate-ish needed (v1/v2-era)

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

This table describes the v1/v2 Cloud Run sandbox design. Today's ate-ish runs substrate's own vendored gVisor worker directly (see `third_party/substrate/VENDOR.md`), so most of the right column above is now substrate's own worker, not a Cloud Run sandbox equivalent.

## Agent Substrate's demo templates, results from the v2 round

The v2 worker runs substrate's own gVisor worker, so MEMORY fidelity (a runsc checkpoint, not just the durable dirs) works, fixing a failure mode an earlier round had with the `counter` demo.

| Demo | Fidelity asked | What ran | Changed from the demo as written | Result | Numbers |
|---|---|---|---|---|---|
| pi-durable (coding-agents-on-substrate) | VOLUMES (default) | Full walkthrough: turn 1, suspend, tag, fork, turn 2 on both branches; locally and on gen2 | Gemini key revoked: ran the Gateway variant (`server-gateway.ts`, `AI_GATEWAY_API_KEY`) instead of `server.ts`/`GEMINI_API_KEY`. `command` added (not in the upstream template). | Pass, locally and on gen2 | local: turn 1 8.8 s, turn 2 13.4/14.0 s. gen2: turn 1 6.6 s, turn 2 7.2/11.5 s |
| libfx (coding-agents-on-substrate) | VOLUMES (default) | Turn 1, locally and on gen2 | None to the agent. Egress policy's `credentialUri` rewritten from the demo's `ate-secret://k8s.io/...` form (unsupported) to ate-ish's `ate-secret://secretmanager.googleapis.com/...` form, with a `Bearer ` prefix added. `command` added. | Streams and completes turns normally (a client drop reaches the actor in about 10 ms; v2 suspends through a filesystem checkpoint with no SIGTERM). An earlier "stuck after a dropped stream" result was a curl timeout during a long turn, not a wedge. The one real hang was on ate-ish v1 on Cloud Run and is the demo's own SIGTERM handler waiting in `server.close()` while an `/events` stream is open. | turn 1 completes (26 s locally, model-bound) |
| cursor-agent-sdk (coding-agents-on-substrate) | VOLUMES (default) | Session create and stream, no key, locally only (no key to try on gen2) | None; ran as written, including port 80 | Pass, to the documented stopping point: session created, then `turn.failed` `"Invalid User API Key"` | 5.8 s create, 1.27 s to the failure |
| substrate `counter` | MEMORY | Built from source, run locally, two suspend/resume cycles | Template's nested `snapshotConfig.preferredFidelity` (a field ate-ish ignores) replaced with a top-level `preferredFidelity: MEMORY` | Pass, the fix for an earlier known failure. The file counter, the in-memory counter, and the rootfs counter all survived two suspend/resume cycles, staying in lockstep | 3 → suspend/resume → 4 → suspend/resume → 5, all three counters equal at each read |

## Same-host two-node recipe

Two nodes on one machine, sharing one bucket, each reachable on its own
port. This is what a fix-pass verification run needed to exercise the
gen2 cgroup-helper path and the credential-resolution fallback side by
side with an ordinary node, without a second machine. It is distinct from
"Operate a fleet" in the README, which is about separate machines; two
nodes sharing one host need the extra steps below because
`ateworker-gvisor`'s own control ports and the vendored worker's fixed
`/var/lib/ate` path are not node-aware by themselves.

```bash
# Node A: the ordinary single-node shape, nothing extra.
sudo -E ATE_BUCKET=<bucket> ATE_DIR=/var/lib/ate-ish-a ATE_LISTEN=8080 \
  ATE_CAPACITY=4 ate-ish &

# Node B: a disjoint port triple for ateworker-gvisor's own control
# ports (they bind on the host network namespace, unlike the actor
# sandboxes, so node A's and node B's would otherwise collide), and a
# private /var/lib/ate so the vendored worker's fixed
# WorkerSupportSocket path (and everything else nodepath.BasePath
# roots there) does not collide with node A's. `unshare --mount
# --propagation private` first, so the bind mount inside it is
# invisible to node A and the host.
sudo -E unshare --mount --propagation private -- bash -c '
  mkdir -p /var/lib/ate /var/lib/ate-b
  mount --bind /var/lib/ate-b /var/lib/ate
  export ATE_BUCKET=<bucket> ATE_DIR=/var/lib/ate-ish-b ATE_LISTEN=8081 \
    ATE_CAPACITY=4
  export ATE_READY_PORT=29080 ATE_ATUNNEL_PORT=29443 ATE_CONNECT_PORT=29444
  exec ate-ish
'
```

Either node's `--endpoint`/`ATE_URL` accepts every verb for every actor on
the shared bucket; a request that names an actor another node currently
hosts gets proxied there. `ate-ish get workers` from either node lists
both.

## Cleanup (v1/v2-era commands)

```bash
./deploy.sh down play
./deploy.sh down scale    # only if you deployed it
INSTANCE=ate-node-libfx ./deploy.sh down play    # and each demo instance, then its bucket
gcloud storage rm -r gs://ate-node-$PROJECT gs://ate-node-scale-$PROJECT
gcloud secrets delete gemini-api-key
```
