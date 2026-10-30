# Vendored from agent-substrate/substrate

Upstream: https://github.com/agent-substrate/substrate
Commit: `8767e8d62eebe7fe4c94d318b2ee30e8ed80ab82` (2026-10-27 16:46:07 +0000)
License: Apache-2.0, `LICENSE` in this directory, copied unmodified.

Everything under this directory is copied from the upstream commit above
with only import paths rewritten (`go.ate.dev/substrate/...` ->
`github.com/taeold/ate-ish/third_party/substrate/...`, and, for six
packages, also moved from `internal/` to `exported/` — see "Packages moved
out of internal/" below). No other line was touched in a vendored file.

The rewrite only replaces whole-line quoted import paths (one import per
line, as `gofmt` produces), not arbitrary substring occurrences. Three
generated `.pb.go` files embed the pre-rewrite path inside their raw
protobuf descriptor byte literals (`api/ateapi/v1/ateapi.pb.go`,
`internal/proto/ateletpb/atelet.pb.go`, `internal/proto/ateworkerpb/ateworker.pb.go`)
and were deliberately left alone there: the embedded bytes are a
length-prefixed protobuf wire encoding, so substituting a same-looking but
different-length string inside it corrupts the message (confirmed: the
initial, broader rewrite caused `desc_init.go`'s `unmarshalSeed` to panic
with `slice bounds out of range [-1:]` at package `init()`). Those three
occurrences describe the *upstream* Go package path for proto-reflection
tooling; they do not affect this module's own import resolution, and are
cosmetic only (not patches, so not listed below). Two other occurrences
are inert: `//go:generate` comments in `api/infra/k8s/ate/v1/gen.go`
naming code-generator flags, and the doc comment
`+k8s:validation-gen-input=go.ate.dev/substrate/internal/proto/ateworkerpb`
in `internal/ateworkervalidation/doc.go`. One is a real but harmless string
constant: `internal/actorevent/actorevent.go`'s
`ScopeName = "go.ate.dev/substrate/internal/actorevent"`, an OpenTelemetry
instrumentation-scope label with no effect on behavior; left as upstream
wrote it, since "rewrite only the import paths" does not cover it.

Test files (`*_test.go`) are vendored only for the two packages stage 1's
gate exercises, `cmd/ateworker-gvisor` (and its own subpackages
`internal/cdiinject`, `internal/cgroupstats`) and
`internal/ateworker/actornet`; every other vendored package carries
production code only. Carrying every package's tests would have pulled in
several more upstream-internal packages used only by tests nothing here
runs (`internal/testenv`, `api/credprovider/v1`, `api/objectstore/v1`,
`internal/proto/ateenv/v1alpha`, `internal/proto/glutton`,
`internal/proto/grpcechopb`, `internal/proto/objectstoreexchangepb`,
`pkg/proto/ateconfigpb`), none of which this codebase otherwise needs.

`go.mod`/`go.sum` gained whatever these packages import transitively, via
`go mod tidy` (same versions upstream's own `go.mod` resolves to, since
the module proxy serves one answer for the same packages at the same
commit's dependency graph).

## Patch 1 (stage 5f, 2026-10-30): `exported/ocispec/gvisor.go`

**File:** `exported/ocispec/gvisor.go`

**Diff** (the only change to any vendored file in this tree):

```diff
 import (
+	"os"
 	"path/filepath"
 
 	"github.com/opencontainers/runtime-spec/specs-go"
 	"github.com/taeold/ate-ish/third_party/substrate/exported/proto/ateworkerpb"
 	"github.com/taeold/ate-ish/third_party/substrate/internal/sizing"
 )
 
+// noCgroupsPath is ate-ish's own patch: on a host whose cgroup v2 root
+// exposes no memory/cpu controller to this container at all, runsc's own
+// subcontainer cgroup setup fails even with --ignore-cgroups set, because
+// it unconditionally tries to configure whatever spec.Linux.CgroupsPath
+// names below. Read once at package init.
+var noCgroupsPath = os.Getenv("ATE_ISH_NO_CGROUP_PATH") == "1"
+
 ... (ShapeGVisor, unchanged above this line) ...
 	// Set a colon-free default cgroupsPath relative to the pod scope.
-	if spec.Linux.CgroupsPath == "" {
+	if spec.Linux.CgroupsPath == "" && !noCgroupsPath {
 		spec.Linux.CgroupsPath = "/" + GVisorCgroupLeaf(o.ActorUID, o.ContainerName)
 	}
```

**Reason:** hosts whose cgroup v2 root exposes no memory/cpu controller to
the container at all — Cloud Run gen2 is the one found so far: it is a
hybrid cgroup host that binds memory and cpu to v1 hierarchies this
process cannot reach from inside the container, so the v2 mount it does
control can only offer `hugetlb` (stage 5d/5e's own measurement, not a
guess: a 5-second, 20-attempt retry confirmed this is not transient).
`ateworker-gvisor`'s own `--ignore-cgroups` global flag (staged into
`kodata/internal_flags.json`, not this file — stage 5e) suppresses the
*sandbox*'s own cgroup setup, but a `runsc create` for an actor's
*application subcontainer* still unconditionally tries to configure the
cgroup at `spec.Linux.CgroupsPath` (measured: `--ignore-cgroups` present
verbatim in that exact invocation's own argv, confirmed by reading the
worker's own log, and the failure happening anyway — `"configuring
subcontainer cgroup: mandatory cgroup controller \"memory\" is
missing"`). `exported/ocispec/gvisor.go`'s `ShapeGVisor` is the only call
site that ever writes `spec.Linux.CgroupsPath` (confirmed by reading
every vendored `ocispec` file: `ocispec.Build` never sets it, so it is
always empty going into `ShapeGVisor`), and nothing in `ateworkerpb`'s
`WorkloadSpec`/`ContainerSpec` — the wire-level API ate-ish's own,
non-vendored code controls — has a field that reaches this assignment.
That ruled out a non-vendored fix (ate-ish's stage 5f own first attempt,
per the brief's ordering): ate-ish never writes the container's
`config.json` itself (the worker generates it internally, inside
`ateworker-gvisor`, at `RunWorkload` time), so there is no file of ours
to strip the field from either.

**Upstream proposal:** an explicit opt-out on `WorkloadSpec` (or a
worker-wide flag, matching `--ignore-cgroups`'s own scope) to run actor
subcontainers without a per-container cgroup when the host's cgroup v2
root does not expose the controllers a sandbox's resource limits need —
degrading per-actor cpu/memory/pids accounting and limits to "none" on
such hosts, the same way `--ignore-cgroups` already documents doing for
the sandbox cgroup itself, rather than refusing to start the container at
all.

**Drift check:** compares every vendored file against the pinned upstream
commit except this one; `exported/ocispec/gvisor.go` is diffed against
the block above instead.

## Packages copied (non-test unless noted), line counts of their own `*.go` files

- `api/ateapi/v1` 13688 (proto types some ateworkerpb messages reference)
- `api/infra/k8s/ate/v1` 1160 (`SandboxClassGvisor` etc., used by `cmd/ateworker-gvisor`'s registration)
- `cmd/ateworker-gvisor` 7424, with tests
- `cmd/ateworker-gvisor/internal/cdiinject` 1131, with tests
- `cmd/ateworker-gvisor/internal/cgroupstats` 485, with tests
- `internal/actorevent` 380
- `internal/actorlock` 231
- `internal/actorlog` 256
- `internal/apierror` 144
- `internal/ateattr` 529
- `internal/ateinterceptors` 218
- `internal/ateletdial` 97
- `internal/atenet` 40
- `internal/ateworker` 97
- `internal/ateworker/actornet` 1749, with tests (the gate's other target)
- `internal/ateworker/cgroup` 499
- `internal/ateworker/phaselog` 108
- `internal/ateworker/pid1` 114
- `internal/ateworker/register` 718
- `internal/ateworkervalidation` 2689 (generated k8s-style field validators)
- `internal/atunnel` 468
- `internal/atunnel/dns` 1594
- `internal/atunnel/dns/freemap` 221
- `internal/atunnel/dns/protocol` 221
- `internal/atunnel/egress` 1810
- `internal/atunnel/ingress` 590
- `internal/cdi` 125
- `internal/contextlogging` 184
- `internal/credbundle` 222
- `internal/egresspolicy` 583
- `internal/hardware` 782
- `internal/imagecache` 3374
- `internal/installdefaults` 136
- `internal/linuxsys` 131
- `internal/netns` 449
- `internal/nodepath` 130
- `internal/ocispec` 460
- `internal/otlprelay` 668
- `internal/pidfd` 157
- `internal/preview` 368
- `internal/principal` 46
- `internal/proto/ateletpb` 5668 (generated)
- `internal/proto/ateworkerpb` 2802 (generated)
- `internal/protoredact` 230
- `internal/resources` 1321
- `internal/roottest` 33 (root-test skip/gate helper the two gated test suites use)
- `internal/sandboxruntimes` 112
- `internal/serverboot` 1018
- `internal/sizing` 98
- `internal/snapshotcache` 172
- `internal/substratex509` 207
- `internal/version` 62
- `internal/volume` 211
- `internal/volumepath` 55
- `internal/wakeupprobe` 189
- `internal/workercapacity` 23
- `internal/workerstats` 441
- `internal/workerstats/workerstatstest` 72 (test helper package)
- `pkg/objectstorage` 2574
- `pkg/sandboxclass` 40

`cmd/ateworker-gvisor/kodata/internal_flags.json` is also copied verbatim:
the per-runsc-command global/subcommand flags (`-allow-connected-on-save`,
`--cpu-num-from-quota`, etc.) the worker reads via `KO_DATA_PATH` or a
relative `internal_flags.json`, proven in stage 0 to be the complete,
correct set for this release of runsc.

60 upstream packages total (the number above minus `kodata`, which is data,
not a Go package).

## Packages moved out of internal/ (stage 2)

Stage 1 found that Go's internal-import rule blocks any code outside
`third_party/substrate/` from importing its vendored `internal/...`
packages (the boundary is the import-path string itself, not which module
owns the code), which would have blocked `worker.go`/`sandbox.go` and the
fake-atelet/PKI helper (then at `third_party/substrate/ateish/devatelet`)
the moment either imported a vendored internal package directly.

Stage 2's fix, directed by the coordinator: move each package ate-ish's own
code needs out from under `internal/` to `exported/` (one path element
renamed, same as the `go.ate.dev/substrate` rewrite — a full-line import
match, never touching the embedded descriptor bytes), and nothing else.
Moved: `internal/proto/ateworkerpb` -> `exported/proto/ateworkerpb`,
`internal/proto/ateletpb` -> `exported/proto/ateletpb`,
`internal/ocispec` -> `exported/ocispec`,
`internal/nodepath` -> `exported/nodepath`,
`internal/credbundle` -> `exported/credbundle`,
`internal/substratex509` -> `exported/substratex509` (the last two because
the fake-atelet/PKI helper, now real ate-ish code, needs them). Every other
vendored package stays under `internal/`, including `internal/volume`,
which nothing in this tree or in ate-ish actually imports (vendored at
stage 1 per the brief's package list; not moved since "if used" did not
apply).

With `proto/ateletpb`, `credbundle`, and `substratex509` now reachable from
outside `third_party/substrate/`, the former `third_party/substrate/ateish/devatelet`
moved to `ate-ish/internal/devatelet`, its originally-asked-for location:
ate-ish's own code, not vendored, now importable from the module root.
