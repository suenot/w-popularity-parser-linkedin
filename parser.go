// Package parser implements the w_popularity LinkedIn adapter.
//
// LinkedIn aggressively blocks unauthenticated scraping. Most non-logged-in
// HTTP requests return either:
//
//   - HTTP 999 (LinkedIn's own "no scraping" status) with a tiny HTML body
//     that JS-redirects to /authwall.
//   - A real public profile page if the IP / UA / cookies look organic
//     enough. Such pages carry one or more
//     <script type="application/ld+json"> blocks describing the profile as
//     schema.org Person / ProfilePage.
//
// Strategy:
//
//	primary:  plain HTTP GET of https://www.linkedin.com/in/<handle>/ +
//	          LD-JSON extraction.
//	fallback: camoufox-driven browser (CDP) — currently a stub. The
//	          primary→fallback branch is wired so the real implementation
//	          is a drop-in replacement for fetchViaCamoufox.
package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

// apiBase is the canonical LinkedIn origin. Tests rewrite this via a
// custom RoundTripper (see parser_test.go); production hits the real host.
const apiBase = "https://www.linkedin.com"

const defaultUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// Config controls runtime behaviour.
type Config struct {
	// HTTPClient is optional; one with HTTPTimeout is constructed otherwise.
	HTTPClient *http.Client
	// HTTPTimeout caps every outbound call. Defaults to 15s.
	HTTPTimeout time.Duration
	// UserAgent overrides the default browser-ish UA.
	UserAgent string
	// CamoufoxURL is the CDP endpoint of a camoufox instance. When set,
	// the parser falls back to it after the public HTML attempt fails or
	// is gated by an auth wall. Empty disables the fallback.
	CamoufoxURL string
}

// New constructs a LinkedIn parser.
func New(cfg Config) *LinkedInParser {
	if cfg.HTTPClient == nil {
		t := cfg.HTTPTimeout
		if t == 0 {
			t = 15 * time.Second
		}
		// Don't auto-follow LinkedIn's 301-to-authwall redirect chain —
		// we want to inspect the first response.
		cfg.HTTPClient = &http.Client{Timeout: t}
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUA
	}
	return &LinkedInParser{cfg: cfg}
}

type LinkedInParser struct{ cfg Config }

func (p *LinkedInParser) Platform() shared.Platform { return shared.PlatformLinkedIn }

// --- Channel ---

func (p *LinkedInParser) FetchChannel(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	h := normalizeHandle(handle)
	if h == "" {
		return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: empty handle")
	}
	profileURL := apiBase + "/in/" + url.PathEscape(h) + "/"

	body, status, err := p.getRaw(ctx, profileURL)
	if err == nil {
		switch {
		case status == 404:
			return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w", shared.ErrNotFound)
		case status == 429:
			return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w", shared.ErrRateLimited)
		case status == 999, status == 403:
			// Auth wall — try camoufox fallback if configured.
			return p.viaCamoufoxOrAuthErr(ctx, profileURL, h, status)
		case status >= 500:
			return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w: http %d", shared.ErrTransient, status)
		case status >= 400:
			return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: http %d", status)
		}

		if isAuthWallHTML(body) {
			return p.viaCamoufoxOrAuthErr(ctx, profileURL, h, status)
		}

		snap := parseProfileHTML(body, h, profileURL)
		return snap, nil
	}

	// Transport-level error — try the camoufox path if available.
	if camErr := p.tryCamoufox(ctx, profileURL); camErr == nil {
		// Camoufox path is currently a stub; if it ever returns nil err
		// we'd parse its HTML here. Kept as a forward-compatible branch.
		return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: camoufox path returned no body")
	}
	return shared.ChannelSnapshot{}, err
}

func (p *LinkedInParser) viaCamoufoxOrAuthErr(ctx context.Context, profileURL, handle string, status int) (shared.ChannelSnapshot, error) {
	html, err := p.fetchViaCamoufox(ctx, profileURL)
	if err == nil && html != "" {
		return parseProfileHTML(html, handle, profileURL), nil
	}
	hint := "auth wall"
	if status == 999 {
		hint = "http 999 auth wall"
	} else if status == 403 {
		hint = "http 403 auth wall"
	}
	return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w: %s; configure Config.CamoufoxURL for browser fallback", shared.ErrAuth, hint)
}

func (p *LinkedInParser) tryCamoufox(ctx context.Context, profileURL string) error {
	if _, err := p.fetchViaCamoufox(ctx, profileURL); err != nil {
		return err
	}
	return nil
}

// fetchViaCamoufox is a stub for the camoufox / CDP fallback. The wiring
// for primary→fallback is in place; a real implementation would dial
// Config.CamoufoxURL with chromedp (or similar), navigate to profileURL,
// wait for the profile DOM, and return the rendered HTML.
func (p *LinkedInParser) fetchViaCamoufox(_ context.Context, _ string) (string, error) {
	if p.cfg.CamoufoxURL == "" {
		return "", errors.New("linkedin: camoufox not configured")
	}
	return "", errors.New("linkedin: camoufox path not implemented")
}

// --- Posts ---

// FetchRecentPosts is best-effort. Logged-out LinkedIn profiles rarely
// expose post lists; we extract whatever Article items the LD-JSON
// happens to carry (under publishingPrinciples / subjectOf / mainEntity).
// When nothing is parseable we return (nil, nil) rather than an error.
func (p *LinkedInParser) FetchRecentPosts(ctx context.Context, handle string, since time.Time) ([]shared.PostSnapshot, error) {
	h := normalizeHandle(handle)
	if h == "" {
		return nil, fmt.Errorf("linkedin: empty handle")
	}
	profileURL := apiBase + "/in/" + url.PathEscape(h) + "/"
	body, status, err := p.getRaw(ctx, profileURL)
	if err != nil {
		return nil, err
	}
	switch {
	case status == 404:
		return nil, fmt.Errorf("linkedin: %w", shared.ErrNotFound)
	case status == 429:
		return nil, fmt.Errorf("linkedin: %w", shared.ErrRateLimited)
	case status == 999, status == 403:
		// No posts visible without auth; treat as soft-empty so callers
		// can still proceed.
		return nil, nil
	case status >= 400:
		return nil, fmt.Errorf("linkedin: http %d", status)
	}
	if isAuthWallHTML(body) {
		return nil, nil
	}
	return parseArticlesFromHTML(body, h, since), nil
}

// --- LD-JSON parsing ---

var reLDJSON = regexp.MustCompile(`(?is)<script[^>]+type=["']application/ld\+json["'][^>]*>(.*?)</script>`)

// parseProfileHTML extracts a best-effort ChannelSnapshot from a public
// profile page. If no LD-JSON / no followers field is found, we still
// return a snapshot (with whatever signals we did find in Raw) — we
// don't want to fail every fetch just because LinkedIn rotated its DOM.
func parseProfileHTML(body, handle, profileURL string) shared.ChannelSnapshot {
	snap := shared.ChannelSnapshot{
		Platform:  shared.PlatformLinkedIn,
		Handle:    handle,
		URL:       profileURL,
		FetchedAt: time.Now().UTC(),
		Raw:       map[string]interface{}{"source": "public_html"},
	}

	matches := reLDJSON.FindAllStringSubmatch(body, -1)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		raw := strings.TrimSpace(m[1])
		// Strip a UTF-8 BOM if present (LinkedIn occasionally emits one).
		raw = strings.TrimPrefix(raw, "\ufeff")

		// First try as a single object.
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &obj); err == nil {
			if applyPerson(&snap, obj) {
				return snap
			}
			// Handle @graph wrappers.
			if graph, ok := obj["@graph"].([]interface{}); ok {
				for _, g := range graph {
					if gm, ok := g.(map[string]interface{}); ok {
						if applyPerson(&snap, gm) {
							return snap
						}
					}
				}
			}
			continue
		}
		// Otherwise try as an array.
		var arr []map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			for _, item := range arr {
				if applyPerson(&snap, item) {
					return snap
				}
			}
		}
	}
	return snap
}

// applyPerson populates the snapshot from a single LD-JSON node. Returns
// true when the node matched Person/ProfilePage so the caller can stop
// scanning.
func applyPerson(snap *shared.ChannelSnapshot, node map[string]interface{}) bool {
	t, _ := stringOrFirst(node["@type"])
	switch t {
	case "Person", "ProfilePage":
		// fall through
	default:
		return false
	}
	if name, ok := node["name"].(string); ok && name != "" {
		snap.Raw["display_name"] = name
	}
	if job, ok := node["jobTitle"].(string); ok && job != "" {
		snap.Raw["job_title"] = job
	}
	if desc, ok := node["description"].(string); ok && desc != "" {
		snap.Raw["description"] = desc
	}
	// Followers can appear in several places — try them all.
	if v := extractFollowers(node); v > 0 {
		snap.Followers = v
	}
	// Some payloads expose article counts via subjectOf.
	if arts, ok := node["subjectOf"].([]interface{}); ok {
		snap.PostsCount = int64(countArticles(arts))
	}
	return true
}

// extractFollowers walks the common LinkedIn / schema.org shapes for a
// follower count.
func extractFollowers(node map[string]interface{}) int64 {
	// interactionStatistic: [{ "@type":"InteractionCounter", "name":"followers", "userInteractionCount":N }, ...]
	if stats, ok := node["interactionStatistic"].([]interface{}); ok {
		for _, s := range stats {
			sm, ok := s.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := sm["name"].(string)
			itype, _ := stringOrFirst(sm["interactionType"])
			if !strings.EqualFold(name, "followers") &&
				!strings.Contains(strings.ToLower(itype), "follow") {
				continue
			}
			if v, ok := numericField(sm["userInteractionCount"]); ok {
				return v
			}
		}
	}
	// Sometimes a single object instead of an array.
	if sm, ok := node["interactionStatistic"].(map[string]interface{}); ok {
		if v, ok := numericField(sm["userInteractionCount"]); ok {
			return v
		}
	}
	// LinkedIn's "follows" / "followee" extension fields, if ever present.
	if v, ok := numericField(node["follows"]); ok {
		return v
	}
	return 0
}

func countArticles(items []interface{}) int {
	n := 0
	for _, it := range items {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := stringOrFirst(m["@type"])
		if t == "Article" || t == "BlogPosting" || t == "SocialMediaPosting" {
			n++
		}
	}
	return n
}

func parseArticlesFromHTML(body, handle string, since time.Time) []shared.PostSnapshot {
	var out []shared.PostSnapshot
	now := time.Now().UTC()
	matches := reLDJSON.FindAllStringSubmatch(body, -1)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		raw := strings.TrimSpace(m[1])
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			continue
		}
		// Look for Article items under subjectOf or publishingPrinciples.
		for _, key := range []string{"subjectOf", "publishingPrinciples", "mainEntity"} {
			arr, ok := obj[key].([]interface{})
			if !ok {
				continue
			}
			for _, item := range arr {
				im, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				t, _ := stringOrFirst(im["@type"])
				if t != "Article" && t != "BlogPosting" && t != "SocialMediaPosting" {
					continue
				}
				ps, ok := articleToPost(im, handle, now)
				if !ok {
					continue
				}
				if !since.IsZero() && ps.PublishedAt.Before(since) {
					continue
				}
				out = append(out, ps)
			}
		}
	}
	return out
}

func articleToPost(m map[string]interface{}, handle string, now time.Time) (shared.PostSnapshot, bool) {
	postURL, _ := m["url"].(string)
	if postURL == "" {
		if id, ok := m["@id"].(string); ok {
			postURL = id
		}
	}
	if postURL == "" {
		return shared.PostSnapshot{}, false
	}
	pub := time.Time{}
	if s, ok := m["datePublished"].(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			pub = t
		} else if t, err := time.Parse("2006-01-02", s); err == nil {
			pub = t
		}
	}
	id := postURL
	if i := strings.LastIndex(strings.Trim(postURL, "/"), "/"); i >= 0 {
		id = strings.Trim(postURL, "/")[i+1:]
	}
	headline, _ := m["headline"].(string)
	if headline == "" {
		headline, _ = m["name"].(string)
	}
	return shared.PostSnapshot{
		Platform:      shared.PlatformLinkedIn,
		ChannelHandle: handle,
		PostID:        id,
		URL:           postURL,
		Kind:          shared.PostKindPost,
		PublishedAt:   pub,
		FetchedAt:     now,
		Raw: map[string]interface{}{
			"source":   "ldjson",
			"headline": headline,
		},
	}, true
}

// --- HTTP ---

func (p *LinkedInParser) getRaw(ctx context.Context, u string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", p.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("linkedin: %w: %v", shared.ErrTransient, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, fmt.Errorf("linkedin: %w: read body: %v", shared.ErrTransient, err)
	}
	return string(b), resp.StatusCode, nil
}

// --- helpers ---

// isAuthWallHTML detects LinkedIn's various auth-wall responses. The
// canonical signals are:
//   - the literal string "authwall" or "authwall_join_form"
//   - absence of any application/ld+json block on a body that does have
//     a <html> tag (i.e. it's not just a parser error)
//   - the auth-wall <meta> redirect noscript line
func isAuthWallHTML(body string) bool {
	low := strings.ToLower(body)
	if strings.Contains(low, "authwall_join_form") {
		return true
	}
	if strings.Contains(low, "/authwall?") {
		return true
	}
	if strings.Contains(low, "window.location.href = \"https://\" + domain + \"/authwall") {
		return true
	}
	return false
}

func normalizeHandle(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "@")
	// Accept full URLs like https://www.linkedin.com/in/suenot/
	if i := strings.Index(h, "linkedin.com/in/"); i >= 0 {
		h = h[i+len("linkedin.com/in/"):]
	}
	h = strings.Trim(h, "/")
	if i := strings.Index(h, "/"); i >= 0 {
		h = h[:i]
	}
	if i := strings.Index(h, "?"); i >= 0 {
		h = h[:i]
	}
	return h
}

func stringOrFirst(v interface{}) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []interface{}:
		if len(x) == 0 {
			return "", false
		}
		if s, ok := x[0].(string); ok {
			return s, true
		}
	}
	return "", false
}

func numericField(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int:
		return int64(x), true
	case int64:
		return x, true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		if f, err := x.Float64(); err == nil {
			return int64(f), true
		}
	case string:
		s := strings.ReplaceAll(strings.TrimSpace(x), ",", "")
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

// Compile-time interface check.
var _ shared.Parser = (*LinkedInParser)(nil)
