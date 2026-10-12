package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", "")
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// A typo in a key used to be ignored with nothing said: the default applied
// and the file looked as if the setting had been made. The file must still
// load (an older binary opening a newer config after a rollback depends on
// that), and the keys must be named, at any depth.
func TestUnknownKeysAreReportedAndStillLoad(t *testing.T) {
	writeConfig(t, `{
  "listen": "127.0.0.1:7999",
  "admin_lisen": "127.0.0.1:7001",
  "intercept": {"mode": "transparent", "hots": "api.example.com"},
  "pricing": {"my-vendor/some.model": {"input_per_mtok": 1.5, "output_per_mtok": 6, "cache_read_mtok": 0.1}},
  "setting_from_a_newer_version": {"anything": [1, 2, 3]}
}`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a config with unknown keys must still load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:7999" || cfg.AdminListen != Default().AdminListen || cfg.Intercept.Host != "api.anthropic.com" {
		t.Fatalf("known keys must apply and misspelt ones leave the default: listen=%q admin=%q host=%q",
			cfg.Listen, cfg.AdminListen, cfg.Intercept.Host)
	}
	got, err := UnknownKeys()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"admin_lisen",
		"intercept.hots",
		"pricing.my-vendor/some.model.cache_read_mtok",
		"setting_from_a_newer_version",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnknownKeys =\n  %v\nwant\n  %v", got, want)
	}
	msg := UnknownKeysMessage(got)
	for _, k := range want {
		if !strings.Contains(msg, k) {
			t.Errorf("message does not name %q: %s", k, msg)
		}
	}
	if UnknownKeysMessage(nil) != "" {
		t.Error("nothing unknown must be nothing to say")
	}
}

// Every key Save writes is a key this version reads. If this fails, a field
// was given a shape collectUnknown does not follow, and every user would be
// told their own config.json is misspelt.
func TestNothingSaveWritesIsReportedUnknown(t *testing.T) {
	cfg := Default()
	cfg.Intercept.Mode = InterceptTransparent
	cfg.ResolveRoutes()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unknownKeys(b)
	if err != nil || len(got) != 0 {
		t.Fatalf("a config written by this version reported unknown keys %v (%v)", got, err)
	}
	// And one with every field present, zero or not, so a block that
	// omitempty leaves out of the default file is covered too.
	if got := unknownKeys2(t, everyKey(reflect.TypeOf(Config{}))); len(got) != 0 {
		t.Fatalf("known keys reported unknown: %v", got)
	}
}

func unknownKeys2(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unknownKeys(b)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// everyKey builds the JSON object that names every field of t, with one
// entry in each map and list, from the json tags alone.
func everyKey(t reflect.Type) any {
	switch t.Kind() {
	case reflect.Pointer:
		return everyKey(t.Elem())
	case reflect.Struct:
		obj := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" || !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			obj[name] = everyKey(f.Type)
		}
		return obj
	case reflect.Map:
		return map[string]any{"any key": everyKey(t.Elem())}
	case reflect.Slice, reflect.Array:
		return []any{everyKey(t.Elem())}
	}
	return nil
}

// Keys an earlier version wrote and this one dropped are not mistakes, and
// are in config.json files nobody edited: they are not reported.
func TestRetiredKeysAreNotReported(t *testing.T) {
	writeConfig(t, `{"shunt":{"read":true,"write":true,"min_lines":350},"primary_compaction":{"enabled":true,"warn_at_tokens":120000}}`)
	got, err := UnknownKeys()
	if err != nil || len(got) != 0 {
		t.Fatalf("UnknownKeys = %v, %v; want none", got, err)
	}
}

func TestUnknownKeysWithNoFileOrABrokenOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got, err := UnknownKeys(); err != nil || got != nil {
		t.Fatalf("no file: %v, %v; want nothing and no error", got, err)
	}
	writeConfig(t, `{"listen": `)
	if _, err := UnknownKeys(); err == nil {
		t.Fatal("a file that does not parse is an error, not an empty list")
	}
}

// A built-in price that was later corrected reaches a config.json that holds
// the old one: Save writes the built-in prices into the file, where they won
// over every later default. A price the user set to anything else is theirs.
func TestACorrectedDefaultPriceReachesAnExistingConfig(t *testing.T) {
	writeConfig(t, `{"pricing":{
  "claude-sonnet-5-5": {"input_per_mtok": 2, "output_per_mtok": 10, "cache_read_per_mtok": 0.2},
  "claude-fable-5-1":  {"input_per_mtok": 10, "output_per_mtok": 50},
  "claude-opus-5-5":   {"input_per_mtok": 4, "output_per_mtok": 20, "cache_read_per_mtok": 0.33},
  "my-vendor/model":   {"input_per_mtok": 2, "output_per_mtok": 10, "cache_read_per_mtok": 0.2}
}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	def := Default().Pricing
	for _, m := range []string{"claude-sonnet-5-5", "claude-fable-5-1"} {
		if cfg.Pricing[m] != def[m] {
			t.Errorf("%s = %+v, still the superseded default; want the current one %+v", m, cfg.Pricing[m], def[m])
		}
	}
	if got := cfg.Pricing["claude-opus-5-5"].CacheReadPerMTok; got != 0.33 {
		t.Errorf("a price the user set was replaced: opus cache read = %v, want 0.33", got)
	}
	if got := cfg.Pricing["my-vendor/model"].CacheReadPerMTok; got != 0.2 {
		t.Errorf("another model at the same numbers is not a superseded default: got %v", got)
	}

	// A user's own price for a corrected model, not equal to the old
	// default, stays.
	writeConfig(t, `{"pricing":{"claude-sonnet-5-5": {"input_per_mtok": 2, "output_per_mtok": 10, "cache_read_per_mtok": 0.15}}}`)
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Pricing["claude-sonnet-5-5"].CacheReadPerMTok; got != 0.15 {
		t.Errorf("a deliberate price was replaced: got %v, want 0.15", got)
	}

	// And the correction is written back by the next save, so the file
	// stops holding the old price.
	writeConfig(t, `{"pricing":{"claude-sonnet-5-5": {"input_per_mtok": 2, "output_per_mtok": 10, "cache_read_per_mtok": 0.2}}}`)
	if err := Update(func(*Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	p, _ := ConfigPath()
	b, _ := os.ReadFile(p)
	var onDisk struct {
		Pricing map[string]ModelPrice `json:"pricing"`
	}
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Pricing["claude-sonnet-5-5"] != def["claude-sonnet-5-5"] {
		t.Errorf("after a save the file holds %+v, want %+v", onDisk.Pricing["claude-sonnet-5-5"], def["claude-sonnet-5-5"])
	}
}

// The superseded list is only right if each entry is a model that still has
// a default and is not that default: an entry equal to the current price
// does nothing, and one for a model with no default would delete its price.
func TestSupersededPricingNamesOnlyCorrectedDefaults(t *testing.T) {
	def := Default().Pricing
	for model, old := range supersededPricing {
		cur, ok := def[model]
		if !ok {
			t.Errorf("%s has a superseded price and no current default", model)
			continue
		}
		for _, was := range old {
			if was == cur {
				t.Errorf("%s: superseded price %+v is the current default", model, was)
			}
		}
	}
}

func TestLoopbackListen(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:7777":    true,
		"127.0.0.2:7777":    true,
		"localhost:7788":    true,
		"[::1]:7777":        true,
		":7777":             false, // every interface
		"0.0.0.0:7777":      false,
		"[::]:7777":         false,
		"192.168.1.20:7777": false,
		"example.com:7777":  false,
		"127.0.0.1":         false, // no port: not an address to listen on
		"":                  false,
	} {
		if got := LoopbackListen(addr); got != want {
			t.Errorf("LoopbackListen(%q) = %v, want %v", addr, got, want)
		}
	}
}
