// ate-ish deploy is the one-command hook: from a directory holding an
// actor's own Dockerfile, template.yaml and (optionally) egress-policy.yaml,
// it builds an image with ate-ish added, pushes it, creates the bucket and
// the Cloud Run gen2 instance if missing, uploads the template and egress
// policy, creates one actor, and prints the URL and a ready curl line.
//
// It shells out to docker and gcloud throughout (same approach as
// deploy.sh), rather than linking their SDKs, to keep the binary small and
// the commands it runs visible and reproducible by hand.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

const deployUsage = `usage: ate-ish deploy -f template.yaml [-e egress-policy.yaml] [flags]
  run from a directory holding the actor's own Dockerfile
  -f, --file FILE        actor-template YAML (required)
  -e, --egress FILE      egress-policy YAML
      --project P        GCP project (default: gcloud config get project)
      --region R         Cloud Run region (default: us-west1)
      --name N           Cloud Run instance name (default: ate-ish)
      --base IMAGE       published ate-ish base image to add to the actor image
                         (default: us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest)
      --no-actor         upload the template but do not create an actor
      --down             delete the instance (keeps the bucket) and exit`

// deployConfig holds one deploy run's resolved flags.
type deployConfig struct {
	templateFile string
	egressFile   string
	project      string
	region       string
	name         string
	base         string
	noActor      bool
	down         bool
}

func runDeploy(args []string) error {
	fs := pflag.NewFlagSet("ate-ish deploy", pflag.ContinueOnError)
	cfg := deployConfig{}
	fs.StringVarP(&cfg.templateFile, "file", "f", "", "actor-template YAML")
	fs.StringVarP(&cfg.egressFile, "egress", "e", "", "egress-policy YAML")
	fs.StringVar(&cfg.project, "project", "", "GCP project")
	fs.StringVar(&cfg.region, "region", "us-west1", "Cloud Run region")
	fs.StringVar(&cfg.name, "name", "ate-ish", "Cloud Run instance name")
	fs.StringVar(&cfg.base, "base", "us-central1-docker.pkg.dev/daniel-junk/ate-ish/ate-ish:latest", "published ate-ish base image")
	fs.BoolVar(&cfg.noActor, "no-actor", false, "upload the template but do not create an actor")
	fs.BoolVar(&cfg.down, "down", false, "delete the instance (keeps the bucket) and exit")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, deployUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	if cfg.project == "" {
		out, err := exec.Command("gcloud", "config", "get", "project").Output()
		if err != nil {
			return fmt.Errorf("gcloud config get project: %w (pass --project)", err)
		}
		cfg.project = strings.TrimSpace(string(out))
	}

	if cfg.down {
		return sh("gcloud", "beta", "run", "instances", "delete", cfg.name,
			"--project", cfg.project, "--region", cfg.region, "--quiet")
	}

	if cfg.templateFile == "" {
		fs.Usage()
		return fmt.Errorf("-f/--file is required")
	}
	tmplBody, err := yamlFile(cfg.templateFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", cfg.templateFile, err)
	}
	var tmpl Template
	if err := json.Unmarshal(tmplBody, &tmpl); err != nil {
		return fmt.Errorf("parsing %s: %w", cfg.templateFile, err)
	}
	if tmpl.Metadata.Atespace == "" || tmpl.Metadata.Name == "" {
		return fmt.Errorf("%s: metadata.atespace and metadata.name are required", cfg.templateFile)
	}
	if len(tmpl.Containers) != 1 {
		return fmt.Errorf("%s: needs exactly one container", cfg.templateFile)
	}

	var egressBody []byte
	var egress EgressPolicy
	if cfg.egressFile != "" {
		if egressBody, err = yamlFile(cfg.egressFile); err != nil {
			return fmt.Errorf("reading %s: %w", cfg.egressFile, err)
		}
		if err := json.Unmarshal(egressBody, &egress); err != nil {
			return fmt.Errorf("parsing %s: %w", cfg.egressFile, err)
		}
	}

	fmt.Printf("== building the actor image (base %s) ==\n", cfg.base)
	image, userCommand, userArgs, err := buildAndPushActorImage(cfg)
	if err != nil {
		return err
	}

	// ate-ish does not read the image's CMD/ENTRYPOINT at run time
	// (records.go's Container has its own command field); a template that
	// leaves command to the image, as the upstream demos' templates do,
	// needs it filled in here instead, from the image this command just
	// built, so the template file itself does not have to change.
	if tmpl.Containers[0].Command == nil {
		if len(userCommand) == 0 {
			return fmt.Errorf("the image has no ENTRYPOINT/CMD to infer a command from; set containers[0].command in %s", cfg.templateFile)
		}
		tmplBody, err = setContainerCommand(tmplBody, userCommand, userArgs)
		if err != nil {
			return err
		}
		fmt.Printf("  no command in the template; inferred %q from the image's own ENTRYPOINT/CMD\n", append(userCommand, userArgs...))
	}

	fmt.Println("== bucket and IAM ==")
	bucketName := "ate-ish-" + cfg.project
	sa, err := computeServiceAccount(cfg.project)
	if err != nil {
		return err
	}
	if err := ensureBucket(cfg.project, cfg.region, bucketName, sa); err != nil {
		return err
	}
	if err := grantSecrets(cfg.project, sa, egress); err != nil {
		return err
	}

	fmt.Println("== Cloud Run gen2 instance ==")
	if err := ensureInstance(cfg, image, bucketName); err != nil {
		return err
	}
	url, err := instanceURL(cfg)
	if err != nil {
		return err
	}

	fmt.Println("== atespace, template, actor ==")
	if err := os.Setenv("ATE_URL", url); err != nil {
		return err
	}
	if err := waitInstanceReady(url); err != nil {
		return err
	}
	if err := idempotent(call("PUT", "/admin/atespaces/"+tmpl.Metadata.Atespace, nil)); err != nil {
		return err
	}
	if err := idempotent(call("PUT", "/admin/atespaces/"+tmpl.Metadata.Atespace+"/templates/"+tmpl.Metadata.Name, tmplBody)); err != nil {
		return err
	}
	as := "/admin/atespaces/" + tmpl.Metadata.Atespace
	actorName := tmpl.Metadata.Name
	if !cfg.noActor {
		if err := idempotent(call("PUT", as+"/actors/"+actorName,
			map[string]string{"template": tmpl.Metadata.Name})); err != nil {
			return err
		}
		if cfg.egressFile != "" {
			if err := idempotent(call("PUT", as+"/actors/"+actorName+"/egress-policy", egressBody)); err != nil {
				return err
			}
		}
	}

	fmt.Println()
	fmt.Printf("URL: %s\n", url)
	fmt.Printf("export ATE_URL=%s PATH=%s/bin:$PATH\n", url, mustGetwd())
	if !cfg.noActor {
		fmt.Printf("curl -sS -H \"Authorization: Bearer $(gcloud auth print-identity-token)\" \\\n"+
			"  -H \"ate-target-actor: %s/%s\" %s/submit -H 'Content-Type: application/json' \\\n"+
			"  -d '{\"prompt\": \"hello\"}'\n", tmpl.Metadata.Atespace, actorName, url)
	}
	return nil
}

// waitReady polls the admin API until the instance answers, instead of
// racing it: a fresh gen2 instance spends 40-50s copying the node
// filesystem onto its rootfs volume before the worker is up (see
// README's Limits), so the first request after create/update fails with a
// 500 if sent immediately.
func waitInstanceReady(url string) error {
	fmt.Println("waiting for the instance to finish booting...")
	deadline := time.Now().Add(2 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := call("GET", "/admin/workers", nil); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("instance at %s did not become ready: %w", url, lastErr)
}

// idempotent treats "already exists" (409, from a prior deploy of the same
// template/actor/atespace) as success, so a rerun of `ate-ish deploy`
// converges instead of failing on the second run.
func idempotent(_ []byte, err error) error {
	if err != nil && strings.Contains(err.Error(), "409") {
		return nil
	}
	return err
}

// buildAndPushActorImage builds the user's own Dockerfile in the current
// directory, then a second image FROM that result with ate-ish,
// ateworker-gvisor, runsc, kodata and the apt packages the worker needs
// added on top (the exact list from the base ate-ish image's own
// Dockerfile), and pushes it to an Artifact Registry repo named "ate-ish"
// in the project (created if missing). It also returns the user's own
// image's effective command (ENTRYPOINT, or CMD if there is no
// ENTRYPOINT) and args (CMD, when ENTRYPOINT is set), read with `docker
// inspect` before the wrapper stage's own ENTRYPOINT overwrites it — the
// command a template that leaves `command` to the image actually wants.
func buildAndPushActorImage(cfg deployConfig) (image string, command, args []string, err error) {
	if _, err := os.Stat("Dockerfile"); err != nil {
		return "", nil, nil, fmt.Errorf("no Dockerfile in the current directory: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, nil, err
	}
	userTag := "ate-ish-deploy/" + cfg.name + "-user:latest"
	if err := sh("docker", "build", "--platform", "linux/amd64", "-t", userTag, cwd); err != nil {
		return "", nil, nil, fmt.Errorf("building the user's own Dockerfile: %w", err)
	}
	command, args, err = imageCommand(userTag)
	if err != nil {
		return "", nil, nil, err
	}

	repoHost := cfg.region + "-docker.pkg.dev"
	repo := repoHost + "/" + cfg.project + "/ate-ish"
	if err := ensureArtifactRegistryRepo(cfg.project, cfg.region); err != nil {
		return "", nil, nil, err
	}
	finalImage := repo + "/" + cfg.name + ":latest"

	wrapperDir, err := os.MkdirTemp("", "ate-ish-deploy-")
	if err != nil {
		return "", nil, nil, err
	}
	defer os.RemoveAll(wrapperDir)
	wrapper := fmt.Sprintf(`FROM %s
# The apt packages the vendored worker needs (atelet.go, bundle.go, and the
# vendored actornet package exec these directly): same list as the base
# ate-ish image's own Dockerfile.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates nftables iproute2 util-linux kmod python3 curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=%s /usr/local/bin/ate-ish /usr/local/bin/ate-ish
COPY --from=%s /ate-ish/bin/ateworker-gvisor /ate-ish/bin/ateworker-gvisor
COPY --from=%s /ate-ish/bin/runsc /ate-ish/bin/runsc
COPY --from=%s /ate-ish/third_party/substrate/cmd/ateworker-gvisor/kodata /ate-ish/third_party/substrate/cmd/ateworker-gvisor/kodata
WORKDIR /ate-ish
ENTRYPOINT ["/usr/local/bin/ate-ish", "serve"]
`, userTag, cfg.base, cfg.base, cfg.base, cfg.base)
	dockerfilePath := filepath.Join(wrapperDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(wrapper), 0o644); err != nil {
		return "", nil, nil, err
	}
	if err := sh("docker", "build", "--platform", "linux/amd64", "-f", dockerfilePath, "-t", finalImage, wrapperDir); err != nil {
		return "", nil, nil, fmt.Errorf("adding ate-ish to the user's image: %w", err)
	}
	if err := sh("docker", "push", finalImage); err != nil {
		return "", nil, nil, fmt.Errorf("pushing %s: %w", finalImage, err)
	}
	return finalImage, command, args, nil
}

// imageCommand reads an image's effective command with `docker inspect`:
// its ENTRYPOINT if it has one (with CMD as that entrypoint's args), or
// its CMD alone otherwise.
func imageCommand(tag string) (command, args []string, err error) {
	out, err := exec.Command("docker", "inspect", "--format",
		`{{json .Config.Entrypoint}}|{{json .Config.Cmd}}`, tag).Output()
	if err != nil {
		return nil, nil, fmt.Errorf("docker inspect %s: %w", tag, err)
	}
	entrypointJSON, cmdJSON, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	var entrypoint, cmd []string
	_ = json.Unmarshal([]byte(entrypointJSON), &entrypoint)
	_ = json.Unmarshal([]byte(cmdJSON), &cmd)
	if len(entrypoint) > 0 {
		return entrypoint, cmd, nil
	}
	return cmd, nil, nil
}

// setContainerCommand fills in containers[0].command (and .args, if given)
// on the raw template JSON, leaving every other field (including ones
// records.go's own Template/Container types do not model, like
// workerSelector, resources and snapshotConfig) exactly as the file had
// it.
func setContainerCommand(tmplBody []byte, command, args []string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(tmplBody, &raw); err != nil {
		return nil, err
	}
	containers, _ := raw["containers"].([]any)
	if len(containers) != 1 {
		return nil, fmt.Errorf("template needs exactly one container")
	}
	c, _ := containers[0].(map[string]any)
	c["command"] = command
	if len(args) > 0 {
		c["args"] = args
	}
	return json.Marshal(raw)
}

func ensureArtifactRegistryRepo(project, region string) error {
	check := exec.Command("gcloud", "artifacts", "repositories", "describe", "ate-ish",
		"--project", project, "--location", region)
	if check.Run() == nil {
		return nil
	}
	return sh("gcloud", "artifacts", "repositories", "create", "ate-ish",
		"--project", project, "--location", region, "--repository-format", "docker", "--quiet")
}

func computeServiceAccount(project string) (string, error) {
	out, err := exec.Command("gcloud", "iam", "service-accounts", "list",
		"--project", project, "--filter", "email~-compute@developer", "--format", "value(email)").Output()
	if err != nil {
		return "", fmt.Errorf("gcloud iam service-accounts list: %w", err)
	}
	sa := strings.TrimSpace(string(out))
	if sa == "" {
		return "", fmt.Errorf("no default compute service account found in project %s", project)
	}
	return sa, nil
}

func ensureBucket(project, region, name, sa string) error {
	check := exec.Command("gcloud", "storage", "buckets", "describe", "gs://"+name, "--project", project)
	if check.Run() != nil {
		if err := sh("gcloud", "storage", "buckets", "create", "gs://"+name,
			"--project", project, "--location", region, "--uniform-bucket-level-access"); err != nil {
			return err
		}
	}
	return sh("gcloud", "storage", "buckets", "add-iam-policy-binding", "gs://"+name,
		"--member", "serviceAccount:"+sa, "--role", "roles/storage.objectAdmin")
}

// grantSecrets grants the compute service account secretAccessor on every
// Secret Manager secret the egress policy's credentialUris name, in the
// native ate-secret://secretmanager.googleapis.com/<project>/<secret> form
// (the only form egress.go's own secrets.get resolves). A credentialUri in
// another form, such as a demo's ate-secret://k8s.io/... one, is not
// something this command can map to a real secret name on its own; it is
// named instead, with the ATE_SECRET_MAP line that would resolve it, so
// rewriting the policy to the native form (or setting ATE_SECRET_MAP by
// hand) is the user's call.
func grantSecrets(project, sa string, egress EgressPolicy) error {
	const nativePrefix = "ate-secret://secretmanager.googleapis.com/"
	for _, rule := range egress.Rules {
		for _, rh := range rule.HTTPS.Effects.ReplaceHeaders {
			uri := rh.CredentialURI
			if uri == "" {
				continue
			}
			rest, ok := strings.CutPrefix(uri, nativePrefix)
			if !ok {
				fmt.Printf("  skipping IAM grant for non-native credentialUri %q: egress.go only resolves the %s form at request time.\n", uri, nativePrefix)
				fmt.Printf("  either rewrite the policy's credentialUri to that form, or set ATE_SECRET_MAP=\"%s=<project>/<secret>\" on the instance yourself.\n", uri)
				continue
			}
			secretProject, secret, ok := strings.Cut(rest, "/")
			if !ok || secretProject == "" || secret == "" {
				fmt.Printf("  skipping malformed credentialUri %q\n", uri)
				continue
			}
			if err := sh("gcloud", "secrets", "add-iam-policy-binding", secret,
				"--project", secretProject, "--member", "serviceAccount:"+sa,
				"--role", "roles/secretmanager.secretAccessor"); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureInstance creates the Cloud Run gen2 instance if missing, or updates
// its image and env vars if it already exists, with the same flags
// deploy.sh's play case uses (gen2: no --sandbox-launcher, an in-memory
// rootfs volume since ephemeral-disk is unsupported, role all).
func ensureInstance(cfg deployConfig, image, bucket string) error {
	sa, err := computeServiceAccount(cfg.project)
	if err != nil {
		return err
	}
	env := "ATE_ROLE=all,ATE_BUCKET=" + bucket + ",ATE_CAPACITY=4"
	check := exec.Command("gcloud", "beta", "run", "instances", "describe", cfg.name,
		"--project", cfg.project, "--region", cfg.region)
	if check.Run() == nil {
		return sh("gcloud", "beta", "run", "instances", "update", cfg.name,
			"--project", cfg.project, "--region", cfg.region, "--quiet",
			"--image", image, "--update-env-vars", env)
	}
	return sh("gcloud", "beta", "run", "instances", "create", cfg.name,
		"--project", cfg.project, "--region", cfg.region, "--quiet",
		"--image", image, "--service-account", sa,
		"--cpu", "2", "--memory", "4Gi", "--port", "8080",
		"--add-volume", "name=rootfs,type=in-memory,size-limit=2Gi,mount-path=/var/lib/ate-ish/rootfs",
		"--set-env-vars", env)
}

func instanceURL(cfg deployConfig) (string, error) {
	out, err := exec.Command("gcloud", "beta", "run", "instances", "describe", cfg.name,
		"--project", cfg.project, "--region", cfg.region, "--format", "value(status.urls[0])").Output()
	if err != nil {
		return "", fmt.Errorf("gcloud beta run instances describe %s: %w", cfg.name, err)
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("instance %s has no URL yet", cfg.name)
	}
	return url, nil
}

// sh runs a command with its own stdout/stderr, printing the command first
// (deploy.sh's own convention), so a user can see and rerun every step by
// hand.
func sh(name string, args ...string) error {
	fmt.Println("$", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}
