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
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// egressCA is the worker's MITM authority. Actors get its certificate as
// their trust bundle.
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

// egressProxy is one actor's HTTPS proxy. The listener port identifies the
// actor. It allows the policy's hostnames, replaces their headers with
// secrets, and refuses everything else.
type egressProxy struct {
	actor   string
	policy  *EgressPolicy
	ca      *egressCA
	secrets *secrets
}

func (p *egressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()
	headers, ok := p.allowed(host)
	slog.Info("egress", "actor", p.actor, "remote", r.RemoteAddr, "method", r.Method, "host", host, "allowed", ok)
	if !ok || r.Method != http.MethodConnect {
		http.Error(w, "ate-node egress: "+host+" is not in the egress policy of "+p.actor, http.StatusForbidden)
		return
	}
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	fmt.Fprint(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	tlsConn := tls.Server(conn, &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return p.ca.leaf(host)
	}})
	in := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(in)
		if err != nil {
			return
		}
		req.URL.Scheme, req.URL.Host, req.RequestURI = "https", r.Host, ""
		for _, h := range headers {
			secret, err := p.secrets.get(context.Background(), h.CredentialURI)
			if err != nil {
				slog.Error("egress secret", "actor", p.actor, "uri", h.CredentialURI, "err", err)
				return
			}
			req.Header.Set(h.Header, h.Prefix+secret)
		}
		resp, err := upstream.RoundTrip(req)
		if err != nil {
			slog.Error("egress", "actor", p.actor, "host", host, "err", err)
			return
		}
		err = resp.Write(tlsConn)
		resp.Body.Close()
		if err != nil || resp.Close {
			return
		}
	}
}

// upstream speaks HTTP/1.1 only, so its responses go back to the actor's
// HTTP/1.1 connection as they came.
var upstream = &http.Transport{TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}

func (p *egressProxy) allowed(host string) ([]ReplaceHeader, bool) {
	if p.policy == nil {
		return nil, false
	}
	for _, rule := range p.policy.Rules {
		if slices.Contains(rule.HTTPS.Hostnames, host) {
			return rule.HTTPS.Effects.ReplaceHeaders, true
		}
	}
	return nil, false
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

// listen opens an actor's egress listener on addr.
func (p *egressProxy) listen(addr string) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: p}
	go srv.Serve(ln)
	return srv, nil
}
