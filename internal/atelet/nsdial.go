package atelet

import (
	"context"
	"fmt"
	"net"

	"github.com/taeold/ate-ish/third_party/substrate/exported/netns"
	"golang.org/x/sys/unix"
)

// gatewayNetNSPath returns the path of the actor's gateway namespace (the
// one holding the host side of its veth, atunnel's egress listener, and its
// own DNS relay) as the vendored worker subprocess sees it:
// /run/netns/worker-actor:<uid>-at, named in
// internal/ateworker/actornet.SandboxGatewayNetNSName.
//
// The worker runs in its own private mount namespace (see startWorker, for
// cgroup.Delegate's benefit), so that bind mount is invisible at this path
// from ate-ish's own mount namespace: opening it directly returns EINVAL
// from setns. /proc/<pid>/root resolves a path through a process's own
// mount namespace from any other process that can ptrace it (root can), so
// going through the worker subprocess's /proc entry reaches the real
// namespace without this process entering the worker's mount namespace
// itself.
func gatewayNetNSPath(workerPID int, actorUID string) string {
	return fmt.Sprintf("/proc/%d/root/run/netns/worker-actor:%s-at", workerPID, actorUID)
}

// dialViaActorGateway dials addr (expected to be the actor's veth address,
// 169.254.17.2:<port>) from inside its gateway namespace: the only place
// that address is routable from, since actornet gives every actor's veth
// pair its own named namespace pair rather than attaching to the host's root
// network namespace.
func dialViaActorGateway(ctx context.Context, workerPID int, actorUID string) func(context.Context, string, string) (net.Conn, error) {
	path := gatewayNetNSPath(workerPID, actorUID)
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		fd, err := unix.Open(path, unix.O_RDONLY, 0)
		if err != nil {
			return nil, fmt.Errorf("opening actor gateway namespace %s: %w", path, err)
		}
		ns := netns.Handle(fd)
		defer ns.Close()
		return netns.Dialer(ns)(ctx, network, addr)
	}
}
