# What ate-ish is

Written 2026-10-31 to realign the project after a week that bent toward
Cloud Run's constraints.

## One sentence

ate-ish is the best of celld and Agent Substrate: celld's shape, one
daemon per machine and a bucket as the whole control plane, running Agent
Substrate's actors under Agent Substrate's own worker, with the same
templates, egress policies and actor contract. It runs on a Linux box, in
Docker, on a VM, or on Cloud Run, and is exclusive to none of them.

From celld: one binary, a fleet is the nodes sharing a bucket, conditional
writes are the lock, no database, no membership protocol, a directory for
local development, idle actors cost nothing. From Agent Substrate: the
actor, its YAML, its header, and its worker (gVisor isolation, per-actor
network with egress policy at the edge, checkpoint and restore with memory
fidelity, tags and forks), vendored rather than reimplemented. Ours is the
thin layer between them.

## What it is for

Someone who read about Agent Substrate and wants to write an actor and run
it today, without standing up a Kubernetes cluster, Postgres, an object
store and eight services. They write a Dockerfile, a template.yaml and
optionally an egress-policy.yaml, exactly as they would for substrate, and
ate-ish runs it. When they outgrow ate-ish, the same files go to substrate.

## What it promises

- The actor runs under Agent Substrate's own gVisor worker, vendored
  unmodified except one documented patch. Snapshots, memory fidelity,
  per-actor network isolation and egress enforcement are substrate's.
- The template, the egress policy, the `ate-target-actor` header and the
  CLI verbs are substrate's. A template that works here works there.
- No database. State is a local directory or one GCS bucket (S3 later).
- Scale target: one machine to a few. Not millions of actors.

## What it does not promise

- Substrate's control-plane scale, Postgres-backed consistency, or its
  per-actor edge identity to external systems.
- Anything about a platform it runs on beyond what the README's limits
  section states for that platform.

## Command surface (decided 2026-10-31)

One binary, one daemon, kubectl-ate's verbs. No `dev`, no `deploy`.

- `ate-ish` with no arguments runs a node: role all, a local directory for
  state unless ATE_BUCKET names a bucket, runsc fetched if missing. The same
  invocation on a laptop (root, or a privileged container on macOS), a VM,
  or a Cloud Run gen2 instance. The node pulls actor images itself: from
  the local Docker daemon when one is reachable, else from the registry
  the template names.
- `ate-ish create actor-template -f template.yaml`, `create actor`,
  `create egress-policy`, `create tag`, `suspend actor`, `delete actor`,
  `get actors|tags|workers`, all against `--endpoint` (default
  http://localhost:8080). Deploying an actor is creating a template and an
  actor against a node. Nothing else.
- Putting a node on Cloud Run is one `gcloud beta run instances create`
  line with the ate-ish image, documented in the README under its own
  heading with its limits. A fleet is more nodes pointed at one bucket.
- The user brings an image (a Dockerfile they build). Rebuilding and
  redeploying is their loop, as with kind. No watch mode, no source mounts.

## Targets, in the order the README presents them

1. Laptop or dev box: `sudo ate-ish` on Linux, or
   `docker run --privileged ... ate-ish` on macOS. Local directory store.
   Actor rootfs from the template's image. This is the primary target and
   the one the quickstart shows.
2. A VM you own (GCE or anything else): the same binary as a service,
   bucket store, two or three nodes over one bucket. The k3s-style
   "small install you keep".
3. Cloud Run gen2 instance: the gcloud line. With its limits stated: in-memory rootfs copy at boot, no per-actor cgroup
   limits, single node, undocumented capability dependency.

Cloud Run is a target, not the design center. Its findings (overlayfs
root, missing cgroup controllers, the launcher's closed host link) are
documented facts for the platform conversation, not constraints the rest
of the design bends around.

## Measures of success

- A new user runs the pi-durable demo locally in three commands.
- Substrate's own demos pass at the fidelity their templates ask for.
- The vendored worker stays byte-identical to upstream except Patch 1,
  checked at every commit.
- Our own code stays small: control plane, atelet role, egress gateway,
  deploy, dev. Today 4,035 lines.
