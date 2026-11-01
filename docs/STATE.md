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
  `hack/fakeupstream`: a TLS stand-in for ai-gateway.vercel.sh (`-delay`
  holds every reply, for G6); `demo/g5-nonroot`/`demo/g5-gosu`: G5's
  own images.

## Running a node and the demos (as exedev, no sudo)

```bash
CGO_ENABLED=0 go build -o bin/ate-ish ./cmd/ate-ish  # must be static
ATE_DIR=/tmp/ate-node ATE_STORE=dir:/tmp/ate-store ./bin/ate-ish &  # listens 127.0.0.1:8080
export ATE_URL=http://127.0.0.1:8080
ate-ish create atespace ate-demo-counter
ate-ish create actor-template -f demo/counter/template.yaml
ate-ish create actor counter1 --template counter -a ate-demo-counter
curl -H "ate-target-actor: ate-demo-counter/counter1" $ATE_URL/
bash hack/gate.sh   # needs ATE_URL, gcloud ADC; --fake uses hack/fakeupstream
bash hack/smoke.sh  # one command, no network or Secret Manager needed
```

## Non-negotiable requirements

- **ate-ish must be a static binary** (`CGO_ENABLED=0`). runsc does
  not auto-create a missing bind-mount destination; `prepareMountpoints`
  must run after the overlay mount, before every `runsc run`/`restore`.
  Nothing this package owns may bind-mount under `/run/ate`; its own
  sockets live under `/run/ate-ish/*`.
- **Checkpoint needs SIGUSR1, restore needs SIGUSR2**, relayed to
  every egress forwarder (`fwdSet`) and the ingress outforwarder too.
- **Suspend must reset the checkpoint directory first**; the uploaded
  snapshot carries three directories: `checkpoint/`, `durable-dirs/`,
  `rootfs-upper/`.
- **Every template's container needs an `image`.** A wildcard hostname
  resolves through the in-sandbox DNS responder on `127.0.53.53:53`,
  not `/etc/hosts`; an exact hostname still gets a static one.
  `tlsPassthrough`/`actorJwt` parse but do nothing; `openEgress` logs
  one line per rule using either.
- `runsc` and exported image rootfses share one directory,
  `ATE_CACHE_DIR` or `~/.cache/ate-ish`, not `~/.local/state/ate-ish`.
- `ATE_EGRESS_RESOLVE=host=ip[:port],...` and `ATE_EGRESS_TEST_CA=<pem>`
  are test-only Gateway overrides: the former dials a stand-in instead
  of a hostname's real address, the latter trusts its self-signed leaf.
- **`Open` records its holder pid next to `--root`** and reaps a prior
  run's orphans first (`reapOrphans`): `runsc --root=<dir> list` finds
  a SIGKILLed node's leftover sandboxes with no nsenter, and each gets
  `delete -force` plus one log line.
- **ID mapping beyond root** (`idmap.go`): root only when newuidmap/
  newgidmap or a subuid/subgid range is missing (logged once, "non-root
  images will fail"); otherwise root plus that range, via newuidmap/
  newgidmap right after `unshare`. `ATE_SANDBOX_FORCE_ROOT_ONLY_MAPPING=1`
  forces the root-only path for testing. The sandbox's own init (pid 1)
  always stays root; only the actor's own child drops to the image's
  declared uid/gid. `imageRootfsDir`'s export cache keys on the
  id-mapping mode, not just the digest: root-only and multi-ID exports
  of one image are not interchangeable.
- **The ingress outforwarder's connection does not survive restore on
  its own**; `RunInit` kills and restarts it on SIGUSR2 for an instant
  redial, not 10+ s.
- **`ATE_LISTEN` is a full `host:port`** (default `127.0.0.1:8080`, no
  auth). `advertiseAddr` (`cmd/ate-ish/main.go`) picks a lease address:
  `ATE_ADVERTISE` if set, else `ATE_LISTEN` when not a wildcard, else
  the machine's first non-loopback IPv4 with the port.
- The node image's Dockerfile installs `uidmap` and drops `nsenter`
  (not a separate Debian trixie package).

## Not done yet

- G1 to G9 all pass: `hack/smoke.sh`/`hack/gate.sh` (no `--fake`) cover
  G1-G7, G8/G9 run by hand (docs/PROGRESS-ROOTLESS.md, stage D).
- G6 found: checkpoint succeeds mid-flight, but the original request
  does not survive it (502 "unexpected EOF"). Documented, not changed.
- `demo/pi-durable/egress-policy.yaml` (the non-gateway variant) names
  a Secret Manager secret that does not exist (404); README uses the
  gateway variant instead. Not fixed this stage.

## Where the detail lives

docs/PROGRESS-ROOTLESS.md: "Built"/"Deleted", "Gate results", stages B/C/D.
