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
  `internal/egress`; records/leases/placement; the ingress bridge;
  `EnsureRunsc`; image export and USER resolution (`bundle.go`).
- `internal/records`, `internal/ingress`, `internal/store`,
  `internal/cli`, `internal/apierr`, `internal/envutil`: unchanged.
- `hack/fakeupstream`: a TLS stand-in for ai-gateway.vercel.sh; `-delay`
  holds every reply that long (G6). `demo/g5-nonroot`, `demo/g5-gosu`:
  G5's own images (USER node; gosu-to-nobody).

## Running a node and the demos (as exedev, no sudo)

```bash
CGO_ENABLED=0 go build -o bin/ate-ish ./cmd/ate-ish  # must be static
ATE_DIR=/tmp/ate-node ATE_STORE=dir:/tmp/ate-store ./bin/ate-ish &
export ATE_URL=http://127.0.0.1:8080
ate-ish create atespace ate-demo-counter
ate-ish create actor-template -f demo/counter/template.yaml
ate-ish create actor counter1 --template counter -a ate-demo-counter
curl -H "ate-target-actor: ate-demo-counter/counter1" $ATE_URL/
bash hack/gate.sh   # needs ATE_URL, gcloud ADC; --fake uses hack/fakeupstream
bash hack/smoke.sh  # one command, no network or Secret Manager needed
```

## Non-negotiable requirements

- **ate-ish must be a static binary** (`CGO_ENABLED=0`).
- **runsc does not auto-create a missing bind-mount destination**;
  `prepareMountpoints` must run after the overlay mount, before every
  `runsc run`/`restore`. Nothing this package owns may bind-mount
  under `/run/ate`; its own sockets live under `/run/ate-ish/*`.
- **Checkpoint needs SIGUSR1, restore needs SIGUSR2**, relayed to
  every egress forwarder (`fwdSet`) and, since this stage, the ingress
  outforwarder too (below).
- **Suspend must reset the checkpoint directory first**; the uploaded
  snapshot carries three directories: `checkpoint/`, `durable-dirs/`,
  `rootfs-upper/`.
- **Every template's container needs an `image`.**
- **A wildcard hostname resolves through the in-sandbox DNS responder
  on `127.0.53.53:53`**, not `/etc/hosts`: it asks the host's control
  socket to resolve each name on demand. An exact hostname still gets
  a static `/etc/hosts` entry. `tlsPassthrough`/`actorJwt` parse but
  do nothing; `openEgress` logs one line per rule using either.
- `runsc` and exported image rootfses share one directory,
  `ATE_CACHE_DIR` or `~/.cache/ate-ish`, not `~/.local/state/ate-ish`
  (root-owned on this VM by an earlier sudo'd run).
- `ATE_EGRESS_RESOLVE=host=ip[:port],...` and `ATE_EGRESS_TEST_CA=<pem>`
  are test-only Gateway overrides, read once at startup: the former
  dials a stand-in instead of a hostname's real address, the latter
  trusts its self-signed leaf, only for a named destination.
- **`Open` records its holder pid next to `--root`** and reaps a prior
  run's orphans first (`reapOrphans`): a SIGKILLed node's sandboxes are
  plain host processes, so `runsc --root=<dir> list` finds them with
  no nsenter, and each gets `delete -force` plus one log line.
- **ID mapping beyond root** (`idmap.go`): root only when newuidmap/
  newgidmap or a subuid/subgid range is missing (logged once, "non-root
  images will fail"); otherwise root plus that range, via newuidmap/
  newgidmap right after `unshare`. `ATE_SANDBOX_FORCE_ROOT_ONLY_MAPPING=1`
  forces the root-only path for testing.
- **The sandbox's own init (pid 1) always stays root**; only the
  actor's own child drops to the image's declared uid/gid
  (`ATE_ACTOR_UID`/`GID`, a `Credential` in `RunInit`).
- **`imageRootfsDir`'s export cache is keyed on the id-mapping mode**,
  not just the digest: root-only and multi-ID exports of one image are
  not interchangeable, so `extractCmd` runs tar inside the held
  namespace (so it can chown at all) and `tarExtractErr` downgrades a
  chown-only failure to a warning.
- **The ingress outforwarder's connection does not survive restore on
  its own** (a fresh bind mount on the other end); `RunInit` now kills
  and restarts it on SIGUSR2 for an instant redial, not 10+ s.

## Not done yet

- G1 to G7 ran (`hack/smoke.sh`, plus `hack/gate.sh` without `--fake`
  for G3): all pass. G8/G9 not run this stage.
- G6 found: checkpoint succeeds mid-flight, but the original request
  does not survive it (sandbox torn down post-checkpoint; 502
  "unexpected EOF"). Documented, not changed.
- README still describes the vendored worker; stage D's job.

## Where the detail lives

docs/PROGRESS-ROOTLESS.md: "Built"/"Deleted", "Gate results", stages B/C.
