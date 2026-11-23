package main

import (
	"crypto/ecdh"
	"errors"
	"fmt"
	"time"

	"github.com/trimlet/gui"
)

type screen int

const (
	screenMenu  screen = iota // 1 Start a chat
	screenStart               // Gino: the key, the code, the wait (one screen, its phase below)
	screenJoin                // Mario: paste the code
	screenChat                // the chat, both (chat.go)
)

// The phases of screenStart.
type phase int

const (
	phaseKey     phase = iota // the access key is not ready: "Setting up your secure access…"
	phaseMaking               // "Making the code…": the key pair, the proof of work, the server
	phaseWaiting              // the code is on the server: "Waiting to be joined…"
	phaseExpired              // nobody joined in 5 minutes
	phaseLost                 // the connection was lost (block 3 gives it 30 s to come back)
	phaseError                // the server could not be reached, or refused
)

// model holds the state of Trimlet Chat and reacts to keys. It knows nothing about the window:
// copying, quitting and running in the user interface are done through callbacks.
type model struct {
	srv    server
	screen screen
	phase  phase

	// The access key (access.go).
	access  *Access
	work    *work  // the long work running, if any
	workErr string // why the last work failed, until it is tried again

	// Gino's chat. The private key lives only in memory (block 2 makes the chat's keys from it).
	code     string // grouped, as shown
	priv     *ecdh.PrivateKey
	conn     chatConn
	deadline time.Time // when the 5 minutes end, from the time left the server said
	look     []byte    // the lookup of the chat and this app's token: to come back after a loss
	token    []byte
	// Gino's connection is lost while he waits: trying to come back for 30 s (the code stays valid).
	reconnecting bool
	attempt      int    // a new start or an esc makes an older answer of the server obsolete
	errLine      string // phaseError: what went wrong
	retry        bool   // phaseError: enter tries again

	// Mario's join screen.
	joinCode editor
	joinErr  string // under the field
	joining  bool   // "Joining…"

	chat        *chat // the chat in progress or just ended (chat.go)
	dotsRunning bool  // the typing dots are being redrawn

	status gui.Line // message at the bottom (e.g. "✔ Code copied"), cleared by the next key
	cols   int
	rows   int

	copy func(text string)
	quit func()
	// alert asks for attention when the window is behind: the icon and a notification, never the
	// message text (decided). askToNotify asks the permission for notifications (once, when 1 or 2
	// is pressed: the user is looking).
	alert       func(text string)
	askToNotify func()
	still       bool        // the system asks for less motion: the dots stay still (decided)
	waitedKey   bool        // Gino waited for the access key: "Your code is ready." when it comes
	dialog      *gui.Dialog // the window in the middle, when open
	now         func() time.Time
	// async runs slow work (the proof of work, the network) away from the user interface; ui runs f
	// in the user interface and redraws, from any goroutine; later runs f in the user interface after d.
	async func(f func())
	ui    func(f func())
	later func(d time.Duration, f func())
}

func newModel(srv server) *model {
	return &model{
		srv:         srv,
		access:      &Access{},
		cols:        gui.DefaultCols,
		copy:        func(string) {},
		quit:        func() {},
		alert:       func(string) {},
		askToNotify: func() {},
		now:         time.Now,
		async:       func(f func()) { go f() },
		ui:          func(f func()) { f() },
		later:       func(time.Duration, func()) {},
	}
}

// start is the opening: the first time, the work for the access key starts at once, in the
// background (decided: for everyone, the app cannot know whether 1 or 2 will be pressed).
func (m *model) start() { m.prepare() }

// shutdown is the app closing (the window's X, Cmd+Q, Yes to Quit): an open chat is ended at once on
// the server (decided: closing the app in any way ends the chat for both).
func (m *model) shutdown() {
	if m.conn != nil {
		m.conn.end()
		m.conn = nil
	}
	if m.chat != nil && !m.chat.ended {
		m.chat.conn.end()
	}
}

// ---- Keys ----

func (m *model) Key(k string) {
	if m.dialog != nil {
		d := m.dialog
		m.dialog = nil
		if !d.Key(k) {
			m.dialog = d
		}
		return
	}
	m.status = nil
	switch m.screen {
	case screenMenu:
		if k == gui.KeyEsc {
			m.askQuit()
		}
	case screenJoin:
		switch {
		case k == gui.KeyEsc:
			m.toMenu()
		case m.joining:
		case k == gui.KeyEnter:
			m.join()
		default:
			if m.joinCode.key(k) {
				m.joinErr = ""
			}
		}
	case screenChat:
		m.chatKey(k)
	case screenStart:
		switch m.phase {
		case phaseWaiting:
			if k == gui.KeyEsc {
				m.dialog = gui.YesNo("Cancel this chat?", m.cancelChat, true)
			}
		case phaseExpired, phaseLost:
			switch k {
			case gui.KeyEnter:
				m.makeCode()
			case gui.KeyEsc:
				m.toMenu()
			}
		case phaseError:
			switch {
			case k == gui.KeyEnter && m.retry:
				m.makeCode()
			case k == gui.KeyEsc:
				m.toMenu()
			}
		default: // phaseKey, phaseMaking: nothing is on the server yet
			if k == gui.KeyEsc {
				m.toMenu()
			}
		}
	}
}

func (m *model) Rune(r rune) {
	if m.dialog != nil {
		d := m.dialog
		m.dialog = nil
		if !d.Rune(r) {
			m.dialog = d
		}
		return
	}
	m.status = nil
	switch m.screen {
	case screenMenu:
		switch r {
		case '1':
			m.startChat()
		case '2':
			m.askToNotify()
			m.screen, m.joining, m.joinErr = screenJoin, false, ""
			m.joinCode.clear()
		case 'q':
			m.askQuit()
		}
	case screenJoin:
		if !m.joining {
			m.joinCode.insert(string(r))
			m.joinErr = ""
		}
	case screenChat:
		m.chatRune(r)
	case screenStart:
		if r == 'c' && m.phase == phaseWaiting {
			m.copy(m.code)
			m.status = gui.Line{gui.Strong("✔ Code copied", gui.Amber)}
		}
	}
}

func (m *model) Paste(text string) {
	switch {
	case m.screen == screenJoin && !m.joining:
		m.joinCode.insert(text)
		m.joinErr = ""
	case m.screen == screenChat:
		m.chatPaste(text)
	}
}

func (m *model) CanPaste() bool {
	return (m.screen == screenJoin && !m.joining) || (m.screen == screenChat && !m.chat.ended)
}

func (m *model) askQuit() { m.dialog = gui.QuitDialog("Trimlet Chat", m.quit) }

// toMenu goes back to the menu; whatever was on the server is ended, and the chat is forgotten
// (its keys and messages leave the memory with it).
func (m *model) toMenu() {
	m.attempt++
	m.dropChat()
	if c := m.chat; c != nil {
		m.chat = nil
		if !c.ended {
			m.async(c.conn.end)
		}
	}
	m.joining = false
	m.joinCode.clear()
	m.screen = screenMenu
}

// dropChat forgets the chat: its key, its code, its connection (ended on the server).
func (m *model) dropChat() {
	if c := m.conn; c != nil {
		m.conn = nil
		m.async(c.end)
	}
	m.code, m.priv = "", nil
}

// ---- Start a chat (Gino) ----

// startChat is 1 on the menu: the code, at once if the access key is ready, otherwise as soon as it
// is (decided: the code screen appears by itself when the key is ready; the 5 minutes start only
// when the code is on the server).
func (m *model) startChat() {
	m.screen = screenStart
	m.askToNotify()
	m.waitedKey = m.access.Key == ""
	if m.access.Key == "" {
		m.phase = phaseKey
		m.prepare()
		return
	}
	m.makeCode()
}

// keyReady is called when the work for the key ends: Gino may be waiting for it.
func (m *model) keyReady() {
	if m.screen == screenStart && m.phase == phaseKey {
		m.makeCode()
	}
}

// makeCode makes the key pair of a new chat, its lookup and the small proof of work, and opens the
// chat on the server. The code is shown only once the server has it.
func (m *model) makeCode() {
	m.dropChat()
	m.attempt++
	attempt := m.attempt
	m.phase = phaseMaking
	key := m.access.key()
	m.async(func() {
		code, priv, err := newChatKey()
		var conn chatConn
		var w waiting
		if err == nil {
			look := lookup(priv.PublicKey().Bytes())
			var ch challenge
			if ch, err = m.srv.challenge(workCode); err == nil {
				nonce := powSolve(ch.Challenge, b64(look), ch.Difficulty)
				conn, w, err = m.srv.start(key, look, ch.Challenge, nonce)
			}
		}
		m.ui(func() { m.made(attempt, code, priv, conn, w, err) })
	})
}

// made is the server's answer to a new chat.
func (m *model) made(attempt int, code string, priv *ecdh.PrivateKey, conn chatConn, w waiting, err error) {
	if attempt != m.attempt { // Gino left meanwhile
		if conn != nil {
			m.async(conn.end)
		}
		return
	}
	switch {
	case errors.Is(err, errUnknownKey): // purged, or a year unused: a new key, then the code
		m.phase = phaseKey
		m.forgetKey()
		return
	case errors.Is(err, errServerFull):
		m.phase, m.errLine, m.retry = phaseError, "✖ The server is full right now. Try again later.", false
		return
	case isNetwork(err) || errors.Is(err, errServerBusy):
		m.phase, m.errLine, m.retry = phaseError, "✖ Could not reach the server.", true
		return
	case err != nil:
		m.phase, m.errLine, m.retry = phaseError, fmt.Sprintf("✖ Could not start the chat: %v.", err), true
		return
	}
	if m.waitedKey {
		m.waitedKey = false
		m.alert("Your code is ready.")
	}
	m.phase, m.code, m.priv, m.conn = phaseWaiting, code, priv, conn
	m.look, m.token, m.reconnecting = lookup(priv.PublicKey().Bytes()), w.Token, false
	m.deadline = m.now().Add(time.Duration(w.SecondsLeft) * time.Second)
	m.tick(attempt)
	go m.listen(conn)
}

// listen passes the events of a connection to the user interface, in order: one reader for the whole
// life of the connection (waiting, then the chat).
func (m *model) listen(conn chatConn) {
	for ev := range conn.events() {
		m.ui(func() { m.connEvent(conn, ev) })
	}
}

// connEvent gives an event to the screen of its connection; events of a connection left behind are
// dropped.
func (m *model) connEvent(conn chatConn, ev event) {
	switch {
	case m.chat != nil && m.chat.conn == conn:
		m.chatEvent(ev)
	case m.conn == conn && m.screen == screenStart && m.phase == phaseWaiting:
		m.event(ev)
	}
}

func (m *model) event(ev event) {
	switch ev.kind {
	case evJoined:
		m.joined(ev)
		return
	case evDropped: // the same 30 s as in a chat (decided): the code stays valid meanwhile
		m.reconnecting = true
		m.animate()
		attempt := m.attempt
		m.reconnect(m.look, m.token, m.now(), func(conn chatConn, _ bool, err error) {
			if attempt != m.attempt || m.phase != phaseWaiting {
				if conn != nil {
					m.async(conn.end)
				}
				return
			}
			m.reconnecting = false
			if err == nil {
				m.conn = conn
				go m.listen(conn)
				return
			}
			m.phase = phaseLost
			if !m.now().Before(m.deadline) {
				m.phase = phaseExpired // the 5 minutes ended meanwhile: the end of the waiting
			}
			m.conn, m.code, m.priv = nil, "", nil
		})
		return
	case evExpired:
		m.phase = phaseExpired
	case evLost, evEnded, evReplaced: // replaced: only another copy of this app's key can do it
		m.phase = phaseLost
	default:
		return
	}
	m.conn = nil
	m.code, m.priv = "", nil // the code is no longer valid: it must not be copied by mistake
}

// joined: someone joined Gino's chat. His proof is checked: only who has the code can make it (the
// server cannot: it never had the code). Right → Gino's proof, and the chat starts; wrong → refused,
// and the chat goes on waiting.
func (m *model) joined(ev event) {
	gino := m.priv.PublicKey().Bytes()
	k, err := deriveKeys(m.priv, ev.pub, gino, ev.pub, true)
	if err != nil || !k.checkProof("joiner", ev.proof) {
		m.conn.send("reject", nil)
		return
	}
	m.conn.send("confirm", map[string][]byte{"proof": k.proof("starter")})
	m.enterChat(k, m.conn, m.look, m.token, true) // the connection already has its reader (listen)
	m.alert("Joined.")
}

// ---- Join a chat (Mario) ----

// join is Enter on the join screen: the code is checked here first (its format), then the server.
func (m *model) join() {
	gino, err := decode32(m.joinCode.String(), keyLen)
	if err != nil {
		m.joinErr = "✖ Wrong code format."
		return
	}
	m.joining, m.joinErr = true, ""
	m.attempt++
	attempt := m.attempt
	m.async(func() {
		var k *chatKeys
		var conn chatConn
		var ok event
		priv, err := newJoinKey()
		if err == nil {
			mario := priv.PublicKey().Bytes()
			if k, err = deriveKeys(priv, gino, gino, mario, false); err == nil {
				conn, ok, err = m.srv.join(lookup(gino), mario, k.proof("joiner"))
			}
		}
		if err == nil && ok.kind == evConfirmed && !k.checkProof("starter", ok.proof) {
			conn.end() // not Gino's app: only whoever controls the server could answer so
			err = errors.New("could not reach the server (the connection was lost)")
		}
		m.ui(func() { m.joinAnswer(attempt, k, conn, lookup(gino), ok, err) })
	})
}

func (m *model) joinAnswer(attempt int, k *chatKeys, conn chatConn, look []byte, ok event, err error) {
	if attempt != m.attempt {
		if conn != nil {
			m.async(conn.end)
		}
		return
	}
	m.joining = false
	switch {
	case err == nil:
		m.enterChat(k, conn, look, ok.token, ok.kind == evConfirmed)
		go m.listen(conn)
	case errors.Is(err, errInvalidCode):
		m.joinErr = "✖ Invalid code."
	case errors.Is(err, errInUse):
		m.joinErr = "✖ Code already in use: the chat is in progress."
	case isNetwork(err):
		m.joinErr = "✖ Could not reach the server."
	default:
		m.joinErr = fmt.Sprintf("✖ Could not join: %v.", err)
	}
}

// tick redraws the countdown every second while the chat waits.
func (m *model) tick(attempt int) {
	m.later(time.Second, func() {
		if attempt == m.attempt && m.phase == phaseWaiting {
			m.tick(attempt)
		}
	})
}

// The 30 s of a lost connection (decided), tried every 2 s (mine).
const (
	reconnectFor   = 30 * time.Second
	reconnectEvery = 2 * time.Second
)

var errGaveUp = errors.New("the connection was lost")

// reconnect tries to bring back this app's lost connection with its token, every 2 s, for 30 s from
// lostAt; done gets the new connection, or errGaveUp (time over, or the server no longer has the chat).
func (m *model) reconnect(look, token []byte, lostAt time.Time, done func(conn chatConn, away bool, err error)) {
	var try func()
	try = func() {
		m.async(func() {
			conn, away, err := m.srv.resume(look, token)
			m.ui(func() {
				switch {
				case err == nil:
					done(conn, away, nil)
				case errors.Is(err, errInvalidCode) || m.now().Sub(lostAt) >= reconnectFor:
					done(nil, false, errGaveUp)
				default:
					m.later(reconnectEvery, try)
				}
			})
		})
	}
	try()
}

// cancelChat is Yes to "Cancel this chat?": the code dies, back to the menu.
func (m *model) cancelChat() { m.toMenu() }

// countdown is the time left as decided (docs/design.md 3c): the nearest minute in words, then from
// 0:30 the seconds.
func countdown(left time.Duration) string {
	if left <= 30*time.Second {
		s := max(int((left+time.Second-1)/time.Second), 0)
		return fmt.Sprintf("0:%02d", s)
	}
	n := int((left - 30*time.Second + time.Minute - 1) / time.Minute)
	if n == 1 {
		return "about 1 minute left"
	}
	return fmt.Sprintf("about %d minutes left", n)
}
