# State

Current facts about the `rootless` branch; see docs/PROGRESS-ROOTLESS.md
for how any of this was found.

## Packages

- `cmd/ate-ish`: the one binary. No args runs a node; other args run
  the kubectl-ate CLI; `__sandbox_init__`/`__sandbox_forwarder__`/
  `__sandbox_outforwarder__` dispatch to `internal/sandbox` first
  (what runs inside a sandbox, never typed by a user).
- `internal/sandbox`: the held user+mount+network namespace, OCI
  bundle construction and the overlay rootfs mount, runsc run/
  checkpoint/restore/delete/kill, the systemd-scope limit wrapper, the
  in-sandbox init/forwarders/DNS responder. Nothing outside it calls
  runsc directly.
- `internal/egress`: the per-destination-socket gateway (one unix
  socket per actor+hostname, plus one control socket per actor with a
  wildcard rule), header replacement, Secret Manager/env secret
  resolution, and the `ATE_EGRESS_RESOLVE`/`ATE_EGRESS_TEST_CA`
  test-only dial overrides.
- `internal/atelet`: the `Worker` driving `internal/sandbox` and
  `internal/egress` per actor; records/leases/placement; the node-side
  ingress bridge (`ingressBridge`); `EnsureRunsc`; the shared
  `cacheDir()` (runsc release and exported image rootfses).
- `internal/records`: `EgressPolicy` now holds `http`/`https`/
  `tlsPassthrough`/`actorJwt` rules, each with a port and a wildcard-
  capable hostname list (`IsWildcard`/`MatchesWildcard`).
- `internal/ingress`, `internal/store`, `internal/cli`,
  `internal/apierr`, `internal/envutil`: unchanged.
- `hack/fakeupstream`: a TLS stand-in for ai-gateway.vercel.sh (or any
  other https destination via `-hostname`), for `hack/smoke.sh`.

## Running a node and the demos (as exedev, no sudo)

```bash
CGO_ENABLED=0 go build -o bin/ate-ish ./cmd/ate-ish  # must be static, see below
ATE_DIR=/tmp/ate-node ATE_STORE=dir:/tmp/ate-store ./bin/ate-ish &
export ATE_URL=http://127.0.0.1:8080
# counter demo (demo/counter, image counter-demo:latest):
ate-ish create atespace ate-demo-counter
ate-ish create actor-template -f demo/counter/template.yaml
ate-ish create actor counter1 --template counter -a ate-demo-counter
curl -H "ate-target-actor: ate-demo-counter/counter1" $ATE_URL/
# pi-durable gateway demo, needs gcloud ADC for Secret Manager:
bash hack/gate.sh  # ATE_URL must already be exported; --fake points at hack/fakeupstream
# one command, no network, no Secret Manager, builds its own node:
bash hack/smoke.sh
```

## Non-negotiable requirements

- **ate-ish must be a static binary** (`bundle.go`'s `copyExecutable`
  refuses a dynamic one, naming `CGO_ENABLED=0` as the fix).
- **runsc does not auto-create a missing bind-mount destination**;
  `Namespace.prepareMountpoints` must run after the overlay mount and
  before every `runsc run`/`restore`.
- **Nothing this package owns may bind-mount under `/run/ate`**; this
  package's own sockets live under `/run/ate-ish/*` instead.
- **Checkpoint needs SIGUSR1, restore needs SIGUSR2**, relayed to
  every egress forwarder the sandbox's init has started, including one
  a wildcard rule's DNS responder starts after boot (`sandbox.fwdSet`).
- **Suspend must reset the checkpoint directory first.**
- **The uploaded snapshot carries three directories**: `checkpoint/`,
  `durable-dirs/`, `rootfs-upper/`.
- **Every template's container needs an `image`.**
- **A wildcard hostname (`*.suffix` or bare `*`) resolves through the
  in-sandbox DNS responder on `127.0.53.53:53`, not `/etc/hosts`.** It
  asks the host's control socket (`egress.Gateway.ListenControl`) to
  resolve each name on demand; the host allocates a loopback address
  and a `Gateway.Listen` destination the first time a name matches,
  then answers the same way on every later query for it. An exact
  hostname still gets a static `/etc/hosts` entry and socket at boot.
  `tlsPassthrough`/`actorJwt` rules parse but do nothing; `openEgress`
  logs one line per rule that uses either.
- `runsc` and exported image rootfses now share one directory,
  `ATE_CACHE_DIR` or `~/.cache/ate-ish` (`internal/atelet.cacheDir`),
  not `~/.local/state/ate-ish`, which this VM left root-owned by an
  earlier sudo'd run.
- `ATE_EGRESS_RESOLVE=host=ip[:port],...` and `ATE_EGRESS_TEST_CA=<pem>`
  are test-only Gateway overrides, read once at startup, never set by
  default: the former dials a stand-in instead of a hostname's real
  address, the latter trusts its self-signed leaf, only for a
  destination the resolve override actually named.

## Not done yet

- G4 ran (http, https on a non-443 port, `*.vercel.sh`, bare `*`, an
  unlisted name under a specific policy): all pass, `hack/smoke.sh`.
- G5 to G9 not run. ID mapping beyond root is not implemented; every
  sandbox runs as one mapped root. A wildcard destination's forwarder
  is not exercised by G6/G7 specifically.
- README's non-Cloud-Run sections still describe the vendored worker;
  gone. Left alone for stage D.

## Where the detail lives

docs/PROGRESS-ROOTLESS.md: "Built"/"Deleted", "Two real bugs found and
fixed", "Gate results", and the stage B section (fakeupstream, the
shared cache, wildcard DNS, timings).
