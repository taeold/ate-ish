//go:build linux

// Package devatelet stands in for the one piece of atelet the vendored
// ateworker-gvisor cannot start without: a credential broker on a unix
// socket, speaking the mTLS handshake internal/ateletdial.TLSConfig expects
// (a SPIFFE URI SAN plus a Pod-identity X.509 extension with matching
// NodeName/NodeUID on both ends), and signing authority for the short-lived
// actor certificates egress_gateway needs. It throws away its own CA on
// every run; nothing it mints is meant to outlive one ate-ish process.
//
// Lifted from the stage-0 probe (/tmp/substrate/cmd/ateish-probe) that first
// proved the vendored worker runs standalone; worker.go is stage 2's caller.
package devatelet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/taeold/ate-ish/third_party/substrate/exported/credbundle"
	"github.com/taeold/ate-ish/third_party/substrate/exported/substratex509"
)

// probePodIdentity is the shared Pod identity both the fake atelet and the
// worker's own credential present: ateletdial.TLSConfig requires both ends to
// carry the identical NodeName/NodeUID.
var probePodIdentity = &substratex509.PodIdentity{
	Namespace:          "ate-system",
	ServiceAccountName: "probe",
	ServiceAccountUID:  "probe-sa-uid",
	PodName:            "probe-pod",
	PodUID:             "probe-pod-uid",
	NodeName:           "ate-ish-probe-node",
	NodeUID:            "ate-ish-probe-node-uid",
}

type caKeyPair struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newCA(cn string) (*caKeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &caKeyPair{cert: cert, key: key, der: der}, nil
}

// leafOptions configures one leaf certificate signed by a caKeyPair.
type leafOptions struct {
	cn          string
	spiffeID    string
	podIdentity *substratex509.PodIdentity
	ips         []net.IP
	dnsNames    []string
	extKeyUsage []x509.ExtKeyUsage
}

func (ca *caKeyPair) issue(o leafOptions) (certDER []byte, key *ecdsa.PrivateKey, err error) {
	key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: o.cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  o.extKeyUsage,
		IPAddresses:  o.ips,
		DNSNames:     o.dnsNames,
	}
	if o.spiffeID != "" {
		u, err := url.Parse(o.spiffeID)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing spiffe id %q: %w", o.spiffeID, err)
		}
		tmpl.URIs = []*url.URL{u}
	}
	if o.podIdentity != nil {
		if err := substratex509.AddPodIdentityToCertificate(o.podIdentity, tmpl); err != nil {
			return nil, nil, fmt.Errorf("adding pod identity: %w", err)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	return der, key, nil
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// writeCredentialBundle writes a credbundle.Parse-compatible file: a PRIVATE
// KEY block followed by the leaf certificate (and any intermediates).
func writeCredentialBundle(path string, key *ecdsa.PrivateKey, chainDER ...[]byte) error {
	keyPEM, err := pemKey(key)
	if err != nil {
		return err
	}
	out := append([]byte{}, keyPEM...)
	for _, der := range chainDER {
		out = append(out, pemCert(der)...)
	}
	return os.WriteFile(path, out, 0o600)
}

func writeTrustBundle(path string, cas ...*caKeyPair) error {
	var out []byte
	for _, ca := range cas {
		out = append(out, pemCert(ca.der)...)
	}
	return os.WriteFile(path, out, 0o644)
}

// PKI holds every certificate ate-ish-probe generates: one CA stands in
// for the cluster's Pod-identity signer (atelet and the worker's own
// credential, plus every actor certificate minted at RunWorkload time), and a
// second, independent CA stands in for the egress gateway's own serving CA,
// matching production's separate trust domains for node identity and the
// gateway.
type PKI struct {
	dir          string
	nodeCA       *caKeyPair
	gateCA       *caKeyPair
	ateletSPIFFE string
}

// File names GeneratePKI writes under its dir, exported so callers (the
// worker subprocess's flags, the egress gateway's own listener) can find
// them without reaching into PKI's unexported fields.
const (
	WorkerCredentialBundleFile  = "worker-credential-bundle.pem"
	AteletCredentialBundleFile  = "atelet-credential-bundle.pem"
	NodeTrustBundleFile         = "node-trust-bundle.pem"
	GatewayCredentialBundleFile = "gateway-credential-bundle.pem"
	GatewayTrustBundleFile      = "gateway-trust-bundle.pem"
)

// Path joins the PKI's directory with one of the File constants above.
func (p *PKI) Path(file string) string { return filepath.Join(p.dir, file) }

// GatewayCertificate parses the gateway's own serving credential, for the
// egress gateway listener (egress.go) to present.
func (p *PKI) GatewayCertificate() (tls.Certificate, error) {
	cert, err := credbundle.Parse(p.Path(GatewayCredentialBundleFile))
	if err != nil {
		return tls.Certificate{}, err
	}
	return *cert, nil
}

func GeneratePKI(dir string, ateletSPIFFE string) (*PKI, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	nodeCA, err := newCA("ate-ish-probe node CA")
	if err != nil {
		return nil, fmt.Errorf("node CA: %w", err)
	}
	gateCA, err := newCA("ate-ish-probe gateway CA")
	if err != nil {
		return nil, fmt.Errorf("gateway CA: %w", err)
	}

	// The worker's own credential, used to authenticate to our fake atelet.
	workerDER, workerKey, err := nodeCA.issue(leafOptions{
		cn:          "worker",
		podIdentity: probePodIdentity,
		extKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("worker cert: %w", err)
	}
	if err := writeCredentialBundle(filepath.Join(dir, "worker-credential-bundle.pem"), workerKey, workerDER); err != nil {
		return nil, err
	}

	// The fake atelet's credential, presented back to the worker.
	ateletDER, ateletKey, err := nodeCA.issue(leafOptions{
		cn:          "atelet",
		spiffeID:    ateletSPIFFE,
		podIdentity: probePodIdentity,
		extKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("atelet cert: %w", err)
	}
	if err := writeCredentialBundle(filepath.Join(dir, "atelet-credential-bundle.pem"), ateletKey, ateletDER); err != nil {
		return nil, err
	}

	if err := writeTrustBundle(filepath.Join(dir, "node-trust-bundle.pem"), nodeCA); err != nil {
		return nil, err
	}

	// The gateway's serving credential, trusted by the egress trust bundle.
	gatewayDER, gatewayKey, err := gateCA.issue(leafOptions{
		cn:          "egress-gateway",
		ips:         []net.IP{net.ParseIP("127.0.0.1")},
		dnsNames:    []string{"localhost"},
		extKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, fmt.Errorf("gateway cert: %w", err)
	}
	if err := writeCredentialBundle(filepath.Join(dir, "gateway-credential-bundle.pem"), gatewayKey, gatewayDER); err != nil {
		return nil, err
	}
	if err := writeTrustBundle(filepath.Join(dir, "gateway-trust-bundle.pem"), gateCA); err != nil {
		return nil, err
	}

	return &PKI{dir: dir, nodeCA: nodeCA, gateCA: gateCA, ateletSPIFFE: ateletSPIFFE}, nil
}

// issueActorCertificate signs an actor CSR from the node CA, the way the fake
// atelet's MintActorCertificate handler does: the caller already parsed and
// holds the CSR's public key. The actor's atespace/name go in as a URI SAN
// (ate-actor://<atespace>/<name>) rather than the Subject, so the real
// egress gateway can map a verified client certificate straight to an
// actor identity and look up its policy by that, not by which listener
// port accepted the connection (there is only one gateway listener for
// every actor now).
func (p *PKI) issueActorCertificate(csr *x509.CertificateRequest, atespace, name string) ([]byte, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "actor"},
		URIs:         []*url.URL{{Scheme: "ate-actor", Host: atespace, Path: "/" + name}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.nodeCA.cert, csr.PublicKey, p.nodeCA.key)
	if err != nil {
		return nil, err
	}
	return der, nil
}

// ActorIdentity parses the atespace/name a verified actor client
// certificate carries, as issueActorCertificate wrote it.
func ActorIdentity(cert *x509.Certificate) (atespace, name string, ok bool) {
	if len(cert.URIs) != 1 || cert.URIs[0].Scheme != "ate-actor" {
		return "", "", false
	}
	name = strings.TrimPrefix(cert.URIs[0].Path, "/")
	if cert.URIs[0].Host == "" || name == "" {
		return "", "", false
	}
	return cert.URIs[0].Host, name, true
}

// NodeCAPEM returns the node CA's own certificate, PEM-encoded: the real
// egress gateway verifies atunnel's client certificates against this pool,
// since the node CA is what issueActorCertificate signs them with.
func (p *PKI) NodeCAPEM() []byte { return pemCert(p.nodeCA.der) }
