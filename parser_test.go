package parser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

// --- Happy path: HTML with one Person LD-JSON containing followers stat ---

const happyHTML = `<!doctype html>
<html lang="en"><head>
<title>Eugen Soloviov - LinkedIn</title>
<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Person",
  "name": "Eugen Soloviov",
  "jobTitle": "Engineer",
  "description": "Builds things.",
  "interactionStatistic": [
    {
      "@type": "InteractionCounter",
      "name": "followers",
      "userInteractionCount": 4242
    },
    {
      "@type": "InteractionCounter",
      "name": "connections",
      "userInteractionCount": 500
    }
  ]
}
</script>
</head><body>profile</body></html>`

func TestFetchChannel_HappyLDJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/in/suenot") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(happyHTML))
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	snap, err := p.FetchChannel(context.Background(), "suenot")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Platform != shared.PlatformLinkedIn {
		t.Fatalf("platform: %s", snap.Platform)
	}
	if snap.Handle != "suenot" {
		t.Fatalf("handle = %q", snap.Handle)
	}
	if snap.URL != "https://www.linkedin.com/in/suenot/" {
		t.Fatalf("url = %q", snap.URL)
	}
	if snap.Followers != 4242 {
		t.Fatalf("followers = %d, want 4242", snap.Followers)
	}
	if got := snap.Raw["display_name"]; got != "Eugen Soloviov" {
		t.Fatalf("display_name = %v", got)
	}
	if got := snap.Raw["job_title"]; got != "Engineer" {
		t.Fatalf("job_title = %v", got)
	}
	if got := snap.Raw["source"]; got != "public_html" {
		t.Fatalf("source = %v", got)
	}
}

// --- Happy path: handle accepts full URL form ---

func TestFetchChannel_HandleNormalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/in/suenot/" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(happyHTML))
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	snap, err := p.FetchChannel(context.Background(), "https://www.linkedin.com/in/suenot/?foo=bar")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Handle != "suenot" {
		t.Fatalf("normalized handle = %q", snap.Handle)
	}
}

// --- Auth wall: HTTP 999 + authwall body -> ErrAuth (with camoufox hint) ---

const authWallHTML = `<html><head>
<script type="text/javascript">
window.onload = function() {
  window.location.href = "https://" + domain + "/authwall?trk=bf&trkInfo=bf" +
      "&original_referer=" + document.referrer.substr(0, 200) +
      "&sessionRedirect=" + encodeURIComponent(window.location.href);
}
</script>
</head></html>`

func TestFetchChannel_AuthWall999(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(999)
		_, _ = w.Write([]byte(authWallHTML))
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	_, err := p.FetchChannel(context.Background(), "suenot")
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "camoufox") {
		t.Fatalf("error should mention camoufox fallback, got %q", err.Error())
	}
}

// --- Auth wall by body only (status 200 but redirect HTML) -> ErrAuth ---

func TestFetchChannel_AuthWallBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// authwall_join_form marker, status 200.
		_, _ = w.Write([]byte(`<html><body><form class="authwall_join_form"></form></body></html>`))
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	_, err := p.FetchChannel(context.Background(), "suenot")
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "camoufox") {
		t.Fatalf("expected camoufox hint, got %q", err.Error())
	}
}

// --- 404 -> ErrNotFound ---

func TestFetchChannel_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<html><body>not found</body></html>`))
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	_, err := p.FetchChannel(context.Background(), "ghost")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// --- 429 -> ErrRateLimited ---

func TestFetchChannel_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	_, err := p.FetchChannel(context.Background(), "suenot")
	if !errors.Is(err, shared.ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
}

// --- Camoufox stub: when CamoufoxURL set, fetchViaCamoufox surfaces
// "not implemented" but the auth path still returns ErrAuth.

func TestFetchChannel_CamoufoxStubFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(999)
		_, _ = w.Write([]byte(authWallHTML))
	}))
	defer srv.Close()

	client := &http.Client{
		Transport: rewriteRT{target: strings.TrimSuffix(srv.URL, "/"), next: http.DefaultTransport},
		Timeout:   3 * time.Second,
	}
	p := New(Config{HTTPClient: client, CamoufoxURL: "ws://example.invalid/cdp"})
	_, err := p.FetchChannel(context.Background(), "suenot")
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth even when camoufox is set-but-stubbed, got %v", err)
	}
}

// --- Profile with no LD-JSON: returns a snapshot anyway (Followers=0) ---

func TestFetchChannel_NoLDJSON_Soft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>x</title></head><body><h1>profile</h1></body></html>`))
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	snap, err := p.FetchChannel(context.Background(), "suenot")
	if err != nil {
		t.Fatalf("expected soft success, got err=%v", err)
	}
	if snap.Followers != 0 {
		t.Fatalf("followers = %d, want 0 (no signal in body)", snap.Followers)
	}
	if snap.Handle != "suenot" {
		t.Fatalf("handle = %q", snap.Handle)
	}
}

// --- Posts: auth wall -> (nil, nil) ---

func TestFetchRecentPosts_AuthWallSoftEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(999)
		_, _ = w.Write([]byte(authWallHTML))
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	posts, err := p.FetchRecentPosts(context.Background(), "suenot", time.Time{})
	if err != nil {
		t.Fatalf("want nil error on auth-wall, got %v", err)
	}
	if posts != nil {
		t.Fatalf("expected nil slice, got %d posts", len(posts))
	}
}

// --- Posts: extracts Article items from LD-JSON, applies since filter ---

const postsHTML = `<!doctype html><html><head>
<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Person",
  "name": "Eugen Soloviov",
  "subjectOf": [
    {
      "@type": "Article",
      "url": "https://www.linkedin.com/pulse/fresh-article-suenot/",
      "headline": "Fresh article",
      "datePublished": "2024-06-01T00:00:00Z"
    },
    {
      "@type": "Article",
      "url": "https://www.linkedin.com/pulse/stale-article-suenot/",
      "headline": "Stale article",
      "datePublished": "2023-01-01T00:00:00Z"
    }
  ]
}
</script>
</head></html>`

func TestFetchRecentPosts_LDJSONArticles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(postsHTML))
	}))
	defer srv.Close()

	p := newWithBase(t, srv.URL)
	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	posts, err := p.FetchRecentPosts(context.Background(), "suenot", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1 (stale should be filtered)", len(posts))
	}
	if posts[0].PostID != "fresh-article-suenot" {
		t.Fatalf("post id = %q", posts[0].PostID)
	}
	if posts[0].ChannelHandle != "suenot" {
		t.Fatalf("channel = %q", posts[0].ChannelHandle)
	}
	if got := posts[0].Raw["headline"]; got != "Fresh article" {
		t.Fatalf("headline = %v", got)
	}
}

// --- helpers ---

func newWithBase(t *testing.T, base string) *LinkedInParser {
	t.Helper()
	client := &http.Client{
		Transport: rewriteRT{target: strings.TrimSuffix(base, "/"), next: http.DefaultTransport},
		Timeout:   3 * time.Second,
	}
	return New(Config{HTTPClient: client})
}

// rewriteRT swaps the request host so requests aimed at the real
// LinkedIn origin land on httptest.Server instead.
type rewriteRT struct {
	target string
	next   http.RoundTripper
}

func (r rewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.String(), apiBase) {
		newURL := r.target + strings.TrimPrefix(req.URL.String(), apiBase)
		req2, err := http.NewRequestWithContext(req.Context(), req.Method, newURL, req.Body)
		if err != nil {
			return nil, err
		}
		req2.Header = req.Header
		return r.next.RoundTrip(req2)
	}
	return r.next.RoundTrip(req)
}
