// ate-ish is a small Agent Substrate for Cloud Run: one program, one bucket.
//
//	ate-ish serve    the server; ATE_ROLE=all|ingress|worker (worker needs root)
//	ate-ish dev      the local, laptop-facing path: one node, one actor, in the foreground
//	ate-ish deploy   builds, pushes, deploys to Cloud Run, and creates the actor, in one command
//	ate-ish ...      the CLI (create, get, suspend, delete)
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	// Root certificates for images without them, such as node:24-slim.
	_ "golang.org/x/crypto/x509roots/fallback"
)

func main() {
	var err error
	switch cmd := append(os.Args[1:], "")[0]; cmd {
	case "serve":
		err = serve()
	case "dev":
		err = runDev(os.Args[2:])
	case "deploy":
		err = runDeploy(os.Args[2:])
	case "__cgroup-helper":
		// Hidden: cgrouphelper.go's own private-mount-namespace child,
		// re-exec'd by prepareLeafViaHelper. Not a user-facing command.
		err = runCgroupHelper(os.Args[2])
	default:
		err = runCLI(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ate-ish:", err)
		os.Exit(1)
	}
}

// node is one running ingress+worker (or just one of the two), the shared
// shape serve() and runDev() both start.
type node struct {
	in  *ingress
	w   *worker
	srv *http.Server
}

// startNode builds the ingress/worker/http.Server for role against store b,
// but does not call ListenAndServe: serve() and runDev() each drive that
// (and shutdown) differently — serve() blocks the process on it, runDev()
// backgrounds it and keeps the foreground for the CLI-equivalent setup and
// the Ctrl-C wait.
func startNode(ctx context.Context, role, port string, b store, dir string, capacity int) (*node, error) {
	mux := http.NewServeMux()
	n := &node{}
	if role == "all" || role == "ingress" {
		n.in = &ingress{store: b, placing: map[string]*placement{}}
		n.in.routes(mux)
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
		if n.w, err = newWorker(ctx, b, addr, capacity, dir); err != nil {
			return nil, err
		}
		n.w.routes(mux)
	}
	if role == "all" {
		idle := env("ATE_IDLE_SECONDS", "600")
		go reapLoop(ctx, n.in, idle)
	}
	n.srv = &http.Server{Addr: ":" + port, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if n.in != nil && r.Header.Get("ate-target-actor") != "" {
			n.in.route(rw, r)
			return
		}
		mux.ServeHTTP(rw, r)
	})}
	return n, nil
}

func reapLoop(ctx context.Context, in *ingress, idleSeconds string) {
	for range time.Tick(time.Minute) {
		if reaped, err := in.reap(ctx, idleSeconds); err != nil || len(reaped) > 0 {
			slog.Info("reaped", "actors", reaped, "err", err)
		}
	}
}

// shutdown suspends every hosted actor (if this node runs a worker) and
// closes the listener; serve()'s SIGTERM/SIGINT handler and runDev()'s
// Ctrl-C handler both call this the same way.
func (n *node) shutdown(ctx context.Context) {
	if n.w != nil {
		n.w.suspendAll(ctx)
	}
	n.srv.Close()
}

func serve() error {
	ctx := context.Background()
	role := env("ATE_ROLE", "all")
	port := env("PORT", "8080")
	if (role == "all" || role == "worker") && os.Geteuid() != 0 {
		return fmt.Errorf("ATE_ROLE=%s runs the vendored gVisor worker, which needs root (cgroup delegation, network namespaces): run as sudo", role)
	}
	b, err := newStore(ctx)
	if err != nil {
		return err
	}
	capacity, _ := strconv.Atoi(env("ATE_CAPACITY", "4"))
	dir := env("ATE_DIR", "/var/lib/ate-ish")
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
