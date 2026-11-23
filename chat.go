package main

import (
	"fmt"
	"image/color"
	"math"
	"strings"
	"time"

	"github.com/trimlet/gui"
)

// The chat screen (block 2, docs/apps/chat.md): your lines start with › in green, the other's are
// white and indented two spaces; service lines in grey; the > line fixed at the bottom; ↑↓ move the
// messages by half a window; at the end the messages stay one minute, then the app goes back to the
// menu by itself.

type itemKind int

const (
	itMine itemKind = iota
	itTheirs
	itService
)

type item struct {
	kind  itemKind
	text  string
	n     uint64 // mine: its number, for the acknowledgement
	acked bool   // mine: the other app has it (green)
}

// chat is a chat in progress, or just ended.
type chat struct {
	s     *session
	conn  chatConn
	look  []byte // the chat's lookup and this app's token: to come back after a lost connection
	token []byte

	outbox    []sent    // my messages waiting to leave, at the pace (block 4)
	tokens    float64   // the reserve of messages in a row, as the server counts it
	tokensAt  time.Time // when tokens was last refilled
	pumping   bool      // a later pump is scheduled
	warnQuiet int       // the index of "will end in 1 minute: no messages" in items, or -1

	confirmed    bool      // Mario: Gino's app accepted (false while Mario waits for Gino, away)
	reconnecting bool      // this app's connection is lost: trying to come back for 30 s
	otherAway    bool      // the other's connection is lost: it has 30 s
	lostAt       time.Time // when this app's connection was lost
	items        []item
	input        editor
	scroll       int  // rows scrolled up from the bottom
	newBelow     bool // a message arrived while scrolled up
	tooLong      string

	theirTyping time.Time // the dots show until then
	typingOn    bool      // this side said "typing" and not "stopped" yet
	typingSent  time.Time // when "typing" was last sent
	lastKey     time.Time

	ended    bool
	closesAt time.Time // ended: back to the menu by itself

	rows      []rowInfo // the rows of the last drawing, for copying
	lastTotal int       // the rows of the messages at the last drawing
}

// The times of the typing dots (mine): "typing" is said again every 3 s while one types, the dots go
// 6 s after the last one heard, and "stopped" is said 5 s after the last key.
const (
	typingEvery  = 3 * time.Second
	typingShown  = 6 * time.Second
	typingIdle   = 5 * time.Second
	closeAfter   = time.Minute // decided: the messages stay one minute after the end
	dotsCycle    = 1600 * time.Millisecond
	dotsInterval = 100 * time.Millisecond
)

// enterChat shows the chat screen. The chat has started (both proofs checked), or, for Mario, waits
// for Gino who is away (confirmed false: decided, Mario sees the chat screen with "reconnecting", and
// the chat starts when Gino is back). The events of conn reach chatEvent through listen (model.go).
func (m *model) enterChat(k *chatKeys, conn chatConn, look, token []byte, confirmed bool) {
	m.attempt++
	m.code, m.priv, m.conn = "", nil, nil
	m.reconnecting = false
	m.chat = &chat{s: newSession(k), conn: conn, look: look, token: token, confirmed: confirmed, otherAway: !confirmed,
		tokens: msgBurst, tokensAt: m.now(), warnQuiet: -1}
	if confirmed {
		m.started()
	} else {
		m.animate()
	}
	m.screen = screenChat
}

// started writes the first line of the chat.
func (m *model) started() {
	c := m.chat
	c.items = append(c.items, item{kind: itService, text: "── chat started " + m.now().Format("15:04") + " ──"})
}

// blocked: nothing can leave now (a connection is lost, or Gino has not accepted yet); messages wait
// in the app, grey, and leave by themselves when it is over (decided).
func (c *chat) blocked() bool { return c.reconnecting || c.otherAway || !c.confirmed }

// The pace (decided): at most one message every 2 s on average, with a reserve of 5 in a row. The
// app keeps it by itself, so the server (which allows one more, for the network) never ends a chat of
// an honest app; extra messages wait in grey with "sending…".
const (
	msgEvery = 2 * time.Second
	msgBurst = 5.0
)

// flush puts again every message the other app has not acknowledged in the outbox, in order (after a
// lost connection: the other drops those it already has), and sends what the pace allows.
func (m *model) flush() {
	c := m.chat
	c.outbox = append([]sent(nil), c.s.unacked...)
	m.pump()
}

// pump sends the messages of the outbox the pace allows now, and comes back for the others.
func (m *model) pump() {
	c := m.chat
	if c.ended || c.blocked() {
		return
	}
	now := m.now()
	c.tokens = min(c.tokens+now.Sub(c.tokensAt).Seconds()/msgEvery.Seconds(), msgBurst)
	c.tokensAt = now
	for len(c.outbox) > 0 && c.tokens >= 1 {
		c.conn.send(evMsg, map[string][]byte{"data": c.outbox[0].data})
		c.outbox = c.outbox[1:]
		c.tokens--
	}
	if len(c.outbox) > 0 && !c.pumping {
		c.pumping = true
		wait := time.Duration((1 - c.tokens) * float64(msgEvery))
		m.later(max(wait, 10*time.Millisecond), func() {
			c.pumping = false
			if m.chat == c {
				m.pump()
			}
		})
	}
}

// clearQuietWarning removes "will end in 1 minute: no messages": a message was written (decided).
func (c *chat) clearQuietWarning() {
	if c.warnQuiet >= 0 && c.warnQuiet < len(c.items) {
		c.items = append(c.items[:c.warnQuiet], c.items[c.warnQuiet+1:]...)
	}
	c.warnQuiet = -1
}

// span says a rule's time in words: 3-hour, 15-minute (adjective) or 3 hours, 15 minutes.
func span(secs int, adjective bool) string {
	n, unit := secs, "second"
	switch {
	case secs > 0 && secs%3600 == 0:
		n, unit = secs/3600, "hour"
	case secs > 0 && secs%60 == 0:
		n, unit = secs/60, "minute"
	}
	if adjective {
		return fmt.Sprintf("%d-%s", n, unit)
	}
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func (m *model) chatEvent(ev event) {
	c := m.chat
	if c.ended {
		return
	}
	switch ev.kind {
	case evMsg:
		text, n, dup, err := c.s.openMessage(ev.data)
		if err != nil {
			m.endChat("── a message was lost: the chat was ended ──")
			m.async(c.conn.end)
			return
		}
		if !dup {
			m.alert("New message.")
			c.clearQuietWarning()
			c.items = append(c.items, item{kind: itTheirs, text: text})
			c.theirTyping = time.Time{}
			if c.scroll > 0 {
				c.newBelow = true
			}
		}
		if data, err := c.s.ack(n); err == nil {
			c.conn.send(evAck, map[string][]byte{"data": data})
		}
	case evAck:
		if upTo, ok := c.s.openAck(ev.data); ok {
			c.s.acked(upTo)
			for i := range c.items {
				if c.items[i].kind == itMine && c.items[i].n <= upTo {
					c.items[i].acked = true
				}
			}
		}
	case evTyping:
		if on, ok := c.s.openTyping(ev.data); ok {
			if on {
				c.theirTyping = m.now().Add(typingShown)
				m.animate()
			} else {
				c.theirTyping = time.Time{}
			}
		}
	case evAway:
		c.otherAway = true
		c.theirTyping = time.Time{}
		m.animate()
	case evBack:
		if c.confirmed {
			c.otherAway = false
			m.flush()
		}
	case evConfirmed: // Mario waited for Gino, who was away: Gino's app has now accepted
		if c.confirmed || !c.s.keys.checkProof("starter", ev.proof) {
			m.endChat("── the connection was lost ──") // not Gino's app: only whoever controls the server
			m.async(c.conn.end)
			return
		}
		c.confirmed, c.otherAway = true, false
		m.started()
		m.flush()
	case evDropped:
		c.reconnecting, c.lostAt = true, m.now()
		c.theirTyping = time.Time{}
		m.animate()
		m.reconnect(c.look, c.token, c.lostAt, func(conn chatConn, away bool, err error) {
			if m.chat != c || c.ended {
				if conn != nil {
					m.async(conn.end)
				}
				return
			}
			if err != nil {
				m.endChat("── the connection was lost ──")
				return
			}
			c.conn, c.reconnecting, c.otherAway = conn, false, away || !c.confirmed
			go m.listen(conn)
			m.flush()
		})
	case evWarning:
		m.alert("The chat will end in 1 minute.")
		switch ev.reason {
		case "silence":
			if c.warnQuiet < 0 {
				c.warnQuiet = len(c.items)
				c.items = append(c.items, item{kind: itService, text: "── the chat will end in 1 minute: no messages ──"})
			}
		case "limit":
			c.items = append(c.items, item{kind: itService, text: "── the chat will end in 1 minute: " + span(ev.secs, true) + " limit ──"})
		}
	case evEnded, evReplaced:
		switch ev.reason {
		case "silence":
			m.endChat("── the chat ended after " + span(ev.secs, false) + " of silence ──")
		case "limit":
			m.endChat("── the chat ended: " + span(ev.secs, true) + " limit ──")
		case "server":
			m.endChat("── the chat was ended by the server ──")
		default:
			m.endChat("── chat ended ──")
		}
	case evLost:
		m.endChat("── the connection was lost ──")
	}
}

// endChat: the messages stay, nothing can be written, back to the menu after a minute.
func (m *model) endChat(line string) {
	m.alert("The chat ended.")
	c := m.chat
	c.ended, c.reconnecting, c.otherAway = true, false, false
	c.theirTyping = time.Time{}
	c.items = append(c.items, item{kind: itService, text: line}, item{kind: itService, text: "Nothing was kept."})
	c.closesAt = m.now().Add(closeAfter)
	attempt := m.attempt
	var tick func()
	tick = func() {
		if attempt != m.attempt || m.chat != c {
			return
		}
		if !m.now().Before(c.closesAt) {
			m.toMenu()
			return
		}
		m.later(time.Second, tick)
	}
	m.later(time.Second, tick)
}

// animating says whether something moves on the screen: the typing dots, or "reconnecting".
func (m *model) animating() bool {
	if c := m.chat; c != nil && m.screen == screenChat {
		return !c.ended && (c.reconnecting || c.otherAway || m.now().Before(c.theirTyping))
	}
	return m.screen == screenStart && m.phase == phaseWaiting && m.reconnecting
}

// animate redraws the screen often while something moves.
func (m *model) animate() {
	if m.dotsRunning {
		return
	}
	m.dotsRunning = true
	var tick func()
	tick = func() {
		if !m.animating() {
			m.dotsRunning = false
			return
		}
		m.later(dotsInterval, tick)
	}
	m.later(dotsInterval, tick)
}

// chatKey handles a key on the chat screen.
func (m *model) chatKey(k string) {
	c := m.chat
	switch k {
	case gui.KeyUp:
		c.scroll += m.half()
		return
	case gui.KeyDown:
		c.scroll = max(c.scroll-m.half(), 0)
		if c.scroll == 0 {
			c.newBelow = false
		}
		return
	case gui.KeyEsc:
		if c.ended {
			m.toMenu()
		} else {
			m.dialog = gui.YesNo("End the chat?", m.hangUp, true)
		}
		return
	}
	if c.ended {
		return
	}
	switch k {
	case gui.KeyEnter:
		m.sendMessage()
	case gui.KeyShiftEnter:
		c.input.insert("\n")
		m.typed()
	default:
		if c.input.key(k) && k == gui.KeyBackspace {
			m.typed()
		}
	}
}

// hangUp is Yes to "End the chat?": the chat ends for both, back to the menu (mine: whoever hangs up
// does not need the end screen).
func (m *model) hangUp() {
	if c := m.chat; c != nil {
		m.async(c.conn.end)
	}
	m.toMenu()
}

func (m *model) chatRune(r rune) {
	if c := m.chat; !c.ended {
		c.input.insert(string(r))
		m.typed()
	}
}

func (m *model) chatPaste(text string) {
	if c := m.chat; !c.ended {
		c.input.insert(text)
		m.typed()
	}
}

// typed: the text changed. Says "typing" if not said recently; "stopped" 5 s after the last key.
func (m *model) typed() {
	c := m.chat
	c.tooLong = ""
	now := m.now()
	c.lastKey = now
	if c.blocked() {
		return
	}
	if !c.typingOn || now.Sub(c.typingSent) >= typingEvery {
		if data, err := c.s.typing(true); err == nil {
			c.conn.send(evTyping, map[string][]byte{"data": data})
			c.typingOn, c.typingSent = true, now
		}
	}
	attempt := m.attempt
	m.later(typingIdle, func() {
		if attempt != m.attempt || m.chat != c || !c.typingOn || c.ended || m.now().Sub(c.lastKey) < typingIdle {
			return
		}
		if data, err := c.s.typing(false); err == nil {
			c.conn.send(evTyping, map[string][]byte{"data": data})
		}
		c.typingOn = false
	})
}

// sendMessage is Enter: the message goes up at once, grey with "sending…", green when the other app
// has it.
func (m *model) sendMessage() {
	c := m.chat
	text := c.input.String()
	if text == "" {
		return
	}
	if len(text) > maxMessage {
		c.tooLong = fmt.Sprintf("Too long: at most 4 KB (this is %.1f KB)", float64(len(text))/1024)
		return
	}
	n, data, err := c.s.message(text)
	if err != nil {
		return
	}
	c.clearQuietWarning()
	c.outbox = append(c.outbox, sent{n, data})
	m.pump()
	c.items = append(c.items, item{kind: itMine, text: text, n: n})
	c.input.clear()
	c.scroll, c.newBelow = 0, false
	c.typingOn = false // the message itself makes the dots go on the other side
}

// half is half the rows of the messages.
func (m *model) half() int { return max(m.msgRoom()/2, 1) }

// ---- Drawing ----

// rowInfo says where a drawn row comes from, so a selection copies the original text: not the
// marks before the lines, not the line breaks added to fit the window.
type rowInfo struct {
	text   []rune // the characters of the row, as in the original
	prefix int    // columns before them (› , the indent, > )
	cont   bool   // the same line of the original as the row before, cut to fit
}

// The colors of the dots: from a dark grey to grey 500 and back.
var dotDark color.Color = gui.Palettes["grey"][8]

func mix(a, b color.Color, t float64) color.Color {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	f := func(x, y uint32) uint8 { return uint8((float64(x)*(1-t) + float64(y)*t) / 257) }
	return color.NRGBA{f(ar, br), f(ag, bg), f(ab, bb), 0xFF}
}

// dots are the three grey dots, fading one after the other (decided 2026-11-22).
func (m *model) dots() gui.Line {
	phase := float64(m.now().UnixNano()%int64(dotsCycle)) / float64(dotsCycle)
	l := gui.Line{gui.Plain("  ")}
	for i := range 3 {
		t := 0.5 - 0.5*math.Cos(2*math.Pi*(phase-float64(i)/6))
		if m.still {
			t = 1
		}
		l = append(l, gui.Colored("·", mix(dotDark, gui.Muted, t)))
	}
	return l
}

// reconnectRow is "── reconnecting ··· ──", the three dots fading all together (decided 2026-11-23:
// unlike "typing", where they fade one after the other).
func (m *model) reconnectRow() gui.Line {
	phase := float64(m.now().UnixNano()%int64(dotsCycle)) / float64(dotsCycle)
	col := mix(dotDark, gui.Muted, 0.5-0.5*math.Cos(2*math.Pi*phase))
	if m.still {
		col = gui.Muted
	}
	return gui.Line{gui.Colored("── reconnecting ", gui.Muted), gui.Colored("···", col), gui.Colored(" ──", gui.Muted)}
}

// msgRoom is the rows for the messages: the window less the > line(s) and the hint.
func (m *model) msgRoom() int {
	if m.chat == nil {
		return 1
	}
	return max(m.rows-1-len(m.chatBottom(nil)), 1)
}

// chatBottom is what stays fixed under the messages: the > line, the "too long" line, the hint.
// rows, if given, gets the rowInfo of the > line's rows.
func (m *model) chatBottom(rows *[]rowInfo) []line {
	c := m.chat
	lines := []line{nil}
	if !c.ended {
		width := max(m.cols, 10)
		maxRows := max((m.rows-1)/2, 1)
		ed := c.input.render(gui.Strong("> ", gui.Accent), width, maxRows)
		lines = append(lines, ed...)
		if rows != nil {
			*rows = append(*rows, rowInfo{})
			er := c.input.rows(width - 2)
			first := len(er) - len(ed)
			for i := range ed {
				r := er[max(first, 0)+i]
				*rows = append(*rows, rowInfo{text: r.runes, prefix: 2, cont: r.cont})
			}
		}
		if c.tooLong != "" {
			lines = append(lines, line{gui.Colored(c.tooLong, gui.Red)})
		}
		lines = append(lines, nil)
	}
	hint := "enter send · esc end chat"
	switch {
	case c.ended:
		left := max(c.closesAt.Sub(m.now()), 0)
		secs := int((left + time.Second - 1) / time.Second)
		hint = fmt.Sprintf("esc back to the menu · closes in %d:%02d", secs/60, secs%60)
	case c.scroll > 0 && c.newBelow:
		hint = "new messages ↓   ↑↓ scroll"
	case c.scroll > 0:
		hint = "↑↓ scroll"
	}
	return append(lines, line{gui.Colored(hint, gui.Muted)})
}

// renderChat draws the messages (the last ones, or higher up after ↑) and the fixed bottom.
func (m *model) renderChat() []line {
	c := m.chat
	width := max(m.cols-2, 8)
	var lines []line
	var rows []rowInfo
	add := func(l line, info rowInfo) { lines, rows = append(lines, l), append(rows, info) }
	pending := -1 // the last of my messages not acknowledged yet: "sending…" under it
	for i, it := range c.items {
		if it.kind == itMine && !it.acked {
			pending = i
		}
	}
	for i, it := range c.items {
		if it.kind == itService {
			add(line{gui.Colored(it.text, gui.Muted)}, rowInfo{text: []rune(it.text)})
			continue
		}
		col := gui.White
		mark := "  "
		if it.kind == itMine {
			col, mark = gui.Accent, "› "
			if !it.acked {
				col = gui.Muted
			}
		}
		first := true
		for _, src := range strings.Split(it.text, "\n") {
			for j, r := range wrapRunes([]rune(src), width) {
				pre := "  "
				if first {
					pre = mark
				}
				first = false
				shownRow := make([]rune, len(r))
				for k, ch := range r {
					shownRow[k] = shown(ch)
				}
				var l line
				if pre == "› " {
					l = line{gui.Strong("›", col), gui.Plain(" "), gui.Colored(string(shownRow), col)}
				} else {
					l = line{gui.Plain(pre), gui.Colored(string(shownRow), col)}
				}
				add(l, rowInfo{text: r, prefix: 2, cont: j > 0})
			}
		}
		if i == pending {
			add(line{gui.Colored("  sending…", gui.Muted)}, rowInfo{text: []rune("sending…"), prefix: 2})
		}
	}
	switch {
	case c.ended:
	case c.reconnecting || c.otherAway:
		add(m.reconnectRow(), rowInfo{})
	case m.now().Before(c.theirTyping):
		add(m.dots(), rowInfo{})
	}

	var bottomRows []rowInfo
	bottom := m.chatBottom(&bottomRows)
	room := max(m.rows-1-len(bottom), 1)
	if c.scroll > 0 && c.lastTotal > 0 {
		c.scroll += len(lines) - c.lastTotal // new rows below: the view stays where it was
	}
	c.lastTotal = len(lines)
	c.scroll = min(max(c.scroll, 0), max(len(lines)-room, 0))
	end := len(lines) - c.scroll
	start := max(end-room, 0)
	shownLines, shownRows := lines[start:end], rows[start:end]
	out := append([]line(nil), shownLines...)
	c.rows = append([]rowInfo(nil), shownRows...)
	for len(out) < room {
		out = append(out, nil)
		c.rows = append(c.rows, rowInfo{})
	}
	c.rows = append(c.rows, bottomRows...)
	return append(out, bottom...)
}

// SelectedText is the mouse selection copied as the original text (gui.Selecter): without › and the
// indent, without the line breaks added to fit the window, tabs as tabs.
func (m *model) SelectedText(fromRow, fromCol, toRow, toCol int) string {
	if m.screen != screenChat || m.chat == nil {
		return ""
	}
	rows := m.chat.rows
	var b strings.Builder
	for r := max(fromRow, 0); r <= toRow && r < len(rows); r++ {
		info := rows[r]
		if r > max(fromRow, 0) && !info.cont {
			b.WriteByte('\n')
		}
		from, to := 0, len(info.text)
		if r == fromRow {
			from = fromCol - info.prefix
		}
		if r == toRow {
			to = toCol - info.prefix + 1
		}
		from, to = min(max(from, 0), len(info.text)), min(max(to, 0), len(info.text))
		if from < to {
			b.WriteString(string(info.text[from:to]))
		}
	}
	return strings.Trim(b.String(), "\n")
}
