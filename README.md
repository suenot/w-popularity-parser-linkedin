# w-popularity-parser-linkedin

`linkedin` parser for [w_popularity](https://github.com/suenot/w-popularity).

## Strategy

LinkedIn aggressively blocks unauthenticated scraping. A logged-out
`curl https://www.linkedin.com/in/<handle>/` returns **HTTP 999** with
a tiny HTML body that JS-redirects to `/authwall`. Even the rare 200
responses carry an empty schema.org `Person` LD-JSON (no follower
count), so the previous "public LD-JSON" approach is effectively dead.

This parser takes two paths, in priority order:

1. **Authenticated HTML scrape via `li_at` session cookie (primary).**
   When `Config.LIATCookie` is set we attach `Cookie: li_at=<token>`
   (and an optional `JSESSIONID`) to the request. LinkedIn then serves
   the real profile page HTML. We pull structured data out of it via
   three tolerant extractors, in order:
   - **BPR datalets** (`<code id="datalet-bpr-guid-…"><!--{…}--></code>`) —
     LinkedIn's own browser-proxy payload format. **Most reliable
     surface today**; carries `followerCount`, `connectionCount`,
     `headline`, `locationName`, `industryName`, `currentCompany`, etc.
   - **`window.__APOLLO_STATE__`** — a JS global emitted by some
     render paths. Walked the same way.
   - **`<script type="application/ld+json">` with `@type=Person`** —
     normally empty on logged-out fetches, but populated when
     authenticated.

   The extractor deep-walks every decoded JSON tree looking for known
   keys (`followerCount`, `connectionsCount`, `headline`, `location`,
   `industryName`, …) and stops at the first follower hit. Missing
   fields downgrade to `Followers=0` plus a diagnostic
   `Raw["fetch_note"]`.

2. **camoufox via Playwright (fallback — skeleton).** When
   `Config.CamoufoxURL` is set and `LIATCookie` is empty, we'd dial the
   CDP endpoint and pull the rendered HTML through the same extractor.
   Currently a stub (`fetchViaCamoufox` returns "camoufox path not
   implemented"); the branching is in place for a drop-in replacement.

## Getting an `li_at` cookie

1. Log in to https://www.linkedin.com in a regular browser.
2. Open DevTools → **Application** → **Cookies** → `https://www.linkedin.com`.
3. Copy the value of the `li_at` cookie (it looks like
   `AQEDA…` followed by ~200 characters).
4. Optionally copy `JSESSIONID` too (LinkedIn wraps it in quotes; the
   parser will re-add the quotes if you strip them).

**Caveat:** `li_at` cookies expire roughly every **365 days**, and may
be invalidated sooner if LinkedIn detects unusual activity (different
IP / UA, multiple concurrent sessions, etc). When the cookie is
rejected the parser returns
`shared.ErrAuth` with the message `"li_at cookie expired"` — that's
your cue to refresh it.

For production deployments behind a single source IP, rotate the
cookie across a small pool of accounts (and a residential proxy) to
keep one expiry from taking down the pipeline.

## Configuration

```go
import parser "github.com/suenot/w-popularity-parser-linkedin"

p := parser.New(parser.Config{
    LIATCookie:  os.Getenv("LINKEDIN_LI_AT"),     // primary auth
    JSESSIONID:  os.Getenv("LINKEDIN_JSESSIONID"), // optional
    HTTPTimeout: 15 * time.Second,
    CamoufoxURL: os.Getenv("CAMOUFOX_URL"),       // optional fallback (stub)
})

snap, err := p.FetchChannel(ctx, "suenot")
// snap.Followers, snap.Raw["headline"], snap.Raw["location"], ...
```

## Error mapping

| Condition                                          | Error                                                       |
|----------------------------------------------------|-------------------------------------------------------------|
| `LIATCookie` set, HTTP 999 / 403 / authwall body   | `ErrAuth: li_at cookie expired`                             |
| `LIATCookie` empty, no `CamoufoxURL`               | `ErrAuth: set LI_AT cookie or CAMOUFOX_URL`                 |
| HTTP 404                                           | `ErrNotFound`                                               |
| HTTP 429                                           | `ErrRateLimited`                                            |
| HTTP 5xx / transport error                         | `ErrTransient`                                              |
| 2xx with no extractable structure                  | snapshot, `Followers=0`, `Raw["fetch_note"]` set            |

`FetchRecentPosts` is best-effort. Profile pages don't expose a
lifetime post list without hitting `/detail/recent-activity/shares/`
as the logged-in user; we extract whatever `Article` items happen to
appear in LD-JSON and return `(nil, nil)` for everything else.

## License

MIT
