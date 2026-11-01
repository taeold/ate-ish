# Cloud Run findings

Findings from running ate-ish's pre-rootless build on Cloud Run gen2. Not
instructions; docs/ROOTLESS.md's build does not target Cloud Run yet and
this file is not re-verified against it.

- Cloud Run gen2's own root filesystem is already an overlay, so the
  pre-rootless node copied the node's filesystem once per instance onto
  a real (non-overlay) volume and used that copy as every actor's
  rootfs base, adding memory and startup time at every instance start
  (see `docs/measurements.md`).
- gen2 exposes only the `hugetlb` cgroup controller to the container;
  `memory` and `cpu` are bound to cgroup v1 hierarchies the container
  cannot reach. The pre-rootless worker detected this and ran with
  `--ignore-cgroups`, so actors on gen2 got no cpu, memory, or pids
  accounting or limit at all.
- One instance is one node. A fleet on Cloud Run is more instances,
  each pointed at the same bucket.
- Credentials for egress policies come from Secret Manager through the
  instance's service account; it needs `secretAccessor` on each secret
  the policy names.
- gen2 gives the container root and all of the (vendored) worker's
  capability set except `MKNOD`. Cloud Run does not document this, so
  it can change.
