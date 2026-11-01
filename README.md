# ate-ish

ate-ish runs [Agent Substrate](https://github.com/agent-substrate/substrate) actors on your own machines without Kubernetes: one binary, a directory or a bucket for state, and no root.

## How it works

ate-ish is one binary: a node, and a `kubectl-ate`-style CLI against it. The node runs as both an ingress (routes a request by its `ate-target-actor` header, serves the admin API) and a worker (runs actors), or either role alone. All control-plane state, an atespace, its templates, its actors' records, their snapshots and tags, and worker leases, lives as JSON and tar objects in one store: a local directory by default, or one GCS bucket when `ATE_BUCKET` names one. There is no database. Every record write is conditional on the generation it read, so two nodes sharing a store never clobber each other's writes.

Each actor runs in its own gVisor sandbox, under a user, mount and network namespace the node holds for itself with no root. The sandbox gets no network of its own: every outbound connection it opens goes through the node's egress gateway over a per-destination Unix socket, which enforces the actor's egress policy and can replace a header (such as an API key) before forwarding. `ate-ish suspend actor` checkpoints the sandbox and uploads the result to the store as the actor's snapshot; the next request for that actor resumes it on a worker with a free slot. At `preferredFidelity: MEMORY`, the snapshot includes the sandbox's process memory, not just its durable directories, so an in-process counter survives suspend and resume. A tag is a copy of a suspended actor's snapshot, and an actor created with `--tag` starts as a fork of it, sharing history up to that point and diverging from there.

## Install

```bash
go install github.com/taeold/ate-ish/cmd/ate-ish@latest
```

The node fetches gVisor's `runsc` itself on first start, into `$XDG_STATE_HOME/ate-ish/runsc` or `~/.local/state/ate-ish/runsc`; `ATE_RUNSC_DIR` overrides that location.

## Run it

As yourself, no sudo, from a clone of this repository (the demos live in `demo/`):

```bash
git clone https://github.com/taeold/ate-ish && cd ate-ish
docker build -t counter-demo:latest demo/counter

ate-ish &

export ATE_URL=http://127.0.0.1:8080
ate-ish create atespace ate-demo-counter
ate-ish create actor-template -f demo/counter/template.yaml
ate-ish create actor counter1 --template counter -a ate-demo-counter
curl -H "ate-target-actor: ate-demo-counter/counter1" $ATE_URL/
```

Each request bumps the counter's in-process, file, and rootfs-overlay counts; `ate-ish suspend actor counter1 -a ate-demo-counter` checkpoints it, and the next request resumes it where it left off.

pi-durable, a coding-agent actor. Its model key comes from the node's environment, so export it before you start the node:

```bash
export AI_GATEWAY_API_KEY=...   # then restart ate-ish so it sees the key
ate-ish create atespace ate-demo-pi
ate-ish create actor-template -f demo/pi-durable/template-gateway.yaml
ate-ish create actor demo-agent --template pi-durable-data -a ate-demo-pi
ate-ish create egress-policy demo-agent -a ate-demo-pi -f demo/pi-durable/egress-policy-gateway.yaml

curl -X POST $ATE_URL/submit -H "ate-target-actor: ate-demo-pi/demo-agent" \
  -H "Content-Type: application/json" \
  -d '{"prompt": "Create app.js with add(a, b) and a test.js that verifies it."}'
```

The actor holds a placeholder key; the node's gateway replaces it with the real one on the way out, so the actor's own process never sees it. A policy's `credentialUri` reads the key from the node's environment (`ate-secret://env/<VAR>`) or from Secret Manager (`ate-secret://secretmanager.googleapis.com/<project>/<secret>`). `demo/pi-durable/template.yaml` and `egress-policy.yaml` are the same actor against Gemini with `GEMINI_API_KEY`. `ate-ish suspend actor demo-agent -a ate-demo-pi`, `ate-ish create tag`, `ate-ish get actors|tags|workers`, and creating a second actor with `--tag` run the same way against the same node; `hack/gate.sh` runs the whole sequence end to end, including a fork and an egress check.

## Requirements

Linux, with unprivileged user namespaces enabled. Ubuntu 23.10 and later restrict them by default through AppArmor, and the node's error says so. The fix takes sudo once: give ate-ish its own copy of `unshare` and allow user namespaces for that copy only, so the restriction stays in place for every other program.

```bash
sudo install -D -m 0755 /usr/bin/unshare /usr/local/libexec/ate-ish/unshare
sudo tee /etc/apparmor.d/ate-ish-unshare >/dev/null <<'EOF'
abi <abi/4.0>,
include <tunables/global>
profile ate-ish-unshare /usr/local/libexec/ate-ish/unshare flags=(unconfined) {
  userns,
}
EOF
sudo apparmor_parser -r /etc/apparmor.d/ate-ish-unshare
```

The node uses that copy when it exists; `ATE_UNSHARE` names another path.

An actor image whose Dockerfile declares a non-root `USER` needs `newuidmap`/`newgidmap` (the `uidmap` package) and an `/etc/subuid`/`/etc/subgid` range for your user; without them, the node maps only root and logs that non-root images will fail. Per-actor memory/CPU/task limits need a systemd user session (`systemd-run --user --scope`); without one, the node runs actors without limits and logs that once. macOS has no unprivileged Linux namespaces at all, so it runs the node image in a privileged container instead (`docker build -t ate-ish .`, then `docker run --privileged -p 8080:8080 -e ATE_LISTEN=0.0.0.0:8080 ate-ish`). The node inside the container pulls actor images from a registry, since your local Docker images are not visible to it, so push the actor image somewhere the container can reach.

## Operate a fleet

The node listens on `127.0.0.1:8080` by default; it has no authentication, so keep that port on a private network if you widen it. `ATE_LISTEN` overrides the listen address, for example `ATE_LISTEN=0.0.0.0:8080` to serve a fleet. `ATE_ADVERTISE` sets the address this node puts in its lease for other nodes to forward to; without it, the node advertises its own listen address when that names a specific host, or its machine's first non-loopback IPv4 address when the listen address is a wildcard. Point several nodes at the same bucket (`ATE_BUCKET=<name>` on each) and they form a fleet: any node accepts ingress for any actor, and placement goes through the same conditional writes a single node uses. A worker's lease in the bucket names its advertised address and capacity; the ingress on whichever node a request reaches picks a worker with a free slot (or the actor's current owner, if it still holds the lease) and routes there. There is no membership protocol beyond the leases themselves.

## Compatibility

| | |
|---|---|
| Start as an ordinary user; a clear error when user namespaces are refused | works |
| Snapshots at `MEMORY` fidelity: process memory, files and the writable layer survive suspend, resume and a node restart | works |
| Suspend, tag, fork, and divergent turns after the fork | works |
| `http` rules, `https` on any port, wildcard hostnames (`*.example.com` and bare `*`) | works |
| An unlisted hostname under a specific policy | refused |
| Actor images with a non-root `USER` | works with `uidmap` and a subuid range |
| `tls_passthrough` rules and `actorJwt` | not supported; the node logs one line per rule that uses them |
| A request in flight when its actor is suspended | not supported; the request ends in an error |
| The first request after a restore | takes 1 to 3 seconds |

An actor has no network of its own, only loopback; every outbound connection goes through the gateway, and a program that bypasses the system resolver, for example by connecting to a raw IP address, has no route at all.

## Limits

ate-ish does not promise substrate's control-plane scale, its Postgres-backed consistency, or its per-actor edge identity to external systems. Its scale target is one machine to a few, not millions of actors.

## Contributing and License

Everything in this repository is Apache-2.0 (`LICENSE`). Contributions are welcome as pull requests; keep a change small enough to review against the files it touches, and run `hack/smoke.sh` (and `hack/gate.sh` against a real model, where the change touches the worker, the ingress, or egress) before sending one.
