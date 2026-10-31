package atelet

import (
	"archive/tar"
	"bufio"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// runscVersion is the pinned gVisor release this file fetches.
const runscVersion = "release-20261019.0"

// runscAssetArch is the gVisor release asset's own architecture name for
// runtime.GOARCH, matching the two asset names google/gvisor publishes
// (gvisor-x86_64.tar.zstd, gvisor-aarch64.tar.zstd).
func runscAssetArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	default:
		return "", fmt.Errorf("no gVisor release asset for GOARCH=%s", runtime.GOARCH)
	}
}

// EnsureRunsc stages a pinned gVisor release (runsc plus its gvisor-bin/
// sidecars) at runscReleaseDir(), the layout ateworker-gvisor demanded in
// stage 0 (runsc looks for gvisor_sentry and friends next to itself), if
// one is not already there. Fetches over plain HTTPS, verifies against
// the release's own SHA512SUMS asset, and extracts with archive/tar and
// github.com/klauspost/compress/zstd — no gh CLI, no shell tar, so a bare
// `ate-ish` run needs no checkout of this repo and no gh login to get one.
func EnsureRunsc() error {
	dest := runscReleaseDir()
	if out, err := exec.Command(filepath.Join(dest, "runsc"), "--version").CombinedOutput(); err == nil && strings.Contains(string(out), runscVersion) {
		return nil
	}
	arch, err := runscAssetArch()
	if err != nil {
		return err
	}
	asset := "gvisor-" + arch + ".tar.zstd"
	base := "https://github.com/google/gvisor/releases/download/" + runscVersion + "/"

	work, err := os.MkdirTemp("", "ate-ish-runsc-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	fmt.Println("== fetching runsc", runscVersion, asset, "==")
	tarball := filepath.Join(work, asset)
	if err := fetchFile(base+asset, tarball); err != nil {
		return fmt.Errorf("fetching %s: %w", asset, err)
	}
	wantSHA512, err := fetchExpectedSHA512(base+"SHA512SUMS", asset)
	if err != nil {
		return fmt.Errorf("fetching SHA512SUMS: %w", err)
	}
	if err := verifySHA512(tarball, wantSHA512); err != nil {
		return fmt.Errorf("verifying %s: %w", asset, err)
	}

	staged := filepath.Join(work, "staged")
	if err := extractZstdTar(tarball, staged); err != nil {
		return fmt.Errorf("extracting %s: %w", asset, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(staged, dest); err != nil {
		return fmt.Errorf("installing runsc at %s: %w", dest, err)
	}
	out, err := exec.Command(filepath.Join(dest, "runsc"), "--version").CombinedOutput()
	fmt.Print(string(out))
	return err
}

// fetchFile GETs url and writes its body to dest, failing on any
// non-2xx status.
func fetchFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// fetchExpectedSHA512 GETs url (the release's SHA512SUMS asset, the
// standard `sha512sum` output format: "<hex>  <filename>" per line) and
// returns the hex digest it lists for asset.
func fetchExpectedSHA512(url, asset string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s not listed in SHA512SUMS", asset)
}

func verifySHA512(path, wantHex string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantHex) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, wantHex)
	}
	return nil
}

// extractZstdTar decompresses and extracts tarball (a .tar.zstd) into
// dest, preserving each entry's mode, including the gvisor-bin/ sidecar
// symlinks the release layout uses.
func extractZstdTar(tarball, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.Clean("/"+hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
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
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}
