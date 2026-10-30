//go:build linux

package devatelet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/taeold/ate-ish/third_party/substrate/exported/credbundle"
	"github.com/taeold/ate-ish/third_party/substrate/exported/proto/ateletpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// PolicyLookup returns an actor's current egress policy, translated into
// the wire shape atunnel's DNS relay and egress path check queries against
// (GetActorEgressPolicy). A nil policy (ok, nil error) denies all of the
// actor's egress, DNS included — atunnel's own dns.allowQuery comment: "An
// actor without a policy may resolve nothing, as it may reach nothing
// through the egress gateway."
type PolicyLookup func(ctx context.Context, atespace, actorName string) (*ateletpb.EgressPolicy, error)

// fakeAtelet implements just the five WorkerSupport RPCs the vendored
// worker's register package and atunnel/egress package call; every other
// RPC is unimplemented.
type fakeAtelet struct {
	ateletpb.UnimplementedWorkerSupportServer
	pki    *PKI
	policy PolicyLookup
}

func (f *fakeAtelet) AcquireWorkerEpoch(ctx context.Context, req *ateletpb.AcquireWorkerEpochRequest) (*ateletpb.AcquireWorkerEpochResponse, error) {
	log.Printf("[fake-atelet] AcquireWorkerEpoch")
	return &ateletpb.AcquireWorkerEpochResponse{Epoch: 1}, nil
}

func (f *fakeAtelet) RegisterWorker(ctx context.Context, req *ateletpb.RegisterWorkerRequest) (*ateletpb.RegisterWorkerResponse, error) {
	log.Printf("[fake-atelet] RegisterWorker epoch=%d ips=%v capacity=%v", req.GetEpoch(), req.GetIps(), req.GetCapacity())
	return &ateletpb.RegisterWorkerResponse{}, nil
}

func (f *fakeAtelet) SetWorkerSandboxRuntimes(ctx context.Context, req *ateletpb.SetWorkerSandboxRuntimesRequest) (*ateletpb.SetWorkerSandboxRuntimesResponse, error) {
	log.Printf("[fake-atelet] SetWorkerSandboxRuntimes epoch=%d classes=%d", req.GetEpoch(), len(req.GetSandboxClasses()))
	return &ateletpb.SetWorkerSandboxRuntimesResponse{}, nil
}

func (f *fakeAtelet) MintActorCertificate(ctx context.Context, req *ateletpb.MintActorCertificateRequest) (*ateletpb.MintActorCertificateResponse, error) {
	csr, err := x509.ParseCertificateRequest(req.GetCertificateSigningRequest())
	if err != nil {
		return nil, fmt.Errorf("parsing actor CSR: %w", err)
	}
	der, err := f.pki.issueActorCertificate(csr)
	if err != nil {
		return nil, fmt.Errorf("issuing actor certificate: %w", err)
	}
	log.Printf("[fake-atelet] MintActorCertificate actor=%s/%s uid=%s", req.GetActorAtespace(), req.GetActorName(), req.GetActorUid())
	return &ateletpb.MintActorCertificateResponse{ActorCertificates: [][]byte{der}}, nil
}

func (f *fakeAtelet) GetActorEgressPolicy(ctx context.Context, req *ateletpb.GetActorEgressPolicyRequest) (*ateletpb.GetActorEgressPolicyResponse, error) {
	if f.policy == nil {
		return &ateletpb.GetActorEgressPolicyResponse{}, nil
	}
	policy, err := f.policy(ctx, req.GetActorAtespace(), req.GetActorName())
	if err != nil {
		return nil, fmt.Errorf("looking up egress policy for %s/%s: %w", req.GetActorAtespace(), req.GetActorName(), err)
	}
	return &ateletpb.GetActorEgressPolicyResponse{EgressPolicy: policy}, nil
}

// ServeFakeAtelet listens on socketPath (removing any stale socket first) and
// serves the WorkerSupport service with mTLS matching ateletdial.TLSConfig's
// expectations: a server certificate carrying the AteletSPIFFEID URI and the
// probe's Pod identity. policy may be nil, denying every actor's DNS and
// egress (matching an actor with no egress policy at all).
func ServeFakeAtelet(ctx context.Context, socketPath string, pki *PKI, policy PolicyLookup) (*grpc.Server, error) {
	if err := os.RemoveAll(socketPath); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, err
	}
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}

	cert, err := credbundle.Parse(filepath.Join(pki.dir, "atelet-credential-bundle.pem"))
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{*cert},
		ClientAuth:   tls.RequireAnyClientCert, // logged, not chain-verified: this is a test double
	}

	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	ateletpb.RegisterWorkerSupportServer(srv, &fakeAtelet{pki: pki, policy: policy})
	go func() {
		if err := srv.Serve(lis); err != nil {
			log.Printf("[fake-atelet] Serve exited: %v", err)
		}
	}()
	log.Printf("[fake-atelet] serving on %s", socketPath)
	return srv, nil
}
