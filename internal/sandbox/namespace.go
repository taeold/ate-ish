// Package sandbox is ate-ish's own unprivileged driver of gVisor's runsc:
// the held user+mount+network namespace every actor sandbox runs inside
// (docs/ROOTLESS.md), OCI bundle construction from a template and the
// existing per-digest exported image, and run/checkpoint/restore/delete
// through runsc in that one namespace. It replaces the vendored
// ateworker-gvisor subprocess this build deletes. The design (one held
// namespace, a systemd user scope per actor, runsc in "namespace-root"
// mode) is the one proven in the rootless spike, branch rootless-spike,
// docs/ROOTLESS-SPIKE.md rounds 2 and 3.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Namespace is the one user+mount+network(+pid) namespace this node holds
// for as long as it runs. Every runsc invocation, and every mount this
// package makes for an actor's bundle, happens inside it via nsenter. A
// sandbox is a child of the runsc call that starts it, not of the holder
// process itself, so per-actor limits come from the systemd user scope
// wrapped around that runsc call (scope.go), not from this namespace.
type Namespace struct {
	holder   *exec.Cmd
	pid      int // the held namespace's own pid, i.e. the "sleep infinity" process's
	stateDir string
}

// unprivilegedHint is appended to the error Open returns when the kernel
// itself refuses the unshare, naming the fix round 3 settled on
// (docs/ROOTLESS-SPIKE.md): Ubuntu's AppArmor profile for unprivileged
// user namespaces, and the sysctl that controls it directly.
const unprivilegedHint = `ate-ish could not create an unprivileged user+mount+network namespace.
On Ubuntu 23.10+, AppArmor restricts unprivileged user namespaces by default. Either:
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
or install a profile that allows this binary, e.g.:
  cat <<'EOF' | sudo tee /etc/apparmor.d/ate-ish-userns
abi <abi/4.0>,
include <tunables/global>
profile ate-ish-userns flags=(unconfined) {
  userns,
}
EOF
  sudo systemctl reload apparmor
Then retry.`

// ATE_SANDBOX_NO_USERNS, set to any non-empty value, skips namespace
// creation and fails immediately: the test switch G1 uses to exercise the
// unprivileged-userns-refused error path without actually disabling user
// namespaces on the host.
const noUsernsEnv = "ATE_SANDBOX_NO_USERNS"

// Open unshares a fresh user+mount+network+pid namespace, mapping this
// process's own uid/gid to root inside it (ID mapping beyond root is
// stage C's job; for now every sandbox under this namespace runs as a
// single mapped root, logged once), and holds it open with a sleep
// placeholder exactly as the spike's holder_start did. stateDir is where
// runsc keeps its own --root state (container metadata); it must be
// resolved inside the held mount namespace, so every runsc call that
// enters this namespace sees the same one.
func Open(stateDir string) (*Namespace, error) {
	if os.Getenv(noUsernsEnv) != "" {
		return nil, fmt.Errorf("%s is set: simulating unprivileged user namespaces being refused.\n%s", noUsernsEnv, unprivilegedHint)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	holder := exec.Command("unshare", "--user", "--map-root-user", "--net", "--mount", "--pid",
		"--fork", "--propagation=private", "sleep", "infinity")
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		return nil, fmt.Errorf("unshare: %w\n%s", err, unprivilegedHint)
	}
	n := &Namespace{holder: holder, stateDir: stateDir}
	pid, err := waitForHeldPID(holder.Process.Pid, 5*time.Second)
	if err != nil {
		n.Close()
		return nil, fmt.Errorf("waiting for the held namespace: %w\n%s", err, unprivilegedHint)
	}
	n.pid = pid
	logUIDMappingOnce()
	return n, nil
}

// waitForHeldPID polls unsharePID's own child list for the "sleep
// infinity" process unshare's --fork created: that process, not unshare
// itself, is the one whose /proc/<pid>/ns/* are the namespaces to enter
// (matching the spike's node.sh holder_start, which looked the same
// process up by name). Reading unsharePID's own children, rather than a
// system-wide pgrep by command name, keeps this correct with more than
// one node (and so more than one held namespace) on the same host.
func waitForHeldPID(unsharePID int, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	childrenFile := fmt.Sprintf("/proc/%d/task/%d/children", unsharePID, unsharePID)
	for {
		if data, err := os.ReadFile(childrenFile); err == nil {
			for _, f := range strings.Fields(string(data)) {
				if pid, err := strconv.Atoi(f); err == nil {
					return pid, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("no child of unshare (pid %d) appeared within %s", unsharePID, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var loggedUIDMapping = false

// logUIDMappingOnce logs, once per node instance, that this build only
// maps root inside the held namespace: a template whose image runs as a
// non-root user (USER node, etc.) still works (gVisor's own emulated
// filesystem gives every in-sandbox uid its own view), but a real subuid
// range (newuidmap/newgidmap, rootless Podman's own approach) is not wired
// up yet; that is stage C's job (docs/ROOTLESS.md).
func logUIDMappingOnce() {
	if loggedUIDMapping {
		return
	}
	loggedUIDMapping = true
	fmt.Fprintln(os.Stderr, "ate-ish: held namespace maps only root (no subuid range yet); non-root actor images still run under gVisor's own uid emulation")
}

// Close tears the held namespace down. The holder is pid 1 of its own pid
// namespace, so it ignores SIGTERM; the spike's own holder_stop used
// SIGKILL for the same reason.
func (n *Namespace) Close() error {
	if n == nil || n.holder == nil || n.holder.Process == nil {
		return nil
	}
	err := n.holder.Process.Kill()
	n.holder.Wait()
	return err
}

// nsenterArgs is the --user/--mount/--net triple every Enter call uses,
// matching the spike's own ns() helper exactly.
func (n *Namespace) nsenterArgs() []string {
	return []string{
		"--user=/proc/" + strconv.Itoa(n.pid) + "/ns/user",
		"--mount=/proc/" + strconv.Itoa(n.pid) + "/ns/mnt",
		"--net=/proc/" + strconv.Itoa(n.pid) + "/ns/net",
		"--preserve-credentials",
		"--",
	}
}

// Enter returns a command that runs name/args inside the held namespace.
func (n *Namespace) Enter(name string, args ...string) *exec.Cmd {
	full := append(append([]string{}, n.nsenterArgs()...), name)
	full = append(full, args...)
	return exec.Command("nsenter", full...)
}

// EnterSh runs script as `sh -c` inside the held namespace, for the small
// shell one-liners (mkdir/mount) the spike's node.sh used directly.
func (n *Namespace) EnterSh(ctx context.Context, script string) ([]byte, error) {
	cmd := n.Enter("sh", "-c", script)
	return cmd.CombinedOutput()
}
