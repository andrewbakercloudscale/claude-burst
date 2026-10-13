package codex

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// Codex's turns over a WebSocket.
//
// Codex opens GET .../responses as a WebSocket and sends each turn as one
// text message, {"type":"response.create", ...}, holding what the body of a
// POST would. ChatGPT answers with the events of the stream, one message
// each. Verified against codex-cli 0.160.1 (13 Oct 2026). Until that day
// the socket was passed through unread: its turns were neither masked nor
// counted, and with Automask on every one raised a warning.
//
// The socket is now read as it passes. What Codex sends is put together a
// message at a time and treated as a POSTed turn is (prepareBody); what
// ChatGPT sends goes on untouched while its completed, error and limit
// events are read. A message that was not changed goes on in the very
// bytes Codex wrote. Compression is not negotiated (see New), so a message
// can be read.

// isTurnSocket reports whether a request opens the WebSocket turns use.
func isTurnSocket(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && strings.HasSuffix(r.URL.Path, "/responses")
}

// relay puts a socket between Codex and ChatGPT on a connection ChatGPT
// has just agreed to upgrade. The proxy copies both ways through the
// reply's body, which is the connection itself.
func (g *Gateway) relay(resp *http.Response) {
	back, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		return
	}
	r := resp.Request
	s := &socket{g: g, r: r, back: back, opened: time.Now(), tap: &sseTap{bare: true}}
	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		// Not asked for, so not expected: what follows cannot be read.
		s.out.raw, s.in.raw = true, true
		s.unread("the turns of a Codex WebSocket, because ChatGPT turned on " + ext)
	}
	s.out.s, s.in.s = s, s
	g.logger.Printf("codex: websocket %q opened session=%s client=%q", r.URL.Path, logID(r.Header.Get("Session-Id")), r.Header.Get("Originator"))
	resp.Body = s
}

// socket is one WebSocket of turns. Write takes what Codex sent, Read
// gives what ChatGPT sent; the proxy calls each from its own goroutine.
type socket struct {
	g      *Gateway
	r      *http.Request // the handshake: its headers name the session
	back   io.ReadWriteCloser
	opened time.Time
	out    socketOut
	in     socketIn

	mu      sync.Mutex
	pending []pendingTurn // turns sent and not yet answered, oldest first
	sent    int           // messages from Codex
	got     int           // messages from ChatGPT
	turns   int
	close   string // who closed it and with what code, when a close frame said
	upEnded bool   // ChatGPT's side of the connection ended first

	tap  *sseTap // read only by Read
	once sync.Once
}

type pendingTurn struct {
	start time.Time
	// warm: Codex asked for nothing to be generated ("generate": false),
	// which it does to have the prompt cached before the first turn.
	warm bool
}

func (s *socket) Write(p []byte) (int, error) {
	if err := s.out.feed(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *socket) Read(p []byte) (int, error) {
	n, err := s.back.Read(p)
	s.in.scan(p[:n])
	if err != nil {
		s.mu.Lock()
		s.upEnded = true
		s.mu.Unlock()
	}
	return n, err
}

// Close ends the socket. A turn still waiting for its answer did not
// finish, and is recorded as that, never dropped.
func (s *socket) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		left, sent, got, turns, how, upEnded := s.pending, s.sent, s.got, s.turns, s.close, s.upEnded
		s.pending = nil
		s.mu.Unlock()
		for _, t := range left {
			if !upEnded {
				s.g.recordTurn(s.r, t.start, metrics.StatusClientClosed, usage{}, "Codex closed the WebSocket before the turn finished")
			} else {
				s.g.recordTurn(s.r, t.start, http.StatusBadGateway, usage{}, "the WebSocket closed before the turn finished")
			}
		}
		if how == "" {
			how = "no close frame"
		}
		s.g.logger.Printf("codex: websocket %q closed after %dms, %d message(s) from Codex, %d from ChatGPT, %d turn(s), %s, session=%s", s.r.URL.Path, time.Since(s.opened).Milliseconds(), sent, got, turns, how, logID(s.r.Header.Get("Session-Id")))
	})
	return s.back.Close()
}

// unread says that something on the socket went to ChatGPT unread. Only
// Automask promises otherwise, so only with it on is it a warning.
func (s *socket) unread(what string) {
	if s.g.masks() == nil {
		return
	}
	s.g.logger.Printf("codex: automask could not read %s (session %s): it goes to ChatGPT as Codex wrote it", what, logID(s.r.Header.Get("Session-Id")))
	unmaskedNotice(what)
}

// closed keeps who closed the socket and the code the close frame gave.
func (s *socket) closed(who string, payload []byte) {
	how := "closed by " + who
	if len(payload) >= 2 {
		how += fmt.Sprintf(" with code %d", binary.BigEndian.Uint16(payload))
	}
	s.mu.Lock()
	if s.close == "" {
		s.close = how
	}
	s.mu.Unlock()
}

// fromCodex is given each text message Codex sends, whole. It returns the
// message to send in its place, or nil to send it as it came.
func (s *socket) fromCodex(msg []byte) []byte {
	var m struct {
		Type     string `json:"type"`
		Previous string `json:"previous_response_id"`
		Generate *bool  `json:"generate"`
	}
	if json.Unmarshal(msg, &m) != nil {
		s.unread("a message on a Codex WebSocket that is not JSON")
		return nil
	}
	sid := s.r.Header.Get("Session-Id")
	// A turn that names the response before it carries only what is new,
	// so it cannot stand for the session's context in the inspector.
	whole := m.Type == "response.create" && m.Previous == ""
	out := s.g.prepareBody(sid, s.g.masks(), msg, whole)
	s.mu.Lock()
	s.sent++
	if m.Type == "response.create" {
		s.pending = append(s.pending, pendingTurn{start: time.Now(), warm: m.Generate != nil && !*m.Generate})
	}
	s.mu.Unlock()
	if bytes.Equal(out, msg) {
		return nil
	}
	return out
}

// answered takes the oldest turn waiting for its answer.
func (s *socket) answered() pendingTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns++
	if len(s.pending) == 0 {
		return pendingTurn{start: time.Now()}
	}
	t := s.pending[0]
	s.pending = s.pending[1:]
	return t
}

// fromChatGPT is called at the end of each message ChatGPT sends, with the
// message in the tap as a line of a stream would be.
func (s *socket) fromChatGPT() {
	s.mu.Lock()
	s.got++
	s.mu.Unlock()
	t := s.tap
	if !t.skip {
		s.event(t.line)
	}
	t.endLine()
	if !t.got {
		return
	}
	u := t.u
	t.u, t.got = usage{}, false
	turn := s.answered()
	note := ""
	if turn.warm {
		note = "warm-up: Codex had the prompt cached and asked for nothing to be generated"
	}
	s.g.noteBackWithin()
	s.g.recordTurn(s.r, turn.start, http.StatusOK, u, note)
}

// event reads the messages that are not a completed turn: one that failed,
// and the plan limits, which a WebSocket carries as a message where a POST
// has headers.
func (s *socket) event(line []byte) {
	switch {
	case bytes.Contains(line, []byte(`"codex.rate_limits"`)):
		s.g.noteSocketLimits(line)
	case len(line) <= 64<<10 && bytes.Contains(line, []byte(`"error"`)):
		var ev struct {
			Type   string          `json:"type"`
			Status int             `json:"status"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type != "error" {
			return
		}
		if ev.Status == 0 {
			ev.Status = http.StatusBadGateway
		}
		s.g.noteRefusal(ev.Status, http.Header{}, line)
		s.g.recordTurn(s.r, s.answered().start, ev.Status, usage{}, snippet(ev.Error))
	case bytes.Contains(line, []byte(`"response.failed"`)) || bytes.Contains(line, []byte(`"response.incomplete"`)):
		var ev struct {
			Type     string `json:"type"`
			Response struct {
				Model   string          `json:"model"`
				Error   json.RawMessage `json:"error"`
				Details json.RawMessage `json:"incomplete_details"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &ev) != nil || (ev.Type != "response.failed" && ev.Type != "response.incomplete") {
			return
		}
		why := ev.Response.Error
		if ev.Type == "response.incomplete" {
			why = ev.Response.Details
		}
		s.g.recordTurn(s.r, s.answered().start, http.StatusBadGateway, usage{Model: ev.Response.Model}, ev.Type+": "+snippet(why))
	}
}

// noteSocketLimits keeps the plan usage a codex.rate_limits message
// reports, under the names the x-codex-* headers of a POSTed turn use, so
// that everything reading Limits is served by either.
func (g *Gateway) noteSocketLimits(line []byte) {
	type window struct {
		Used    *float64 `json:"used_percent"`
		Minutes *int64   `json:"window_minutes"`
		After   *int64   `json:"reset_after_seconds"`
		At      *int64   `json:"reset_at"`
	}
	var ev struct {
		Type   string `json:"type"`
		Plan   string `json:"plan_type"`
		Limits struct {
			Primary   *window `json:"primary"`
			Secondary *window `json:"secondary"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Type != "codex.rate_limits" {
		return
	}
	got := map[string]string{}
	if ev.Plan != "" {
		got["x-codex-plan-type"] = ev.Plan
	}
	for name, w := range map[string]*window{"primary": ev.Limits.Primary, "secondary": ev.Limits.Secondary} {
		if w == nil {
			continue
		}
		if w.Used != nil {
			got["x-codex-"+name+"-used-percent"] = fmt.Sprint(*w.Used)
		}
		if w.Minutes != nil {
			got["x-codex-"+name+"-window-minutes"] = fmt.Sprint(*w.Minutes)
		}
		if w.After != nil {
			got["x-codex-"+name+"-reset-after-seconds"] = fmt.Sprint(*w.After)
		}
		if w.At != nil {
			got["x-codex-"+name+"-reset-at"] = fmt.Sprint(*w.At)
		}
	}
	if len(got) == 0 {
		return
	}
	g.mu.Lock()
	// Merged, where a POST's headers replace: the message has fewer
	// readings than the headers, and the rest stay as last reported.
	h := make(map[string]string, len(g.limits.Headers)+len(got))
	for k, v := range g.limits.Headers {
		h[k] = v
	}
	for k, v := range got {
		h[k] = v
	}
	g.limits = Limits{Seen: time.Now(), Headers: h}
	g.persist()
	g.mu.Unlock()
}

// frameHead is the header of one WebSocket frame (RFC 6455, section 5.2).
type frameHead struct {
	fin, rsv1, masked bool
	op                byte
	key               [4]byte
	size              int64
	n                 int // the header's own length
}

const (
	opText  = 1
	opClose = 8
	// headMax is the longest a frame header can be.
	headMax = 14
)

// parseHead reads a frame header from the start of b. ok is false while b
// does not hold all of it; bad means what is there is no frame header.
func parseHead(b []byte) (h frameHead, ok, bad bool) {
	if len(b) < 2 {
		return h, false, false
	}
	h.fin, h.rsv1, h.op = b[0]&0x80 != 0, b[0]&0x40 != 0, b[0]&0x0f
	h.masked = b[1]&0x80 != 0
	h.size, h.n = int64(b[1]&0x7f), 2
	switch h.size {
	case 126:
		if len(b) < 4 {
			return h, false, false
		}
		h.size, h.n = int64(binary.BigEndian.Uint16(b[2:])), 4
	case 127:
		if len(b) < 10 {
			return h, false, false
		}
		v := binary.BigEndian.Uint64(b[2:])
		if v>>63 != 0 {
			return h, false, true
		}
		h.size, h.n = int64(v), 10
	}
	if h.masked {
		if len(b) < h.n+4 {
			return h, false, false
		}
		copy(h.key[:], b[h.n:])
		h.n += 4
	}
	// A control frame is never longer than 125 bytes, nor in pieces.
	if h.op >= opClose && (h.size > 125 || !h.fin) {
		return h, false, true
	}
	return h, true, false
}

// socketOut is what Codex sends. Frames are held until their message is
// whole, since a message may be changed, and then sent on: as they came
// when it was not, as one new frame when it was.
type socketOut struct {
	s   *socket
	buf []byte // not yet sent on
	// held is how much of the front of buf is whole frames of a message
	// that is not yet complete; msg is that message's payload so far.
	held int
	msg  []byte
	op   byte
	// pass is how many bytes of the frame at the front of buf go on unread;
	// passing says the frames after it do too, to the end of the message.
	pass    int64
	passing bool
	// raw: the stream stopped making sense as frames and is only copied.
	raw bool
}

func (o *socketOut) send(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, err := o.s.back.Write(b)
	return err
}

func (o *socketOut) feed(p []byte) error {
	if o.raw {
		return o.send(p)
	}
	o.buf = append(o.buf, p...)
	for {
		if o.pass > 0 {
			n := int64(len(o.buf))
			if n > o.pass {
				n = o.pass
			}
			if err := o.send(o.buf[:n]); err != nil {
				return err
			}
			o.buf, o.pass = o.buf[n:], o.pass-n
			if o.pass > 0 {
				o.buf = nil
				return nil
			}
		}
		rest := o.buf[o.held:]
		h, ok, bad := parseHead(rest)
		if bad {
			o.raw = true
			o.s.unread("a Codex WebSocket, whose frames stopped making sense")
			err := o.send(o.buf)
			o.buf, o.msg, o.held = nil, nil, 0
			return err
		}
		if !ok {
			if len(o.buf) == 0 {
				o.buf = nil
			}
			return nil
		}
		total := int64(h.n) + h.size
		if h.op >= opClose {
			// A control frame may come between the frames of a message,
			// and goes on at once, ahead of the ones held.
			if int64(len(rest)) < total {
				return nil
			}
			frame := rest[:total]
			if h.op == opClose {
				o.s.closed("Codex", unmasked(frame[h.n:], h))
			}
			if err := o.send(frame); err != nil {
				return err
			}
			o.buf = append(o.buf[:o.held:o.held], rest[total:]...)
			continue
		}
		if h.op != 0 {
			o.op = h.op
		}
		why := ""
		switch {
		case o.passing:
		case h.op == 0 && o.held == 0:
			why = "a message on a Codex WebSocket that began in its middle"
		case h.rsv1:
			why = "a compressed message on a Codex WebSocket"
		case o.op != opText:
			why = "a message on a Codex WebSocket that is not text"
		case int64(len(o.msg))+h.size > inspectLimit:
			why = "a turn on a Codex WebSocket, it is larger than Burst reads"
		}
		if o.passing || why != "" {
			if why != "" {
				o.s.unread(why)
			}
			// What was held of the message goes on as it came, then the
			// rest of it as it arrives.
			if err := o.send(o.buf[:o.held+h.n]); err != nil {
				return err
			}
			o.buf, o.held, o.msg = o.buf[o.held+h.n:], 0, nil
			o.pass, o.passing = h.size, !h.fin
			continue
		}
		if int64(len(rest)) < total {
			return nil
		}
		o.msg = append(o.msg, unmasked(rest[h.n:total], h)...)
		o.held += int(total)
		if !h.fin {
			continue
		}
		msg := o.msg
		// Not kept for the next message: the inspector may hold this one.
		o.msg = nil
		var err error
		if out := o.s.fromCodex(msg); out != nil {
			err = o.send(textFrame(out))
		} else {
			err = o.send(o.buf[:o.held])
		}
		if err != nil {
			return err
		}
		o.buf, o.held = o.buf[o.held:], 0
	}
}

// unmasked returns a frame's payload as it reads, in a new slice.
func unmasked(p []byte, h frameHead) []byte {
	out := make([]byte, len(p))
	copy(out, p)
	if h.masked {
		for i := range out {
			out[i] ^= h.key[i&3]
		}
	}
	return out
}

// textFrame is one whole text message as a client sends it, masked.
func textFrame(payload []byte) []byte {
	head := make([]byte, 0, headMax)
	head = append(head, 0x80|opText)
	switch n := len(payload); {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n < 1<<16:
		head = append(head, 0x80|126, byte(n>>8), byte(n))
	default:
		head = append(head, 0x80|127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	var key [4]byte
	_, _ = rand.Read(key[:])
	head = append(head, key[:]...)
	out := make([]byte, len(head)+len(payload))
	copy(out, head)
	body := out[len(head):]
	for i, c := range payload {
		body[i] = c ^ key[i&3]
	}
	return out
}

// socketIn watches what ChatGPT sends as the proxy reads it. Nothing is
// held or changed: each text message is shown to the tap as it passes.
type socketIn struct {
	s    *socket
	head []byte // a frame header still arriving
	h    frameHead
	left int64 // of the frame's payload
	at   int   // how far into the payload, for a masked one
	// reading: the message under way is text Burst can read.
	reading bool
	ctl     []byte // a close frame's payload
	raw     bool
}

func (in *socketIn) scan(b []byte) {
	for len(b) > 0 && !in.raw {
		if in.left == 0 {
			had := len(in.head)
			k := headMax - had
			if k > len(b) {
				k = len(b)
			}
			in.head = append(in.head, b[:k]...)
			h, ok, bad := parseHead(in.head)
			if bad {
				in.raw = true
				return
			}
			if !ok {
				b = b[k:]
				continue
			}
			b = b[h.n-had:]
			in.h, in.head, in.left, in.at = h, in.head[:0], h.size, 0
			if h.op < opClose && h.op != 0 {
				in.reading = h.op == opText && !h.rsv1
			}
			in.ctl = in.ctl[:0]
			if h.size == 0 {
				in.end()
			}
			continue
		}
		n := int64(len(b))
		if n > in.left {
			n = in.left
		}
		chunk := b[:n]
		if in.h.masked && (in.reading || in.h.op == opClose) {
			chunk = append([]byte(nil), chunk...)
			for i := range chunk {
				chunk[i] ^= in.h.key[(in.at+i)&3]
			}
		}
		switch {
		case in.h.op == opClose:
			in.ctl = append(in.ctl, chunk...)
		case in.h.op < opClose && in.reading:
			in.s.tap.add(chunk)
		}
		in.left, in.at, b = in.left-n, in.at+int(n), b[n:]
		if in.left == 0 {
			in.end()
		}
	}
}

// end is called when a frame has passed whole.
func (in *socketIn) end() {
	switch {
	case in.h.op == opClose:
		in.s.closed("ChatGPT", in.ctl)
	case in.h.op < opClose && in.h.fin:
		if in.reading {
			in.s.fromChatGPT()
		} else {
			in.s.mu.Lock()
			in.s.got++
			in.s.mu.Unlock()
		}
		in.reading = false
	}
}
