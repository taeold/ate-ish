# ate-node progress

## v2 stage 5f (2026-10-30): the subcontainer cgroup path, patched; `gate.sh play` green on real gen2

Brief (coordinator): get past the subcontainer cgroup wall, in order of
preference: (1) a non-vendored route, measured not guessed; (2) a
documented, one-line vendored patch if (1) is truly closed; (3) the real
gen2 deploy and gate.

**Route taken: 2 (documented patch).** Route 1 was conclusively closed by
reading the actual vendored source, not by assumption: `ate-ish` never
writes an actor's `config.json` itself (the worker generates it
internally, inside `ateworker-gvisor`, at `RunWorkload` time), so there
is no file of ours to strip `linux.cgroupsPath` from. Every vendored
`ocispec` file was read to confirm `ocispec.Build` never sets
`spec.Linux.CgroupsPath`; `exported/ocispec/gvisor.go`'s `ShapeGVisor` is
the only call site that ever does, unconditionally once the field reads
empty, and nothing in `ateworkerpb`'s `WorkloadSpec`/`ContainerSpec` (the
wire API ate-ish's own, non-vendored code controls) reaches that
assignment. The "match the sandbox's own cgroupsPath" idea was also
dead on arrival: `GVisorCgroupLeaf(ActorUID, ContainerName)` always
differs between `_sandbox` and an actor's own container name, which
ate-ish's own code cannot override to force a collision.

**Patch 1**, `third_party/substrate/exported/ocispec/gvisor.go`: a
package-level `noCgroupsPath = os.Getenv("ATE_ISH_NO_CGROUP_PATH") == "1"`
(read once), and `if spec.Linux.CgroupsPath == "" && !noCgroupsPath` in
`ShapeGVisor` — the only line touched. Documented in `VENDOR.md` as
"Patch 1," with the diff, the measured reason (stage 5d/5e/5f's own
findings, not restated here), and an upstream proposal: an explicit
`WorkloadSpec` or worker-flag opt-out for hosts whose cgroup v2 root
exposes no usable controllers, degrading limits/stats the same way
`--ignore-cgroups` already does for the sandbox cgroup, instead of
refusing the container outright. `atelet.go`'s `startAteworker` now sets
`ATE_ISH_NO_CGROUP_PATH=1` on the worker subprocess's own env whenever it
also sets `--ignore-cgroups` (the same `ignoreCgroups` bool, same
detection, same `ATE_IGNORE_CGROUPS=1` local-test override).

**Local verification, both green**, `ATE_IGNORE_CGROUPS=1` forced on a
host that has the controllers: single-node `gate.sh play` (full
sequence, suspend/tag/fork, both egress checks); a MEMORY-fidelity
variant of the pi-durable template (`preferredFidelity: MEMORY`, built
by copying `demo/template-gateway.yaml`) checkpointed and restored with
its conversation history intact (5 messages after one suspend/resume
cycle) — quick enough to run both locally and on gen2 (below), so not
left untested.

**Real gen2 deploy: green.** `gate.sh play` against `ate-ish-v2`: **exit
0, the first full pass on real Cloud Run gen2 this project** (every
prior round from stage 5 on stopped at a different point before this).
One unrelated snag surfaced only once execution got this far: the
pi-durable image's own base (`Dockerfile`) never installed `curl`, which
one of `gate.sh`'s own checks (added stage 3, always run with `node -e
fetch` for the DNS-refusal checks but a raw `curl` for the no-header-401
check) needs; every prior stage's local runs never noticed because
local `lowerdir=/` is this dev VM's own filesystem, which already has
`curl`. Added `curl` to `Dockerfile`'s apt list; redeployed; green.

Also ran the MEMORY-fidelity check again on gen2 directly (a separate
atespace, cleaned up before the final redeploy): turn 1 (2.1s),
suspend/checkpoint (12.3s), restore+turn 2 (1.6s, `ate-resume-ms: 762`),
5 messages carried through the cycle — confirmed working, not left
untested.

**Timings, gen2 vs. local** (this round's final, green gen2 run vs.
stage 2/3's local numbers, same `pi-durable` template, `VOLUMES`
fidelity): boot (`RunWorkload`, fresh actor) 1015 ms gen2 vs. 1335-1716
ms local; checkpoint 44 ms gen2 vs. 69-107 ms local; restore (own
snapshot) 1015 ms gen2 vs. 1427 ms local; restore (fork) 1014 ms gen2 vs.
1432 ms local — gen2 is at least as fast as local on every measured
step, not slower. Rootfs copy: 44.6-47.4 s / 578-588 MB across this
round's gen2 deploys (vs. local's 120 s / 8.9 GB in stage 5d, almost
entirely this dev VM's own bloat, not the mechanism). Full `gate.sh
play` wall time on gen2: 54-56 s.

**Resources left running: `ate-ish-v2` (green).** `ate-node` (v1)
untouched throughout.

Files changed: `third_party/substrate/exported/ocispec/gvisor.go`
(Patch 1, see above); `third_party/substrate/VENDOR.md` (Patch 1
documented); `atelet.go` (`ATE_ISH_NO_CGROUP_PATH` set alongside
`--ignore-cgroups`); `Dockerfile` (added `curl`); `deploy.sh` (`play`
targets gen2: `INSTANCE` defaults to `ate-ish-v2`, no
`--sandbox-launcher`, `--add-volume=type=in-memory` for the rootfs copy,
`ATE_SECRET_MAP`/`SECRET_URI` plumbing removed — the worker's own egress
gateway reads Secret Manager directly via ADC, never through an injected
env var, so that plumbing was never wired to anything real); `README.md`
(new limits bullet: no per-actor cgroup limits on gen2, why, and the
rootfs-copy mechanism/size). No commit made, per the brief.

## v2 stage 5e (2026-10-30): --ignore-cgroups fixes the sandbox; the subcontainer path does not honor it

Brief (coordinator): stage 5d's finding is a real platform fact (gen2 is
a hybrid cgroup host; memory/cpu are bound to v1 hierarchies this
container cannot reach, so the v2 mount it controls can only offer
hugetlb). Make the worker run without them: runsc's own
`--ignore-cgroups` global flag, staged into our own copy of
`kodata/internal_flags.json` (configuration we ship, not vendored
source), automatically when the host lacks memory/cpu.

**Part 1, local, `ATE_IGNORE_CGROUPS=1` forced on a host that has the
controllers: fully green.** `atelet.go` gained `hostLacksMemoryOrCPU`
(checks the worker leaf's parent's `cgroup.controllers` for `memory`/
`cpu`) and `stageKodata` (copies the vendored `internal_flags.json` into
`<ATE_DIR>/kodata`, adding `--ignore-cgroups` to `globalFlags` when
needed, then points `KO_DATA_PATH` at the staged copy — the vendored
tree under `third_party/substrate` is never written to). Single-node
`gate.sh play`: exit 0, full sequence including suspend/tag/fork
(checkpoint) and both forked-and-original turns (restore), both egress
checks. No `GetWorkloadStats`/memwatch errors; `"Actor usage sampled"`
logged with `ate.stats.source: unspecified` (a graceful degrade, not a
failure) instead of real cgroup-backed numbers. Stage 4's two-node
check (node A `ATE_CAPACITY=0`, node B under `unshare --mount`, both
`ATE_IGNORE_CGROUPS=1`): cross-node resume, history carried across two
turns, same as every prior round.

**Part 2, automatic detection: works exactly as specified.** On this VM
(has the controllers), nothing changes. On gen2, `startAteworker` logs
`"this host exposes no memory/cpu cgroup controllers to the container;
staging ateworker-gvisor with --ignore-cgroups"` on its own, after the
existing 5-second retry (stage 5d) confirms the shortfall is not
transient, and the vendored worker's own `runsc create` for the sandbox
(`_sandbox`) container succeeded — for the first time this project,
`cgroup delegation ready` no longer blocks anything downstream of it.

**Part 3, real gen2 deploy: progressed one more step, then a different
wall.** `--add-volume=type=in-memory,size-limit=2Gi` (ephemeral-disk
still unsupported, per stage 5d) at the same mount path; copy 47-48s /
578MB, consistent with stage 5d. `gate.sh play`'s first `/submit`
now fails with:
```
RunWorkload: ... while creating "pi-durable" application container: while running `runsc create`: exit status 128
```
Read via the kept `/admin/workerlog` diagnostic: the `_sandbox`
container's own `runsc create` (with `--ignore-cgroups` present in its
args) **succeeded this time** — confirmed by its absence from the error
log and the next step actually running. The failure is one level deeper:
the actor's own application container ("pi-durable", a gVisor
*subcontainer* within the already-running sandbox) fails with
```
configuring subcontainer cgroup: mandatory cgroup controller "memory" is missing for "/sys/fs/cgroup/<uid>-pi-durable"
```
despite `--ignore-cgroups` being present verbatim in that exact `runsc
create` invocation's own argv (confirmed by grepping the worker's own
log line for it). `--ignore-cgroups` suppresses the *sandbox's* own
cgroup setup but not a subcontainer's: traced to vendored
`exported/ocispec/gvisor.go`'s `if spec.Linux.CgroupsPath == ""` —
it unconditionally assigns a per-container `CgroupsPath` whenever the
OCI spec doesn't already carry one, and nothing in `ateworkerpb`'s
`WorkloadSpec`/`ContainerSpec` (what our own, non-vendored `actors.go`
controls) has a field to leave it unset or opt out. This is exactly the
brief's own stop condition ("if something in the vendored worker refuses
to proceed without an actor cgroup, record the exact error and stop
there"): fixing it would mean editing vendored `ocispec` or runsc's own
subcontainer cgroup-configuration path, not `atelet.go`/`bundle.go`.

**Gate result: not green, past a new point.** Sandbox-level cgroup setup
now works on gen2 for the first time; the per-container (subcontainer)
cgroup setup inside the sandbox does not, and `--ignore-cgroups` does
not reach it. No turn completed, so no boot/checkpoint/restore/turn
timing exists to compare against local beyond the copy time (47-48s on
gen2 vs. local's 120s/8.9GB — both consistent with stage 5d).

**Resources left running: none.** `ate-ish-v2` deleted (not green).
`ate-node` (v1) untouched. No local stray processes.

Files changed: `atelet.go` (`hostLacksMemoryOrCPU`, `stageKodata`, wired
into `startAteworker` with `ATE_IGNORE_CGROUPS` as the local-test
override). `bundle.go`/`worker.go` unchanged from stage 5d.
`Dockerfile`/`deploy.sh`/README not updated this round: the brief asked
for those once gen2's gate passes, which it does not yet. No commit
made, per the brief.

## v2 stage 5d (2026-10-30): rootfs overlay fixed; a hard cgroup-controller limit found one layer deeper

Brief (coordinator): fix the overlay-on-overlay depth problem (gen2's "/"
is itself overlayfs) by copying the node filesystem once per instance to
a real, non-overlay directory and using that as every actor's lowerdir;
verify locally with `ATE_ROOTFS_COPY=1`; deploy to gen2 with a volume at
the copy path; record timings; leave `ate-ish-v2` running if green.

**Part 1: the rootfs copy — works, confirmed on real gen2.** `bundle.go`
gained `rootIsOverlayfs` (statfs(2), `OVERLAYFS_SUPER_MAGIC`) and
`rootfsLowerDir`: on a plain filesystem (every local/VM run), returns "/"
unchanged; on overlayfs (or with `ATE_ROOTFS_COPY=1` forcing it locally),
tars "/" once per node instance into `<ATE_DIR>/rootfs` (excluding
`/proc /sys /dev /run /tmp`, `nodepath.BasePath`, and `ATE_DIR` itself —
the last two to avoid copying ate-ish's own data, including the
destination, into itself) and uses that copy as lowerdir from then on.
`mountBundleRootfs` takes the resolved lowerdir as a parameter instead of
a hardcoded "/"; the earlier `index=off,metacopy=off,redirect_dir=off`
mount options (stage 5c, unproven) are gone — moot once lowerdir is never
overlayfs again.

Local: `ATE_ROOTFS_COPY=1`, copy took 120s / 8.9GB (this dev VM's own "/"
is unusually large — docker/containerd state and the home directory
alone are ~50GB, excluded as container-runtime/dev-workspace storage
that is never part of a real actor's rootfs on any host, a harmless
no-op exclude on gen2 where those paths do not exist); `gate.sh play`
green (exit 0, full sequence, both egress checks), confirming
`setupAppRootfs`'s `/etc/resolv.conf` rewrite still works (the overlay
upper is still tmpfs, unchanged).

gen2: `--add-volume=type=ephemeral-disk` is **not supported for Cloud Run
Instances** ("spec.volumes: Ephemeral-disk volumes are not supported");
switched to `type=in-memory,size-limit=2Gi` as the brief's own fallback
anticipated. Copy took 42-49s / **578MB** (the real pi-durable image,
unlike this dev VM, has none of that bloat) — comfortably inside the 2Gi
volume. The overlay mount itself then succeeded on gen2 for the first
time this project: no more "wrong fs type, bad option, bad superblock on
overlay."

**Part 2: a new, different, hard blocker one layer deeper.** Past the
overlay fix, `runsc create` for the actor's sandbox failed:
```
cannot set up cgroup for root: configuring cgroup: mandatory cgroup controller "memory" is missing for "/sys/fs/cgroup/<uid>-_sandbox"
```
(seen as `"cpu"` missing on a later run too — see below). Added a
diagnostic `GET /admin/workerlog` route (kept; same bearer-token gate as
every other `/admin` route; answers "what did the worker actually log"
when Cloud Run's own log capture drops lines, which recurred here) and
read it directly: the vendored `cgroup.Delegate`'s own
`"cgroup delegation ready"` line showed `"controllers":["hugetlb"]` —
only one of the seven controllers runsc needs, with **zero** write
failures logged for the other six, meaning they were never even in
`cgroup.controllers`' available list to try, not refused.

First suspected a startup race (one gen2 deploy showed all seven,
"memory" missing; the very next, identical, deploy showed only one, with
no code change in between) and added a bounded retry
(`enableAvailableControllersWithRetry`, 20 attempts / 250ms apart / 5s
total, re-reading `cgroup.controllers` until the full set it wants
appears or the budget runs out) around ate-ish's own controller-enabling
step. Redeployed: the retry ran its full 5 seconds (confirmed in the log,
`attempt=0` through `attempt=19`) and **`have=[hugetlb]` the entire
time** — not a race that more waiting resolves. This reads as a hard
platform limit: Cloud Run gen2 exposes only the `hugetlb` cgroup
controller to this container's own cgroup v2 root; `cpuset cpu io memory
pids rdma` are simply not present in `cgroup.controllers` at any level
this process can reach, root included (`nearestDomainAncestor` returns
`/sys/fs/cgroup` itself here, since ate-ish is the container's PID 1 at
the real `0::/`) — not a permissions or ordering problem our own code
can work around, since there is no ancestor above the one we already
tried, inside this sandbox, that could have them to delegate down.

Also tried, then reverted before it could even be tested: enabling
controllers on the leaf itself (not just its parent) before the worker's
`CLONE_INTO_CGROUP` birth — reverted because it risks the same "no
internal process" conflict this project's whole leaf design exists to
avoid (a cgroup can't both be born into and have a non-empty
`subtree_control` already). Documented in `atelet.go` rather than
silently dropped, since it is the next thing to re-examine if this
constraint turns out to be partial rather than absolute.

**Not attempted this round, out of time:** the coordinator's named
fallback (`--overlay2=root:memory` via the worker's own staged
`internal_flags.json`, config not vendored source) addresses the
filesystem-overlay-depth problem, which is already fixed; it would not
by itself add the missing cgroup controllers, so it was not tried.

**Gate result: still not green.** The rootfs-copy fix is fully validated
(overlay mount succeeds on real gen2 for the first time); actor boot now
fails one step later, at `runsc create`'s own cgroup setup, on a cgroup
controller exposure limit this process has no ancestor cgroup available
to fix from inside the container.

Timings: gen2 rootfs copy 42-49s / 578MB (vs. 120s / 8.9GB locally, the
difference being almost entirely this dev VM's own bloat, not the
mechanism); no actor turn completed on gen2 this round, so no
boot/checkpoint/restore/turn timing exists to compare against local yet.

**Resources left running: none.** `ate-ish-v2` deleted (not green). No
local stray processes. `ate-node` (v1) untouched throughout.

Files changed: `bundle.go` (`rootIsOverlayfs`, `rootfsLowerDir`, `dirSize`,
`mountBundleRootfs` takes `lowerDir`); `worker.go` (`rootfsLower` field,
computed once in `newWorker`; new `GET /admin/workerlog` diagnostic
route, kept); `atelet.go` (`enableAvailableControllersWithRetry` replacing
the single-shot enable, `wantControllers`). `Dockerfile`/`deploy.sh` not
updated this round: the brief asked for this once gen2's gate passes,
which it does not yet. No commit made, per the brief.

## v2 stage 5c (2026-10-30): cgroup topology fixed; a second, different gen2 blocker found at actor boot

Brief (coordinator): fix stage 5b's cgroup-drain root cause in atelet.go
(not vendored code), verify locally (single-node `gate.sh play`, then
stage 4's two-node check), then the real stage 5 deploy and `gate.sh play`
against `ate-ish-v2` on gen2.

**Part 1 — identify the stray (one gen2 cycle).** A throwaway diagnostic
(reverted after; never committed) moved ate-ish into a test leaf exactly
as the old code did and dumped `/sys/fs/cgroup/<leaf>/cgroup.procs` plus
each PID's `cmdline`/`Name`, and `/proc/self/cgroup` of both ate-ish and
the bash wrapper. On gen2: ate-ish itself runs as the container's PID 1,
and its own `/proc/self/cgroup` is `0::/` — the real root. The leaf's
`cgroup.procs` held exactly one entry: PID 1, `/usr/local/bin/ate-ish
serve`. This matches the brief's candidate #1 exactly (`moveSelfIntoLeafCgroup`
puts ate-ish's own process in the leaf the worker inherits as its cgroup
namespace root) and is consistent with stage 5b's strace (ate-ish's real
PID moves out cleanly on the first pass; something else, reading back as
PID "0", blocks the remaining 99).

**Part 2 — the fix, in atelet.go only (no vendored file touched).**
Replaced "move ate-ish's own PID into the worker's leaf" with: find
ate-ish's own current cgroup (`/proc/self/cgroup`'s `0::` entry); walk up
to the nearest ancestor whose `cgroup.type` is plain `domain` (a cgroup
namespace can only root at a domain cgroup — this VM's own dev-harness
cgroup turned out to be `domain threaded`, which makes every direct child
`domain invalid`; clone3 refuses to use those outright, "operation not
supported" — found by hitting it locally, not anticipated); create a
leaf as a *child* of that ancestor, enabling available controllers in the
ancestor's own `subtree_control` (best-effort, one at a time, matching
the vendored `Delegate`'s own style); launch the worker with
`SysProcAttr{Cloneflags: CLONE_NEWCGROUP|CLONE_NEWNS, UseCgroupFD: true,
CgroupFD: <leaf dir fd>}` (clone3's `CLONE_INTO_CGROUP`, kernel 5.7+) so
it is born directly into that leaf — nothing else, ever, is a member.
`removeWorkerCgroupLeaf` runs on graceful shutdown (`worker.suspendAll`).
Also found and fixed along the way: ate-ish's own *default* `/sys/fs/cgroup`
view on gen2 has no `cgroup.controllers` file at all ("no such file or
directory") until explicitly remounted — unlike every local/VM run, where
the ambient mount is already fully functional. Fixed by running the same
lazy-umount-then-remount dance stage 5 already uses for the worker's own
script, once more, for ate-ish's own process, before reading any cgroup
file.

**Local verification, both green:**
- Single node: fresh `ATE_CAPACITY=4` instance, `cgroup delegation ready`
  logs all seven controllers (`cpuset cpu io memory hugetlb pids rdma`,
  zero failures — contrast stage 5c's own first, buggy attempt, which
  logged four `ENOTSUP` warnings from reading the wrong ancestor), then
  `ATE_URL=http://127.0.0.1:8080 AS=ate-5c-single ./gate.sh`: exit 0, full
  sequence including both egress-enforcement checks.
- Two nodes (stage 4's shape: node A `ATE_CAPACITY=0` to force the
  cross-node path, node B under `unshare --mount` with its own
  `/var/lib/ate`): actor created and run via node B directly, suspended,
  then a second turn submitted through node A's own ingress — resumed on
  node B (`Ate-Worker: w-59731482`), history carried (9 messages across
  two turns). Confirms stage 4's cross-node placement and the new cgroup
  leaf scheme compose correctly (each node still gets its own leaf, under
  its own ambient cgroup, independent of the other).
- Graceful shutdown removed both leaves; confirmed by listing
  `/sys/fs/cgroup/system.slice/claude-remote.service/` after both
  processes stopped (empty of `ate-ish-worker-*` entries).

**Part 3 — the real stage 5 deploy.** Rebuilt `Dockerfile` (unchanged
from stage 5) and `demo/pi-durable/Dockerfile`'s image, deployed
`ate-ish-v2` (`us-west1`, 2 vCPU/4GiB, `ATE_ROLE=all`, no
`--sandbox-launcher`). The cgroup fix is confirmed working on real gen2:
`ate-ish` (PID 1) now logs `cgroup delegation ready` immediately, the
worker registers, and the Cloud Run startup probe succeeds on the first
attempt (contrast every stage-5/5b attempt, which never got this far).
`ate-ish create` and the first `/submit` reached the worker's `/resume`
endpoint and got as far as booting the actor's sandbox — then hit a
**second, different, gen2-only blocker**, found only once the cgroup fix
let execution reach this far:
```
mounting bundle rootfs: mounting overlay rootfs at .../bundle/pi-durable/rootfs: exit status 32
(mount: .../rootfs: wrong fs type, bad option, bad superblock on overlay, missing codepage or helper program, or other error.)
```
`bundle.go`'s `mountBundleRootfs` overlays the actor's rootfs with
`lowerdir=/` — on every local/VM run so far, "/" is a plain ext4
filesystem; on Cloud Run gen2, "/" is itself the container runtime's own
overlayfs mount, and overlay-on-overlay nesting is a well-known source of
kernel-version- and feature-dependent `mount(2)` refusals. Tried
`index=off,metacopy=off,redirect_dir=off` (drops overlay feature
negotiation that often trips on a non-uniform lower layer) — verified
harmless locally (ext4 lowerdir, same successful mount, same actor turn
completing end to end), then redeployed to gen2: **identical error**, so
this was not the specific mismatch. Not further root-caused: ran out of
this round's budget past the point of safely attempting another blind
mount-option guess at the cost of another full image-build-deploy cycle
each time, the same reasoning stage 5 used to stop rather than keep
guessing. The `index=off,metacopy=off,redirect_dir=off` addition is kept
in `bundle.go` (harmless, same successful mount locally) as a real step
toward whatever the actual fix turns out to be, not reverted.

**Gate result: cgroup fix confirmed (atelet/worker boot, registration,
and the ingress-to-worker `/resume` request path all succeed on real
gen2 for the first time this project). `./gate.sh play` against
`ate-ish-v2`'s URL still fails, now at actor boot's overlay mount, not at
worker startup. Local gates (single-node, two-node) both green.**

Timings: gen2 image import ~4-5s (base) warm, instance start ~10s,
first log line ~15-20s after create; the full failing `gate.sh` run to
its first FAILED step: 6.2-6.5s (just the four admin calls plus the one
`/submit` that reaches the overlay-mount failure). No successful turn
timing to compare against local yet, since no turn completed.

**Resources left running: none.** `ate-ish-v2` deleted (gate.sh did not
pass, so there is no working deployment to leave up — redeploying for a
next attempt is one more build-push-create cycle, not a teardown-and-setup
from scratch, given the images are already in Artifact Registry).
`ate-node` (v1) untouched throughout. No local stray processes.

Files changed: `atelet.go` (`cgroupLeafFor` two-arg, `currentCgroupPath`,
`nearestDomainAncestor`, `prepareWorkerCgroupLeaf`/`cleanupStaleLeaf`/
`removeWorkerCgroupLeaf`, the ate-ish-side remount-before-reading-cgroup-files
fix, `UseCgroupFD`/`CgroupFD` on the worker's `SysProcAttr`); `worker.go`
(threads the returned leaf path into the `worker` struct, removes it on
shutdown); `bundle.go` (overlay mount options, kept though unproven on
gen2 yet); `Dockerfile`/`.dockerignore` unchanged from stage 5.
`demo/probe-template.yaml` still present from stage 5, untouched. No
commit made, per the brief.

## v2 stage 5b (2026-10-30): root cause found — cgroup delegation never drains on gen2

Brief (coordinator): one diagnostic round on stage 5's silent `exit 1,
zero bytes` finding, cheapest first — local repro under
`capsh --drop=cap_mknod`, then under gen2's exact `CapEff`, then (if
neither reproduces) an `strace`-wrapped, file-redirected (not Go pipes)
launch on one real gen2 cycle. Timebox 90 minutes, no vendored-file edits.

**Root cause found, with the exact log line and strace evidence:**
`cgroup.Delegate` (vendored, `internal/ateworker/cgroup`) tries to drain
`/sys/fs/cgroup`'s own `cgroup.procs` into a child `worker` cgroup by
repeatedly writing `"0"` (move self) to
`/sys/fs/cgroup/worker/cgroup.procs`, polling up to 100 times for the
parent to empty out. On gen2, every single write succeeds (`write(3, "0",
1) = 1`, confirmed over 100 consecutive attempts in the strace capture —
not one `EPERM`/`EACCES`/`ENOENT`) but the parent never drains, so it
gives up and logs, verbatim:
```
{"level":"ERROR","msg":"Error while executing","err":"while setting up cgroup delegation: while moving worker processes into \"/sys/fs/cgroup/worker\": \"/sys/fs/cgroup/cgroup.procs\" did not drain after 100 iterations"}
```
exits 1 about 3.5s after its first log line. This rules out every
syscall-denial theory stage 5 flagged as the headline risk (seccomp
filter, missing capability): nothing is refused at the syscall level:
the writes succeed, the binary just never sees the drain condition it
polls for, which reads as a cgroup *topology* difference — gen2 most
likely keeps another process (its own supervisor/sidecar) pinned in the
same cgroup node this code assumes it fully owns, so the parent's
`cgroup.procs` can never go empty no matter how many times this process
moves itself.

**Why stage 5 saw zero bytes instead of this message:** reproduced
locally once the real cause was suspected to be a capture problem, not a
worker problem — redirecting the worker's stdout/stderr to files
*inside the launch script itself* (`> /tmp/worker.out 2> /tmp/worker.err`,
bypassing Go's `cmd.Stdout`/`cmd.Stderr` pipe plumbing entirely) made the
exact same gen2 instance print this message every time, where stage 5's
Go-side `io.MultiWriter(logFile, os.Stdout)` tee reliably produced
nothing for this same failure. Not fully root-caused *why* the pipe path
drops it (most likely: the process's writes to the pipe's write end race
its own `exit_group(1)` closely enough, combined with gen2's own log
sidecar reading container stdout, that the dying process's last writes
never reach `gcloud logging read`'s backing store before the container
is torn down) — but the fix (redirect to files, not pipes, when
debugging a process that might die fast) is now confirmed, not guessed.

**Step 1, local reproduction attempts (both negative, both informative):**
- `capsh --drop=cap_mknod --` wrapping the real `atelet.go` launch
  (patched in locally, reverted after, never committed): worker started
  and registered normally (`"worker up" ... capacity=1`). MKNOD alone
  does not reproduce the failure.
- The *exact* gen2 `CapEff` mask `0x1fff7fcffff` decoded (missing three
  capabilities, not the one the original brief named: `cap_mknod`,
  `cap_sys_module`, `cap_sys_rawio`) and dropped together via
  `capsh --drop=cap_mknod,cap_sys_module,cap_sys_rawio --`: worker again
  started and registered normally. The full gen2 capability set, exactly
  matched, does not reproduce the failure either — consistent with the
  strace finding that no syscall was ever refused.
- Hybrid cgroup v1+v2 layout: not attempted with confidence. This VM's
  own host cgroup setup is pure v2 (`cat /proc/self/cgroup` →
  `0::/system.slice/...`; `/proc/mounts` shows one `cgroup2` mount, no v1
  controllers at all), so synthesizing an accurate hybrid layout to test
  against would have been a guess at gen2's real internals, not a
  measurement — skipped once the `--caps=` match above still passed,
  and skipped entirely once the file-redirect fix (below) found the real
  cause with higher confidence than a low-fidelity local hybrid-cgroup
  mockup would have given.

**Step 2 (confirm capture) and 3 (one gen2 cycle):** built a throwaway,
never-committed diagnostic (`strace_probe.go` + a `main.go` hook behind
`ATE_STRACE_PROBE=1`, both reverted after; `strace` added to the
Dockerfile's apt list, also reverted after): starts an HTTP server
immediately (so gen2's own startup probe passes regardless of whether the
worker launch succeeds), runs `ateworker-gvisor --version` first, then
the real launch under
`strace -f -tt -e trace=process,execve,openat,mount,unshare,setns,prctl,capset,write`
with stdout/stderr redirected to files by the script itself, and serves
all four files back over `/debug/version.out|worker.out|worker.err|worker.strace`.
Verified working locally first (`--version` RC=0; the full launch
registered normally, confirming the harness itself has no bug), then one
`gcloud beta run instances create ate-ish-v2-probe` cycle (`us-west1`, 2
vCPU/4GiB, `ATE_STRACE_PROBE=1`, no other env needed): `--version` RC=0
on gen2 too (rules out "the Go runtime or pflag can't even run here");
the real launch's `worker.out`/`worker.err` carried the message quoted
above; the last strace lines before exit (`/tmp/gen2-strace.txt`,
not committed) are the ~30 repeated
`openat(.../worker/cgroup.procs, O_WRONLY|O_CREAT|O_TRUNC|O_CLOEXEC) = 3`
/ `write(3, "0", 1) = 1` pairs immediately preceding the error log line
and `exit_group(1)`.

**Timings:** `--version` + full strace-wrapped launch together took
~3.6s wall on the gen2 instance from first log line to `exit_group`;
image build-to-first-log-line for this round's single gen2 cycle was
about 35-40s (import) + 15s (first log line), one cycle total, well
inside the 90-minute budget (local repro + harness build/verify + this
one cycle: approximately 55 minutes).

**Resources left running: none.** `ate-ish-v2-probe` deleted again; its
`:strace-probe` image tag deleted from Artifact Registry. All local
diagnostic processes, `/tmp` scratch files, and the two
repro-specific cgroup leaves (`ate-ish-worker-ate-ish-repro{,2}`) cleaned
up. `ate-node` (v1) untouched throughout.

**Files changed, net of this round:** `docs/PROGRESS.md` only. Every
diagnostic edit used to find the root cause (`main.go`'s `ATE_STRACE_PROBE`
hook, the new `strace_probe.go`, the `atelet.go` `capsh` wrapping used for
the two local repro attempts, the Dockerfile's temporary `strace` package)
was reverted once its answer was in hand — none of it has lasting value
the way stage 5's `umount -l` fallback and `cmd.Wait()` logging did, so it
was removed rather than left as inert diagnostic cruft. `atelet.go` keeps
only stage 5's two permanent changes (lazy-unmount fallback, exit-status
logging), unchanged by this round. No commit made, per the brief. The
actual fix for the drain-timeout (the next round's work, not this one)
still needs to either patch the vendored `cgroup.Delegate` call site (a
documented `VENDOR.md` patch) or find an atelet-side workaround that
avoids relying on that drain check at all — not attempted here, out of
scope for a diagnostic-only round.

## v2 stage 5 (2026-10-30): Cloud Run gen2 probe — stopped on a fundamental finding

Brief (docs/V2.md stage 5, coordinator's follow-up): one gen2 Cloud Run
instance, role all, same binary. "Measure before you build": probe first
with a throwaway instance before committing to the full pi-durable image.
Timebox three hours; stop and record verbatim if something fundamental
(runsc under the seccomp filter, nftables in the gateway netns) fails.

**Result: stopped at the probe, on a fundamental finding, per the brief's
own explicit permission to do so. No real deploy attempted (`ate-ish-v2`
was never created). `ate-node` (v1) and its bucket untouched.**

What was built for the probe: the root `Dockerfile` (previously v1-only:
just the `ate-ish` binary) now also builds the vendored `ateworker-gvisor`
from source and stages `bin/runsc` and its `kodata`, plus
`nftables`/`iproute2`/`util-linux`/`python3` in the final Debian image —
the same layout the real deploy needs, reused as the probe image directly
(no separate probe binary written; ate-ish's own HTTP admin/ingress API
already "reports over HTTP", so driving the deployed probe instance with
`curl`/`ate-ish create ...` exactly like `gate.sh` does was the fastest
way to exercise runsc+cgroup+netns+checkpoint end to end). Deployed as a
throwaway `gcloud beta run instances create ate-ish-v2-probe` (`us-west1`,
2 vCPU/4GiB, `ATE_ROLE=all`, no `--sandbox-launcher`, the v2 bucket,
no secrets needed for this probe). The compute service account was
granted `roles/storage.objectAdmin` on `gs://ate-ish-v2-daniel-junk`
(missing going in; `ai-gateway-token`'s `secretAccessor` binding already
existed from v1).

**Finding, in order, each confirmed from `gcloud logging read` on the
live instance, verbatim:**

1. First failure (original, unmodified atelet.go script): the instance
   never got past 30s, repeatedly:
   ```
   ate-ish: waiting for ateworker-gvisor's socket: socket "/var/lib/ate/workers/ate-ish-<uid>/worker.sock" did not appear within 30s
   Container called exit(1).
   Default STARTUP TCP probe failed 1 time consecutively for container "worker" on port 8080. The instance was not started.
   ```
   `cmd.Start()` for the `bash -c "mount --make-rprivate / && umount /sys/fs/cgroup && mount -t cgroup2 ... && exec ateworker-gvisor ..."` script was succeeding (no error returned to Go), but nothing further ever happened — ateworker-gvisor's own log file (previously the only place its output went, per stage 0-4) stayed empty. Diagnosed by teeing `ateworker-gvisor`'s stdout/stderr to this process's own stdout/stderr too (Cloud Run only captures the container's own fds, not an arbitrary file on disk) and by logging `cmd.Wait()`'s result (`atelet.go`), since a silently-killed process is otherwise indistinguishable from a slow one.
2. With the tee in place: the real error surfaced immediately —
   ```
   umount: /sys/fs/cgroup: target is busy.
   ATELET_STEP=wait-returned err=exit status 1 state=exit status 1
   ```
   Every prior environment (this VM, locally, across stages 0-4) had
   `/sys/fs/cgroup` freely unmountable inside the private mount namespace
   `mount --make-rprivate /` set up; gen2's own container runtime holds a
   live reference to it (an additional bind mount or an open fd from the
   platform's own process supervision), so a plain `umount` is refused
   with `EBUSY`. This is a genuine environment difference, not a seccomp
   or capability denial.
3. Fixed with `umount -l` (lazy unmount: detach now, the mount goes away
   once nothing still references it) as a fallback when the plain
   `umount` fails — confirmed by the next run reaching every subsequent
   step: `rprivate-done rc=0`, `umount-busy-retrying-lazy`,
   `umount-done rc=0`, `cgroup2-done rc=0`, `exec-start`. This part of the
   atelet.go script is kept (see "Files changed" below) since the lazy
   fallback is strictly safer than the old unconditional `umount` even on
   a VM, not a Cloud-Run-only patch.
4. **The fundamental finding**, past the cgroup2 remount: `ateworker-gvisor`
   itself, execed with the exact argv that works on this VM (stages 0-4),
   exits with status 1 and produces **zero** bytes of output on either
   stdout or stderr — not even its own first startup log line
   (`"initializing preview gates"`, confirmed present as the literal first
   line of every successful local run's log file, e.g.
   `/var/lib/ate-ish-a/ateworker-gvisor.log` from this same session's
   stage 4 testing). Isolated by replacing the script's final `exec
   ateworker-gvisor ...` with a plain (non-exec) invocation followed by an
   explicit `echo rc=$?`, to rule out the exit code being bash's own (a
   bad `exec` target reports differently and prints its own message).
   Verbatim:
   ```
   ATELET_STEP=exec-start
   ATELET_STEP=binary-exited rc=1
   ATELET_STEP=wait-returned err=<nil> state=exit status 0
   ```
   (`state=exit status 0` there is the *bash* process's own exit, which
   is 0 because the script's last command, the `echo`, succeeded — `rc=1`
   is `ateworker-gvisor`'s own exit code, captured by `$?` in the line
   right after it ran.) A clean, intentional `exit(1)` is not what a
   seccomp `SECCOMP_RET_KILL`/`RET_TRAP` produces (those terminate by
   signal — Go's `exec.ExitError` would read `signal: bad system call`,
   and bash would report exit code 128+31, not a bare 1) — so this reads
   as the binary choosing to exit, before it reaches its own first log
   line, for a reason not yet identified: not the cgroup2 remount (fixed,
   confirmed reached), not a missing argument (identical argv to the
   working local runs), and not a signal-based kill (rules out the
   obvious seccomp-filter-blocks-a-syscall story the brief flagged as the
   headline risk). Candidates not yet distinguished, given the probe
   budget ran out: an early self-reexec/unshare step inside
   `ateworker-gvisor`'s own `main()` (common for binaries that enter a new
   namespace before their own logger is set up) landing in a child process
   whose stderr does not inherit the same fd, or a sandboxed syscall gen2
   permits for `clone()`/`mount()` (used above, successfully) but denies
   for something `ateworker-gvisor` tries immediately after — e.g.
   `prctl(PR_SET_NO_NEW_PRIVS)` or a capability probe for `CAP_MKNOD`
   (confirmed missing from gen2's `CapEff` per the coordinator's brief)
   that the vendored binary's own startup path checks eagerly rather than
   deferring to the point it is actually needed.

**Why this stops the stage rather than getting one more fix**: the brief
budgeted "under an hour" for the probe and three hours total; reaching
this point (cgroup2 remount working, the binary itself failing opaquely)
took the whole probe budget across five build/push/deploy/log-read
cycles (~35-45s container-import plus ~20-40s to first log line, each
round). Guessing at the real cause blind (no strace available in the
image, no shell access to the running gen2 instance, and each hypothesis
costs one more full image-build-and-deploy cycle to test) is exactly what
the brief's stop condition exists to prevent. Per the brief: "stop after
recording it with the exact errors and the probe's full output; that
result is a finding, not a failure of the stage."

**Not attempted, per the above:** the real pi-durable image build,
`ate-ish-v2` deploy, `./gate.sh play` against a Cloud Run URL, and
`deploy.sh`'s update to v2 image/flags — all gated on the probe passing,
which it did not.

**Resources left running: none.** `ate-ish-v2-probe` instance deleted;
its image tag (`us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:v2`)
deleted from Artifact Registry. `ate-node` (v1) and its bucket were never
touched. The IAM binding granted to the compute service account
(`roles/storage.objectAdmin` on `gs://ate-ish-v2-daniel-junk`) was left in
place (needed for any future attempt at this stage; harmless, no
resources behind it incur cost).

Timings: image build ~2.5 min (first, cold Go module cache) down to
~15-20s (cached layers) per iteration; `gcloud beta run instances create`
~45-70s to "successfully created"; first log line after that, 15-20s.
Total wall time for the probe loop (five iterations, each a full
build+push+delete+create+log-read cycle): approximately 50 minutes.

Files changed: `Dockerfile` (builds and stages `ateworker-gvisor`/
`bin/runsc`/`kodata`; adds `nftables`/`iproute2`/`util-linux`/`python3`
to the final image — needed for the real deploy regardless of this
stage's outcome, so kept); `.dockerignore` (`bin/runsc` no longer
excluded, only the two binaries that get rebuilt fresh); `atelet.go`
(the `umount -l` fallback, kept; the `cmd.Wait()` exit-status logging to
both the log file and this process's own stdout/stderr, kept — both are
genuinely useful beyond this one debugging session: the lazy-unmount
fallback is strictly safer everywhere, and silent-subprocess-death
visibility has no cost and would have caught this immediately on a
future, different silent failure); `demo/probe-template.yaml` (new; the
minimal `python3 -m http.server 80` actor template used to drive the
probe instance — left in the tree, costs nothing, useful for any retry).
No commit made, per the brief.

## v2 stage 4 (2026-10-30): two nodes, one bucket

Brief (docs/V2.md stage 4): two ate-ish processes on this VM, different
ports/node ids/worker dirs/lease addresses, one bucket, either node
accepts ingress. Gate: an actor created and run through node A, suspended,
resumed through node B and actually running there with its history; a
capacity-1-per-node park/503 test with three actors; `./gate.sh play`
still green against node A alone.

Gate: PASS, all three parts.

(1) Cross-node resume. `ate-ish create actor cross-node ...`, turn 1
through node A's ingress (port 8080) — ran on node A (`w-0ab34a93`),
`settled.status: done`. `ate-ish suspend actor cross-node`. Node A then
relaunched with `ATE_CAPACITY=0` (forces every placement elsewhere, the
deterministic way to prove node B specifically, since both nodes'
fewest-hosted tie would otherwise pick either). Turn 2 submitted to node
A's own ingress (port 8080, the one node A never stopped accepting on):
`Ate-Worker: w-22845e0a` (node B), node B's own log shows `[fake-atelet]
MintActorCertificate` + `actor started ... generation=4` + `egress
... allowed=true`, all minted and served by node B's own, independent PKI
and gateway — never node A's. The resumed actor's history carried on (14
messages across the two turns, same conversation). Timings: resume_ms
2612, total request 5364 ms (includes the real model turn).

(2) Capacity-1 park/503. Both nodes relaunched at `ATE_CAPACITY=1`. Three
fresh actors (a1, a2, a3) created, then three concurrent `/submit` calls
fired at node A's ingress. Result: a2 ran on node A (`w-4d371b1e`), a3 ran
on node B (`w-6beb5257`) — one per node — and a1 parked for the full 5 s
placement budget then got exactly `503` with body `actor
ate-stage4/a1: no worker has room for the actor` (`errNoRoom`'s own
message, verbatim). Recorded which: **parks and 503s**, not "served after
one finishes" — `place()`'s 5 s budget expired before either slot freed
(neither turn had finished suspending).

(3) `./gate.sh play` alone. Both multi-node processes stopped, node A
relaunched solo at `ATE_CAPACITY=4` (its ordinary single-node shape).
`ATE_URL=http://127.0.0.1:8080 AS=ate-gate-stage4 ./gate.sh` and a second
run with a different `AS`: both exit 0, including the create/turn/
suspend/tag/fork/divergent-turns sequence and the three egress-enforcement
assertions (unlisted-host refusal with and without the proxy env var, the
listed-host-no-header 401). Unaffected by anything stage 4 changed, as
expected — single-node is still the default shape of every port/dir/cgroup
parameterization below.

What assumed one node, and the fix for each (none touched any vendored
file):

- **ateworker-gvisor's own control ports.** `worker.go` hardcoded
  `--readiness-listen-address`, `--atunnel-listen-address`,
  `--atunnel-connect-listen-address` to 28080/28443/28444 — these bind on
  the *host* network namespace (unlike the actor sandboxes, which get their
  own netns from `internal/ateworker/actornet`), so a second node's
  subprocess would fail to bind the instant it tried. Fixed: `worker.go`
  now reads `ATE_READY_PORT`/`ATE_ATUNNEL_PORT`/`ATE_CONNECT_PORT`
  (defaulting to the old hardcoded values, so single-node callers see no
  change), one disjoint triple per node.
- **The stale-process pkill pattern.** `atelet.go`'s `startAteworker`
  cleaned up a leftover `ateworker-gvisor` from an unclean previous run by
  `pkill -f` matching the bare binary path — identical for every node,
  since every node runs the same binary at the same relative path. Node
  B's own startup would `pkill -9` node A's *live* worker subprocess out
  from under it the moment it started. Found by reading the code, not by
  reproducing the kill blind: confirmed it was real by checking that the
  pattern string (`bin/ateworker-gvisor`) has no node-specific component
  anywhere in `ateworkerConfig`. Fixed: the kill pattern now matches
  `cfg.pki.Path(WorkerCredentialBundleFile)`, which embeds that node's own
  `ATE_DIR` — unique per node, stable across one node's own restarts (so
  same-node stale cleanup still works), never matching another node's
  process.
- **The cgroup leaf.** `moveSelfIntoLeafCgroup`/`cleanupLeafCgroup` used a
  single fixed path, `/sys/fs/cgroup/ate-ish-worker` — real cgroupfs, not
  namespaced away from the ate-ish *parent* process (only the forked
  `ateworker-gvisor` gets `CLONE_NEWCGROUP`). A second node's startup would
  call `cleanupLeafCgroup` against the first node's still-populated leaf
  (removal silently ignored, since errors there were already swallowed)
  and then move its own PID into that shared leaf, racing both nodes for
  the same "/worker" child `cgroup.Delegate` creates — this surfaces
  either as an outright startup error (cgroup v2 refuses adding a process
  to a non-leaf cgroup once it has children) or silent cross-node resource
  accounting. Fixed: `cgroupLeafFor(nodeTag)` suffixes the path with
  `filepath.Base(ATE_DIR)`, so each node gets its own leaf
  (`ate-ish-worker-ate-ish-a`, `ate-ish-worker-ate-ish-b` this run).
- **`nodepath.BasePath` (`/var/lib/ate`), a vendored `const`.** Everything
  the vendored worker and `internal/devatelet`'s fake atelet pin to a
  fixed host path hangs off this one constant — most critically
  `WorkerSupportSocket` (`/var/lib/ate/worker-support.sock`, no per-node
  component at all): a second node's `ServeFakeAtelet` would
  `os.RemoveAll` and re-`Listen` on the exact path the first node's own
  worker subprocess dials for every `MintActorCertificate`/
  `GetActorEgressPolicy` call, silently rewiring node A's atunnel to node
  B's atelet on next reconnect. Cannot be fixed in Go without patching a
  vendored file (disallowed; `BasePath` is a `const`, not a flag). Fixed
  outside the Go tree instead, in the launch invocation: node B runs
  inside `unshare --mount --propagation private`, with
  `mount --bind /var/lib/ate-b /var/lib/ate` performed first thing inside
  that new namespace, before `exec bin/ate-ish serve`. Every vendored
  path rooted at `/var/lib/ate` (the socket, `ActorsDir`, `WorkersDir`,
  the OTLP relay socket) now resolves against a private directory for
  node B, invisible to node A and vice versa; the forked
  `ateworker-gvisor` subprocess inherits the same remapped view, since its
  own `CLONE_NEWNS` namespace starts as a copy of whatever node B's parent
  already sees. Node A keeps the host's real `/var/lib/ate`, unchanged —
  this is purely additive for additional nodes.
  - **Side effect this surfaced, fixed in Go:** `bundle.go`'s `stageRunsc`
    hardlinked the runsc release into the actor's `sandbox-assets` dir.
    `link(2)` refuses across a bind-mount boundary with `EXDEV` even when
    both paths resolve to the same underlying device (confirmed: `stat`
    showed identical `st_dev` on both sides; the kernel's `do_linkat`
    compares `vfsmount` identity, not device number) — node B's first
    actor boot failed with `linking "bin/runsc/containerd-shim-runsc-v1":
    ... invalid cross-device link`. Fixed with a `copyFile` fallback used
    only on `EXDEV`, so the single-node, same-mount case still gets the
    cheap hardlink unchanged.
- **Placement and the lease address.** Checked, not changed:
  `ingress.go`'s `place()` already asks every live lease with room,
  fewest-hosted first, and `Lease.Addr` (worker.go's `writeLease`) was
  already the flag-supplied `addr`, not a hardcoded host — this part of
  V2.md's stage 4 ask ("a lease carries the address other nodes reach this
  node on... any node accepts ingress") was already true going into this
  stage, nothing to fix. Verified by the gate itself: node A's ingress
  correctly proxied to node B's `127.0.0.1:8081` lease address in test (1).

**Where the node CA key lives, and why it did not need to move to the
bucket:** each node's `devatelet.GeneratePKI` still generates its own,
fresh, node-local CA (unchanged from stage 2/3; lives only in that node's
own `ATE_DIR/pki`, never written to the bucket). This turned out to be
sufficient, not a gap: an actor's client certificate for atunnel is
minted fresh on *every* boot (`RunWorkload`/`RestoreWorkload`), by
whichever node is booting it, from that node's own atelet
(`MintActorCertificate`, `internal/devatelet/fakeatelet.go`) — and that
same node's own egress gateway is the only gateway address the boot
request names (`egressGateway{Address: w.gateway.Addr}` in `worker.go`'s
`boot`), trusting its own node CA (`startEgressGateway`'s `nodeCAPEM`
argument). An actor's certificate therefore only ever has to be accepted
by the gateway that minted it; it never crosses a node boundary. Verified
directly in test (1)'s log: node B minted `cross-node`'s certificate and
accepted it at its own gateway, with no reference to node A's CA at all.
If a future stage needs a certificate to outlive the node that minted it
(e.g. an in-flight TCP connection surviving a resume onto a different
node, which this design does not attempt — a resume always re-boots and
re-mints), *that* is when the CA key would need to move to the bucket;
stage 4's gate does not exercise that case, so the brief's "simplest: CA
key in the bucket" option was not built.

Timings: see gate (1)-(3) above; cross-node resume total 5.4 s wall
(resume_ms 2612, the rest the actual model turn through the real AI
Gateway).

Commands (abbreviated; full session used explicit PIDs for restarts
between capacity changes):
```
# Node A
sudo HOME=$HOME ATE_BUCKET=ate-ish-v2-daniel-junk ATE_ROLE=all \
  ATE_DIR=/var/lib/ate-ish-a PORT=8080 ATE_CAPACITY=4 bin/ate-ish serve

# Node B: private /var/lib/ate so the vendored WorkerSupportSocket path
# (and every other nodepath.BasePath-rooted path) does not collide with
# node A's.
sudo HOME=$HOME unshare --mount --propagation private -- bash -c '
  mkdir -p /var/lib/ate /var/lib/ate-b
  mount --bind /var/lib/ate-b /var/lib/ate
  export ATE_BUCKET=ate-ish-v2-daniel-junk ATE_ROLE=all \
    ATE_DIR=/var/lib/ate-ish-b PORT=8081 ATE_CAPACITY=4
  export ATE_READY_PORT=29080 ATE_ATUNNEL_PORT=29443 ATE_CONNECT_PORT=29444
  exec bin/ate-ish serve
'

# Gate (1): create+turn1 via node A, suspend, turn2 via node A's own
# ingress with node A at ATE_CAPACITY=0 (forces the cross-node path).
curl -X POST http://127.0.0.1:8080/submit -H 'ate-target-actor: AS/cross-node' ...

# Gate (2): both nodes at ATE_CAPACITY=1, three actors, three concurrent
# POSTs at node A's ingress (see run_one() in the session transcript).

# Gate (3), node A alone, default capacity:
ATE_URL=http://127.0.0.1:8080 AS=ate-gate-stage4 ./gate.sh
```

Files changed: `worker.go` (configurable atunnel/ready/connect ports,
`nodeTag` threaded into `ateworkerConfig`); `atelet.go`
(`cgroupLeafFor`/parameterized `moveSelfIntoLeafCgroup`/
`cleanupLeafCgroup`, node-scoped pkill pattern); `bundle.go` (`stageRunsc`
EXDEV fallback to `copyFile`). No vendored file touched. No bucket records
left running: every actor created this stage (`cross-node`, `a1`, `a2`,
`a3`, and `gate.sh`'s own `demo-agent`/`demo-agent-fork` under
`ate-gate-stage4`/`ate-gate-stage4-2`) suspended before shutdown; both
node processes stopped cleanly (`suspendAll`, lease deleted); no stray
`ateworker-gvisor` or `runsc` processes left (checked before and after).
No commit made, per the brief.

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
