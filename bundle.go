package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

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

// mountBundleRootfs builds containerName's OCI bundle rootfs as a runsc
// overlay of the node's own filesystem (v1 ran actors from the node image
// the same way): lowerdir "/" read-only, upperdir/workdir on a fresh tmpfs
// (overlayfs refuses an upperdir nested under its own lowerdir, which a
// plain subdirectory of "/" always is on this single-filesystem VM — the
// worker needs a writable upper regardless of how little the node image
// itself needs to change, since setupAppRootfs rewrites /etc/resolv.conf
// and /etc/hosts inside it on every boot).
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
func mountBundleRootfs(workerPID int, d *actorDirs, containerName string) error {
	bundle := filepath.Join(d.pb.GetOciBundleDir(), containerName)
	rootfs, ovl := filepath.Join(bundle, "rootfs"), filepath.Join(bundle, "ovl")
	for _, dir := range []string{rootfs, ovl} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	script := fmt.Sprintf(
		"set -e; mount -t tmpfs tmpfs %[1]q; mkdir -p %[1]s/upper %[1]s/work; "+
			"mount -t overlay overlay -o lowerdir=/,upperdir=%[1]s/upper,workdir=%[1]s/work %[2]q",
		ovl, rootfs)
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
