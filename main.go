// ate-ish is a small Agent Substrate for Cloud Run: one program, one bucket.
//
//	ate serve   the server; ATE_ROLE=all|ingress|worker
//	ate init    pid 1 inside an actor's sandbox
//	ate stop    sends SIGTERM to pid 1 inside the sandbox
//	ate ...     the CLI (create, get, suspend, delete)
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
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	var err error
	switch cmd := append(os.Args[1:], "")[0]; cmd {
	case "serve":
		err = serve()
	case "init":
		err = runInit()
	case "stop":
		err = syscall.Kill(1, syscall.SIGTERM)
	default:
		err = runCLI(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ate:", err)
		os.Exit(1)
	}
}

func serve() error {
	ctx := context.Background()
	role := env("ATE_ROLE", "all")
	port := env("PORT", "8080")
	b, err := newBucket(ctx, os.Getenv("ATE_BUCKET"))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	var in *ingress
	var w *worker
	if role == "all" || role == "ingress" {
		in = &ingress{bucket: b, placing: map[string]*placement{}}
		in.routes(mux)
	}
	if role == "all" || role == "worker" {
		addr := "127.0.0.1:" + port
		if role == "worker" {
			ip, err := metadata("instance/network-interfaces/0/ip")
			if err != nil {
				return err
			}
			addr = ip + ":" + port
		}
		capacity, _ := strconv.Atoi(env("ATE_CAPACITY", "4"))
		if w, err = newWorker(ctx, b, addr, capacity); err != nil {
			return err
		}
		w.routes(mux)
	}
	if role == "all" {
		idle := env("ATE_IDLE_SECONDS", "600")
		go func() {
			for range time.Tick(time.Minute) {
				if reaped, err := in.reap(ctx, idle); err != nil || len(reaped) > 0 {
					slog.Info("reaped", "actors", reaped, "err", err)
				}
			}
		}()
	}
	srv := &http.Server{Addr: ":" + port, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// Actors run with egress allowed, so they can reach this port too.
		if ip, _, _ := net.SplitHostPort(r.RemoteAddr); actorNet.Contains(net.ParseIP(ip)) {
			slog.Warn("refused a request from an actor", "remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(rw, "actors cannot call ate-ish", http.StatusForbidden)
			return
		}
		if in != nil && r.Header.Get("ate-target-actor") != "" {
			in.route(rw, r)
			return
		}
		mux.ServeHTTP(rw, r)
	})}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
		<-stop
		if w != nil {
			w.suspendAll(ctx)
		}
		srv.Close()
	}()
	slog.Info("serving", "role", role, "port", port)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// actorNet holds the addresses the sandbox launcher gives actors (measured: 172.20.0.N).
var actorNet = &net.IPNet{IP: net.IPv4(172, 20, 0, 0), Mask: net.CIDRMask(16, 32)}

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
