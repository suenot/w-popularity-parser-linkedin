# w-popularity-parser-linkedin

`linkedin` parser for [w_popularity](https://github.com/suenot/w-popularity).

**Status:** stub. `FetchChannel` and `FetchRecentPosts` return `shared.ErrNotImplemented`.

## Strategy

- **Primary:** HTML scrape of public profile
- **Fallback:** camoufox

## Usage

```go
import parser "github.com/suenot/w-popularity-parser-linkedin"

p := parser.New(parser.Config{Credential: os.Getenv("CRED")})
snap, err := p.FetchChannel(ctx, handle)
```

## License

MIT
