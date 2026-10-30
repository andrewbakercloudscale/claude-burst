package admin

import (
	"net/http"
	"runtime"
	"runtime/pprof"
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
