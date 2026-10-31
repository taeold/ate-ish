# Cleanup pass (2026-10-31)

What changed when the project moved from a stage-by-stage build (V2.md's
stages 0-8) to the command surface docs/WHAT.md decided.

## Deleted

- `dev` subcommand (`dev.go`) and everything that only served it: the
  in-container `docker build` convenience (and the static Docker CLI
  staged in the root `Dockerfile` for that convenience), the `scripts/`
  directory name (its one file moved, see below).
- `deploy` subcommand (`deploy.go`), `deploy.sh`.
- `serve` as a separate verb: the bare command already runs a node, and
  nothing in `hack/gate.sh`/`hack/park.sh` called `serve` as a distinct
  name.
- Scaffolding env vars, each replaced by the automatic detection that
  already existed alongside it: `ATE_ROOTFS=host` (bundle.go's image pull
  now covers the rootfs choice once a template names a real image),
  `ATE_ROOTFS_COPY` (was a force-override of `rootIsOverlayfs()`'s own
  detection), `ATE_IGNORE_CGROUPS` (same pattern, `hostLacksMemoryOrCPU`
  decides it), `ATE_SECRET_MAP` (the native Secret Manager and `env`
  credential forms already cover every live demo once
  `demo/pi-durable/egress-policy.yaml` moved off the old
  `ate-secret://k8s.io/...` form).
- `demo/coding-agents/` (the three vendored copies of
  coding-agents-on-substrate's ports): replaced by
  `docs/CODING-AGENTS-PORTS.md`, a pointer at the upstream repo with the
  one-line delta each port needed. No source copied.
- Stage-era parallel demo files (`demo/template.yaml`,
  `demo/template-gateway.yaml`, `demo/egress-policy.yaml`,
  `demo/egress-policy-gateway.yaml`, `demo/probe-template.yaml`): folded
  into the one canonical `demo/pi-durable/`.
- `runs/` as a top-level directory name (contents moved, see below).

## Moved

- `main.go` and the 13 other root `.go` files split into
  `cmd/ate-ish/main.go` and `internal/{store,records,apierr,egress,
  ingress,atelet,cli,envutil}`, grouped by what the code already does
  rather than one file per package. `internal/atelet` holds
  `atelet.go`, `cgrouphelper.go`, `nsdial.go`, `worker.go`, `bundle.go`,
  `actors.go`, and a new `runsc.go` (the runsc-fetch logic ported out of
  the deleted `dev.go`) as one package: they are one tightly coupled
  subsystem, and splitting further would have meant exporting internals
  with no structural benefit.
- `gate.sh`, `park.sh`, `scripts/fetch-runsc.sh` to `hack/`.
- `runs/*.txt` to `docs/log/`, since they are real recorded transcripts
  the README and docs/measurements.md cite, not scratch output.
- Everything that was in the old README but is not part of the new
  10-section surface: measurements and timing tables to
  `docs/measurements.md`, demo/deploy walkthroughs and narrative to
  `docs/demos.md`.

## Added

- `cmd/ate-ish/main.go` as the one entrypoint.
- `internal/apierr` and `internal/envutil`: a few shared lines
  (`reply()`/sentinel errors, the one `env()` lookup) that crossed what
  are now separate packages; neither existed as a named package before.
- `demo/counter`, ported from substrate's own counter demo, to prove
  `preferredFidelity: MEMORY` parity.
- `LICENSE` at the repo root (Apache-2.0, matching
  `third_party/substrate/LICENSE`'s text).
- `docs/CODING-AGENTS-PORTS.md`, `docs/measurements.md`, `docs/demos.md`.

## Unchanged

- Everything under `third_party/substrate/`: zero vendored lines
  touched, confirmed via `git status --short third_party` throughout.
  `third_party/substrate/VENDOR.md` needed no update; no vendored path
  moved.
- `docs/WHAT.md`, `docs/V2.md`, `docs/PROGRESS.md`: kept as the fixed
  product spec and the historical log they already were.
- `ATE_ISH_NO_CGROUP_PATH` (Patch 1's own env var) and
  `ATE_ROOTFS_EXPORT`/`ATE_ROOTFS_EXPORT_REPO` (a legitimate force-the-
  no-daemon-export-path test knob, independent of the deleted `dev`):
  neither was scaffolding for a stage, so neither was removed.

## Reader-facing surface now

One binary (`cmd/ate-ish`), bare invocation runs a node (role all, local
directory or `ATE_BUCKET`, `ATE_LISTEN` for the port), `ate-ish <verb>`
against `--endpoint`/`ATE_URL` for the kubectl-ate verbs. No `dev`, no
`deploy`, no `serve`. README.md covers it in the 10 sections celld's
outline suggested: opener, how it works, what runs on it, install, run
it, operate a fleet, Cloud Run, compatibility, limits, contributing and
license. `go build ./...`, `go vet ./...`, and `gofmt -l .` (excluding
`third_party`) are all clean against this tree.

Gate results (hack/gate.sh and the other README-verbatim checks) are
tracked separately; see the final report for what ran and what didn't.
