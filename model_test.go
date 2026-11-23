package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trimlet/gui"
)

// ---- A fake server in memory: it pairs the two apps as trimletchat-server does ----

type fakeHub struct {
	mu        sync.Mutex
	keys      int
	chats     map[string]*fakeChat
	starts    [][]byte // the lookups started
	conns     []*fakeConn
	startErrs []error // the answers to the next starts, in order (nil = ok)
	joinErr   error   // the answer to the next join, if set
	noResume  bool    // resumes fail as if the network were still down
}

type fakeChat struct {
	gino, mario       *fakeConn
	ginoTok, marioTok []byte
	started           bool
	confirm           chan event // Mario's join waits here for Gino's answer
	joinWaits         bool       // Mario's join call is waiting on confirm
	pendingJoin       *event     // joined while Gino was away: given to him when he is back
}

type fakeConn struct {
	hub   *fakeHub
	chat  *fakeChat
	gino  bool
	ev    chan event
	ended bool
	dead  bool // its connection was lost
}

func newFakeHub() *fakeHub { return &fakeHub{chats: map[string]*fakeChat{}} }

// drop makes c's connection lost: its app is told, and the other is told "away".
func (s *fakeHub) drop(c *fakeConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.dead = true
	c.ev <- event{kind: evDropped}
	if o := c.other(); o != nil && !o.dead && (c.chat.started || c.gino) {
		o.ev <- event{kind: evAway}
	}
}

func (s *fakeHub) resume(lookup, token []byte) (chatConn, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noResume {
		return nil, false, errors.New("could not reach the server (no network)")
	}
	ch := s.chats[string(lookup)]
	if ch == nil {
		return nil, false, errInvalidCode
	}
	gino := bytes.Equal(token, ch.ginoTok)
	if !gino && !bytes.Equal(token, ch.marioTok) {
		return nil, false, errInvalidCode
	}
	c := &fakeConn{hub: s, chat: ch, gino: gino, ev: make(chan event, 64)}
	if gino {
		ch.gino = c
	} else {
		ch.mario = c
	}
	s.conns = append(s.conns, c)
	o := c.other()
	if o != nil && !o.dead && (ch.started || gino) {
		o.ev <- event{kind: evBack}
	}
	if gino && ch.pendingJoin != nil {
		c.ev <- *ch.pendingJoin
		ch.pendingJoin = nil
	}
	return c, o != nil && o.dead, nil
}

func (s *fakeHub) challenge(kind string) (challenge, error) {
	return challenge{Challenge: []byte("challenge-" + kind), Difficulty: 1}, nil
}

func (s *fakeHub) newKey([]byte, uint64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys++
	return bytes.Repeat([]byte{byte(s.keys)}, 32), nil
}

func (s *fakeHub) start(key, lookup, _ []byte, _ uint64) (chatConn, waiting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.startErrs) > 0 {
		err := s.startErrs[0]
		s.startErrs = s.startErrs[1:]
		if err != nil {
			return nil, waiting{}, err
		}
	}
	s.starts = append(s.starts, lookup)
	ch := &fakeChat{confirm: make(chan event, 1), ginoTok: append([]byte("gino"), lookup...), marioTok: append([]byte("mario"), lookup...)}
	c := &fakeConn{hub: s, chat: ch, gino: true, ev: make(chan event, 64)}
	ch.gino = c
	s.chats[string(lookup)] = ch
	s.conns = append(s.conns, c)
	return c, waiting{SecondsLeft: 300, Token: ch.ginoTok}, nil
}

func (s *fakeHub) join(lookup, pub, proof []byte) (chatConn, event, error) {
	s.mu.Lock()
	if err := s.joinErr; err != nil {
		s.joinErr = nil
		s.mu.Unlock()
		return nil, event{}, err
	}
	ch := s.chats[string(lookup)]
	switch {
	case ch == nil:
		s.mu.Unlock()
		return nil, event{}, errInvalidCode
	case ch.mario != nil:
		s.mu.Unlock()
		return nil, event{}, errInUse
	}
	c := &fakeConn{hub: s, chat: ch, ev: make(chan event, 64)}
	ch.mario = c
	s.conns = append(s.conns, c)
	joined := event{kind: evJoined, pub: pub, proof: proof}
	if ch.gino.dead {
		ch.pendingJoin = &joined
		s.mu.Unlock()
		return c, event{kind: evAway, token: ch.marioTok}, nil
	}
	ch.joinWaits = true
	ch.gino.ev <- joined
	s.mu.Unlock()
	select {
	case ev := <-ch.confirm:
		if ev.kind != evConfirmed {
			return nil, event{}, errInvalidCode
		}
		ev.token = ch.marioTok
		return c, ev, nil
	case <-time.After(5 * time.Second):
		return nil, event{}, errors.New("could not reach the server (no answer)")
	}
}

func (c *fakeConn) events() <-chan event { return c.ev }

func (c *fakeConn) other() *fakeConn {
	if c.gino {
		return c.chat.mario
	}
	return c.chat.gino
}

func (c *fakeConn) send(kind string, f map[string][]byte) {
	h := c.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.ended || c.dead {
		return
	}
	switch {
	case kind == "confirm" && c.gino:
		c.chat.started = true
		if c.chat.joinWaits {
			c.chat.joinWaits = false
			c.chat.confirm <- event{kind: evConfirmed, proof: f["proof"]}
		} else if m := c.chat.mario; m != nil && !m.dead {
			m.ev <- event{kind: evConfirmed, proof: f["proof"]}
		}
	case kind == "reject" && c.gino:
		c.chat.mario = nil
		c.chat.confirm <- event{kind: "error"}
	case c.chat.started && c.other() != nil && !c.other().dead:
		c.other().ev <- event{kind: kind, data: f["data"]}
	}
}

func (c *fakeConn) end() {
	h := c.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.ended {
		return
	}
	c.ended = true
	if o := c.other(); o != nil && !o.ended {
		o.ev <- event{kind: evEnded}
	}
	for k, ch := range h.chats {
		if ch == c.chat {
			delete(h.chats, k)
		}
	}
}

// ---- A harness: one app, with its user interface run by the test ----

type timer struct {
	at time.Time
	f  func()
}

type harness struct {
	m      *model
	hub    *fakeHub
	uiq    chan func()
	timers []timer
	copied string
	quit   bool
	alerts []string
	clock  time.Time
	mu     sync.Mutex // the clock and the timers
}

func newHarness(hub *fakeHub, key string) *harness {
	h := &harness{hub: hub, uiq: make(chan func(), 256), clock: time.Date(2026, 11, 23, 10, 14, 0, 0, time.Local)}
	m := newModel(hub)
	m.access = &Access{Key: key}
	m.async = func(f func()) { go f() }
	m.ui = func(f func()) { h.uiq <- f }
	m.later = func(d time.Duration, f func()) {
		h.mu.Lock()
		h.timers = append(h.timers, timer{h.clock.Add(d), f})
		h.mu.Unlock()
	}
	m.copy = func(t string) { h.copied = t }
	m.quit = func() { h.quit = true }
	m.alert = func(t string) { h.alerts = append(h.alerts, t) }
	m.now = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.clock }
	m.rows, m.cols = 30, 100
	h.m = m
	return h
}

// pump runs what the background work sent to the user interface, for a moment.
func pump(t *testing.T, hs ...*harness) {
	t.Helper()
	idle := 0
	for idle < 5 {
		ran := false
		for _, h := range hs {
			select {
			case f := <-h.uiq:
				f()
				ran = true
			default:
			}
		}
		if ran {
			idle = 0
		} else {
			idle++
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// until pumps until cond holds (at most a few seconds).
func until(t *testing.T, cond func() bool, hs ...*harness) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		pump(t, hs...)
	}
	t.Fatalf("not reached; screen:\n%s", hs[0].screen())
}

// advance moves this app's clock and runs the timers due.
func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.clock = h.clock.Add(d)
	h.mu.Unlock()
	for {
		h.mu.Lock()
		sort.SliceStable(h.timers, func(i, j int) bool { return h.timers[i].at.Before(h.timers[j].at) })
		if len(h.timers) == 0 || h.timers[0].at.After(h.clock) {
			h.mu.Unlock()
			return
		}
		f := h.timers[0].f
		h.timers = h.timers[1:]
		h.mu.Unlock()
		f()
	}
}

func (h *harness) screen() string {
	var b strings.Builder
	for _, l := range h.m.Render(h.m.cols, h.m.rows) {
		for _, s := range l {
			b.WriteString(s.Text)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (h *harness) sees(t *testing.T, want ...string) {
	t.Helper()
	s := h.screen()
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Fatalf("the screen does not show %q:\n%s", w, s)
		}
	}
}

func (h *harness) notSees(t *testing.T, what string) {
	t.Helper()
	if s := h.screen(); strings.Contains(s, what) {
		t.Fatalf("the screen shows %q:\n%s", what, s)
	}
}

func (h *harness) typeText(s string) {
	for _, r := range s {
		h.m.Rune(r)
	}
}

// aKey is a key the app already has (the fake server makes keys of bytes 1, 2, …).
var aKey = b64(bytes.Repeat([]byte{9}, 32))

// ---- Block 1: Gino starts a chat ----

func TestCountdown(t *testing.T) {
	for _, c := range []struct {
		left time.Duration
		want string
	}{
		{5 * time.Minute, "about 5 minutes left"},
		{4*time.Minute + 31*time.Second, "about 5 minutes left"},
		{4*time.Minute + 30*time.Second, "about 4 minutes left"},
		{3*time.Minute + 31*time.Second, "about 4 minutes left"},
		{90 * time.Second, "about 1 minute left"},
		{31 * time.Second, "about 1 minute left"},
		{30 * time.Second, "0:30"},
		{1500 * time.Millisecond, "0:02"},
		{time.Second, "0:01"},
		{-time.Second, "0:00"},
	} {
		if got := countdown(c.left); got != c.want {
			t.Errorf("countdown(%v) = %q, want %q", c.left, got, c.want)
		}
	}
}

func TestTheServerGetsOnlyTheLookup(t *testing.T) {
	code, priv, err := newChatKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := decode32(code, keyLen)
	if err != nil || !bytes.Equal(pub, priv.PublicKey().Bytes()) {
		t.Fatalf("the code does not read back as the public key: %v", err)
	}
	want := sha256.Sum256(append([]byte("trimletchat-v1 lookup"), pub...))
	if l := lookup(pub); !bytes.Equal(l, want[:]) || bytes.Equal(l, pub) {
		t.Fatal("the lookup is not SHA-256 of the label and the code")
	}

	h := newHarness(newFakeHub(), aKey)
	h.m.Rune('1')
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)
	shown, _ := decode32(h.m.code, keyLen)
	if len(h.hub.starts) != 1 || bytes.Contains(h.hub.starts[0], shown) || !bytes.Equal(h.hub.starts[0], lookup(shown)) {
		t.Fatal("the server must get the lookup of the code shown, never the code")
	}
}

func TestStartWaitCopyCancel(t *testing.T) {
	h := newHarness(newFakeHub(), aKey)
	h.sees(t, "1  Start a chat", "2  Join a chat", "1 start a chat · 2 join a chat · esc quit")
	h.m.Rune('1')
	h.sees(t, "Making the code…", "esc cancel")
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)
	h.sees(t, "Send this code to the person you want to chat with.", h.m.code, "Waiting to be joined…  about 5 minutes left", "c copy · esc cancel")
	if len(h.m.code) != 64 { // 52 characters and 12 dashes
		t.Fatalf("code %q", h.m.code)
	}
	h.advance(4*time.Minute + 40*time.Second)
	h.sees(t, "Waiting to be joined…  0:20")

	h.m.Rune('c')
	if h.copied != h.m.code {
		t.Fatalf("copied %q", h.copied)
	}
	h.sees(t, "✔ Code copied")

	h.m.Key(gui.KeyEsc)
	h.sees(t, "Cancel this chat?")
	h.m.Rune('n')
	h.sees(t, "Waiting to be joined…")
	h.m.Key(gui.KeyEsc)
	h.m.Rune('y')
	until(t, func() bool { h.hub.mu.Lock(); defer h.hub.mu.Unlock(); return h.hub.conns[0].ended }, h)
	h.sees(t, "1  Start a chat")
}

func TestNobodyJoins(t *testing.T) {
	h := newHarness(newFakeHub(), aKey)
	h.m.Rune('1')
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)
	code := h.m.code
	h.hub.conns[0].ev <- event{kind: evExpired}
	until(t, func() bool { return h.m.phase == phaseExpired }, h)
	h.sees(t, "✖ No one joined: the code has expired.", "enter new code · esc back")
	h.notSees(t, code)
	h.m.Key(gui.KeyEnter)
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)
	if len(h.hub.starts) != 2 || h.m.code == code {
		t.Fatal("enter must make a new code at once")
	}
	h.hub.conns[1].ev <- event{kind: evLost}
	until(t, func() bool { return h.m.phase == phaseLost }, h)
	h.sees(t, "✖ The connection was lost: the code no longer works.")
	h.m.Key(gui.KeyEsc)
	h.sees(t, "1  Start a chat")
}

func TestTheFirstTimeTheKeyComesFirst(t *testing.T) {
	h := newHarness(newFakeHub(), "")
	h.m.start() // the opening: the work starts at once
	h.m.Rune('1')
	h.sees(t, "Start a chat", "Setting up your secure access…", "esc cancel")
	until(t, func() bool { return h.m.phase == phaseWaiting }, h) // the key, then the code by itself
	if h.m.access.Key == "" || len(h.hub.starts) != 1 {
		t.Fatal("the code must follow the key by itself")
	}

	// Esc while the key is made: back to the menu, the work goes on, no code is made.
	h = newHarness(newFakeHub(), "")
	h.m.start()
	h.m.Rune('1')
	h.m.Key(gui.KeyEsc)
	until(t, func() bool { return h.m.access.Key != "" }, h)
	pump(t, h)
	if len(h.hub.starts) != 0 {
		t.Fatal("no code must be made")
	}
	h.sees(t, "1  Start a chat")
}

func TestAPurgedKeyIsReplaced(t *testing.T) {
	h := newHarness(newFakeHub(), aKey)
	h.hub.startErrs = []error{errUnknownKey}
	h.m.Rune('1')
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)
	if h.hub.keys != 1 || h.m.access.Key == aKey || len(h.hub.starts) != 1 {
		t.Fatalf("a new key, then the code: keys %d, starts %d", h.hub.keys, len(h.hub.starts))
	}
}

func TestTheServerRefuses(t *testing.T) {
	h := newHarness(newFakeHub(), aKey)
	h.hub.startErrs = []error{errors.New("could not reach the server (no network)")}
	h.m.Rune('1')
	until(t, func() bool { return h.m.phase == phaseError }, h)
	h.sees(t, "✖ Could not reach the server.", "enter try again · esc back")
	h.m.Key(gui.KeyEnter)
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)

	h = newHarness(newFakeHub(), aKey)
	h.hub.startErrs = []error{errServerFull}
	h.m.Rune('1')
	until(t, func() bool { return h.m.phase == phaseError }, h)
	h.sees(t, "✖ The server is full right now. Try again later.", "esc back")
	h.notSees(t, "enter try again")
}

func TestLeavingBeforeTheAnswerEndsTheChat(t *testing.T) {
	h := newHarness(newFakeHub(), aKey)
	h.m.Rune('1')
	h.m.Key(gui.KeyEsc) // while "Making the code…"
	until(t, func() bool {
		h.hub.mu.Lock()
		defer h.hub.mu.Unlock()
		return len(h.hub.conns) == 1 && h.hub.conns[0].ended
	}, h)
	h.sees(t, "1  Start a chat")
}

func TestQuit(t *testing.T) {
	h := newHarness(newFakeHub(), aKey)
	h.m.Key(gui.KeyEsc)
	h.sees(t, "Quit Trimlet Chat?")
	h.m.Rune('y')
	if !h.quit {
		t.Fatal("yes must quit")
	}
	// Closing the app while a chat waits ends it on the server.
	h = newHarness(newFakeHub(), aKey)
	h.m.Rune('1')
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)
	h.m.shutdown()
	if !h.hub.conns[0].ended {
		t.Fatal("closing the app must end the chat")
	}
}

// ---- Block 2: Mario joins, and they write ----

// chatting makes Gino start a chat and Mario join it.
func chatting(t *testing.T) (gino, mario *harness) {
	t.Helper()
	hub := newFakeHub()
	gino, mario = newHarness(hub, aKey), newHarness(hub, "")
	gino.m.Rune('1')
	until(t, func() bool { return gino.m.phase == phaseWaiting }, gino)
	mario.m.Rune('2')
	mario.sees(t, "Join a chat", "Paste the code you received:", "enter join · esc back")
	mario.m.Paste(strings.ToLower(gino.m.code)) // case and dashes do not matter
	mario.m.Key(gui.KeyEnter)
	until(t, func() bool { return gino.m.screen == screenChat && mario.m.screen == screenChat }, gino, mario)
	return gino, mario
}

func TestGinoAndMarioWrite(t *testing.T) {
	gino, mario := chatting(t)
	gino.sees(t, "── chat started 10:14 ──", "enter send · esc end chat", "> ")
	mario.sees(t, "── chat started 10:14 ──")

	gino.typeText("ciao, ti passo il comando")
	until(t, func() bool { return strings.Contains(mario.screen(), "  ···") }, gino, mario)
	gino.m.Key(gui.KeyEnter)
	gino.sees(t, "› ciao, ti passo il comando", "sending…")
	until(t, func() bool { return strings.Contains(mario.screen(), "  ciao, ti passo il comando") }, gino, mario)
	mario.notSees(t, "···")
	until(t, func() bool { return !strings.Contains(gino.screen(), "sending…") }, gino, mario)
	if it := gino.m.chat.items[1]; !it.acked {
		t.Fatal("the message must turn green when Mario's app has it")
	}

	// Several lines: a paste keeps them, Shift+Enter adds one; one message.
	mario.m.Paste("cd /var/www\ngit pull")
	mario.m.Key(gui.KeyShiftEnter)
	mario.typeText("systemctl reload caddy")
	mario.m.Key(gui.KeyEnter)
	until(t, func() bool { return strings.Contains(gino.screen(), "  systemctl reload caddy") }, gino, mario)
	gino.sees(t, "  cd /var/www\n", "  git pull\n")
	if got := gino.m.chat.items[len(gino.m.chat.items)-1].text; got != "cd /var/www\ngit pull\nsystemctl reload caddy" {
		t.Fatalf("got %q", got)
	}

	// Gino hangs up: back to the menu; Mario sees the end, the messages stay a minute.
	gino.m.Key(gui.KeyEsc)
	gino.sees(t, "End the chat?")
	gino.m.Rune('y')
	gino.sees(t, "1  Start a chat")
	until(t, func() bool { return mario.m.chat != nil && mario.m.chat.ended }, gino, mario)
	mario.sees(t, "── chat ended ──", "Nothing was kept.", "systemctl reload caddy", "esc back to the menu · closes in 1:00")
	mario.notSees(t, "> ")
	mario.advance(15 * time.Second)
	mario.sees(t, "closes in 0:45")
	mario.advance(45 * time.Second)
	mario.sees(t, "1  Start a chat")
	if mario.m.chat != nil {
		t.Fatal("the messages must leave the memory")
	}
}

func TestTheConnectionLostEndsTheChat(t *testing.T) {
	gino, mario := chatting(t)
	gino.m.chat.conn.(*fakeConn).ev <- event{kind: evLost}
	until(t, func() bool { return gino.m.chat.ended }, gino, mario)
	gino.sees(t, "── the connection was lost ──", "Nothing was kept.")
	gino.m.Key(gui.KeyEsc)
	gino.sees(t, "1  Start a chat")
}

func TestJoinErrors(t *testing.T) {
	hub := newFakeHub()
	mario := newHarness(hub, "")
	mario.m.Rune('2')
	mario.typeText("QXJM-8VR2")
	mario.m.Key(gui.KeyEnter)
	mario.sees(t, "✖ Wrong code format.")
	mario.m.Key(gui.KeyBackspace)
	mario.notSees(t, "Wrong code format")

	retype := func(code string) {
		for range 70 {
			mario.m.Key(gui.KeyBackspace)
		}
		mario.m.Paste(code)
		mario.m.Key(gui.KeyEnter)
	}
	code, _, _ := newChatKey()
	retype(code)
	until(t, func() bool { return mario.m.joinErr != "" }, mario)
	mario.sees(t, "✖ Invalid code.")

	gino, luca := newHarness(hub, aKey), newHarness(hub, "")
	gino.m.Rune('1')
	until(t, func() bool { return gino.m.phase == phaseWaiting }, gino)
	ginoCode := gino.m.code
	luca.m.Rune('2')
	luca.m.Paste(ginoCode)
	luca.m.Key(gui.KeyEnter)
	until(t, func() bool { return luca.m.screen == screenChat }, gino, luca)
	retype(ginoCode)
	until(t, func() bool { return mario.m.joinErr != "" }, mario)
	mario.sees(t, "✖ Code already in use: the chat is in progress.")

	hub.mu.Lock()
	hub.joinErr = errors.New("could not reach the server (no network)")
	hub.mu.Unlock()
	mario.m.Key(gui.KeyEnter)
	until(t, func() bool { return mario.m.joinErr == "✖ Could not reach the server." }, mario)
	mario.m.Key(gui.KeyEsc)
	mario.sees(t, "1  Start a chat")
}

func TestAWrongProofIsRefused(t *testing.T) {
	h := newHarness(newFakeHub(), aKey)
	h.m.Rune('1')
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)
	// What only whoever controls the server could send: a key and a proof made without the code.
	ch := h.hub.chats[string(h.hub.starts[0])]
	h.hub.mu.Lock()
	ch.mario = &fakeConn{hub: h.hub, chat: ch, ev: make(chan event, 4)}
	h.hub.mu.Unlock()
	priv, _ := newJoinKey()
	h.hub.conns[0].ev <- event{kind: evJoined, pub: priv.PublicKey().Bytes(), proof: make([]byte, 32)}
	pump(t, h)
	select {
	case ev := <-ch.confirm:
		if ev.kind != "error" {
			t.Fatalf("Gino's app must refuse, got %q", ev.kind)
		}
	default:
		t.Fatal("Gino's app said nothing")
	}
	h.sees(t, "Waiting to be joined…")
}

func TestAMissingMessageEndsTheChat(t *testing.T) {
	gino, mario := chatting(t)
	// A message numbered 1 while 0 is expected: the one before was removed on the way.
	fake := &session{keys: gino.m.chat.s.keys}
	fake.nextOut[kindMsg] = 1
	_, data, _ := fake.message("hello")
	mario.m.chat.conn.(*fakeConn).ev <- event{kind: evMsg, data: data}
	until(t, func() bool { return mario.m.chat.ended }, gino, mario)
	mario.sees(t, "── a message was lost: the chat was ended ──", "Nothing was kept.")
	until(t, func() bool { return gino.m.chat.ended }, gino, mario)
	gino.sees(t, "── chat ended ──")
}

func TestTooLong(t *testing.T) {
	gino, _ := chatting(t)
	gino.m.Paste(strings.Repeat("x", 5300))
	gino.m.Key(gui.KeyEnter)
	gino.sees(t, "Too long: at most 4 KB (this is 5.2 KB)")
	if len(gino.m.chat.items) != 1 {
		t.Fatal("it must not be sent")
	}
	gino.m.Key(gui.KeyBackspace)
	gino.notSees(t, "Too long")
}

func TestScrollUpAndDown(t *testing.T) {
	gino, mario := chatting(t)
	for i := range 40 {
		mario.typeText("line " + string(rune('A'+i%26)))
		mario.m.Key(gui.KeyEnter)
	}
	// The pace: 5 at once, then one every 2 s.
	until(t, func() bool { return len(gino.m.chat.items) == 6 }, gino, mario)
	for len(gino.m.chat.items) < 41 {
		mario.advance(msgEvery)
		pump(t, gino, mario)
	}
	gino.sees(t, "line N")
	gino.notSees(t, "chat started")
	gino.m.Key(gui.KeyUp)
	gino.m.Key(gui.KeyUp)
	gino.m.Key(gui.KeyUp)
	gino.sees(t, "chat started", "↑↓ scroll")
	gino.notSees(t, "enter send")
	gino.typeText("ok") // writing while looking up: the messages stay where they are
	gino.sees(t, "chat started", "> ok")

	mario.typeText("new one")
	mario.m.Key(gui.KeyEnter)
	mario.advance(msgEvery)
	until(t, func() bool { return strings.Contains(gino.screen(), "new messages ↓") }, gino, mario)
	gino.sees(t, "chat started")
	for range 10 {
		gino.m.Key(gui.KeyDown)
	}
	gino.sees(t, "  new one", "enter send · esc end chat")
	gino.notSees(t, "new messages")
}

func TestCopyGivesTheOriginalText(t *testing.T) {
	gino, mario := chatting(t)
	long := "docker run --rm -it -v /data:/data " + strings.Repeat("--flag ", 30) + "image"
	mario.m.Paste(long + "\n\tsecond line")
	mario.m.Key(gui.KeyEnter)
	until(t, func() bool { return len(gino.m.chat.items) == 2 }, gino, mario)
	lines := gino.m.Render(gino.m.cols, gino.m.rows)
	first, last := -1, -1
	for i, l := range lines {
		text := ""
		for _, s := range l {
			text += s.Text
		}
		if strings.HasPrefix(text, "  docker run") {
			first = i
		}
		if strings.Contains(text, "second line") {
			last = i
		}
	}
	if first < 0 || last <= first+1 {
		t.Fatalf("the message should wrap over several rows: %d, %d\n%s", first, last, gino.screen())
	}
	// From the start of the row (the indent included) to far right of the last row.
	got := gino.m.SelectedText(first, 0, last, 200)
	if got != long+"\n\tsecond line" {
		t.Fatalf("copied\n%q\nwant\n%q", got, long+"\n\tsecond line")
	}
	// Part of a row only.
	if got := gino.m.SelectedText(first, 2, first, 7); got != "docker" {
		t.Fatalf("copied %q", got)
	}
}

// ---- Block 3: the line drops ----

func TestALostLineComesBack(t *testing.T) {
	gino, mario := chatting(t)
	hub := gino.hub
	hub.mu.Lock()
	hub.noResume = true
	hub.mu.Unlock()
	hub.drop(mario.m.chat.conn.(*fakeConn))
	until(t, func() bool { return mario.m.chat.reconnecting && gino.m.chat.otherAway }, gino, mario)
	mario.sees(t, "── reconnecting ··· ──")
	gino.sees(t, "── reconnecting ··· ──", "enter send · esc end chat")

	// Writing meanwhile: the messages wait, grey; several, in order.
	gino.typeText("uno")
	gino.m.Key(gui.KeyEnter)
	gino.typeText("due")
	gino.m.Key(gui.KeyEnter)
	gino.sees(t, "› uno", "› due", "sending…")
	mario.typeText("ci sono?")
	mario.m.Key(gui.KeyEnter)

	hub.mu.Lock()
	hub.noResume = false
	hub.mu.Unlock()
	mario.advance(reconnectEvery) // the next try
	until(t, func() bool {
		return !mario.m.chat.reconnecting && !gino.m.chat.otherAway && len(mario.m.chat.items) == 4 && len(gino.m.chat.items) == 4
	}, gino, mario)
	until(t, func() bool {
		return !strings.Contains(gino.screen(), "sending…") && !strings.Contains(mario.screen(), "sending…")
	}, gino, mario)
	mario.sees(t, "  uno\n", "  due\n", "› ci sono?")
	gino.sees(t, "  ci sono?")
	gino.notSees(t, "reconnecting")
	mario.notSees(t, "reconnecting")
	if mario.m.chat.ended || gino.m.chat.ended {
		t.Fatal("the chat must go on")
	}
}

func TestTheLineDoesNotComeBack(t *testing.T) {
	gino, mario := chatting(t)
	hub := gino.hub
	hub.mu.Lock()
	hub.noResume = true
	hub.mu.Unlock()
	hub.drop(mario.m.chat.conn.(*fakeConn))
	until(t, func() bool { return mario.m.chat.reconnecting }, gino, mario)
	for range 16 {
		mario.advance(reconnectEvery)
		pump(t, mario)
	}
	until(t, func() bool { return mario.m.chat.ended }, mario)
	mario.sees(t, "── the connection was lost ──", "Nothing was kept.")
	mario.notSees(t, "reconnecting")
	// Gino: the server says it after Mario's 30 s.
	gino.m.chat.conn.(*fakeConn).ev <- event{kind: evLost}
	until(t, func() bool { return gino.m.chat.ended }, gino)
	gino.sees(t, "── the connection was lost ──")
}

func TestGinoLosesTheLineWhileWaiting(t *testing.T) {
	h := newHarness(newFakeHub(), aKey)
	h.m.Rune('1')
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)
	code := h.m.code
	h.hub.drop(h.hub.conns[0])
	until(t, func() bool { return h.m.phase == phaseWaiting && h.m.conn != h.hub.conns[0] }, h)
	h.sees(t, code, "Waiting to be joined…")
	h.notSees(t, "reconnecting")

	// Again, and this time it does not come back: the code no longer works.
	h.hub.mu.Lock()
	h.hub.noResume = true
	h.hub.mu.Unlock()
	h.hub.drop(h.hub.conns[1])
	until(t, func() bool { return h.m.reconnecting }, h)
	h.sees(t, code, "── reconnecting ··· ──")
	for range 16 {
		h.advance(reconnectEvery)
		pump(t, h)
	}
	until(t, func() bool { return h.m.phase == phaseLost }, h)
	h.sees(t, "✖ The connection was lost: the code no longer works.")
	h.notSees(t, code)

	// Lost after the 5 minutes ended meanwhile: the end of the waiting.
	h = newHarness(newFakeHub(), aKey)
	h.m.Rune('1')
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)
	h.advance(4*time.Minute + 50*time.Second)
	h.hub.mu.Lock()
	h.hub.noResume = true
	h.hub.mu.Unlock()
	h.hub.drop(h.hub.conns[0])
	until(t, func() bool { return h.m.reconnecting }, h)
	for range 16 {
		h.advance(reconnectEvery)
		pump(t, h)
	}
	until(t, func() bool { return h.m.phase == phaseExpired }, h)
	h.sees(t, "✖ No one joined: the code has expired.")
}

func TestMarioJoinsWhileGinoIsAway(t *testing.T) {
	hub := newFakeHub()
	gino, mario := newHarness(hub, aKey), newHarness(hub, "")
	gino.m.Rune('1')
	until(t, func() bool { return gino.m.phase == phaseWaiting }, gino)
	code := gino.m.code
	hub.mu.Lock()
	hub.noResume = true
	hub.mu.Unlock()
	hub.drop(hub.conns[0])
	until(t, func() bool { return gino.m.reconnecting }, gino)

	mario.m.Rune('2')
	mario.m.Paste(code)
	mario.m.Key(gui.KeyEnter)
	until(t, func() bool { return mario.m.screen == screenChat }, gino, mario)
	mario.sees(t, "── reconnecting ··· ──")
	mario.notSees(t, "chat started")
	mario.typeText("ciao Gino")
	mario.m.Key(gui.KeyEnter)
	mario.sees(t, "› ciao Gino", "sending…")

	hub.mu.Lock()
	hub.noResume = false
	hub.mu.Unlock()
	gino.advance(reconnectEvery)
	until(t, func() bool { return gino.m.screen == screenChat && mario.m.chat.confirmed }, gino, mario)
	until(t, func() bool { return len(gino.m.chat.items) == 2 }, gino, mario)
	gino.sees(t, "── chat started", "  ciao Gino")
	mario.sees(t, "── chat started")
	until(t, func() bool { return !strings.Contains(mario.screen(), "sending…") }, gino, mario)
	mario.notSees(t, "reconnecting")
}

// ---- Block 4: times and limits ----

func TestThePace(t *testing.T) {
	gino, mario := chatting(t)
	for i := range 8 {
		gino.typeText("m" + string(rune('0'+i)))
		gino.m.Key(gui.KeyEnter)
	}
	// 5 at once (the reserve), the others wait in grey.
	until(t, func() bool { return len(mario.m.chat.items) == 6 }, gino, mario)
	pump(t, gino, mario)
	if len(mario.m.chat.items) != 6 {
		t.Fatalf("%d messages arrived, want 5", len(mario.m.chat.items)-1)
	}
	gino.sees(t, "› m7", "sending…")
	gino.advance(msgEvery)
	until(t, func() bool { return len(mario.m.chat.items) == 7 }, gino, mario)
	gino.advance(2 * msgEvery)
	until(t, func() bool { return len(mario.m.chat.items) == 9 }, gino, mario)
	mario.sees(t, "  m7")
}

func TestWarningsAndTheServersEnds(t *testing.T) {
	gino, mario := chatting(t)
	conn := gino.m.chat.conn.(*fakeConn)
	conn.ev <- event{kind: evWarning, reason: "silence", secs: 900}
	until(t, func() bool { return strings.Contains(gino.screen(), "will end") }, gino)
	gino.sees(t, "── the chat will end in 1 minute: no messages ──")
	gino.typeText("un attimo")
	gino.m.Key(gui.KeyEnter)
	gino.notSees(t, "will end in 1 minute")

	conn.ev <- event{kind: evWarning, reason: "limit", secs: 3 * 3600}
	until(t, func() bool { return strings.Contains(gino.screen(), "3-hour limit") }, gino)
	gino.sees(t, "── the chat will end in 1 minute: 3-hour limit ──")

	conn.ev <- event{kind: evEnded, reason: "limit", secs: 3 * 3600}
	until(t, func() bool { return gino.m.chat.ended }, gino)
	gino.sees(t, "── the chat ended: 3-hour limit ──", "Nothing was kept.")

	mconn := mario.m.chat.conn.(*fakeConn)
	mconn.ev <- event{kind: evEnded, reason: "silence", secs: 900}
	until(t, func() bool { return mario.m.chat.ended }, mario)
	mario.sees(t, "── the chat ended after 15 minutes of silence ──")

	g2, m2 := chatting(t)
	g2.m.chat.conn.(*fakeConn).ev <- event{kind: evEnded, reason: "server"}
	until(t, func() bool { return g2.m.chat.ended }, g2, m2)
	g2.sees(t, "── the chat was ended by the server ──")
}

func TestSpan(t *testing.T) {
	for _, c := range []struct {
		secs int
		adj  bool
		want string
	}{{3 * 3600, true, "3-hour"}, {900, false, "15 minutes"}, {60, false, "1 minute"}, {120, true, "2-minute"}, {45, false, "45 seconds"}} {
		if got := span(c.secs, c.adj); got != c.want {
			t.Errorf("span(%d, %v) = %q, want %q", c.secs, c.adj, got, c.want)
		}
	}
}

// ---- Block 5: alerts ----

func TestAlerts(t *testing.T) {
	hub := newFakeHub()
	gino, mario := newHarness(hub, ""), newHarness(hub, "")
	gino.m.start()
	gino.m.Rune('1') // the first time: the key, then the code
	until(t, func() bool { return gino.m.phase == phaseWaiting }, gino)
	code := gino.m.code
	mario.m.Rune('2')
	mario.m.Paste(code)
	mario.m.Key(gui.KeyEnter)
	until(t, func() bool { return gino.m.screen == screenChat && mario.m.screen == screenChat }, gino, mario)
	mario.typeText("ciao")
	mario.m.Key(gui.KeyEnter)
	until(t, func() bool { return len(gino.m.chat.items) == 2 }, gino, mario)
	gino.m.chat.conn.(*fakeConn).ev <- event{kind: evWarning, reason: "silence", secs: 900}
	until(t, func() bool { return len(gino.alerts) == 4 }, gino)
	mario.m.Key(gui.KeyEsc)
	mario.m.Rune('y')
	until(t, func() bool { return gino.m.chat.ended }, gino, mario)
	want := []string{"Your code is ready.", "Joined.", "New message.", "The chat will end in 1 minute.", "The chat ended."}
	if strings.Join(gino.alerts, "|") != strings.Join(want, "|") {
		t.Fatalf("alerts %q, want %q", gino.alerts, want)
	}
	// Whoever hangs up gets none; Mario got none for joining himself.
	if len(mario.alerts) != 0 {
		t.Fatalf("Mario's alerts %q", mario.alerts)
	}
	// The end of the waiting gives none (decided: a missed call is not urgent).
	h := newHarness(newFakeHub(), aKey)
	h.m.Rune('1')
	until(t, func() bool { return h.m.phase == phaseWaiting }, h)
	h.hub.conns[0].ev <- event{kind: evExpired}
	until(t, func() bool { return h.m.phase == phaseExpired }, h)
	if len(h.alerts) != 0 {
		t.Fatalf("alerts %q", h.alerts)
	}
}
