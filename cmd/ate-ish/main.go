// ate-ish is the easiest way to run Agent Substrate actors: one binary, a
// directory or a bucket, a laptop, a VM, or Cloud Run.
//
//	ate-ish            runs a node (ATE_ROLE=all|ingress|worker; worker needs root)
//	ate-ish VERB KIND   the kubectl-ate CLI (create, get, suspend, delete)
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/taeold/ate-ish/internal/atelet"
	"github.com/taeold/ate-ish/internal/cli"
	"github.com/taeold/ate-ish/internal/envutil"
	"github.com/taeold/ate-ish/internal/ingress"
	"github.com/taeold/ate-ish/internal/store"

	// Root certificates for images without them, such as node:24-slim.
	_ "golang.org/x/crypto/x509roots/fallback"
)

func main() {
	var err error
	switch cmd := append(os.Args[1:], "")[0]; cmd {
	case "":
		err = serve()
	case "__cgroup-helper":
		// Hidden: internal/atelet's own private-mount-namespace child,
		// re-exec'd by its cgroup helper. Not a user-facing command.
		err = atelet.RunCgroupHelper(os.Args[2])
	default:
		err = cli.Run(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ate-ish:", err)
		os.Exit(1)
	}
}

// node is one running ingress+worker (or just one of the two), the shape
// serve() starts.
type node struct {
	in  *ingress.Ingress
	w   *atelet.Worker
	srv *http.Server
}

// startNode builds the ingress/worker/http.Server for role against store b,
// but does not call ListenAndServe: serve() drives that (and shutdown).
func startNode(ctx context.Context, role, port string, b store.Store, dir string, capacity int) (*node, error) {
	mux := http.NewServeMux()
	n := &node{}
	if role == "all" || role == "ingress" {
		n.in = ingress.New(b)
		n.in.Routes(mux)
	}
	if role == "all" || role == "worker" {
		addr := "127.0.0.1:" + port
		if role == "worker" {
			ip, err := metadata("instance/network-interfaces/0/ip")
			if err != nil {
				return nil, err
			}
			addr = ip + ":" + port
		}
		var err error
		if n.w, err = atelet.New(ctx, b, addr, capacity, dir); err != nil {
			return nil, err
		}
		n.w.Routes(mux)
	}
	if role == "all" {
		idle := envutil.Get("ATE_IDLE_SECONDS", "600")
		go reapLoop(ctx, n.in, idle)
	}
	n.srv = &http.Server{Addr: ":" + port, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if n.in != nil && r.Header.Get("ate-target-actor") != "" {
			n.in.Route(rw, r)
			return
		}
		mux.ServeHTTP(rw, r)
	})}
	return n, nil
}

func reapLoop(ctx context.Context, in *ingress.Ingress, idleSeconds string) {
	for range time.Tick(time.Minute) {
		if reaped, err := in.Reap(ctx, idleSeconds); err != nil || len(reaped) > 0 {
			slog.Info("reaped", "actors", reaped, "err", err)
		}
	}
}

// shutdown suspends every hosted actor (if this node runs a worker) and
// closes the listener; serve()'s SIGTERM/SIGINT handler calls this.
func (n *node) shutdown(ctx context.Context) {
	if n.w != nil {
		n.w.SuspendAll(ctx)
	}
	n.srv.Close()
}

// defaultDir is a node's own runtime-state directory (pki, hosted actors'
// OCI bundles, logs) when ATE_DIR is not set: $XDG_STATE_HOME/ate-ish/node,
// or ~/.local/state/ate-ish/node.
func defaultDir() string {
	base := envutil.Get("XDG_STATE_HOME", "")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/root"
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ate-ish", "node")
}

func serve() error {
	ctx := context.Background()
	role := envutil.Get("ATE_ROLE", "all")
	port := envutil.Get("ATE_LISTEN", "8080")
	if (role == "all" || role == "worker") && os.Geteuid() != 0 {
		return fmt.Errorf("ATE_ROLE=%s runs the vendored gVisor worker, which needs root (cgroup delegation, network namespaces): run as sudo", role)
	}
	if role == "all" || role == "worker" {
		if err := atelet.EnsureRunsc(); err != nil {
			return fmt.Errorf("fetching runsc: %w", err)
		}
	}
	b, err := store.New(ctx)
	if err != nil {
		return err
	}
	capacity, _ := strconv.Atoi(envutil.Get("ATE_CAPACITY", "4"))
	dir := envutil.Get("ATE_DIR", defaultDir())
	n, err := startNode(ctx, role, port, b, dir, capacity)
	if err != nil {
		return err
	}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
		<-stop
		n.shutdown(ctx)
	}()
	slog.Info("serving", "role", role, "port", port)
	if err := n.srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func metadata(path string) (string, error) {
	req, _ := http.NewRequest("GET", "http://metadata.google.internal/computeMetadata/v1/"+path, nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(data)), err
}
