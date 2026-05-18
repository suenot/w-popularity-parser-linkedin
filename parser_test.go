package parser

import (
	"context"
	"errors"
	"testing"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

func TestStub_ReturnsNotImplemented(t *testing.T) {
	p := New(Config{})
	if p.Platform() != shared.PlatformLinkedIn {
		t.Fatalf("platform mismatch: %s", p.Platform())
	}
	_, err := p.FetchChannel(context.Background(), "x")
	if !errors.Is(err, shared.ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented, got %v", err)
	}
	_, err = p.FetchRecentPosts(context.Background(), "x", time.Now())
	if !errors.Is(err, shared.ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented, got %v", err)
	}
}
