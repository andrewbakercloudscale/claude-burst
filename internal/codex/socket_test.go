package codex

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// chatGPTSocket is ChatGPT's end of a WebSocket: it keeps the headers of
// the handshake and every byte Codex's frames arrive in, and says what the
// test tells it to.
type chatGPTSocket struct {
	mu   sync.Mutex
	hdr  http.Header
	raw  bytes.Buffer
	conn net.Conn
	up   chan struct{}
}

func newChatGPTSocket() *chatGPTSocket { return &chatGPTSocket{up: make(chan struct{})} }

func (c *chatGPTSocket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	c.mu.Lock()
	c.hdr, c.conn = r.Header.Clone(), conn
	c.mu.Unlock()
	close(c.up)
	buf := make([]byte, 32<<10)
	for {
		n, err := brw.Read(buf)
		c.mu.Lock()
		c.raw.Write(buf[:n])
		c.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// got is every byte that has arrived from Codex.
func (c *chatGPTSocket) got() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.raw.Bytes()...)
}

// waitGot waits until n bytes have arrived.
func (c *chatGPTSocket) waitGot(t *testing.T, n int) []byte {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if b := c.got(); len(b) >= n {
			return b
		}
	}
	t.Fatalf("ChatGPT got %d bytes, want %d", len(c.got()), n)
	return nil
}

// say sends one text message as a server does: unmasked, here in two
// frames so that a message in pieces is read too.
func (c *chatGPTSocket) say(msg string) {
	half := len(msg) / 2
	c.conn.Write(append(serverFrame(opText, false, []byte(msg[:half])), serverFrame(0, true, []byte(msg[half:]))...))
}

func serverFrame(op byte, fin bool, p []byte) []byte {
	b := []byte{op}
	if fin {
		b[0] |= 0x80
	}
	switch n := len(p); {
	case n < 126:
		b = append(b, byte(n))
	case n < 1<<16:
		b = append(b, 126, byte(n>>8), byte(n))
	default:
		b = append(b, 127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	return append(b, p...)
}

// clientFrame is a frame as Codex sends it: masked with key.
func clientFrame(first byte, key [4]byte, p []byte) []byte {
	b := []byte{first}
	switch n := len(p); {
	case n < 126:
		b = append(b, 0x80|byte(n))
	case n < 1<<16:
		b = append(b, 0x80|126, byte(n>>8), byte(n))
	default:
		b = append(b, 0x80|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	b = append(b, key[:]...)
	for i, c := range p {
		b = append(b, c^key[i&3])
	}
	return b
}

// messages reads the frames Codex's side sent into the messages they make,
// control frames left out.
func messages(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	var msg []byte
	for len(b) > 0 {
		h, ok, bad := parseHead(b)
		if !ok || bad || int64(len(b)) < int64(h.n)+h.size {
			t.Fatalf("what reached ChatGPT is not whole frames: %d bytes left", len(b))
		}
		if !h.masked {
			t.Fatalf("a frame reached ChatGPT unmasked, which a server refuses")
		}
		p := unmasked(b[h.n:int64(h.n)+h.size], h)
		b = b[int64(h.n)+h.size:]
		if h.op >= opClose {
			continue
		}
		msg = append(msg, p...)
		if h.fin {
			out, msg = append(out, string(msg)), nil
		}
	}
	return out
}

// openSocket opens a WebSocket through the gateway as Codex does.
func openSocket(t *testing.T, gwURL, sid string, up *chatGPTSocket) net.Conn {
	t.Helper()
	addr := strings.TrimPrefix(gwURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	io.WriteString(conn, "GET /backend-api/codex/responses HTTP/1.1\r\nHost: "+addr+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: oFzrHZOBhLiMkyoiMMpM8g==\r\noriginator: codex_exec\r\nsession-id: "+sid+"\r\nx-client-request-id: "+sid+"\r\nsec-websocket-extensions: permessage-deflate; client_max_window_bits\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("no upgrade: %v %v", resp, err)
	}
	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		t.Fatalf("Codex was told compression is on: %q", ext)
	}
	<-up.up
	return conn
}

const (
	socketTurn      = `{"type":"response.create","model":"gpt-6.1-sol","input":[{"type":"message","role":"user","content":"ship bluebird"}]}`
	socketCompleted = `{"type":"response.completed","response":{"model":"gpt-6.1-sol","usage":{"input_tokens":20433,"input_tokens_details":{"cached_tokens":20000},"output_tokens":5}}}`
)

// Codex is free to send its turns over a WebSocket, and by default it
// does. Each one is masked like a POSTed turn, counted from ChatGPT's
// completed message, and the plan limits are read from the message that
// carries them. Compression is not negotiated, or nothing could be read.
func TestATurnOverAWebSocketIsMaskedAndCounted(t *testing.T) {
	up := newChatGPTSocket()
	g, mp, gw := newTestGateway(t, up)
	g.SetMasker(&fakeMasker{on: true})
	conn := openSocket(t, gw.URL, "ws-1", up)

	if ext := up.hdr.Get("Sec-WebSocket-Extensions"); ext != "" {
		t.Fatalf("compression was offered to ChatGPT, so its messages could not be read: %q", ext)
	}

	// One message in three frames, with a ping between them, a byte at a
	// time: nothing may depend on how the bytes happen to arrive.
	key := [4]byte{1, 2, 3, 4}
	var wire []byte
	wire = append(wire, clientFrame(opText, key, []byte(socketTurn[:20]))...)
	wire = append(wire, clientFrame(0, key, []byte(socketTurn[20:60]))...)
	wire = append(wire, clientFrame(0x80|9, key, []byte("hi"))...)
	wire = append(wire, clientFrame(0x80, key, []byte(socketTurn[60:]))...)
	for i := range wire {
		conn.Write(wire[i : i+1])
	}
	masked := strings.ReplaceAll(socketTurn, "bluebird", "[WORD-1]")
	var msgs []string
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end) && len(msgs) == 0; time.Sleep(5 * time.Millisecond) {
		if b := up.got(); len(b) > len(masked) {
			msgs = messages(t, b)
		}
	}
	if len(msgs) != 1 || msgs[0] != masked {
		t.Fatalf("ChatGPT got %q, want the turn masked", msgs)
	}
	if text, ok := g.InspectItem("ws-1", 0); !ok || !strings.Contains(text, "[WORD-1]") {
		t.Errorf("the inspector must hold what was sent: %q %v", text, ok)
	}

	up.say(`{"type":"codex.rate_limits","plan_type":"pro","rate_limits":{"primary":{"used_percent":42,"window_minutes":10080,"reset_at":1792483852},"secondary":null}}`)
	up.say(`{"type":"response.output_text.delta","delta":"pong"}`)
	up.say(socketCompleted)
	evs := waitEvents(t, mp, 1)
	if e := evs[0]; e.Model != "gpt-6.1-sol" || e.InputTokens != 433 || e.CacheReadTokens != 20000 || e.OutputTokens != 5 || e.SessionID != "ws-1" || e.HTTPStatus != 200 {
		t.Errorf("recorded %+v", e)
	}
	h := g.Limits().Headers
	if h["x-codex-primary-used-percent"] != "42" || h["x-codex-plan-type"] != "pro" || h["x-codex-primary-reset-at"] != "1792483852" {
		t.Errorf("plan usage not kept: %v", h)
	}

	// What ChatGPT says reaches Codex as it was said.
	want := append(serverFrame(opText, false, []byte(socketCompleted[:len(socketCompleted)/2])), serverFrame(0, true, []byte(socketCompleted[len(socketCompleted)/2:]))...)
	back := make([]byte, 0, 1024)
	buf := make([]byte, 1024)
	for !bytes.HasSuffix(back, want) {
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("Codex got %q, which does not end with the completed message: %v", back, err)
		}
		back = append(back, buf[:n]...)
	}

	// A second turn on the same socket, refused; then a third left without
	// an answer when Codex hangs up.
	conn.Write(clientFrame(0x80|opText, key, []byte(socketTurn)))
	up.say(`{"type":"error","error":{"message":"try again","type":"server_error"},"status":503}`)
	evs = waitEvents(t, mp, 2)
	if e := evs[1]; e.HTTPStatus != 503 || !strings.Contains(e.Note, "try again") {
		t.Errorf("a refused turn recorded as %+v", e)
	}
	conn.Write(clientFrame(0x80|opText, key, []byte(socketTurn)))
	up.waitGot(t, 3*len(masked))
	conn.Close()
	evs = waitEvents(t, mp, 3)
	if e := evs[2]; e.HTTPStatus != metrics.StatusClientClosed {
		t.Errorf("a turn Codex walked away from recorded as %+v", e)
	}
}

// A message Burst does not change reaches ChatGPT in the very bytes Codex
// wrote, frames and masking key and all, and so does everything when
// Automask does not cover ChatGPT.
func TestAWebSocketMessageThatIsNotChangedGoesAsItCame(t *testing.T) {
	up := newChatGPTSocket()
	g, mp, gw := newTestGateway(t, up)
	m := &fakeMasker{}
	g.SetMasker(m)
	conn := openSocket(t, gw.URL, "ws-2", up)

	key := [4]byte{9, 8, 7, 6}
	var wire []byte
	wire = append(wire, clientFrame(opText, key, []byte(socketTurn[:33]))...)
	wire = append(wire, clientFrame(0x80, key, []byte(socketTurn[33:]))...)
	conn.Write(wire)
	if got := up.waitGot(t, len(wire)); !bytes.Equal(got, wire) {
		t.Fatalf("with Automask off the turn changed on its way:\n%q\n%q", got, wire)
	}
	// Counted all the same: reading the socket is not Automask's alone.
	up.say(socketCompleted)
	if e := waitEvents(t, mp, 1)[0]; e.OutputTokens != 5 {
		t.Errorf("recorded %+v", e)
	}

	m.on = true
	clean := clientFrame(0x80|opText, key, []byte(`{"type":"response.create","input":[]}`))
	conn.Write(clean)
	if got := up.waitGot(t, len(wire)+len(clean)); !bytes.Equal(got[len(wire):], clean) {
		t.Errorf("a turn with nothing to mask changed on its way:\n%q\n%q", got[len(wire):], clean)
	}
}

// What cannot be read is passed on as it came and said so, never dropped
// and never held: a message that is not text, one marked compressed, and
// one larger than Burst reads. The socket goes on working after each.
func TestAWebSocketMessageBurstCannotReadIsPassedOnAndSaid(t *testing.T) {
	old := inspectLimit
	inspectLimit = 4 << 10
	t.Cleanup(func() { inspectLimit = old })

	up := newChatGPTSocket()
	out := &lockedBuf{}
	g, _, gw := newTestGateway(t, up)
	g.logger.SetOutput(out)
	g.SetMasker(&fakeMasker{on: true})
	conn := openSocket(t, gw.URL, "ws-3", up)

	key := [4]byte{5, 5, 5, 5}
	big := `{"type":"response.create","input":"bluebird ` + strings.Repeat("x", 6<<10) + `"}`
	var wire []byte
	wire = append(wire, clientFrame(0x80|2, key, []byte("bluebird in binary"))...)
	wire = append(wire, clientFrame(0x80|0x40|opText, key, []byte("bluebird deflated"))...)
	wire = append(wire, clientFrame(opText, key, []byte(big[:3<<10]))...)
	wire = append(wire, clientFrame(0x80, key, []byte(big[3<<10:]))...)
	conn.Write(wire)
	if got := up.waitGot(t, len(wire)); !bytes.Equal(got, wire) {
		t.Fatalf("what Burst could not read changed on its way: %d bytes of %d", len(got), len(wire))
	}
	log := out.String()
	for _, want := range []string{"is not text", "a compressed message", "larger than Burst reads"} {
		if !strings.Contains(log, "automask could not read") || !strings.Contains(log, want) {
			t.Errorf("the log does not say %q:\n%s", want, log)
		}
	}

	conn.Write(clientFrame(0x80|opText, key, []byte(socketTurn)))
	rest := up.waitGot(t, len(wire)+len(socketTurn))[len(wire):]
	if msgs := messages(t, rest); len(msgs) != 1 || !strings.Contains(msgs[0], "[WORD-1]") {
		t.Errorf("the turn after them was not masked: %q", msgs)
	}
}
