package atelet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/taeold/ate-ish/third_party/substrate/exported/nodepath"
	"github.com/taeold/ate-ish/third_party/substrate/exported/proto/ateworkerpb"
)

// actorDirs holds one actor's ActorDirs plus the local paths worker.go needs
// that are not part of the proto message.
type actorDirs struct {
	pb               *ateworkerpb.ActorDirs
	sandboxAssetsDir string
	sandboxRunscPath string
	durableRoot      string
	systemInfoRoot   string
}

func buildActorDirs(baseDir, actorUID string) *actorDirs {
	root := filepath.Join(baseDir, "actors", actorUID)
	sandboxAssetsDir := nodepath.ActorSandboxAssetsDir(actorUID)
	checkpointDir := filepath.Join(root, "checkpoint")
	durableRoot := filepath.Join(root, "durable-dirs")
	systemInfoRoot := filepath.Join(root, "system-info")
	return &actorDirs{
		sandboxAssetsDir: sandboxAssetsDir,
		sandboxRunscPath: filepath.Join(sandboxAssetsDir, "runsc"),
		durableRoot:      durableRoot,
		systemInfoRoot:   systemInfoRoot,
		pb: &ateworkerpb.ActorDirs{
			RootDir:                   root,
			OciBundleDir:              filepath.Join(root, "bundle"),
			CheckpointDir:             checkpointDir,
			RestoreDir:                checkpointDir, // stage 2 skips the blob-storage hop; see worker.go's suspend/resume
			DurableDirVolumeMountsDir: durableRoot,
			SystemInfoVolumeRootsDir:  systemInfoRoot,
			ExternalVolumeMountsDir:   filepath.Join(root, "volumes"),
			SandboxAssetsDir:          sandboxAssetsDir,
		},
	}
}

func prepareActorDirs(d *actorDirs, volumeNames []string, systemInfoNames []string) error {
	dirs := []string{
		d.pb.GetRootDir(), d.pb.GetOciBundleDir(), d.pb.GetCheckpointDir(),
		d.pb.GetDurableDirVolumeMountsDir(), d.pb.GetSystemInfoVolumeRootsDir(), d.pb.GetExternalVolumeMountsDir(),
	}
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

// stageRunsc hardlinks a fresh copy of the runsc release (runsc plus its
// gvisor-bin/ sidecars; see docs/PROGRESS.md's stage-0 notes on why the
// whole layout is needed) into the actor's sandbox assets dir, the only
// place validateRunscPath lets a request name. Hardlinks instead of copies:
// cheap, and runscRelease never changes under a running actor.
//
// Stage 4 (two nodes, one host): isolating a node's nodepath.BasePath
// (/var/lib/ate) with a private bind mount, so two nodes' fixed vendored
// socket paths do not collide (see docs/PROGRESS.md's stage 4 notes), puts
// the actor's sandbox-assets dir on a different vfsmount than this
// relative runscRelease path even though both resolve to the same
// underlying device: link(2) refuses that with EXDEV (the kernel compares
// mount instances, not device numbers). copyFile is the fallback, used only
// when linking fails, so the single-node, same-mount case still gets the
// cheap hardlink.
func stageRunsc(runscRelease string, d *actorDirs) error {
	if err := os.RemoveAll(d.sandboxAssetsDir); err != nil {
		return err
	}
	if err := os.MkdirAll(d.sandboxAssetsDir, 0o755); err != nil {
		return err
	}
	return filepath.Walk(runscRelease, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(runscRelease, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(d.sandboxAssetsDir, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if err := os.Link(path, dst); err != nil {
			if !errors.Is(err, syscall.EXDEV) {
				return fmt.Errorf("linking %q: %w", path, err)
			}
			if err := copyFile(path, dst, info.Mode()); err != nil {
				return fmt.Errorf("copying %q (link crossed a mount): %w", path, err)
			}
		}
		return nil
	})
}

// copyFile is stageRunsc's fallback when path and dst are not on the same
// vfsmount (see stageRunsc's doc comment).
func copyFile(path, dst string, mode os.FileMode) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

// overlayfsSuperMagic is statfs(2)'s f_type for an overlayfs mount
// (uapi/linux/magic.h's OVERLAYFS_SUPER_MAGIC).
const overlayfsSuperMagic = 0x794c7630

// rootIsOverlayfs reports whether "/" itself is an overlayfs mount.
func rootIsOverlayfs() (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return false, fmt.Errorf("statfs /: %w", err)
	}
	return int64(st.Type) == overlayfsSuperMagic, nil
}

// rootfsLowerDir returns the directory to use as every actor's runsc
// overlay lowerdir. On a plain filesystem (every local/VM run so far),
// that is "/" itself, unchanged from stage 0 onward.
//
// Stage 5d: Cloud Run gen2's own "/" is already an overlayfs mount (the
// container runtime's own image layer), and the kernel's overlay driver
// refuses to stack a second overlay past its maximum depth — stage 5c
// confirmed this is not a mount-option problem (index=off and friends made
// no difference; removed here, since they are moot once lowerdir is never
// overlayfs again). The fix: copy the node filesystem once per node
// instance onto a real, non-overlay directory (locally a plain
// subdirectory; on Cloud Run gen2, a mounted ephemeral-disk volume), and
// use that copy as lowerdir for every actor on this node, instead of "/".
func rootfsLowerDir(dir string) (string, error) {
	isOverlay, err := rootIsOverlayfs()
	if err != nil {
		return "", err
	}
	if !isOverlay {
		return "/", nil
	}
	dest := filepath.Join(dir, "rootfs")
	if _, err := os.Stat(filepath.Join(dest, "bin")); err == nil {
		// A previous boot of this same node instance already copied it;
		// the copy outlives one actor (or even one worker subprocess
		// restart) and only needs to happen once per node instance.
		slog.Info("rootfs lowerdir already copied", "dest", dest)
		return dest, nil
	}
	t0 := time.Now()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	// Exclude: pseudo-filesystems that make no sense to snapshot; ate-ish's
	// own data directory (dir), which is where dest itself lives, and
	// nodepath.BasePath ("/var/lib/ate"), the vendored worker's own shared
	// socket/state directory — neither should be part of the actor's
	// rootfs image, and dir in particular would otherwise have the copy
	// try to copy itself into itself. The last four are container-runtime
	// and dev-workspace storage that is never part of a real actor's
	// rootfs on any host; added once local testing measured this dev VM's
	// own docker/containerd state and home directory at ~50GB (more than
	// its free disk), which a slim Cloud Run image never has in the first
	// place — a harmless no-op exclude there, since those paths do not
	// exist in the gen2 container.
	exclude := []string{"./proc", "./sys", "./dev", "./run", "./tmp",
		"." + nodepath.BasePath, "." + dir,
		"./var/lib/docker", "./var/lib/containerd", "./home", "./snap"}
	args := []string{"-C", "/", "--xattrs", "-cf", "-"}
	for _, e := range exclude {
		args = append(args, "--exclude="+e)
	}
	args = append(args, ".")
	create := exec.Command("tar", args...)
	extract := exec.Command("tar", "-C", dest, "--xattrs", "-xpf", "-")
	pr, pw := io.Pipe()
	create.Stdout, extract.Stdin = pw, pr
	var createErr, extractErr bytes.Buffer
	create.Stderr, extract.Stderr = &createErr, &extractErr
	if err := extract.Start(); err != nil {
		return "", fmt.Errorf("starting tar extract: %w", err)
	}
	if err := create.Start(); err != nil {
		return "", fmt.Errorf("starting tar create: %w", err)
	}
	go func() { create.Wait(); pw.Close() }()
	if err := extract.Wait(); err != nil {
		return "", fmt.Errorf("copying the node filesystem for %s: %w (create: %s) (extract: %s)",
			dest, err, createErr.String(), extractErr.String())
	}
	size := dirSize(dest)
	slog.Info("copied the node filesystem as the actor rootfs lowerdir", "dest", dest,
		"took_ms", time.Since(t0).Milliseconds(), "bytes", size)
	return dest, nil
}

// dirSize sums file sizes under dir (an approximation of its disk usage,
// for the log line above; errors are swallowed since this is informational).
func dirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// imageRootfsDir exports image's filesystem once into <dir>/rootfs/<digest>
// and returns that directory, for use as an actor's runsc overlay
// lowerdir (stage 8: locally, the actor rootfs stops being the host
// filesystem once a template names an image). Exporting is keyed by the
// image's own content digest, so a second actor (or a second `ate-ish
// dev` run) naming the same image reuses the same export instead of
// re-pulling or re-exporting it.
//
// Two export paths, same output shape (a plain directory holding the
// image's flattened filesystem): `docker create` + `docker export` when a
// docker daemon is reachable (the common case on a dev box, and what the
// brief asks for first); otherwise go-containerregistry's crane library
// (already a direct dependency; vendored packages transitively use it),
// which needs no daemon at all — the path the published ate-ish base
// image itself has to use, since `docker run --privileged` gives it no
// docker-in-docker.
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
// `docker inspect` first (works for an image only present locally, e.g.
// one a plain `docker build` just produced, which crane's registry-only
// Pull cannot see at all); crane.Digest as the fallback, which needs no
// daemon.
func imageDigest(image string) (digest string, dockerAvailable bool) {
	// ATE_ROOTFS_EXPORT=crane forces the no-daemon path even when docker
	// itself answers (e.g. a wedged dockerd whose metadata calls still
	// work but whose `docker run`/cgroup path does not — see
	// docs/PROGRESS.md's stage 8 cgroup-helper incident notes): useful to
	// exercise, and gate, the path the published ate-ish image actually
	// has to use.
	if env("ATE_ROOTFS_EXPORT", "") != "crane" {
		if out, err := exec.Command("docker", "inspect", "--format", "{{.Id}}", image).Output(); err == nil {
			return strings.TrimSpace(string(out)), true
		}
	}
	if d, err := crane.Digest(image); err == nil {
		return d, false
	}
	// Neither resolved a digest (no docker, and crane could not reach the
	// registry either, e.g. the demo's own locally-built, unpushed image
	// name): fall back to the image reference string itself as the cache
	// key. Still correct, just not shared across a rebuild under the same
	// tag; exportImageDocker/exportImageCrane's own errors catch a truly
	// missing image either way.
	useDocker := env("ATE_ROOTFS_EXPORT", "") != "crane" && exec.Command("docker", "image", "inspect", image).Run() == nil
	return image, useDocker
}

func sanitizeDigest(d string) string {
	return strings.NewReplacer(":", "-", "/", "_").Replace(d)
}

// exportImageDocker is imageRootfsDir's docker path: create a (never
// started) container from image, export its filesystem as a tar stream,
// and extract it into dest, exactly as rootfsLowerDir's node-copy path
// already extracts its own tar stream.
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

// exportImageCrane is imageRootfsDir's no-daemon path: pull the image with
// go-containerregistry and stream crane.Export's tar straight into tar
// -x, the same pipe shape exportImageDocker uses.
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

// mountBundleRootfs builds containerName's OCI bundle rootfs as a runsc
// overlay of lowerDir (the node's own filesystem, or a copy of it — see
// rootfsLowerDir — v1 ran actors from the node image the same way):
// lowerdir read-only, upperdir/workdir on a fresh tmpfs (overlayfs refuses
// an upperdir nested under its own lowerdir, which a plain subdirectory of
// lowerDir always is when lowerDir is "/" on this single-filesystem VM —
// the worker needs a writable upper regardless of how little the node
// image itself needs to change, since setupAppRootfs rewrites
// /etc/resolv.conf and /etc/hosts inside it on every boot).
//
// The mounts must land in the worker subprocess's own mount namespace, not
// this process's: ateworker-gvisor runs in a private mount namespace (see
// startWorker, for cgroup.Delegate's sake), and mounts this process makes
// are invisible there. Two things ruled out first: targeting the mount(2)
// call's target through the /proc/<pid>/root magic symlink (the trick that
// works for nsdial.go's *network* namespace, via a plain open() of a path
// under it) does not move mount(2)'s attach point — measured, the new mount
// still lands in the caller's own namespace. Calling setns(2) in-process
// does not work either: the kernel refuses CLONE_NEWNS for any thread of a
// multithreaded process (every Go binary, including this one), returning
// EINVAL — also measured. What does work, and is what this uses: `nsenter`
// as a short-lived external process, which is single-threaded at the
// moment it calls setns(2).
func mountBundleRootfs(workerPID int, d *actorDirs, containerName, lowerDir string) error {
	bundle := filepath.Join(d.pb.GetOciBundleDir(), containerName)
	rootfs, ovl := filepath.Join(bundle, "rootfs"), filepath.Join(bundle, "ovl")
	for _, dir := range []string{rootfs, ovl} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	script := fmt.Sprintf(
		"set -e; mount -t tmpfs tmpfs %[1]q; mkdir -p %[1]s/upper %[1]s/work; "+
			"mount -t overlay overlay -o lowerdir=%[3]s,upperdir=%[1]s/upper,workdir=%[1]s/work %[2]q",
		ovl, rootfs, lowerDir)
	if out, err := nsenterSh(workerPID, script).CombinedOutput(); err != nil {
		nsenterSh(workerPID, fmt.Sprintf("umount -l %q 2>/dev/null; umount -l %q 2>/dev/null", rootfs, ovl)).Run()
		return fmt.Errorf("mounting overlay rootfs at %s: %w (%s)", rootfs, err, out)
	}
	return nil
}

func unmountBundleRootfs(workerPID int, d *actorDirs, containerName string) error {
	bundle := filepath.Join(d.pb.GetOciBundleDir(), containerName)
	rootfs, ovl := filepath.Join(bundle, "rootfs"), filepath.Join(bundle, "ovl")
	script := fmt.Sprintf("umount -l %q; umount -l %q", rootfs, ovl)
	out, err := nsenterSh(workerPID, script).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "not mounted") {
		return fmt.Errorf("unmounting %s: %w (%s)", rootfs, err, out)
	}
	return nil
}

// nsenterSh runs script as `sh -c` inside the worker subprocess's mount
// namespace.
func nsenterSh(workerPID int, script string) *exec.Cmd {
	return exec.Command("nsenter", fmt.Sprintf("--mount=/proc/%d/ns/mnt", workerPID), "--", "sh", "-c", script)
}
