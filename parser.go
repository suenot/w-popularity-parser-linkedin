// Package parser implements the w_popularity linkedin adapter.
//
// Status: STUB. Returns shared.ErrNotImplemented.
//
// Strategy:
//   primary:  HTML scrape of public profile
//   fallback: camoufox
package parser

import (
	"context"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

// Config controls runtime behaviour. Add platform-specific fields here.
type Config struct {
	// Token, cookie, or API key — fill in per implementation.
	Credential string
	// HTTPTimeout caps every outbound call.
	HTTPTimeout time.Duration
	// CamoufoxURL is set when falling back to browser-based scraping.
	CamoufoxURL string
}

// New constructs a stubbed parser. Real impl is pending.
func New(cfg Config) *LinkedInParser { return &LinkedInParser{cfg: cfg} }

type LinkedInParser struct{ cfg Config }

func (p *LinkedInParser) Platform() shared.Platform { return shared.PlatformLinkedIn }

func (p *LinkedInParser) FetchChannel(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	return shared.ChannelSnapshot{}, shared.ErrNotImplemented
}

func (p *LinkedInParser) FetchRecentPosts(ctx context.Context, handle string, since time.Time) ([]shared.PostSnapshot, error) {
	return nil, shared.ErrNotImplemented
}
