# What ate-ish is

Written 2026-10-31 to realign the project after a week that bent toward
Cloud Run's constraints.

## One sentence

ate-ish is the easiest way to run Agent Substrate actors: one binary, a
directory or a bucket, your Linux box, Docker, a VM, or Cloud Run, and the
same templates, egress policies and actor contract as Agent Substrate.

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

## Targets, in the order the README presents them

1. Laptop or dev box: `docker run --privileged ... ate-ish dev`, or
   `sudo ate-ish dev` on Linux. Local directory store. Actor rootfs from
   the template's image. This is the primary target and the one the
   quickstart shows.
2. A VM you own (GCE or anything else): the same binary as a service,
   bucket store, two or three nodes over one bucket. The k3s-style
   "small install you keep".
3. Cloud Run gen2 instance: `ate-ish deploy`. One command, with its
   limits stated: in-memory rootfs copy at boot, no per-actor cgroup
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
