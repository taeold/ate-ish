package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/taeold/ate-ish/internal/devatelet"
	"github.com/taeold/ate-ish/internal/devgateway"
	"github.com/taeold/ate-ish/third_party/substrate/exported/installdefaults"
	"github.com/taeold/ate-ish/third_party/substrate/exported/nodepath"
	"github.com/taeold/ate-ish/third_party/substrate/exported/proto/ateworkerpb"
	"google.golang.org/grpc"
)

var (
	errFull      = errors.New("worker has no free slot")
	errStopping  = errors.New("actor is stopping on this worker")
	errNotHosted = errors.New("actor is not running on this worker")
)

// actorVethIP and actorVethUp are the sandbox's fixed address and the
// gateway-namespace-name suffix internal/ateworker/actornet gives every
// actor (net.go's ActorVethIP, sandbox.go's SandboxGatewayNetNSName). Both
// are stable parts of the vendored worker's own contract, so ate-ish names
// them directly rather than importing actornet just for two strings.
const (
	actorVethIP       = "169.254.17.2"
	gatewayNSSuffix   = "-at"
	runscReleaseDir   = "bin/runsc"
	workerBinaryInBin = "bin/ateworker-gvisor"
	workerKodataPath  = "third_party/substrate/cmd/ateworker-gvisor/kodata"
)

// worker is ate-ish acting as both atelet and worker-pool node: it runs the
// vendored ateworker-gvisor as a subprocess, serves it the one atelet RPC
// surface it needs (internal/devatelet), fronts its own egress gateway
// (internal/devgateway), and drives actor lifecycle over ateworkerpb.
type worker struct {
	id     string
	addr   string
	dir    string // base dir for actor state, e.g. /var/lib/ate-ish
	bucket *bucket

	pki        *devatelet.PKI
	ateletSrv  *grpc.Server
	gateway    *devgateway.Gateway
	workerPID  int
	workerConn *grpc.ClientConn
	client     ateworkerpb.WorkerClient

	mu    sync.Mutex
	slots []*hosted

	dirty    atomic.Bool
	leaseMu  sync.Mutex
	leaseGen int64
}

type hosted struct {
	as, name string
	gen      int64
	slot     int
	actorUID string
	dirs     *actorDirs
	tmpl     Template
	cname    string // the one container's name
	port     int    // its wakeup-probe / listen port, on actorVethIP

	ready    chan struct{}
	err      error
	busy     int
	lastUsed time.Time
	stopping bool
}

func (h *hosted) key() string { return h.as + "/" + h.name }

// addr is the address the ingress (via worker.proxy) and the wakeup-probe
// poller reach the actor on: its own veth address and container port,
// reachable only from inside its gateway network namespace (see nsdial.go).
func (h *hosted) addr() string { return fmt.Sprintf("%s:%d", actorVethIP, h.port) }

func newWorker(ctx context.Context, b *bucket, addr string, capacity int, dir string) (*worker, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ateletSPIFFE := installdefaults.AteletSPIFFEID(installdefaults.SystemNamespace)
	pki, err := devatelet.GeneratePKI(filepath.Join(dir, "pki"), ateletSPIFFE)
	if err != nil {
		return nil, fmt.Errorf("generating node PKI: %w", err)
	}
	ateletSrv, err := devatelet.ServeFakeAtelet(ctx, nodepath.WorkerSupportSocket, pki, actorPolicyLookup(b))
	if err != nil {
		return nil, fmt.Errorf("serving the atelet credential broker: %w", err)
	}
	gwCert, err := pki.GatewayCertificate()
	if err != nil {
		return nil, fmt.Errorf("loading the gateway's own credential: %w", err)
	}
	gateway, err := devgateway.Start("127.0.0.1:0", gwCert)
	if err != nil {
		return nil, fmt.Errorf("starting the egress gateway: %w", err)
	}

	podUID := "ate-ish-" + randomHex(4)
	cmd, err := startAteworker(ateworkerConfig{
		binary:      workerBinaryInBin,
		kodata:      workerKodataPath,
		podUID:      podUID,
		maxActors:   capacity,
		pki:         pki,
		readyPort:   28080,
		atunnelPort: 28443,
		connectPort: 28444,
		logFile:     filepath.Join(dir, "ateworker-gvisor.log"),
	})
	if err != nil {
		return nil, err
	}
	sockPath := nodepath.WorkerSocketPath(podUID)
	if err := waitForSocket(sockPath, 30*time.Second); err != nil {
		return nil, fmt.Errorf("waiting for ateworker-gvisor's socket: %w", err)
	}
	conn, err := dialAteworker(sockPath)
	if err != nil {
		return nil, err
	}

	w := &worker{
		id: "w-" + randomHex(4), addr: addr, dir: dir, bucket: b,
		pki: pki, ateletSrv: ateletSrv, gateway: gateway,
		workerPID: cmd.Process.Pid, workerConn: conn, client: ateworkerpb.NewWorkerClient(conn),
		slots: make([]*hosted, capacity),
	}
	if err := w.writeLease(ctx); err != nil {
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
	slog.Info("worker up", "id", w.id, "addr", addr, "capacity", capacity, "worker_pid", w.workerPID, "egress_gateway", gateway.Addr)
	return w, nil
}

func (w *worker) routes(mux *http.ServeMux) {
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
		reply(rw, map[string]int64{"generation": gen}, err)
	})
	mux.HandleFunc("POST /suspend", func(rw http.ResponseWriter, r *http.Request) {
		var req struct{ As, Actor string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		reply(rw, nil, w.suspend(context.WithoutCancel(r.Context()), req.As+"/"+req.Actor))
	})
	mux.HandleFunc("POST /reap", func(rw http.ResponseWriter, r *http.Request) {
		idle, _ := strconv.Atoi(r.URL.Query().Get("idleSeconds"))
		reply(rw, w.reap(context.WithoutCancel(r.Context()), time.Duration(idle)*time.Second), nil)
	})
	mux.HandleFunc("/actors/{as}/{actor}/{path...}", w.proxy)
}

func (w *worker) find(key string) *hosted {
	for _, h := range w.slots {
		if h != nil && h.key() == key {
			return h
		}
	}
	return nil
}

// resume runs an actor in a free slot and returns the generation it runs.
func (w *worker) resume(ctx context.Context, as, name string, recordGen int64) (int64, error) {
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
		return 0, errFull
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

func (w *worker) current(ctx context.Context, h *hosted) error {
	w.mu.Lock()
	stopping := h.stopping
	w.mu.Unlock()
	if stopping {
		return fmt.Errorf("%w: %s", errStopping, h.key())
	}
	var rec Actor
	if _, err := w.bucket.get(ctx, actorKey(h.as, h.name), &rec); err != nil {
		return err
	}
	if rec.Owner != w.id || rec.Generation != h.gen {
		go w.suspend(context.Background(), h.key())
		return fmt.Errorf("%w: the record of %s names generation %d on %s, this copy is %d",
			errConflict, h.key(), rec.Generation, rec.Owner, h.gen)
	}
	return nil
}

// start claims the actor with a conditional write of its record, then boots
// it. If the boot fails, the record goes back to suspended.
func (w *worker) start(ctx context.Context, h *hosted, recordGen int64) error {
	key := actorKey(h.as, h.name)
	var rec Actor
	gen, err := w.bucket.get(ctx, key, &rec)
	if err != nil {
		return err
	}
	if gen != recordGen {
		return fmt.Errorf("%w: the record of %s changed", errConflict, h.key())
	}
	rec.Status, rec.Owner, rec.Generation = running, w.id, rec.Generation+1
	if _, err = w.bucket.put(ctx, key, rec, gen); err != nil {
		return err
	}
	h.gen = rec.Generation
	if err := w.boot(ctx, h, rec); err != nil {
		return errors.Join(err, w.release(ctx, h))
	}
	return nil
}

func (w *worker) release(ctx context.Context, h *hosted) error {
	var err error
	for range 5 {
		var rec Actor
		var gen int64
		if gen, err = w.bucket.get(ctx, actorKey(h.as, h.name), &rec); err != nil {
			return err
		}
		if rec.Owner != w.id || rec.Generation != h.gen {
			return nil
		}
		rec.Status, rec.Owner = suspended, ""
		_, err = w.bucket.put(ctx, actorKey(h.as, h.name), rec, gen)
		if !errors.Is(err, errConflict) && !errors.Is(err, errRateLimited) {
			return err
		}
		time.Sleep(time.Second)
	}
	return err
}

// boot builds the actor's dirs and OCI bundle, then RunWorkload (a fresh
// actor) or RestoreWorkload (a snapshot downloaded from the bucket).
func (w *worker) boot(ctx context.Context, h *hosted, rec Actor) error {
	t0 := time.Now()
	var tmpl Template
	if _, err := w.bucket.get(ctx, templateKey(h.as, rec.Template), &tmpl); err != nil {
		return fmt.Errorf("template %s: %w", rec.Template, err)
	}
	if len(tmpl.Containers) != 1 {
		return fmt.Errorf("template %s: exactly one container is supported, got %d", rec.Template, len(tmpl.Containers))
	}
	h.tmpl = tmpl
	h.cname = containerName(tmpl.Containers[0])
	h.port = tmpl.Containers[0].WakeupProbe.HTTPGet.Port
	if h.port == 0 {
		h.port = 80
	}

	spec, err := buildSpec(tmpl)
	if err != nil {
		return err
	}
	dirs := buildActorDirs(w.dir, h.actorUID)
	h.dirs = dirs
	durableNames, systemInfoNames := durableVolumeNames(tmpl)
	if err := prepareActorDirs(dirs, durableNames, systemInfoNames); err != nil {
		return err
	}
	if err := writeSystemInfoFiles(dirs, tmpl, w.pki.GatewayCAPEM()); err != nil {
		return err
	}
	if err := stageRunsc(runscReleaseDir, dirs); err != nil {
		return fmt.Errorf("staging runsc: %w", err)
	}
	if err := mountBundleRootfs(w.workerPID, dirs, h.cname); err != nil {
		return fmt.Errorf("mounting bundle rootfs: %w", err)
	}

	egress := &ateworkerpb.EgressGateway{Address: w.gateway.Addr}
	snapshotTar := filepath.Join(dirs.pb.GetRootDir(), "snapshot.tar.gz")
	fidelity := fidelityOf(tmpl)
	var bootErr error
	switch err := w.bucket.downloadFile(ctx, snapshotKey(h.as, h.name), snapshotTar); {
	case err == nil:
		defer os.Remove(snapshotTar)
		if err := untarDir(snapshotTar, dirs.pb.GetCheckpointDir()); err != nil {
			return fmt.Errorf("extracting snapshot: %w", err)
		}
		bootErr = callWithRetry(ctx, "RestoreWorkload", func(ctx context.Context) error {
			_, err := w.client.RestoreWorkload(ctx, &ateworkerpb.RestoreWorkloadRequest{
				Atespace: h.as, ActorName: h.name, ActorUid: h.actorUID,
				RunscPath: dirs.sandboxRunscPath, Spec: spec, ActorDirs: dirs.pb,
				Fidelity: fidelity, EgressGateway: egress,
			})
			return err
		})
	case errors.Is(err, errNotFound):
		bootErr = callWithRetry(ctx, "RunWorkload", func(ctx context.Context) error {
			_, err := w.client.RunWorkload(ctx, &ateworkerpb.RunWorkloadRequest{
				Atespace: h.as, ActorName: h.name, ActorUid: h.actorUID,
				RunscPath: dirs.sandboxRunscPath, Spec: spec, ActorDirs: dirs.pb,
				EgressGateway: egress,
			})
			return err
		})
	default:
		return fmt.Errorf("downloading snapshot: %w", err)
	}
	downloaded := time.Since(t0)
	if bootErr != nil {
		unmountBundleRootfs(w.workerPID, dirs, h.cname)
		return bootErr
	}

	path := tmpl.Containers[0].WakeupProbe.HTTPGet.Path
	timeout := time.Duration(max(tmpl.Containers[0].WakeupProbe.TimeoutSeconds, 30)) * time.Second
	if err := waitReady(ctx, w.workerPID, h, path, timeout); err != nil {
		w.terminateWorkload(context.Background(), h, spec)
		unmountBundleRootfs(w.workerPID, dirs, h.cname)
		return err
	}
	slog.Info("actor started", "actor", h.key(), "generation", h.gen, "actor_uid", h.actorUID, "slot", h.slot,
		"fidelity", fidelity, "snapshot_ms", downloaded.Milliseconds(), "boot_ms", (time.Since(t0) - downloaded).Milliseconds())
	return nil
}

// waitReady polls the wakeup probe through the actor's own veth address,
// dialed from inside its gateway network namespace (nsdial.go): that
// address is not routable from ate-ish's own network namespace any other
// way.
func waitReady(ctx context.Context, workerPID int, h *hosted, path string, timeout time.Duration) error {
	dial := dialViaActorGateway(ctx, workerPID, h.actorUID)
	client := &http.Client{Transport: &http.Transport{DialContext: dial}}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + h.addr() + path)
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

func (w *worker) terminateWorkload(ctx context.Context, h *hosted, spec *ateworkerpb.WorkloadSpec) {
	err := callWithRetry(ctx, "TerminateWorkload", func(ctx context.Context) error {
		_, err := w.client.TerminateWorkload(ctx, &ateworkerpb.TerminateWorkloadRequest{
			Atespace: h.as, ActorName: h.name, ActorUid: h.actorUID,
			RunscPath: h.dirs.sandboxRunscPath, Spec: spec, ActorDirs: h.dirs.pb,
		})
		return err
	})
	if err != nil {
		slog.Warn("TerminateWorkload", "actor", h.key(), "err", err)
	}
}

// suspend checkpoints an actor, uploads the checkpoint as the snapshot, and
// marks the record suspended. If any step fails, the actor keeps its sandbox
// and running record, so a later suspend can try again.
func (w *worker) suspend(ctx context.Context, key string) error {
	t0 := time.Now()
	w.mu.Lock()
	h := w.find(key)
	if h == nil {
		w.mu.Unlock()
		return errNotHosted
	}
	if h.stopping {
		w.mu.Unlock()
		return fmt.Errorf("%w: %s", errStopping, key)
	}
	h.stopping = true
	w.mu.Unlock()
	<-h.ready
	if h.err != nil {
		return errNotHosted
	}

	spec, err := buildSpec(h.tmpl)
	if err != nil {
		return err
	}
	fidelity := fidelityOf(h.tmpl)
	err = callWithRetry(ctx, "CheckpointWorkload", func(ctx context.Context) error {
		_, err := w.client.CheckpointWorkload(ctx, &ateworkerpb.CheckpointWorkloadRequest{
			Atespace: h.as, ActorName: h.name, ActorUid: h.actorUID,
			RunscPath: h.dirs.sandboxRunscPath, Spec: spec, ActorDirs: h.dirs.pb, Fidelity: fidelity,
		})
		return err
	})
	checkpointed := time.Since(t0)

	snapshotTar := filepath.Join(h.dirs.pb.GetRootDir(), "snapshot-out.tar.gz")
	if err == nil {
		err = tarDir(h.dirs.pb.GetCheckpointDir(), snapshotTar)
	}
	var rec Actor
	if err == nil {
		_, err = w.bucket.get(ctx, actorKey(h.as, h.name), &rec)
	}
	if err == nil && rec.Owner == w.id && rec.Generation == h.gen {
		err = w.bucket.uploadFile(ctx, snapshotKey(h.as, h.name), snapshotTar)
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

	w.terminateWorkload(ctx, h, spec)
	unmountBundleRootfs(w.workerPID, h.dirs, h.cname)
	os.RemoveAll(h.dirs.pb.GetRootDir())
	os.RemoveAll(h.dirs.sandboxAssetsDir)
	err = w.release(ctx, h)
	w.mu.Lock()
	w.slots[h.slot] = nil
	w.mu.Unlock()
	w.dirty.Store(true)
	slog.Info("actor suspended", "actor", key, "generation", h.gen, "checkpoint_ms", checkpointed.Milliseconds(),
		"upload_ms", (uploaded - checkpointed).Milliseconds(), "total_ms", time.Since(t0).Milliseconds(), "bytes", size, "err", err)
	return err
}

func fileSize(file string) int64 {
	st, err := os.Stat(file)
	if err != nil {
		return 0
	}
	return st.Size()
}

func (w *worker) reap(ctx context.Context, idle time.Duration) []string {
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

// suspendAll runs on SIGTERM.
func (w *worker) suspendAll(ctx context.Context) {
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
	slog.Info("lease deleted", "err", w.bucket.delete(ctx, leaseKey(w.id)))
	if w.gateway != nil {
		w.gateway.Close()
	}
	if w.ateletSrv != nil {
		w.ateletSrv.GracefulStop()
	}
	if w.workerConn != nil {
		w.workerConn.Close()
	}
	if w.workerPID > 0 {
		// ateworker-gvisor has its own graceful-shutdown path (it stops
		// cleanly, since every actor was already suspended above); give it
		// a few seconds before this process exits out from under it.
		syscall.Kill(w.workerPID, syscall.SIGTERM)
		for range 50 {
			if syscall.Kill(w.workerPID, 0) != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// proxy forwards an ingress request to the actor, dialing its veth address
// from inside its gateway network namespace.
func (w *worker) proxy(rw http.ResponseWriter, r *http.Request) {
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
	target := &url.URL{Scheme: "http", Host: h.addr()}
	transport := &http.Transport{DialContext: dialViaActorGateway(r.Context(), w.workerPID, h.actorUID)}
	(&httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/" + r.PathValue("path")
			pr.Out.Header.Del("ate-actor-generation")
		},
	}).ServeHTTP(rw, r)
}

func (w *worker) writeLease(ctx context.Context) error {
	w.leaseMu.Lock()
	defer w.leaseMu.Unlock()
	lease := Lease{Addr: w.addr, Capacity: len(w.slots), Hosted: []string{}, Expires: time.Now().Add(30 * time.Second)}
	w.mu.Lock()
	for _, h := range w.slots {
		if h != nil {
			lease.Hosted = append(lease.Hosted, h.key())
		}
	}
	w.mu.Unlock()
	gen, err := w.bucket.put(ctx, leaseKey(w.id), lease, w.leaseGen)
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

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func reply(rw http.ResponseWriter, v any, err error) {
	switch {
	case errors.Is(err, errFull), errors.Is(err, errStopping), errors.Is(err, errNoRoom):
		http.Error(rw, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, errNotHosted), errors.Is(err, errConflict), errors.Is(err, errExists):
		http.Error(rw, err.Error(), http.StatusConflict)
	case errors.Is(err, errNotFound):
		http.Error(rw, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(rw, err.Error(), http.StatusInternalServerError)
	default:
		rw.Header().Set("content-type", "application/json")
		json.NewEncoder(rw).Encode(v)
	}
}
