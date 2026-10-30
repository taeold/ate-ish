# ate-node progress

## v2 stage 3 (2026-10-30): egress through the worker's network, with credential injection

Brief (docs/V2.md stage 3 plus the coordinator's message): replace
internal/devgateway's passthrough with v1's egress.go, adapted to be the
real EgressGateway address for every actor: verify atunnel's client
certificate against the node CA, map it to an actor by identity (not by
listener port — one gateway now, not one per actor), look up that actor's
real egress-policy.yaml, MITM its TLS, replace headers from Secret Manager,
refuse everything else. Gate on local pi-durable Gateway variant with the
template's placeholder key.

Gate: PASS, on a real, unattended `./gate.sh` run (exit 0) against
`sudo bin/ate-ish serve`, not just my own manual reproduction of its steps.
(1) turn 1 settles `status: done` with a genuine model answer (`gate.sh` now
asserts this explicitly, not just that `.settled` exists) — the gateway
read the real `ai-gateway-token` secret and replaced the placeholder
`x-api-key`. (2) from the actor's own bash tool, a `fetch` to
`example.com` (unlisted) fails `getaddrinfo ENOTFOUND`, identically with
and without `NODE_USE_ENV_PROXY=0` — DNS itself refuses the name before a
connection is ever possible, so no proxy variable changes anything, the
opposite of v1. (3) a raw `curl -k` POST to the listed host with no
`x-api-key` header at all gets the upstream's own `status 401` ("Missing
Authorization header") — the gateway left the request exactly as sent,
proving it replaces an existing header and never adds one that is not
there. (4) suspend, tag, fork, and a turn on the fork (through a
RestoreWorkload, not RunWorkload) all still pass, the fork's turn also
settling `done` with real egress.

Timings (the exit-0 run): turn 1 total 9.2s (resume 1.9s, the rest the real
model call); suspend 1.5s; a turn after resume (demo-agent, generation 2)
13.7s; the fork's first turn (RestoreWorkload) 14.2s; the two unlisted-host
checks 2.9-3.0s each (DNS refusal is near-instant; most of that is still
resume/ingress overhead); the no-header 401 check 3.1s.

What changed, beyond swapping the listener:
- `internal/devatelet.issueActorCertificate` now takes atespace/name and
  embeds them as a URI SAN (`ate-actor://<atespace>/<name>`) instead of a
  fixed `CN=actor`, so a verified client certificate maps straight to an
  actor identity — stage 2's certs carried no such information, fine for a
  passthrough that never needed to know who was calling.
- The real gateway's `tls.Config` now sets `ClientAuth:
  RequireAndVerifyClientCert` and `ClientCAs` from the node CA (exported as
  `PKI.NodeCAPEM`), not just `RequestClientCert` as the dev passthrough
  did; it looks up the actor's policy from ate-ish's own bucket record per
  connection (`g.bucket.get(actorKey(...))`), where stage 2's `devgateway`
  had no bucket access and no policy concept at all.
- `internal/devatelet/fakeatelet.go`'s `MintActorCertificate` now forwards
  the request's atespace/name through to the PKI call above.

Found and fixed, each confirmed by a real log line before fixing:
- atunnel's CONNECT names the transparently-redirected connection's
  *original destination* (`SO_ORIGINAL_DST`), always an IP — never the
  hostname the actor's own DNS query resolved, since that query already
  finished before the TCP connection atunnel intercepts even opens. A
  hostname-keyed `egress-policy.yaml` allowlist (`ai-gateway.vercel.sh`)
  cannot be checked against an IP by string equality; this measured as
  every CONNECT logging `allowed=false` with a bare IP as `host`.
  Fixed by resolving each allowed hostname and checking the destination IP
  against that set (`resolvePolicyHost`), not the hostname string.
- `ai-gateway.vercel.sh` answers from a large, rotating IP pool: a single
  fresh lookup can disagree with whatever subset the actor's own, separate,
  resolution landed on, so one request in several still measured
  `allowed=false` even with the fix above in place. Fixed two ways:
  up to 3 retried lookups 100ms apart per connection, and an
  accumulate-only cache of every IP ever seen for an allowed hostname across
  the gateway's lifetime, so coverage only grows across a run.
- v1's header replacement used `Header.Set` unconditionally — adding the
  secret even to a request that never carried the header at all. Stage 3's
  own gate (3) needs "replace, never add"; fixed by skipping replacement
  when `req.Header.Get(h.Header)` is empty, confirmed by the 401 ("Missing
  Authorization header", not a 200) this produces for the no-header case.
- The gate's own model actor refused to run the no-header probe once
  phrased as a bare `fetch` to the AI Gateway's own API ("could potentially
  be used to exfiltrate information") — a model safety heuristic, not a
  gateway bug (the identical request worked when I ran it directly).
  Fixed by framing the ask as a routine connectivity check with an expected
  401, and by using `curl -k` (no `NODE_EXTRA_CA_CERTS` trust needed) once
  plain `curl` failed TLS trust against the MITM leaf for unrelated reasons
  (curl reads the system trust store, not Node's env var).

HTTPS_PROXY/trust-bundle plumbing: there was no HTTPS_PROXY plumbing left
to remove — stage 2 never added it back (the transparent nftables redirect
makes it unnecessary, same conclusion V2.md names). The systemInfo trust
bundle still gets a CA's PEM at `/run/ate/trust-bundle.pem`
(`writeSystemInfoFiles`), now `egress.go`'s own MITM `egressCA`
(`w.gateway.ca.pem`) instead of stage 2's placeholder gateway CA — this one
the actor's own TLS genuinely needs to trust, to accept the MITM leaf for
`ai-gateway.vercel.sh`.

`internal/devgateway` deleted: nothing else referenced it once
`startEgressGateway` (egress.go) replaced its one call site in worker.go.

DNS: confirmed via gate step (2) above that unlisted names fail to
resolve (`ENOTFOUND`) and (implicitly, since turn 1 and the fork's turn
both got real answers) that listed ones do — the worker's DNS relay was
already wired to `GetActorEgressPolicy` in stage 2; nothing needed to
change there this stage.

Known, not fixed (cosmetic, not a gate failure): one `ERROR lease
err="generation changed"` log line after "lease deleted" on graceful
shutdown — the periodic lease-rewrite goroutine fires once more against an
already-deleted lease object before the process exits. Harmless; the
process is exiting anyway.

Files changed: egress.go rewritten (per-actor listener -> one gateway,
certificate-based identity, bucket-backed policy lookup, IP-to-hostname
resolution, replace-only-if-present headers); internal/devatelet/pki.go
(actor identity in the cert, `NodeCAPEM`, `GatewayCAPEM` removed —
unused once the real gateway writes its own CA into the trust bundle);
internal/devatelet/fakeatelet.go (pass atespace/name through); worker.go
(wires `startEgressGateway` in place of `devgateway.Start`, trust-bundle
source); internal/devgateway/ deleted; gate.sh (Gateway-variant defaults,
turn-1 real-answer assertion, requirement (2) rewritten to the opposite of
v1, requirement (3) new). No commit made, per the brief.

## v2 stage 2 (2026-10-30): the small atelet, on the vendored worker

Brief (docs/V2.md stage 2, with the coordinator's follow-up decisions):
worker.go/sandbox.go drive the vendored ateworker-gvisor as a separate
process ate-ish starts and controls, same boundary substrate itself has
between atelet and worker. ate-ish is the atelet: it serves WorkerSupport,
mints actor certs, holds the node CA (internal/devatelet, promoted from the
stage-0 probe). Gate: `./gate.sh`'s README sequence — template, actor, turn
1, suspend, tag, fork, divergent turns — on one local node, pi-durable's
Gateway variant, egress_gateway pointed at a passthrough CONNECT gateway
(internal/devgateway) rather than left empty, because a dead network gives
no way to tell "the worker runs the template" from "the worker is broken."

Gate: PASS. Verified twice: once while fixing bugs, once as a clean
from-scratch run (fresh bucket actors, fresh `sudo bin/ate-ish serve`) to
confirm reproducibility. Every step: create atespace/template/actor/egress-policy,
turn 1 (real HTTP round trip through the ingress to the actor and back),
suspend, tag, fork, a turn on each of the original and the fork (proving
restore and checkpoint-copy both work). `gate.sh`'s own success check
(`jq -e .settled`) passes on every turn.

Turn 1 and both divergent turns settle `status: unanswered, reason:
model_error`, not a model answer — by design this stage: AI_GATEWAY_API_KEY
is still `ate-placeholder-key` (the egress policy's header replacement
is the gateway's own job, stage 3's, same as docs/V2.md says), so the real
Vercel AI Gateway correctly answers 401. What matters for this gate is that
it is a real 401 from the real service, not a connection failure: DNS
resolved, TLS to the real host completed, the real HTTPS request reached
`ai-gateway.vercel.sh`, logged on the gateway as `CONNECT
destination=<real IP>:443 actor_cert="CN=actor"`.

Timings (clean run): RunWorkload (fresh actor) boot_ms 1716 (snapshot_ms 189
downloading nothing, since none existed yet); CheckpointWorkload+upload
total_ms 443 (checkpoint_ms 107, upload_ms 136, VOLUMES fidelity, 13.9 KB);
RestoreWorkload (resume from own snapshot) boot_ms 1427, snapshot_ms 211;
RestoreWorkload (fork, from a tag-copied snapshot) boot_ms 1432, snapshot_ms
216. Ingress round trip (placement + proxied turn) 2.3-2.6 s per turn.

Decisions taken as directed:
- ateworker-gvisor stays a separate process (`bin/ateworker-gvisor`, built
  from the vendored tree unmodified), launched the stage-0 way: a private
  cgroup+mount namespace so `internal/ateworker/cgroup.Delegate` sees
  `0::/`, flags pointing at files `internal/devatelet.GeneratePKI` writes.
- Packages ate-ish's own code imports moved from `internal/` to `exported/`
  within the vendored tree (not a new API, the same move stage 1's VENDOR.md
  already flagged as coming): `proto/ateworkerpb`, `proto/ateletpb`,
  `ocispec`, `nodepath`, `netns`, `installdefaults`, `credbundle`,
  `substratex509` (the last four not in the original stage-1 list, needed
  once `devatelet`/`worker.go`/`nsdial.go` actually compiled against them).
  `internal/volume` stayed under `internal/`: nothing imports it. Recorded
  in VENDOR.md's "Packages moved out of internal/ (stage 2)" section.
  `ate-ish/internal/devatelet` (asked for at that path) now compiles there,
  since its own dependencies are exported too; it serves `WorkerSupport` for
  real now (AcquireWorkerEpoch, RegisterWorker, SetWorkerSandboxRuntimes,
  MintActorCertificate, and new this stage, GetActorEgressPolicy, reading
  the actor's real policy out of ate-ish's own bucket record) rather than
  logging and returning fixed stage-0 answers.
- runsc: `scripts/fetch-runsc.sh` (pinned release, gh or a direct GitHub
  release URL, staged at `bin/runsc/` in the full release layout — runsc's
  sidecars next to it, same requirement stage 0 found). Not yet wired into
  the Dockerfile; local-only this stage, since the gate is local.
- Store: `gs://ate-ish-v2-daniel-junk` (us-west1), created this stage.
  bucket.go's own token source needed a fallback: no Application Default
  Credentials file exists here (`gcloud auth application-default login`
  needs a browser this shell cannot drive), so a missing-ADC error falls
  back to `gcloud auth print-access-token` (the same source cli.go's own
  admin-API calls already used). Running the server also needs
  `sudo HOME=$HOME`, not bare `sudo`: sudo's own default HOME (root's) has
  no gcloud config, so the fallback token source fails too, silently,
  until HOME is preserved.
- Fidelity: `Template.PreferredFidelity` (new field; default, and
  pi-durable's own template, is VOLUMES). A fresh actor always
  RunWorkload-only regardless (no snapshot to restore from yet); Checkpoint
  and Restore both read the template's fidelity.
- Local-directory store: not attempted — GCS already covers the gate, so
  the "only if it fits under 100 lines" budget was not spent.

Launcher-path rewrite (worker.go, sandbox.go deleted, new bundle.go/
actors.go/atelet.go/nsdial.go): ActorDirs and the OCI bundle are built per
boot; the rootfs is the node's own filesystem, as an actual runsc-style
overlay (lowerdir `/` read-only, upperdir/workdir on a fresh tmpfs) rather
than the plain read-only bind mount V2.md offered as the simpler
alternative — tried that first, and setupAppRootfs's own rewrite of
`/etc/resolv.conf` inside the bundle needs a writable layer regardless of
how little else the node image needs changed. Both the overlay mount and
unmount run via `nsenter --mount=/proc/<workerpid>/ns/mnt`, not in-process:
measured two things that do not work first — mounting through
`/proc/<pid>/root/<path>` (works for open(), used for the actor's network
namespace below, but a mount(2) call still attaches in the caller's own
mount namespace) and in-process `setns(CLONE_NEWNS)` (the kernel refuses it
for any thread of a multithreaded process, which every Go binary is;
returns EINVAL). The actor's address is its veth address and the
template's container port, dialed through its gateway network namespace
(nsdial.go, open() through `/proc/<pid>/root/run/netns/...-at`, which does
work from outside, then `exported/netns.Dialer`); the ingress proxy and the
wakeup-probe poller both dial this way. The `-p` publish flag and the
low-port workaround are gone along with the old sandbox-launcher path.

Found and fixed along the way, beyond the mount-namespace ones above:
- gVisor starts a container with no PATH at all ("error finding executable
  ... in PATH []") — the sandbox's rootfs is the node's own filesystem, but
  gVisor does not consult the host's PATH or any image config for one, so
  ate-ish sets `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`
  itself (actors.go), overridable per-container by the template's own PATH
  env entry.
- atunnel's DNS relay fails every query closed ("Could not fetch the
  actor's egress policy") until `GetActorEgressPolicy` exists on the
  credential broker; unimplemented, not merely denying, so no hostname
  ever resolved, including the one stage 0 never needed (literal IPs only).
- Node 24 (not previously on this VM) installed at `/usr/local/node24`,
  symlinked onto `/usr/local/bin/{node,npm,npx}`: the actor's rootfs is
  this VM's own filesystem, so pi-durable's `node /actor/server-gateway.ts`
  needed a real `node` binary and `npm ci --omit=dev` (`@earendil-works/*`,
  90 packages) installed at `/actor` on the host, matching the template's
  command exactly.
- `pkill -f <path>` self-matches its own argv (same stage-1 finding,
  applied again here for the worker subprocess and this run's own `bin/
  ate-ish serve`); the `[x]` bracket idiom, or killing by PID when the
  pattern has no leading `/` to bracket, both used across these scripts.

Blockers: none; the gate is green. Carried forward for stage 3, named
where relevant above: credential replacement inside the gateway (currently
a pure passthrough, internal/devgateway); wiring runsc fetch into the
Dockerfile; the two egress-enforcement checks at the end of `gate.sh`
(`NODE_USE_ENV_PROXY=0` direct-fetch) test v1's HTTPS_PROXY-env-var
convention, which v2 does not use (egress is transparent nftables capture
now) — not run this stage, need rewriting for v2's model, not just a
passing/failing re-run.

Files changed: worker.go, main.go, records.go, bucket.go rewritten;
sandbox.go, egress.go's callers removed (egress.go itself kept, unreferenced,
for stage 3's credential-replacement gateway); new bundle.go, actors.go,
atelet.go, nsdial.go, internal/devgateway/gateway.go; internal/devatelet
extended (GetActorEgressPolicy, PKI path/cert accessors); third_party/substrate
six packages moved internal/ -> exported/, VENDOR.md updated; scripts/
fetch-runsc.sh; demo/template-gateway.yaml (port 8080 -> 80, preferredFidelity
added); bin/runsc/ staged, bin/ate-ish and bin/ateworker-gvisor built. GCS
bucket gs://ate-ish-v2-daniel-junk created. No commit made, per the brief.

## v2 stage 1 (2026-10-29): vendor substrate's worker

Brief (docs/V2.md stage 1): copy the named packages and whatever they
transitively import from go.ate.dev/substrate (commit
8767e8d62eebe7fe4c94d318b2ee30e8ed80ab82) into
third_party/substrate/, import paths rewritten only, LICENSE and VENDOR.md
added; vendor the stage-0 probe's fake atelet and PKI helper into
ate-ish's own code since stage 2 builds on it; add Go deps; no edits to
worker.go/sandbox.go. No commit.

Gate: PASS. `go build ./...` and `go vet ./...` clean. `go test` of the
vendored actornet and ateworker-gvisor (plus its cdiinject and cgroupstats
subpackages) suites: all four `ok`. Tests needing root and skipped (14,
all print "needs root: creates network namespaces" or "needs root:
mount/unmount", each naming `hack/run-root-tests.sh`): actornet's
TestSandboxSessionDialerConcurrentClose, TestSetupSandboxNetwork,
TestActorEgressIsFailClosedWithoutAtunnel,
TestIngressCrossesThePairWhileEgressIsCaptured,
TestSetupSandboxNetworkWithoutVeth, TestSetupSucceedsOverALeftoverNamespace,
TestActorUDPHasNowhereToGoBeyondTheNamespacePair,
TestCleanupClosesEachDescriptorOnce, TestSessionCloseWaitsForServingToStop,
TestSessionCloseReportsAWaitItCouldNotFinish,
TestSandboxNamespacesDoNotForward,
TestGatewayDoesNotRouteActorTrafficOverAStrayLink,
TestSandboxEgressReachesAtunnelOnAnyPort,
TestSandboxLinkLocalDoesNotReachAtunnel; ateworker-gvisor's
TestResetRunscStateAndPidFileDirs_DetachesMounts. Not re-run as root (the
gate only asked for the non-root run and a report of what it skipped).

60 upstream packages copied (320 .go files on the first pass, 187 after
pruning test files from the 58 packages the gate does not exercise; full
per-package list with line counts in third_party/substrate/VENDOR.md),
plus cmd/ateworker-gvisor/kodata/internal_flags.json (the runsc flag
config stage 0 found was required and correct, copied verbatim). go.mod
grew from 2 direct deps to the full transitive closure `go mod tidy`
resolved from the module proxy (same versions upstream's own graph picks):
k8s.io/apimachinery + client-go + controller-runtime + cel.dev (the
generated validators' dependency), the full OpenTelemetry SDK and
exporters (serverboot's tracing/metrics/logging init), aws-sdk-go-v2
(pkg/objectstorage's S3 backend), google-containerregistry, nftables,
vishvananda/netlink+netns, go-spiffe. go.mod 129 lines, go.sum 301 lines.

One patch, not to a vendored file: the import-path rewrite is line-anchored
(whole-line quoted imports only), not a blanket substring replace, because
three generated .pb.go files embed the pre-rewrite path inside a
length-prefixed protobuf descriptor byte literal — the first, broader
rewrite attempt corrupted those bytes and crashed every vendored binary at
package init with "slice bounds out of range [-1:]" in
google.golang.org/protobuf's desc_init.go. Fixed by re-copying clean and
redoing the rewrite with a line-anchored regex; verified by grep that the
only remaining "go.ate.dev/substrate" occurrences are the three descriptor
literals (cosmetic, upstream's own go_package option bytes, unrelated to
this module's import resolution), two inert `//go:generate`/doc-comment
mentions, and one real but harmless string constant
(`internal/actorevent.ScopeName`, an OTel scope label) — detail in
VENDOR.md.

Blocker found and resolved, documented in VENDOR.md's last section: the
coordinator's ate-ish/internal/devatelet location for the vendored fake
atelet/PKI helper does not compile. Go's internal-import rule only allows
code rooted under third_party/substrate/ to import
third_party/substrate/internal/* packages (credbundle, proto/ateletpb,
substratex509 — everything devatelet needs), and that boundary is the
import-path string itself, unrelated to which module owns the code.
Relocated to third_party/substrate/ateish/devatelet (the only
non-upstream directory under third_party/substrate/, clearly marked as
such in VENDOR.md) to satisfy the rule while staying visibly separate from
vendored content; exported generatePKI/serveFakeAtelet as
GeneratePKI/ServeFakeAtelet (nothing else behavioral changed) so another
package can call them. This same wall will block stage 2's worker.go and
sandbox.go the moment they import a vendored internal package directly
(ocispec, nodepath, ateworkerpb's request types) — flagged for that stage,
not solved here, since stage 1 does not touch worker.go/sandbox.go.

Files: third_party/substrate/ (new, 187 vendored .go files + LICENSE +
VENDOR.md + kodata/internal_flags.json; ateish/devatelet/ is ours, 2 files,
~340 lines, adapted from /tmp/substrate/cmd/ateish-probe/{pki,fakeatelet}.go);
go.mod, go.sum (deps added, nothing removed). No commit made, per the
brief.

## v2 stage 0 (2026-10-29): substrate's gVisor worker, local, no control plane

Brief (docs/V2.md stage 0): build ateworker-gvisor from the substrate clone,
fetch runsc, run the worker as root with a unix socket, drive RunWorkload by
hand, gate on HTTP reachability, egress confinement to the gateway, and
checkpoint/restore round-trip. No ate-ish code touched; everything lives
under /tmp/substrate (clone) and /tmp/ate-ish-probe (working dirs).

Gate: PASS, all four, on two independent runs.

1. `sudo ip netns exec <actor-uid>-at busybox wget http://169.254.17.2/`
   from the host answers 200 (through the actor's own gateway namespace,
   since the sandbox's veth lives in named netns the host's root netns does
   not route to).
2. No `egress_gateway` on the request: a raw HTTP/1.0 GET to 1.1.1.1:80 from
   inside the sandbox (`runsc exec`, busybox nc) gets zero bytes back
   (atunnel accepts the nftables-redirected TCP locally, then closes it:
   "like a network failure", per internal/atunnel/egress/egress.go). With
   `egress_gateway` set to the probe's fake CONNECT gateway, the same
   request gets a real `HTTP/1.1 301 Moved Permanently ... Server: cloudflare`
   back, round-tripped through the gateway's own dial to 1.1.1.1:80.
3. CheckpointWorkload (MEMORY fidelity) then RestoreWorkload: the durable-dir
   file written before checkpoint survives (`counter=1`, a plain host bind
   mount, untouched by the snapshot) and the in-memory counter continues
   from where it left off (1 before checkpoint, 2 after restore, not reset).
4. atunnel's CONNECT, captured by the probe's fake gateway:
   `CONNECT //1.1.1.1:80 HTTP/1.1`, header `User-Agent: Go-http-client/1.1`,
   TLS client certificate `subject="CN=actor" issuer="CN=ate-ish-probe node CA"`
   (no URI SAN; the worker's egress.Client mints this from the credential
   broker's CSR response, with no SPIFFE identity of its own).

Timings (two runs): RunWorkload without a gateway 80-103 ms; RunWorkload with
one 87-101 ms; CheckpointWorkload 118-133 ms; RestoreWorkload 156-191 ms.
runsc version: `release-20261019.0`, spec 1.2.1, platform systrap (default;
confirmed via `runsc flags`, no override needed). `-allow-connected-on-save`
is present in that release's `runsc flags` output and already wired into
production's `cmd/ateworker-gvisor/kodata/internal_flags.json` (the `start`
command's globalFlags) — copied verbatim via `KO_DATA_PATH`, not
hand-written.

Commands: `gh release download -R google/gvisor release-20261019.0 -p
'gvisor-x86_64.tar.zstd'`, extracted with `tar --zstd -xf`.
`cd /tmp/substrate && go build -o ateworker-gvisor ./cmd/ateworker-gvisor`
(no errors, no vendor changes). Driver: `go build -o ateish-probe
./cmd/ateish-probe`, a new package added under the substrate clone (not
ate-ish) so it could import substrate's own internal packages; it generates
its own PKI, the worker binary, and drives it.

What stage 0 needed beyond the files named in the brief, found by reading
error output rather than guessing:

- `internal/ateworker/cgroup.Delegate` requires `/proc/self/cgroup` to read
  `0::/`: a private cgroup namespace whose root is the caller's own cgroup
  at unshare(2) time, plus a fresh cgroup2 mount taken inside that namespace
  (otherwise the existing host mount still shows the full tree). Done with
  `CLONE_NEWCGROUP|CLONE_NEWNS` plus `mount --make-rprivate / && umount
  /sys/fs/cgroup && mount -t cgroup2 cgroup2 /sys/fs/cgroup` in the child,
  after moving the launching process into a fresh leaf
  (`/sys/fs/cgroup/ate-ish-probe-worker/cgroup.procs`). Needs cleanup between
  runs: cgroup's "no internal processes" rule refuses a second run's `echo
  $$ > cgroup.procs` once the first run enabled subtree_control and left a
  `worker` child leaf behind.
- Unexpected side effect of that same private mount namespace: actornet's
  named network namespaces (`/run/netns/<name>`, `<name>-at`) are bind-mounted
  from inside it, so they are invisible to the driver's own (host) mount
  namespace — `/run/netns/<name>` there is an empty regular file, and `ip
  netns exec` on it fails `EINVAL`. Fixed by `nsenter
  --mount=/proc/<workerpid>/ns/mnt -- ip netns exec <name> ...` before every
  host-side HTTP check.
- `validateRunscPath` requires `runsc_path` to live under
  `nodepath.ActorSandboxAssetsDir(actorUID)` (`/var/lib/ate/actors/<uid
  >/sandbox-assets`, not configurable — `nodepath.ActorsDir` is a package
  var atelet normally owns, the worker never takes a flag for it), and
  runsc's sentry sidecars (`gvisor_sentry` etc.) are looked up in a
  `gvisor-bin/` directory next to the `runsc` binary itself: staging only
  the `runsc` file (not the whole release layout) fails `runsc create` with
  "sidecar \"gvisor_sentry\" not usable: ... no such file or directory".
- `EgressGateway.address` needs a live actor TLS client certificate, minted
  by dialing `nodepath.WorkerSupportSocket` (atelet's credential broker, not
  reused per the brief) over mTLS matching `ateletdial.TLSConfig`: a SPIFFE
  URI SAN plus a JSON-encoded Pod-identity X.509 extension with matching
  NodeName/NodeUID on both the worker's own credential and the peer's. The
  probe stands in a ~90-line fake `WorkerSupportServer` (AcquireWorkerEpoch,
  RegisterWorker, SetWorkerSandboxRuntimes, MintActorCertificate) with its
  own throwaway CA for exactly this; `--dns-egress-policy=enforce-in-atunnel`
  is required (empty is rejected) but never exercised (the probe's addresses
  are literal IPs, no DNS lookups).
- `--readiness-listen-address` collided with an unrelated process already on
  :18080; the worker's health server treats a bind failure as fatal
  (`os.Exit(1)`) with no process-level indication beyond the log line, so
  the gRPC socket silently stopped accepting.
- A fully static Go binary (`CGO_ENABLED=0`) as the container's one process
  (serves `/counter` in-memory and `/volwrite`+`/volread` under a
  durable-dir bind mount) plus `/usr/bin/busybox` (already static on this
  VM) for ad hoc `runsc exec` checks; no docker image needed; the bundle
  rootfs is a plain directory (no `rootfs-overlay.json`), so composing it by
  hand is exactly what `imagecache.SetupBundleRootfs` does for an
  atelet-less caller.
- `pkill -f <path>` matches its own argv (the pattern string is literally
  present in the `pkill` process's own command line) and can SIGKILL itself
  before reaching the real target; fixed with the `[a]teworker-gvisor`
  bracket idiom in both ad hoc shell cleanup and the probe's own
  between-runs cleanup.
- Gate 2's egress check needed two iterations: `busybox wget` to
  `http://1.1.1.1/` follows 1.1.1.1's redirect to `:443` and then fails in
  busybox's own mbedtls stack ("TLS error from peer (alert code 20): bad
  MAC"), unrelated to the tunnel; and a bare TCP connect-only check (`nc` to
  port 443 with no payload) passes even with no egress gateway configured,
  because nftables REDIRECTs the SYN to atunnel's local listener either way
  — the distinguishing signal is whether real response bytes come back, not
  whether the handshake completes. Settled on a raw HTTP/1.0 GET to port 80
  over `busybox nc`, checked for an actual `HTTP/` response line.

Cleanup: `ip netns delete` on every leftover `worker-actor:*` namespace,
`pkill -9 -f '[a]teworker-gvisor'`; the probe's own graceful run leaves none
behind (confirmed: the final run's actor netns is absent from `ip netns
list` after its own TerminateWorkload). Files: the substrate clone keeps a
new, additive-only `cmd/ateish-probe/` package (pki.go, fakeatelet.go,
fakegateway.go, dirs.go, worker.go, netns.go, egress.go, main.go); nothing
under it is vendored or copied into ate-ish. No ate-ish source files were
touched, per the stage-0 rule.

Blockers: none; stage 0's gate is green. A note for stage 1 (vendoring): the
worker's own gRPC surface carries no TLS (plaintext over the unix socket)
and the CredentialBroker/atelet dependency only activates when
`egress_gateway` is set — a template with no egress gets a much shorter
path to RunWorkload.

## Run 4 (2026-10-29): the binary and CLI are ate-ish

Brief: rename the binary and command from `ate` to `ate-ish` (subcommands,
ATE_ env vars, ate-target-actor and resource names unchanged); rebuild and
push the five images; redeploy ate-node; gate on the Gateway variant.

- [x] rename: `go build -o bin/ate-ish`, /usr/local/bin/ate-ish in the base
  and the three coding-agents Dockerfiles, gate.sh, park.sh, journey.sh,
  usage text and error prefix in cli.go, main.go and sandbox.go, README.
  `bin/ate-ish nope` prints `ate-ish: unknown command "nope"` and the usage.
- [x] images, each with the same ate-ish binary (sha256 2a40dadf0668...):
  ate-ish@sha256:9888dc2baa55, pi-durable@sha256:0f91e316a00f,
  coding-agents-pi-durable@sha256:e74f09112bf5,
  coding-agents-libfx@sha256:f245d3776b82,
  coding-agents-cursor-agent-sdk@sha256:7611ced9ff99. The pi-durable rebuild
  pulled node:24-slim again: node v24.22.2 (was v24.22.1).
- [x] ate-node updated while running to pi-durable@sha256:0f91e316a00f
  (93 s, "Imported container image"). Worker log: sandboxes run
  `/usr/local/bin/ate-ish init`.
- gate (Gateway variant, AS=ate-demo-pi-rename): red, exit 5. The four setup
  commands passed; the first turn settled unanswered/model_error "Request
  timed out." after about 55 s. A second turn did the same. No `egress` line
  in the worker log, Gateway balance unchanged (13.852), and a direct curl
  to the Gateway with the same token answered 200 in 1.9 s.
- Cause: a sandbox can no longer reach its own container. Probe actor (node
  net.connect, 4 to 5 s timeout) from inside a sandbox: 169.254.8.1:8080,
  169.254.8.1:<egress port>, 169.254.9.1:8080, 169.254.169.1:8080,
  172.20.0.1:8080 all time out; 1.1.1.1:443 connects in 7 ms. The container
  holds 169.254.8.1/16 and 169.254.9.1/24 (from a debug image that printed
  its interfaces), and the egress listener on 169.254.8.1 opened without
  error. Same result on the previous image pi-durable@sha256:92dd34fed7d5
  (old binary, node v24.22.1) after a redeploy, so the rename did not cause
  it. The run 3 walkthroughs, committed at 00:56 UTC the same day, reached the
  proxy, so Cloud Run changed between 00:56 and 16:29 UTC (inferred). Every actor that needs
  egress through the proxy fails until the egress path changes; ingress to
  actors still works.
- [x] infra path on the final image with probe actors: create, resume
  (794 ms), tag, fork from the tag (542 ms), suspend, delete all passed.
- cleanup: probe and gate actors deleted, their atespaces removed from the
  bucket, debug image ate-ish-debug deleted. ate-node left running on
  pi-durable@sha256:0f91e316a00f.
- review: no independent reviewer ran.

## Run 3 (2026-10-29): the coding-agents-on-substrate demos on ate-ish

Brief: port 80 test; per-demo minimal delta under demo/coding-agents/<name>/;
run each demo's README walkthrough on its own instance ate-node-<demo>;
README section "Running the Agent Substrate demos"; push.

- [x] 1 low ports. Throwaway template on ate-node (atespace port-probe, two
  actors, deleted). The command ran as init's child, same uid and caps:
  uid 0, CapEff 0x20000420 (CAP_KILL, CAP_NET_BIND_SERVICE, CAP_AUDIT_WRITE).
  /proc/sys/net/ipv4/ip_unprivileged_port_start does not exist in gVisor's
  /proc ("No such file or directory"; a write: "Permission denied").
  node bind: 80 EACCES, 1023 EACCES, 1024 ok, 8080 ok, with the probe port
  published as 80 and as 8080. setcap/ambient caps not tried: the process
  already holds CAP_NET_BIND_SERVICE in its effective set. Result: no init
  change can bind 80. pi-durable and libfx get the one-line PORT change
  (sed in the Dockerfile); cursor-agent-sdk takes --port in the template.
- [x] ate on node:24-slim: the demo images have no /etc/ssl/certs, and ate
  failed TLS ("x509: certificate signed by unknown authority", local run of
  `ate get workers`). main.go imports golang.org/x/crypto/x509roots/fallback;
  after that the same run reached Cloud Run (401). ate-ish:latest rebuilt
  and pushed, sha256:c60f78838509 (was bb25d41b6733).
- [x] deploy.sh: INSTANCE (default ate-node), BUCKET defaults to
  $INSTANCE-$PROJECT, SECRET_URI, SECRET optional.
- [x] images: coding-agents-{pi-durable,libfx,cursor-agent-sdk}:latest in
  ate-ish (a24c30d4a6da, efa65555f5b5, ed27604ed580).
- [x] secret ai-gateway-bearer = "Bearer " + ai-gateway-token, because the
  libfx policy replaces authorization and its README stores "Bearer $KEY".
- [x] instances: ate-node-pi-durable (opencode-gemini-key), ate-node-libfx
  (ai-gateway-bearer), ate-node-cursor-agent-sdk (no secret). deploy.sh play
  42, 42 and 50 s each, run in parallel, bucket creation included. Each
  registered one worker (`ate get workers`).
- opencode-gemini-key: 401 "API key not valid" (one curl, 2026-10-29).
- [x] journey.sh (demo/coding-agents/journey.sh <demo>) runs a demo README
  walkthrough; outputs in runs/coding-agents-*.txt with the worker's
  started/suspended log lines appended.
- [x] pi-durable, Gemini image: every infra step ran; every turn settled
  unanswered/model_error 401 ACCESS_TOKEN_TYPE_UNSUPPORTED. A curl with the
  real key gives that reason, a curl with ate-placeholder-key gives
  API_KEY_INVALID, so the proxy injected the secret. Fork history: Turn 1
  prompt + its own. Then instances update to ate-ish/pi-durable:latest
  (102 s) and the full walkthrough on template-gateway.yaml: all turns done,
  entry 7, then 18 on both branches with divergent prompts.
- [x] libfx: full walkthrough passed; branches diverge at cursor 41; /events
  replays and stays open. Found: after a dropped /events stream the actor
  does not exit within 8 s of SIGTERM; suspend and delete fail ("actor did
  not exit 8s after SIGTERM") and the actor keeps its slot. Control: the
  same actor suspended in 1.5 s after /readyz. Probe (disconnect-probe/h1,
  node holding a response): no close event in 30 s after curl left; the
  request ended when the actor was deleted ~80 s later, Cloud Run logged
  "Truncated response body". Layer inferred, Cloud Run front end; not
  confirmed. Not fixed: h2c to the container or a forced stop would be the
  candidates, neither tried.
  README block run verbatim (bash -x, last curl with --max-time 5): every
  step through Turn 2 on demo-agent passed; the fork's turn got 503 "no
  worker has room" because the stuck fork from the journey run had the same
  key ate-demo-fx/demo-agent-fork (setup artifact: the atespace objects were
  moved while that copy still ran). The fork turn passed in journey.sh.
- [x] cursor-agent-sdk: placeholder key. Every infra step ran; every turn
  turn.failed "Invalid User API Key" 0.23 s after turn.started; requests
  reached api2.cursor.sh and api.cursor.com through the proxy. First
  journey run used the Turn 1 token for Turns 3 and 4 (409
  stale_continuation_token, script bug, fixed; the README uses the Turn 2
  token); runs/ holds the second run. Boot 2.79 to 2.98 s (n=7), cause not
  measured.
- [x] Gateway spend for both pi and libfx runs: $0.23 (credits 14.153 before,
  13.928 after); $0.30 with the verbatim libfx rerun (13.852 at the end).
- [x] README: "Running the Agent Substrate demos", Build your own actor
  (COPY ate into an existing image), Limits (no ports below 1024, request
  stays open after the client leaves, no forced stop), Cleanup line.
- [x] deleted: instances ate-node-pi-durable, ate-node-libfx,
  ate-node-cursor-agent-sdk (deploy.sh down play, 14 to 109 s), their
  buckets, secret ai-gateway-bearer. ate-node left running on
  pi-durable@sha256:92dd34fed7d5.
- review: no independent reviewer ran in this delegate session.

Resources after run 3:
- Running: instance ate-node (unchanged), bucket gs://ate-node-daniel-junk.
- Public images: ate-ish:latest (now sha256:c60f78838509, embedded roots),
  coding-agents-pi-durable, coding-agents-libfx, coding-agents-cursor-agent-sdk.

## Run 2 (2026-10-28): from proof of concept to public repo

Brief: single-template model, review fixes a-f, public images, BYO Gemini
key, gate both shapes, README, publish as taeold/ate-node.

- [x] 1 single-template model (commit; gated with step 2)
- [x] 2 review fixes a-f (commit; live checks of b and d pending the deploy)
  a real: Host header must name the CONNECT host (local check: without the
    fix a Host: www.google.com request reached Cloudflare; with it the proxy
    answers 403).
  b real: record write first, then the tag copy; a failed copy deletes the
    new record.
  c real: a failed stop, tar or upload keeps the sandbox and the running
    record and returns the error.
  d real: worker.release re-reads on 412, waits out 429, 5 tries. Ingress:
    a running record whose owner's live lease does not list the actor goes
    to placement, owner asked first. The proxied 409 itself cannot be
    retried once the body went to the worker, so the ingress reads the
    owner's lease. ate suspend on such a record marks it suspended.
  e real: a resume for a hosted copy waits for it, then refuses 503 if
    stopping, 409 if the record no longer names it (and stops that copy).
  f real: gate.sh asserts both fetch results from the bash tool output.
- [x] 3 images. Renamed mid-run to ate-ish (coordinator): public repo
  us-central1-docker.pkg.dev/daniel-junk/ate-ish (allUsers reader), images
  ate-ish@sha256:bb25d41b6733... and pi-durable@sha256:92dd34fed7d5...;
  anonymous manifest GETs answer 200. The first repo, ate-node, is deleted.
- [x] 4 model key: Gemini primary (demo/template.yaml), Gateway variant
  (demo/template-gateway.yaml) in the same pi-durable image; gates on Gateway
- [x] 5 gate: play (ate-node, us-west1) and scale + park, scale deleted.
  Final runs on pi-durable@sha256:92dd34fed7d5: runs/play.txt, runs/scale.txt,
  runs/park.txt, all exit 0. Scale: turn 1 on w-42db59ba, the fork restored
  on w-43e1e8bb. Park: p1..p8 200 in 1.6 to 4.4 s (p2 and p7 reached the
  ingress 2.7 s after the other six per the Cloud Run request log, then
  placed in 1.5 s); p9 parked 3.9 s, 200 at 5.2 s; p10 503 at 5.5 s.
  Boot: first sandbox on a fresh container 2.6 s (n=3, 2635/2641 ms, and
  2.97 s resume on play); later boots alone 0.74 to 0.95 s (n=6); four at
  once on one worker 2.33 to 2.36 s (n=4); suspend 0.46 to 0.55 s (n=8);
  snapshots 122 to 164 KB. Worker pool, ingress service and
  gs://ate-node-scale-daniel-junk deleted. Instance ate-node left running.
  play: green (runs/play.txt). Found: the instance had been stopped
  (StopInstance by taeold@gmail.com, 2026-10-27). `instances update --image`
  on a stopped instance changes the spec but does not import the image, and
  `start` then runs the last imported one (old code answered the probe
  template with no command). An update while running imported it (92 s).
  The first gate run on the old code is discarded; old atespaces moved to
  gs://ate-node-daniel-junk/archive/2026-10-28/.
  Live: b (create --tag on an existing actor: 409, its snapshot generation
  and md5 unchanged); d (record set to running on the live worker that did
  not host it: the request placed it on that worker, 200 in 3.5 s,
  generation 2 to 3).
  Boot (sandbox run to wakeup probe) 741 to 747 ms, n=3; first-request
  resume at the ingress 1018 to 1128 ms, n=3; suspend 463 to 482 ms.
- [x] 6 README; PROGRESS.md moved to docs/. demo/template*.yaml rebuilt
  from pi-durable-substrate's template.yaml (changes: image, command
  /actor/..., PORT and probe port 8080). The parsed fields match the gated
  file; one actor from the final gateway template booted on the instance
  (/readyz 200 in 1.06 s), atespace moved to archive/.
- [x] 7 commit as Daniel Lee, gh repo create taeold/ate-ish --public

Resources after run 2:
- Running: Cloud Run instance ate-node (us-west1, role all) on
  ate-ish/pi-durable@sha256:92dd34fed7d5, bucket gs://ate-node-daniel-junk
  (ate-demo-pi from runs/play.txt; earlier runs under archive/).
- Public: Artifact Registry repository ate-ish (us-central1).
- Deleted: Artifact Registry repository ate-node, worker pool
  ate-node-workers, service ate-node-ingress, gs://ate-node-scale-daniel-junk.

Notes:
- 2026-10-28: opencode-gemini-key still answers 401 "API key not valid"
  (one curl to v1beta/models). Gates run on the Gateway variant.

## Run 1 (2026-10-26)


Region us-west1, project daniel-junk. The project was called ate-node then.

## Decisions

- Language Go: one static binary is the server, the sandbox's pid-1 init and
  the CLI, and the MITM egress proxy needs X.509 minting, which Node core lacks.
- Gemini key `opencode-gemini-key` returns 401 (checked 2026-10-26 ~18:50 UTC).
  Turns use Vercel AI Gateway through pi-ai's `vercelAIGatewayProvider`.
  It sends the key as `x-api-key` (measured with a fetch hook), so the gateway
  policy replaces `x-api-key`. `anthropic/claude-haiku-5.5` is not in pi-ai
  1.0.4's catalog (turn settles `unanswered`, reason `no_model`);
  `anthropic/claude-haiku-4.5` works.

## Probe (throwaway instance ate-node-probe, deleted)

Sandbox network reach, curl from inside, host server on 0.0.0.0:9999:

| sandbox flags | host 169.254.8.1 / 172.20.0.1 | metadata 169.254.169.254 | example.com |
|---|---|---|---|
| `-p` only | refused (rc 7) | timeout | timeout |
| `--allow-egress -p` | 200 | timeout | 200 |
| neither | refused | refused | DNS fails |

A sandbox without `--allow-egress` cannot reach the host, so actors run with
egress allowed and HTTPS_PROXY pointing at the worker's per-actor listener.
Enforcement is by convention (spec's fallback).

## Built

Go program (bucket.go, records.go, ingress.go, worker.go, sandbox.go,
egress.go, cli.go, main.go), Dockerfile, deploy.sh (setup, image, play,
scale, url, down), gate.sh (README sequence), demo/ (templates, policies,
gateway agent). Bucket gs://ate-node-daniel-junk (us-west1).

## Found while building

- MITM proxy: Go's default transport speaks HTTP/2 upstream, and
  Response.Write then writes an `HTTP/2.0` status line to the actor's
  HTTP/1.1 connection (Node: `Connection error.`, curl hung until timeout).
  The upstream transport is HTTP/1.1 only (`TLSNextProto` empty map).
- An actor (egress allowed) reached `http://169.254.8.1:8080/admin/workers`
  on the instance, past Cloud Run IAM. The server now refuses requests from
  172.20.0.0/16; measured source of actor connections: 172.20.0.56.
- `instances update --image` took 94 s and 128 s. The old container got
  SIGTERM and suspended its actor in 475 ms (`suspend on shutdown`).
- gcloud `projects describe` hit a 429 on the shared gcloud client quota;
  deploy.sh looks the service account up only when it needs it.

- Scale shape, first parking run: eight concurrent wakes, three failed. GCS
  answered `429 ... exceeded the rate limit for object mutation operations`
  on `atespaces/park/actors/p5.json`. Cause: the ingress wrote the record
  (running, owner) before asking a worker, every worker it asked by a stale
  lease refused as full, and each refusal cost a write and a revert. Fix:
  the worker reserves a slot first and then claims the record with
  ifGenerationMatch; a full worker refuses with no write. Lease writes are
  coalesced to at most one a second.

## Status

- [x] program builds, local smoke, instance smoke
- [x] play gate on the final image's code path (runs/play.txt), exit 0
- [x] scale gate, ingress + 2 workers (runs/scale.txt), exit 0; turn 1 on one
      worker, the fork restored on the other
- [x] parking, 10 actors on 8 slots (runs/park.txt): 8 at once 200 in 10.5 s
      (fresh workers, image pull); p9 parked 4.3 s then 200; p10 503
      "no worker has room for the actor" after 5.4 s
- [x] scale deployment and its bucket deleted
- [x] README
- [x] review: the independent reviewer did not report before the run ended.
      Self-review of worker.go and ingress.go found one bug: a suspend that
      arrived while a boot was failing would close a nil egress server and
      clear the slot after the boot had freed it. suspend now returns
      errNotHosted when the boot failed. One comment fixed (call also maps
      409). Neither change was redeployed or gated; the running image is
      sha256:bb6d3e88...
- [x] commit

## Resources

- Cloud Run instance `ate-node` (us-west1), left running, role all
- Bucket gs://ate-node-daniel-junk (ate-demo-pi: demo-agent, demo-agent-fork, tag checkpoint-v1)
- Images ate-node:latest and ate-node:agent-gateway in
  us-central1-docker.pkg.dev/daniel-junk/cloud-run-source-deploy
- IAM: re-applied existing bindings (compute SA: secretAccessor on
  opencode-gemini-key and ai-gateway-token; objectAdmin on the buckets)
- Deleted: instance ate-node-probe and its image ate-node:probe, worker pool ate-node-workers, service
  ate-node-ingress, bucket gs://ate-node-scale-daniel-junk
