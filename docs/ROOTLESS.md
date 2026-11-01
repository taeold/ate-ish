# Rootless worker: build spec

Decision (2026-11-01): replace Agent Substrate's vendored gVisor worker with
ate-ish's own rootless driver of gVisor's runsc. ate-ish then runs as an
ordinary user. The design is the one proven in the rootless spike (branch
`rootless-spike`, docs/ROOTLESS-SPIKE.md rounds 1 to 3, code under
spike/rootless/r2 and r3): no sandbox network, per-destination host Unix
sockets for egress, an in-sandbox init and forwarders, a dial-out ingress
forwarder, a gateway that identifies the actor by socket, per-actor limits
from systemd's user scopes, one namespace-root runsc mode for run,
checkpoint and restore.

Parity with substrate's actor contract is the bar. Compatibility is now
proven by the gates below, not inherited.

## Shape

- One binary. The in-sandbox programs (init, egress forwarder, ingress
  forwarder) are hidden subcommands of `ate-ish` itself, copied into each
  bundle at /.ate/ate-ish. `go install github.com/taeold/ate-ish/cmd/ate-ish@latest`
  is the whole install. runsc is still fetched on first start (existing
  code), into $XDG_STATE_HOME/ate-ish/runsc or ~/.local/state/ate-ish/runsc.
- The node runs as the invoking user. It creates and holds one user,
  mount and network namespace (as the spike's holder did) and runs every
  runsc invocation inside it. If unprivileged user namespaces are refused,
  it exits with one clear error that names the Ubuntu AppArmor fix from
  round 3 (profile text in docs/ROOTLESS-SPIKE.md) and the sysctl.
- ID mapping: when newuidmap/newgidmap exist and the user has /etc/subuid
  and /etc/subgid ranges, map root plus the user's range (as rootless
  Podman does); otherwise map only root and say so in one log line.
- Actor rootfs: the exported image (existing per-digest cache) as the
  lower layer, an unprivileged tmpfs+overlay upper, mounted in the held
  mount namespace.
- Limits: `systemd-run --user --scope` with MemoryMax/CPUQuota/TasksMax
  from the template's resources; without a user systemd session, run
  without limits and log it once.
- Egress: per (host, port) loopback addresses and sockets as in round 3;
  rule kinds `http` and `https` with replaceHeaders; wildcard hostnames
  (one leftmost label, and bare `*`) through a small DNS responder inside
  the sandbox that answers any name the policy allows with a loopback
  address it allocates and tells the forwarder about (no static
  /etc/hosts for wildcard rules); names the policy does not allow get
  NXDOMAIN. Existing credential URIs (secretmanager, env) unchanged.
  `tls_passthrough` and `actorJwt` stay unsupported in this build and the
  node logs one line when a policy uses them.
- Ingress: the dial-out forwarder from round 3, with a redial on restore so
  the first request after restore does not wait seconds.
- Snapshots: the node quiesces before checkpoint (stop accepting, wait for
  in-flight with a timeout) as round 3 found sufficient; files-only,
  rootfs and MEMORY fidelity as today.
- Fleet over one bucket unchanged (records, leases, placement, parking).

## Deleted in this build

third_party/substrate (tag the last commit that has it as
`substrate-worker` first), internal/devatelet, the cgroup helper, the
mount-namespace dialer, Patch 1 and VENDOR.md, the gen2 rootfs copy and
controller probe, ATE_ISH_NO_CGROUP_PATH, the per-node worker port
variables, the same-host unshare recipe. The README's Cloud Run section
moves to docs/cloud-run-findings.md as findings only.

## Gates (run for real, as user exedev, no sudo except where stated)

- G1 Runs unprivileged: `ate-ish` as exedev starts, fetches runsc, serves.
  With user namespaces disabled for a test (`unshare` denied, simulate via
  an env switch that skips namespace creation and fails), the error names
  the fix.
- G2 Counter at MEMORY: 1, 2, suspend, 3; then Ctrl+C the node, start it
  again, next request 4.
- G3 pi-durable (gateway variant, AI_GATEWAY_API_KEY from Secret Manager
  into the environment only): hack/gate.sh green: turn, suspend, tag,
  fork, divergent turns; egress checks: unlisted name fails, raw IP fails,
  proxy variables irrelevant, killing the forwarder reaches nothing,
  header replaced never added.
- G4 Rule kinds: an `http` rule (http://example.com/), `https` on a
  non-443 port (host-side test server with a test-only resolve override,
  as round 3), `*.vercel.sh` matching ai-gateway.vercel.sh with key
  replacement, bare `*` reaching example.com and example.org with no key,
  and under a specific policy an unlisted name still fails.
- G5 Non-root image: an image FROM node:24-slim with USER node writing its
  own files and reporting `id`. This VM has /etc/subuid ranges for
  exedev but no newuidmap: install the `uidmap` package with sudo once for
  this gate (a host prep step, like the Ubuntu profile), then pass. Also
  show the single-mapping fallback's behaviour and its log line.
- G6 In-flight suspend: an actor request that is waiting on a slow
  outbound call (host-side test server holding 20 s) while the node
  suspends: record whether the checkpoint succeeds and whether the actor's
  own request completes after restore. Either answer is acceptable; the
  README states it.
- G7 Restore latency: the first request after restore succeeds within
  1 s of the restore completing.
- G8 Two nodes on one host sharing a throwaway bucket, both as exedev:
  create via B, request via A, suspend, stop B, resume on A with the
  counter continuing. Delete the bucket after.
- G9 The node image: `docker run --privileged -p 8090:8080 <image>` serves
  and runs the counter.
- G10 Build hygiene: gofmt, go vet, go build, go test clean; no file in
  the repo needs root to build or test.

## README after this build

Install is one go install line. Run it is `ate-ish` as yourself (no sudo),
with the Ubuntu one-time profile in a short note. Compatibility table:
parity rows from G3 to G7, and "not supported yet": tls_passthrough
rules, actorJwt, plus whatever G6 found about in-flight requests. Limits:
loopback-only actor networking, programs must use the system resolver,
per-actor limits need a systemd user session, macOS uses the privileged
container.
