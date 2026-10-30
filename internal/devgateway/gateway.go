// Package devgateway is ate-ish's own egress gateway: the remote endpoint an
// actor's EgressGateway.address names. atunnel (inside the vendored worker)
// opens an mTLS CONNECT tunnel to it for every actor TCP connection; this
// gateway answers the CONNECT and relays bytes to the real destination,
// unmodified.
//
// This is the stage-2 shape: a passthrough, so an actor's model call can
// actually reach the internet while egress_gateway is set. Credential
// injection and hostname policy enforcement (replacing a placeholder API
// key with the real one, refusing unlisted hosts) are stage 3's job, on
// this same listener.
package devgateway

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
)

// Gateway is a CONNECT-accepting TLS listener that tunnels to whatever host
// the CONNECT names.
type Gateway struct {
	ln   net.Listener
	Addr string
}

// Start listens on addr (empty host for a wildcard port) using cert for its
// serving identity. ClientAuth is requested but not verified: atunnel always
// presents the actor certificate devatelet minted, but stage 2 does not yet
// check it against policy.
func Start(addr string, cert tls.Certificate) (*Gateway, error) {
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequestClientCert,
		NextProtos:   []string{"http/1.1"},
	}
	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return nil, err
	}
	g := &Gateway{ln: ln, Addr: ln.Addr().String()}
	go g.acceptLoop()
	return g, nil
}

func (g *Gateway) Close() error { return g.ln.Close() }

func (g *Gateway) acceptLoop() {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		go g.handle(conn)
	}
}

func (g *Gateway) handle(conn net.Conn) {
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return
	}
	if err := tlsConn.Handshake(); err != nil {
		slog.Warn("devgateway: TLS handshake failed", "err", err)
		return
	}

	br := bufio.NewReader(tlsConn)
	req, err := http.ReadRequest(br)
	if err != nil {
		slog.Warn("devgateway: reading CONNECT request failed", "err", err)
		return
	}
	if req.Method != http.MethodConnect {
		fmt.Fprintf(tlsConn, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}

	var actorCert string
	if state := tlsConn.ConnectionState(); len(state.PeerCertificates) > 0 {
		actorCert = state.PeerCertificates[0].Subject.String()
	}
	slog.Info("devgateway: CONNECT", "destination", req.URL.Host, "actor_cert", actorCert)

	target, err := net.Dial("tcp", req.URL.Host)
	if err != nil {
		slog.Warn("devgateway: dialing destination failed", "destination", req.URL.Host, "err", err)
		fmt.Fprintf(tlsConn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer target.Close()
	fmt.Fprintf(tlsConn, "HTTP/1.1 200 Connection Established\r\n\r\n")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(target, br) }()
	go func() { defer wg.Done(); io.Copy(tlsConn, target) }()
	wg.Wait()
}
