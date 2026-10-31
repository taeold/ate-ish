// ate-ish dev is the primary, laptop-facing target (docs/WHAT.md): one
// node, one actor, in the foreground, from a directory holding the
// actor's own Dockerfile (if any) and the template/egress-policy it
// names. Builds on the same bootstrap deploy.go already does (build an
// image, infer its command, upload the template, create the actor), but
// against a local directory store and a local node instead of Cloud Run.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/pflag"
)

const devUsage = `usage: ate-ish dev [-f template.yaml] [-e egress-policy.yaml] [--dir DIR] [--clean] [--port PORT]
  run from a directory holding the actor's own Dockerfile (if any) and
  the template/egress-policy files it names
  -f, --file FILE      actor-template YAML (default: template.yaml)
  -e, --egress FILE    egress-policy YAML
      --dir DIR        the dev node's data dir (default: ./.ate-ish/dev)
      --clean          delete --dir first
      --port PORT      ingress/worker port (default: 8080)

Secrets an egress policy's credentialUri names: either
ate-secret://secretmanager.googleapis.com/<project>/<secret> (read through
gcloud's Application Default Credentials, if set up), or
ate-secret://env/<VAR> (read from this process's own environment, no GCP
project needed at all).`

type devConfig struct {
	templateFile, egressFile, dir, port string
	clean                               bool
}

func runDev(args []string) error {
	fs := pflag.NewFlagSet("ate-ish dev", pflag.ContinueOnError)
	cfg := devConfig{}
	fs.StringVarP(&cfg.templateFile, "file", "f", "template.yaml", "actor-template YAML")
	fs.StringVarP(&cfg.egressFile, "egress", "e", "", "egress-policy YAML")
	fs.StringVar(&cfg.dir, "dir", "./.ate-ish/dev", "the dev node's data dir")
	fs.BoolVar(&cfg.clean, "clean", false, "delete --dir first")
	fs.StringVar(&cfg.port, "port", "8080", "ingress/worker port")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, devUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("ate-ish dev runs the vendored gVisor worker, which needs root: run with sudo, or inside a --privileged container")
	}

	origCwd, err := os.Getwd()
	if err != nil {
		return err
	}
	templateFile := absPath(origCwd, cfg.templateFile)
	egressFile := absPath(origCwd, cfg.egressFile)
	devDir := absPath(origCwd, cfg.dir)

	if cfg.clean {
		fmt.Println("== --clean: removing", devDir, "==")
		if err := os.RemoveAll(devDir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		return err
	}

	setAteIshHome()
	runscDir := env("ATE_RUNSC_DIR", filepath.Join(devDir, "bin", "runsc"))
	os.Setenv("ATE_RUNSC_DIR", runscDir)
	if err := fetchRunsc(runscDir); err != nil {
		return fmt.Errorf("fetching runsc: %w", err)
	}

	tmplBody, err := yamlFile(templateFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", templateFile, err)
	}
	var tmpl Template
	if err := json.Unmarshal(tmplBody, &tmpl); err != nil {
		return fmt.Errorf("parsing %s: %w", templateFile, err)
	}
	if tmpl.Metadata.Atespace == "" || tmpl.Metadata.Name == "" {
		return fmt.Errorf("%s: metadata.atespace and metadata.name are required", templateFile)
	}
	if len(tmpl.Containers) != 1 {
		return fmt.Errorf("%s: needs exactly one container", templateFile)
	}

	// A Dockerfile in the current directory is built fresh every run and
	// the template is pointed at that build (the same "no edits needed"
	// convenience as ate-ish deploy): a demo's own template.yaml keeps
	// whatever image reference it was written with. Needs a reachable
	// docker daemon; see dev.go's own notes and docs/PROGRESS.md for the
	// no-daemon case (the published ate-ish image run with no docker
	// socket available cannot build a Dockerfile this way at all).
	if dockerfile := filepath.Join(origCwd, "Dockerfile"); fileExists(dockerfile) {
		fmt.Println("== building", dockerfile, "==")
		imageTag := "ate-ish-dev/" + tmpl.Metadata.Name + ":latest"
		if err := sh("docker", "build", "--platform", "linux/amd64", "-t", imageTag, origCwd); err != nil {
			return fmt.Errorf("building %s: %w (ate-ish dev needs a reachable docker daemon to build an actor's "+
				"own Dockerfile; with none reachable, point containers[0].image in the template at an "+
				"already-built, pullable image instead)", dockerfile, err)
		}
		command, cmdArgs, err := imageCommand(imageTag)
		if err != nil {
			return err
		}
		// ATE_ROOTFS_EXPORT=crane (bundle.go's imageDigest/imageRootfsDir)
		// forces the no-daemon rootfs export path, which pulls from a
		// registry, not the local docker image store a plain `docker
		// build` leaves this in — so push it there first. Exercises and
		// gates the exact path the published, daemon-less ate-ish image
		// has to use in production.
		if env("ATE_ROOTFS_EXPORT", "") == "crane" {
			pushed := env("ATE_ROOTFS_EXPORT_REPO", "us-central1-docker.pkg.dev/daniel-junk/ate-ish") + "/gate-stage8-" + tmpl.Metadata.Name + ":latest"
			if err := sh("docker", "tag", imageTag, pushed); err != nil {
				return err
			}
			if err := sh("docker", "push", pushed); err != nil {
				return err
			}
			imageTag = pushed
		}
		if tmplBody, err = setContainerImage(tmplBody, imageTag); err != nil {
			return err
		}
		tmpl.Containers[0].Image = imageTag
		if tmpl.Containers[0].Command == nil && len(command) > 0 {
			if tmplBody, err = setContainerCommand(tmplBody, command, cmdArgs); err != nil {
				return err
			}
			fmt.Printf("  no command in the template; inferred %q from the image's own ENTRYPOINT/CMD\n", append(command, cmdArgs...))
		}
	}

	var egressBody []byte
	if egressFile != "" {
		if egressBody, err = yamlFile(egressFile); err != nil {
			return fmt.Errorf("reading %s: %w", egressFile, err)
		}
	}

	fmt.Println("== starting the dev node ==")
	st, err := newDirStore(filepath.Join(devDir, "store"))
	if err != nil {
		return err
	}
	ctx := context.Background()
	n, err := startNode(ctx, "all", cfg.port, st, filepath.Join(devDir, "node"), 2)
	if err != nil {
		return err
	}
	go func() {
		if err := n.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("dev node", "err", err)
		}
	}()

	url := "http://127.0.0.1:" + cfg.port
	os.Setenv("ATE_URL", url)
	if err := waitInstanceReady(url); err != nil {
		return err
	}
	as := "/admin/atespaces/" + tmpl.Metadata.Atespace
	if err := idempotent(call("PUT", "/admin/atespaces/"+tmpl.Metadata.Atespace, nil)); err != nil {
		return err
	}
	if err := idempotent(call("PUT", as+"/templates/"+tmpl.Metadata.Name, tmplBody)); err != nil {
		return err
	}
	actorName := tmpl.Metadata.Name
	if err := idempotent(call("PUT", as+"/actors/"+actorName, map[string]string{"template": actorName})); err != nil {
		return err
	}
	if egressFile != "" {
		if err := idempotent(call("PUT", as+"/actors/"+actorName+"/egress-policy", egressBody)); err != nil {
			return err
		}
	}

	fmt.Println()
	fmt.Printf("export ATE_URL=%s\n", url)
	fmt.Printf("curl -sS -H \"ate-target-actor: %s/%s\" %s/submit -H 'Content-Type: application/json' -d '{\"prompt\": \"hello\"}'\n",
		tmpl.Metadata.Atespace, actorName, url)
	fmt.Println()
	fmt.Println("ate-ish dev is running; Ctrl-C to stop (suspends the actor first).")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	fmt.Println("\nshutting down...")
	n.shutdown(ctx)
	return nil
}

// setAteIshHome sets ATE_ISH_HOME, which worker.go's ateIshHome() reads to
// find bin/ateworker-gvisor and the vendored kodata directory, unless it
// is already set (the published dev image sets it in its own Dockerfile).
// On a bare checkout, guessed from this very binary's own location:
// <repo>/bin/ate-ish implies home <repo>.
func setAteIshHome() {
	if os.Getenv("ATE_ISH_HOME") != "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	if filepath.Base(dir) == "bin" {
		dir = filepath.Dir(dir)
	}
	os.Setenv("ATE_ISH_HOME", dir)
}

func absPath(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// setContainerImage is setContainerCommand's own sibling: fills in
// containers[0].image on the raw template JSON, leaving every other
// field exactly as the file had it.
func setContainerImage(tmplBody []byte, image string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(tmplBody, &raw); err != nil {
		return nil, err
	}
	containers, _ := raw["containers"].([]any)
	if len(containers) != 1 {
		return nil, fmt.Errorf("template needs exactly one container")
	}
	c, _ := containers[0].(map[string]any)
	c["image"] = image
	return json.Marshal(raw)
}

// fetchRunsc stages a pinned gVisor release at dest (runsc plus its
// gvisor-bin/ sidecars), mirroring scripts/fetch-runsc.sh's own logic in
// Go so `ate-ish dev` needs no checkout of this repo to get one.
func fetchRunsc(dest string) error {
	const version = "release-20261019.0"
	if out, err := exec.Command(filepath.Join(dest, "runsc"), "--version").CombinedOutput(); err == nil && strings.Contains(string(out), version) {
		fmt.Printf("runsc %s already staged at %s\n", version, dest)
		return nil
	}
	work, err := os.MkdirTemp("", "ate-ish-runsc-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tarball := filepath.Join(work, "gvisor-x86_64.tar.zstd")
	fmt.Println("== fetching runsc", version, "==")
	if _, err := exec.LookPath("gh"); err == nil {
		if err := sh("gh", "release", "download", "-R", "google/gvisor", version, "-p", "gvisor-x86_64.tar.zstd", "-D", work); err != nil {
			return err
		}
	} else {
		url := "https://github.com/google/gvisor/releases/download/" + version + "/gvisor-x86_64.tar.zstd"
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
