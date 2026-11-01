# State

Current facts about the `rootless` branch; see docs/PROGRESS-ROOTLESS.md
for how any of this was found.

## Packages

- `cmd/ate-ish`: the one binary. No args runs a node; other args run
  the kubectl-ate CLI; `__sandbox_init__`/`__sandbox_forwarder__`/
  `__sandbox_outforwarder__` dispatch to `internal/sandbox` first.
- `internal/sandbox`: the held user+mount+network namespace
  (`namespace.go`, `idmap.go`), OCI bundle construction and the overlay
  rootfs mount, runsc run/checkpoint/restore/delete/kill, the
  systemd-scope limit wrapper, the in-sandbox init/forwarders/DNS
  responder (`init.go`). Nothing outside it calls runsc directly.
- `internal/egress`: the per-destination-socket gateway, header
  replacement, secret resolution, `ATE_EGRESS_RESOLVE`/
  `ATE_EGRESS_TEST_CA` test-only dial overrides.
- `internal/atelet`: the `Worker` driving `internal/sandbox` and
  `internal/egress`; records/leases/placement; the ingress bridge
  (`ingressBridge`, `worker.go`); image export and USER resolution
  (`bundle.go`).
- `internal/records`, `internal/ingress`, `internal/store`,
  `internal/cli`, `internal/apierr`, `internal/envutil`: unchanged.

## Running a node and the demos (as exedev, no sudo)

```bash
CGO_ENABLED=0 go build -o bin/ate-ish ./cmd/ate-ish  # must be static
ATE_DIR=/tmp/ate-node ATE_STORE=dir:/tmp/ate-store ./bin/ate-ish &  # listens 127.0.0.1:8080
export ATE_URL=http://127.0.0.1:8080
ate-ish create atespace ate-demo-counter
ate-ish create actor-template -f demo/counter/template.yaml
ate-ish create actor counter1 --template counter -a ate-demo-counter
curl -H "ate-target-actor: ate-demo-counter/counter1" $ATE_URL/
bash hack/gate.sh   # ATE_URL defaults to :8080; starts its own node (inheriting AI_GATEWAY_API_KEY) if none answers there
bash hack/smoke.sh  # one command, no network or Secret Manager needed
```

## Non-negotiable requirements

- **ate-ish must be a static binary** (`CGO_ENABLED=0`); `prepareMountpoints`
  must run after the overlay mount, before every `runsc run`/`restore`
  (runsc won't auto-create a missing bind-mount destination).
- **Checkpoint needs SIGUSR1, restore needs SIGUSR2**, relayed to every
  egress forwarder (`fwdSet`) and the ingress outforwarder too. Suspend
  must reset the checkpoint directory first; the uploaded snapshot
  carries `checkpoint/`, `durable-dirs/`, `rootfs-upper/`.
- **Every template's container needs an `image`.** A wildcard hostname
  resolves through the in-sandbox DNS responder on `127.0.53.53:53`,
  not `/etc/hosts`. `tlsPassthrough`/`actorJwt` parse but do nothing.
- `runsc` and exported image rootfses share one directory, `ATE_CACHE_DIR`
  or `~/.cache/ate-ish`, not `~/.local/state/ate-ish`. `ATE_EGRESS_RESOLVE=
  host=ip[:port],...`/`ATE_EGRESS_TEST_CA=<pem>`: test-only overrides.
- **The held namespace's holder is `sh -c 'read _ <&3'` on a pipe this
  process keeps the write end of**, not `sleep infinity` (which outlived
  its node under SIGINT and SIGKILL alike — 107 orphaned on this VM —
  since Close killed the `unshare` wrapper, not it). Losing that fd for
  any reason, including the kernel's own SIGKILL cleanup, ends the
  holder's read and so the holder, pid 1 of its own namespace; Close
  also closes it and kills it explicitly. `Open` still reaps a prior
  run's orphaned *sandboxes* via `reapOrphans`.
- **ID mapping beyond root** (`idmap.go`): root only when newuidmap/
  newgidmap or a subuid/subgid range is missing, else root plus that
  range via newuidmap/newgidmap after `unshare` (`ATE_SANDBOX_FORCE_ROOT_ONLY_MAPPING=1`
  forces root-only). `imageRootfsDir`'s export cache keys on the mode too.
- **The ingress outforwarder's connection does not survive restore on
  its own**; `RunInit` kills and restarts it on SIGUSR2 for an instant
  redial. Restore resumes the *previous* generation's workers before
  SIGUSR2 lands, so one can dial in and get pooled by `ingressBridge`
  first; `Restarted` (right after `Restore`, before SIGUSR2) tags
  pooled connections with a generation and drops whatever is queued.
  `RoundTrip` retries only a write-side EPIPE/ECONNRESET, never after a
  response started reading (a broader retry turned one counter request
  into two, live: counter 3 to 4). A straggler whose write lands but
  whose read then hits "unexpected EOF" still fails outright; not
  fixed, since retrying there isn't provably safe.
- **`ATE_LISTEN` is a full `host:port`** (default `127.0.0.1:8080`, no
  auth); `advertiseAddr` picks a lease address: `ATE_ADVERTISE` if set,
  else `ATE_LISTEN` when not a wildcard, else the first non-loopback IPv4.
  The Dockerfile installs `uidmap`, drops `nsenter` (not a separate package).

## Not done yet

- G1 to G9 all pass (docs/PROGRESS-ROOTLESS.md, stage D). G6: checkpoint
  succeeds mid-flight, but the original request doesn't survive it
  (502 "unexpected EOF"). Documented, not changed.
- `demo/pi-durable/egress-policy.yaml` (the non-gateway variant) names
  a Secret Manager secret that does not exist (404); README uses the
  gateway variant instead. Not fixed this stage.
- `hack/smoke.sh`'s counter-resume request can still fail right after a
  restore (the residual ingress-pool race above): 1 failure in 5 runs
  in this stage's final check, never a double-execution. Every
  assertion now prints what it got and the node log's last 40 lines
  on failure (also `/tmp/ate-ish-smoke-last.log`).

## Where the detail lives

docs/PROGRESS-ROOTLESS.md: "Built"/"Deleted", "Gate results", stages B-D.
