# w-popularity-parser-linkedin

`linkedin` parser for [w_popularity](https://github.com/suenot/w-popularity).

## Strategy

- **Primary:** plain HTTP `GET https://www.linkedin.com/in/<handle>/`,
  then extract a schema.org `Person` / `ProfilePage` node from any
  `<script type="application/ld+json">` block. Followers are read from
  `interactionStatistic[name="followers"].userInteractionCount`.
- **Fallback:** camoufox (browser-driven) via CDP — currently a stub.
  The primary→fallback branch is wired so swapping in a real
  implementation is a one-function change (`fetchViaCamoufox`).

## Production reality

**LinkedIn will almost always block unauthenticated scraping.** A
typical `curl -A 'Mozilla/5.0' https://www.linkedin.com/in/<handle>/`
returns HTTP **999** with a tiny HTML body that JS-redirects to
`/authwall`. The parser detects this (status `999`/`403`, or
`authwall*` markers in the body) and returns `shared.ErrAuth` with a
hint to configure `Config.CamoufoxURL`.

For any production deployment, you almost certainly need the camoufox
fallback wired in. The plain-HTML path is kept for:

1. Residential / authenticated IPs where LinkedIn occasionally serves
   the real public profile (with LD-JSON).
2. CI / test environments that mock the response.

## Configuration

```go
import parser "github.com/suenot/w-popularity-parser-linkedin"

p := parser.New(parser.Config{
    HTTPTimeout: 15 * time.Second,
    // Optional: CDP endpoint of a running camoufox instance.
    // When set, the parser falls back to it after the HTML attempt is
    // gated by the auth wall.
    CamoufoxURL: os.Getenv("CAMOUFOX_CDP"),
})

snap, err := p.FetchChannel(ctx, "suenot")
```

## Error mapping

| Condition                              | Error                  |
|----------------------------------------|------------------------|
| HTTP 999 / 403 / authwall HTML         | `shared.ErrAuth`       |
| HTTP 404                               | `shared.ErrNotFound`   |
| HTTP 429                               | `shared.ErrRateLimited`|
| HTTP 5xx / transport error             | `shared.ErrTransient`  |
| 2xx with no LD-JSON                    | snapshot, `Followers=0`|

`FetchRecentPosts` is best-effort: logged-out LinkedIn profiles rarely
expose post lists. When LD-JSON contains `Article` items under
`subjectOf` / `publishingPrinciples` / `mainEntity`, they are
extracted; otherwise it returns `(nil, nil)`.

## License

MIT
