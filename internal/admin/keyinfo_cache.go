package admin

import (
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
)

// keyInfoTTL is how long /api/state answers "is the secondary's key there"
// from its last look. Each look runs /usr/bin/security, and the dashboard
// and console poll /api/state every few seconds: on 24 Nov 2026 System
// Sentinel counted claude-burst among the Mac's top process starters.
const keyInfoTTL = 30 * time.Second

type keyInfoCache struct {
	mu   sync.Mutex
	key  string
	at   time.Time
	info keychain.Info
}

// get returns the cached answer for service and envVar while it is fresh,
// otherwise asks look and keeps its answer.
func (c *keyInfoCache) get(service, envVar string, now time.Time, look func(service, envVar string) keychain.Info) keychain.Info {
	k := service + "\x00" + envVar
	c.mu.Lock()
	if c.key == k && !c.at.IsZero() && now.Sub(c.at) < keyInfoTTL {
		info := c.info
		c.mu.Unlock()
		return info
	}
	c.mu.Unlock()
	info := look(service, envVar)
	c.mu.Lock()
	c.key, c.at, c.info = k, now, info
	c.mu.Unlock()
	return info
}

// forget drops the cached answer, so a key just saved shows at once.
func (c *keyInfoCache) forget() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}
