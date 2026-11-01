package atelet

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/taeold/ate-ish/internal/records"
)

// newActorUID returns a fresh lowercase UUID: it is both the runsc
// container id and the bundle directory name, so it must be a safe path
// component, which a standard lowercase UUID already is.
func newActorUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}

// fidelityIsMemory reports whether tmpl asks for a MEMORY-fidelity
// checkpoint (the process's own memory state, not just its durable
// dirs).
func fidelityIsMemory(tmpl records.Template) bool {
	return strings.EqualFold(tmpl.PreferredFidelity, "MEMORY")
}

// containerName returns c's name, defaulting to "app" for a template that
// left it blank.
func containerName(c records.Container) string {
	if c.Name != "" {
		return c.Name
	}
	return "app"
}

// actorCommandEnv builds the actor's command and environment from
// ate-ish's own Template/Container shape.
func actorCommandEnv(c records.Container) (command, env []string) {
	command = append(append([]string{}, c.Command...), c.Args...)
	for _, e := range c.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	return command, env
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

// volumeMounts returns the actor's durable-dir and system-info volume
// mounts as host-dir -> container-path maps, ready for
// sandbox.ActorSpec.
func volumeMounts(d *actorDirs, tmpl records.Template) (durable, systemInfo map[string]string, err error) {
	durable, systemInfo = map[string]string{}, map[string]string{}
	for _, m := range tmpl.Containers[0].VolumeMounts {
		i := indexVolume(tmpl.Volumes, m.Name)
		if i < 0 {
			return nil, nil, fmt.Errorf("volumeMount %q has no volume", m.Name)
		}
		switch v := tmpl.Volumes[i]; {
		case v.DurableDir != nil:
			durable[filepath.Join(d.durableRoot, m.Name)] = m.MountPath
		case v.SystemInfo != nil:
			systemInfo[filepath.Join(d.systemInfoRoot, m.Name)] = m.MountPath
		}
	}
	return durable, systemInfo, nil
}

// writeSystemInfoFiles writes the trust-bundle file(s) a systemInfo
// volume's dataSources ask for.
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

// snapshotDirs names the two directories a suspended actor's uploaded
// snapshot must carry, as sibling top-level entries in the tarball: the
// runsc checkpoint runsc itself produced (process/memory state), and the
// durable-dir volumes' own content. Both matter for a resume on any
// worker in the fleet, not just the one that suspended it: suspend()
// deletes the actor's whole on-disk directory once the upload succeeds
// (another worker's disk has no reason to agree with this one's actor
// UIDs), so anything not in the tarball does not survive. A plain
// runsc-checkpoint-only tarball (this package's shape before this
// comment) silently dropped durable-dir content on every restore; the
// counter demo's own file counter caught it (resets to 1 across a
// suspend/resume instead of continuing).
const (
	snapshotCheckpointPrefix  = "checkpoint"
	snapshotDurablePrefix     = "durable-dirs"
	snapshotRootfsUpperPrefix = "rootfs-upper"
)

// tarDirs writes a gzipped tar at tarPath holding each dirs[prefix]
// directory's contents under that top-level prefix.
func tarDirs(dirs map[string]string, tarPath string) error {
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for prefix, dir := range dirs {
		if err := tarInto(tw, dir, prefix); err != nil {
			return fmt.Errorf("archiving %s: %w", dir, err)
		}
	}
	return nil
}

func tarInto(tw *tar.Writer, dir, prefix string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := prefix
		if rel != "." {
			name = prefix + "/" + rel
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
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

// untarDirs extracts a gzipped tar written by tarDirs, routing each
// top-level prefix in dirs to its matching destination directory.
func untarDirs(tarPath string, dirs map[string]string) error {
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
		prefix, rest, _ := strings.Cut(hdr.Name, "/")
		dest, ok := dirs[prefix]
		if !ok {
			continue
		}
		target := dest
		if rest != "" {
			target = filepath.Join(dest, rest)
		}
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
