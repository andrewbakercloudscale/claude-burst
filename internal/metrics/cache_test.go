package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func eventLine(t *testing.T, id string) string {
	t.Helper()
	b, err := json.Marshal(Event{Time: time.Now(), RequestID: id, Route: "anthropic", HTTPStatus: 200})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func ids(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	if err := scanEvents(path, func(e Event) { out = append(out, e.RequestID) }); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameIDs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// The history is parsed once, then only what was appended, and a file
// replaced under the same name, as rotation does, is read afresh.
func TestHeldEventsFollowTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	sameIDs(t, "no file", ids(t, path))

	appendTo(t, path, eventLine(t, "a")+eventLine(t, "b"))
	sameIDs(t, "first read", ids(t, path), "a", "b")
	appendTo(t, path, eventLine(t, "c"))
	sameIDs(t, "after an append", ids(t, path), "a", "b", "c")

	// The writer part way through a line: not read until it is whole.
	line := eventLine(t, "d")
	appendTo(t, path, line[:10])
	sameIDs(t, "half a line", ids(t, path), "a", "b", "c")
	appendTo(t, path, line[10:])
	sameIDs(t, "the line finished", ids(t, path), "a", "b", "c", "d")

	// Rotation: the file moves away and a new one takes the name.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, eventLine(t, "e"))
	sameIDs(t, "the new file", ids(t, path), "e")
	sameIDs(t, "the rotated one", ids(t, path+".1"), "a", "b", "c", "d")

	// Rewritten in place, same inode: shorter, then different from the start.
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "emptied", ids(t, path))
	if err := os.WriteFile(path, []byte(eventLine(t, "f")+eventLine(t, "g")), 0o600); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "rewritten", ids(t, path), "f", "g")
	if err := os.WriteFile(path, []byte(eventLine(t, "h")+eventLine(t, "i")+eventLine(t, "j")), 0o600); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "rewritten longer", ids(t, path), "h", "i", "j")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "removed", ids(t, path))
}

// A file written by hand without a final newline still has its last event
// read, and the event is read again, not twice, once the line is finished.
func TestALastLineWithoutANewlineIsRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	a, b := eventLine(t, "a"), eventLine(t, "b")
	appendTo(t, path, a+b[:len(b)-1])
	sameIDs(t, "no final newline", ids(t, path), "a", "b")
	sameIDs(t, "asked again", ids(t, path), "a", "b")
	appendTo(t, path, "\n"+eventLine(t, "c"))
	sameIDs(t, "finished", ids(t, path), "a", "b", "c")
}

// Readers walk the events while the gateway appends: run with -race.
func TestHeldEventsReadWhileAppended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	appendTo(t, path, eventLine(t, "0"))
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				n := 0
				_ = scanEvents(path, func(e Event) { n++ })
				if n == 0 {
					t.Error("a read saw no events")
					return
				}
			}
		}()
	}
	for i := 1; i <= 200; i++ {
		appendTo(t, path, eventLine(t, fmt.Sprint(i)))
	}
	wg.Wait()
	if got := len(ids(t, path)); got != 201 {
		t.Errorf("got %d events, want 201", got)
	}
}

// A price learned after an event was held reaches it: prices are applied
// on each read, not kept with the event.
func TestHeldEventsAreRepricedOnEachRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	b, _ := json.Marshal(Event{Time: time.Now(), Route: "anthropic", Model: "m", InputTokens: 10, PricingUnknown: true})
	appendTo(t, path, string(b)+"\n")
	SetPricer(nil)
	t.Cleanup(func() { SetPricer(nil) })
	var usd float64
	_ = scanEvents(path, func(e Event) { usd = e.APIEquivalentUSD })
	if usd != 0 {
		t.Fatalf("priced with no pricer: %v", usd)
	}
	SetPricer(func(model string, in, out, cr, cw int64) (float64, bool) { return 1.5, true })
	_ = scanEvents(path, func(e Event) { usd = e.APIEquivalentUSD })
	if usd != 1.5 {
		t.Errorf("after a price arrived: %v, want 1.5", usd)
	}
}

// Every field of Event survives packing. Each is set to a value of its own,
// so a field added to Event and not to packedEvent, or two swapped, fails.
func TestPackingKeepsEveryField(t *testing.T) {
	var e Event
	v := reflect.ValueOf(&e).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString(fmt.Sprintf("s%d", i))
		case reflect.Int, reflect.Int64:
			f.SetInt(int64(1000 + i))
		case reflect.Float64:
			f.SetFloat(float64(i) + 0.25)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Struct:
			if _, ok := f.Interface().(time.Time); ok {
				f.Set(reflect.ValueOf(time.Date(2026, 10, 16, 9, 0, 0, 123, time.FixedZone("", 7200))))
				continue
			}
			t.Fatalf("Event.%s: a %s, which this test does not know how to fill", v.Type().Field(i).Name, f.Type())
		default:
			t.Fatalf("Event.%s: a %s, which this test does not know how to fill", v.Type().Field(i).Name, f.Type())
		}
	}
	var tab strTable
	p, ok := pack(&e, &tab)
	if !ok {
		t.Fatal("an event with small counts was not packed")
	}
	if got := p.unpack(tab.strs); !reflect.DeepEqual(got, e) {
		t.Errorf("packing changed the event:\n got %+v\nwant %+v", got, e)
	}
	if size := unsafe.Sizeof(p); size > 160 {
		t.Errorf("a packed event is %d bytes, want 160 at most", size)
	}
}

// A count too big for 32 bits is held whole, and read back exactly.
func TestAnEventTooBigToPackIsHeldWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	b, _ := json.Marshal(Event{Time: time.Now(), RequestID: "big", Route: "anthropic", InputTokens: 1 << 40})
	appendTo(t, path, eventLine(t, "a")+string(b)+"\n"+eventLine(t, "c"))
	var got []Event
	_ = scanEvents(path, func(e Event) { got = append(got, e) })
	if len(got) != 3 || got[1].RequestID != "big" || got[1].InputTokens != 1<<40 || got[2].RequestID != "c" {
		t.Errorf("got %+v", got)
	}
}
