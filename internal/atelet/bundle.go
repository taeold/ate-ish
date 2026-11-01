package atelet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
)

// actorDirs holds one actor's on-disk layout. Stage A owns this layout
// directly (the vendored worker's nodepath package named it before);
// nothing outside this node reads these paths.
type actorDirs struct {
	root           string
	ociBundleDir   string
	checkpointDir  string
	durableRoot    string
	rootfsUpperDir string
	systemInfoRoot string
	ingressSocket  string
}

func buildActorDirs(baseDir, actorUID string) *actorDirs {
	root := filepath.Join(baseDir, "actors", actorUID)
	return &actorDirs{
		root:           root,
		ociBundleDir:   filepath.Join(root, "bundle"),
		checkpointDir:  filepath.Join(root, "checkpoint"),
		durableRoot:    filepath.Join(root, "durable-dirs"),
		rootfsUpperDir: filepath.Join(root, "rootfs-upper"),
		systemInfoRoot: filepath.Join(root, "system-info"),
		ingressSocket:  filepath.Join(root, "ingress.sock"),
	}
}

func prepareActorDirs(d *actorDirs, volumeNames []string, systemInfoNames []string) error {
	dirs := []string{d.root, d.ociBundleDir, d.checkpointDir, d.durableRoot, d.systemInfoRoot}
	for _, name := range volumeNames {
		dirs = append(dirs, filepath.Join(d.durableRoot, name))
	}
	for _, name := range systemInfoNames {
		dirs = append(dirs, filepath.Join(d.systemInfoRoot, name))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %q: %w", dir, err)
		}
	}
	return nil
}

// imageRootfsDir exports image's filesystem once into <dir>/rootfs/<digest>
// and returns that directory, for use as an actor's runsc overlay
// lowerdir: this build's only source of an actor rootfs (docs/ROOTLESS.md
// drops the old node-filesystem-as-rootfs fallback and its Cloud Run gen2
// copy workaround along with it). Exporting is keyed by the image's own
// content digest, so a second actor (or a second `ate-ish dev` run)
// naming the same image reuses the same export instead of re-pulling or
// re-exporting it.
//
// Two export paths, same output shape (a plain directory holding the
// image's flattened filesystem): `docker create` + `docker export` when a
// docker daemon is reachable; otherwise go-containerregistry's crane
// library, which needs no daemon at all.
func imageRootfsDir(dir, image string) (string, error) {
	digest, dockerAvailable := imageDigest(image)
	dest := filepath.Join(dir, "rootfs", sanitizeDigest(digest))
	marker := filepath.Join(dest, ".ate-ish-complete")
	if _, err := os.Stat(marker); err == nil {
		return dest, nil
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	t0 := time.Now()
	var exportErr error
	if dockerAvailable {
		exportErr = exportImageDocker(image, dest)
	} else {
		exportErr = exportImageCrane(image, dest)
	}
	if exportErr != nil {
		os.RemoveAll(dest)
		return "", fmt.Errorf("exporting %s's rootfs: %w", image, exportErr)
	}
	if err := os.WriteFile(marker, []byte(digest), 0o644); err != nil {
		return "", err
	}
	slog.Info("exported image rootfs", "image", image, "digest", digest, "dest", dest,
		"docker", dockerAvailable, "took_ms", time.Since(t0).Milliseconds())
	return dest, nil
}

// imageDigest returns image's content digest and whether a docker daemon
// answered the lookup (the caller then uses the same tool to export).
func imageDigest(image string) (digest string, dockerAvailable bool) {
	// ATE_ROOTFS_EXPORT=crane forces the no-daemon path even when docker
	// itself answers.
	if env("ATE_ROOTFS_EXPORT", "") != "crane" {
		if out, err := exec.Command("docker", "inspect", "--format", "{{.Id}}", image).Output(); err == nil {
			return strings.TrimSpace(string(out)), true
		}
	}
	if d, err := crane.Digest(image); err == nil {
		return d, false
	}
	useDocker := env("ATE_ROOTFS_EXPORT", "") != "crane" && exec.Command("docker", "image", "inspect", image).Run() == nil
	return image, useDocker
}

func sanitizeDigest(d string) string {
	return strings.NewReplacer(":", "-", "/", "_").Replace(d)
}

// exportImageDocker is imageRootfsDir's docker path: create a (never
// started) container from image, export its filesystem as a tar stream,
// and extract it into dest.
func exportImageDocker(image, dest string) error {
	out, err := exec.Command("docker", "create", image).Output()
	if err != nil {
		return fmt.Errorf("docker create: %w", err)
	}
	cid := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", cid).Run()
	export := exec.Command("docker", "export", cid)
	extract := exec.Command("tar", "-C", dest, "--xattrs", "-xpf", "-")
	pr, pw := io.Pipe()
	export.Stdout, extract.Stdin = pw, pr
	var exportErr, extractErr bytes.Buffer
	export.Stderr, extract.Stderr = &exportErr, &extractErr
	if err := extract.Start(); err != nil {
		return err
	}
	if err := export.Start(); err != nil {
		return err
	}
	go func() { export.Wait(); pw.Close() }()
	if err := extract.Wait(); err != nil {
		return fmt.Errorf("%w (export: %s) (extract: %s)", err, exportErr.String(), extractErr.String())
	}
	return nil
}

// exportImageCrane is imageRootfsDir's no-daemon path.
func exportImageCrane(image, dest string) error {
	img, err := crane.Pull(image, crane.WithContext(context.Background()))
	if err != nil {
		return fmt.Errorf("crane pull: %w", err)
	}
	extract := exec.Command("tar", "-C", dest, "--xattrs", "-xpf", "-")
	pr, pw := io.Pipe()
	extract.Stdin = pr
	var extractErr bytes.Buffer
	extract.Stderr = &extractErr
	if err := extract.Start(); err != nil {
		return err
	}
	exportErr := crane.Export(img, pw)
	pw.Close()
	if err := extract.Wait(); err != nil {
		return fmt.Errorf("%w (export: %v) (extract: %s)", err, exportErr, extractErr.String())
	}
	return exportErr
}
