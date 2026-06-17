// Package parser implements the w_popularity LinkedIn adapter.
//
// LinkedIn aggressively blocks unauthenticated scraping. A logged-out
// HTTP GET of https://www.linkedin.com/in/<handle>/ almost always
// returns HTTP 999 (LinkedIn's own "no scraping" status) and a body
// that JS-redirects to /authwall. The schema.org Person LD-JSON block
// is also empty on the logged-out path: there is no follower count to
// scrape.
//
// Strategy (in priority order):
//
//  1. **Authenticated HTML scrape via `li_at` session cookie.** When
//     Config.LIATCookie is set we attach `Cookie: li_at=...` (and an
//     optional JSESSIONID) to the request. LinkedIn then serves the
//     real profile page HTML, which embeds structured data in three
//     possible shapes:
//
//     a. BPR datalets:
//
//     <code id="datalet-bpr-guid-…" style="display:none">
//     <!--{"data":{"data": {...}, "included": [{...}]}}-->
//     </code>
//
//     Each datalet body is an HTML comment around a JSON object.
//     For profile pages, the `included` array contains nodes with
//     follower / connection counters and headline/location fields.
//     This is LinkedIn's own browser-proxy payload format and is the
//     most reliable extraction surface today.
//
//     b. window.__APOLLO_STATE__: occasionally a different render path
//     emits an Apollo cache as a JS global. Less common, but worth
//     trying as a secondary.
//
//     c. <script type="application/ld+json"> with @type=Person: when
//     authenticated the LD-JSON `interactionStatistic` array carries
//     a real `followers` counter (zero for logged-out fetches, which
//     is why the previous implementation always returned 0).
//
//     The extractor is intentionally tolerant: it deep-walks every
//     decoded JSON tree looking for known keys (followerCount,
//     connectionsCount, headline, location, industryName, …) and
//     stops at the first hit. We never want a LinkedIn UI rotation to
//     break the whole pipeline; missing fields downgrade to 0 plus a
//     diagnostic marker in Raw["fetch_note"].
//
//  2. **camoufox via Playwright (skeleton).** Config.CamoufoxURL is the
//     CDP endpoint of a running camoufox instance (same shape as the
//     facebook/instagram adapters). fetchViaCamoufox is currently a
//     stub — the branching is in place so a real implementation is a
//     drop-in replacement.
//
// When both LIATCookie and CamoufoxURL are empty we return ErrAuth with
// the hint "set LI_AT cookie or CAMOUFOX_URL". When LIATCookie is set
// but LinkedIn still answers 999 we treat the cookie as expired and
// return ErrAuth with "li_at cookie expired".
package parser

import (
	"bytes"
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

	shared "github.com/suenot/socials-auto"
)

// defaultBaseURL is the canonical LinkedIn origin. Tests rewrite this via
// Config.BaseURL or a custom RoundTripper.
const defaultBaseURL = "https://www.linkedin.com"

const defaultUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

// Config controls runtime behaviour.
type Config struct {
	// LIATCookie is the `li_at` session cookie of a logged-in LinkedIn
	// browser session. This is the primary auth surface. Pulled from
	// the LINKEDIN_LI_AT env var by callers. Empty disables the
	// authenticated path.
	LIATCookie string

	// JSESSIONID is an optional companion cookie. Some LinkedIn server
	// endpoints check it; the profile page generally doesn't, but we
	// forward it when present.
	JSESSIONID string

	// HTTPClient is optional; one with HTTPTimeout is constructed
	// otherwise. The default client does not auto-follow LinkedIn's
	// 301-to-authwall chain — we want to inspect the first response.
	HTTPClient *http.Client

	// HTTPTimeout caps every outbound call. Defaults to 15s.
	HTTPTimeout time.Duration

	// UserAgent overrides the default desktop Chrome UA. LinkedIn is
	// pickier about UAs than most sites; a realistic value is required.
	UserAgent string

	// CamoufoxURL is the CDP endpoint of a camoufox instance. When set,
	// and LIATCookie is not, the parser falls back to it. Empty
	// disables the fallback.
	CamoufoxURL string

	// BaseURL overrides https://www.linkedin.com. Test hook.
	BaseURL string
}

// New constructs a LinkedIn parser.
func New(cfg Config) *LinkedInParser {
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 15 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.HTTPTimeout}
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUA
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &LinkedInParser{cfg: cfg}
}

// LinkedInParser is the public entry point.
type LinkedInParser struct{ cfg Config }

// Platform implements shared.Parser.
func (p *LinkedInParser) Platform() shared.Platform { return shared.PlatformLinkedIn }

// --- Channel ---

// FetchChannel returns the latest snapshot for handle. The handle may
// be a bare vanity name (`suenot`) or a full profile URL.
func (p *LinkedInParser) FetchChannel(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	h := normalizeHandle(handle)
	if h == "" {
		return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: empty handle")
	}
	profileURL := p.cfg.BaseURL + "/in/" + url.PathEscape(h) + "/"
	// The canonical (production) URL is always what we surface to callers,
	// regardless of any BaseURL test override.
	canonicalURL := defaultBaseURL + "/in/" + url.PathEscape(h) + "/"

	// Branching: no cookie -> try camoufox -> fail with the right hint.
	if p.cfg.LIATCookie == "" {
		return p.viaCamoufoxOrAuthErr(ctx, profileURL, canonicalURL, h, 0)
	}

	body, status, err := p.getRaw(ctx, profileURL)
	if err != nil {
		return shared.ChannelSnapshot{}, err
	}

	switch {
	case status == http.StatusNotFound:
		return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w", shared.ErrNotFound)
	case status == http.StatusTooManyRequests:
		return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w", shared.ErrRateLimited)
	case status == 999, status == http.StatusForbidden:
		// Cookie was set but LinkedIn still slammed the door. Almost
		// always means the li_at cookie has expired (they live ~1y).
		return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w: li_at cookie expired (refresh from a logged-in browser)", shared.ErrAuth)
	case status >= 500:
		return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w: http %d", shared.ErrTransient, status)
	case status >= 400:
		return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: http %d", status)
	}

	// 2xx: even with a valid cookie LinkedIn occasionally bounces us to
	// /authwall (region restrictions, rate limiting, etc).
	if isAuthWallHTML(body) {
		return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w: authwall returned despite li_at cookie (likely expired)", shared.ErrAuth)
	}

	snap := parseProfileHTML(body, h, canonicalURL)
	// If none of the extractors found a follower/connection count we treat
	// this as a transient parse failure rather than a legitimate zero. This
	// prevents a DOM rotation from wiping out the previous valid value and
	// producing a bogus -100 % delta.
	if snap.Followers == 0 && snap.Raw["fetch_note"] != nil {
		return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w: no structured data extracted (DOM may have rotated)", shared.ErrTransient)
	}
	return snap, nil
}

// viaCamoufoxOrAuthErr is the no-cookie branch. It tries the camoufox
// path (currently a stub) and, on failure, returns ErrAuth with a
// status-appropriate hint.
func (p *LinkedInParser) viaCamoufoxOrAuthErr(ctx context.Context, profileURL, canonicalURL, handle string, status int) (shared.ChannelSnapshot, error) {
	html, err := p.fetchViaCamoufox(ctx, profileURL)
	if err == nil && html != "" {
		return parseProfileHTML(html, handle, canonicalURL), nil
	}
	return shared.ChannelSnapshot{}, fmt.Errorf("linkedin: %w: set LI_AT cookie or CAMOUFOX_URL", shared.ErrAuth)
}

// camoufoxFetchRequest mirrors the JSON body accepted by POST /fetch on the
// camoufox HTTP wrapper (see camoufox/server.py).
type camoufoxFetchRequest struct {
	URL             string `json:"url"`
	Profile         string `json:"profile"`
	WaitForSelector string `json:"wait_for_selector,omitempty"`
	TimeoutMS       int    `json:"timeout_ms,omitempty"`
}

// camoufoxFetchResponse mirrors the response envelope.
type camoufoxFetchResponse struct {
	HTML           string `json:"html"`
	Status         int    `json:"status"`
	FinalURL       string `json:"final_url"`
	Title          string `json:"title"`
	CookiesPresent bool   `json:"cookies_present"`
	// Populated when the wrapper returns a non-2xx with {"error": "..."}.
	Error string `json:"error,omitempty"`
}

// fetchViaCamoufox POSTs the target URL to the camoufox wrapper, gets back
// the fully-rendered HTML, and returns it. The caller passes the HTML
// through the same extractors that the cookie-auth path uses.
//
// Network/transport failures are mapped to ErrTransient so the scheduler
// retries them. HTTP 4xx from the wrapper means "we asked for something
// invalid" — we propagate as-is.
func (p *LinkedInParser) fetchViaCamoufox(ctx context.Context, profileURL string) (string, error) {
	if p.cfg.CamoufoxURL == "" {
		return "", errors.New("linkedin: camoufox not configured")
	}

	body, err := json.Marshal(camoufoxFetchRequest{
		URL:             profileURL,
		Profile:         "linkedin",
		WaitForSelector: "main",
		TimeoutMS:       20_000,
	})
	if err != nil {
		return "", fmt.Errorf("linkedin: %w: marshal camoufox request: %v", shared.ErrTransient, err)
	}

	endpoint := strings.TrimRight(p.cfg.CamoufoxURL, "/") + "/fetch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("linkedin: %w: build camoufox request: %v", shared.ErrTransient, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("linkedin: %w: camoufox: %v", shared.ErrTransient, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", fmt.Errorf("linkedin: %w: read camoufox body: %v", shared.ErrTransient, err)
	}

	var parsed camoufoxFetchResponse
	if jerr := json.Unmarshal(raw, &parsed); jerr != nil {
		return "", fmt.Errorf("linkedin: %w: parse camoufox response (http %d): %v", shared.ErrTransient, resp.StatusCode, jerr)
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusBadGateway {
		msg := parsed.Error
		if msg == "" {
			msg = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return "", fmt.Errorf("linkedin: %w: camoufox: %s", shared.ErrTransient, msg)
	}
	if resp.StatusCode >= 400 {
		msg := parsed.Error
		if msg == "" {
			msg = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return "", fmt.Errorf("linkedin: camoufox: %s", msg)
	}
	if parsed.HTML == "" {
		return "", fmt.Errorf("linkedin: %w: camoufox returned empty HTML", shared.ErrTransient)
	}
	return parsed.HTML, nil
}

// --- Posts ---

// FetchRecentPosts is best-effort. Profile pages don't expose a post
// stream without hitting /detail/recent-activity/shares/ as the logged-in
// user — which costs a second request and a second cookie surface. For
// now we extract whatever Article items happen to appear in LD-JSON
// (rare, but free) and return (nil, nil) for everything else.
func (p *LinkedInParser) FetchRecentPosts(ctx context.Context, handle string, since time.Time) ([]shared.PostSnapshot, error) {
	h := normalizeHandle(handle)
	if h == "" {
		return nil, fmt.Errorf("linkedin: empty handle")
	}
	if p.cfg.LIATCookie == "" {
		// Same branching as FetchChannel: without a cookie we can't see
		// the post stream. Soft-empty so callers keep going.
		return nil, nil
	}
	profileURL := p.cfg.BaseURL + "/in/" + url.PathEscape(h) + "/"
	body, status, err := p.getRaw(ctx, profileURL)
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusNotFound:
		return nil, fmt.Errorf("linkedin: %w", shared.ErrNotFound)
	case status == http.StatusTooManyRequests:
		return nil, fmt.Errorf("linkedin: %w", shared.ErrRateLimited)
	case status == 999, status == http.StatusForbidden:
		return nil, nil
	case status >= 400:
		return nil, fmt.Errorf("linkedin: http %d", status)
	}
	if isAuthWallHTML(body) {
		return nil, nil
	}
	return parseArticlesFromHTML(body, h, since), nil
}

// --- Profile HTML parsing ---

var (
	// reLDJSON matches <script type="application/ld+json">…</script>.
	reLDJSON = regexp.MustCompile(`(?is)<script[^>]+type=["']application/ld\+json["'][^>]*>(.*?)</script>`)
	// reDatalet matches a single BPR datalet <code id="datalet-bpr-…">…</code>
	// block. The body of each datalet is wrapped in an HTML comment.
	reDatalet = regexp.MustCompile(`(?is)<code[^>]*id=["']datalet-bpr-guid-[^"']+["'][^>]*>(.*?)</code>`)
	// reDataletComment strips the surrounding `<!--` / `-->` wrapper.
	reDataletComment = regexp.MustCompile(`(?s)^\s*<!--(.*?)-->\s*$`)
	// reApolloState matches the inline `window.__APOLLO_STATE__ = {...};`
	// assignment. Trailing semicolon optional.
	reApolloState = regexp.MustCompile(`(?is)window\.__APOLLO_STATE__\s*=\s*(\{.*?\})\s*;\s*</script>`)
)

// followerKeys are the JSON keys we treat as a follower count. Order
// doesn't matter — extraction returns the first numeric hit.
var followerKeys = map[string]struct{}{
	"followerCount":       {},
	"followersCount":      {},
	"numFollowers":        {},
	"numberOfFollowers":   {},
	"followers":           {},
	"userInteractionCount": {}, // schema.org InteractionCounter
}

// connectionKeys mirror followerKeys for connections.
var connectionKeys = map[string]struct{}{
	"connectionCount":     {},
	"connectionsCount":    {},
	"numConnections":      {},
	"numberOfConnections": {},
}

// stringKeysOfInterest are the textual fields we copy into Raw when we
// stumble across them during the deep walk.
var stringKeysOfInterest = map[string]string{
	"headline":       "headline",
	"firstName":      "first_name",
	"lastName":       "last_name",
	"locationName":   "location",
	"geoLocationName": "location",
	"industryName":   "industry",
	"occupation":     "occupation",
}

// parseProfileHTML extracts a best-effort ChannelSnapshot from a
// (authenticated) profile page. We try the BPR datalets first, then
// __APOLLO_STATE__, then LD-JSON, accumulating Raw fields as we go and
// short-circuiting the moment we find a follower count.
func parseProfileHTML(body, handle, profileURL string) shared.ChannelSnapshot {
	snap := shared.ChannelSnapshot{
		Platform:  shared.PlatformLinkedIn,
		Handle:    handle,
		URL:       profileURL,
		FetchedAt: time.Now().UTC(),
		Raw:       map[string]interface{}{"source": "authenticated_html"},
	}

	// Try BPR datalets (most reliable surface today).
	if applyDatalets(&snap, body) {
		return snap
	}
	// Apollo state.
	if applyApolloState(&snap, body) {
		return snap
	}
	// LD-JSON.
	if applyLDJSON(&snap, body) {
		return snap
	}

	// Fallback: regex-scrape the React Server Components serialized output
	// for `N connections` / `N followers`. LinkedIn's current personal-
	// profile render embeds the counts as plain text inside the RSC payload
	// (e.g. `<p>218 connections</p>` and `\"218 connections\"`).
	if applyTextCounters(&snap, body) {
		return snap
	}

	// Nothing matched. Surface a diagnostic so operators can tell this
	// apart from "auth wall" or "real zero-follower account".
	snap.Raw["fetch_note"] = "no structured data extracted (datalets/apollo/ldjson/text-counters all empty); LinkedIn DOM may have rotated"
	return snap
}

// applyTextCounters scrapes the rendered HTML for the visible counters that
// LinkedIn prints on the profile top-card: "<N> connections" and/or
// "<N> followers" (premium accounts only). Connection count is the closest
// "audience" proxy for non-premium personal profiles, which is the common
// case for w_popularity users.
//
// Numbers may be plain ("218"), comma-grouped ("1,234"), or use the LinkedIn
// "500+" suffix — we strip the plus.
var reTextCounters = regexp.MustCompile(`([\d,]+)\+?\s+(connection|follower)s?\b`)

func applyTextCounters(snap *shared.ChannelSnapshot, body string) bool {
	// Normalize common HTML entities that LinkedIn inserts between the
	// numeric counter and the label (e.g. "218&nbsp;connections").
	clean := strings.ReplaceAll(body, "&nbsp;", " ")
	matches := reTextCounters.FindAllStringSubmatch(clean, -1)
	if len(matches) == 0 {
		return false
	}
	var connections, followers int64
	for _, m := range matches {
		nRaw := strings.ReplaceAll(m[1], ",", "")
		n, err := strconv.ParseInt(nRaw, 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(m[2]) {
		case "connection":
			if n > connections {
				connections = n
			}
		case "follower":
			if n > followers {
				followers = n
			}
		}
	}
	if connections == 0 && followers == 0 {
		return false
	}
	// Followers preferred when present (premium), else connections.
	if followers > 0 {
		snap.Followers = followers
	} else {
		snap.Followers = connections
	}
	if connections > 0 {
		snap.Raw["connection_count"] = connections
	}
	if followers > 0 {
		snap.Raw["follower_count"] = followers
	}
	snap.Raw["extracted_via"] = "text_counters"
	return true
}

// applyDatalets walks every BPR datalet body, decodes its JSON, and
// deep-walks the tree. Returns true once a follower count is found.
func applyDatalets(snap *shared.ChannelSnapshot, body string) bool {
	got := false
	for _, m := range reDatalet.FindAllStringSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}
		inner := m[1]
		// Datalet bodies are wrapped in <!-- … -->. Strip the wrapper.
		if cm := reDataletComment.FindStringSubmatch(inner); len(cm) == 2 {
			inner = cm[1]
		}
		inner = strings.TrimSpace(inner)
		if inner == "" {
			continue
		}
		var obj interface{}
		if err := json.Unmarshal([]byte(inner), &obj); err != nil {
			continue
		}
		if walkForProfile(snap, obj) {
			got = true
			snap.Raw["source"] = "bpr_datalet"
			// Don't return early: keep walking so Raw keeps
			// accumulating from later datalets.
		}
	}
	return got
}

// applyApolloState extracts window.__APOLLO_STATE__ and walks it.
func applyApolloState(snap *shared.ChannelSnapshot, body string) bool {
	m := reApolloState.FindStringSubmatch(body)
	if len(m) < 2 {
		return false
	}
	var obj interface{}
	if err := json.Unmarshal([]byte(m[1]), &obj); err != nil {
		return false
	}
	if walkForProfile(snap, obj) {
		snap.Raw["source"] = "apollo_state"
		return true
	}
	return false
}

// applyLDJSON walks each ld+json block.
func applyLDJSON(snap *shared.ChannelSnapshot, body string) bool {
	got := false
	for _, m := range reLDJSON.FindAllStringSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}
		raw := strings.TrimSpace(m[1])
		raw = strings.TrimPrefix(raw, "\ufeff") // strip UTF-8 BOM if present

		var obj interface{}
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			continue
		}
		if walkForProfile(snap, obj) {
			snap.Raw["source"] = "ldjson"
			got = true
		}
	}
	return got
}

// walkForProfile is the deep JSON walker. It descends maps and slices
// looking for any followerKeys / connectionKeys / stringKeysOfInterest
// and populates snap as it goes. Returns true once it has populated
// at least a follower count.
func walkForProfile(snap *shared.ChannelSnapshot, node interface{}) bool {
	switch v := node.(type) {
	case map[string]interface{}:
		// Special-case schema.org InteractionCounter: only treat
		// userInteractionCount as followers when the sibling "name" or
		// "interactionType" says so. Otherwise it could be e.g. likes.
		if _, ok := v["userInteractionCount"]; ok {
			name, _ := v["name"].(string)
			itype := stringOrEmpty(v["interactionType"])
			if strings.EqualFold(name, "followers") ||
				strings.Contains(strings.ToLower(itype), "follow") {
				if n, ok := numericField(v["userInteractionCount"]); ok && snap.Followers == 0 {
					snap.Followers = n
				}
			}
		}

		for k, val := range v {
			// Numeric fields of interest.
			if _, ok := followerKeys[k]; ok && k != "userInteractionCount" {
				if n, ok := numericField(val); ok && snap.Followers == 0 {
					snap.Followers = n
				}
			}
			if _, ok := connectionKeys[k]; ok {
				if n, ok := numericField(val); ok {
					if _, exists := snap.Raw["connections_count"]; !exists {
						snap.Raw["connections_count"] = n
					}
				}
			}
			// Textual fields of interest.
			if rawKey, ok := stringKeysOfInterest[k]; ok {
				if s, ok := val.(string); ok && s != "" {
					if _, exists := snap.Raw[rawKey]; !exists {
						snap.Raw[rawKey] = s
					}
				}
			}
			// currentCompany / company is sometimes a string and
			// sometimes a nested object with a `name` field.
			if k == "currentCompany" || k == "companyName" {
				switch cv := val.(type) {
				case string:
					if cv != "" {
						if _, exists := snap.Raw["current_company"]; !exists {
							snap.Raw["current_company"] = cv
						}
					}
				case map[string]interface{}:
					if name, ok := cv["name"].(string); ok && name != "" {
						if _, exists := snap.Raw["current_company"]; !exists {
							snap.Raw["current_company"] = name
						}
					}
				}
			}
			// Recurse.
			walkForProfile(snap, val)
		}
	case []interface{}:
		for _, item := range v {
			walkForProfile(snap, item)
		}
	}
	return snap.Followers > 0
}

// --- Article extraction (best-effort posts) ---

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

	// Attach the li_at session cookie. We build the Cookie header
	// directly rather than using http.Client.Jar to keep the parser
	// stateless across calls.
	var cookies []string
	if p.cfg.LIATCookie != "" {
		cookies = append(cookies, "li_at="+p.cfg.LIATCookie)
	}
	if p.cfg.JSESSIONID != "" {
		// LinkedIn wraps JSESSIONID in quotes in real browsers. Match that.
		js := p.cfg.JSESSIONID
		if !strings.HasPrefix(js, "\"") {
			js = "\"" + strings.Trim(js, "\"") + "\""
		}
		cookies = append(cookies, "JSESSIONID="+js)
	}
	if len(cookies) > 0 {
		req.Header.Set("Cookie", strings.Join(cookies, "; "))
	}

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("linkedin: %w: %v", shared.ErrTransient, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 16 MiB cap
	if err != nil {
		return "", resp.StatusCode, fmt.Errorf("linkedin: %w: read body: %v", shared.ErrTransient, err)
	}
	return string(b), resp.StatusCode, nil
}

// --- helpers ---

// isAuthWallHTML detects LinkedIn's various auth-wall responses.
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

func stringOrEmpty(v interface{}) string {
	s, _ := stringOrFirst(v)
	return s
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
