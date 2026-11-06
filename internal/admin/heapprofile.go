package admin

import (
	"bytes"
	"net/http"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"
)

// handleHeapProfile writes the gateway's heap profile, for
// `go tool pprof http://127.0.0.1:7788/api/heap-profile`. The memory line
// in the log says how much the gateway holds; this says what holds it, so a
// change made to save memory can be measured rather than guessed at.
// A profile names functions and sizes, never what a request contained.
// ?gc=1 collects first, so only live memory is counted.
//
// runtime/pprof, not net/http/pprof: importing that one registers its
// handlers on http.DefaultServeMux for anything that serves it.
func handleHeapProfile(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("gc") == "1" {
		runtime.GC()
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="heap.pb.gz"`)
	if err := pprof.Lookup("heap").WriteTo(w, 0); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleCPUProfile samples where the gateway spends CPU for ?seconds=N
// (30 by default, 120 at most), for
// `go tool pprof http://127.0.0.1:7788/api/cpu-profile?seconds=60`.
// macOS's sample(1) loses Go's goroutine stacks, so it shows the hot
// functions but never which request or loop called them; this does.
// One at a time: the runtime has only the one CPU profiler.
func handleCPUProfile(w http.ResponseWriter, r *http.Request) {
	secs := 30
	if v, err := strconv.Atoi(r.URL.Query().Get("seconds")); err == nil && v > 0 {
		secs = min(v, 120)
	}
	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	select {
	case <-time.After(time.Duration(secs) * time.Second):
	case <-r.Context().Done():
	}
	pprof.StopCPUProfile()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="cpu.pb.gz"`)
	_, _ = w.Write(buf.Bytes())
}
