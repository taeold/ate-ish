package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/taeold/ate-ish/internal/devatelet"
	"github.com/taeold/ate-ish/third_party/substrate/exported/installdefaults"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// ateletSPIFFE is the identity our fake atelet (internal/devatelet) presents,
// and the one the worker is told (--atunnel-broker-identity) to require.
var ateletSPIFFE = installdefaults.AteletSPIFFEID(installdefaults.SystemNamespace)

// cgroupLeafFor is the worker's own cgroup, a child of ate-ish's own current
// cgroup (never ate-ish's own cgroup itself — see startAteworker's doc
// comment on why stage 5c moved away from that). Stage 4 (two nodes, one
// host): the real cgroupfs is not namespaced away from the ate-ish parent
// process (only the forked ateworker-gvisor gets CLONE_NEWCGROUP), so a
// single fixed leaf would have a second node's startup clean up and reuse
// the first node's still-live leaf. nodeTag (ATE_DIR's base name, stable
// across one node's restarts, distinct between nodes) keeps each node in
// its own leaf.
func cgroupLeafFor(parent, nodeTag string) string { return parent + "/ate-ish-worker-" + nodeTag }

// currentCgroupPath returns the absolute path, under base (a cgroup2
// mountpoint), of this process's own (cgroup v2, "0::") cgroup.
func currentCgroupPath(base string) (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			// rel is "/" when this process's own cgroup already is the
			// real root (Cloud Run gen2: ate-ish runs as the container's
			// PID 1, whose cgroup v2 path is "0::/"); path.Join collapses
			// that to a bare base instead of the trailing "base/" string
			// concatenation left, which made nearestDomainAncestor's
			// "cgroup == base" root check never match and go on to read
			// a nonexistent "base//cgroup.type".
			return path.Join(base, rel), nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 (0::) entry in /proc/self/cgroup")
}

// nearestDomainAncestor walks up from cgroup (under base, a cgroup2
// mountpoint) until it finds one whose own cgroup.type is the plain
// "domain" (a cgroup namespace, and clone3's CLONE_INTO_CGROUP, can only
// root at a domain cgroup). A cgroup whose own type is "domain threaded"
// (deliberately hosting threaded children, found in this dev VM's own
// harness cgroup) forces every direct child to "domain invalid", which
// clone3 refuses outright ("operation not supported") — walking up one
// level at a time finds the nearest usable ancestor instead of assuming
// ate-ish's own immediate cgroup always qualifies. Stops at base itself if
// nothing closer qualifies.
func nearestDomainAncestor(base, cgroup string) (string, error) {
	for {
		// The real root cgroup has no cgroup.type file (only non-root
		// cgroups do) and is always usable.
		if cgroup == base {
			return cgroup, nil
		}
		typ, err := os.ReadFile(cgroup + "/cgroup.type")
		if err != nil {
			return "", fmt.Errorf("reading %s/cgroup.type: %w", cgroup, err)
		}
		if strings.TrimSpace(string(typ)) == "domain" {
			return cgroup, nil
		}
		cgroup = cgroup[:strings.LastIndex(cgroup, "/")]
	}
}

// prepareWorkerCgroupLeaf creates leaf as a child of parent (ate-ish's own
// cgroup) and enables every controller parent has available in its own
// subtree_control, so the leaf (and the worker's own "worker" sub-leaf
// under it, which the vendored cgroup.Delegate creates once the worker is
// running) can use them. It never touches cgroup.procs of parent or leaf:
// the worker is born directly into leaf via clone3's CLONE_INTO_CGROUP
// (SysProcAttr's CgroupFD below), so nothing but the worker itself is ever
// a member.
//
// Stage 5c found, on Cloud Run gen2 only, that moving ate-ish's own process
// into the leaf (stage 0-4's approach, cheap and sufficient on every local
// VM run) left ate-ish itself, and on gen2 at least one more process this
// session could not identify by a real PID (cgroup.procs read back "0"
// after ate-ish's own real pid was successfully moved out), permanently
// resident where cgroup.Delegate's own drain-and-move-into-"worker" step
// expected to find only the worker: "/sys/fs/cgroup/cgroup.procs" did not
// drain after 100 iterations, every single write to the destination
// nonetheless succeeding (this was never a denied syscall). Giving the
// worker a leaf that only ever contained the worker, from birth, removes
// the question of what else might be resident there rather than answering
// it for gen2 specifically.
// hostLacksMemoryOrCPU reports whether cgroup's own cgroup.controllers is
// missing "memory" or "cpu" — the two controllers runsc treats as
// mandatory for a per-actor sandbox cgroup, and the two stage 5d found
// Cloud Run gen2 does not expose (a hybrid-cgroup host: gen2 binds memory
// and cpu to v1 hierarchies this process cannot reach from inside the
// container, so the v2 mount it does control can only offer what v1 has
// not claimed — hugetlb, confirmed by stage 5d's own retry-and-measure).
func hostLacksMemoryOrCPU(cgroup string) (bool, error) {
	b, err := os.ReadFile(cgroup + "/cgroup.controllers")
	if err != nil {
		return false, fmt.Errorf("reading %s/cgroup.controllers: %w", cgroup, err)
	}
	have := strings.Fields(string(b))
	return !slices.Contains(have, "memory") || !slices.Contains(have, "cpu"), nil
}

// stageKodata copies the vendored kodata directory's internal_flags.json
// (the worker's own global/subcommand runsc flags; VENDOR.md's own words:
// "proven in stage 0 to be the complete, correct set") into dir, adding
// "--ignore-cgroups" to globalFlags when ignoreCgroups is true. Returns
// the staged directory for KO_DATA_PATH to point the worker at instead of
// the vendored tree directly.
//
// This is ate-ish's own shipped configuration, not a vendored-source
// edit (the coordinator's framing, stage 5e): the vendored copy under
// third_party/substrate is never written to; a fresh copy is staged
// under dir every time, so a host that gains or loses the flag (a
// restart with ATE_IGNORE_CGROUPS toggled, or genuinely different
// hardware) never sees a stale staged file from a previous boot.
func stageKodata(dir string, ignoreCgroups bool) (string, error) {
	staged := filepath.Join(dir, "kodata")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(workerKodataPath(), "internal_flags.json"))
	if err != nil {
		return "", fmt.Errorf("reading the vendored internal_flags.json: %w", err)
	}
	if ignoreCgroups {
		var flags map[string]any
		if err := json.Unmarshal(raw, &flags); err != nil {
			return "", fmt.Errorf("parsing internal_flags.json: %w", err)
		}
		global, _ := flags["globalFlags"].([]any)
		flags["globalFlags"] = append(global, "--ignore-cgroups")
		if raw, err = json.Marshal(flags); err != nil {
			return "", err
		}
		slog.Info("this host exposes no memory/cpu cgroup controllers to the container; " +
			"staging ateworker-gvisor with --ignore-cgroups: actors get no per-actor cpu/memory/pids limits on this host")
	}
	return staged, os.WriteFile(filepath.Join(staged, "internal_flags.json"), raw, 0o644)
}

func prepareWorkerCgroupLeaf(leaf string) error {
	parent := leaf[:strings.LastIndex(leaf, "/")]
	cleanupStaleLeaf(leaf)
	// Only the parent's subtree_control is touched here, not the leaf's
	// own: cgroup v2's "no internal process" rule means a cgroup cannot
	// both be born into (clone3's CLONE_INTO_CGROUP, a worker PID landing
	// directly in leaf) and already have a non-empty subtree_control —
	// enabling leaf's own subtree_control has to wait until after the
	// worker has moved itself into "leaf/worker" (the vendored
	// cgroup.Delegate's own job, in that order, once the worker is
	// running).
	if err := enableAvailableControllersWithRetry(parent); err != nil {
		return err
	}
	return os.MkdirAll(leaf, 0o755)
}

// wantControllers is every controller runsc's own per-actor sandbox cgroup
// (set up by the vendored worker, not this code) needs available; see
// enableAvailableControllersWithRetry.
var wantControllers = []string{"cpuset", "cpu", "io", "memory", "hugetlb", "pids", "rdma"}

// enableAvailableControllersWithRetry enables every controller cgroup lists
// as available in its own cgroup.controllers, in its own
// cgroup.subtree_control, retrying for a few seconds if fewer than
// wantControllers' full set shows up.
//
// Stage 5d, real gen2 deploy: this measured non-deterministic across two
// otherwise-identical deploys of the same image — one run's
// cgroup.controllers listed all seven ("cgroup delegation ready" logged
// all of them, confirmed by the vendored worker's own log, and the actor
// booted past this point to a different, separate blocker); the next
// run's listed only "hugetlb", with zero write failures logged for the
// other six (they were simply never in the available list to try), and
// runsc's own per-actor sandbox cgroup then failed outright ("mandatory
// cgroup controller \"cpu\" is missing"). No write to parent ever errored
// in either run — this reads as Cloud Run gen2's own container cgroup
// setup not being fully settled yet when this code runs, not a logic bug
// here, so retrying rather than failing once is the reasonable response: a
// fixed, generous retry budget (20 attempts, 250ms apart, 5s total) buys
// time for whatever gen2-side delegation finishes without fixing the
// deadline to a guessed single number.
func enableAvailableControllersWithRetry(cgroup string) error {
	var avail string
	for attempt := 0; attempt < 20; attempt++ {
		b, err := os.ReadFile(cgroup + "/cgroup.controllers")
		if err != nil {
			return fmt.Errorf("reading %s/cgroup.controllers: %w", cgroup, err)
		}
		avail = string(b)
		have := strings.Fields(avail)
		missing := 0
		for _, want := range wantControllers {
			if !slices.Contains(have, want) {
				missing++
			}
		}
		if missing == 0 {
			break
		}
		if attempt == 0 || attempt == 19 {
			slog.Warn("cgroup.controllers is missing some of the controllers runsc needs; retrying",
				"cgroup", cgroup, "have", have, "want", wantControllers, "attempt", attempt)
		}
		time.Sleep(250 * time.Millisecond)
	}
	for _, c := range strings.Fields(avail) {
		if err := os.WriteFile(cgroup+"/cgroup.subtree_control", []byte("+"+c), 0o644); err != nil {
			slog.Warn("could not enable cgroup controller", "cgroup", cgroup, "controller", c, "err", err)
		}
	}
	return nil
}

// cleanupStaleLeaf undoes what a previous run's worker left behind:
// cgroup.Delegate enables every controller in subtree_control and moves
// itself into a "worker" child leaf, and cgroupfs refuses to add a process
// to a non-leaf cgroup (the "no internal processes" rule), so a stale,
// still-populated leaf from an unclean shutdown must be cleared, not just
// recreated.
func cleanupStaleLeaf(leaf string) {
	for _, c := range []string{"cpuset", "cpu", "io", "memory", "hugetlb", "pids", "rdma"} {
		_ = os.WriteFile(leaf+"/cgroup.subtree_control", []byte("-"+c), 0o644)
	}
	_ = os.Remove(leaf + "/worker")
	_ = os.Remove(leaf)
}

// removeWorkerCgroupLeaf is prepareWorkerCgroupLeaf's inverse, best-effort,
// called on graceful shutdown so a clean stop does not leave the leaf (and
// the parent's now-unnecessary subtree_control delegation) behind.
// On the ambient-cgroup2-works path (every host measured so far) leaf is
// a real, still-mounted path and this cleans it up. On the cgrouphelper.go
// path (Cloud Run gen2 only), the helper's own private mount namespace,
// and the mountpoint leaf names, are already gone by the time this runs
// (the helper exits right after handing back its fd); best-effort cleanup
// here is then a silent no-op, not a new leak — the whole container's
// cgroup tree is torn down with it when the sandbox itself is deleted.
func removeWorkerCgroupLeaf(leaf string) {
	cleanupStaleLeaf(leaf)
}

// ateworkerConfig is what startAteworker needs to launch the vendored
// worker and reach its socket.
type ateworkerConfig struct {
	binary      string // bin/ateworker-gvisor
	kodata      string // third_party/substrate/cmd/ateworker-gvisor/kodata (internal_flags.json)
	podUID      string
	nodeTag     string // ATE_DIR's base name: scopes the cgroup leaf and the stale-process pkill to this node only
	maxActors   int
	pki         *devatelet.PKI
	readyPort   int
	atunnelPort int
	connectPort int
	logFile     string
}

// startAteworker launches ateworker-gvisor in a fresh cgroup and mount
// namespace and a fresh cgroup2 mount, the same way a kubelet-run pod would
// give it one: born directly into its own leaf cgroup (never ate-ish's own,
// see prepareWorkerCgroupLeaf's doc comment) via clone3's
// CLONE_INTO_CGROUP. Returns the leaf path so the caller can remove it on
// shutdown.
func startAteworker(cfg ateworkerConfig) (*exec.Cmd, string, error) {
	// Stage 4: two nodes on this host run the identical binary path, so a
	// pattern on cfg.binary alone (single-node assumption) would pkill the
	// OTHER node's live ateworker-gvisor the moment this node starts.
	// cfg.pki.Path(...) embeds this node's own ATE_DIR, unique per node, so
	// scoping the kill pattern to it only ever reaps this node's own stale
	// process from a previous, uncleanly-stopped run.
	marker := cfg.pki.Path(devatelet.WorkerCredentialBundleFile)
	selfSafePattern := "[" + marker[:1] + "]" + marker[1:]
	_ = exec.Command("pkill", "-9", "-f", selfSafePattern).Run()
	time.Sleep(300 * time.Millisecond)

	// INCIDENT (2026-10-31): this used to unconditionally run `umount
	// /sys/fs/cgroup || umount -l /sys/fs/cgroup; mount -t cgroup2 ...`
	// directly in ate-ish's OWN (shared) mount namespace, on every host.
	// On this dev VM — a general-purpose host with its own live systemd
	// and dbus, not a single-container sandbox — that lazily detached the
	// cgroup2 mount systemd (pid 1) holds system-wide; afterward
	// `systemctl`/`systemd-run --scope`/anything needing dbus activation
	// hung, and Docker's systemd cgroup driver could no longer start ANY
	// container (first misread as a Docker/gVisor-specific "cgroup
	// activation timeout" — see docs/PROGRESS.md's stage 8 notes). Never
	// remount /sys/fs/cgroup itself in this process's own namespace again:
	// prepareWorkerCgroupFD below probes the ambient view read-only first
	// (every local/VM host, this one included) and only reaches for a
	// mount at all when that probe fails (so far, only Cloud Run gen2);
	// even then it is a *fresh, separate* cgroup2 instance at a private
	// path, mounted inside a throwaway helper subprocess's own private
	// mount namespace (CLONE_NEWNS) — never this one's, and never
	// /sys/fs/cgroup itself.
	leafFD, ateishCgroup, ignoreCgroups, err := prepareWorkerCgroupFD(cfg.nodeTag)
	if err != nil {
		return nil, "", err
	}
	defer leafFD.Close()
	leaf := cgroupLeafFor(ateishCgroup, cfg.nodeTag)

	if env("ATE_IGNORE_CGROUPS", "") == "1" {
		ignoreCgroups = true
	}
	stagedKodata, err := stageKodata(filepath.Dir(cfg.logFile), ignoreCgroups)
	if err != nil {
		return nil, "", fmt.Errorf("staging the worker's runsc flags: %w", err)
	}
	cfg.kodata = stagedKodata

	logFile, err := os.Create(cfg.logFile)
	if err != nil {
		return nil, "", err
	}

	args := []string{
		"--pod-uid=" + cfg.podUID,
		"--pod-ips=127.0.0.1",
		"--log-level=info",
		fmt.Sprintf("--max-actors=%d", cfg.maxActors),
		fmt.Sprintf("--readiness-listen-address=127.0.0.1:%d", cfg.readyPort),
		fmt.Sprintf("--atunnel-listen-address=127.0.0.1:%d", cfg.atunnelPort),
		fmt.Sprintf("--atunnel-connect-listen-address=127.0.0.1:%d", cfg.connectPort),
		"--atunnel-credential-bundle=" + cfg.pki.Path(devatelet.WorkerCredentialBundleFile),
		"--atunnel-trust-bundle=" + cfg.pki.Path(devatelet.NodeTrustBundleFile),
		"--atunnel-broker-identity=" + ateletSPIFFE,
		"--atunnel-egress-trust-bundle=" + cfg.pki.Path(devatelet.GatewayTrustBundleFile),
		"--dns-egress-policy=enforce-in-atunnel",
	}
	cmdLine := cfg.binary
	for _, a := range args {
		cmdLine += " " + a
	}
	// Stage 5 (Cloud Run gen2) found /sys/fs/cgroup already mounted in a
	// way a plain `umount` refuses as EBUSY (the platform's own runtime
	// holds a reference), where every local/VM run had an unshared mount
	// free to unmount outright. `umount -l` (lazy: detach now, the
	// underlying mount goes away once nothing still references it) is the
	// fallback, and makes the remount succeed in both environments.
	script := "mount --make-rprivate /; " +
		"umount /sys/fs/cgroup 2>&1 || umount -l /sys/fs/cgroup; " +
		"mount -t cgroup2 cgroup2 /sys/fs/cgroup && exec " + cmdLine

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "KO_DATA_PATH="+cfg.kodata)
	if ignoreCgroups {
		// Stage 5f: --ignore-cgroups (above) only suppresses the sandbox's
		// own cgroup setup; runsc's subcontainer path unconditionally
		// tries to configure whatever cgroupsPath the vendored
		// exported/ocispec/gvisor.go assigns, which fails the same way on
		// a host with no memory/cpu controller. ATE_ISH_NO_CGROUP_PATH is
		// VENDOR.md's "Patch 1": leaves that field unset instead.
		cmd.Env = append(cmd.Env, "ATE_ISH_NO_CGROUP_PATH=1")
	}
	// Also tee to this process's own stdout/stderr: on Cloud Run, the
	// platform only captures this container's own streams, not an
	// arbitrary file on disk, and ateworker-gvisor's startup failures (the
	// ones stage 5's probe needs to see) happen before anything calls it
	// over its socket.
	cmd.Stdout = io.MultiWriter(logFile, os.Stdout)
	cmd.Stderr = io.MultiWriter(logFile, os.Stderr)
	// Stage 5c: UseCgroupFD+CgroupFD (clone3's CLONE_INTO_CGROUP, kernel
	// 5.7+; this VM is 6.12, gen2 is 6.9) places the new process directly
	// into leaf at birth, atomically with creating its cgroup namespace
	// (CLONE_NEWCGROUP) there — nothing else is ever a member of leaf,
	// unlike stage 0-4's "move ate-ish's own pid into the leaf first"
	// approach, which stage 5c found left ate-ish (and on Cloud Run gen2,
	// at least one more process this session could not identify) resident
	// where cgroup.Delegate's own drain step expected only the worker.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWCGROUP | syscall.CLONE_NEWNS,
		UseCgroupFD: true,
		CgroupFD:    int(leafFD.Fd()),
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting ateworker-gvisor: %w", err)
	}
	// Stage 5 probe diagnostic: a seccomp SECCOMP_RET_KILL (or RET_TRAP)
	// terminates the process with a signal and produces no output at all
	// on the fds above, so waitForSocket's 30s timeout alone would not
	// distinguish that from "just slow." Logging Wait()'s result (exit
	// code or killing signal) makes that distinction visible.
	go func() {
		err := cmd.Wait()
		fmt.Fprintf(logFile, "ATELET_STEP=wait-returned err=%v state=%v\n", err, cmd.ProcessState)
		fmt.Fprintf(os.Stderr, "ATELET_STEP=wait-returned err=%v state=%v\n", err, cmd.ProcessState)
	}()
	return cmd, leaf, nil
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("socket %q did not appear within %v", path, timeout)
}

func dialAteworker(sockPath string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///ateworker",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		}),
	)
}

// callWithRetry retries while the worker reports Unavailable (not yet
// registered with our atelet), which clears within a second or two of
// startup.
func callWithRetry(ctx context.Context, name string, fn func(context.Context) error) error {
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if status.Code(err) != codes.Unavailable {
			return fmt.Errorf("%s: %w", name, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("%s: giving up after retries: %w", name, lastErr)
}
