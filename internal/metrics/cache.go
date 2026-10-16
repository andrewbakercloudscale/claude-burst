package metrics

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sync"
	"syscall"
)

// Every dashboard poll, usage panel and in-session band reads the metrics
// history, and each read used to parse it from JSON again: 173,000 events
// in 52MB on 16 Oct 2026, which kept the gateway near 30% of a core with a
// few panels open. The history is only ever appended to, and a rotated file
// never changes, so each file is now parsed once and kept in memory: a
// rotated file until rotation moves another file to its name, the live file
// from where the last read stopped. A read costs a walk over the events
// held: 4ms for all 173,000 against 175ms to parse them. The price is
// memory, about 75MB for that history, and no more than that grows to:
// rotation keeps at most maxBackups files of maxBytes each.

// heldFile is one metrics file's events, as parsed so far.
type heldFile struct {
	mu     sync.Mutex
	ino    uint64
	offset int64  // bytes parsed: always the end of a complete line
	head   []byte // the file's first bytes, which say it is still the same file
	events []Event
}

const headLen = 256

var held = struct {
	sync.Mutex
	m map[string]*heldFile
}{m: map[string]*heldFile{}}

// heldEvents returns path's events, parsing only what was added since the
// last call. The slice it returns is never changed afterwards, so callers
// read it without a lock.
func heldEvents(path string) ([]Event, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		forget(path)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var ino uint64
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		ino = sys.Ino
	}

	held.Lock()
	h := held.m[path]
	if h == nil {
		h = &heldFile{}
		held.m[path] = h
	}
	held.Unlock()

	// One parse per file at a time: callers that arrive during it wait for
	// it and share the answer instead of each parsing the same bytes.
	h.mu.Lock()
	defer h.mu.Unlock()
	head := make([]byte, headLen)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	// A new file under the name (rotation, removal), or one shorter than
	// what was parsed or starting differently (rewritten in place), is
	// parsed from the start.
	if h.ino != ino || st.Size() < h.offset || !bytes.HasPrefix(head, h.head) && !bytes.HasPrefix(h.head, head) {
		h.ino, h.offset, h.head, h.events = ino, 0, nil, nil
	}
	if len(head) > len(h.head) {
		h.head = head
	}
	if st.Size() == h.offset {
		return h.events, nil
	}
	if _, err := f.Seek(h.offset, io.SeekStart); err != nil {
		return nil, err
	}
	// Appending writes only past the end of any slice already handed out,
	// so a caller still walking one never sees it change.
	events := h.events
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// A line longer than the buffer: gather it whole.
			long := append([]byte(nil), line...)
			for err == bufio.ErrBufferFull {
				line, err = r.ReadSlice('\n')
				long = append(long, line...)
			}
			line = long
		}
		if err != nil {
			// What is left has no newline yet: the writer is part way
			// through it, or a file written by hand ends without one. It
			// is not held, so it is read again next time, whole; when it
			// parses already it is in this answer, on a copy, since the
			// slot after the held events is the next append's.
			h.events = events
			var e Event
			if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &e) == nil {
				return append(events[:len(events):len(events)], e), nil
			}
			return events, nil
		}
		h.offset += int64(len(line))
		var e Event
		if json.Unmarshal(line, &e) == nil {
			events = append(events, e)
		}
	}
}

func forget(path string) {
	held.Lock()
	delete(held.m, path)
	held.Unlock()
}
