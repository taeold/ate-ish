package sandbox

import (
	"bufio"
	"context"
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

	"golang.org/x/net/dns/dnsmessage"
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
// SIGTERM/SIGINT and reaping. SIGUSR1/SIGUSR2 are relayed to every
// egress forwarder and the ingress outforwarder alike (Namespace.
// Checkpoint sends USR1 before checkpointing, since gVisor refuses to
// save a sandbox holding a bound host socket, or (outforwarder's own
// doc comment) one of its own dial-out connections sitting idle in the
// node's pool; Namespace.Restore sends USR2 after restoring, and
// nothing redials or re-listens before that).
func RunInit(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: ate-ish %s <cmd> [args...]", sandboxInitArg)
	}
	self, err := os.Executable()
	if err != nil {
		self = "/.ate/ate-ish"
	}

	fwds := &fwdSet{self: self}
	for _, entry := range strings.Fields(os.Getenv("ATE_EGRESS")) {
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) != 3 {
			return fmt.Errorf("bad ATE_EGRESS entry %q, want ip:port:sockpath", entry)
		}
		ip, port, sock := parts[0], parts[1], parts[2]
		if err := fwds.start(ip, port, sock); err != nil {
			return fmt.Errorf("starting egress forwarder for %s: %w", entry, err)
		}
	}

	// A wildcard-hostname rule's control socket (egressControlPath,
	// bundle.go's Build) is present only when the actor's egress policy
	// named one; its DNS responder resolves names on demand and starts
	// one more forwarder (via fwds, so its listener still closes for
	// checkpoint and reopens after restore like every other one) the
	// first time a name actually matches.
	if _, err := os.Stat(egressControlPath); err == nil {
		go runDNSResponder(fwds)
	}

	var ing *outforwarder
	if target := os.Getenv("ATE_INGRESS_TARGET"); target != "" {
		ing = &outforwarder{self: self, target: target}
		if err := ing.start(); err != nil {
			return fmt.Errorf("starting ingress forwarder: %w", err)
		}
	}

	actor := exec.Command(args[0], args[1:]...)
	actor.Stdin, actor.Stdout, actor.Stderr = os.Stdin, os.Stdout, os.Stderr
	// init itself (this process, pid 1) stays root throughout: it is
	// the one thing in the sandbox that needs real access to the
	// host-backed ingress/egress sockets bind-mounted under
	// /run/ate-ish, for as long as the sandbox runs. The actor's own
	// process is what an image's USER directive names, dropped here
	// (bundle.go's buildConfig sets these only when the resolved uid/gid
	// is non-root), the same gosu or tini pattern a real root-starting
	// entrypoint uses, via Go's own Credential rather than a second
	// binary: CAP_SETUID/CAP_SETGID are already in this sandbox's
	// bounding set (bundle.go's bundleCaps).
	if uid, gid, ok := actorCredential(); ok {
		actor.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	}
	if err := actor.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", args[0], err)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGCHLD, syscall.SIGUSR1, syscall.SIGUSR2)
	for s := range sigs {
		if s == syscall.SIGUSR1 || s == syscall.SIGUSR2 {
			fwds.signal(s)
			if ing != nil {
				ing.signal(s)
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

// actorCredential reads ATE_ACTOR_UID/ATE_ACTOR_GID (bundle.go's
// buildConfig, set only for a non-root image), ok false when neither is
// set: the common case, every actor before this build's G5.
func actorCredential() (uid, gid uint32, ok bool) {
	u, uok := os.LookupEnv("ATE_ACTOR_UID")
	g, gok := os.LookupEnv("ATE_ACTOR_GID")
	if !uok && !gok {
		return 0, 0, false
	}
	uid64, _ := strconv.ParseUint(u, 10, 32)
	gid64, _ := strconv.ParseUint(g, 10, 32)
	return uint32(uid64), uint32(gid64), true
}

// fwdSet is every egress forwarder RunInit has started, static or
// started later by the DNS responder: SIGUSR1/SIGUSR2 must reach all
// of them, including ones that did not exist yet when Namespace.Build
// first ran, or a wildcard destination's listener would still be
// bound when gVisor tries to checkpoint.
type fwdSet struct {
	self string
	mu   sync.Mutex
	fwds []*exec.Cmd
}

func (s *fwdSet) start(ip, port, sock string) error {
	fwd := exec.Command(s.self, forwarderArg, "tcp", ip+":"+port, "unix", sock)
	fwd.Stdout, fwd.Stderr = os.Stdout, os.Stderr
	if err := fwd.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.fwds = append(s.fwds, fwd)
	s.mu.Unlock()
	return nil
}

func (s *fwdSet) signal(sig os.Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.fwds {
		f.Process.Signal(sig)
	}
}

// runDNSResponder answers every query on dnsListenIP:53 by asking the
// host's control socket (egress.Gateway.ListenControl) whether the
// queried name matches a wildcard rule. A match gets an A record for
// the loopback address the host allocated, and (the first time that
// exact name is seen) one more forwarder from that address to the
// socket the host is now listening on; anything else gets NXDOMAIN.
// Never returns; RunInit starts it as its own goroutine.
func runDNSResponder(fwds *fwdSet) {
	conn, err := net.ListenPacket("udp", dnsListenIP+":53")
	if err != nil {
		log.Printf("dns responder: listen %s:53: %v", dnsListenIP, err)
		return
	}
	defer conn.Close()
	started := map[string]bool{} // "ip:port" already forwarding
	var mu sync.Mutex
	buf := make([]byte, 512)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		var req dnsmessage.Message
		if err := req.Unpack(buf[:n]); err != nil || len(req.Questions) == 0 {
			continue
		}
		go func(req dnsmessage.Message, addr net.Addr) {
			resp := resolveQuestion(req, fwds, started, &mu)
			if out, err := resp.Pack(); err == nil {
				conn.WriteTo(out, addr)
			}
		}(req, addr)
	}
}

func resolveQuestion(req dnsmessage.Message, fwds *fwdSet, started map[string]bool, mu *sync.Mutex) dnsmessage.Message {
	resp := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: req.Header.ID, Response: true, RecursionAvailable: true},
		Questions: req.Questions,
	}
	q := req.Questions[0]
	name := strings.TrimSuffix(q.Name.String(), ".")
	line, err := dialControl(name)
	if err != nil || !strings.HasPrefix(line, "OK ") {
		resp.Header.RCode = dnsmessage.RCodeNameError
		return resp
	}
	fields := strings.Fields(strings.TrimPrefix(line, "OK "))
	if len(fields) != 3 {
		resp.Header.RCode = dnsmessage.RCodeServerFailure
		return resp
	}
	ip, port, sock := fields[0], fields[1], fields[2]
	if q.Type != dnsmessage.TypeA {
		// No AAAA (or anything else) support: an empty NOERROR answer
		// tells the resolver to fall back to A rather than treat this
		// as a hard failure, matching a resolver with IPv6 off.
		return resp
	}
	key := ip + ":" + port
	mu.Lock()
	needStart := !started[key]
	started[key] = true
	mu.Unlock()
	if needStart {
		if err := fwds.start(ip, port, "/run/ate-ish/egress/"+sock); err != nil {
			log.Printf("dns responder: starting forwarder for %s: %v", name, err)
			resp.Header.RCode = dnsmessage.RCodeServerFailure
			return resp
		}
		// The DNS answer races the actor's own connect against this
		// forwarder's listen: a fresh exec.Cmd is asynchronous, so wait
		// for the socket to actually accept before replying, instead of
		// handing back an address nothing is listening on yet.
		waitDial(ip + ":" + port)
	}
	var a [4]byte
	for i, part := range strings.SplitN(ip, ".", 4) {
		v, _ := strconv.Atoi(part)
		a[i] = byte(v)
	}
	resp.Answers = []dnsmessage.Resource{{
		Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 1},
		Body:   &dnsmessage.AResource{A: a},
	}}
	return resp
}

// waitDial blocks until addr accepts a TCP connection or 2 seconds
// pass, whichever comes first.
func waitDial(addr string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dialControl(name string) (string, error) {
	conn, err := net.Dial("unix", egressControlPath)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "RESOLVE %s\n", name); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line), err
}

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

// outforwarder is init's own handle on RunOutForwarder (the
// ATE_INGRESS_TARGET dial-out pool, started once per boot): signal
// relays SIGUSR1/SIGUSR2 to it, exactly as fwds.signal does for every
// egress forwarder. RunOutForwarder's own signal loop is what used to
// live here as restart (kill the old process, start a new one): a
// SIGKILLed pool still left its idle connections to be resumed, as
// part of the sandbox's own process tree, by gVisor's restore, and
// killing the pool from outside could lose the race against that
// resume (round D's own review: a resumed, about-to-be-killed worker
// could still dial into the node's new-generation ingressBridge and
// get pooled a moment before dying, leaving it stuck with a connection
// that looks fine but is already dead). Quiescing the pool itself on
// SIGUSR1 — closing every connection and dialing nothing more, instead
// of waiting to be killed — means there is nothing left running to
// resume by the time checkpoint actually saves state, so restore has
// nothing stale to inherit in the first place; RunOutForwarder only
// dials again once SIGUSR2 says restore is done. ingressBridge's own
// generation drop (worker.go) stays as a second line of defense.
type outforwarder struct {
	self, target string
	cmd          *exec.Cmd
}

func (o *outforwarder) start() error {
	cmd := exec.Command(o.self, outforwarderArg, "/run/ate-ish/ingress.sock", "2", o.target)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	o.cmd = cmd
	return nil
}

// signal relays sig to the running pool, same as fwdSet.signal does
// per forwarder.
func (o *outforwarder) signal(sig os.Signal) {
	if o.cmd != nil && o.cmd.Process != nil {
		o.cmd.Process.Signal(sig)
	}
}

// RunOutForwarder is the ingress dial-out forwarder: it never listens, it
// only ever connects out to dialAddr (the node's own ingress.sock,
// bind-mounted at /run/ate-ish/ingress.sock) and relays that connection to
// tcpAddr (the actor's own server). Unlike a listening socket, an
// outbound connection does not block checkpoint on its own
// (docs/ROOTLESS-SPIKE.md round 3, Q5), but one sitting idle, dialed
// and pooled on the node side yet never used, is exactly the stale
// connection outforwarder's own doc comment describes if it survives
// into a restored generation. SIGUSR1 here, same as every egress
// forwarder's own listener close, quiesces this pool instead of
// leaving that question to restart: every active connection is closed
// and no worker dials again until SIGUSR2.
func RunOutForwarder(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: ate-ish %s <dial-unix-addr> <pool-size> <dial-tcp-addr>", outforwarderArg)
	}
	dialAddr, tcpAddr := args[0], args[2]
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 1 {
		return fmt.Errorf("bad pool size %q", args[1])
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGUSR1, syscall.SIGUSR2)
	for {
		cs := newConnSet()
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				outforwarderWorker(ctx, cs, dialAddr, tcpAddr)
			}()
		}
		for s := <-sigs; s != syscall.SIGUSR1; s = <-sigs {
		}
		cancel()
		cs.closeAll()
		wg.Wait()
		log.Printf("outforwarder: quiesced for checkpoint, every connection closed")
		for s := <-sigs; s != syscall.SIGUSR2; s = <-sigs {
		}
	}
}

// connSet is every connection an outforwarderWorker currently has
// dialed to the node, whether idle in the node's own pool or actively
// relaying: closeAll is what quiesce uses to end all of them at once.
type connSet struct {
	mu sync.Mutex
	m  map[net.Conn]struct{}
}

func newConnSet() *connSet { return &connSet{m: map[net.Conn]struct{}{}} }

func (cs *connSet) add(c net.Conn) {
	cs.mu.Lock()
	cs.m[c] = struct{}{}
	cs.mu.Unlock()
}

func (cs *connSet) remove(c net.Conn) {
	cs.mu.Lock()
	delete(cs.m, c)
	cs.mu.Unlock()
}

func (cs *connSet) closeAll() {
	cs.mu.Lock()
	for c := range cs.m {
		c.Close()
	}
	cs.m = map[net.Conn]struct{}{}
	cs.mu.Unlock()
}

// outforwarderWorker dials, relays, and redials in a loop until ctx is
// done. It tracks its own connection to the node (dialAddr) in cs for
// the whole time it might sit idle, dialed but not yet picked up by
// the node's own pool — exactly the one closeAll needs to be able to
// end on quiesce — and checks ctx right after adding it, in case
// quiesce landed in the gap between the dial and the add: ctx is
// always cancelled before closeAll runs, so this is never racy, only
// possibly one extra dial per worker that gets closed immediately.
func outforwarderWorker(ctx context.Context, cs *connSet, dialAddr, tcpAddr string) {
	for {
		if ctx.Err() != nil {
			return
		}
		c, err := net.Dial("unix", dialAddr)
		if err != nil {
			log.Printf("outforwarder: dial %s: %v", dialAddr, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		cs.add(c)
		if ctx.Err() != nil {
			cs.remove(c)
			c.Close()
			return
		}
		d, err := dialRetry(tcpAddr)
		if err != nil {
			log.Printf("outforwarder: dial %s: %v", tcpAddr, err)
			cs.remove(c)
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
		cs.remove(c)
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
