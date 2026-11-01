package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EgressDestination is one (host, port) pair an actor's egress policy
// allows, with the loopback address inside the sandbox that the egress
// forwarder ate-ish's own init starts will listen on, and the host-side
// unix socket (built by internal/egress) it forwards to. Round 3's own
// design (docs/ROOTLESS-SPIKE.md): every destination gets its own
// address and socket, so a plain-HTTP or non-443 rule needs no SNI and
// no preamble to tell apart from the others.
type EgressDestination struct {
	ListenIP   string // e.g. "127.0.0.2", unique per destination
	Port       int
	SocketName string // the socket's file name under EgressSocketsDir, e.g. "2.sock"
	Hostname   string // for /etc/hosts
}

// ActorSpec is what the sandbox needs to build one actor's OCI bundle:
// everything records.Template/Container carries that matters at this
// layer, already translated out of ate-ish's own JSON shape by the
// caller (internal/atelet).
type ActorSpec struct {
	ID          string // runsc container id, the actor's UID
	Hostname    string
	Command     []string
	Env         []string
	RootfsLower string // overlay lowerdir: the node's own filesystem or an exported image

	DurableMounts    map[string]string // host dir -> container mount path, rw (includes any workspace volume a template declares)
	SystemInfoMounts map[string]string // host dir -> container mount path, ro (trust-bundle files already written under it)

	// UID and GID are the actor process's own identity inside the
	// sandbox, resolved from the image's USER directive (empty means
	// root, uid/gid 0, every template before this one used). A non-root
	// value only actually works when Namespace.Open mapped more than
	// root (idmap.go); under a root-only mapping the gofer's own
	// setfsuid for this uid resolves to the host's overflow id and the
	// actor's writes fail with EACCES, by design (see Open's doc
	// comment).
	UID, GID uint32

	Egress              []EgressDestination
	EgressSocketsDir    string // host dir holding every Egress[].SocketName, bind-mounted read-only at /run/ate-ish/egress
	EgressControlSocket string // host path to a wildcard rule's control socket (internal/egress's ListenControl), bind-mounted at /run/ate-ish/egress-control.sock; empty when the policy has no wildcard hostname
	IngressSocket       string // host path to the node's ingress.sock (bind-mounted at /run/ate-ish/ingress.sock)
	IngressTarget       string // "127.0.0.1:<port>" the actor's own server listens on, ATE_INGRESS_TARGET

	AteIshBinary string // this process's own executable, copied to <bundle>/.ate/ate-ish
}

// Bundle is one built OCI bundle, ready for Namespace.Run/Restore.
type Bundle struct {
	Dir    string // the bundle directory itself (config.json lives here)
	Rootfs string
	spec   ActorSpec
}

// sandboxInitArg is argv[1] the in-sandbox init (RunInit, initcmd.go)
// recognizes; ate-ish's own hidden dispatch (cmd/ate-ish/main.go) routes
// this exact string to RunInit before any normal CLI parsing.
const sandboxInitArg = "__sandbox_init__"

// dnsListenIP and egressControlPath are shared between Build (which
// writes resolv.conf and the control-socket bind mount) and RunInit
// (which starts the in-sandbox DNS responder and dials the control
// socket): the fixed address and mount path a wildcard-hostname rule
// needs, present only when ActorSpec.EgressControlSocket is set.
// dnsListenIP sits outside 127.0.0.0/24, openEgress's own static-
// destination range, so the two never collide no matter how many
// static destinations an actor's policy also names.
const (
	dnsListenIP       = "127.0.53.53"
	egressControlPath = "/run/ate-ish/egress-control.sock"
)

// Build writes bundleDir/config.json and every file the spec's mounts
// name (/etc/hosts, /etc/resolv.conf, the ATE_EGRESS env value), then
// mounts the tmpfs+overlay rootfs inside the held namespace, matching
// the spike's actor_bundle/actor_overlay exactly. Call Namespace.Destroy
// to unmount and remove bundleDir when the actor is gone for good.
func (n *Namespace) Build(ctx context.Context, bundleDir string, spec ActorSpec) (*Bundle, error) {
	rootfs := filepath.Join(bundleDir, "rootfs")
	for _, dir := range []string{bundleDir, rootfs, filepath.Join(bundleDir, "etc")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	resolvConf := []byte(nil)
	if spec.EgressControlSocket != "" {
		// Only set when a wildcard-hostname rule exists: a plain static
		// policy keeps the empty resolv.conf today's actors already run
		// with, resolving nothing but /etc/hosts.
		resolvConf = []byte("nameserver " + dnsListenIP + "\n")
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "etc", "resolv.conf"), resolvConf, 0o644); err != nil {
		return nil, err
	}
	hosts := "127.0.0.1 localhost\n"
	egressEnv := ""
	for _, d := range spec.Egress {
		hosts += d.ListenIP + " " + d.Hostname + "\n"
		egressEnv += d.ListenIP + ":" + fmt.Sprint(d.Port) + ":/run/ate-ish/egress/" + d.SocketName + " "
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "etc", "hosts"), []byte(hosts), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "etc", "hostname"), []byte(spec.Hostname+"\n"), 0o644); err != nil {
		return nil, err
	}
	ateDir := filepath.Join(bundleDir, ".ate")
	if err := os.MkdirAll(ateDir, 0o755); err != nil {
		return nil, err
	}
	if err := copyExecutable(spec.AteIshBinary, filepath.Join(ateDir, "ate-ish")); err != nil {
		return nil, fmt.Errorf("copying ate-ish into the bundle: %w", err)
	}
	cfg := buildConfig(spec, bundleDir, egressEnv)
	data, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), data, 0o644); err != nil {
		return nil, err
	}

	b := &Bundle{Dir: bundleDir, Rootfs: rootfs, spec: spec}
	if err := n.mountOverlay(ctx, b); err != nil {
		return nil, err
	}
	if err := n.prepareMountpoints(ctx, b.Rootfs, cfg.Mounts); err != nil {
		return nil, err
	}
	return b, nil
}

// prepareMountpoints creates every bind mount's destination inside
// rootfs before runsc runs: unlike runc, runsc does not create a
// missing bind-mount target itself (measured: "failed to load
// /.ate/ate-ish: no such file or directory" with the destination
// missing, even though the bind mount's source exists). A directory
// source gets mkdir -p; a file source gets its parent directory plus an
// empty placeholder file for the bind to land on.
func (n *Namespace) prepareMountpoints(ctx context.Context, rootfs string, mounts []ociMount) error {
	var script strings.Builder
	script.WriteString("set -e\n")
	for _, m := range mounts {
		if m.Type != "bind" {
			continue
		}
		dest := filepath.Join(rootfs, m.Destination)
		info, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("stat %s (mount source for %s): %w", m.Source, m.Destination, err)
		}
		if info.IsDir() {
			fmt.Fprintf(&script, "mkdir -p %q\n", dest)
		} else {
			fmt.Fprintf(&script, "mkdir -p %q && touch %q\n", filepath.Dir(dest), dest)
		}
	}
	out, err := n.EnterSh(ctx, script.String())
	if err != nil {
		return fmt.Errorf("preparing mount points: %w: %s", err, out)
	}
	return nil
}

// SeedUpper copies srcDir (a plain host directory, outside any mount
// this package made, e.g. the actor's own root dir) into the bundle's
// overlay upperdir, before the caller runs restore: a suspended actor's
// writes to its own rootfs (outside any durable-dir volume) live only in
// that upperdir, which a fresh Build starts empty, so restoring them is
// this package's job, not runsc restore's (gVisor's own checkpoint is
// process/memory state, not a second copy of changed rootfs bytes). Both
// srcDir and the upperdir must be reached through the held namespace:
// the upperdir always (it lives under the tmpfs Build mounted), srcDir
// only incidentally (nsenter's cwd works either way, since the held
// namespace's mount tree is the host's plus this package's own private
// additions).
func (n *Namespace) SeedUpper(ctx context.Context, b *Bundle, srcDir string) error {
	if _, err := os.Stat(srcDir); os.IsNotExist(err) {
		return nil
	}
	script := fmt.Sprintf("(cd %q && tar -cf - .) | (cd %q && tar -xf -)", srcDir, filepath.Join(b.Dir, "ovl", "upper"))
	out, err := n.EnterSh(ctx, script)
	if err != nil {
		return fmt.Errorf("seeding rootfs upper: %w: %s", err, out)
	}
	return nil
}

// DumpUpper copies the bundle's overlay upperdir into destDir (a plain
// host directory), the save side of SeedUpper.
func (n *Namespace) DumpUpper(ctx context.Context, b *Bundle, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	script := fmt.Sprintf("(cd %q && tar -cf - .) | (cd %q && tar -xf -)", filepath.Join(b.Dir, "ovl", "upper"), destDir)
	out, err := n.EnterSh(ctx, script)
	if err != nil {
		return fmt.Errorf("dumping rootfs upper: %w: %s", err, out)
	}
	return nil
}

// Destroy unmounts the bundle's overlay rootfs and removes its directory.
// Best effort: an already-unmounted or already-removed bundle is not an
// error.
func (n *Namespace) Destroy(ctx context.Context, b *Bundle) error {
	ovl := filepath.Join(b.Dir, "ovl")
	script := fmt.Sprintf("umount -l %q 2>/dev/null; umount -l %q 2>/dev/null; true", b.Rootfs, ovl)
	if out, err := n.EnterSh(ctx, script); err != nil {
		return fmt.Errorf("unmounting %s: %w: %s", b.Rootfs, err, out)
	}
	return os.RemoveAll(b.Dir)
}

// mountOverlay mounts a tmpfs-backed overlay over spec.RootfsLower as
// b.Rootfs, inside the held namespace: lowerdir read-only, upperdir/
// workdir on fresh tmpfs, exactly as the spike's actor_overlay did. The
// mount must land in the held namespace's own mount namespace (where
// runsc will look for it), not this process's, so it goes through
// EnterSh rather than a plain syscall.Mount here.
func (n *Namespace) mountOverlay(ctx context.Context, b *Bundle) error {
	ovl := filepath.Join(b.Dir, "ovl")
	script := fmt.Sprintf(
		"set -e; mkdir -p %[1]q; mount -t tmpfs tmpfs %[1]q; mkdir -p %[1]s/upper %[1]s/work; "+
			"mount -t overlay overlay -o lowerdir=%[3]s,upperdir=%[1]s/upper,workdir=%[1]s/work %[2]q",
		ovl, b.Rootfs, b.spec.RootfsLower)
	if out, err := n.EnterSh(ctx, script); err != nil {
		n.EnterSh(ctx, fmt.Sprintf("umount -l %q 2>/dev/null; umount -l %q 2>/dev/null", b.Rootfs, ovl))
		return fmt.Errorf("mounting overlay rootfs at %s: %w: %s", b.Rootfs, err, out)
	}
	return nil
}

// copyExecutable copies the running ate-ish binary into the bundle. A
// plain io.Copy, not a hardlink: the source keeps running (and may be
// replaced by a later `go install`) for as long as this process does.
func copyExecutable(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// A dynamically linked ate-ish has no libc to resolve against inside
	// the sandbox's own rootfs (no /lib64/ld-linux-*.so.2 there, ever):
	// gVisor's own error for that ("failed to load /.ate/ate-ish: no
	// such file or directory") names no missing dynamic linker at all,
	// so this checks for one directly rather than let every boot fail
	// on that message.
	if isDynamicallyLinked(data) {
		return fmt.Errorf("%s is dynamically linked; build ate-ish with CGO_ENABLED=0 (or add -tags netgo) so it runs with no libc inside the sandbox", src)
	}
	return os.WriteFile(dst, data, 0o755)
}

type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
}

type ociSpec struct {
	OCIVersion string     `json:"ociVersion"`
	Process    ociProcess `json:"process"`
	Root       ociRoot    `json:"root"`
	Hostname   string     `json:"hostname"`
	Mounts     []ociMount `json:"mounts"`
	Linux      ociLinux   `json:"linux"`
}

type ociProcess struct {
	User         ociUser         `json:"user"`
	Args         []string        `json:"args"`
	Env          []string        `json:"env"`
	Cwd          string          `json:"cwd"`
	Capabilities ociCapabilities `json:"capabilities"`
}

type ociUser struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

type ociCapabilities struct {
	Bounding  []string `json:"bounding"`
	Effective []string `json:"effective"`
	Permitted []string `json:"permitted"`
}

type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type ociLinux struct {
	Namespaces []ociNamespace `json:"namespaces"`
}

type ociNamespace struct {
	Type string `json:"type"`
}

// bundleCaps is the capability set the spike's actor_bundle granted: just
// enough for a plain actor process to bind a low port and run as
// whatever uid its own image's USER directive names.
var bundleCaps = []string{"CAP_NET_BIND_SERVICE", "CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_SETUID", "CAP_SETGID", "CAP_KILL"}

func buildConfig(spec ActorSpec, bundleDir, egressEnv string) ociSpec {
	env := append([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, spec.Env...)
	if egressEnv != "" {
		env = append(env, "ATE_EGRESS="+egressEnv)
	}
	if spec.IngressSocket != "" {
		env = append(env, "ATE_INGRESS_TARGET="+spec.IngressTarget)
	}
	if spec.UID != 0 || spec.GID != 0 {
		// RunInit (init.go) reads these to start the actor's own
		// process under that uid/gid, instead of this spec's own
		// always-root Process.User above.
		env = append(env, fmt.Sprintf("ATE_ACTOR_UID=%d", spec.UID), fmt.Sprintf("ATE_ACTOR_GID=%d", spec.GID))
	}
	mounts := []ociMount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs"},
		{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs"},
		bind("/.ate", filepath.Join(bundleDir, ".ate"), true),
		bind("/etc/hosts", filepath.Join(bundleDir, "etc", "hosts"), true),
		bind("/etc/resolv.conf", filepath.Join(bundleDir, "etc", "resolv.conf"), true),
	}
	if len(spec.Egress) > 0 || spec.EgressControlSocket != "" {
		// Not read-only when a control socket is in play: a wildcard
		// match creates its socket under this same directory after
		// boot, and the sandbox's mount of it must see that file
		// appear, which an "ro" bind still allows (ro only blocks
		// writes from inside the sandbox, not new entries the host
		// side adds to the directory the bind mount points at).
		mounts = append(mounts, bind("/run/ate-ish/egress", spec.EgressSocketsDir, true))
	}
	if spec.EgressControlSocket != "" {
		mounts = append(mounts, bind(egressControlPath, spec.EgressControlSocket, false))
	}
	if spec.IngressSocket != "" {
		mounts = append(mounts, bind("/run/ate-ish/ingress.sock", spec.IngressSocket, false))
	}
	for host, path := range spec.DurableMounts {
		mounts = append(mounts, bind(path, host, false))
	}
	for host, path := range spec.SystemInfoMounts {
		mounts = append(mounts, bind(path, host, true))
	}
	return ociSpec{
		OCIVersion: "1.0.0",
		Process: ociProcess{
			// Always root: the sandbox's pid 1 is this package's own
			// init (RunInit, init.go), which needs real access to the
			// host-backed sockets bind-mounted under /run/ate-ish
			// (ingress, egress) for as long as it runs, regardless of
			// what uid the actor's own image declares. RunInit itself
			// drops the *actor child process* to spec.UID/GID (passed
			// through ATE_ACTOR_UID/ATE_ACTOR_GID below) before
			// starting it, the same pattern gosu or tini use, not this
			// OCI field.
			User:         ociUser{UID: 0, GID: 0},
			Args:         append([]string{"/.ate/ate-ish", sandboxInitArg}, spec.Command...),
			Env:          env,
			Cwd:          "/",
			Capabilities: ociCapabilities{Bounding: bundleCaps, Effective: bundleCaps, Permitted: bundleCaps},
		},
		Root:     ociRoot{Path: filepath.Join(bundleDir, "rootfs"), Readonly: false},
		Hostname: spec.Hostname,
		Mounts:   mounts,
		Linux: ociLinux{Namespaces: []ociNamespace{
			{Type: "pid"}, {Type: "network"}, {Type: "ipc"}, {Type: "uts"}, {Type: "mount"},
		}},
	}
}

func bind(dst, src string, ro bool) ociMount {
	opt := "rw"
	if ro {
		opt = "ro"
	}
	return ociMount{Destination: dst, Type: "bind", Source: src, Options: []string{"rbind", opt}}
}
