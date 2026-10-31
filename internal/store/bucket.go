package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// bucket is the GCS JSON API, the few calls ate-ish needs. Every record is a
// JSON object written with ifGenerationMatch; generation 0 means "must not exist".
type bucket struct {
	name   string
	client *http.Client
}

func newBucket(ctx context.Context, name string) (*bucket, error) {
	return &bucket{name: name, client: GoogleHTTPClient(ctx)}, nil
}

// GoogleHTTPClient returns an http.Client authenticated as Google Cloud's
// own Application Default Credentials, falling back to the gcloud user
// login's own token when ADC is not set up (no metadata server, no
// GOOGLE_APPLICATION_CREDENTIALS, no `gcloud auth application-default
// login`, which needs a browser this shell cannot drive). Used both for
// the GCS store and, regardless of which store backend is running, for
// Secret Manager access from the egress gateway (internal/egress's
// secrets type): a local-directory node still resolves
// ate-secret://secretmanager.googleapis.com/... credentials through
// whichever of these two sources is available on the laptop.
func GoogleHTTPClient(ctx context.Context) *http.Client {
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
//
// `sudo ate-ish` (no -E) loses the invoking user's own HOME: root's gcloud
// config at $HOME/.config/gcloud (now /root/.config/gcloud) has no login
// of its own, so this failed outright until this process also tried
// SUDO_USER's own gcloud config directory. `sudo -E` (HOME preserved) or a
// non-root run never takes that branch; sudoUserGcloudConfigDir reports
// false for either.
type gcloudTokenSource struct{}

func (gcloudTokenSource) Token() (*oauth2.Token, error) {
	out, primaryErr := exec.Command("gcloud", "auth", "print-access-token").Output()
	if primaryErr == nil {
		return tokenFromOutput(out), nil
	}
	altConfig, ok := sudoUserGcloudConfigDir()
	if !ok {
		return nil, fmt.Errorf("gcloud auth print-access-token (HOME=%s): %w", os.Getenv("HOME"), primaryErr)
	}
	cmd := exec.Command("gcloud", "auth", "print-access-token")
	cmd.Env = append(os.Environ(), "CLOUDSDK_CONFIG="+altConfig)
	out, altErr := cmd.Output()
	if altErr != nil {
		return nil, fmt.Errorf("gcloud auth print-access-token: failed under this process's own HOME=%s (%v), and under SUDO_USER %s's own gcloud config at %s (%v)",
			os.Getenv("HOME"), primaryErr, os.Getenv("SUDO_USER"), altConfig, altErr)
	}
	return tokenFromOutput(out), nil
}

func tokenFromOutput(out []byte) *oauth2.Token {
	// A short expiry, well inside gcloud's own ~1h token lifetime, so
	// oauth2's reuse-until-expired wrapper (which treats a zero Expiry as
	// "never expires") calls back here instead of using one token forever.
	return &oauth2.Token{AccessToken: strings.TrimSpace(string(out)), TokenType: "Bearer", Expiry: time.Now().Add(5 * time.Minute)}
}

// sudoUserGcloudConfigDir reports SUDO_USER's own gcloud config directory
// (~/.config/gcloud), but only when this process is plausibly root under a
// `sudo` that did not carry HOME through: euid 0, SUDO_USER set to someone
// other than root, and HOME still reading as root's own home ("/root"),
// meaning `sudo -E` was not used. `sudo -E` (or a non-root run) never
// reaches this: HOME already names the real invoking user's home in that
// case, and google.DefaultTokenSource/gcloud's own default lookup already
// find whatever is there.
func sudoUserGcloudConfigDir() (string, bool) {
	if os.Geteuid() != 0 || os.Getenv("HOME") != "/root" {
		return "", false
	}
	name := os.Getenv("SUDO_USER")
	if name == "" || name == "root" {
		return "", false
	}
	u, err := user.Lookup(name)
	if err != nil {
		return "", false
	}
	return filepath.Join(u.HomeDir, ".config", "gcloud"), true
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
		return nil, ErrNotFound
	case http.StatusPreconditionFailed:
		resp.Body.Close()
		return nil, ErrConflict
	case http.StatusTooManyRequests:
		resp.Body.Close()
		return nil, ErrRateLimited
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	return nil, fmt.Errorf("gcs %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, msg)
}

// Get reads a JSON record and returns its generation.
func (b *bucket) Get(ctx context.Context, name string, v any) (int64, error) {
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

// Put writes a JSON record if its generation is still gen, and returns the new one.
func (b *bucket) Put(ctx context.Context, name string, v any, gen int64) (int64, error) {
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

func (b *bucket) UploadFile(ctx context.Context, name, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = b.upload(ctx, name, f, -1)
	return err
}

func (b *bucket) DownloadFile(ctx context.Context, name, file string) error {
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

func (b *bucket) Copy(ctx context.Context, src, dst string) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", b.objectURL(src)+"/copyTo/b/"+b.name+"/o/"+url.PathEscape(dst), nil)
	resp, err := b.do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (b *bucket) Delete(ctx context.Context, name string) error {
	req, _ := http.NewRequestWithContext(ctx, "DELETE", b.objectURL(name), nil)
	resp, err := b.do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// List returns the names of the objects under prefix.
func (b *bucket) List(ctx context.Context, prefix string) ([]string, error) {
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
