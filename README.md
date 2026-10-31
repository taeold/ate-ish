# ate-ish

ate-ish runs [Agent Substrate](https://github.com/agent-substrate/substrate) actors without a Kubernetes cluster. One binary runs on a directory or a bucket, on a Linux box, in Docker, on a VM, or on Cloud Run, using the same templates, egress policies, and actor contract as Agent Substrate.

## How it works

ate-ish is one binary with one daemon and a `kubectl-ate`-style CLI. The daemon runs as a node: an ingress that routes requests by the `ate-target-actor` header and serves the admin API, a worker that runs actors, or both (the default). All control-plane state, an atespace, its templates, its actors' records, their snapshots and tags, and worker leases, lives as JSON and tar objects in one store: a local directory by default, or one GCS bucket when `ATE_BUCKET` names one. There is no database. Every record write is conditional on the generation it read, so two nodes sharing a store never clobber each other's writes.

Each actor runs under Agent Substrate's own `ateworker-gvisor`, vendored unmodified except one documented patch (`third_party/substrate/VENDOR.md`). The worker starts the actor's command inside a gVisor sandbox, with the sandbox's own network, filesystem overlay, and checkpoint/restore. `ate-ish suspend actor` asks the worker to checkpoint the sandbox and uploads the result to the store as the actor's snapshot; the next request for that actor resumes it on a worker with a free slot. A tag is a copy of a suspended actor's snapshot, and an actor created with `--tag` starts as a fork of it, sharing history up to that point and diverging from there.

## What runs on it

An actor is a `template.yaml` plus, optionally, an `egress-policy.yaml`, the same files Agent Substrate itself reads. ate-ish honors `containers[0].image` (the actor's rootfs, pulled from a local Docker daemon when one is reachable, otherwise from the registry the template names), `command`, `args`, `env`, `wakeupProbe`, `volumeMounts`, `volumes` (`durableDir` and the `systemInfo` trust bundle), and the top-level `preferredFidelity`. It stores every other field and ignores it. `preferredFidelity: MEMORY` gets a real checkpoint of the sandbox's process memory on suspend, not just its durable directories; `ROOTFS` adds the sandbox's writable layer; the default, `VOLUMES`, snapshots the durable directories only.

An egress policy names the hosts an actor may reach and the credential each gets, resolved from Secret Manager (`ate-secret://secretmanager.googleapis.com/<project>/<secret>`) or a plain environment variable (`ate-secret://env/<VAR>`). The worker enforces it at the sandbox's own network: every actor TCP connection is routed through the policy regardless of any proxy environment variable the actor's own process sets or ignores. Requests to it carry the `ate-target-actor: <atespace>/<actor>` header, and the actor itself can bind port 80.

## Install

```bash
go install github.com/taeold/ate-ish/cmd/ate-ish@latest
```

No release binary is built yet; `go install` or building from source are the only paths today. The node image builds with a plain `docker build .` from the repo root; it carries the `ate-ish` binary, the vendored `ateworker-gvisor`, and a staged `runsc` release, with no Docker CLI or socket of its own.

## Run it

On Linux, run a node as root (the worker needs cgroup delegation, network namespaces, and nftables), keeping your environment so the node can use your gcloud credentials for Secret Manager, and drive it with the CLI in another shell:

```bash
sudo -E ate-ish &

export ATE_URL=http://localhost:8080
ate-ish create atespace ate-demo-pi
ate-ish create actor-template -f demo/pi-durable/template.yaml
ate-ish create actor demo-agent --template pi-durable-data -a ate-demo-pi
ate-ish create egress-policy demo-agent -a ate-demo-pi -f demo/pi-durable/egress-policy.yaml

curl -X POST $ATE_URL/submit -H "ate-target-actor: ate-demo-pi/demo-agent" \
  -H "Content-Type: application/json" \
  -d '{"prompt": "Create app.js with add(a, b) and a test.js that verifies it."}'
```

Export `GEMINI_API_KEY` before starting the node, or point the policy's `credentialUri` at `ate-secret://env/<VAR>`. `demo/pi-durable` also ships a gateway variant (`server-gateway.ts`, `template-gateway.yaml`, `egress-policy-gateway.yaml`) that uses `AI_GATEWAY_API_KEY` and `ai-gateway.vercel.sh`; `hack/gate.sh` uses that one. `ate-ish suspend actor demo-agent -a ate-demo-pi`, `ate-ish create tag`, `ate-ish get actors|tags|workers`, and creating a second actor with `--tag` run the same way against the same node; `hack/gate.sh` runs the whole sequence, including a fork and an egress check, end to end.

On macOS, run the node image in a privileged container instead, with no Docker socket mounted (the node image needs none: it exports an already-pulled image's filesystem, not a local build):

```bash
docker build -t ate-ish .
docker run --privileged -p 8080:8080 ate-ish
```

The node inside the container pulls actor images from a registry, not from the host's Docker daemon: a local Docker daemon is not visible from inside the container, so the actor image must be pushed somewhere the container can reach.

The CLI's verbs work the same way against either node: `--endpoint` (default `http://localhost:8080`) or `$ATE_URL` points them at it, and no `gcloud` login is needed locally.

## Operate a fleet

Point several nodes at the same bucket (`ATE_BUCKET=<name>` on each) and they form a fleet: any node accepts ingress for any actor, and placement goes through the same conditional writes a single node uses. A worker's lease in the bucket names its address and capacity; the ingress on whichever node a request reaches picks a worker with a free slot (or the actor's current owner, if it still holds the lease) and routes there. There is no membership protocol beyond the leases themselves.

## Cloud Run

```bash
gcloud beta run instances create ate-ish \
  --image=<your image> \
  --cpu=2 --memory=4Gi \
  --port=8080 \
  --add-volume=name=state,type=in-memory,size-limit=2Gi \
  --add-volume-mount=volume=state,mount-path=/ate-ish-state \
  --set-env-vars=ATE_DIR=/ate-ish-state,ATE_BUCKET=<your bucket>
```

What differs on Cloud Run gen2:

- Cloud Run gen2's own root filesystem is already an overlay, so ate-ish copies the node's filesystem once per instance onto a real (non-overlay) volume and uses that copy as every actor's rootfs base, adding memory and startup time at every instance start; see `docs/measurements.md`. This is why the command above gives it an in-memory volume to land in.
- gen2 exposes only the `hugetlb` cgroup controller to the container; `memory` and `cpu` are bound to cgroup v1 hierarchies the container cannot reach. ate-ish detects this and runs the worker with `--ignore-cgroups` plus a one-line patch to substrate's own code (`third_party/substrate/VENDOR.md`'s Patch 1), so actors on gen2 get no cpu, memory, or pids accounting or limit at all.
- An instance's first boot can fail while the node sets up the worker's cgroup and be restarted by Cloud Run's restart policy; the cause is a boot-time race in how the node detects the controllers it has, tracked in `docs/PROGRESS.md`.
- One instance is one node. A fleet on Cloud Run is more instances, each with the same `--set-env-vars=ATE_BUCKET=<same bucket>`.
- Credentials for egress policies come from Secret Manager through the instance's service account; grant it `secretAccessor` on each secret the policy names.
- gen2 gives the container root and all of the worker's capability set except `MKNOD`. Cloud Run does not document this, so it can change.

## Compatibility

| Demo | Runs here | Why |
|---|---|---|
| `demo/pi-durable` | Yes | ate-ish's own quickstart actor; ships both a plain-Gemini and an AI-Gateway server variant, picked by the template. |
| `demo/counter` | Yes, at `preferredFidelity: MEMORY` | Needs a real checkpoint of process memory to keep its in-process counter across suspend/resume; the default `VOLUMES` fidelity would only keep its file counter. |
| coding-agents-on-substrate's `pi-durable` port | Yes | Runs as written; the actor's own model key is the user's to supply. |
| coding-agents-on-substrate's `libfx` port | Yes | Runs as written. |
| coding-agents-on-substrate's `cursor-agent-sdk` port | Up to the first model call | Needs a Cursor API key; none was supplied in the runs behind this table. |

See `docs/CODING-AGENTS-PORTS.md` for the ported demos and `docs/demos.md` for full walkthroughs.

## Limits

ate-ish does not promise substrate's control-plane scale, its Postgres-backed consistency, or its per-actor edge identity to external systems. Its scale target is one machine to a few, not millions of actors. A template's `resources` field is accepted and stored but never enforced; a worker counts actors against a flat slot count (`ATE_CAPACITY`, default 4), not against a cgroup limit, on any host.

Running the worker needs Linux, root (or the specific capability set substrate's worker pool declares: `NET_ADMIN`, `SYS_ADMIN`, `SYS_CHROOT`, `SYS_PTRACE`, `NET_RAW`, `DAC_OVERRIDE`, `FOWNER`, `CHOWN`, `MKNOD`), nftables in the kernel, and `runsc` on the systrap platform, which needs no KVM. `ATE_ISH_NO_CGROUP_PATH=1` skips assigning a per-container cgroup path for hosts whose cgroup v2 root exposes none of the controllers runsc wants; ate-ish sets it automatically where it detects that case. `ATE_ROOTFS_EXPORT=crane` forces the no-daemon image export path even when a local Docker daemon is reachable.

## Contributing and License

Everything in this repository is Apache-2.0 (`LICENSE`), except `third_party/substrate`, which carries Agent Substrate's own upstream Apache-2.0 license unmodified; see `third_party/substrate/VENDOR.md` and its own `LICENSE` file for the vendored commit and the one patch applied on top of it. Contributions are welcome as pull requests; keep a change small enough to review against the files it touches, and run `hack/gate.sh` against a local node before sending one that touches the worker, the ingress, or egress.
