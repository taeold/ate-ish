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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errFull      = errors.New("worker has no free slot")
	errStopping  = errors.New("actor is stopping on this worker")
	errNotHosted = errors.New("actor is not running on this worker")
)

// worker runs actors as sandboxes. Slot i of capacity serves the actor on
// host port 10000+i and its egress proxy on 11000+i.
type worker struct {
	id     string
	addr   string
	hostIP string // the worker's address as actors see it
	dir    string
	exe    string // this binary; the sandbox sees the same path, so it runs as init
	bucket *bucket
	ca     *egressCA
	secret *secrets

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
	sandbox  string
	ready    chan struct{} // closed once started, or failed with err
	err      error
	egress   *http.Server
	busy     int
	lastUsed time.Time
	stopping bool
}

func (h *hosted) key() string { return h.as + "/" + h.name }
func appPort(slot int) int    { return 10000 + slot }

func newWorker(ctx context.Context, b *bucket, addr string, capacity int) (*worker, error) {
	ca, err := newEgressCA()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	w := &worker{
		id: "w-" + randomHex(4), addr: addr, hostIP: env("ATE_HOST_IP", "169.254.8.1"), dir: "/tmp/ate", exe: exe,
		bucket: b, ca: ca, secret: &secrets{client: b.client, cache: map[string]string{}},
		slots: make([]*hosted, capacity),
	}
	for _, d := range []string{"actors", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(w.dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	if err := w.writeLease(ctx); err != nil {
		return nil, err
	}
	// GCS takes about one write per second to an object, so changes mark
	// the lease dirty and this loop writes it at most once a second.
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
// A full worker refuses before it writes anything. A second call for an actor
// the worker already runs waits for the first, and succeeds only if that copy
// is still the one the record names.
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
	h = &hosted{as: as, name: name, slot: slot, sandbox: "a-" + randomHex(6), ready: make(chan struct{}), lastUsed: time.Now()}
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

// current refuses a copy that is stopping or that the record no longer names,
// and stops a copy the record no longer names.
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

// release marks the record suspended if it still names this copy. It re-reads
// on 412 and waits out 429, so the record does not stay running on a worker
// that no longer hosts the actor.
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

func (w *worker) boot(ctx context.Context, h *hosted, rec Actor) error {
	t0 := time.Now()
	var tmpl Template
	if _, err := w.bucket.get(ctx, templateKey(h.as, rec.Template), &tmpl); err != nil {
		return fmt.Errorf("template %s: %w", rec.Template, err)
	}
	c := tmpl.Containers[0]
	dir := filepath.Join(w.dir, "actors", h.sandbox)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// No --rootfs: the actor's root is this container's own filesystem, read
	// only, under a writable overlay.
	args := []string{"run", h.sandbox, "--detach", "--write", "--allow-egress",
		"-p", fmt.Sprintf("%d:%d", appPort(h.slot), c.WakeupProbe.HTTPGet.Port),
		"--mount", "type=bind,source=" + dir + ",destination=" + actorDir}

	spec := initSpec{Command: slices.Concat(c.Command, c.Args)}
	for _, m := range c.VolumeMounts {
		i := slices.IndexFunc(tmpl.Volumes, func(v Volume) bool { return v.Name == m.Name })
		if i < 0 {
			return fmt.Errorf("volumeMount %q has no volume", m.Name)
		}
		switch v := tmpl.Volumes[i]; {
		case v.DurableDir != nil:
			spec.Dirs = append(spec.Dirs, m.MountPath)
		case v.SystemInfo != nil:
			src := filepath.Join(dir, "volume-"+v.Name)
			if err := os.MkdirAll(src, 0o755); err != nil {
				return err
			}
			for _, ds := range v.SystemInfo.DataSources {
				if ds.TrustBundle != nil {
					if err := os.WriteFile(filepath.Join(src, ds.TrustBundle.Path), w.ca.pem, 0o644); err != nil {
						return err
					}
				}
			}
			args = append(args, "--mount", "type=bind,source="+src+",destination="+m.MountPath)
		}
	}

	proxy := fmt.Sprintf("http://%s:%d", w.hostIP, 11000+h.slot)
	spec.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	for _, e := range c.Env {
		spec.Env = append(spec.Env, e.Name+"="+e.Value)
	}
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		spec.Env = append(spec.Env, name+"="+proxy)
	}
	spec.Env = append(spec.Env, "NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1", "NODE_USE_ENV_PROXY=1")
	data, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(dir, "spec.json"), data, 0o600); err != nil {
		return err
	}

	tar := filepath.Join(w.dir, "snapshots", h.sandbox+".tar")
	switch err := w.bucket.downloadFile(ctx, snapshotKey(h.as, h.name), tar); {
	case err == nil:
		args = append(args, "--import-tar", tar)
		defer os.Remove(tar)
	case !errors.Is(err, errNotFound):
		return err
	}
	downloaded := time.Since(t0)

	p := &egressProxy{actor: h.key(), policy: rec.EgressPolicy, ca: w.ca, secrets: w.secret}
	var err error
	if h.egress, err = p.listen(fmt.Sprintf("%s:%d", w.hostIP, 11000+h.slot)); err != nil {
		return err
	}
	if err := sandbox(ctx, append(args, "--", w.exe, "init")...); err != nil {
		h.egress.Close()
		return err
	}
	if err := waitReady(ctx, h, dir, c.WakeupProbe.HTTPGet.Path, time.Duration(max(c.WakeupProbe.TimeoutSeconds, 30))*time.Second); err != nil {
		sandbox(context.Background(), "delete", h.sandbox, "--force")
		h.egress.Close()
		return err
	}
	slog.Info("actor started", "actor", h.key(), "generation", h.gen, "sandbox", h.sandbox, "slot", h.slot,
		"snapshot_ms", downloaded.Milliseconds(),
		"boot_ms", (time.Since(t0) - downloaded).Milliseconds(), "restored", slices.Contains(args, "--import-tar"))
	return nil
}

// waitReady polls the wakeup probe and fails early if the actor exits.
func waitReady(ctx context.Context, h *hosted, dir, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", appPort(h.slot), path))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if code, err := os.ReadFile(filepath.Join(dir, "exit")); err == nil {
			return fmt.Errorf("actor exited with %s: %s", strings.TrimSpace(string(code)), tail(filepath.Join(dir, "log")))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("wakeup probe %s not ready after %s: %s", path, timeout, tail(filepath.Join(dir, "log")))
}

func tail(file string) string {
	data, _ := os.ReadFile(file)
	return string(data[max(0, len(data)-2000):])
}

// suspend stops an actor, uploads its rootfs as the snapshot, and marks the
// record suspended. If the stop, tar or upload fails, the actor keeps its
// sandbox and its running record, so a later suspend can try again.
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

	dir := filepath.Join(w.dir, "actors", h.sandbox)
	tar := filepath.Join(w.dir, "snapshots", h.sandbox+".tar")
	err := sandbox(ctx, "exec", h.sandbox, "--", w.exe, "stop")
	if err == nil {
		err = waitExit(dir, 8*time.Second)
	}
	stopped := time.Since(t0)
	if err == nil {
		err = sandbox(ctx, "tar", h.sandbox, "--file", tar)
	}
	tarred := time.Since(t0)
	// Upload only while the record still names this copy, so a superseded
	// copy never overwrites the snapshot of the new one.
	var rec Actor
	if err == nil {
		_, err = w.bucket.get(ctx, actorKey(h.as, h.name), &rec)
	}
	if err == nil && rec.Owner == w.id && rec.Generation == h.gen {
		err = w.bucket.uploadFile(ctx, snapshotKey(h.as, h.name), tar)
	}
	uploaded := time.Since(t0)
	size := fileSize(tar)
	os.Remove(tar)
	if err != nil {
		w.mu.Lock()
		h.stopping = false
		w.mu.Unlock()
		slog.Error("suspend failed, actor kept", "actor", key, "generation", h.gen, "err", err)
		return err
	}
	err = errors.Join(w.release(ctx, h), sandbox(ctx, "delete", h.sandbox, "--force"), h.egress.Close())
	os.RemoveAll(dir)
	w.mu.Lock()
	w.slots[h.slot] = nil
	w.mu.Unlock()
	w.dirty.Store(true)
	slog.Info("actor suspended", "actor", key, "generation", h.gen, "stop_ms", stopped.Milliseconds(),
		"tar_ms", (tarred - stopped).Milliseconds(), "upload_ms", (uploaded - tarred).Milliseconds(),
		"total_ms", time.Since(t0).Milliseconds(), "bytes", size, "err", err)
	return err
}

func waitExit(dir string, timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
			return nil
		}
	}
	return fmt.Errorf("actor did not exit %s after SIGTERM", timeout)
}

func fileSize(file string) int64 {
	st, err := os.Stat(file)
	if err != nil {
		return 0
	}
	return st.Size()
}

// reap suspends actors that have served no request for idle.
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

// suspendAll runs on SIGTERM: Cloud Run gives the container 10 s.
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
}

// proxy forwards an ingress request to the actor's published port.
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
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", appPort(h.slot))}
	(&httputil.ReverseProxy{
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

// reply writes v as JSON, or err with a status that names its kind.
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
