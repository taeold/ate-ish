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
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	multiID  bool // Open's own idmap.go result; see MultiIDMapping
}

// MultiIDMapping reports whether Open mapped more than root (idmap.go):
// true means a non-root uid this namespace's mapping covers can actually
// own a file on the host side, false means every file this namespace
// creates or chowns ends up owned by this node's own single mapped id
// regardless of what uid asked for it. imageRootfsDir's own export cache
// must key on this (bundle.go): the same image digest exported once
// under each mode produces a different, non-interchangeable result.
func (n *Namespace) MultiIDMapping() bool { return n != nil && n.multiID }

// unprivilegedHint is appended to the error Open returns when the kernel
// itself refuses the unshare, naming the fix round 3 settled on
// (docs/ROOTLESS-SPIKE.md): Ubuntu's AppArmor profile for unprivileged
// user namespaces, and the sysctl that controls it directly.
const unprivilegedHint = `ate-ish could not create an unprivileged user+mount+network namespace.
On Ubuntu 23.10 and later, AppArmor restricts unprivileged user namespaces by default.
Allow them for a private copy of unshare that only ate-ish uses (sudo once):
  sudo install -D -m 0755 /usr/bin/unshare ` + privateUnshare + `
  sudo tee /etc/apparmor.d/ate-ish-unshare >/dev/null <<'EOF'
abi <abi/4.0>,
include <tunables/global>
profile ate-ish-unshare ` + privateUnshare + ` flags=(unconfined) {
  userns,
}
EOF
  sudo apparmor_parser -r /etc/apparmor.d/ate-ish-unshare
Then retry.`

// privateUnshare is where the Ubuntu setup above puts ate-ish's own copy of
// unshare. Granting userns to this path, not /usr/bin/unshare, keeps the
// restriction in place for every other program on the machine.
const privateUnshare = "/usr/local/libexec/ate-ish/unshare"

// unshareBinary picks ATE_UNSHARE, else the private copy when it exists, else
// unshare from PATH.
func unshareBinary() string {
	if p := os.Getenv("ATE_UNSHARE"); p != "" {
		return p
	}
	if _, err := os.Stat(privateUnshare); err == nil {
		return privateUnshare
	}
	return "unshare"
}

// ATE_SANDBOX_NO_USERNS, set to any non-empty value, skips namespace
// creation and fails immediately: the test switch G1 uses to exercise the
// unprivileged-userns-refused error path without actually disabling user
// namespaces on the host.
const noUsernsEnv = "ATE_SANDBOX_NO_USERNS"

// Open unshares a fresh user+mount+network+pid namespace and holds it
// open with a sleep placeholder exactly as the spike's holder_start did.
// stateDir is where runsc keeps its own --root state (container
// metadata); it must be resolved inside the held mount namespace, so
// every runsc call that enters this namespace sees the same one.
//
// ID mapping: detectIDMapping decides, once, whether this host can map
// more than root (newuidmap/newgidmap on PATH, a /etc/subuid and
// /etc/subgid range for this user, as rootless Podman itself requires).
// When it can, the held namespace unshares with no mapping of its own
// and applyIDMapping writes root plus that range through newuidmap/
// newgidmap before anything runs inside it. When it can't, the
// namespace falls back to --map-root-user (this process's own id to
// root, the only mapping that needs no helper), and logUIDMappingOnce
// says so: a template whose image declares a non-root USER will still
// boot, but every file operation its own uid performs through the gofer
// resolves to the kernel's overflow id on the host side and fails with
// EACCES.
func Open(stateDir, runscPath string) (*Namespace, error) {
	if os.Getenv(noUsernsEnv) != "" {
		return nil, fmt.Errorf("%s is set: simulating unprivileged user namespaces being refused.\n%s", noUsernsEnv, unprivilegedHint)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	reapOrphans(stateDir, runscPath)

	mapping, fallbackReason := detectIDMapping()
	n, err := openHolder(mapping)
	if err != nil {
		return nil, err
	}
	if mapping.multi {
		if err := applyIDMapping(n.pid, mapping); err != nil {
			n.Close()
			fallbackReason = fmt.Sprintf("newuidmap/newgidmap failed: %v", err)
			if n, err = openHolder(idMapping{}); err != nil {
				return nil, err
			}
			mapping.multi = false
		}
	}
	n.stateDir = stateDir
	n.multiID = mapping.multi
	logUIDMappingOnce(mapping, fallbackReason)
	if err := os.WriteFile(holderPIDFile(stateDir), []byte(strconv.Itoa(n.pid)), 0o644); err != nil {
		n.Close()
		return nil, fmt.Errorf("recording the held namespace's pid: %w", err)
	}
	return n, nil
}

// openHolder starts the "sleep infinity" placeholder and waits for its
// pid, mapping only root itself (via unshare --map-root-user) when
// mapping.multi is false, or leaving the id map empty and pending
// (Open's own applyIDMapping call writes it) when true.
func openHolder(mapping idMapping) (*Namespace, error) {
	args := []string{"--user"}
	if !mapping.multi {
		args = append(args, "--map-root-user")
	}
	args = append(args, "--net", "--mount", "--pid", "--fork", "--propagation=private", "sleep", "infinity")
	holder := exec.Command(unshareBinary(), args...)
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		return nil, fmt.Errorf("unshare: %w\n%s", err, unprivilegedHint)
	}
	n := &Namespace{holder: holder}
	pid, err := waitForHeldPID(holder.Process.Pid, 5*time.Second)
	if err != nil {
		n.Close()
		return nil, fmt.Errorf("waiting for the held namespace: %w\n%s", err, unprivilegedHint)
	}
	n.pid = pid
	return n, nil
}

// holderPIDFile is where Open records the held namespace's own pid
// (n.pid, the "sleep infinity" placeholder), so a later Open against the
// same stateDir can tell a holder a SIGKILLed node never closed from one
// this package itself is about to replace. Sibling to stateDir rather
// than inside it, so it is never mistaken for one of runsc's own
// per-sandbox state entries.
func holderPIDFile(stateDir string) string {
	return filepath.Join(filepath.Dir(stateDir), "holder.pid")
}

// reapOrphans runs at the start of every Open, before this node creates
// its own holder. A node that exits via SuspendAll (SIGTERM/SIGINT)
// already suspends or stops every sandbox it runs, so normally there is
// nothing here to find. A node killed with SIGKILL gets no such chance:
// its held namespace (holder) and every runsc sandbox inside it are not
// its children in a way SIGKILL reaps, so they outlive it. Since
// Namespace.Enter never joins the holder's pid namespace (only
// user/mount/net), every runsc sandbox this package starts is already a
// plain process in the host's own pid namespace; runscPath invoked
// directly, with no nsenter, can list and kill them by the stateDir they
// share, regardless of whether the old holder is still alive. This
// chooses kill-and-delete over restore: an orphan's checkpoint state (if
// any) belongs to the generation record, not to the runsc process left
// behind, so restoring it here would race whatever that record's actual
// owner does next.
func reapOrphans(stateDir, runscPath string) {
	out, err := exec.Command(runscPath, "--root="+stateDir, "list", "-quiet", "-sandbox").Output()
	if err != nil {
		// No prior state directory, or nothing runsc recognizes in it:
		// the common case, not an error worth logging.
		out = nil
	}
	for _, id := range strings.Fields(string(out)) {
		_, err := exec.Command(runscPath, "--root="+stateDir, "delete", "-force", id).CombinedOutput()
		slog.Warn("reaped orphaned sandbox", "id", id, "state_dir", stateDir, "err", err)
	}

	data, err := os.ReadFile(holderPIDFile(stateDir))
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
		if proc, err := os.FindProcess(pid); err == nil {
			if err := proc.Signal(syscall.Signal(0)); err == nil {
				slog.Warn("killing orphaned held namespace", "pid", pid, "state_dir", stateDir)
				proc.Kill()
			}
		}
	}
	os.Remove(holderPIDFile(stateDir))
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

// logUIDMappingOnce logs, once per node instance, which of the two id
// mappings Open settled on.
func logUIDMappingOnce(mapping idMapping, fallbackReason string) {
	if loggedUIDMapping {
		return
	}
	loggedUIDMapping = true
	if mapping.multi {
		fmt.Fprintf(os.Stderr, "ate-ish: held namespace maps root plus subuid %d-%d, subgid %d-%d (rootless Podman's own approach); non-root actor images work\n",
			mapping.uid.start, mapping.uid.start+mapping.uid.length-1, mapping.gid.start, mapping.gid.start+mapping.gid.length-1)
		return
	}
	fmt.Fprintf(os.Stderr, "ate-ish: held namespace maps root only (%s); non-root images will fail\n", fallbackReason)
}

// Close tears the held namespace down. The holder is pid 1 of its own pid
// namespace, so it ignores SIGTERM; the spike's own holder_stop used
// SIGKILL for the same reason.
func (n *Namespace) Close() error {
	if n == nil || n.holder == nil || n.holder.Process == nil {
		return nil
	}
	if n.stateDir != "" {
		os.Remove(holderPIDFile(n.stateDir))
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
