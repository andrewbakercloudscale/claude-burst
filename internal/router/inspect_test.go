package router

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
)

func pngEncode(w io.Writer, width, height int) error {
	return png.Encode(w, image.NewGray(image.Rect(0, 0, width, height)))
}

func TestInspectContextItemsAndFlags(t *testing.T) {
	f := &fakeAnthropic{context: 1000}
	s := compactServer(t, f, config.CompactionConfig{})
	reminder := "<system-reminder>\\nContents of /nowhere/CLAUDE.md (project instructions):\\n\\nUse tabs.\\n\\nContents of /nowhere/other/CLAUDE.md (project instructions):\\n\\nNo dashes.\\n</system-reminder>"
	msg := func(s string) json.RawMessage { return json.RawMessage(s) }
	history := []json.RawMessage{
		msg(`{"role":"user","content":[{"type":"text","text":"` + reminder + `"},{"type":"text","text":"fix the bug"}]}`),
		msg(`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/nowhere/a.go"}}]}`),
		msg(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + strings.Repeat("x", 30000) + ` card 4111 1111 1111 1111"}]}`),
	}
	for i := 0; i < 11; i++ {
		history = append(history, msg(fmt.Sprintf(`{"role":"assistant","content":"done %d"}`, i)), msg(fmt.Sprintf(`{"role":"user","content":"next %d"}`, i)))
	}
	history = append(history,
		msg(`{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/nowhere/a.go"}}]}`),
		msg(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"short"}]}`))
	send(t, s, "S9", history)

	if got := s.InspectSessions(); len(got) != 1 || got[0].Session != "S9" {
		t.Fatalf("sessions = %+v", got)
	}
	rep := s.InspectContext("S9")
	if rep == nil {
		t.Fatal("no report")
	}
	if rep.Prompts != 12 {
		t.Errorf("prompts = %d, want 12", rep.Prompts)
	}
	by := map[string]ContextItem{}
	for _, it := range rep.Items {
		if _, seen := by[it.Group+"|"+it.Name]; !seen {
			by[it.Group+"|"+it.Name] = it
		}
	}
	if it, ok := by["Instruction files|/nowhere/CLAUDE.md"]; !ok || it.Preview != "Use tabs." {
		t.Errorf("instruction file = %+v", it)
	}
	if _, ok := by["Instruction files|/nowhere/other/CLAUDE.md"]; !ok {
		t.Error("second instruction file missing")
	}
	old := by["Tool results|Read /nowhere/a.go"]
	flags := strings.Join(old.Flags, "; ")
	for _, want := range []string{"large and 11 prompts old", "read again at prompt 12: this is the older copy", "file no longer exists", "personal data: 1 credit card number"} {
		if !strings.Contains(flags, want) {
			t.Errorf("old read flags %q lack %q", flags, want)
		}
	}
	if rep.Flagged < 3 {
		t.Errorf("flagged = %d", rep.Flagged)
	}
	full, ok := s.InspectItem("S9", 0)
	if !ok || full == "" {
		t.Error("no full text for item 0")
	}
}

// A tool result that is a picture has a size (its pixels, not its bytes),
// is told from every other picture, and removing one leaves the others.
func TestAPictureResultHasASizeAndItsOwnIdentity(t *testing.T) {
	png := func(w, h int) string {
		var b bytes.Buffer
		if err := pngEncode(&b, w, h); err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(b.Bytes())
	}
	pic := func(id, data string) string {
		return `{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}]}]}`
	}
	call := func(id, path string) string {
		return `{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"Read","input":{"file_path":"` + path + `"}}]}`
	}
	body := `{"messages":[{"role":"user","content":"look"},` + call("a", "/nowhere/one.png") + `,` + pic("a", png(1500, 750)) + `,` +
		call("b", "/nowhere/two.png") + `,` + pic("b", png(300, 250)) + `,{"role":"assistant","content":"seen"},{"role":"user","content":"` + strings.Repeat("word ", 400) + `"}]}`
	items, prompts := contextItems([]byte(body))
	ctxview.Scale(items, 10_000, prompts)
	ctxview.Flag(items, "", "Read ")
	var pics []ContextItem
	for _, it := range items {
		if it.Pictures > 0 {
			pics = append(pics, it)
		}
	}
	if len(pics) != 2 || pics[0].ID == pics[1].ID {
		t.Fatalf("want two pictures told apart, got %d", len(pics))
	}
	// 1500 x 750 pixels over 750 is 1500 tokens, 300 x 250 is 100, each
	// with the share of its one line of text.
	if pics[0].Tokens < 1501 || pics[0].Tokens > 1700 || pics[1].Tokens < 101 || pics[1].Tokens > 300 {
		t.Errorf("tokens %d and %d, want about 1501 and 101", pics[0].Tokens, pics[1].Tokens)
	}
	if f := strings.Join(pics[0].Flags, "; "); !strings.Contains(f, "picture from 1 prompt ago") || !strings.Contains(f, "big: 16% of the context") {
		t.Errorf("flags %q", f)
	}
	var total int64
	for _, it := range items {
		total += it.Tokens
	}
	if total < 9_900 || total > 10_000 {
		t.Errorf("the items add up to %d of a context of 10000", total)
	}
	var top struct {
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal([]byte(body), &top)
	removed := map[string]ctxview.Removal{pics[0].ID: {ID: pics[0].ID, Name: pics[0].Name}}
	out, _ := removeInMessage(top.Messages[2], removed)
	kept, changed := removeInMessage(top.Messages[4], removed)
	if !strings.Contains(string(out), "burst-removed:"+pics[0].ID) || changed || !strings.Contains(string(kept), `"image"`) {
		t.Fatalf("removing one picture must leave the other: changed %v", changed)
	}
}

// The same command run again makes the older output worth a look, when it
// is large enough to matter.
func TestAnOlderOutputOfACommandRunAgainIsFlagged(t *testing.T) {
	big, small := strings.Repeat("line\n", 600), "ok"
	items := []ContextItem{
		ctxview.NewItem(grpResults, "Bash: go test ./...", 1, big),
		ctxview.NewItem(grpResults, "Bash: git status", 1, small),
		ctxview.NewItem(grpResults, "Bash: go test ./...", 3, big+"more"),
		ctxview.NewItem(grpResults, "Bash: git status", 3, small+"!"),
	}
	ctxview.Scale(items, 0, 3)
	ctxview.Flag(items, "", "Read ")
	if f := strings.Join(items[0].Flags, "; "); !strings.Contains(f, "run again at prompt 3: this is the older output") {
		t.Errorf("older output flags %q", f)
	}
	for _, i := range []int{1, 2, 3} {
		if f := strings.Join(items[i].Flags, "; "); strings.Contains(f, "run again") {
			t.Errorf("item %d flags %q", i, f)
		}
	}
}
