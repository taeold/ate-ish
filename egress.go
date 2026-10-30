package main

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
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/taeold/ate-ish/internal/devatelet"
)

// egressCA is the gateway's MITM authority: it signs a fresh leaf for
// whatever host an actor's CONNECT names, on demand, so the gateway can
// terminate the actor's own TLS and inspect its HTTP requests. Actors get
// its certificate as their trust bundle (worker.go writes it into the
// systemInfo volume's trust-bundle file).
type egressCA struct {
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	pem    []byte
	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

func newEgressCA() (*egressCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "egress-mitm.ate.dev"},
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
	return &egressCA{cert: cert, key: key, leaves: map[string]*tls.Certificate{},
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

func (ca *egressCA) leaf(host string) (*tls.Certificate, error) {
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

// egressGateway is the EgressGateway address every actor's RunWorkload
// request names: one shared listener (not one per actor, as v1 had — v1
// had no transparent capture, so each actor's own HTTPS_PROXY pointed at
// its own port; atunnel's redirect makes that unnecessary, and the actor is
// now identified by its client certificate instead). atunnel opens an mTLS
// CONNECT tunnel here per actor TCP connection; this verifies that
// certificate against the node CA, maps it to an atespace/name, reads that
// actor's current egress policy from the bucket, and only then MITMs the
// actor's own TLS to apply header replacement — v1's egress.go, with the
// per-actor listener/port replaced by per-connection certificate identity.
type egressGateway struct {
	ln      net.Listener
	Addr    string
	ca      *egressCA
	bucket  *bucket
	secrets *secrets

	// resolved caches every IP ever seen for an allowed hostname, keyed by
	// hostname. A rotating-DNS host (ai-gateway.vercel.sh answers from a
	// large, varying pool) means atunnel's CONNECT — which only carries the
	// IP the actor's own, separate, resolution landed on — and a fresh
	// lookup made here can disagree on any single request; the set only
	// grows, never evicts, so coverage improves across a run instead of
	// repeating the same race every time.
	resolveMu sync.Mutex
	resolved  map[string]map[string]bool // hostname -> set of IP strings
}

// startEgressGateway listens on addr with serverCert as its own TLS
// identity (verified by the worker's --atunnel-egress-trust-bundle) and
// nodeCAPEM as the pool that verifies the actor client certificates
// atunnel presents (the same CA devatelet mints them from).
func startEgressGateway(addr string, serverCert tls.Certificate, nodeCAPEM []byte, b *bucket) (*egressGateway, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(nodeCAPEM) {
		return nil, fmt.Errorf("egress gateway: node CA trust bundle has no certificates")
	}
	ca, err := newEgressCA()
	if err != nil {
		return nil, fmt.Errorf("egress gateway MITM CA: %w", err)
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		NextProtos:   []string{"http/1.1"},
	}
	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return nil, err
	}
	g := &egressGateway{
		ln: ln, Addr: ln.Addr().String(), ca: ca, bucket: b,
		secrets:  &secrets{client: b.client, cache: map[string]string{}},
		resolved: map[string]map[string]bool{},
	}
	go g.acceptLoop()
	return g, nil
}

func (g *egressGateway) Close() error { return g.ln.Close() }

func (g *egressGateway) acceptLoop() {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		go g.handle(conn)
	}
}

func (g *egressGateway) handle(conn net.Conn) {
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return
	}
	if err := tlsConn.Handshake(); err != nil {
		slog.Warn("egress: atunnel TLS handshake failed", "err", err)
		return
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		slog.Warn("egress: no actor certificate presented")
		return
	}
	atespace, name, ok := devatelet.ActorIdentity(state.PeerCertificates[0])
	if !ok {
		slog.Warn("egress: actor certificate carries no identity", "subject", state.PeerCertificates[0].Subject)
		return
	}
	actor := atespace + "/" + name

	br := bufio.NewReader(tlsConn)
	req, err := http.ReadRequest(br)
	if err != nil {
		slog.Warn("egress: reading atunnel's CONNECT failed", "actor", actor, "err", err)
		return
	}
	if req.Method != http.MethodConnect {
		fmt.Fprint(tlsConn, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}
	// atunnel's CONNECT names the transparently-redirected TCP connection's
	// original destination (SO_ORIGINAL_DST), which is an IP — the DNS
	// query that resolved the actual hostname already happened and ended,
	// so there is no hostname on this connection to read. The allowlist is
	// written in hostnames (so a policy need not track an API's rotating
	// IPs by hand), so matching it means resolving each allowed hostname
	// ourselves and checking the destination IP against that set. The
	// hostname of the match, not the destination, is what the actor's own
	// TLS expects to see in the MITM leaf's subject (its ClientHello SNI
	// names the hostname it resolved, never the IP), so it is kept from
	// here on, not the raw destination.
	dest := req.URL.Hostname()

	var rec Actor
	_, err = g.bucket.get(context.Background(), actorKey(atespace, name), &rec)
	if err != nil {
		slog.Error("egress: reading actor's record", "actor", actor, "err", err)
		fmt.Fprint(tlsConn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	host, headers, allowed := g.resolvePolicyHost(context.Background(), rec.EgressPolicy, dest)
	slog.Info("egress", "actor", actor, "destination", dest, "host", host, "allowed", allowed)
	if !allowed {
		fmt.Fprint(tlsConn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"+
			"ate-ish egress: "+dest+" is not in the egress policy of "+actor+"\r\n")
		return
	}
	fmt.Fprint(tlsConn, "HTTP/1.1 200 Connection Established\r\n\r\n")

	mitm := tls.Server(&bufferedConn{Conn: tlsConn, r: br}, &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return g.ca.leaf(host) },
	})
	defer mitm.Close()
	in := bufio.NewReader(mitm)
	for {
		req, err := http.ReadRequest(in)
		if err != nil {
			return
		}
		// The secret goes to the CONNECT host only, so the Host header must name it too.
		if got := (&url.URL{Host: req.Host}).Hostname(); got != host {
			slog.Warn("egress", "actor", actor, "host", host, "hostHeader", req.Host, "allowed", false)
			fmt.Fprint(mitm, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		req.URL.Scheme, req.URL.Host, req.RequestURI = "https", host, ""
		for _, h := range headers {
			// Replace, never add: a request that did not carry the header
			// (the gate's own probe for this) reaches the upstream exactly
			// as sent, so a wrong assumption about what the actor sends
			// cannot silently paper over a missing credential.
			if req.Header.Get(h.Header) == "" {
				continue
			}
			secret, err := g.secrets.get(context.Background(), h.CredentialURI)
			if err != nil {
				slog.Error("egress secret", "actor", actor, "uri", h.CredentialURI, "err", err)
				return
			}
			req.Header.Set(h.Header, h.Prefix+secret)
		}
		resp, err := upstream.RoundTrip(req)
		if err != nil {
			slog.Error("egress", "actor", actor, "host", host, "err", err)
			return
		}
		err = resp.Write(mitm)
		resp.Body.Close()
		if err != nil || resp.Close {
			return
		}
	}
}

// bufferedConn lets tls.Server read through a bufio.Reader that already
// consumed some of conn's bytes (the CONNECT line and headers), instead of
// losing whatever the Reader buffered.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// upstream speaks HTTP/1.1 only, so its responses go back to the actor's
// HTTP/1.1 connection as they came.
var upstream = &http.Transport{TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}

// resolvePolicyHost finds which, if any, of policy's allowed hostnames
// resolves to dest (an IP, per atunnel's CONNECT — see its call site) and
// returns that hostname and the headers its rule replaces. It checks the
// accumulated resolved cache first, then falls back to a fresh lookup
// (merged into the cache either way) for the case dest is an IP this
// process has not seen from that hostname yet.
func (g *egressGateway) resolvePolicyHost(ctx context.Context, policy *EgressPolicy, dest string) (string, []ReplaceHeader, bool) {
	if policy == nil {
		return "", nil, false
	}
	destIP := net.ParseIP(dest)
	for _, rule := range policy.Rules {
		for _, hostname := range rule.HTTPS.Hostnames {
			if destIP == nil {
				if hostname == dest {
					return hostname, rule.HTTPS.Effects.ReplaceHeaders, true
				}
				continue
			}
			if g.resolvedHas(hostname, dest) {
				return hostname, rule.HTTPS.Effects.ReplaceHeaders, true
			}
			// A rotating-DNS host can answer a different subset of its
			// pool on each query, so one lookup missing dest does not mean
			// dest is not a real answer for hostname; a couple of retries
			// cheaply raises the odds of this lookup's subset overlapping
			// with whatever subset the actor's own, earlier, resolution
			// got.
			for attempt := 0; attempt < 3; attempt++ {
				addrs, err := net.DefaultResolver.LookupIPAddr(ctx, hostname)
				if err != nil {
					slog.Warn("egress: resolving an allowed hostname", "hostname", hostname, "err", err)
					break
				}
				ips := make([]string, len(addrs))
				for i, addr := range addrs {
					ips[i] = addr.IP.String()
				}
				g.resolvedAdd(hostname, ips)
				if g.resolvedHas(hostname, dest) {
					return hostname, rule.HTTPS.Effects.ReplaceHeaders, true
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
	return "", nil, false
}

func (g *egressGateway) resolvedHas(hostname, ip string) bool {
	g.resolveMu.Lock()
	defer g.resolveMu.Unlock()
	return g.resolved[hostname][ip]
}

func (g *egressGateway) resolvedAdd(hostname string, ips []string) {
	g.resolveMu.Lock()
	defer g.resolveMu.Unlock()
	set := g.resolved[hostname]
	if set == nil {
		set = map[string]bool{}
		g.resolved[hostname] = set
	}
	for _, ip := range ips {
		set[ip] = true
	}
}

// secrets reads credentialUri values from Secret Manager. The native form is
// ate-secret://secretmanager.googleapis.com/<project>/<secret>; other URIs,
// like the demo's k8s.io one, map to that form through ATE_SECRET_MAP
// ("<uri>=<project>/<secret>,...").
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
	name, ok := strings.CutPrefix(uri, "ate-secret://secretmanager.googleapis.com/")
	for _, m := range strings.Split(os.Getenv("ATE_SECRET_MAP"), ",") {
		if from, to, _ := strings.Cut(m, "="); from == uri {
			name, ok = to, true
		}
	}
	project, secret, _ := strings.Cut(name, "/")
	if !ok || project == "" || secret == "" {
		return "", fmt.Errorf("unknown credentialUri %q", uri)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://secretmanager.googleapis.com/v1/projects/"+project+"/secrets/"+secret+"/versions/latest:access", nil)
	resp, err := s.client.Do(req)
	if err != nil {
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
