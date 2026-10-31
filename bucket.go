package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var (
	errNotFound    = errors.New("not found")
	errConflict    = errors.New("generation changed")
	errRateLimited = errors.New("rate limited")
)

// bucket is the GCS JSON API, the few calls ate-ish needs. Every record is a
// JSON object written with ifGenerationMatch; generation 0 means "must not exist".
type bucket struct {
	name   string
	client *http.Client
}

func newBucket(ctx context.Context, name string) (*bucket, error) {
	return &bucket{name: name, client: googleHTTPClient(ctx)}, nil
}

// googleHTTPClient returns an http.Client authenticated as Google Cloud's
// own Application Default Credentials, falling back to the gcloud user
// login's own token when ADC is not set up (no metadata server, no
// GOOGLE_APPLICATION_CREDENTIALS, no `gcloud auth application-default
// login`, which needs a browser this shell cannot drive). Used both for
// the GCS store and, regardless of which store backend is running, for
// Secret Manager access from the egress gateway (egress.go's secrets type):
// a local-directory dev node still resolves
// ate-secret://secretmanager.googleapis.com/... credentials through
// whichever of these two sources is available on the laptop.
func googleHTTPClient(ctx context.Context) *http.Client {
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		ts = gcloudTokenSource{}
	}
	return oauth2.NewClient(ctx, ts)
}

// gcloudTokenSource gets a fresh access token from `gcloud auth
// print-access-token` on every call. gcloud caches and refreshes the
// underlying credential itself, so this just adds one process spawn
// (tens of milliseconds) to the token refresh path, not to every request.
type gcloudTokenSource struct{}

func (gcloudTokenSource) Token() (*oauth2.Token, error) {
	out, err := exec.Command("gcloud", "auth", "print-access-token").Output()
	if err != nil {
		return nil, fmt.Errorf("gcloud auth print-access-token: %w", err)
	}
	// A short expiry, well inside gcloud's own ~1h token lifetime, so
	// oauth2's reuse-until-expired wrapper (which treats a zero Expiry as
	// "never expires") calls back here instead of using one token forever.
	return &oauth2.Token{AccessToken: strings.TrimSpace(string(out)), TokenType: "Bearer", Expiry: time.Now().Add(5 * time.Minute)}, nil
}

const gcs = "https://storage.googleapis.com"

func (b *bucket) objectURL(name string) string {
	return gcs + "/storage/v1/b/" + b.name + "/o/" + url.PathEscape(name)
}

func (b *bucket) do(req *http.Request) (*http.Response, error) {
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return resp, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, errNotFound
	case http.StatusPreconditionFailed:
		resp.Body.Close()
		return nil, errConflict
	case http.StatusTooManyRequests:
		resp.Body.Close()
		return nil, errRateLimited
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	return nil, fmt.Errorf("gcs %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, msg)
}

// get reads a JSON record and returns its generation.
func (b *bucket) get(ctx context.Context, name string, v any) (int64, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", b.objectURL(name)+"?alt=media", nil)
	resp, err := b.do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return strconv.ParseInt(resp.Header.Get("x-goog-generation"), 10, 64)
}

// put writes a JSON record if its generation is still gen, and returns the new one.
func (b *bucket) put(ctx context.Context, name string, v any, gen int64) (int64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return b.upload(ctx, name, bytes.NewReader(data), gen)
}

func (b *bucket) upload(ctx context.Context, name string, body io.Reader, gen int64) (int64, error) {
	q := url.Values{"uploadType": {"media"}, "name": {name}}
	if gen >= 0 {
		q.Set("ifGenerationMatch", strconv.FormatInt(gen, 10))
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", gcs+"/upload/storage/v1/b/"+b.name+"/o?"+q.Encode(), body)
	resp, err := b.do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var obj struct {
		Generation int64 `json:"generation,string"`
	}
	return obj.Generation, json.NewDecoder(resp.Body).Decode(&obj)
}

func (b *bucket) uploadFile(ctx context.Context, name, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = b.upload(ctx, name, f, -1)
	return err
}

func (b *bucket) downloadFile(ctx context.Context, name, file string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", b.objectURL(name)+"?alt=media", nil)
	resp, err := b.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (b *bucket) copy(ctx context.Context, src, dst string) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", b.objectURL(src)+"/copyTo/b/"+b.name+"/o/"+url.PathEscape(dst), nil)
	resp, err := b.do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (b *bucket) delete(ctx context.Context, name string) error {
	req, _ := http.NewRequestWithContext(ctx, "DELETE", b.objectURL(name), nil)
	resp, err := b.do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// list returns the names of the objects under prefix.
func (b *bucket) list(ctx context.Context, prefix string) ([]string, error) {
	var names []string
	q := url.Values{"prefix": {prefix}, "fields": {"items(name),nextPageToken"}}
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", gcs+"/storage/v1/b/"+b.name+"/o?"+q.Encode(), nil)
		resp, err := b.do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items         []struct{ Name string }
			NextPageToken string
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			names = append(names, it.Name)
		}
		if page.NextPageToken == "" {
			return names, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}
