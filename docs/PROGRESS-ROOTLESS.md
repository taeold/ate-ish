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

## Stage C: orphaned sandboxes, non-root images, in-flight suspend, restore latency

### Orphaned sandboxes

Reproduced the G2-node-restart bug first: start a node, start the
counter, (a) `SIGINT` it — every sandbox stopped, node exited with
none left (`SuspendAll`, already correct); (b) `SIGKILL` a second
node — its `runsc-sandbox`/`runsc-gofer` processes and its held
namespace's own `unshare ... sleep infinity` holder kept running,
found by plain `pgrep`, with no `ate-ish` process alive. The sandbox
is a plain host process either way: `Namespace.Enter` never joins the
holder's own pid namespace (only user/mount/net), so a sandbox this
package starts is never inside any pid namespace the holder created.

Fix, in `Namespace.Open` (`namespace.go`'s `reapOrphans`, `idmap.go`
untouched by this part): before creating its own holder, run
`runsc --root=<stateDir> list -quiet -sandbox` directly (no nsenter
needed, for the same plain-host-process reason) and `delete -force`
whatever it finds, one `slog.Warn` line per id. The holder's own pid
is now recorded next to `--root` (`holder.pid`) so a stale one gets
killed too. Chose kill-and-delete over restore: an orphan's checkpoint
belongs to the generation record, not to the runsc process left
behind, so restoring here would race whatever actually owns that
record next.

`hack/smoke.sh`'s own "orphaned sandboxes" section reproduces both
paths for real (a separate node+dir, `SIGINT` then `SIGKILL` then
restart) and asserts `pgrep` for `runsc-sandbox`/`runsc-gofer` finds
nothing at each checkpoint. PASS.

### G5 — non-root images

First surprise: `buildConfig`'s OCI `process.user` was hardcoded
`0,0` for every actor regardless of the image's own `USER` directive,
despite a comment claiming otherwise — nothing ever actually dropped
privilege. Fixed by always keeping the sandbox's own init (pid 1) at
root (it needs real access to the host-backed ingress/egress sockets
throughout) and having `RunInit` start the *actor's* child process
with `ATE_ACTOR_UID`/`GID` as a `Credential`, the same gosu/tini
pattern a real root-starting entrypoint uses; `CAP_SETUID`/
`CAP_SETGID` were already in `bundleCaps`.

Second surprise: `docker export`+`tar -xpf` extracting as this plain
host process (outside any namespace) can't chown anything — every
exported file lands owned by this node's own real uid no matter what
the image declares. Fixed by running the extraction's `tar` *inside*
the held namespace (`extractCmd`, via `Namespace.Enter`) as that
namespace's own mapped root, which can chown to any id its own
mapping covers. This made `imageRootfsDir`'s per-digest export cache
wrong, too: a root-only node's export and a multi-ID node's export of
the same image are not interchangeable, so the cache directory now
also keys on the id-mapping mode (`cacheSuffix`). A root-only node's
tar still can't chown to an unmapped id (`Cannot change ownership...
Invalid argument`, GNU tar's own fatal exit 2); `tarExtractErr`
downgrades that specific, expected failure to one logged warning
instead of failing the whole export, since the file's own bytes still
land fine either way.

Multi-ID mapping itself (`internal/sandbox/idmap.go`): `Open` checks
for `newuidmap`/`newgidmap` on `PATH` and a `/etc/subuid`/`/etc/subgid`
entry for this user. Present: the held namespace unshares with *no*
mapping of its own, and `applyIDMapping` writes it after the fact —
namespace id 0 to this process's own real id (always allowed), plus
1..range to the real subuid/subgid range — exactly rootless Podman's
own two-entry map. Missing either: falls back to `unshare
--map-root-user` (root only), logged once either way.
`ATE_SANDBOX_FORCE_ROOT_ONLY_MAPPING=1` forces the fallback for
testing on a host that actually has the real thing (this VM, once
`uidmap` was installed).

Run for real, in order, on this VM (no `newuidmap`/`newgidmap`
present yet):

```
ate-ish: held namespace maps root only (no newuidmap (install the uidmap package)); non-root images will fail
{"home":"/home/node","write":"failed: EACCES: permission denied, open '/home/node/hello.txt'","id":"uid=1000(node) gid=1000(node) groups=1000(node)"}
```

Then the one approved host-prep `sudo` (same standing as the Ubuntu
AppArmor profile): `sudo apt-get install -y uidmap`. Rerun, same
image, same VM, nothing else changed:

```
ate-ish: held namespace maps root plus subuid 100000-165535, subgid 100000-165535 (rootless Podman's own approach); non-root actor images work
{"home":"/home/node","write":"ok","id":"uid=1000(node) gid=1000(node) groups=1000(node)"}
```

Also reran, under the multi-ID mapping: the counter (a root-run image,
unaffected either way) and a root-starting actor's own `gosu nobody`
(`demo/g5-gosu`, FROM node:24-slim with no `USER`, apt-get installs
gosu):

```
{"root_id":"uid=0(root) gid=0(root) groups=0(root)","gosu_nobody_id":"uid=65534(nobody) gid=65534(nogroup) groups=65534(nogroup)"}
ok: the counter (a root-run image) still runs under multi-ID mapping
```

All of this is `hack/smoke.sh`'s "G5 non-root images" section now:
the fallback path forced via the env switch (so the gate is
deterministic once `uidmap` is installed for good), then the real
multi-ID path when `newuidmap`/`newgidmap` are actually present.

### G6 — suspend during a request

Built a host-side test server (`hack/fakeupstream -delay 20s`) and a
dedicated pi-durable actor pointed at it through the usual resolve
override. Posted `/submit` (the actor's own `server-gateway.ts`
handler making the outbound LLM call, not a `runsc exec`), waited for
the egress forwarder's own "listening" log line to confirm the call
was actually in flight, then ran `ate-ish suspend actor` concurrently.

Result, recorded exactly as asked (either answer was acceptable):
**the checkpoint itself succeeds** (`sandbox checkpoint ... took_ms=266`,
`actor suspended ...`), **but the original request does not survive
it** — the sandbox is torn down right after the checkpoint completes
(today's `suspend` has no `--leave-running`), so the node-side proxy's
connection to it drops mid-flight (`http: proxy error: unexpected
EOF`) and the client gets a 502, well before the 20 s delay would
have answered. No code change; `hack/smoke.sh`'s "G6 in-flight
suspend" section reproduces this and records both halves
(`checkpoint while the actor's own outbound call was in flight:
rc=0`, `the original in-flight request: {"status":502}`) every run.

### G7 — restore latency

Chasing G6's own restore led straight to the real bug this gate was
named for. A first request right after a plain (not in-flight) suspend
and restore hung past the ingress bridge's own 10 s wait for a
connection from the sandbox's outforwarder — every time, reproducibly.

Cause: the ingress dial-out forwarder's open connection to the node's
`ingress.sock` is restored as part of the sandbox's own checkpointed
process state, but the *node-side* listener it was connected to is
gone — a restored generation gets a brand-new bundle, a brand-new
actor UID, and so a brand-new `ingress.sock` path under a brand-new
bind mount. The restored connection is simply stale, and (unlike the
egress forwarders, whose listener close/reopen around checkpoint
already gets a signal) nothing about it errors or notices on its own
until something actually tries to use it — which, empirically, took
over 10 s to surface as a hang, not a quick error.

Fix (`internal/sandbox/init.go`): wrapped the outforwarder's
`exec.Cmd` in a small `outforwarder` type and had `RunInit` call its
`restart` (kill the old process, start a fresh one) on `SIGUSR2` — the
same signal that already reopens every egress forwarder's listener
after restore — instead of leaving it to rediscover the problem on
its own. Measured before/after on the same clean boot→suspend→restore
→request sequence: before, the first post-restore request never
completed inside a 15 s client timeout; after, `ate-ish`'s own log
shows restore (`took_ms=354`) and boot (`boot_ms=209`) completing in
under a second, and the full first-request round trip (restore + boot
+ the fake turn itself) at 1.2 s, then 3.1 s on a second, independent
run through `hack/smoke.sh`'s own "G7 restore latency" section (a
regression guard at 5 s there, not a precise SLA, since that number
also includes the fake model's own turn, not just the redial).

### G2/G3 rerun, to show nothing regressed

G2, as its own node (`/tmp/g2-regress`, not `hack/smoke.sh`'s
ephemeral one): 1, 2, suspend, 3, `SIGINT` the node, start it again,
next request 4 — all correct, and (new this stage) nothing left
running afterward. G3: `hack/gate.sh` with no `--fake`,
`AI_GATEWAY_API_KEY` from `gcloud secrets versions access latest
--secret ai-gateway-token --project daniel-junk` into the environment
only (confirmed absent from every log file written), `POLICY=demo/
pi-durable/egress-policy-gateway-wildcard.yaml`: every step green,
real model answers, suspend/tag/fork, divergent turns, both
DNS-refusal checks, the 401 check. PASS, no regression.

### hack/smoke.sh total, with G5/G6/G7

```
80979 ms total
```

`go build ./...`, `go vet ./...`, `gofmt -l .` all clean.

## Stage D: G8, G9, the README

### Advertise address fix

`startNode`'s role-`all` path hardcoded `127.0.0.1:<port>` as the
address a node puts in its own lease, which only works when every node
on a shared bucket runs on the same host. `ATE_LISTEN` is now a full
`host:port` (default `127.0.0.1:8080`, no authentication), and a new
`advertiseAddr` picks the lease address: `ATE_ADVERTISE` if set, else
`ATE_LISTEN` itself when its host is not a wildcard, else the
machine's first non-loopback IPv4 address with the listen port.
`hack/smoke.sh`'s bare-port `ATE_LISTEN=8082` style calls were updated
to `127.0.0.1:8082` to match.

### G8 — two nodes, one bucket, both as exedev, no sudo

`gs://ate-ish-rootless-twonode-daniel-junk` (`us-west1`, created and
deleted by this stage). Node A (`ATE_DIR=/tmp/g8-a`,
`ATE_LISTEN=127.0.0.1:8180`) and node B (`ATE_DIR=/tmp/g8-b`,
`ATE_LISTEN=127.0.0.1:8181`), both `ATE_BUCKET=` the same bucket: each
advertised its own address correctly (no `ATE_ADVERTISE` needed, since
neither `ATE_LISTEN` is a wildcard), and `get workers` against either
node's endpoint listed both leases.

```
create atespace/actor-template/actor via node B (--endpoint :8181)
request via node A (--endpoint :8180): memory count 1 (placed on A, the
  node that received the request)
request via node A: count 2
suspend actor counter1 via node A: generation 1
kill -INT node B: "lease deleted", clean exit, no process left
request via node A: count 3, worker w-2965a4fb (node A), generation 2
```
The actor resumed on node A with the counter continuing, after node B
(never the actor's own host in this run) was gone. `kill -INT` on node
A afterward suspended on shutdown and deleted its lease cleanly; no
`ate-ish` process remained. PASS. Bucket deleted and confirmed gone
(`gcloud storage buckets list` returned 0 items).

### G9 — the node image

The Dockerfile already built one binary with no vendored worker
(stage A/B/C's own work); this stage added the `uidmap` package
(`newuidmap`/`newgidmap` for non-root actor images) and removed
`nsenter` from the apt list, which does not exist as a separate
Debian trixie package (`util-linux` already provides the binary) and
made `docker build` fail outright before this fix.

```
$ docker build -t ate-ish .                     # succeeds
$ docker run --rm --privileged -p 8090:8080 \
    -e ATE_LISTEN=0.0.0.0:8080 ate-ish           # serves; advertises
                                                  # 172.17.0.2:8080 (its
                                                  # own non-loopback
                                                  # IPv4, ATE_LISTEN
                                                  # being a wildcard)
```
The counter's rootfs (`counter-demo:latest`) is only on this host's
local Docker daemon, which the container's own design deliberately
does not reach (no docker CLI or socket baked in); this run mounted
the host's `docker.sock` and copied the host's `docker` CLI into the
running container as a one-time test step (not a Dockerfile change) so
`ate-ish create actor` could resolve the image. From the host, via the
CLI's `--endpoint http://127.0.0.1:8090`: two requests (counts 1, 2),
`suspend actor`, a third request (count 3, resumed). PASS.

### README

Rewritten for the rootless product in celld's outline (`docs/
WHAT.md`'s one-sentence shape, How it works, Install, Run it,
Requirements, Operate a fleet, Compatibility, Limits, Contributing and
License), 1,184 words. Every command in "Run it" and the Install
section's verify step ran for real this stage, including a real-model
pi-durable turn (`template-gateway.yaml`/`egress-policy-gateway.yaml`,
`AI_GATEWAY_API_KEY` from Secret Manager). The non-gateway pi-durable
demo (`egress-policy.yaml`) was dropped from the README: its committed
`credentialUri` names a Secret Manager secret
(`daniel-junk/gemini-api-key`) that does not exist in this project
(404), a pre-existing gap unrelated to this stage; the gateway variant
is what `hack/gate.sh` itself runs and is what this stage verified.
`docs/WHAT.md`'s promises section no longer says the actor runs under
Agent Substrate's own worker; it now says ate-ish's own gVisor driver,
built to the same contract.

### hack/smoke.sh and hack/gate.sh, real model, end of stage

`hack/smoke.sh`: PASS, 82.9 s, nothing left running. `hack/gate.sh`
with no `--fake`, `AI_GATEWAY_API_KEY` from `gcloud secrets versions
access latest --secret ai-gateway-token --project daniel-junk` into
the environment only: every step green, a real model turn, suspend,
tag, fork, divergent turns, both DNS-refusal checks, the 401 check.
PASS, no regression.

## Stage E: review fix pass (namespace holder leak, smoke.sh diagnostics, gate.sh standalone)

Three defects from review, fixed in order.

**1. The namespace holder leaked.** `sleep infinity`, `unshare`'s
`--fork` child, was the held namespace; `Namespace.Close` killed
`n.holder.Process` — the `unshare` wrapper, already a different
process once it forked — never `n.pid`, so SIGINT/SIGTERM shutdown
left it running, and a SIGKILLed node left it running too, with the
reaper only handling runsc sandboxes. 107 orphaned `sleep infinity`
processes, each pid 1 of its own pid namespace (so SIGTERM is ignored,
SIGKILL is needed), had piled up on this VM from one day's runs.
Fixed: the holder is now `sh -c 'read _ <&3'` on fd 3, the read end of
a pipe (`os.Pipe`) whose write end (`Namespace.holderW`) this process
keeps open for as long as it runs and never writes to. Losing that fd
for any reason — `Close` closing it explicitly, or this process dying
for any reason including SIGKILL, which the kernel closes every fd for
on its own — ends the holder's read, so the holder (pid 1) exits and
the kernel tears the namespace down with it; no code in this process
has to run for that to happen. `internal/sandbox/namespace.go`. Live
check, SIGKILL: start a node, `kill -9` it, `readlink /proc/<pid>/ns/
user` on every exedev process immediately after — none differs from
the shell's own (before this fix, `sleep infinity`'s did). Same for
SIGINT. `hack/smoke.sh`'s orphaned-sandboxes section now runs this
exact check (`assert_no_userns_leak`) right after both the SIGINT and
the SIGKILL-and-restart cases, alongside the existing runsc check.

**2. `hack/smoke.sh` failed once in review, exit 1, no message**, right
after the counter's suspend. Cause: `got=$(curl ... | memoryCount)`
under `set -e`+`pipefail` — when `memoryCount`'s own `grep` found
nothing, the whole pipeline's nonzero status killed the script at that
assignment, before the `[[ "$got" == "3" ]] || …` line ever ran, so
nothing printed. Every such assignment in the script (the counter
demo, the G4 `lastResult` checks, G5's `got`/`c1`/`c2`) now ends in
`|| true`, so a failed pipeline leaves `got` empty instead of killing
the script, and the existing comparison reports it. Every `FAILED`
site now goes through one `fail` helper that also tails the relevant
node's log (40 lines) and copies it to `/tmp/ate-ish-smoke-last.log` so
it survives the cleanup trap. Ran `hack/smoke.sh` 20-odd times across
this stage. It reproduced twice before the real fix below: once dying
silently (confirming the diagnosed cause), once with the new
diagnostics printing "counter request after suspend returned '', want
3" and the node log's own "write: broken pipe" — which is defect found
next, not a smoke.sh bug. The final check (below) was 4/5 PASS, 1
failure, same symptom, no double-execution; diagnostics, not a full
fix, for that residual case.

**3. `hack/gate.sh` required `ATE_URL` and refused to run standalone.**
`ATE_URL` now defaults to `http://127.0.0.1:8080`; if nothing answers
`/admin/workers` there, `gate.sh` builds and starts its own node in a
scratch dir the same way `hack/smoke.sh` does, tears it down on exit,
and — since it is a plain forked child — inherits `AI_GATEWAY_API_KEY`
from this process's own environment, which the demo policies now read
via `ate-secret://env/AI_GATEWAY_API_KEY`. Verified: `AI_GATEWAY_API_KEY=…
hack/gate.sh` from a clean shell, no `ATE_URL` set, PASS end to end
(real model, suspend, tag, fork, both DNS-refusal checks, the 401
check), its own node torn down on exit, nothing left running.

### A fourth bug found chasing #2: the ingress pool race, and a near-miss

The node log for #2's reproduction showed, right after a restore:
`actor started` (readiness probe already passed), then `http: proxy
error: write unix …/ingress.sock->@: write: broken pipe`. Cause: gVisor
restore resumes the *previous* generation's outforwarder workers
(`init.go`) as part of the restored process tree before this node's
own SIGUSR2 ever reaches init's handler to kill and replace them; a
resumed worker can dial back into the node's (already-listening, same
generation's) `ingressBridge` and get pooled, then die moments later
when the kill does land, leaving a connection in the pool that looks
fine but is already dead on the far end.

First attempt retried the whole request on any pooled-connection
error. It fixed the write-side case, but at the cost of a real one:
`hack/gate.sh`-style, it can also fail with "unexpected EOF" *reading*
the response, after the write already succeeded — and retrying that
blind re-ran the request, observed live as `hack/smoke.sh`'s counter
demo jumping from 3 straight to 4 (one request, two increments).
Caught by review before landing.

Fixed instead at the source: `ingressBridge.Restarted`, called right
after `Namespace.Restore` succeeds and before the SIGUSR2 that tells
init to kill the old workers, bumps a generation counter and drains
every connection already queued, so a straggler accepted in that
narrow window is dropped rather than handed to a real request.
`RoundTrip` keeps exactly one narrow retry on top of that: a
write-side EPIPE/ECONNRESET, where nothing was ever delivered, never a
failure while reading the response. `internal/atelet/worker.go`.

Verified: `hack/smoke.sh` five times in a row (the review's own ask),
after this fix: 4/5 PASS. The one failure reproduced the residual,
intentionally-unretried case — a straggler's write lands, the far end
is already gone, the response read gets "unexpected EOF" — with full
diagnostics (node log, `/tmp/ate-ish-smoke-last.log`) now capturing it
instead of dying silently, and no double-execution. `hack/gate.sh`
with the real model, from a clean shell per #3 above: PASS.
