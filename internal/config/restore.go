package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/atomicfile"
	"github.com/andrewbakercloudscale/claude-burst/internal/backup"
)

// ErrConfigLoads is RestoreLastGood's answer when there is nothing to do.
var ErrConfigLoads = errors.New("config.json loads as it is: nothing restored")

// Restored is what RestoreLastGood did.
type Restored struct {
	From string // the backup now in force
	Kept string // where the file that would not load was put
}

// RestoreLastGood puts the newest backup that loads in place of a
// config.json that does not, and keeps the broken file beside the backups.
// It is the way out of the one fault nothing else could fix: the gateway
// exits on a config it cannot read, launchd starts it again, and Restart
// and Repair both stop on the same file. Every save already leaves the
// backups this reads (see internal/backup).
func RestoreLastGood() (Restored, error) {
	p, err := ConfigPath()
	if err != nil {
		return Restored{}, err
	}
	if _, err := loadFile(p); err == nil {
		return Restored{}, ErrConfigLoads
	}
	dir, err := backup.Dir()
	if err != nil {
		return Restored{}, err
	}
	base := filepath.Base(p)
	// The restore point first: it is what the last save wrote. Then the
	// history, newest first.
	candidates := []string{filepath.Join(dir, base+".latest.bak")}
	older, _ := filepath.Glob(filepath.Join(dir, base+".*.bak"))
	sort.Slice(older, func(i, j int) bool { return modTime(older[i]).After(modTime(older[j])) })
	for _, c := range older {
		if c != candidates[0] {
			candidates = append(candidates, c)
		}
	}
	for _, c := range candidates {
		if _, err := loadFile(c); err != nil {
			continue
		}
		good, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		kept := filepath.Join(dir, base+".would-not-load-"+time.Now().Format("20060102-150405")+".bak")
		if bad, err := os.ReadFile(p); err == nil {
			if err := os.MkdirAll(dir, 0o700); err == nil {
				_ = os.WriteFile(kept, bad, 0o600)
			}
		}
		if err := atomicfile.Write(p, good, 0o600); err != nil {
			return Restored{}, err
		}
		return Restored{From: c, Kept: kept}, nil
	}
	return Restored{}, fmt.Errorf("no backup of %s in %s loads either (%d tried)", base, dir, len(candidates))
}

func modTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
