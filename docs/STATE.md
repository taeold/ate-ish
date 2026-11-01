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
  in-sandbox init/forwarders. Nothing outside it calls runsc directly.
- `internal/egress`: the per-destination-socket gateway (one unix
  socket per actor+hostname), header replacement, Secret Manager/env
  secret resolution.
- `internal/atelet`: the `Worker` driving `internal/sandbox` and
  `internal/egress` per actor; records/leases/placement; the node-side
  ingress bridge (`ingressBridge`, replaces the old veth proxy);
  `EnsureRunsc` (fetches the pinned gVisor release).
- `internal/ingress`, `internal/records`, `internal/store`,
  `internal/cli`, `internal/apierr`, `internal/envutil`: unchanged.

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
bash hack/gate.sh  # ATE_URL must already be exported
```

## Non-negotiable requirements

- **ate-ish must be a static binary.** It is copied into every bundle
  at `/.ate/ate-ish`; the sandbox rootfs has no libc. `bundle.go`'s
  `copyExecutable` checks for a `PT_INTERP` ELF segment and refuses a
  dynamic one with the fix (`CGO_ENABLED=0`) in the error; a plain
  `go build` on this toolchain produces a dynamic binary.
- **runsc does not auto-create a missing bind-mount destination**
  (runc does). `Namespace.prepareMountpoints` must run after the
  overlay mount and before every `runsc run`/`restore`.
- **Nothing this package owns may bind-mount under `/run/ate`.**
  That path is a template's own convention (`systemInfo` volumes
  mount there); this package's own sockets live under
  `/run/ate-ish/*` instead. Mounting anything else at `/run/ate`
  while also bind-mounting under it breaks gVisor's own internal
  mounts bootstrap ("Error reading mounts file") before the
  container's mounts even apply.
- **Checkpoint needs SIGUSR1, restore needs SIGUSR2.** gVisor refuses
  to checkpoint a sandbox holding a bound host socket; `suspend()`
  sends `USR1` to close every egress forwarder's listener first, and
  `boot()`'s restore path must send `USR2` after a successful
  `Namespace.Restore` or every post-restore outbound request fails
  silently. Ingress never binds a host-visible socket, so it needs
  neither signal.
- **Suspend must reset the checkpoint directory first**: a restored
  actor's checkpoint dir already holds the extracted `checkpoint.img`,
  and runsc refuses to overwrite one.
- **The uploaded snapshot carries three directories, not one**: the
  runsc checkpoint (`checkpoint/`), durable-dir volumes
  (`durable-dirs/`), and the actor's own rootfs overlay writes outside
  any volume (`rootfs-upper/`, via `Namespace.DumpUpper`/`SeedUpper`).
  Dropping any one silently loses that state on the next restore.
- **Every template's container needs an `image`**; `imageRootfsDir`'s
  per-digest export is the only rootfs source, no node-filesystem
  fallback.
- `runsc` lives at `ATE_RUNSC_DIR` or `~/.local/state/ate-ish/runsc`,
  fetched once, shared by every actor. Exported image rootfses cache
  at `<ATE_DIR>/rootfs/<digest>`; the store defaults to
  `~/.local/state/ate-ish/store` unless `ATE_STORE`/`ATE_BUCKET` is
  set. `ATE_SANDBOX_NO_USERNS=1` simulates the kernel refusing
  unprivileged namespaces; `ATE_SANDBOX_DEBUG=<path>` turns on runsc's
  own `--debug --debug-log=<path> --alsologtostderr`.

## Not done yet

- G4 (rule kinds incl. wildcards), G5 (non-root image + subuid
  mapping), G6 (in-flight suspend), G7 (restore latency), G8 (two
  nodes, one host), G9 (`docker run --privileged` node image): not
  run. ID mapping beyond root is not implemented; every sandbox runs
  as one mapped root.
- README's non-Cloud-Run sections still say `go install` needs
  `third_party/substrate/cmd/ateworker-gvisor` and describe a patch to
  the vendored worker; both are gone. Left alone for stage D.

## Where the detail lives

docs/PROGRESS-ROOTLESS.md: "Built"/"Deleted" (file and line counts),
"Two real bugs found and fixed" (full repro and fix for each), and
"Gate results" (exact commands, outputs, and PASS/not-run per gate).
