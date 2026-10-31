// cgrouphelper.go: see atelet.go's INCIDENT comment on startAteworker for
// why this file exists. ate-ish's own process must never remount
// /sys/fs/cgroup in its own (shared) mount namespace. prepareWorkerCgroupFD
// is the one entry point: it probes the ambient cgroup2 view read-only
// first (every ordinary host, including this dev VM) and only reaches for
// a mount at all when that probe fails (so far, only Cloud Run gen2's
// restricted ambient view) — and even then, the mount happens inside a
// throwaway helper subprocess's own private mount namespace
// (CLONE_NEWNS), never this one's. The helper hands back only a plain
// file descriptor for the worker's cgroup leaf (over a unix socketpair,
// SCM_RIGHTS), which stays valid in this process's hands regardless of
// which mount namespace "sees" the path it came from.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// ambientCgroupFSBase is the one real cgroup2 mountpoint on every host
// ate-ish has run on outside Cloud Run gen2 (this dev VM included).
const ambientCgroupFSBase = "/sys/fs/cgroup"

// prepareWorkerCgroupFD returns an open file descriptor for the worker's
// cgroup leaf (ready for clone3's CLONE_INTO_CGROUP), the leaf's parent
// cgroup path (for logging and cgroupLeafFor), and whether this host lacks
// the memory/cpu controllers runsc's own per-actor sandbox cgroup needs.
func prepareWorkerCgroupFD(nodeTag string) (leafFD *os.File, ancestor string, ignoreCgroups bool, err error) {
	if _, err := os.ReadFile(ambientCgroupFSBase + "/cgroup.controllers"); err == nil {
		// The ambient view already works (every local/VM host measured so
		// far): use it directly, no mount of any kind.
		return prepareLeafAt(ambientCgroupFSBase, nodeTag)
	}
	// Cloud Run gen2's own restricted ambient view (stage 5c's finding):
	// a throwaway helper subprocess mounts a fresh, private cgroup2
	// instance inside its own mount namespace and does the rest of this
	// work there, handing back just the leaf's fd.
	return prepareLeafViaHelper(nodeTag)
}

// prepareLeafAt is prepareWorkerCgroupFD's direct path: base already works.
func prepareLeafAt(base, nodeTag string) (leafFD *os.File, ancestor string, ignoreCgroups bool, err error) {
	cgroup, err := currentCgroupPath(base)
	if err != nil {
		return nil, "", false, fmt.Errorf("finding ate-ish's own cgroup: %w", err)
	}
	ancestor, err = nearestDomainAncestor(base, cgroup)
	if err != nil {
		return nil, "", false, fmt.Errorf("finding a domain-type ancestor of ate-ish's own cgroup: %w", err)
	}
	leaf := cgroupLeafFor(ancestor, nodeTag)
	if err := prepareWorkerCgroupLeaf(leaf); err != nil {
		return nil, "", false, fmt.Errorf("preparing the worker's cgroup leaf: %w", err)
	}
	f, err := os.Open(leaf)
	if err != nil {
		return nil, "", false, fmt.Errorf("opening the worker's cgroup leaf: %w", err)
	}
	// Stage 5e: ATE_IGNORE_CGROUPS=1 forces this locally, to test the
	// gen2-only --ignore-cgroups path on a host (like this one) that
	// actually has the controllers.
	if env("ATE_IGNORE_CGROUPS", "") == "1" {
		return f, ancestor, true, nil
	}
	lacks, err := hostLacksMemoryOrCPU(ancestor)
	if err != nil {
		f.Close()
		return nil, "", false, fmt.Errorf("checking which cgroup controllers this host exposes: %w", err)
	}
	return f, ancestor, lacks, nil
}

// cgroupHelperResponse is what the helper subprocess sends back over the
// socketpair, as the regular (non-control) bytes of one Sendmsg.
type cgroupHelperResponse struct {
	Ancestor      string
	IgnoreCgroups bool
	Err           string
}

// prepareLeafViaHelper spawns the helper (this same binary, re-exec'd with
// a hidden subcommand and CLONE_NEWNS) and receives its leaf fd.
func prepareLeafViaHelper(nodeTag string) (leafFD *os.File, ancestor string, ignoreCgroups bool, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, "", false, fmt.Errorf("socketpair for the cgroup helper: %w", err)
	}
	parentConn := os.NewFile(uintptr(fds[0]), "cgroup-helper-parent")
	defer parentConn.Close()
	childConn := os.NewFile(uintptr(fds[1]), "cgroup-helper-child")

	exe, err := os.Executable()
	if err != nil {
		childConn.Close()
		return nil, "", false, err
	}
	cmd := exec.Command(exe, "__cgroup-helper", nodeTag)
	cmd.ExtraFiles = []*os.File{childConn} // becomes fd 3 in the child
	cmd.Stderr = os.Stderr
	// The only mount this helper ever does lands here, in a mount
	// namespace that dies with it — never ate-ish's own, never the host's.
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if err := cmd.Start(); err != nil {
		childConn.Close()
		return nil, "", false, fmt.Errorf("starting the cgroup helper: %w", err)
	}
	childConn.Close()

	buf := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, recvErr := unix.Recvmsg(int(parentConn.Fd()), buf, oob, 0)
	waitErr := cmd.Wait()
	if recvErr != nil {
		return nil, "", false, fmt.Errorf("receiving from the cgroup helper: %w (exit: %v)", recvErr, waitErr)
	}
	var resp cgroupHelperResponse
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return nil, "", false, fmt.Errorf("parsing the cgroup helper's response: %w", err)
	}
	if resp.Err != "" {
		return nil, "", false, fmt.Errorf("cgroup helper: %s", resp.Err)
	}
	if waitErr != nil {
		return nil, "", false, fmt.Errorf("cgroup helper exited uncleanly: %w", waitErr)
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return nil, "", false, fmt.Errorf("the cgroup helper sent no file descriptor")
	}
	rights, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(rights) == 0 {
		return nil, "", false, fmt.Errorf("the cgroup helper's file descriptor message was empty")
	}
	return os.NewFile(uintptr(rights[0]), resp.Ancestor), resp.Ancestor, resp.IgnoreCgroups, nil
}

// runCgroupHelper is the child side of prepareLeafViaHelper, dispatched
// from main.go as `ate-ish __cgroup-helper <nodeTag>`. It runs alone in
// its own private mount namespace (CLONE_NEWNS, set by the parent's
// SysProcAttr on this exact process), so the only mount it ever makes
// — a fresh cgroup2 instance at a brand-new, empty directory, never
// /sys/fs/cgroup itself — can never be seen outside this one short-lived
// process.
func runCgroupHelper(nodeTag string) error {
	conn := os.NewFile(3, "cgroup-helper-conn")
	defer conn.Close()
	reply := func(fd int, resp cgroupHelperResponse) error {
		body, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		var rights []byte
		if fd >= 0 {
			rights = unix.UnixRights(fd)
		}
		return unix.Sendmsg(int(conn.Fd()), body, rights, nil, 0)
	}
	fail := func(format string, args ...any) error {
		return reply(-1, cgroupHelperResponse{Err: fmt.Sprintf(format, args...)})
	}

	base, err := os.MkdirTemp("", "ate-ish-cgroup2-")
	if err != nil {
		return fail("making a private mountpoint: %v", err)
	}
	defer os.RemoveAll(base)
	// A fresh cgroup2 instance is additive by construction (nothing is
	// unmounted to make room for it) and, inside this process's own
	// private mount namespace, cannot be observed — let alone disturbed
	// — from outside it.
	if err := syscall.Mount("cgroup2", base, "cgroup2", 0, ""); err != nil {
		return fail("mounting a private cgroup2 instance at %s: %v", base, err)
	}
	defer syscall.Unmount(base, 0)

	cgroup, err := currentCgroupPath(base)
	if err != nil {
		return fail("finding this helper's own cgroup: %v", err)
	}
	ancestor, err := nearestDomainAncestor(base, cgroup)
	if err != nil {
		return fail("finding a domain-type ancestor: %v", err)
	}
	leaf := cgroupLeafFor(ancestor, nodeTag)
	if err := prepareWorkerCgroupLeaf(leaf); err != nil {
		return fail("preparing the worker's cgroup leaf: %v", err)
	}
	f, err := os.Open(leaf)
	if err != nil {
		return fail("opening the worker's cgroup leaf: %v", err)
	}
	defer f.Close()
	ignoreCgroups := env("ATE_IGNORE_CGROUPS", "") == "1"
	if !ignoreCgroups {
		lacks, err := hostLacksMemoryOrCPU(ancestor)
		if err != nil {
			return fail("checking which cgroup controllers this host exposes: %v", err)
		}
		ignoreCgroups = lacks
	}
	return reply(int(f.Fd()), cgroupHelperResponse{Ancestor: ancestor, IgnoreCgroups: ignoreCgroups})
}
