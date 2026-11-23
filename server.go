package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// serverURL is trimletchat-server, online since block 6 (docs/apps/chat.md): Caddy on the VPS sends
// vault.trimlet.dev/chat/* to it. TRIMLETCHAT_SERVER changes it (e.g. http://127.0.0.1:8691 for the
// server on this Mac, `cargo run -- serve` in the trimletchat-server project).
const serverURL = "https://vault.trimlet.dev/chat"

// The answers of the server that the app tells apart.
var (
	errServerFull = errors.New("server_full")
	errServerBusy = errors.New("server_busy")
	errChallenge  = errors.New("invalid_challenge")
	errUnknownKey = errors.New("unknown_key") // the access key was purged, or not used for a year
)

// challenge is the server's request of work: for a new chat or a new access key.
type challenge struct {
	Challenge  []byte `json:"challenge"`
	Difficulty uint64 `json:"difficulty"`
}

// The kinds of work (the server sets how much each costs).
const (
	workCode = "code" // ≈1.5 s on the author's Mac, for every new chat
	workKey  = "key"  // ≈2 minutes, once, for the access key
)

// waiting is the server's answer to a new chat: how long the other person has to join, and the
// token that will let this app reconnect (block 3).
type waiting struct {
	SecondsLeft int    `json:"seconds_left"`
	Token       []byte `json:"token"`
}

// What the server says about a chat (trimletchat-server src/conn.rs).
const (
	evExpired   = "expired"   // nobody joined in time
	evReplaced  = "replaced"  // a new chat of the same access key ended this one
	evLost      = "lost"      // the other did not come back within its 30 s (or the code no longer waits)
	evDropped   = "dropped"   // not from the server: this app's connection was lost (block 3)
	evAway      = "away"      // the other's connection is lost: it has 30 s to come back
	evBack      = "back"      // the other is back
	evResumed   = "resumed"   // this app is back (away: the other is away too)
	evEnded     = "ended"     // the other hung up; with a reason, the server ended it
	evWarning   = "warning"   // the chat will end in 1 minute (reason silence or limit)
	evJoined    = "joined"    // to Gino: someone joined, with pub and proof
	evConfirmed = "confirmed" // to Mario: Gino accepted, with proof and token
	evMsg       = "msg"       // from the other: data
	evTyping    = "typing"
	evAck       = "ack"
)

// event is one frame of the server, or the connection lost.
type event struct {
	kind   string
	pub    []byte
	proof  []byte
	data   []byte
	token  []byte
	away   bool
	reason string // ended, warning: silence, limit, server
	secs   int    // ended, warning: the time of the rule (15 minutes, 3 hours)
}

// chatConn is the live connection of one chat.
type chatConn interface {
	// events gives what happens to the chat; it is closed after the last one.
	events() <-chan event
	// send passes a frame to the other side (kind msg, typing or ack; confirm, reject), in order.
	send(kind string, fields map[string][]byte)
	// end hangs up: it tells the server and closes the connection.
	end()
}

// The refusals of a join.
var (
	errInvalidCode = errors.New("invalid_code") // no chat waits with this code
	errInUse       = errors.New("in_use")       // someone has already joined
)

// server is what the app asks of trimletchat-server (a fake one in the tests).
type server interface {
	challenge(kind string) (challenge, error)
	// newKey gets an access key with a solution of a workKey challenge (subject "key").
	newKey(challenge []byte, nonce uint64) (key []byte, err error)
	// start opens a chat for lookup with the access key and the solution of a workCode challenge
	// (subject: the lookup in base64).
	start(key, lookup, challenge []byte, nonce uint64) (chatConn, waiting, error)
	// join joins the chat of lookup with Mario's public key and proof, and waits for Gino's app to
	// accept: the event is the confirmation, with Gino's proof and the token; or evAway, with the token,
	// if Gino's connection is lost (the confirmation comes when he is back).
	join(lookup, pub, proof []byte) (chatConn, event, error)
	// resume brings back a lost connection with its token (within 30 s); away: the other is away too.
	resume(lookup, token []byte) (conn chatConn, away bool, err error)
}

type httpServer struct {
	base   string
	client *http.Client
}

func newHTTPServer() *httpServer {
	base := serverURL
	if s := os.Getenv("TRIMLETCHAT_SERVER"); s != "" {
		base = strings.TrimRight(s, "/")
	}
	return &httpServer{base: base, client: &http.Client{Timeout: 30 * time.Second}}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// answerError turns an {"error": …} code into an error.
func answerError(code, status string) error {
	for _, known := range []error{errServerFull, errServerBusy, errChallenge, errUnknownKey, errInvalidCode, errInUse} {
		if code == known.Error() {
			return known
		}
	}
	if code == "" {
		code = status
	}
	return fmt.Errorf("the server answered %s", code)
}

// post sends body as JSON and reads the answer into out.
func (s *httpServer) post(path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := s.client.Post(s.base+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("could not reach the server (%w)", err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("could not reach the server (%w)", err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(answer, &e)
		return answerError(e.Error, resp.Status)
	}
	if out != nil {
		return json.Unmarshal(answer, out)
	}
	return nil
}

func (s *httpServer) challenge(kind string) (challenge, error) {
	var c challenge // []byte fields are base64 in JSON
	err := s.post("/challenge", map[string]string{"kind": kind}, &c)
	return c, err
}

func (s *httpServer) newKey(challenge []byte, nonce uint64) ([]byte, error) {
	var out struct {
		Key []byte `json:"key"`
	}
	err := s.post("/keys", map[string]any{"challenge": b64(challenge), "nonce": nonce}, &out)
	return out.Key, err
}

// frame is what the server sends on the WebSocket (trimletchat-server src/conn.rs).
type frame struct {
	Type   string `json:"type"`
	Error  string `json:"error"`
	Reason string `json:"reason"`
	Pub    []byte `json:"pub"`
	Proof  []byte `json:"proof"`
	Data   []byte `json:"data"`
	Away   bool   `json:"away"`
	Secs   int    `json:"secs"`
	waiting
}

func (f frame) event() event {
	kind := f.Type
	if kind == "ended" && f.Reason == "replaced" {
		kind = evReplaced
	}
	return event{kind: kind, pub: f.Pub, proof: f.Proof, data: f.Data, token: f.Token, away: f.Away, reason: f.Reason, secs: f.Secs}
}

// dial opens the WebSocket, sends the first frame and reads the answer.
func (s *httpServer) dial(first map[string]any, timeout time.Duration) (*websocket.Conn, frame, error) {
	url := "ws" + strings.TrimPrefix(s.base, "http") + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, frame{}, fmt.Errorf("could not reach the server (%w)", err)
	}
	c.SetReadLimit(16 * 1024)
	data, _ := json.Marshal(first)
	var answer frame
	err = c.Write(ctx, websocket.MessageText, data)
	if err == nil {
		err = readFrame(ctx, c, &answer)
	}
	if err != nil {
		c.CloseNow()
		return nil, frame{}, fmt.Errorf("could not reach the server (%w)", err)
	}
	return c, answer, nil
}

func (s *httpServer) start(key, lookup, challenge []byte, nonce uint64) (chatConn, waiting, error) {
	c, first, err := s.dial(map[string]any{"type": "start", "key": b64(key), "lookup": b64(lookup), "challenge": b64(challenge), "nonce": nonce}, 15*time.Second)
	if err != nil {
		return nil, waiting{}, err
	}
	if first.Type != "waiting" {
		c.CloseNow()
		return nil, waiting{}, answerError(first.Error, first.Type)
	}
	return newWSConn(c), first.waiting, nil
}

func (s *httpServer) join(lookup, pub, proof []byte) (chatConn, event, error) {
	c, first, err := s.dial(map[string]any{"type": "join", "lookup": b64(lookup), "pub": b64(pub), "proof": b64(proof)}, 30*time.Second)
	if err != nil {
		return nil, event{}, err
	}
	if first.Type != evConfirmed && first.Type != evAway {
		c.CloseNow()
		if first.Type == "error" {
			return nil, event{}, answerError(first.Error, first.Type)
		}
		return nil, event{}, errors.New("could not reach the server (the connection was lost)")
	}
	return newWSConn(c), first.event(), nil
}

func (s *httpServer) resume(lookup, token []byte) (chatConn, bool, error) {
	c, first, err := s.dial(map[string]any{"type": "resume", "lookup": b64(lookup), "token": b64(token)}, 5*time.Second)
	if err != nil {
		return nil, false, err
	}
	if first.Type != evResumed {
		c.CloseNow()
		return nil, false, answerError(first.Error, first.Type)
	}
	return newWSConn(c), first.Away, nil
}

func readFrame(ctx context.Context, c *websocket.Conn, f *frame) error {
	_, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, f)
}

type wsConn struct {
	c     *websocket.Conn
	ev    chan event
	mu    sync.Mutex
	out   chan []byte   // frames to write, in order
	done  chan struct{} // closed when the writer has finished
	ended bool          // end was called (under mu)
	quiet atomic.Bool   // ended by this app: its own closing is not a loss
}

func newWSConn(c *websocket.Conn) *wsConn {
	w := &wsConn{c: c, ev: make(chan event, 64), out: make(chan []byte, 256), done: make(chan struct{})}
	ctx, stop := context.WithCancel(context.Background())
	go func() { w.read(); stop() }()
	go w.write()
	go w.ping(ctx)
	return w
}

// The app says it is alive every 3 s; with no answer in 4 s the connection is taken as lost (a Wi-Fi
// gone closes nothing: only the silence tells it). The server, silent 10 s, does the same.
const (
	pingEvery = 3 * time.Second
	pingWait  = 4 * time.Second
)

func (w *wsConn) ping(ctx context.Context) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, pingWait)
			err := w.c.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				w.c.CloseNow() // read then reports the loss
				return
			}
		}
	}
}

func (w *wsConn) events() <-chan event { return w.ev }

// read turns the server's frames into events until the connection ends.
func (w *wsConn) read() {
	defer close(w.ev)
	for {
		var f frame
		if err := readFrame(context.Background(), w.c, &f); err != nil {
			if !w.quiet.Load() {
				w.ev <- event{kind: evDropped}
			}
			return
		}
		w.ev <- f.event()
	}
}

// write sends the queued frames one after the other, so they arrive in the order written; after the
// last one (end) it closes the connection.
func (w *wsConn) write() {
	defer close(w.done)
	for data := range w.out {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := w.c.Write(ctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			w.c.CloseNow() // read then reports the loss
			for range w.out {
			}
			return
		}
	}
	w.c.Close(websocket.StatusNormalClosure, "")
}

func (w *wsConn) send(kind string, fields map[string][]byte) {
	m := map[string]string{"type": kind}
	for k, v := range fields {
		m[k] = b64(v)
	}
	data, _ := json.Marshal(m)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return
	}
	select {
	case w.out <- data:
	default: // far behind: the connection is as good as lost
		w.c.CloseNow()
	}
}

// end queues {"type":"end"} after what is already queued, then waits (at most 2 s) for it to leave.
func (w *wsConn) end() {
	w.mu.Lock()
	if w.ended {
		w.mu.Unlock()
		return
	}
	w.ended = true
	w.quiet.Store(true)
	select {
	case w.out <- []byte(`{"type":"end"}`):
	default:
	}
	close(w.out)
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		w.c.CloseNow()
	}
}
