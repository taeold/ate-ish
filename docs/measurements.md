# Measurements

Timing and sizing data from earlier rounds of running ate-ish on Cloud Run
and locally, plus the limits found along the way. Kept here as a log, not
as a guarantee: a number below is what one run measured, not a promise
about the number you will see. The README's own "Limits" and "Cloud Run"
sections state only the limits that still hold against the current code;
this file has the fuller, dated detail behind them.

## Measured, Cloud Run gen2 round

All runs were in us-west1, on the published pi-durable image, with 2 vCPU
and 4 GiB per worker. `docs/log/play.txt` is the walkthrough on the
instance. `docs/log/scale.txt` is the walkthrough on the scale shape, an
ingress service and two workers with four slots each. There, Turn 1 ran
on one worker and the fork restored on the other. `docs/log/park.txt` is
`hack/park.sh` on the scale shape. n is the number of runs.

| Step | Measured | n |
|---|---|---|
| `instances create` | 16 s | 1 |
| Redeploy on a running instance (`instances update --image`) | 92 and 103 s | 2 |
| Scale-shape deploy | 31 and 41 s | 2 |
| Boot, first sandbox on a new container (sandbox run to wakeup probe) | 2.64 to 2.71 s | 3 |
| Boot, later sandboxes one at a time | 0.70 to 0.95 s | 4 |
| Boot, two at once on one worker | 1.15 to 1.22 s | 4 |
| Boot, four at once on one worker | 2.33 to 2.36 s | 4 |
| Suspend on the worker (stop, tar, upload of 122 to 164 KB, record) | 0.51 to 0.55 s | 5 |
| Request to a suspended actor at the ingress, worker already booted once | 0.94 to 1.24 s | 3 |
| Request to a running actor at the ingress (two GCS reads) | 47 to 77 ms | 4 |
| `ate-ish` CLI call, `gcloud auth print-identity-token` included | 0.95 to 1.18 s, suspend 1.61 and 1.62 s | 22 |
| Turn at the client, resume included (claude-haiku-4.5 through the egress proxy) | 2.97 to 14.6 s | 10 |

Parking, from `docs/log/park.txt`: p1 to p8 at once filled the 8 slots, and all answered 200 in 1.6 to 4.4 s. p9 waited 3.9 s for a slot, which opened when p1 was suspended 2 s into the wait, and answered 200 at 5.2 s. p10 waited 5 s, no slot opened, and got `503 no worker has room for the actor` at 5.5 s.

What bounds them: booting is Node starting pi-durable inside gVisor, and four boots on two vCPUs take about three times as long as one. The first boot on a new container is about 2 s slower than later ones, likely because Cloud Run loads the image's files on first read, though that was not measured directly. Suspend and restore are mostly GCS round trips to a bucket in the same region. A request to a running actor pays two GCS reads at the ingress, for the actor record and for the owner's lease. The CLI starts a gcloud process for its token on every call. In the parking run, p2 and p7 reached the ingress 2.7 s after the other six, according to the Cloud Run request log, and the ingress placed them in 1.5 s.

## Limits found along the way

- **Port 80 works under the v2 worker.** The v2 worker runs substrate's own gVisor worker directly, not a platform sandbox-launcher, so an actor can bind port 80 as root inside the sandbox. The coding-agents demos all run their upstream templates unchanged on port 80, no `PORT` env, no `sed`. (Current and reflected in the README.)
- **A request stays open after its client leaves, and libfx's turns did not close their stream under v1.** Confirmed locally and on Cloud Run gen2 at the time: a libfx turn's model and tool calls complete (seen in the ndjson itself) but the HTTP response never signaled end of stream, so a client timed out instead of seeing the turn finish. `ate-ish suspend actor` still worked on the wedged actor (the lock is in the response, not the actor's own suspend path), but a client could not tell a turn was done without a separate poll. See docs/demos.md.
- **No forced stop.** If an actor does not exit within 8 s of `SIGTERM`, suspend fails and the actor keeps running. `ate-ish delete actor` suspends first, so it fails the same way, and the actor keeps its slot until the worker stops. (Still current.)
- **VOLUMES fidelity by default; MEMORY works when a template asks for it.** A template's top-level `preferredFidelity: MEMORY` (a nested `snapshotConfig.preferredFidelity` is ignored) gets a real runsc checkpoint of the sandbox's process memory, not just the durable dirs. Confirmed: substrate's own `counter` demo's in-memory and rootfs counters both survive suspend/resume under MEMORY, the exact case that failed on v1. (Still current.)
- **The workspace is RAM on the worker.** The launcher keeps the writable layer in memory (`--overlay2=root:memory`). A 20 MB workspace uses 20 MB of worker memory, and the snapshot tar sits in the worker's in-memory `/tmp` during a suspend.
- **Egress is enforced by the actor's own network, not convention.** The v2 worker puts every actor TCP connection through nftables to atunnel regardless of any proxy environment variable; a process that ignores `HTTPS_PROXY`/`NODE_USE_ENV_PROXY` reaches an unlisted host no more than one that respects it, the opposite of v1's proxy-convention enforcement. `hack/gate.sh` asserts this with and without the proxy env var set. (Still current.)
- **The proxy knows an actor by its listener port.** Another actor on the same worker can use a neighbor's port and its credentials.
- **One ingress instance.** The scale shape ran the ingress with `--max-instances 1`, so concurrent placements of one actor shared one process.
- **One template per deployment, in the v1/v2 Cloud Run design.** Every actor ran from the node image. Two templates could share a deployment only if the image held both, as `demo/pi-durable` does for its two servers.
- **A worker has 10 s to drain when Cloud Run replaces it.** On `SIGTERM` the worker suspends every actor and deletes its lease. An actor whose suspend does not finish in 10 s comes back from its last snapshot.
- **On Cloud Run gen2, actors get no per-actor cgroup resource limits at all, by platform design, not by choice.** gen2 is a hybrid cgroup host: it exposes only the `hugetlb` controller to the container's own cgroup v2 root; `memory` and `cpu` are bound to cgroup v1 hierarchies this process cannot reach (a 5-second, 20-attempt retry never saw them appear). ate-ish detects this at node start and runs the worker's own `runsc` with `--ignore-cgroups` plus the one-line patch in `third_party/substrate/VENDOR.md` ("Patch 1") that skips assigning a per-container cgroup path; actors run with no cpu/memory/pids accounting or limit there, and usage stats report `source: unspecified` instead of real cgroup-backed numbers. On a host that does expose the controllers (a plain VM, GKE nodes), nothing changes. (Still current; restated in the README's "Cloud Run" section.)
- **Cloud Run gen2's own root filesystem is already overlayfs,** so ate-ish copies the node filesystem once per instance onto an in-memory volume (`--add-volume=type=in-memory`, since `type=ephemeral-disk` is not supported for Cloud Run Instances) and uses that copy as every actor's rootfs lowerdir. Measured 578 to 588 MB for the pi-durable image, comfortably inside a 2Gi volume, taking about 45 to 50 s once per instance start. (Still current; restated in the README's "Cloud Run" section.)
- **Update a stopped instance after starting it.** On a stopped instance, `instances update --image` changed the spec but did not import the image, and `instances start` ran the previous image (measured once).
- **The removed `ate-ish dev` command's Docker path needed a docker daemon reachable from inside the container to build the actor's own Dockerfile.** The published `ate-ish` image carried no docker daemon inside it by design, so `docker run --privileged ... dev` could only run an actor whose template already named a pullable image. Historical: `dev` no longer exists.

## Estimated cost, Cloud Run gen2 round

These prices were estimates from Cloud Run's instance-based list prices for tier 1 regions at the time, $0.000018 per vCPU-second and $0.000002 per GiB-second. They were never confirmed against the pricing page or a bill, and prices move; treat the numbers below as historical, not current guidance.

| Shape | Always on | Per hour | Per month |
|---|---|---|---|
| Quickstart, one instance with 2 vCPU and 4 GiB | yes | about $0.16 | about $115 |
| Scale shape, two such workers and an ingress billed per request | workers yes | about $0.32 | about $230 |

The bucket held a few KB of JSON and one 120 to 170 KB tar per actor. A turn through the gateway with claude-haiku-4.5 cost about $0.002 in these runs.
