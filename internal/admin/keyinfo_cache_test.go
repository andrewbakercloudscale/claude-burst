package admin

import (
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
)

func TestKeyInfoIsAskedOncePerTTL(t *testing.T) {
	var c keyInfoCache
	looks := 0
	look := func(string, string) keychain.Info { looks++; return keychain.Info{Present: true} }
	t0 := time.Now()
	for i := 0; i < 10; i++ {
		c.get("svc", "ENV", t0.Add(time.Duration(i)*time.Second), look)
	}
	if looks != 1 {
		t.Fatalf("10 polls within the TTL asked the Keychain %d times, want 1", looks)
	}
	c.get("svc", "ENV", t0.Add(keyInfoTTL+time.Second), look)
	if looks != 2 {
		t.Fatalf("after the TTL it should ask again, looks=%d", looks)
	}
	c.get("other", "ENV", t0.Add(keyInfoTTL+2*time.Second), look)
	if looks != 3 {
		t.Fatalf("another service must not get the cached answer, looks=%d", looks)
	}
	c.forget()
	c.get("other", "ENV", t0.Add(keyInfoTTL+3*time.Second), look)
	if looks != 4 {
		t.Fatalf("after forget it should ask again, looks=%d", looks)
	}
}
