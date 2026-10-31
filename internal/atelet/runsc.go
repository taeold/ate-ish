package atelet

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runscVersion is the pinned gVisor release every path that stages runsc
// (this file, and hack/fetch-runsc.sh) fetches.
const runscVersion = "release-20261019.0"

// EnsureRunsc stages a pinned gVisor release (runsc plus its gvisor-bin/
// sidecars) at runscReleaseDir(), the layout ateworker-gvisor demanded in
// stage 0 (runsc looks for gvisor_sentry and friends next to itself), if
// one is not already there. Mirrors hack/fetch-runsc.sh's own logic in Go,
// so a bare `ate-ish` run needs no checkout of this repo to get one.
func EnsureRunsc() error {
	dest := runscReleaseDir()
	if out, err := exec.Command(filepath.Join(dest, "runsc"), "--version").CombinedOutput(); err == nil && strings.Contains(string(out), runscVersion) {
		return nil
	}
	work, err := os.MkdirTemp("", "ate-ish-runsc-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tarball := filepath.Join(work, "gvisor-x86_64.tar.zstd")
	fmt.Println("== fetching runsc", runscVersion, "==")
	if _, err := exec.LookPath("gh"); err == nil {
		if err := sh("gh", "release", "download", "-R", "google/gvisor", runscVersion, "-p", "gvisor-x86_64.tar.zstd", "-D", work); err != nil {
			return err
		}
	} else {
		url := "https://github.com/google/gvisor/releases/download/" + runscVersion + "/gvisor-x86_64.tar.zstd"
		if err := sh("curl", "-fsSL", "-o", tarball, url); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := sh("tar", "--zstd", "-xf", tarball, "-C", dest); err != nil {
		return err
	}
	return sh(filepath.Join(dest, "runsc"), "--version")
}

// sh runs a command with its own stdout/stderr, printing the command first,
// so it is visible and reproducible by hand.
func sh(name string, args ...string) error {
	fmt.Println("$", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
