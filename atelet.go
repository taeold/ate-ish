package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
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

// cgroupLeafFor is where startWorker moves this process before forking the
// worker subprocess with a fresh cgroup namespace (see its doc comment).
// Stage 4 (two nodes, one host): the real cgroupfs is not namespaced away
// from the ate-ish parent process (only the forked ateworker-gvisor gets
// CLONE_NEWCGROUP), so a single fixed leaf would have a second node's
// startup clean up and reuse the first node's still-live leaf. nodeTag
// (ATE_DIR's base name, stable across one node's restarts, distinct between
// nodes) keeps each node in its own leaf.
func cgroupLeafFor(nodeTag string) string { return "/sys/fs/cgroup/ate-ish-worker-" + nodeTag }

// moveSelfIntoLeafCgroup and cleanupLeafCgroup replicate stage 0's cgroup
// namespace trick: internal/ateworker/cgroup.Delegate needs
// /proc/self/cgroup to read "0::/" inside the worker's own cgroup
// namespace, which only happens when the namespace root at clone(2) time is
// a leaf this process already sits in.
func moveSelfIntoLeafCgroup(leaf string) error {
	cleanupLeafCgroup(leaf)
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		return err
	}
	return os.WriteFile(leaf+"/cgroup.procs", []byte(fmt.Sprintf("%d", os.Getpid())), 0o644)
}

// cleanupLeafCgroup undoes what a previous run's worker left behind:
// cgroup.Delegate enables every controller in subtree_control and moves
// itself into a "worker" child leaf, and cgroupfs refuses to add a process
// to a non-leaf cgroup (the "no internal processes" rule).
func cleanupLeafCgroup(leaf string) {
	for _, c := range []string{"cpuset", "cpu", "io", "memory", "hugetlb", "pids", "rdma"} {
		_ = os.WriteFile(leaf+"/cgroup.subtree_control", []byte("-"+c), 0o644)
	}
	_ = os.Remove(leaf + "/worker")
	_ = os.Remove(leaf)
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
// give it one (see moveSelfIntoLeafCgroup).
func startAteworker(cfg ateworkerConfig) (*exec.Cmd, error) {
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

	if err := moveSelfIntoLeafCgroup(cgroupLeafFor(cfg.nodeTag)); err != nil {
		return nil, fmt.Errorf("moving into leaf cgroup: %w", err)
	}

	logFile, err := os.Create(cfg.logFile)
	if err != nil {
		return nil, err
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
	script := "mount --make-rprivate / && umount /sys/fs/cgroup && mount -t cgroup2 cgroup2 /sys/fs/cgroup && exec " + cmdLine

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "KO_DATA_PATH="+cfg.kodata)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWCGROUP | syscall.CLONE_NEWNS}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ateworker-gvisor: %w", err)
	}
	return cmd, nil
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
