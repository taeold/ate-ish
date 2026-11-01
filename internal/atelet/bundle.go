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
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/taeold/ate-ish/internal/sandbox"
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
// It also resolves the image's own USER directive, Config.User, to a
// numeric uid/gid via the exported rootfs's own /etc/passwd and
// /etc/group (imageUserUIDGID): empty, the only value every template
// before this one produced, resolves to 0, 0 (root) with no lookup.
func imageRootfsDir(dir, image string, ns *sandbox.Namespace) (rootfs string, uid, gid uint32, err error) {
	digest, dockerAvailable := imageDigest(image)
	// Keyed on the namespace's own id-mapping mode, not the digest
	// alone: a root-only node's export of a non-root image comes out
	// owned by this node's single mapped id throughout (extractCmd's own
	// chown attempts fail for anything else); a multi-ID node's export
	// of the same digest instead preserves each entry's real uid/gid.
	// The two are not interchangeable, so sharing one cache slot between
	// them would silently serve whichever ran first to a later node
	// using the other mode (found by hack/smoke.sh running both in the
	// same cacheDir).
	cacheSuffix := "-root"
	if ns.MultiIDMapping() {
		cacheSuffix = "-multiid"
	}
	dest := filepath.Join(dir, "rootfs", sanitizeDigest(digest)+cacheSuffix)
	marker := filepath.Join(dest, ".ate-ish-complete")
	if _, statErr := os.Stat(marker); statErr != nil {
		if err := os.RemoveAll(dest); err != nil {
			return "", 0, 0, err
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return "", 0, 0, err
		}
		t0 := time.Now()
		var exportErr error
		if dockerAvailable {
			exportErr = exportImageDocker(image, dest, ns)
		} else {
			exportErr = exportImageCrane(image, dest, ns)
		}
		if exportErr != nil {
			os.RemoveAll(dest)
			return "", 0, 0, fmt.Errorf("exporting %s's rootfs: %w", image, exportErr)
		}
		if err := os.WriteFile(marker, []byte(digest), 0o644); err != nil {
			return "", 0, 0, err
		}
		slog.Info("exported image rootfs", "image", image, "digest", digest, "dest", dest,
			"docker", dockerAvailable, "took_ms", time.Since(t0).Milliseconds())
	}
	userSpec, err := imageUserSpec(image, dockerAvailable)
	if err != nil {
		return "", 0, 0, err
	}
	if uid, gid, err = imageUserUIDGID(userSpec, dest); err != nil {
		return "", 0, 0, err
	}
	return dest, uid, gid, nil
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

// imageUserSpec returns image's own Config.User (Dockerfile USER
// directive): "", a bare name or uid, or "name-or-uid:group-or-gid".
// Empty means root, the only value every template before this one ever
// produced.
func imageUserSpec(image string, dockerAvailable bool) (string, error) {
	if dockerAvailable {
		out, err := exec.Command("docker", "inspect", "--format", "{{.Config.User}}", image).Output()
		if err != nil {
			return "", fmt.Errorf("docker inspect %s's user: %w", image, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	img, err := crane.Pull(image, crane.WithContext(context.Background()))
	if err != nil {
		return "", fmt.Errorf("crane pull %s: %w", image, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return "", fmt.Errorf("reading %s's config: %w", image, err)
	}
	return cfg.Config.User, nil
}

// imageUserUIDGID resolves imageUserSpec's string to a numeric uid/gid
// by looking it up in rootfsDir's own /etc/passwd and /etc/group,
// exactly how docker and podman resolve a named USER: the image's own
// filesystem is the only authority for what its usernames mean, there is
// no host-side passwd entry to fall back to. An empty spec (the common,
// root case) returns 0, 0 without reading anything.
func imageUserUIDGID(spec string, rootfsDir string) (uid, gid uint32, err error) {
	if spec == "" {
		return 0, 0, nil
	}
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	uid, primaryGID, err := resolvePasswdEntry(filepath.Join(rootfsDir, "etc", "passwd"), userPart)
	if err != nil {
		return 0, 0, fmt.Errorf("resolving USER %q: %w", spec, err)
	}
	gid = primaryGID
	if hasGroup {
		if gid, err = resolveGroupEntry(filepath.Join(rootfsDir, "etc", "group"), groupPart); err != nil {
			return 0, 0, fmt.Errorf("resolving USER %q's group: %w", spec, err)
		}
	}
	return uid, gid, nil
}

// resolvePasswdEntry resolves part (a name or a numeric uid) against
// path (an image's own /etc/passwd), returning its uid and its primary
// gid (passwd's own 4th field), used when the USER directive named no
// explicit group.
func resolvePasswdEntry(path, part string) (uid, gid uint32, err error) {
	if n, err := strconv.ParseUint(part, 10, 32); err == nil {
		return uint32(n), 0, nil // a bare numeric uid has no name to look up a group for
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("%s has no numeric uid and the image has no %s to look it up in: %w", part, path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 || f[0] != part {
			continue
		}
		u, err1 := strconv.ParseUint(f[2], 10, 32)
		g, err2 := strconv.ParseUint(f[3], 10, 32)
		if err1 != nil || err2 != nil {
			continue
		}
		return uint32(u), uint32(g), nil
	}
	return 0, 0, fmt.Errorf("no user %q in %s", part, path)
}

// resolveGroupEntry resolves part (a name or a numeric gid) against path
// (an image's own /etc/group).
func resolveGroupEntry(path, part string) (uint32, error) {
	if n, err := strconv.ParseUint(part, 10, 32); err == nil {
		return uint32(n), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("%s has no numeric gid and the image has no %s to look it up in: %w", part, path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 3 || f[0] != part {
			continue
		}
		g, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		return uint32(g), nil
	}
	return 0, fmt.Errorf("no group %q in %s", part, path)
}

func sanitizeDigest(d string) string {
	return strings.NewReplacer(":", "-", "/", "_").Replace(d)
}

// extractCmd returns the "tar -xpf -" command imageRootfsDir's two
// export paths pipe a tar stream into. Run inside ns (when given) rather
// than as this plain host process: tar's own -p restores each entry's
// uid/gid only when the extracting process can actually chown to it,
// which an unprivileged host process cannot (it silently keeps every
// file as its own real uid instead, the long-standing reason a non-root
// actor image's rootfs used to come out owned by this node's own real
// user no matter what the image itself declared). Run as ns's own
// mapped root, chown to any id that namespace's own mapping covers
// succeeds, so a multi-ID mapping (idmap.go) makes extraction preserve
// an image's real per-file ownership, not just this node's single
// mapped id.
func extractCmd(ns *sandbox.Namespace, dest string) *exec.Cmd {
	args := []string{"-C", dest, "--xattrs", "-xpf", "-"}
	if ns == nil {
		return exec.Command("tar", args...)
	}
	return ns.Enter("tar", args...)
}

// tarExtractErr turns extractCmd's Wait error into nil when stderr shows
// only chown failures: expected under a root-only mapping (idmap.go),
// where ns-root has CAP_CHOWN but the specific id an entry names is not
// one the namespace maps, so the kernel refuses it with EINVAL. GNU tar
// (running as this namespace's own root) treats that as fatal on its
// own; the file's bytes and every other entry's ownership still landed
// fine, so this package downgrades it to the one warning line logged
// below rather than failing the whole export over it. Any other tar
// error (a real read/write failure, a corrupt stream) still propagates.
func tarExtractErr(err error, stderr string) error {
	if err == nil {
		return nil
	}
	benign := true
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.Contains(line, "Cannot change ownership to") && line != "tar: Exiting with failure status due to previous errors" {
			benign = false
			break
		}
	}
	if benign && stderr != "" {
		slog.Warn("tar could not restore every file's ownership while extracting an image (expected under a root-only id mapping; see docs/ROOTLESS.md's G5)", "stderr", stderr)
		return nil
	}
	return err
}

// exportImageDocker is imageRootfsDir's docker path: create a (never
// started) container from image, export its filesystem as a tar stream,
// and extract it into dest.
func exportImageDocker(image, dest string, ns *sandbox.Namespace) error {
	out, err := exec.Command("docker", "create", image).Output()
	if err != nil {
		return fmt.Errorf("docker create: %w", err)
	}
	cid := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", cid).Run()
	export := exec.Command("docker", "export", cid)
	extract := extractCmd(ns, dest)
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
	if err := tarExtractErr(extract.Wait(), extractErr.String()); err != nil {
		return fmt.Errorf("%w (export: %s) (extract: %s)", err, exportErr.String(), extractErr.String())
	}
	return nil
}

// exportImageCrane is imageRootfsDir's no-daemon path.
func exportImageCrane(image, dest string, ns *sandbox.Namespace) error {
	img, err := crane.Pull(image, crane.WithContext(context.Background()))
	if err != nil {
		return fmt.Errorf("crane pull: %w", err)
	}
	extract := extractCmd(ns, dest)
	pr, pw := io.Pipe()
	extract.Stdin = pr
	var extractErr bytes.Buffer
	extract.Stderr = &extractErr
	if err := extract.Start(); err != nil {
		return err
	}
	exportErr := crane.Export(img, pw)
	pw.Close()
	if err := tarExtractErr(extract.Wait(), extractErr.String()); err != nil {
		return fmt.Errorf("%w (export: %v) (extract: %s)", err, exportErr, extractErr.String())
	}
	return exportErr
}
