package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const sandboxBin = "/usr/local/gcp/bin/sandbox"

// sandbox runs the launcher CLI. Its output goes to a buffer, never to a
// shared socket: `sandbox exec` closes its stdout and would kill the parent.
func sandbox(ctx context.Context, args ...string) error {
	start := time.Now()
	var out bytes.Buffer
	c := exec.CommandContext(ctx, sandboxBin, args...)
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	slog.Info("sandbox", "op", args[0], "id", args[1], "ms", time.Since(start).Milliseconds(), "err", err)
	if err != nil {
		return fmt.Errorf("sandbox %s %s: %w: %s", args[0], args[1], err, strings.TrimSpace(out.String()))
	}
	return nil
}

type image struct {
	rootfs string
	config v1.Config
}

// images unpacks each template image once into a directory for --rootfs.
type images struct {
	dir      string
	mu       sync.Mutex
	unpacked map[string]image
}

func (im *images) get(ctx context.Context, ref string) (image, error) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if img, ok := im.unpacked[ref]; ok {
		return img, nil
	}
	start := time.Now()
	r, err := name.ParseReference(ref)
	if err != nil {
		return image{}, err
	}
	remoteImg, err := remote.Image(r, remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.NewMultiKeychain(google.Keychain, authn.DefaultKeychain)),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))
	if err != nil {
		return image{}, fmt.Errorf("fetching %s: %w", ref, err)
	}
	digest, err := remoteImg.Digest()
	if err != nil {
		return image{}, err
	}
	cf, err := remoteImg.ConfigFile()
	if err != nil {
		return image{}, err
	}
	img := image{rootfs: filepath.Join(im.dir, digest.Hex), config: cf.Config}
	if err := os.RemoveAll(img.rootfs); err != nil {
		return image{}, err
	}
	if err := os.MkdirAll(img.rootfs, 0o755); err != nil {
		return image{}, err
	}
	// mutate.Extract flattens the layers and applies whiteouts, as crane export
	// does. GNU tar keeps owners, modes and links.
	flat := mutate.Extract(remoteImg)
	defer flat.Close()
	untar := exec.CommandContext(ctx, "tar", "-x", "-p", "--numeric-owner", "-C", img.rootfs)
	untar.Stdin = flat
	if out, err := untar.CombinedOutput(); err != nil {
		return image{}, fmt.Errorf("unpacking %s: %w: %s", ref, err, out)
	}
	slog.Info("unpacked image", "ref", ref, "ms", time.Since(start).Milliseconds())
	im.unpacked[ref] = img
	return img, nil
}

// Paths inside the sandbox. The ate binary is mounted at initDir and the
// actor's host directory at actorDir.
const (
	initDir  = "/.ate"
	actorDir = "/.ate-actor"
)

// initSpec reaches init through a 0600 file in the actor directory, because
// the launcher logs every `sandbox run` command line, -e values included.
type initSpec struct {
	Command []string `json:"command"`
	Env     []string `json:"env"`
	Dirs    []string `json:"dirs"`
	Workdir string   `json:"workdir"`
}

// runInit is pid 1 in the sandbox. It starts the actor, forwards SIGTERM,
// writes the exit code, and stays alive so the sandbox can still be tarred.
func runInit() error {
	specFile := filepath.Join(actorDir, "spec.json")
	data, err := os.ReadFile(specFile)
	if err != nil {
		return err
	}
	if err := os.Remove(specFile); err != nil {
		return err
	}
	var spec initSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return err
	}
	for _, d := range spec.Dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	log, err := os.OpenFile(filepath.Join(actorDir, "log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	os.Clearenv()
	for _, kv := range spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		os.Setenv(k, v)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	child := exec.Command(spec.Command[0], spec.Command[1:]...)
	child.Dir = spec.Workdir
	child.Stdout, child.Stderr = log, log
	if err := child.Start(); err != nil {
		fmt.Fprintf(log, "ate init: %v\n", err)
		return writeExit(-1)
	}
	go func() {
		for s := range signals {
			child.Process.Signal(s)
		}
	}()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			select {} // no children left; wait for `sandbox delete`
		}
		if pid == child.Process.Pid {
			if err := writeExit(ws.ExitStatus()); err != nil {
				return err
			}
		}
	}
}

func writeExit(code int) error {
	return os.WriteFile(filepath.Join(actorDir, "exit"), []byte(fmt.Sprintln(code)), 0o644)
}
