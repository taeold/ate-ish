package atelet

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/taeold/ate-ish/internal/devatelet"
	"github.com/taeold/ate-ish/internal/records"
	"github.com/taeold/ate-ish/internal/store"
	"github.com/taeold/ate-ish/third_party/substrate/exported/proto/ateletpb"
	"github.com/taeold/ate-ish/third_party/substrate/exported/proto/ateworkerpb"
)

// actorPolicyLookup reads the egress policy already stored on the actor's
// own bucket record (the one `ate-ish create egress-policy` writes) and
// translates it for atunnel's DNS relay and egress path, which both check
// hostnames against the *ateletpb.EgressPolicy* shape, not ate-ish's own
// simpler EgressPolicy JSON type. This is a hostname allow-list lookup, not
// credential injection: the hostname just has to be the one the policy
// names for its DNS query and its CONNECT to resolve and reach the gateway
// at all. Replacing a placeholder API key with the real secret, inside the
// gateway's own TLS termination, is stage 3's job.
func actorPolicyLookup(b store.Store) devatelet.PolicyLookup {
	return func(ctx context.Context, atespace, name string) (*ateletpb.EgressPolicy, error) {
		var rec records.Actor
		if _, err := b.Get(ctx, records.ActorKey(atespace, name), &rec); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil
			}
			return nil, err
		}
		if rec.EgressPolicy == nil {
			return nil, nil
		}
		out := &ateletpb.EgressPolicy{}
		for _, rule := range rec.EgressPolicy.Rules {
			if len(rule.HTTPS.Hostnames) == 0 {
				continue
			}
			out.Rules = append(out.Rules, &ateletpb.EgressRule{
				Https: &ateletpb.EgressDestinations{Hostnames: rule.HTTPS.Hostnames},
			})
		}
		return out, nil
	}
}

// actorUID returns a fresh lowercase UUID: ateworkerpb's actor_uid field and
// nodepath.ActorSandboxAssetsDir both require a DNS-1123 label, which a
// standard lowercase UUID already is.
func newActorUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}

// fidelityOf maps a Template's preferredFidelity to the proto enum.
func fidelityOf(tmpl records.Template) ateworkerpb.SnapshotFidelity {
	if strings.EqualFold(tmpl.PreferredFidelity, "MEMORY") {
		return ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
	}
	return ateworkerpb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES
}

// containerName returns c's name, defaulting to "app" for a template that
// left it blank.
func containerName(c records.Container) string {
	if c.Name != "" {
		return c.Name
	}
	return "app"
}

// buildSpec builds the one-container WorkloadSpec the vendored worker reads,
// from ate-ish's own Template/Container shape.
func buildSpec(tmpl records.Template) (*ateworkerpb.WorkloadSpec, error) {
	c := tmpl.Containers[0]
	name := containerName(c)
	// The sandbox's rootfs is the node's own filesystem (see bundle.go), so
	// the node's own PATH applies; runsc's container process otherwise
	// starts with no PATH at all ("error finding executable ... in PATH
	// []"), since gVisor does not consult the host environment or
	// rootfs-image config for one.
	cs := &ateworkerpb.ContainerSpec{
		Args: append(append([]string{}, c.Command...), c.Args...),
		Env:  []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	}
	for _, e := range c.Env {
		cs.Env = append(cs.Env, e.Name+"="+e.Value)
	}
	container := &ateworkerpb.Container{Name: name, ContainerSpec: cs}
	for _, m := range c.VolumeMounts {
		i := indexVolume(tmpl.Volumes, m.Name)
		if i < 0 {
			return nil, fmt.Errorf("volumeMount %q has no volume", m.Name)
		}
		switch v := tmpl.Volumes[i]; {
		case v.DurableDir != nil:
			container.DurableDirVolumeMounts = append(container.DurableDirVolumeMounts,
				&ateworkerpb.VolumeMount{Name: m.Name, MountPath: m.MountPath})
		case v.SystemInfo != nil:
			container.SystemInfoVolumeMounts = append(container.SystemInfoVolumeMounts,
				&ateworkerpb.VolumeMount{Name: m.Name, MountPath: m.MountPath})
		}
	}
	return &ateworkerpb.WorkloadSpec{Containers: []*ateworkerpb.Container{container}}, nil
}

func indexVolume(vols []records.Volume, name string) int {
	for i, v := range vols {
		if v.Name == name {
			return i
		}
	}
	return -1
}

// durableVolumeNames/systemInfoVolumeNames list the volume names
// prepareActorDirs needs to create a subdirectory for.
func durableVolumeNames(tmpl records.Template) (durable, systemInfo []string) {
	for _, m := range tmpl.Containers[0].VolumeMounts {
		i := indexVolume(tmpl.Volumes, m.Name)
		if i < 0 {
			continue
		}
		switch v := tmpl.Volumes[i]; {
		case v.DurableDir != nil:
			durable = append(durable, m.Name)
		case v.SystemInfo != nil:
			systemInfo = append(systemInfo, m.Name)
		}
	}
	return durable, systemInfo
}

// writeSystemInfoFiles writes the trust-bundle file(s) a systemInfo volume's
// dataSources ask for. ate-ish is not MITMing egress yet (stage 3), so the
// content does not need to be a trust anchor the actor's TLS actually needs;
// it only needs to exist, so NODE_EXTRA_CA_CERTS does not fail to load it.
func writeSystemInfoFiles(d *actorDirs, tmpl records.Template, pemBytes []byte) error {
	for _, m := range tmpl.Containers[0].VolumeMounts {
		i := indexVolume(tmpl.Volumes, m.Name)
		if i < 0 || tmpl.Volumes[i].SystemInfo == nil {
			continue
		}
		for _, ds := range tmpl.Volumes[i].SystemInfo.DataSources {
			if ds.TrustBundle == nil {
				continue
			}
			path := filepath.Join(d.systemInfoRoot, m.Name, ds.TrustBundle.Path)
			if err := os.WriteFile(path, pemBytes, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// tarDir writes dir's contents as a gzipped tar at tarPath.
func tarDir(dir, tarPath string) error {
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
}

// untarDir extracts a gzipped tar written by tarDir into dir.
func untarDir(tarPath, dir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
}
