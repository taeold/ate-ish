package atelet

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/taeold/ate-ish/internal/apierr"
	"github.com/taeold/ate-ish/internal/egress"
	"github.com/taeold/ate-ish/internal/envutil"
	"github.com/taeold/ate-ish/internal/records"
	"github.com/taeold/ate-ish/internal/sandbox"
	"github.com/taeold/ate-ish/internal/store"
)

// env is the one ATE_* lookup every file in this package uses.
func env(name, def string) string { return envutil.Get(name, def) }

// cacheDir is the one directory runsc, exported image rootfses and
// (via docker's own layer cache) built demo images live under, shared
// by every node and every hack/smoke.sh run on this machine so a warm
// run never re-fetches or re-exports anything it already has.
// ATE_CACHE_DIR overrides it directly; otherwise XDG_CACHE_HOME/ate-ish,
// or ~/.cache/ate-ish. Not ~/.local/state/ate-ish (stage A's old
// default for the runsc release): that directory was left root-owned
// by an earlier sudo'd run on this VM, so a plain exedev process can
// stage nothing under it without sudo; moving the shared cache under
// $HOME/.cache sidesteps that instead of fixing it.
func cacheDir() string {
	if dir := env("ATE_CACHE_DIR", ""); dir != "" {
		return dir
	}
	base := env("XDG_CACHE_HOME", "")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/root"
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "ate-ish")
}

// runscReleaseDir is where runsc (and its gvisor-bin/ sidecars) are
// staged: ATE_RUNSC_DIR overrides it directly, else cacheDir()/runsc.
// The held namespace (sandbox.Open) shares this process's own mount
// tree at the point it is created, so this one path is visible to every
// runsc invocation inside it; no per-actor copy is needed any more (the
// vendored worker's own validateRunscPath required one, this build does
// not).
func runscReleaseDir() string {
	if dir := env("ATE_RUNSC_DIR", ""); dir != "" {
		return dir
	}
	return filepath.Join(cacheDir(), "runsc")
}

// rootfsCacheDir is where exported image rootfses are kept.
// ATE_ROOTFS_DIR overrides it directly; otherwise cacheDir()/rootfs
// (imageRootfsDir appends that "rootfs" segment itself).
func rootfsCacheDir() string {
	if dir := env("ATE_ROOTFS_DIR", ""); dir != "" {
		return dir
	}
	return cacheDir()
}

// Worker is ate-ish acting as both atelet and worker-pool node: it holds
// one unprivileged namespace (internal/sandbox) that every actor's runsc
// sandbox runs inside, fronts the egress gateway (internal/egress), and
// drives actor lifecycle directly through sandbox.Namespace instead of a
// vendored worker subprocess.
type Worker struct {
	id    string
	addr  string
	dir   string
	store store.Store

	ns        *sandbox.Namespace
	gateway   *egress.Gateway
	runscPath string

	mu    sync.Mutex
	slots []*hosted

	dirty    atomic.Bool
	leaseMu  sync.Mutex
	leaseGen int64
}

// hosted is one actor running in this worker's namespace.
type hosted struct {
	as, name      string
	gen           int64
	slot          int
	actorUID      string
	dirs          *actorDirs
	tmpl          records.Template
	bundle        *sandbox.Bundle
	egressSockets []string
	ingress       *ingressBridge

	ready    chan struct{}
	err      error
	busy     int
	lastUsed time.Time
	stopping bool
}

func (h *hosted) key() string { return h.as + "/" + h.name }

func New(ctx context.Context, b store.Store, addr string, capacity int, dir string) (*Worker, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ns, err := sandbox.Open(filepath.Join(dir, "runsc-state"))
	if err != nil {
		return nil, fmt.Errorf("opening the held namespace: %w", err)
	}
	gateway, err := egress.New(store.GoogleHTTPClient(ctx))
	if err != nil {
		ns.Close()
		return nil, fmt.Errorf("starting the egress gateway: %w", err)
	}

	w := &Worker{
		id: "w-" + randomHex(4), addr: addr, dir: dir, store: b,
		ns: ns, gateway: gateway, runscPath: filepath.Join(runscReleaseDir(), "runsc"),
		slots: make([]*hosted, capacity),
	}
	if err := w.writeLease(ctx); err != nil {
		ns.Close()
		return nil, err
	}
	go func() {
		last := time.Now()
		for range time.Tick(time.Second) {
			if w.dirty.Swap(false) || time.Since(last) > 10*time.Second {
				if err := w.writeLease(context.Background()); err != nil {
					slog.Error("lease", "err", err)
				}
				last = time.Now()
			}
		}
	}()
	slog.Info("worker up", "id", w.id, "addr", addr, "capacity", capacity)
	return w, nil
}

func (w *Worker) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /resume", func(rw http.ResponseWriter, r *http.Request) {
		var req struct {
			As, Actor        string
			RecordGeneration int64
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		gen, err := w.resume(context.WithoutCancel(r.Context()), req.As, req.Actor, req.RecordGeneration)
		apierr.Reply(rw, map[string]int64{"generation": gen}, err)
	})
	mux.HandleFunc("POST /suspend", func(rw http.ResponseWriter, r *http.Request) {
		var req struct{ As, Actor string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		apierr.Reply(rw, nil, w.suspend(context.WithoutCancel(r.Context()), req.As+"/"+req.Actor))
	})
	mux.HandleFunc("POST /reap", func(rw http.ResponseWriter, r *http.Request) {
		idle, _ := strconv.Atoi(r.URL.Query().Get("idleSeconds"))
		apierr.Reply(rw, w.reap(context.WithoutCancel(r.Context()), time.Duration(idle)*time.Second), nil)
	})
	mux.HandleFunc("/actors/{as}/{actor}/{path...}", w.proxy)
}

func (w *Worker) find(key string) *hosted {
	for _, h := range w.slots {
		if h != nil && h.key() == key {
			return h
		}
	}
	return nil
}

// resume runs an actor in a free slot and returns the generation it runs.
func (w *Worker) resume(ctx context.Context, as, name string, recordGen int64) (int64, error) {
	w.mu.Lock()
	h := w.find(as + "/" + name)
	if h != nil {
		w.mu.Unlock()
		<-h.ready
		if h.err != nil {
			return 0, h.err
		}
		return h.gen, w.current(ctx, h)
	}
	slot := slices.Index(w.slots, nil)
	if slot < 0 {
		w.mu.Unlock()
		return 0, apierr.ErrFull
	}
	h = &hosted{as: as, name: name, slot: slot, actorUID: newActorUID(), ready: make(chan struct{}), lastUsed: time.Now()}
	w.slots[slot] = h
	w.mu.Unlock()

	h.err = w.start(ctx, h, recordGen)
	if h.err != nil {
		w.mu.Lock()
		w.slots[slot] = nil
		w.mu.Unlock()
	}
	close(h.ready)
	w.dirty.Store(true)
	return h.gen, h.err
}

func (w *Worker) current(ctx context.Context, h *hosted) error {
	w.mu.Lock()
	stopping := h.stopping
	w.mu.Unlock()
	if stopping {
		return fmt.Errorf("%w: %s", apierr.ErrStopping, h.key())
	}
	var rec records.Actor
	if _, err := w.store.Get(ctx, records.ActorKey(h.as, h.name), &rec); err != nil {
		return err
	}
	if rec.Owner != w.id || rec.Generation != h.gen {
		go w.suspend(context.Background(), h.key())
		return fmt.Errorf("%w: the record of %s names generation %d on %s, this copy is %d",
			store.ErrConflict, h.key(), rec.Generation, rec.Owner, h.gen)
	}
	return nil
}

// start claims the actor with a conditional write of its record, then boots
// it. If the boot fails, the record goes back to suspended.
func (w *Worker) start(ctx context.Context, h *hosted, recordGen int64) error {
	key := records.ActorKey(h.as, h.name)
	var rec records.Actor
	gen, err := w.store.Get(ctx, key, &rec)
	if err != nil {
		return err
	}
	if gen != recordGen {
		return fmt.Errorf("%w: the record of %s changed", store.ErrConflict, h.key())
	}
	rec.Status, rec.Owner, rec.Generation = records.Running, w.id, rec.Generation+1
	if _, err = w.store.Put(ctx, key, rec, gen); err != nil {
		return err
	}
	h.gen = rec.Generation
	if err := w.boot(ctx, h, rec); err != nil {
		return errors.Join(err, w.release(ctx, h))
	}
	return nil
}

func (w *Worker) release(ctx context.Context, h *hosted) error {
	var err error
	for range 5 {
		var rec records.Actor
		var gen int64
		if gen, err = w.store.Get(ctx, records.ActorKey(h.as, h.name), &rec); err != nil {
			return err
		}
		if rec.Owner != w.id || rec.Generation != h.gen {
			return nil
		}
		rec.Status, rec.Owner = records.Suspended, ""
		_, err = w.store.Put(ctx, records.ActorKey(h.as, h.name), rec, gen)
		if !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrRateLimited) {
			return err
		}
		time.Sleep(time.Second)
	}
	return err
}

// boot builds the actor's dirs, egress sockets, and OCI bundle, then runs
// or restores it through the held namespace.
func (w *Worker) boot(ctx context.Context, h *hosted, rec records.Actor) error {
	t0 := time.Now()
	var tmpl records.Template
	if _, err := w.store.Get(ctx, records.TemplateKey(h.as, rec.Template), &tmpl); err != nil {
		return fmt.Errorf("template %s: %w", rec.Template, err)
	}
	if len(tmpl.Containers) != 1 {
		return fmt.Errorf("template %s: exactly one container is supported, got %d", rec.Template, len(tmpl.Containers))
	}
	if tmpl.Containers[0].Image == "" {
		return fmt.Errorf("template %s: container has no image (this build's only rootfs source)", rec.Template)
	}
	h.tmpl = tmpl

	dirs := buildActorDirs(w.dir, h.actorUID)
	h.dirs = dirs
	durableNames, systemInfoNames := durableVolumeNames(tmpl)
	if err := prepareActorDirs(dirs, durableNames, systemInfoNames); err != nil {
		return err
	}
	if err := writeSystemInfoFiles(dirs, tmpl, w.gateway.CAPEM()); err != nil {
		return err
	}
	durableMounts, systemInfoMounts, err := volumeMounts(dirs, tmpl)
	if err != nil {
		return err
	}

	lowerDir, err := imageRootfsDir(rootfsCacheDir(), tmpl.Containers[0].Image)
	if err != nil {
		return fmt.Errorf("preparing %s's rootfs: %w", tmpl.Containers[0].Image, err)
	}

	egressDests, egressSockets, egressControl, err := w.openEgress(h, rec.EgressPolicy)
	if err != nil {
		return fmt.Errorf("egress policy: %w", err)
	}
	h.egressSockets = egressSockets

	port := tmpl.Containers[0].WakeupProbe.HTTPGet.Port
	if port == 0 {
		port = 80
	}
	h.ingress = newIngressBridge(dirs.ingressSocket)
	if err := h.ingress.Listen(); err != nil {
		w.gateway.Close(egressSockets...)
		return fmt.Errorf("listening for ingress: %w", err)
	}

	command, cenv := actorCommandEnv(tmpl.Containers[0])
	spec := sandbox.ActorSpec{
		ID: h.actorUID, Hostname: containerName(tmpl.Containers[0]),
		Command: command, Env: cenv, RootfsLower: lowerDir,
		DurableMounts: durableMounts, SystemInfoMounts: systemInfoMounts,
		Egress:              egressDests,
		EgressSocketsDir:    filepath.Join(dirs.root, "egress"),
		EgressControlSocket: egressControl,
		IngressSocket:       dirs.ingressSocket,
		IngressTarget:       fmt.Sprintf("127.0.0.1:%d", port),
		AteIshBinary:        mustExecutable(),
	}

	bundleDir := filepath.Join(dirs.ociBundleDir, h.actorUID)
	bundle, err := w.ns.Build(ctx, bundleDir, spec)
	if err != nil {
		h.ingress.Close()
		w.gateway.Close(egressSockets...)
		return fmt.Errorf("building the bundle: %w", err)
	}
	h.bundle = bundle

	limits := sandbox.Limits{} // template-driven limits land in a later stage
	fidelity := fidelityIsMemory(tmpl)
	var bootErr error
	switch err := w.store.DownloadFile(ctx, records.SnapshotKey(h.as, h.name), filepath.Join(dirs.root, "snapshot.tar.gz")); {
	case err == nil:
		snapshotTar := filepath.Join(dirs.root, "snapshot.tar.gz")
		defer os.Remove(snapshotTar)
		if err := untarDirs(snapshotTar, map[string]string{
			snapshotCheckpointPrefix:  dirs.checkpointDir,
			snapshotDurablePrefix:     dirs.durableRoot,
			snapshotRootfsUpperPrefix: dirs.rootfsUpperDir,
		}); err != nil {
			bootErr = fmt.Errorf("extracting snapshot: %w", err)
			break
		}
		if err := w.ns.SeedUpper(ctx, bundle, dirs.rootfsUpperDir); err != nil {
			bootErr = fmt.Errorf("restoring rootfs writes: %w", err)
			break
		}
		bootErr = w.ns.Restore(ctx, w.runscPath, bundle.Dir, dirs.checkpointDir, h.actorUID, limits)
		if bootErr == nil {
			// Suspend's SIGUSR1 left every egress forwarder's listener
			// closed (gVisor refuses to checkpoint a bound host socket);
			// SIGUSR2 reopens them, matching the signal Namespace.Checkpoint's
			// own doc comment and the spike's node.sh actor_restore send
			// after a restore completes.
			bootErr = w.ns.Kill(ctx, w.runscPath, h.actorUID, "USR2")
		}
	case errors.Is(err, store.ErrNotFound):
		bootErr = w.ns.Run(ctx, w.runscPath, bundle.Dir, h.actorUID, limits)
	default:
		bootErr = fmt.Errorf("downloading snapshot: %w", err)
	}
	downloaded := time.Since(t0)
	if bootErr != nil {
		w.teardownBundle(ctx, h)
		return bootErr
	}

	path := tmpl.Containers[0].WakeupProbe.HTTPGet.Path
	timeout := time.Duration(max(tmpl.Containers[0].WakeupProbe.TimeoutSeconds, 30)) * time.Second
	if err := h.ingress.WaitReady(ctx, path, timeout); err != nil {
		w.ns.Delete(context.Background(), w.runscPath, h.actorUID)
		w.teardownBundle(ctx, h)
		return err
	}
	slog.Info("actor started", "actor", h.key(), "generation", h.gen, "actor_uid", h.actorUID, "slot", h.slot,
		"memory_fidelity", fidelity, "snapshot_ms", downloaded.Milliseconds(), "boot_ms", (time.Since(t0) - downloaded).Milliseconds())
	return nil
}

// openEgress allocates one loopback address and host socket per exact
// destination policy allows (round 3's design, generalized here past
// https-only and port-443-only), and, when any rule names a wildcard
// hostname, one control socket the sandbox's own DNS responder uses to
// resolve a matching name on demand (docs/ROOTLESS.md's wildcard
// kind). tlsPassthrough and actorJwt parse but are not wired to
// anything; a policy naming either gets one log line per rule and is
// otherwise ignored, exactly as the spec says.
func (w *Worker) openEgress(h *hosted, policy *records.EgressPolicy) (dests []sandbox.EgressDestination, sockets []string, control string, err error) {
	if policy == nil {
		return nil, nil, "", nil
	}
	egressDir := filepath.Join(h.dirs.root, "egress")
	if err := os.MkdirAll(egressDir, 0o755); err != nil {
		return nil, nil, "", err
	}
	n := 2 // 127.0.0.1 is reserved for localhost inside the sandbox
	nextIP := func() string { ip := fmt.Sprintf("127.0.0.%d", n); n++; return ip }

	var wildcards []egress.WildcardRule
	addStatic := func(proto string, spec *records.EgressRuleSpec) error {
		port := spec.Port
		if port == 0 {
			port = map[string]int{"http": 80, "https": 443}[proto]
		}
		for _, hostname := range spec.Hostnames {
			if records.IsWildcard(hostname) {
				wildcards = append(wildcards, egress.WildcardRule{
					Pattern: hostname, Proto: proto, Port: port, ReplaceHeaders: spec.Effects.ReplaceHeaders,
				})
				continue
			}
			ip := nextIP()
			name := fmt.Sprintf("%d.sock", n-1)
			sock := filepath.Join(egressDir, name)
			if err := w.gateway.Listen(egress.Destination{
				Actor: h.key(), Socket: sock, Proto: proto, Host: hostname, Port: port,
				ReplaceHeaders: spec.Effects.ReplaceHeaders,
			}); err != nil {
				return err
			}
			dests = append(dests, sandbox.EgressDestination{ListenIP: ip, Port: port, SocketName: name, Hostname: hostname})
			sockets = append(sockets, sock)
		}
		return nil
	}

	for _, rule := range policy.Rules {
		switch {
		case rule.HTTP != nil:
			if err := addStatic("http", rule.HTTP); err != nil {
				w.gateway.Close(sockets...)
				return nil, nil, "", err
			}
		case rule.HTTPS != nil:
			if err := addStatic("https", rule.HTTPS); err != nil {
				w.gateway.Close(sockets...)
				return nil, nil, "", err
			}
		case rule.TLSPassthrough != nil:
			slog.Info("egress: tlsPassthrough rule is not supported in this build, ignoring", "actor", h.key())
		case rule.ActorJWT != nil:
			slog.Info("egress: actorJwt rule is not supported in this build, ignoring", "actor", h.key())
		}
	}

	if len(wildcards) == 0 {
		return dests, sockets, "", nil
	}
	control = filepath.Join(egressDir, "_control.sock")
	if err := w.gateway.ListenControl(h.key(), control, wildcards, egressDir, nextIP); err != nil {
		w.gateway.Close(sockets...)
		return nil, nil, "", err
	}
	sockets = append(sockets, control)
	return dests, sockets, control, nil
}

func (w *Worker) teardownBundle(ctx context.Context, h *hosted) {
	if h.bundle != nil {
		w.ns.Destroy(ctx, h.bundle)
	}
	if h.ingress != nil {
		h.ingress.Close()
	}
	w.gateway.Close(h.egressSockets...)
	// A wildcard rule's control channel may have opened sockets after
	// boot, for hosts the sandbox's own DNS responder actually queried;
	// h.egressSockets never names those, so sweep by actor as well.
	w.gateway.CloseActor(h.key())
}

func (w *Worker) terminateWorkload(ctx context.Context, h *hosted) {
	if err := w.ns.Delete(ctx, w.runscPath, h.actorUID); err != nil {
		slog.Warn("delete", "actor", h.key(), "err", err)
	}
}

// suspend checkpoints an actor, uploads the checkpoint as the snapshot, and
// marks the record suspended. If any step fails, the actor keeps its sandbox
// and running record, so a later suspend can try again.
func (w *Worker) suspend(ctx context.Context, key string) error {
	t0 := time.Now()
	w.mu.Lock()
	h := w.find(key)
	if h == nil {
		w.mu.Unlock()
		return apierr.ErrNotHosted
	}
	if h.stopping {
		w.mu.Unlock()
		return fmt.Errorf("%w: %s", apierr.ErrStopping, key)
	}
	h.stopping = true
	w.mu.Unlock()
	<-h.ready
	if h.err != nil {
		return apierr.ErrNotHosted
	}

	// gVisor refuses to save a sandbox holding a bound host socket:
	// SIGUSR1 closes the egress forwarders' listeners before checkpoint
	// (docs/ROOTLESS-SPIKE.md round 2); the ingress side never binds one
	// (it only ever dials out), so it needs no signal.
	if err := w.ns.Kill(ctx, w.runscPath, h.actorUID, "USR1"); err != nil {
		slog.Warn("suspend: signalling egress forwarders to close", "actor", key, "err", err)
	}
	// h.dirs.checkpointDir may already hold the checkpoint this actor was
	// restored from (boot's untarDirs extracted it there); runsc refuses
	// to overwrite an existing checkpoint.img rather than replace it.
	if err := resetDir(h.dirs.checkpointDir); err != nil {
		w.mu.Lock()
		h.stopping = false
		w.mu.Unlock()
		return err
	}
	err := w.ns.Checkpoint(ctx, w.runscPath, h.dirs.checkpointDir, h.actorUID)
	checkpointed := time.Since(t0)
	if err == nil {
		err = w.ns.DumpUpper(ctx, h.bundle, h.dirs.rootfsUpperDir)
	}

	snapshotTar := filepath.Join(h.dirs.root, "snapshot-out.tar.gz")
	if err == nil {
		err = tarDirs(map[string]string{
			snapshotCheckpointPrefix:  h.dirs.checkpointDir,
			snapshotDurablePrefix:     h.dirs.durableRoot,
			snapshotRootfsUpperPrefix: h.dirs.rootfsUpperDir,
		}, snapshotTar)
	}
	var rec records.Actor
	if err == nil {
		_, err = w.store.Get(ctx, records.ActorKey(h.as, h.name), &rec)
	}
	if err == nil && rec.Owner == w.id && rec.Generation == h.gen {
		err = w.store.UploadFile(ctx, records.SnapshotKey(h.as, h.name), snapshotTar)
	}
	uploaded := time.Since(t0)
	size := fileSize(snapshotTar)
	os.Remove(snapshotTar)
	if err != nil {
		w.mu.Lock()
		h.stopping = false
		w.mu.Unlock()
		slog.Error("suspend failed, actor kept", "actor", key, "generation", h.gen, "err", err)
		return err
	}

	w.terminateWorkload(ctx, h)
	w.teardownBundle(ctx, h)
	os.RemoveAll(h.dirs.root)
	err = w.release(ctx, h)
	w.mu.Lock()
	w.slots[h.slot] = nil
	w.mu.Unlock()
	w.dirty.Store(true)
	slog.Info("actor suspended", "actor", key, "generation", h.gen, "checkpoint_ms", checkpointed.Milliseconds(),
		"upload_ms", (uploaded - checkpointed).Milliseconds(), "total_ms", time.Since(t0).Milliseconds(), "bytes", size, "err", err)
	return err
}

// resetDir removes dir and recreates it empty.
func resetDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

func fileSize(file string) int64 {
	st, err := os.Stat(file)
	if err != nil {
		return 0
	}
	return st.Size()
}

func (w *Worker) reap(ctx context.Context, idle time.Duration) []string {
	w.mu.Lock()
	var idleKeys []string
	for _, h := range w.slots {
		if h != nil && !h.stopping && h.busy == 0 && time.Since(h.lastUsed) > idle {
			idleKeys = append(idleKeys, h.key())
		}
	}
	w.mu.Unlock()
	var done []string
	for _, key := range idleKeys {
		if err := w.suspend(ctx, key); err != nil {
			slog.Error("reap", "actor", key, "err", err)
			continue
		}
		done = append(done, key)
	}
	return done
}

// SuspendAll runs on SIGTERM.
func (w *Worker) SuspendAll(ctx context.Context) {
	w.mu.Lock()
	var keys []string
	for _, h := range w.slots {
		if h != nil {
			keys = append(keys, h.key())
		}
	}
	w.mu.Unlock()
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Go(func() { slog.Info("suspend on shutdown", "actor", key, "err", w.suspend(ctx, key)) })
	}
	wg.Wait()
	slog.Info("lease deleted", "err", w.store.Delete(ctx, records.LeaseKey(w.id)))
	if w.ns != nil {
		w.ns.Close()
	}
}

// proxy forwards an ingress request to the actor, through its node-side
// ingress bridge (the pool of connections the sandbox's own dial-out
// forwarder keeps open; see ingress.go).
func (w *Worker) proxy(rw http.ResponseWriter, r *http.Request) {
	key := r.PathValue("as") + "/" + r.PathValue("actor")
	gen, _ := strconv.ParseInt(r.Header.Get("ate-actor-generation"), 10, 64)
	w.mu.Lock()
	h := w.find(key)
	w.mu.Unlock()
	if h != nil {
		<-h.ready
	}
	if h == nil || h.gen != gen {
		http.Error(rw, fmt.Sprintf("generation %d of %s is not running on %s", gen, key, w.id), http.StatusConflict)
		return
	}
	w.mu.Lock()
	ok := h.err == nil && !h.stopping
	if ok {
		h.busy++
	}
	w.mu.Unlock()
	if !ok {
		http.Error(rw, key+" is not available on "+w.id, http.StatusServiceUnavailable)
		return
	}
	defer func() {
		w.mu.Lock()
		h.busy--
		h.lastUsed = time.Now()
		w.mu.Unlock()
	}()
	(&httputil.ReverseProxy{
		Transport:     h.ingress,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "actor"
			pr.Out.URL.Path = "/" + r.PathValue("path")
			pr.Out.Header.Del("ate-actor-generation")
		},
	}).ServeHTTP(rw, r)
}

func (w *Worker) writeLease(ctx context.Context) error {
	w.leaseMu.Lock()
	defer w.leaseMu.Unlock()
	lease := records.Lease{Addr: w.addr, Capacity: len(w.slots), Hosted: []string{}, Expires: time.Now().Add(30 * time.Second)}
	w.mu.Lock()
	for _, h := range w.slots {
		if h != nil {
			lease.Hosted = append(lease.Hosted, h.key())
		}
	}
	w.mu.Unlock()
	gen, err := w.store.Put(ctx, records.LeaseKey(w.id), lease, w.leaseGen)
	if err == nil {
		w.leaseGen = gen
	}
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// mustExecutable resolves this process's own binary path, for copying
// into each actor's bundle at /.ate/ate-ish.
func mustExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return "/proc/self/exe"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

// ingressBridge is the node-side half of round 3's dial-out ingress
// (docs/ROOTLESS-SPIKE.md): it listens on a unix socket the sandbox's
// own outforwarder dials out to (never one the sandbox binds), pools the
// connections that arrive, and replays each HTTP request over one of
// them, exactly as the spike's nodebridge did.
type ingressBridge struct {
	sockPath string
	ln       net.Listener
	pool     chan net.Conn
}

func newIngressBridge(sockPath string) *ingressBridge {
	return &ingressBridge{sockPath: sockPath, pool: make(chan net.Conn, 8)}
}

func (b *ingressBridge) Listen() error {
	os.Remove(b.sockPath)
	ln, err := net.Listen("unix", b.sockPath)
	if err != nil {
		return err
	}
	b.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.pool <- c
		}
	}()
	return nil
}

func (b *ingressBridge) Close() {
	if b.ln != nil {
		b.ln.Close()
	}
	os.Remove(b.sockPath)
}

// RoundTrip implements http.RoundTripper over the pooled connections,
// for httputil.ReverseProxy.
func (b *ingressBridge) RoundTrip(req *http.Request) (*http.Response, error) {
	c, err := b.take(req.Context(), 10*time.Second)
	if err != nil {
		return nil, err
	}
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		c.Close()
		return nil, err
	}
	resp.Body = closeOnBodyClose{resp.Body, c}
	return resp, nil
}

func (b *ingressBridge) take(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	select {
	case c := <-b.pool:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, fmt.Errorf("ingress bridge %s: no connection from the sandbox's outforwarder within %s", b.sockPath, timeout)
	}
}

// WaitReady polls path through the bridge until it answers 200 or
// timeout elapses.
func (b *ingressBridge) WaitReady(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://actor"+path, nil)
		resp, err := b.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("wakeup probe: status %s", resp.Status)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("wakeup probe %s not ready after %s: %w", path, timeout, lastErr)
}

type closeOnBodyClose struct {
	io.ReadCloser
	extra net.Conn
}

func (c closeOnBodyClose) Close() error { c.extra.Close(); return c.ReadCloser.Close() }
