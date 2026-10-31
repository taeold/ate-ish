// Package ingress routes requests by the ate-target-actor header and
// serves the admin API the CLI calls.
package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/taeold/ate-ish/internal/apierr"
	"github.com/taeold/ate-ish/internal/records"
	"github.com/taeold/ate-ish/internal/store"
)

// Ingress keeps no state beyond in-flight placements.
type Ingress struct {
	store   store.Store
	mu      sync.Mutex
	placing map[string]*placement
}

// New returns an Ingress backed by b.
func New(b store.Store) *Ingress {
	return &Ingress{store: b, placing: map[string]*placement{}}
}

type placement struct {
	done   chan struct{}
	actor  records.Actor
	lease  *records.Lease
	parked time.Duration
	err    error
}

// Route forwards a request to the actor named by its ate-target-actor
// header, placing the actor first if it is not already running.
func (in *Ingress) Route(rw http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	as, name, _ := strings.Cut(r.Header.Get("ate-target-actor"), "/")
	p := in.locate(r.Context(), as, name)
	if p.err != nil {
		apierr.Reply(rw, nil, fmt.Errorf("actor %s/%s: %w", as, name, p.err))
		return
	}
	placed := time.Since(t0)
	rw.Header().Set("ate-worker", p.actor.Owner)
	rw.Header().Set("ate-resume-ms", strconv.FormatInt(placed.Milliseconds(), 10))
	rw.Header().Set("ate-parked-ms", strconv.FormatInt(p.parked.Milliseconds(), 10))
	target := &url.URL{Scheme: "http", Host: p.lease.Addr}
	(&httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/actors/" + as + "/" + name + r.URL.Path
			pr.Out.Header.Del("ate-target-actor")
			pr.Out.Header.Set("ate-actor-generation", strconv.FormatInt(p.actor.Generation, 10))
		},
	}).ServeHTTP(rw, r)
	slog.Info("routed", "actor", as+"/"+name, "worker", p.actor.Owner, "resume_ms", placed.Milliseconds(),
		"parked_ms", p.parked.Milliseconds(), "total_ms", time.Since(t0).Milliseconds())
}

// locate returns where the actor runs, placing it if it is not running on a
// live worker. Concurrent requests for one actor share one placement.
func (in *Ingress) locate(ctx context.Context, as, name string) *placement {
	var rec records.Actor
	if _, err := in.store.Get(ctx, records.ActorKey(as, name), &rec); err != nil {
		return &placement{err: err}
	}
	key := as + "/" + name
	if rec.Status == records.Running {
		lease, err := in.lease(ctx, rec.Owner)
		if err != nil || lease.Hosts(key) {
			return &placement{actor: rec, lease: lease, err: err}
		}
	}
	in.mu.Lock()
	p, ok := in.placing[key]
	if !ok {
		p = &placement{done: make(chan struct{})}
		in.placing[key] = p
		go func() {
			p.err = in.place(context.WithoutCancel(ctx), as, name, p)
			in.mu.Lock()
			delete(in.placing, key)
			in.mu.Unlock()
			close(p.done)
		}()
	}
	in.mu.Unlock()
	select {
	case <-p.done:
		return p
	case <-ctx.Done():
		return &placement{err: ctx.Err()}
	}
}

// place asks live workers with room, fewest actors first, to resume the
// actor. The worker that has a free slot claims the record. With no room it
// retries for 5 s. A running record whose owner's lease does not list the
// actor counts as suspended: the owner is asked first, and either still runs
// that copy or claims the actor again.
func (in *Ingress) place(ctx context.Context, as, name string, p *placement) error {
	start := time.Now()
	var parkedSince time.Time
	for {
		var rec records.Actor
		gen, err := in.store.Get(ctx, records.ActorKey(as, name), &rec)
		if err != nil {
			return err
		}
		if rec.Status == records.Running {
			lease, err := in.lease(ctx, rec.Owner)
			if err != nil || lease.Hosts(as+"/"+name) {
				p.actor, p.lease = rec, lease
				return err
			}
		}
		leases, err := in.leases(ctx)
		if err != nil {
			return err
		}
		ids := slices.Collect(maps.Keys(leases))
		slices.SortFunc(ids, func(a, b string) int { return len(leases[a].Hosted) - len(leases[b].Hosted) })
		if i := slices.Index(ids, rec.Owner); i > 0 {
			ids = slices.Insert(slices.Delete(ids, i, i+1), 0, rec.Owner)
		}
		conflict := false
		for _, id := range ids {
			l := leases[id]
			if !l.Live() || len(l.Hosted) >= l.Capacity {
				continue
			}
			var out struct{ Generation int64 }
			asked := time.Now()
			err := in.call(ctx, l.Addr, "/resume", map[string]any{"as": as, "actor": name, "recordGeneration": gen}, &out)
			if err == nil {
				if !parkedSince.IsZero() {
					p.parked = asked.Sub(parkedSince)
				}
				rec.Status, rec.Owner, rec.Generation = records.Running, id, out.Generation
				p.actor, p.lease = rec, l
				return nil
			}
			if conflict = errors.Is(err, store.ErrConflict); conflict {
				break
			}
			if !errors.Is(err, apierr.ErrFull) {
				return err
			}
		}
		if conflict {
			continue
		}
		if parkedSince.IsZero() {
			parkedSince = time.Now()
		}
		if time.Since(start) > 5*time.Second {
			return apierr.ErrNoRoom
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// lease reads a worker's lease; a missing one is nil.
func (in *Ingress) lease(ctx context.Context, id string) (*records.Lease, error) {
	var l records.Lease
	_, err := in.store.Get(ctx, records.LeaseKey(id), &l)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return &l, err
}

func (in *Ingress) leases(ctx context.Context) (map[string]*records.Lease, error) {
	names, err := in.store.List(ctx, "workers/")
	if err != nil {
		return nil, err
	}
	leases := map[string]*records.Lease{}
	for _, n := range names {
		id := strings.TrimSuffix(strings.TrimPrefix(n, "workers/"), ".json")
		l, err := in.lease(ctx, id)
		if err != nil {
			return nil, err
		}
		if l != nil {
			leases[id] = l
		}
	}
	return leases, nil
}

// call POSTs JSON to a worker. A 503 means the worker is full, a 409 that the
// actor record changed.
func (in *Ingress) call(ctx context.Context, addr, path string, body, out any) error {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://"+addr+path, bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return fmt.Errorf("%w: %s", apierr.ErrFull, msg)
	case resp.StatusCode == http.StatusConflict:
		return fmt.Errorf("%w: %s", store.ErrConflict, msg)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("worker %s%s: %s: %s", addr, path, resp.Status, bytes.TrimSpace(msg))
	case out != nil:
		return json.Unmarshal(msg, out)
	}
	return nil
}

func (in *Ingress) suspend(ctx context.Context, as, name string) error {
	var rec records.Actor
	gen, err := in.store.Get(ctx, records.ActorKey(as, name), &rec)
	if err != nil || rec.Status == records.Suspended {
		return err
	}
	lease, err := in.lease(ctx, rec.Owner)
	if err != nil {
		return err
	}
	if lease.Live() {
		err := in.call(ctx, lease.Addr, "/suspend", map[string]string{"as": as, "actor": name}, nil)
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	// The owner is gone or does not host the actor: the last snapshot is what remains.
	rec.Status, rec.Owner = records.Suspended, ""
	_, err = in.store.Put(ctx, records.ActorKey(as, name), rec, gen)
	return err
}

// Routes registers the ate-target-actor-routed admin API on mux.
func (in *Ingress) Routes(mux *http.ServeMux) {
	b := in.store
	mux.HandleFunc("PUT /admin/atespaces/{as}", func(rw http.ResponseWriter, r *http.Request) {
		_, err := b.Put(r.Context(), records.AtespaceKey(r.PathValue("as")), map[string]time.Time{"created": time.Now()}, 0)
		apierr.Reply(rw, map[string]string{"name": r.PathValue("as")}, apierr.ExistsErr(err))
	})
	mux.HandleFunc("PUT /admin/atespaces/{as}/templates/{t}", func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var t records.Template
		if err := json.Unmarshal(raw, &t); err != nil || len(t.Containers) != 1 || len(t.Containers[0].Command) == 0 {
			http.Error(rw, fmt.Sprintf("a template needs exactly one container with a command: %v", err), http.StatusBadRequest)
			return
		}
		_, err := b.Put(r.Context(), records.TemplateKey(r.PathValue("as"), r.PathValue("t")), json.RawMessage(raw), 0)
		apierr.Reply(rw, t.Metadata, apierr.ExistsErr(err))
	})
	mux.HandleFunc("PUT /admin/atespaces/{as}/actors/{a}", func(rw http.ResponseWriter, r *http.Request) {
		var req struct{ Template, Tag string }
		json.NewDecoder(r.Body).Decode(&req)
		as, a := r.PathValue("as"), r.PathValue("a")
		rec := records.Actor{Template: req.Template, Status: records.Suspended, Created: time.Now()}
		_, err := b.Get(r.Context(), records.TemplateKey(as, req.Template), &records.Template{})
		if err == nil {
			_, err = b.Put(r.Context(), records.ActorKey(as, a), rec, 0)
			err = apierr.ExistsErr(err)
		}
		// The tag becomes the new actor's snapshot only after its record exists,
		// so an existing actor's snapshot is never overwritten.
		if err == nil && req.Tag != "" {
			if err = b.Copy(r.Context(), records.TagKey(as, req.Tag), records.SnapshotKey(as, a)); err != nil {
				b.Delete(r.Context(), records.ActorKey(as, a))
			}
		}
		apierr.Reply(rw, named(a, rec), err)
	})
	mux.HandleFunc("PUT /admin/atespaces/{as}/actors/{a}/egress-policy", func(rw http.ResponseWriter, r *http.Request) {
		var policy records.EgressPolicy
		if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		for {
			var rec records.Actor
			gen, err := b.Get(r.Context(), records.ActorKey(r.PathValue("as"), r.PathValue("a")), &rec)
			if err == nil {
				rec.EgressPolicy = &policy
				_, err = b.Put(r.Context(), records.ActorKey(r.PathValue("as"), r.PathValue("a")), rec, gen)
			}
			if !errors.Is(err, store.ErrConflict) {
				apierr.Reply(rw, policy, err)
				return
			}
		}
	})
	mux.HandleFunc("POST /admin/atespaces/{as}/actors/{a}/suspend", func(rw http.ResponseWriter, r *http.Request) {
		as, a := r.PathValue("as"), r.PathValue("a")
		err := in.suspend(r.Context(), as, a)
		var rec records.Actor
		if err == nil {
			_, err = b.Get(r.Context(), records.ActorKey(as, a), &rec)
		}
		apierr.Reply(rw, named(a, rec), err)
	})
	mux.HandleFunc("DELETE /admin/atespaces/{as}/actors/{a}", func(rw http.ResponseWriter, r *http.Request) {
		as, a := r.PathValue("as"), r.PathValue("a")
		err := in.suspend(r.Context(), as, a)
		if err == nil {
			if err = b.Delete(r.Context(), records.SnapshotKey(as, a)); errors.Is(err, store.ErrNotFound) {
				err = nil
			}
		}
		if err == nil {
			err = b.Delete(r.Context(), records.ActorKey(as, a))
		}
		apierr.Reply(rw, map[string]string{"name": a}, err)
	})
	mux.HandleFunc("PUT /admin/atespaces/{as}/tags/{t}", func(rw http.ResponseWriter, r *http.Request) {
		var req struct{ Actor string }
		json.NewDecoder(r.Body).Decode(&req)
		as := r.PathValue("as")
		var rec records.Actor
		_, err := b.Get(r.Context(), records.ActorKey(as, req.Actor), &rec)
		if err == nil && rec.Status != records.Suspended {
			err = fmt.Errorf("%w: suspend %s before tagging it", store.ErrConflict, req.Actor)
		}
		if err == nil {
			if err = b.Copy(r.Context(), records.SnapshotKey(as, req.Actor), records.TagKey(as, r.PathValue("t"))); errors.Is(err, store.ErrNotFound) {
				err = fmt.Errorf("%w: %s has no snapshot yet", store.ErrConflict, req.Actor)
			}
		}
		apierr.Reply(rw, map[string]string{"name": r.PathValue("t"), "actor": req.Actor}, err)
	})
	mux.HandleFunc("GET /admin/atespaces/{as}/actors", func(rw http.ResponseWriter, r *http.Request) {
		names, err := b.List(r.Context(), "atespaces/"+r.PathValue("as")+"/actors/")
		var out []any
		for _, n := range names {
			var rec records.Actor
			if _, err = b.Get(r.Context(), n, &rec); err != nil {
				break
			}
			out = append(out, named(strings.TrimSuffix(n[strings.LastIndex(n, "/")+1:], ".json"), rec))
		}
		apierr.Reply(rw, out, err)
	})
	mux.HandleFunc("GET /admin/atespaces/{as}/tags", func(rw http.ResponseWriter, r *http.Request) {
		names, err := b.List(r.Context(), "atespaces/"+r.PathValue("as")+"/tags/")
		var out []string
		for _, n := range names {
			out = append(out, strings.TrimSuffix(n[strings.LastIndex(n, "/")+1:], ".tar"))
		}
		apierr.Reply(rw, out, err)
	})
	mux.HandleFunc("GET /admin/workers", func(rw http.ResponseWriter, r *http.Request) {
		leases, err := in.leases(r.Context())
		apierr.Reply(rw, leases, err)
	})
	mux.HandleFunc("POST /admin/reap", func(rw http.ResponseWriter, r *http.Request) {
		reaped, err := in.Reap(r.Context(), r.URL.Query().Get("idleSeconds"))
		apierr.Reply(rw, reaped, err)
	})
}

// Reap asks every live worker to suspend actors idle for idleSeconds.
func (in *Ingress) Reap(ctx context.Context, idleSeconds string) ([]string, error) {
	leases, err := in.leases(ctx)
	if err != nil {
		return nil, err
	}
	reaped := []string{}
	for _, l := range leases {
		if !l.Live() {
			continue
		}
		var keys []string
		if err := in.call(ctx, l.Addr, "/reap?idleSeconds="+url.QueryEscape(idleSeconds), nil, &keys); err != nil {
			return reaped, err
		}
		reaped = append(reaped, keys...)
	}
	return reaped, nil
}

// named adds the record's name for the CLI's tables.
func named(name string, rec records.Actor) map[string]any {
	return map[string]any{"name": name, "template": rec.Template, "status": rec.Status, "owner": rec.Owner,
		"generation": rec.Generation, "created": rec.Created, "egressRules": rec.EgressPolicy != nil}
}
