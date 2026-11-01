package sandbox

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// This file is the in-sandbox side of the rootless design: init (RunInit),
// the egress forwarder (RunForwarder), and the ingress dial-out forwarder
// (RunOutForwarder). All three run as the same ate-ish binary, copied
// into the bundle at /.ate/ate-ish (bundle.go's Build), dispatched by
// cmd/ate-ish/main.go's hidden-subcommand switch before any normal CLI
// parsing runs. They are direct ports of the rootless spike's own
// ateinit/forwarder/outforwarder (docs/ROOTLESS-SPIKE.md round 3,
// spike/rootless/r3 on branch rootless-spike), unchanged in behavior.

// Dispatch handles ate-ish's hidden in-sandbox subcommands (init, the
// egress forwarder, the ingress dial-out forwarder). handled is false
// when args names none of them, so the caller's normal CLI parsing runs
// instead.
func Dispatch(args []string) (handled bool, err error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case sandboxInitArg:
		return true, RunInit(args[1:])
	case forwarderArg:
		return true, RunForwarder(args[1:])
	case outforwarderArg:
		return true, RunOutForwarder(args[1:])
	default:
		return false, nil
	}
}

// RunInit is the sandbox's pid 1. It starts one egress forwarder per
// ATE_EGRESS entry ("listenIP:port:destSocketPath", space-separated),
// starts the ingress dial-out forwarder against ATE_INGRESS_TARGET (if
// set), then execs args as the actor's own command, forwarding
// SIGTERM/SIGINT and reaping. SIGUSR1/SIGUSR2 are relayed to the egress
// forwarders only (Namespace.Checkpoint sends USR1 before checkpointing,
// since gVisor refuses to save a sandbox holding a bound host socket;
// Namespace.Restore sends USR2 after restoring).
func RunInit(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: ate-ish %s <cmd> [args...]", sandboxInitArg)
	}
	self, err := os.Executable()
	if err != nil {
		self = "/.ate/ate-ish"
	}

	var egressFwds []*exec.Cmd
	for _, entry := range strings.Fields(os.Getenv("ATE_EGRESS")) {
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) != 3 {
			return fmt.Errorf("bad ATE_EGRESS entry %q, want ip:port:sockpath", entry)
		}
		ip, port, sock := parts[0], parts[1], parts[2]
		fwd := exec.Command(self, forwarderArg, "tcp", ip+":"+port, "unix", sock)
		fwd.Stdout, fwd.Stderr = os.Stdout, os.Stderr
		if err := fwd.Start(); err != nil {
			return fmt.Errorf("starting egress forwarder for %s: %w", entry, err)
		}
		egressFwds = append(egressFwds, fwd)
	}

	if target := os.Getenv("ATE_INGRESS_TARGET"); target != "" {
		ing := exec.Command(self, outforwarderArg, "/run/ate-ish/ingress.sock", "2", target)
		ing.Stdout, ing.Stderr = os.Stdout, os.Stderr
		if err := ing.Start(); err != nil {
			return fmt.Errorf("starting ingress forwarder: %w", err)
		}
	}

	actor := exec.Command(args[0], args[1:]...)
	actor.Stdin, actor.Stdout, actor.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := actor.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", args[0], err)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGCHLD, syscall.SIGUSR1, syscall.SIGUSR2)
	for s := range sigs {
		if s == syscall.SIGUSR1 || s == syscall.SIGUSR2 {
			for _, f := range egressFwds {
				f.Process.Signal(s)
			}
			continue
		}
		if s != syscall.SIGCHLD {
			actor.Process.Signal(s)
			continue
		}
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			if pid == actor.Process.Pid {
				os.Exit(ws.ExitStatus())
			}
		}
	}
	return nil
}

const (
	forwarderArg    = "__sandbox_forwarder__"
	outforwarderArg = "__sandbox_outforwarder__"
)

// RunForwarder copies raw bytes between a listener and a dialed address,
// never parsing them: ateinit runs one per ATE_EGRESS entry, each on its
// own loopback TCP address, forwarding to the host-side destination
// socket internal/egress listens on. SIGUSR1 closes the listener (gVisor
// cannot checkpoint a bound host socket) and SIGUSR2 reopens it; Init
// relays both from the node.
func RunForwarder(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("usage: ate-ish %s <listen-net> <listen-addr> <dial-net> <dial-addr>", forwarderArg)
	}
	lnet, laddr, dnet, daddr := args[0], args[1], args[2], args[3]
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGUSR1, syscall.SIGUSR2)
	for {
		if lnet == "unix" {
			os.Remove(laddr)
		}
		ln, err := net.Listen(lnet, laddr)
		if err != nil {
			return fmt.Errorf("listen %s:%s: %w", lnet, laddr, err)
		}
		log.Printf("forwarder: %s:%s -> %s:%s", lnet, laddr, dnet, daddr)
		go serveForward(ln, dnet, daddr)
		for s := <-sigs; s != syscall.SIGUSR1; s = <-sigs {
		}
		ln.Close()
		log.Printf("forwarder: %s:%s closed for checkpoint", lnet, laddr)
		for s := <-sigs; s != syscall.SIGUSR2; s = <-sigs {
		}
	}
}

func serveForward(ln net.Listener, dnet, daddr string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			d, err := net.Dial(dnet, daddr)
			if err != nil {
				log.Printf("forwarder: dial %s:%s: %v", dnet, daddr, err)
				return
			}
			defer d.Close()
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				io.Copy(d, c)
				if cw, ok := d.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
			}()
			io.Copy(c, d)
			wg.Wait()
		}()
	}
}

// RunOutForwarder is the ingress dial-out forwarder: it never listens, it
// only ever connects out to dialAddr (the node's own ingress.sock,
// bind-mounted at /run/ate-ish/ingress.sock) and relays that connection to
// tcpAddr (the actor's own server). Unlike a listening socket, an
// outbound connection does not block checkpoint, so it needs no signal
// handling at all (docs/ROOTLESS-SPIKE.md round 3, Q5).
func RunOutForwarder(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: ate-ish %s <dial-unix-addr> <pool-size> <dial-tcp-addr>", outforwarderArg)
	}
	dialAddr, tcpAddr := args[0], args[2]
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 1 {
		return fmt.Errorf("bad pool size %q", args[1])
	}
	for i := 0; i < n; i++ {
		go outforwarderWorker(dialAddr, tcpAddr)
	}
	select {}
}

func outforwarderWorker(dialAddr, tcpAddr string) {
	for {
		c, err := net.Dial("unix", dialAddr)
		if err != nil {
			log.Printf("outforwarder: dial %s: %v", dialAddr, err)
			time.Sleep(time.Second)
			continue
		}
		d, err := dialRetry(tcpAddr)
		if err != nil {
			log.Printf("outforwarder: dial %s: %v", tcpAddr, err)
			c.Close()
			continue
		}
		go func() {
			io.Copy(d, c)
			if cw, ok := d.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			}
		}()
		io.Copy(c, d)
		c.Close()
		d.Close()
	}
}

func dialRetry(addr string) (net.Conn, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil || time.Now().After(deadline) {
			return c, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
