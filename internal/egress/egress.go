// Package egress is ate-ish's egress gateway: one unix socket per
// (actor, hostname) destination an actor's policy allows, each bound
// inside the sandbox's bundle directory and dialed by the in-sandbox
// forwarder internal/sandbox starts (round 3's design,
// docs/ROOTLESS-SPIKE.md). There is no shared mTLS listener and no
// atunnel CONNECT tunnel: the destination is identified by which socket
// the forwarder connected to, not by an actor certificate, so a plain
// HTTP rule needs no SNI and no preamble either.
package egress

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/taeold/ate-ish/internal/records"
)

// Destination is one socket this gateway listens on, forwarding to
// host:port and applying rule's header replacement. Proto is "http" or
// "https"; https terminates the actor's own TLS (a MITM leaf keyed on
// host) before replaying the request, matching the rootless spike's own
// round-3 gateway.
type Destination struct {
	Actor          string
	Socket         string // unix socket path this gateway creates and listens on
	Proto          string // "http" or "https"
	Host           string
	Port           int
	ReplaceHeaders []records.ReplaceHeader
}

// Gateway mints a fresh MITM leaf per https destination's host and holds
// every Destination's listener. One Gateway per node, shared by every
// actor it hosts.
type Gateway struct {
	ca      *mitmCA
	secrets *secrets

	mu  sync.Mutex
	lns map[string]net.Listener // socket path -> listener
}

// New creates a Gateway. secretsClient resolves ate-secret://
// credentialUri values (Secret Manager or this process's own
// environment).
func New(secretsClient *http.Client) (*Gateway, error) {
	ca, err := newCA()
	if err != nil {
		return nil, fmt.Errorf("egress gateway MITM CA: %w", err)
	}
	return &Gateway{ca: ca, secrets: &secrets{client: secretsClient, cache: map[string]string{}}, lns: map[string]net.Listener{}}, nil
}

// CAPEM is the gateway's own MITM certificate, PEM-encoded: actors get it
// as their trust bundle for https rules.
func (g *Gateway) CAPEM() []byte { return g.ca.pem }

// Listen opens d.Socket and starts forwarding from it. Call once per
// destination when an actor boots (Namespace.Build's caller computes the
// destination list from the actor's egress policy).
func (g *Gateway) Listen(d Destination) error {
	if d.Proto != "http" && d.Proto != "https" {
		return fmt.Errorf("egress destination %s: bad proto %q, want http or https", d.Socket, d.Proto)
	}
	os.Remove(d.Socket)
	ln, err := net.Listen("unix", d.Socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", d.Socket, err)
	}
	g.mu.Lock()
	g.lns[d.Socket] = ln
	g.mu.Unlock()
	slog.Info("egress: listening", "actor", d.Actor, "socket", d.Socket, "proto", d.Proto, "host", d.Host, "port", d.Port)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if d.Proto == "https" {
				go g.handleHTTPS(conn, d)
			} else {
				go g.handleHTTP(conn, d)
			}
		}
	}()
	return nil
}

// Close closes every socket this destination's actor opened and removes
// the socket files (the bundle directory removal under it, elsewhere,
// would do the same, but an explicit Close lets a suspend free the port
// before the bundle itself is torn down).
func (g *Gateway) Close(sockets ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range sockets {
		if ln, ok := g.lns[s]; ok {
			ln.Close()
			delete(g.lns, s)
		}
		os.Remove(s)
	}
}

func (g *Gateway) handleHTTPS(conn net.Conn, d Destination) {
	defer conn.Close()
	var name string
	mitm := tls.Server(conn, &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name = hello.ServerName
			if name != d.Host {
				return nil, fmt.Errorf("sni %q does not match this destination's host %q", name, d.Host)
			}
			return g.ca.leaf(name)
		},
	})
	defer mitm.Close()
	if err := mitm.Handshake(); err != nil {
		slog.Warn("egress: tls handshake", "actor", d.Actor, "sni", name, "err", err)
		return
	}
	addr := net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
	upstream := &http.Transport{
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			raw, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			tc := tls.Client(raw, &tls.Config{ServerName: d.Host})
			if err := tc.Handshake(); err != nil {
				raw.Close()
				return nil, err
			}
			return tc, nil
		},
	}
	g.serveLoop(mitm, d, addr, "https", upstream.RoundTrip)
}

func (g *Gateway) handleHTTP(conn net.Conn, d Destination) {
	defer conn.Close()
	addr := net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, err := (&net.Dialer{Timeout: 10 * time.Second}).Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		if err := req.Write(raw); err != nil {
			raw.Close()
			return nil, err
		}
		resp, err := http.ReadResponse(bufio.NewReader(raw), req)
		if err != nil {
			raw.Close()
			return nil, err
		}
		resp.Body = closeBoth{resp.Body, raw}
		return resp, nil
	})
	g.serveLoop(conn, d, addr, "http", rt.RoundTrip)
}

type closeBoth struct {
	io.ReadCloser
	extra net.Conn
}

func (c closeBoth) Close() error { c.extra.Close(); return c.ReadCloser.Close() }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (g *Gateway) serveLoop(rw io.ReadWriter, d Destination, addr, scheme string, roundTrip func(*http.Request) (*http.Response, error)) {
	in := bufio.NewReader(rw)
	for {
		req, err := http.ReadRequest(in)
		if err != nil {
			return
		}
		req.URL.Scheme, req.URL.Host, req.RequestURI = scheme, addr, ""
		for _, h := range d.ReplaceHeaders {
			// Replace, never add: a request that did not carry the header
			// reaches the upstream exactly as sent, so a wrong assumption
			// about what the actor sends cannot silently paper over a
			// missing credential.
			if req.Header.Get(h.Header) == "" {
				continue
			}
			secret, err := g.secrets.get(context.Background(), h.CredentialURI)
			if err != nil {
				slog.Error("egress secret", "actor", d.Actor, "uri", h.CredentialURI, "err", err)
				return
			}
			req.Header.Set(h.Header, h.Prefix+secret)
			slog.Info("egress", "actor", d.Actor, "method", req.Method, "path", req.URL.Path, "replaced", h.Header)
		}
		resp, err := roundTrip(req)
		if err != nil {
			slog.Error("egress", "actor", d.Actor, "host", d.Host, "err", err)
			return
		}
		err = resp.Write(rw)
		resp.Body.Close()
		if err != nil || resp.Close {
			return
		}
	}
}

// mitmCA mints a fresh leaf per hostname on demand, signed by a
// self-signed authority this process generates once.
type mitmCA struct {
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	pem    []byte
	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

func newCA() (*mitmCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ate-ish-egress-mitm"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	return &mitmCA{cert: cert, key: key, leaves: map[string]*tls.Certificate{},
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

func (ca *mitmCA) leaf(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c, ok := ca.leaves[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ca.leaves[host] = c
	return c, nil
}

// secrets reads credentialUri values. The native Secret Manager form is
// ate-secret://secretmanager.googleapis.com/<project>/<secret>.
// ate-secret://env/<VAR> reads this process's own environment instead, no
// GCP project needed.
type secrets struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]string
}

func (s *secrets) get(ctx context.Context, uri string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.cache[uri]; ok {
		return v, nil
	}
	if v, ok := strings.CutPrefix(uri, "ate-secret://env/"); ok {
		val, ok := os.LookupEnv(v)
		if !ok {
			return "", fmt.Errorf("unknown credentialUri %q: environment variable %s is not set", uri, v)
		}
		s.cache[uri] = val
		return val, nil
	}
	name, ok := strings.CutPrefix(uri, "ate-secret://secretmanager.googleapis.com/")
	project, secret, _ := strings.Cut(name, "/")
	if !ok || project == "" || secret == "" {
		return "", fmt.Errorf("unknown credentialUri %q", uri)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://secretmanager.googleapis.com/v1/projects/"+project+"/secrets/"+secret+"/versions/latest:access", nil)
	resp, err := s.client.Do(req)
	if err != nil {
		slog.Error("could not resolve a Secret Manager credentialUri", "credentialUri", uri, "err", err)
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secret manager %s: %s", name, resp.Status)
	}
	var body struct{ Payload struct{ Data string } }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	v, err := base64.StdEncoding.DecodeString(body.Payload.Data)
	if err != nil {
		return "", err
	}
	s.cache[uri] = strings.TrimSpace(string(v))
	return s.cache[uri], nil
}
