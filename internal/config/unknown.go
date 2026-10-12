package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
)

// retiredKeys are keys this version no longer reads but which config.json
// files written by an earlier one still carry. They are not mistakes, so
// UnknownKeys leaves them out: reporting them would nag every user who
// upgraded about a file they never edited.
var retiredKeys = map[string]bool{
	"shunt":                             true, // token shunting, removed 2026-10-02
	"primary_compaction.warn_at_tokens": true, // derived from warn_at_percent since
}

// UnknownKeys lists the keys in config.json that this version does not read,
// as dotted paths ("intercept.mdoe"), sorted. Nil when there are none or
// there is no file.
//
// Load ignores such a key, and until 2026-10-12 nothing said so: a typo in a
// key left the default in force with the file looking as if it had been set.
// They are reported and never refused, on purpose. After a rollback an older
// binary opens a config.json written by a newer one, and a gateway that would
// not start on a key it does not know would turn a rollback into an outage.
//
// It is apart from Load because Load runs on every dashboard request: the
// caller that wants to say this once (the gateway at start, `configure`)
// asks once.
func UnknownKeys() ([]string, error) {
	p, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return unknownKeys(b)
}

// UnknownKeysMessage words UnknownKeys' result for a log line or an
// on-screen notice. "" when there is nothing to say.
func UnknownKeysMessage(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return fmt.Sprintf("config.json has %d setting(s) this version of Claude Burst does not read: %s. "+
		"Each is ignored and its default applies. Check the spelling against docs/configuration.md; "+
		"a setting from a newer version is expected after a rollback.", len(keys), strings.Join(keys, ", "))
}

func unknownKeys(b []byte) ([]string, error) {
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	var out []string
	collectUnknown(raw, reflect.TypeOf(Config{}), "", &out)
	sort.Strings(out)
	return out, nil
}

// collectUnknown walks the decoded file beside the type it is decoded into
// and records every object key with no field to land in. The fields are read
// from Config's own json tags, so a new setting needs nothing added here.
func collectUnknown(v any, t reflect.Type, path string, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" || !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			// encoding/json matches a key to a field whatever its case.
			fields[strings.ToLower(name)] = f.Type
		}
		for key, child := range obj {
			p := key
			if path != "" {
				p = path + "." + key
			}
			ft, known := fields[strings.ToLower(key)]
			if !known {
				if !retiredKeys[p] {
					*out = append(*out, p)
				}
				continue
			}
			collectUnknown(child, ft, p, out)
		}
	case reflect.Map:
		// The keys are the user's own (model names, repository paths); only
		// what is inside each value has a shape to check.
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		for key, child := range obj {
			collectUnknown(child, t.Elem(), path+"."+key, out)
		}
	case reflect.Slice, reflect.Array:
		list, ok := v.([]any)
		if !ok {
			return
		}
		for i, child := range list {
			collectUnknown(child, t.Elem(), fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}
