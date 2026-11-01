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
	"net"
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
	"github.com/taeold/ate-ish/internal/sandbox"
	"github.com/taeold/ate-ish/internal/store"

	// Root certificates for images without them, such as node:24-slim.
	_ "golang.org/x/crypto/x509roots/fallback"
)

func main() {
	// Hidden: the in-sandbox programs (init, the egress forwarder, the
	// ingress dial-out forwarder) are this same binary, copied into each
	// actor's bundle at /.ate/ate-ish and invoked with one of these
	// args. Not part of the kubectl-ate CLI surface, checked before any
	// of it runs.
	if handled, err := sandbox.Dispatch(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "ate-ish:", err)
			os.Exit(1)
		}
		return
	}
	var err error
	switch cmd := append(os.Args[1:], "")[0]; cmd {
	case "":
		err = serve()
	case "fetch-runsc":
		// Hidden: stages the pinned gVisor release at ATE_RUNSC_DIR (or
		// its default), the same code serve() calls on worker startup.
		// Not part of the kubectl-ate CLI surface; exists so the
		// Dockerfile build and anyone pre-staging runsc share this one
		// implementation instead of a second, shell-script copy.
		err = atelet.EnsureRunsc()
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
// listen is a host:port, as ATE_LISTEN sets it (default 127.0.0.1:8080).
func startNode(ctx context.Context, role, listen string, b store.Store, dir string, capacity int) (*node, error) {
	mux := http.NewServeMux()
	n := &node{}
	if role == "all" || role == "ingress" {
		n.in = ingress.New(b)
		n.in.Routes(mux)
	}
	if role == "all" || role == "worker" {
		_, port, err := net.SplitHostPort(listen)
		if err != nil {
			return nil, fmt.Errorf("parsing ATE_LISTEN %q: %w", listen, err)
		}
		addr := listen
		if role == "worker" {
			ip, err := metadata("instance/network-interfaces/0/ip")
			if err != nil {
				return nil, err
			}
			addr = ip + ":" + port
		} else {
			if addr, err = advertiseAddr(listen, port); err != nil {
				return nil, err
			}
		}
		if n.w, err = atelet.New(ctx, b, addr, capacity, dir); err != nil {
			return nil, err
		}
		n.w.Routes(mux)
	}
	if role == "all" {
		idle := envutil.Get("ATE_IDLE_SECONDS", "600")
		go reapLoop(ctx, n.in, idle)
	}
	n.srv = &http.Server{Addr: listen, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if n.in != nil && r.Header.Get("ate-target-actor") != "" {
			n.in.Route(rw, r)
			return
		}
		mux.ServeHTTP(rw, r)
	})}
	return n, nil
}

// advertiseAddr is the address this node puts in its lease, for another
// node's ingress to forward to: ATE_ADVERTISE if set, else listen itself
// when its host is not a wildcard, else the machine's first non-loopback
// IPv4 address with the listen port.
func advertiseAddr(listen, port string) (string, error) {
	if v := envutil.Get("ATE_ADVERTISE", ""); v != "" {
		return v, nil
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return listen, nil
	}
	ip, err := firstNonLoopbackIPv4()
	if err != nil {
		return "", fmt.Errorf("ATE_LISTEN %q listens on every interface and ATE_ADVERTISE is not set: %w", listen, err)
	}
	return ip + ":" + port, nil
}

// firstNonLoopbackIPv4 is the fallback advertise host when ATE_LISTEN is a
// wildcard address and ATE_ADVERTISE is not set.
func firstNonLoopbackIPv4() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String(), nil
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 address found")
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
	listen := envutil.Get("ATE_LISTEN", "127.0.0.1:8080")
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
	n, err := startNode(ctx, role, listen, b, dir, capacity)
	if err != nil {
		return err
	}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
		<-stop
		n.shutdown(ctx)
	}()
	slog.Info("serving", "role", role, "listen", listen)
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
