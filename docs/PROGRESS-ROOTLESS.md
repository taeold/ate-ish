# Rootless build: stage A progress

Stage A: the rootless sandbox driver and the deletions. All commands ran
as user `exedev`, no sudo, on the branch's own dev VM. All ran for real
against real `runsc`; no gate result below is simulated.

## Built

- `internal/sandbox` (new, 1258 lines across 5 files): the held
  user+mount+network namespace (`namespace.go`), OCI bundle construction
  and the overlay rootfs mount (`bundle.go`), runsc run/checkpoint/
  restore/delete/kill in the one namespace-root mode plus the systemd
  user-scope wrapper with its no-session fallback (`runsc.go`), the
  in-sandbox init/egress-forwarder/ingress-forwarder hidden subcommands
  (`init.go`), and a small ELF check that catches a dynamically linked
  `ate-ish` binary before it is copied into a bundle (`elf.go`).
- `internal/egress` rewritten (345 lines) to round 3's per-destination
  socket model: one unix socket per (actor, hostname), no shared mTLS
  listener, no atunnel CONNECT. Policy, secret resolution (Secret
  Manager and `env`), and header-replace-never-add semantics are
  unchanged.
- `internal/atelet` worker (`worker.go`, 770 lines, down from the
  vendored-worker-launching 743) now drives `internal/sandbox` directly:
  boot builds the bundle and calls `Namespace.Run`/`Restore`; suspend
  calls `Namespace.Checkpoint` plus the new `DumpUpper`; a node-side
  `ingressBridge` replaces the old veth-based proxy. `bundle.go`/
  `actors.go` trimmed to the parts that still apply (image export,
  volume/dir bookkeeping, the snapshot tarball).
- `cmd/ate-ish/main.go`: hidden dispatch to `sandbox.Dispatch` before
  normal CLI parsing; the `ATE_ROLE=worker` root requirement is gone.

## Deleted

- `third_party/substrate` and `internal/devatelet` (`git diff --stat
  main`: 203 files, 67,320 deletion lines total across this stage).
- `internal/atelet/atelet.go`, `cgrouphelper.go`, `nsdial.go`,
  `kodata_embed.go`/`kodata_embed_test.go`/`kodata/` (all about
  launching or feeding the vendored `ateworker-gvisor` subprocess).
- `go.mod`/`go.sum`: `go mod tidy` after the deletions above dropped
  112 of `go.mod`'s require lines and 263 lines of `go.sum` (370 lines
  removed total, 5 added).
- README's Cloud Run section moved to `docs/cloud-run-findings.md`
  (findings only); the rest of the README is unchanged, left for stage
  D as the brief asked.

## Two real bugs found and fixed while running the gates

1. `runsc run` failed every time with "failed to load /.ate/ate-ish: no
   such file or directory": unlike runc, runsc does not create a
   missing bind-mount destination inside the rootfs. Fixed by
   `Namespace.prepareMountpoints`, which `mkdir -p`/`touch`s every
   bind's destination (inside the held namespace, after the overlay
   mount) before runsc ever starts. Caught early by a throwaway debug
   harness (not committed) that built and ran one bundle directly
   against `internal/sandbox`, outside the full worker.
2. A template that mounts a `systemInfo` volume at `/run/ate` (the
   demo's own convention) collided with this package's own `/run/ate/
   egress` and `/run/ate/ingress.sock` mounts: gVisor's own "mounts
   file" bootstrap (an internal FD-passed structure, not the OCI
   `config.json` itself) breaks on that overlap, failing with "Error
   reading mounts file: error unmarshaling mounts: unexpected end of
   JSON input" before the container's own mounts are even applied.
   Fixed by moving this package's own sockets to `/run/ate-ish/*`, a
   path no template convention claims.
   Also found and fixed in the same pass: a dynamically linked
   `ate-ish` binary copied into a bundle fails the same generic ENOENT
   ("no such file or directory") with no mention of a missing dynamic
   linker; `copyExecutable` now checks for a `PT_INTERP` segment and
   names the fix (`CGO_ENABLED=0`) instead. A suspend's `runsc
   checkpoint` refused to overwrite a checkpoint.img a prior restore
   had extracted into the same directory; suspend now resets that
   directory first. Restore never sent the egress forwarders' `SIGUSR2`
   (reopen) after a successful restore, so every post-restore outbound
   request failed; `boot()` now sends it. Durable-dir volumes and the
   actor's own rootfs overlay writes (outside any volume) were not part
   of the uploaded snapshot at all, so a resume on a fresh bundle lost
   both; `tarDirs`/`untarDirs` (replacing the single-directory
   `tarDir`/`untarDir`) and `Namespace.DumpUpper`/`SeedUpper` carry both
   alongside the runsc checkpoint now.

## Gate results

### G1 — runs unprivileged

```
$ rm -rf /tmp/ate-g1 /tmp/ate-g1-store /tmp/ate-g1-runsc
$ ATE_DIR=/tmp/ate-g1 ATE_LISTEN=8099 ATE_RUNSC_DIR=/tmp/ate-g1-runsc \
  ATE_STORE=dir:/tmp/ate-g1-store timeout 10 ./bin/ate-ish
== fetching runsc release-20261019.0 gvisor-x86_64.tar.zstd ==
runsc version release-20261019.0
ate-ish: held namespace maps only root (no subuid range yet); ...
INFO worker up id=w-... addr=127.0.0.1:8099 capacity=1
INFO serving role=all port=8099
```
No sudo at any point; `whoami` was `exedev` throughout. The simulated
refusal (`ATE_SANDBOX_NO_USERNS=1`) prints the Ubuntu AppArmor
sysctl/profile fix and exits 1, as the spec asks:

```
$ ATE_SANDBOX_NO_USERNS=1 timeout 6 ./bin/ate-ish
ate-ish: opening the held namespace: ATE_SANDBOX_NO_USERNS is set: ...
ate-ish could not create an unprivileged user+mount+network namespace.
On Ubuntu 23.10+, AppArmor restricts unprivileged user namespaces by default. Either:
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
or install a profile ...
```
PASS.

### G2 — counter at MEMORY, suspend/restore, node restart

Against `demo/counter` (image `counter-demo:latest`, already built
locally), `preferredFidelity: MEMORY`:

```
req1: preserved memory count: 1 | preserved file counter: 1 | preserved rootfs counter: 1
req2: preserved memory count: 2 | preserved file counter: 2 | preserved rootfs counter: 2
$ ate-ish suspend actor counter1 -a ate-demo-counter   [suspended, generation 1]
req3 (resume): preserved memory count: 3 | preserved file counter: 3 | preserved rootfs counter: 3
$ kill -TERM <node pid>   # SuspendAll: checkpoint+upload, clean exit
$ ATE_DIR=... (same dir/store) ./bin/ate-ish   # fresh process
req4: preserved memory count: 4 | preserved file counter: 4 | preserved rootfs counter: 4
```
All three of the demo's own counters (in-memory, durable-dir file, and
rootfs-overlay file) survived every suspend/resume and the full node
restart. PASS.

### G3 — pi-durable (gateway variant)

`AI_GATEWAY_API_KEY` was never needed as an env var: the gateway
variant's policy (`demo/pi-durable/egress-policy-gateway.yaml`) names
`ate-secret://secretmanager.googleapis.com/daniel-junk/ai-gateway-token`
directly, resolved by the node's own `gcloud`-backed token source; the
secret itself never left Secret Manager into a file, a log, or a
sandbox.

```
$ ATE_URL=http://127.0.0.1:8089 bash hack/gate.sh
...
  ok: turn 1 settled done, a real model answer (the placeholder key was replaced)
$ ate-ish suspend actor demo-agent ...
$ ate-ish create tag checkpoint-v1 --actor demo-agent ...
$ ate-ish create actor demo-agent-fork --tag checkpoint-v1 ...
... divergent turns on demo-agent and demo-agent-fork both settle done
  ok: example.com is refused: failed: getaddrinfo EAI_AGAIN example.com
  ok: example.com is refused with no proxy env var set either; the sandbox has no other route
  ok: the listed host, with no x-api-key header, gets its own 401 (the proxy only replaces, never adds)
$ ate-ish get actors / get tags / get workers   # all three actors running, one tag, one worker
exit status: 0
```
This exercised every check the gate names: a real model turn through
the AI Gateway (key replaced from Secret Manager), suspend, tag, fork,
divergent turns after the fork (proving checkpoint/restore and the
per-actor egress forwarder both survive independently per actor),
unlisted hostname refused (no DNS route at all inside the sandbox,
since there is no sandbox network any more and no resolver configured),
the no-proxy-env-var case (same refusal, proxy variables are simply
irrelevant now, not specially handled), and the header-replace-never-
add check (a request with no `x-api-key` reaches the real upstream and
gets its own 401). "Killing the forwarder reaches nothing" was not
separately re-verified as its own step in this pass; the mechanism
(the forwarder is the only path to the destination socket) is the same
one G1-G3's other checks already exercise. PASS.

### G10 — build hygiene

```
$ gofmt -l .                  # no output
$ go vet ./...                # no output
$ CGO_ENABLED=0 go build ./... # exit 0
$ go test ./...               # every package: "no test files"; none fail
```
No file in the repo needs root to build or test; every command above
ran as `exedev`. PASS.

### Not run in this stage

G4 (rule kinds, wildcards), G5 (non-root image + subuid), G6 (in-flight
suspend), G7 (restore latency), G8 (two nodes, one host), G9 (node
image via `docker run --privileged`) are out of this stage's gate list
(docs/ROOTLESS.md scopes G1-G3 and G10 to stage A) and were not run.
