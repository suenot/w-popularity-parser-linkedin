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

// --- Happy path: BPR datalet shape ---

// dataletHTML simulates a logged-in profile page. The BPR datalet body
// is wrapped in an HTML comment, as LinkedIn actually serves it.
const dataletHTML = `<!doctype html>
<html lang="en"><head>
<title>Eugen Soloviov | LinkedIn</title>
</head><body>
<code id="datalet-bpr-guid-1234567" style="display:none">
<!--{"data":{"data":{"*elements":["urn:li:fsd_profile:ACoAAA"]},"included":[
  {
    "$type":"com.linkedin.voyager.dash.identity.profile.Profile",
    "firstName":"Eugen",
    "lastName":"Soloviov",
    "headline":"Builds things",
    "locationName":"Berlin, Germany",
    "industryName":"Software Development",
    "followerCount":4242,
    "connectionCount":500
  },
  {
    "$type":"com.linkedin.voyager.dash.identity.profile.Position",
    "companyName":"Acme Corp"
  }
]}}-->
</code>
</body></html>`

func TestFetchChannel_HappyPath_Datalet(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		if !strings.HasPrefix(r.URL.Path, "/in/suenot") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dataletHTML))
	}))
	defer srv.Close()

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "AQEDATEST123",
		BaseURL:    srv.URL,
	})
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
		t.Fatalf("url = %q (should always be canonical, not test server)", snap.URL)
	}
	if snap.Followers != 4242 {
		t.Fatalf("followers = %d, want 4242", snap.Followers)
	}
	if snap.PostsCount != 0 {
		t.Fatalf("posts_count = %d, want 0 (profile pages don't expose lifetime posts)", snap.PostsCount)
	}
	if got := snap.Raw["headline"]; got != "Builds things" {
		t.Fatalf("headline = %v", got)
	}
	if got := snap.Raw["first_name"]; got != "Eugen" {
		t.Fatalf("first_name = %v", got)
	}
	if got := snap.Raw["last_name"]; got != "Soloviov" {
		t.Fatalf("last_name = %v", got)
	}
	if got := snap.Raw["location"]; got != "Berlin, Germany" {
		t.Fatalf("location = %v", got)
	}
	if got := snap.Raw["industry"]; got != "Software Development" {
		t.Fatalf("industry = %v", got)
	}
	if got := snap.Raw["current_company"]; got != "Acme Corp" {
		t.Fatalf("current_company = %v", got)
	}
	if got, _ := snap.Raw["connections_count"].(int64); got != 500 {
		t.Fatalf("connections_count = %v", snap.Raw["connections_count"])
	}
	if got := snap.Raw["source"]; got != "bpr_datalet" {
		t.Fatalf("source = %v, want bpr_datalet", got)
	}
	// Verify the li_at cookie was actually sent.
	if !strings.Contains(gotCookie, "li_at=AQEDATEST123") {
		t.Fatalf("Cookie header missing li_at: %q", gotCookie)
	}
}

// --- Apollo state shape (tolerance test) ---

const apolloHTML = `<!doctype html>
<html><head><title>x</title></head><body>
<script>
window.__APOLLO_STATE__ = {
  "Profile:urn:li:fsd_profile:ACoAAA": {
    "firstName": "Test",
    "lastName": "User",
    "headline": "Just testing",
    "followerCount": 1337,
    "numberOfConnections": 250
  }
};
</script>
</body></html>`

func TestFetchChannel_ApolloState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(apolloHTML))
	}))
	defer srv.Close()

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "AQEDTOKEN",
		BaseURL:    srv.URL,
	})
	snap, err := p.FetchChannel(context.Background(), "suenot")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Followers != 1337 {
		t.Fatalf("followers = %d, want 1337 (apollo path)", snap.Followers)
	}
	if got, _ := snap.Raw["connections_count"].(int64); got != 250 {
		t.Fatalf("connections_count = %v", snap.Raw["connections_count"])
	}
	if got := snap.Raw["source"]; got != "apollo_state" {
		t.Fatalf("source = %v, want apollo_state", got)
	}
}

// --- Cookie set but server returns 999 -> ErrAuth "expired" ---

func TestFetchChannel_CookieExpired999(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(999)
		_, _ = w.Write([]byte(`<html><body><script>window.location.href = "https://" + domain + "/authwall"</script></body></html>`))
	}))
	defer srv.Close()

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "EXPIRED",
		BaseURL:    srv.URL,
	})
	_, err := p.FetchChannel(context.Background(), "suenot")
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "li_at cookie expired") {
		t.Fatalf("error should mention expired cookie, got %q", err.Error())
	}
}

// --- No cookie, no camoufox -> ErrAuth with hint ---

func TestFetchChannel_NoCookieNoCamoufox(t *testing.T) {
	// Server is never actually hit because the parser short-circuits.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not be called when no cookie is set")
	}))
	defer srv.Close()

	p := New(Config{
		HTTPClient: srv.Client(),
		BaseURL:    srv.URL,
	})
	_, err := p.FetchChannel(context.Background(), "suenot")
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "li_at") || !strings.Contains(low, "camoufox_url") {
		t.Fatalf("error should mention both LI_AT and CAMOUFOX_URL, got %q", err.Error())
	}
}

// --- 404 -> ErrNotFound ---

func TestFetchChannel_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<html><body>not found</body></html>`))
	}))
	defer srv.Close()

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "AQEDTOKEN",
		BaseURL:    srv.URL,
	})
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

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "AQEDTOKEN",
		BaseURL:    srv.URL,
	})
	_, err := p.FetchChannel(context.Background(), "suenot")
	if !errors.Is(err, shared.ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
}

// --- 200 with no extractable structure -> soft success + fetch_note ---

func TestFetchChannel_NoStructure_Soft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>x</title></head><body><h1>profile</h1></body></html>`))
	}))
	defer srv.Close()

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "AQEDTOKEN",
		BaseURL:    srv.URL,
	})
	snap, err := p.FetchChannel(context.Background(), "suenot")
	if err != nil {
		t.Fatalf("expected soft success, got err=%v", err)
	}
	if snap.Followers != 0 {
		t.Fatalf("followers = %d, want 0 (no signal)", snap.Followers)
	}
	if snap.Handle != "suenot" {
		t.Fatalf("handle = %q", snap.Handle)
	}
	if _, ok := snap.Raw["fetch_note"]; !ok {
		t.Fatalf("expected fetch_note diagnostic, got Raw=%v", snap.Raw)
	}
}

// --- Verify JSESSIONID is forwarded when set ---

func TestFetchChannel_JSESSIONIDForwarded(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		_, _ = w.Write([]byte(dataletHTML))
	}))
	defer srv.Close()

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "AQEDTOKEN",
		JSESSIONID: "ajax:1234567890",
		BaseURL:    srv.URL,
	})
	if _, err := p.FetchChannel(context.Background(), "suenot"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCookie, "li_at=AQEDTOKEN") {
		t.Fatalf("missing li_at: %q", gotCookie)
	}
	if !strings.Contains(gotCookie, "JSESSIONID=\"ajax:1234567890\"") {
		t.Fatalf("JSESSIONID not quoted/forwarded: %q", gotCookie)
	}
}

// --- Handle normalization: accepts full URL form ---

func TestFetchChannel_HandleNormalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/in/suenot/" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(dataletHTML))
	}))
	defer srv.Close()

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "AQEDTOKEN",
		BaseURL:    srv.URL,
	})
	snap, err := p.FetchChannel(context.Background(), "https://www.linkedin.com/in/suenot/?foo=bar")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Handle != "suenot" {
		t.Fatalf("normalized handle = %q", snap.Handle)
	}
}

// --- Camoufox stub: when set-but-stubbed, no-cookie still ends in ErrAuth ---

func TestFetchChannel_CamoufoxStubFallback(t *testing.T) {
	p := New(Config{
		CamoufoxURL: "ws://example.invalid/cdp",
	})
	_, err := p.FetchChannel(context.Background(), "suenot")
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth even when camoufox is set-but-stubbed, got %v", err)
	}
}

// --- LD-JSON tolerance ---

const ldJSONHTML = `<!doctype html><html><head>
<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Person",
  "name": "Eugen Soloviov",
  "jobTitle": "Engineer",
  "interactionStatistic": [
    {
      "@type": "InteractionCounter",
      "name": "followers",
      "userInteractionCount": 9999
    }
  ]
}
</script>
</head></html>`

func TestFetchChannel_LDJSONFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ldJSONHTML))
	}))
	defer srv.Close()

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "AQEDTOKEN",
		BaseURL:    srv.URL,
	})
	snap, err := p.FetchChannel(context.Background(), "suenot")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Followers != 9999 {
		t.Fatalf("followers = %d, want 9999", snap.Followers)
	}
	if got := snap.Raw["source"]; got != "ldjson" {
		t.Fatalf("source = %v, want ldjson", got)
	}
}

// --- Posts ---

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

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "AQEDTOKEN",
		BaseURL:    srv.URL,
	})
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
}

func TestFetchRecentPosts_NoCookieSoftEmpty(t *testing.T) {
	p := New(Config{}) // no cookie
	posts, err := p.FetchRecentPosts(context.Background(), "suenot", time.Time{})
	if err != nil {
		t.Fatalf("want nil error on no-cookie, got %v", err)
	}
	if posts != nil {
		t.Fatalf("expected nil slice, got %d posts", len(posts))
	}
}

func TestFetchRecentPosts_AuthWallSoftEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(999)
		_, _ = w.Write([]byte(`<html><body>authwall</body></html>`))
	}))
	defer srv.Close()

	p := New(Config{
		HTTPClient: srv.Client(),
		LIATCookie: "AQEDTOKEN",
		BaseURL:    srv.URL,
	})
	posts, err := p.FetchRecentPosts(context.Background(), "suenot", time.Time{})
	if err != nil {
		t.Fatalf("want nil error on auth-wall, got %v", err)
	}
	if posts != nil {
		t.Fatalf("expected nil slice, got %d posts", len(posts))
	}
}

// --- Platform identity ---

func TestPlatform(t *testing.T) {
	p := New(Config{})
	if p.Platform() != shared.PlatformLinkedIn {
		t.Fatalf("platform = %s", p.Platform())
	}
}
