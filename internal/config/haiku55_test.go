package config

import (
	"math"
	"testing"
)

// Haiku 5.5 is priced by prompt length: $0.10 in, $0.50 out and $0.01 a cache
// read per million up to 100,000 prompt tokens, five times that over it.
func TestHaiku55IsPricedByPromptLength(t *testing.T) {
	c := Default()
	short, ok := c.PriceTokens("claude-haiku-5-5", 512, 656, 6234, 0)
	if !ok {
		t.Fatal("claude-haiku-5-5 has no price")
	}
	if want := (512*0.10 + 656*0.50 + 6234*0.01) / 1e6; math.Abs(short-want) > 1e-12 {
		t.Fatalf("short prompt = %v, want %v", short, want)
	}
	long, _ := c.PriceTokens("claude-haiku-5-5", 1000, 1000, 120_000, 0)
	if want := (1000*0.50 + 1000*2.50 + 120_000*0.05) / 1e6; math.Abs(long-want) > 1e-12 {
		t.Fatalf("long prompt = %v, want %v", long, want)
	}
	opus, _ := c.PriceTokens("claude-opus-5-5", 1000, 0, 500_000, 0)
	if want := (1000*4 + 500_000*0.20) / 1e6; math.Abs(opus-want) > 1e-12 {
		t.Fatalf("a model with one price changed with prompt length: %v, want %v", opus, want)
	}
}
