package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Limits are one actor's resource limits, applied through a systemd user
// scope around its runsc run/restore call (scope()). Zero values mean
// "systemd-run's own default" (no -p flag for that resource).
type Limits struct {
	MemoryMax string // e.g. "1G"
	CPUQuota  string // e.g. "100%"
	TasksMax  int
}

// runscFlags is the one namespace-root runsc mode this build uses for
// run, checkpoint and restore alike (docs/ROOTLESS.md): no sandbox
// network at all (egress and ingress are per-destination host sockets,
// not a veth), no gVisor-native overlay (the bundle's rootfs is already
// an unprivileged tmpfs+overlay the node built), host-uds=open (every
// socket a sandbox touches is one the node already created and the
// sandbox only ever dials, never binds), cgroups left alone (this
// process is not root and cannot delegate a cgroup; limits come from the
// systemd scope instead), platform=systrap (the one platform that needs
// no privilege at all: no KVM, no ptrace of a process outside this user
// namespace).
func (n *Namespace) runscFlags(runscPath string) []string {
	flags := []string{runscPath,
		"--network=none", "--host-uds=open", "--overlay2=none",
		"--ignore-cgroups", "--platform=systrap", "--root=" + n.stateDir}
	if d := os.Getenv("ATE_SANDBOX_DEBUG"); d != "" {
		flags = append(flags, "--debug", "--debug-log="+d, "--alsologtostderr")
	}
	return flags
}

// run builds the full nsenter+runsc argv for one runsc subcommand
// (subArgs, e.g. []string{"run", "--bundle=...", "--detach", id}) inside
// the held namespace.
func (n *Namespace) runsc(runscPath string, subArgs ...string) *exec.Cmd {
	args := append(n.runscFlags(runscPath), subArgs...)
	return n.Enter(args[0], args[1:]...)
}

// Run starts id (a fresh actor, never checkpointed) detached, wrapped in
// a systemd user scope carrying limits. The sandbox's own stdout/stderr
// (and, with --detach, runsc's own boot log for as long as the sandbox
// lives, since the detached process inherits these same fds) land in
// bundleDir/console.log.
func (n *Namespace) Run(ctx context.Context, runscPath, bundleDir, id string, limits Limits) error {
	t0 := time.Now()
	console, err := os.Create(filepath.Join(bundleDir, "console.log"))
	if err != nil {
		return err
	}
	defer console.Close()
	cmd := n.runsc(runscPath, "run", "--bundle="+bundleDir, "--detach", id)
	cmd.Stdout, cmd.Stderr = console, console
	if _, err := runInScope(id, limits, cmd); err != nil {
		return fmt.Errorf("runsc run %s: %w (see %s)", id, err, console.Name())
	}
	slog.Info("sandbox run", "id", id, "took_ms", time.Since(t0).Milliseconds())
	return nil
}

// Checkpoint saves id's state to imageDir. The caller must have already
// quiesced anything holding a bound host socket (gVisor refuses to save
// those); CheckpointForwarders does that for the egress forwarders this
// package's init starts.
func (n *Namespace) Checkpoint(ctx context.Context, runscPath, imageDir, id string) error {
	t0 := time.Now()
	if out, err := n.runsc(runscPath, "checkpoint", "--image-path="+imageDir, id).CombinedOutput(); err != nil {
		return fmt.Errorf("runsc checkpoint %s: %w: %s", id, err, out)
	}
	slog.Info("sandbox checkpoint", "id", id, "took_ms", time.Since(t0).Milliseconds())
	return nil
}

// Restore starts id from a checkpoint at imageDir, detached, wrapped in a
// systemd user scope exactly as Run is. The caller deletes any stale
// runsc state for id first (restore refuses to reuse a live id).
func (n *Namespace) Restore(ctx context.Context, runscPath, bundleDir, imageDir, id string, limits Limits) error {
	t0 := time.Now()
	n.runsc(runscPath, "delete", "-force", id).Run() // best effort; id may not exist yet
	console, err := os.Create(filepath.Join(bundleDir, "console.log"))
	if err != nil {
		return err
	}
	defer console.Close()
	cmd := n.runsc(runscPath, "restore", "--bundle="+bundleDir, "--image-path="+imageDir, "--detach", id)
	cmd.Stdout, cmd.Stderr = console, console
	if _, err := runInScope(id, limits, cmd); err != nil {
		return fmt.Errorf("runsc restore %s: %w (see %s)", id, err, console.Name())
	}
	slog.Info("sandbox restore", "id", id, "took_ms", time.Since(t0).Milliseconds())
	return nil
}

// Kill sends sig (a signal name runsc understands, e.g. "USR1", "KILL")
// to id's pid 1.
func (n *Namespace) Kill(ctx context.Context, runscPath, id, sig string) error {
	out, err := n.runsc(runscPath, "kill", id, sig).CombinedOutput()
	if err != nil {
		return fmt.Errorf("runsc kill %s %s: %w: %s", id, sig, err, out)
	}
	return nil
}

// Delete removes id's runsc state. Safe to call on an id that is not
// running (used as teardown-time best effort).
func (n *Namespace) Delete(ctx context.Context, runscPath, id string) error {
	out, err := n.runsc(runscPath, "delete", "-force", id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("runsc delete %s: %w: %s", id, err, out)
	}
	return nil
}

// Exec runs args as id's own `runsc exec`, used by probes and tests, not
// by the actor boot path itself.
func (n *Namespace) Exec(ctx context.Context, runscPath, id string, args ...string) ([]byte, error) {
	return n.runsc(runscPath, append([]string{"exec", id}, args...)...).CombinedOutput()
}

var scopeFallbackLogged sync.Once

// runInScope runs cmd inside a systemd --user scope carrying limits, for
// the per-actor memory/CPU/task limits docs/ROOTLESS.md asks for. Without
// a user systemd session (no XDG_RUNTIME_DIR bus; the common case in a
// container with no login manager), systemd-run fails immediately; this
// falls back to running cmd directly, with one logged line, rather than
// failing every boot.
func runInScope(id string, limits Limits, cmd *exec.Cmd) ([]byte, error) {
	args := []string{"--user", "--scope", "-q",
		"--unit=ate-" + id + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)}
	if limits.MemoryMax != "" {
		args = append(args, "-p", "MemoryMax="+limits.MemoryMax)
	}
	if limits.CPUQuota != "" {
		args = append(args, "-p", "CPUQuota="+limits.CPUQuota)
	}
	if limits.TasksMax > 0 {
		args = append(args, "-p", "TasksMax="+strconv.Itoa(limits.TasksMax))
	}
	args = append(args, cmd.Path)
	args = append(args, cmd.Args[1:]...)
	scoped := exec.Command("systemd-run", args...)
	scoped.Env = append(os.Environ(), "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(os.Getuid()))
	scoped.Stdout, scoped.Stderr = cmd.Stdout, cmd.Stderr
	if scoped.Stdout == nil && scoped.Stderr == nil {
		out, err := scoped.CombinedOutput()
		if err == nil {
			return out, nil
		}
		scopeFallbackLogged.Do(func() {
			slog.Warn("no user systemd session; running actor sandboxes with no per-actor limits", "err", err)
		})
		return cmd.CombinedOutput()
	}
	if err := scoped.Run(); err == nil {
		return nil, nil
	}
	scopeFallbackLogged.Do(func() {
		slog.Warn("no user systemd session; running actor sandboxes with no per-actor limits")
	})
	return nil, cmd.Run()
}
