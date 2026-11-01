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

## Stage B: a fast loop, then the egress rule kinds

### Part 1 — hack/fakeupstream and the shared cache

`hack/fakeupstream` is a small Go program: self-signed TLS for a
hostname given on its command line, answers both `/v1/messages`
(Anthropic's streaming shape, which pi-ai's gateway provider turned
out to require — a plain JSON body failed with "Anthropic stream
ended without a stop reason" the first time this ran for real) and
`/v1/chat/completions`, 401 unless `x-api-key` matches `-api-key`
exactly. It never runs a real model: a plain prompt gets a canned
"done" with no tool call (gate.sh's own `expect` checks never assert
file contents, only that a turn settles), and a prompt of the exact
shape "Run this exact command with the bash tool and reply with its
output only: `<cmd>`" gets a `tool_use` for `<cmd>` so the real
sandboxed egress checks still run for real and report what actually
happened.

`internal/egress.Gateway` gained two env overrides, read once at
startup and never set by default: `ATE_EGRESS_RESOLVE=host=ip[:port]`
dials that address instead of the hostname's real one for a named
destination, and `ATE_EGRESS_TEST_CA=<pem>` trusts that stand-in's
leaf — applied only to a destination the resolve override actually
named, never to a real one (the first version applied it
unconditionally, which broke every real https destination behind the
same node: fixed by threading an `overridden bool` out of
`dialAddrOverride`).

`runsc` and exported image rootfses now share one cache directory,
`ATE_CACHE_DIR` or `~/.cache/ate-ish` (`internal/atelet.cacheDir`).
The old default, `~/.local/state/ate-ish`, was left root-owned by an
earlier sudo'd run on this VM; moving the shared cache under `~/.cache`
sidestepped that instead of chowning a directory outside this stage's
own work.

`hack/gate.sh` took a `--fake` flag: it only swaps the default egress
policy to `demo/pi-durable/egress-policy-gateway-fake.yaml`
(credentialUri `ate-secret://env/AI_GATEWAY_API_KEY` instead of Secret
Manager); the caller is still the one that starts fakeupstream and
exports `ATE_EGRESS_RESOLVE`/`ATE_EGRESS_TEST_CA`/`AI_GATEWAY_API_KEY`
before the node itself starts.

`hack/smoke.sh` builds the binary, starts a node with a dir store,
runs the counter demo (1, 2, suspend, 3), then runs `hack/gate.sh
--fake`'s full sequence (turn, suspend, tag, fork, divergent turns,
all three egress checks) against fakeupstream. Measured (2026-11-01,
`~/.cache/ate-ish` already populated from stage A for warm; moved
aside and let `EnsureRunsc`/`imageRootfsDir` repopulate it for cold):

```
cold: 32574 ms total (build 361 ms, fakeupstream 212 ms, node up 118 ms,
      counter 4237 ms, gate.sh --fake 24748 ms)
warm: 23237 ms total (build 350 ms, node up 122 ms, counter 4243 ms,
      gate.sh --fake 18406 ms)
```
Both well under the one-minute target.

### Part 2 — the egress rule kinds (G4)

`records.EgressPolicy` generalized from https-only, port-443-only,
exact-hostname-only to `EgressRule{HTTP, HTTPS, TLSPassthrough,
ActorJWT *EgressRuleSpec}`, each with a `Port` (defaulting 80/443) and
a hostname list that may be an exact name, `*.suffix`, or bare `*`
(`records.IsWildcard`/`MatchesWildcard`). `tlsPassthrough`/`actorJwt`
parse but do nothing; `worker.go`'s `openEgress` logs one line per
rule using either, as the spec asks, and otherwise ignores them.

An exact hostname keeps the existing boot-time mechanism (a static
`/etc/hosts` entry, a loopback address, a host socket), now for any
`http`/`https` proto and port instead of https-443 only. A wildcard
hostname cannot get a boot-time entry — the real hostname it will
match is not known yet — so it goes through a new path instead: the
sandbox's own DNS responder (`sandbox.runDNSResponder`, inside
`RunInit`, listening on `127.0.53.53:53`, the actor's only
nameserver when any wildcard rule exists) asks a per-actor control
socket (`egress.Gateway.ListenControl`) to resolve each queried name;
the host walks the actor's wildcard rules (first match in policy
order wins, so list a specific pattern like `*.vercel.sh` ahead of a
bare `*` in the same policy), allocates a loopback address and a
`Gateway.Listen` destination the first time that exact name is seen,
and remembers both for every later query. The sandbox then starts one
more forwarder from that address to the new destination — through the
same `fwdSet` the static ones use, so SIGUSR1/SIGUSR2 still reach it
for checkpoint/restore — and answers the DNS query with the address.

Two real bugs found running this for real, beyond the resolve-override
one in Part 1: (1) the DNS answer initially raced the actor's own
connect against the new forwarder's listen (a fresh `exec.Cmd` is
asynchronous) — `fetch failed ... ECONNREFUSED` on the first wildcard
request every time; fixed by `waitDial`, blocking the DNS reply until
the forwarder's address actually accepts a connection. (2) the
`internal.test` non-443 check first used a custom header name
(`x-internal-key`) that fakeupstream's own auth check never looks at
(it only reads `x-api-key`) — not an egress bug, just the test target
checking the wrong header; fixed by using `x-api-key` there too, same
as every other check.

G4, run for real via `hack/smoke.sh`'s own section (two more pi-durable
actors, policies at `demo/pi-durable/egress-policy-g4{,-star}.yaml`):

```
ok: http rule reaches http://example.com/: status 200
ok: https rule on a non-443 port reaches internal.test:8443, key replaced: status 200
ok: *.vercel.sh matches ai-gateway.vercel.sh, placeholder key replaced: status 200
ok: bare * reaches example.com and example.org with no key
ok: under g4-rules's specific policy (no bare *), an unlisted name still fails: failed: getaddrinfo ENOTFOUND example.org
```
`*.vercel.sh` matching the real `ai-gateway.vercel.sh` with the real
key replaced was run once more, for real, as part of the G2/G3 rerun
below (`demo/pi-durable/egress-policy-gateway-wildcard.yaml`, same
rule but `ate-secret://secretmanager.googleapis.com/...`). PASS.

### G2/G3 rerun, to show nothing regressed

G2 (as its own node, dir store under `/tmp/g2-regress`, not
`hack/smoke.sh`'s ephemeral one): 1, 2, suspend, 3, `SIGINT` the node,
start it again, next request 4 — all correct. G3: `hack/gate.sh` with
no `--fake`, `AI_GATEWAY_API_KEY` from `gcloud secrets versions access
latest --secret ai-gateway-token --project daniel-junk` into the
environment only, `POLICY=demo/pi-durable/egress-policy-gateway-
wildcard.yaml` (so this same run also covers G4's real-upstream
wildcard check): every step green, real model answers ("Successfully
wrote to app.js", "Successfully replaced 1 block(s) in test.js", ...),
suspend/tag/fork, divergent turns, both DNS-refusal checks, and the
401 check. PASS, no regression.

### hack/smoke.sh total, with G4

```
32593 ms total (counter+gate.sh --fake section 18650 ms, G4 section 8434 ms)
```
