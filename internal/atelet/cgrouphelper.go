// cgrouphelper.go: see atelet.go's INCIDENT comment on startAteworker for
// why this file exists. ate-ish's own process must never remount
// /sys/fs/cgroup in its own (shared) mount namespace.
//
// ambientCgroupControllersWithRetry probes the ambient cgroup2 view
// read-only, with the same bounded retry stage 5d's own
// enableAvailableControllersWithRetry uses, because Cloud Run gen2's own
// container cgroup setup is not always settled the instant this runs —
// racing that settling with a single, unretried read used to send some
// boots into the gen2 path when the ambient view would have worked a
// fraction of a second later.
//
// startWorkerViaHelper is the gen2 path, taken only once the ambient view
// still looks unusable after that retry. It used to hand back a leaf fd
// opened in a throwaway helper subprocess's own private mount namespace
// for THIS process to clone3 a worker into — a cgroup fd crossing a mount
// namespace boundary, which is what made some gen2 boots fail outright
// ("fork/exec /usr/bin/bash: no such file or directory": clone3's
// CLONE_INTO_CGROUP with that fd, combined with the spawned child's own
// CLONE_NEWNS, left the child's mount namespace in a state where its own
// exec target no longer resolved). The fd and the clone3 call that
// consumes it now both happen inside the helper subprocess itself: the
// helper mounts its own private cgroup2 instance, opens the leaf fd
// locally, and clone3's the real ateworker-gvisor process into it directly
// — no fd ever crosses a namespace boundary. The helper then stays alive
// only to relay the worker's exit status (so atelet.go's own Wait-and-log
// goroutine on the helper's *exec.Cmd keeps working unchanged); the real
// worker's own pid is reported back separately and is what worker.go
// actually signals and nsenters into — see startWorkerViaHelper's own doc
// comment for why that distinction is load-bearing, not cosmetic.
package atelet

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ambientCgroupFSBase is the one real cgroup2 mountpoint on every host
// ate-ish has run on outside Cloud Run gen2 (this dev VM included), and
// also the path the gen2 helper remounts privately inside its own mount
// namespace (never visible outside it).
const ambientCgroupFSBase = "/sys/fs/cgroup"

// ambientCgroupControllersWithRetry reports whether the ambient cgroup2
// view already exposes a non-empty cgroup.controllers file, retrying up to
// stage 5d's own budget (5s, 250ms steps) before concluding it genuinely
// is not usable. A bare, unretried read raced Cloud Run gen2's own
// container cgroup setup: some boots observed it mid-settle (ENOENT, or an
// empty file with no error) and took the gen2 helper path even though the
// ambient view would have worked moments later.
func ambientCgroupControllersWithRetry() bool {
	for attempt := 0; attempt < 20; attempt++ {
		b, err := os.ReadFile(ambientCgroupFSBase + "/cgroup.controllers")
		if err == nil && len(strings.Fields(string(b))) > 0 {
			return true
		}
		if attempt != 19 {
			time.Sleep(250 * time.Millisecond)
		}
	}
	return false
}

// prepareWorkerCgroupFD returns an open file descriptor for the worker's
// cgroup leaf (ready for clone3's CLONE_INTO_CGROUP) under the ambient
// cgroup2 view, the leaf's parent cgroup path (for logging and
// cgroupLeafFor), and whether this host lacks the memory/cpu controllers
// runsc's own per-actor sandbox cgroup needs. Only called once
// ambientCgroupControllersWithRetry has already said the ambient view
// works; see startWorkerViaHelper for the gen2 fallback.
func prepareWorkerCgroupFD(nodeTag string) (leafFD *os.File, ancestor string, ignoreCgroups bool, err error) {
	return prepareLeafAt(ambientCgroupFSBase, nodeTag)
}

// prepareLeafAt does the real work, against base (a working cgroup2
// mountpoint): find ate-ish's own cgroup, find a usable ("domain") leaf
// ancestor, move ate-ish out of it if it is itself ate-ish's own cgroup,
// and prepare the worker's leaf as a child of that ancestor. Used both by
// prepareWorkerCgroupFD (base = the real /sys/fs/cgroup) and by the gen2
// helper below (base = /sys/fs/cgroup too, but inside the helper's own
// private mount namespace and a fresh cgroup2 instance there).
func prepareLeafAt(base, nodeTag string) (leafFD *os.File, ancestor string, ignoreCgroups bool, err error) {
	cgroup, err := currentCgroupPath(base)
	if err != nil {
		return nil, "", false, fmt.Errorf("finding ate-ish's own cgroup: %w", err)
	}
	ancestor, err = nearestDomainAncestor(base, cgroup)
	if err != nil {
		return nil, "", false, fmt.Errorf("finding a domain-type ancestor of ate-ish's own cgroup: %w", err)
	}
	// Docker finding (stage 8): a plain `docker run` container's init
	// process (ate-ish itself) is a direct member of the container's own
	// cgroup root, which is exactly the cgroup nearestDomainAncestor picks
	// (ancestor == cgroup). Enabling subtree_control there and then
	// clone3'ing the worker CLONE_INTO_CGROUP into a fresh child leaf
	// under it then fails with EOPNOTSUPP — cgroup v2's "no internal
	// process constraint" (cgroup-v2.rst §2.3: a domain cgroup cannot have
	// both member tasks of its own and children with controllers
	// enabled). Every host measured before this one (VM/bare systemd)
	// never hit this because ate-ish's own cgroup there already sits one
	// or more levels below the ancestor subtree_control actually touches.
	// See selfMigrateOutOf's own comment.
	if ancestor == cgroup {
		if err := selfMigrateOutOf(ancestor); err != nil {
			return nil, "", false, fmt.Errorf("moving ate-ish's own process out of %s: %w", ancestor, err)
		}
	}
	leaf := cgroupLeafFor(ancestor, nodeTag)
	if err := prepareWorkerCgroupLeaf(leaf); err != nil {
		return nil, "", false, fmt.Errorf("preparing the worker's cgroup leaf: %w", err)
	}
	f, err := os.Open(leaf)
	if err != nil {
		return nil, "", false, fmt.Errorf("opening the worker's cgroup leaf: %w", err)
	}
	lacks, err := hostLacksMemoryOrCPU(ancestor)
	if err != nil {
		f.Close()
		return nil, "", false, fmt.Errorf("checking which cgroup controllers this host exposes: %w", err)
	}
	return f, ancestor, lacks, nil
}

// selfMigrateOutOf moves the calling process (and nothing else; every
// process measured so far that hits this path is single-threaded-process
// ate-ish or its own single-process cgroup helper, never a multi-process
// group) into a dedicated, newly created "ate-ish-self" child of cgroup,
// so cgroup itself is left with no member tasks of its own before
// anything enables its subtree_control or clone3's a worker into a
// sibling child. Verified directly (a throwaway strace repro, not
// committed): without this, `clone3(..., CLONE_INTO_CGROUP, cgroup=<a
// fresh, empty sibling>)` under a cgroup still holding this process as a
// task failed EOPNOTSUPP the moment that cgroup's own subtree_control had
// any controller enabled; moving this process out first (and only then
// enabling subtree_control) made every controller enable cleanly and the
// clone succeed.
func selfMigrateOutOf(cgroup string) error {
	self := cgroup + "/ate-ish-self"
	if err := os.MkdirAll(self, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", self, err)
	}
	if err := os.WriteFile(self+"/cgroup.procs", []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("moving pid %d into %s/cgroup.procs: %w", os.Getpid(), self, err)
	}
	return nil
}

// shellJoin single-quotes each argument for bash -c, the same way
// startWorkerAmbient's cmdLine is built, so the worker's own flags (none
// of which contain a single quote) survive the shell step untouched.
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// helperAck1 is the gen2 helper's first reply: its leaf is ready (or it
// hit an error getting there). No fd is ever sent — see the package doc
// comment above.
type helperAck1 struct {
	Ancestor      string
	IgnoreCgroups bool
	Err           string
}

// helperLaunch is what the parent sends once it knows IgnoreCgroups
// (needed to stage kodata and build KO_DATA_PATH before the worker can be
// told to start): the fully resolved argv and environment to launch
// ateworker-gvisor with.
type helperLaunch struct {
	Argv []string
	Env  []string
}

// helperAck2 is the helper's reply once it has clone3'd the worker (or
// failed to).
type helperAck2 struct {
	Pid int
	Err string
}

// startWorkerViaHelper is startAteworker's gen2 fallback: it spawns the
// helper (this same binary, re-exec'd with a hidden subcommand and
// CLONE_NEWNS so its mount namespace dies with it, never touching ate-ish's
// own), waits for it to report its leaf is ready, tells it what to launch,
// and returns the helper's own *exec.Cmd (so atelet.go's existing
// Wait-and-log goroutine keeps working unchanged: RunCgroupHelper waits on
// the real worker and exits with its code) alongside the real worker's own
// pid, reported separately.
//
// That pid must not be helperCmd.Process.Pid: mountBundleRootfs/
// unmountBundleRootfs (bundle.go) and worker.go's SIGTERM-on-shutdown both
// need the real ateworker-gvisor process's own pid, not the helper's — the
// real worker gets its own further CLONE_NEWNS inside the helper's already
// private one (see RunCgroupHelper), so nsenter-ing into the helper's own
// mount namespace (an intermediate, further-out one) would miss every
// mount the real worker's own namespace holds, including the actor's own
// bundle rootfs overlay. Measured directly (2026-10-31): with
// helperCmd.Process.Pid used for nsenter, the overlay mount landed one
// namespace level too shallow and the actor's sandbox could not find its
// own command ("error finding executable \"node\" in PATH").
func startWorkerViaHelper(cfg ateworkerConfig, logFile *os.File) (*exec.Cmd, string, int, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, "", 0, fmt.Errorf("socketpair for the cgroup helper: %w", err)
	}
	parentConn := os.NewFile(uintptr(fds[0]), "cgroup-helper-parent")
	childConn := os.NewFile(uintptr(fds[1]), "cgroup-helper-child")

	exe, err := os.Executable()
	if err != nil {
		parentConn.Close()
		childConn.Close()
		return nil, "", 0, err
	}
	helperCmd := exec.Command(exe, "__cgroup-helper", cfg.nodeTag)
	helperCmd.ExtraFiles = []*os.File{childConn} // becomes fd 3 in the helper
	helperCmd.Stdout = logFile
	helperCmd.Stderr = logFile
	// The only mount this helper (and the worker it later clone3's inside
	// itself) ever does lands in a mount namespace that dies with it —
	// never ate-ish's own, never the host's.
	helperCmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if err := helperCmd.Start(); err != nil {
		parentConn.Close()
		childConn.Close()
		return nil, "", 0, fmt.Errorf("starting the cgroup helper: %w", err)
	}
	childConn.Close()

	dec := json.NewDecoder(parentConn)
	enc := json.NewEncoder(parentConn)

	fail := func(format string, args ...any) (*exec.Cmd, string, int, error) {
		parentConn.Close()
		_ = helperCmd.Process.Kill()
		_ = helperCmd.Wait()
		return nil, "", 0, fmt.Errorf(format, args...)
	}

	var ack1 helperAck1
	if err := dec.Decode(&ack1); err != nil {
		return fail("receiving the cgroup helper's leaf status: %w", err)
	}
	if ack1.Err != "" {
		return fail("cgroup helper: %s", ack1.Err)
	}

	stagedKodata, err := stageKodata(filepath.Dir(cfg.logFile), ack1.IgnoreCgroups)
	if err != nil {
		return fail("staging the worker's runsc flags: %w", err)
	}
	env := append(os.Environ(), "KO_DATA_PATH="+stagedKodata)
	if ack1.IgnoreCgroups {
		env = append(env, "ATE_ISH_NO_CGROUP_PATH=1")
	}
	argv := append([]string{cfg.binary}, workerArgs(cfg)...)

	if err := enc.Encode(helperLaunch{Argv: argv, Env: env}); err != nil {
		return fail("sending the launch request to the cgroup helper: %w", err)
	}

	var ack2 helperAck2
	if err := dec.Decode(&ack2); err != nil {
		return fail("receiving the cgroup helper's launch result: %w", err)
	}
	if ack2.Err != "" {
		return fail("cgroup helper: starting ateworker-gvisor: %s", ack2.Err)
	}
	parentConn.Close()
	return helperCmd, cgroupLeafFor(ack1.Ancestor, cfg.nodeTag), ack2.Pid, nil
}

// RunCgroupHelper is startWorkerViaHelper's child side, dispatched from
// cmd/ate-ish's main as `ate-ish __cgroup-helper <nodeTag>`. It runs alone
// in its own private mount namespace (CLONE_NEWNS, set by the parent's
// SysProcAttr on this exact process), mounts a fresh cgroup2 instance
// there, clone3's the real worker into a leaf it opened itself (so the fd
// never needs to leave this process), and then waits for the worker and
// exits with its own code.
func RunCgroupHelper(nodeTag string) error {
	conn := os.NewFile(3, "cgroup-helper-conn")
	defer conn.Close()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	fail1 := func(format string, args ...any) error {
		return enc.Encode(helperAck1{Err: fmt.Sprintf(format, args...)})
	}

	// Belt and suspenders: CLONE_NEWNS alone copies the parent's mount
	// table but does not by itself cut mount propagation, so make this
	// namespace's root (recursively) private before touching
	// /sys/fs/cgroup at all — nothing done here can then ever propagate
	// back to ate-ish's own namespace or the host's, regardless of how
	// the base image mounted things.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fail1("making / rprivate in the helper's own mount namespace: %v", err)
	}
	// Stage 5 (Cloud Run gen2) found /sys/fs/cgroup already mounted in a
	// way a plain `umount` refuses as EBUSY (the platform's own runtime
	// holds a reference); `umount -l` (MNT_DETACH) is the fallback. This
	// only ever touches this helper's own, already-private mount
	// namespace — never ate-ish's own, never the host's.
	if err := syscall.Unmount(ambientCgroupFSBase, 0); err != nil {
		_ = syscall.Unmount(ambientCgroupFSBase, syscall.MNT_DETACH)
	}
	if err := syscall.Mount("cgroup2", ambientCgroupFSBase, "cgroup2", 0, ""); err != nil {
		return fail1("mounting a private cgroup2 instance at %s: %v", ambientCgroupFSBase, err)
	}

	cgroup, err := currentCgroupPath(ambientCgroupFSBase)
	if err != nil {
		return fail1("finding this helper's own cgroup: %v", err)
	}
	ancestor, err := nearestDomainAncestor(ambientCgroupFSBase, cgroup)
	if err != nil {
		return fail1("finding a domain-type ancestor: %v", err)
	}
	// Same "no internal process constraint" as prepareLeafAt's own
	// comment: this helper is itself the one process resident in its
	// fresh private cgroup2 mount's root, which can coincide with
	// ancestor here too.
	if ancestor == cgroup {
		if err := selfMigrateOutOf(ancestor); err != nil {
			return fail1("moving the cgroup helper's own process out of %s: %v", ancestor, err)
		}
	}
	leaf := cgroupLeafFor(ancestor, nodeTag)
	if err := prepareWorkerCgroupLeaf(leaf); err != nil {
		return fail1("preparing the worker's cgroup leaf: %v", err)
	}
	leafFD, err := os.Open(leaf)
	if err != nil {
		return fail1("opening the worker's cgroup leaf: %v", err)
	}
	defer leafFD.Close()
	ignoreCgroups, err := hostLacksMemoryOrCPU(ancestor)
	if err != nil {
		return fail1("checking which cgroup controllers this host exposes: %v", err)
	}

	if err := enc.Encode(helperAck1{Ancestor: ancestor, IgnoreCgroups: ignoreCgroups}); err != nil {
		return fmt.Errorf("sending the leaf status to ate-ish: %w", err)
	}

	var launch helperLaunch
	if err := dec.Decode(&launch); err != nil {
		return fmt.Errorf("receiving the launch request from ate-ish: %w", err)
	}

	// The fix: clone3's CLONE_INTO_CGROUP runs here, in the same process
	// that opened leafFD, in the same (this helper's own) mount namespace
	// — the fd never crosses a namespace boundary, unlike the old design
	// that sent it back to ate-ish over SCM_RIGHTS for ate-ish itself to
	// consume. The worker still gets its own CLONE_NEWNS and the same
	// private cgroup2 remount startWorkerAmbient gives it (measured
	// necessary: inheriting the helper's mount namespace unchanged left
	// cgroup.Delegate's own drain step failing EINVAL moving itself into
	// "worker"; a fresh remount inside the worker's own further-private
	// namespace, matching the ambient path exactly, did not).
	cmdLine := shellJoin(launch.Argv)
	script := "mount --make-rprivate /; " +
		"umount /sys/fs/cgroup 2>&1 || umount -l /sys/fs/cgroup; " +
		"mount -t cgroup2 cgroup2 /sys/fs/cgroup && exec " + cmdLine
	worker := exec.Command("bash", "-c", script)
	worker.Env = launch.Env
	worker.Stdin = os.Stdin
	worker.Stdout = os.Stdout
	worker.Stderr = os.Stderr
	worker.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWCGROUP | syscall.CLONE_NEWNS,
		UseCgroupFD: true,
		CgroupFD:    int(leafFD.Fd()),
	}
	if err := worker.Start(); err != nil {
		return enc.Encode(helperAck2{Err: fmt.Sprintf("%v", err)})
	}
	if err := enc.Encode(helperAck2{Pid: worker.Process.Pid}); err != nil {
		return fmt.Errorf("sending the launch result to ate-ish: %w", err)
	}
	conn.Close()

	// No signal forwarding needed: worker.go now signals the real worker's
	// own pid (ack2.Pid above) directly, not this helper's — this helper's
	// only remaining job is to clone3 it cross-namespace-safely and then
	// wait for it, so its own exit mirrors the real worker's.
	waitErr := worker.Wait()
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		os.Exit(exitErr.ExitCode())
	}
	return waitErr
}
