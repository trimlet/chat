package main

import (
	"strings"

	"github.com/trimlet/gui"
)

// editor is a line where one writes: the chat's `>` line (several lines with Shift+Enter or a paste)
// and the code on the join screen. The text is kept exactly as typed or pasted.
type editor struct {
	text []rune
	cur  int // position of the cursor, 0 … len(text)
}

func (e *editor) String() string { return string(e.text) }
func (e *editor) empty() bool    { return len(e.text) == 0 }
func (e *editor) clear()         { e.text, e.cur = nil, 0 }

func (e *editor) set(s string) {
	e.text = []rune(s)
	e.cur = len(e.text)
}

func (e *editor) insert(s string) {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	rs := []rune(s)
	e.text = append(e.text[:e.cur], append(rs, e.text[e.cur:]...)...)
	e.cur += len(rs)
}

// key handles the editing keys; it returns true if the text or the cursor changed.
func (e *editor) key(k string) bool {
	switch k {
	case gui.KeyBackspace:
		if e.cur == 0 {
			return false
		}
		e.text = append(e.text[:e.cur-1], e.text[e.cur:]...)
		e.cur--
	case gui.KeyLeft:
		e.cur = max(e.cur-1, 0)
	case gui.KeyRight:
		e.cur = min(e.cur+1, len(e.text))
	case gui.KeyHome:
		e.cur = 0
	case gui.KeyEnd:
		e.cur = len(e.text)
	default:
		return false
	}
	return true
}

// shown is how a character appears in the console (a tab as →, as in the other trimlet apps);
// copying gives the character itself.
func shown(r rune) rune {
	if r == '\t' {
		return '→'
	}
	return r
}

// wrapRunes cuts one line of text into rows of at most width characters (by character, not by word:
// a command must stay exact). An empty line is one empty row.
func wrapRunes(line []rune, width int) [][]rune {
	width = max(width, 1)
	if len(line) == 0 {
		return [][]rune{nil}
	}
	var rows [][]rune
	for len(line) > width {
		rows = append(rows, line[:width])
		line = line[width:]
	}
	return append(rows, line)
}

// editRow is one row of the editor as drawn: its characters and where it starts in the text.
type editRow struct {
	runes []rune
	start int  // offset in the text
	cont  bool // the same line as the row before, cut to fit
}

// rows cuts the text into the rows drawn, width characters each.
func (e *editor) rows(width int) []editRow {
	var out []editRow
	off := 0
	for _, line := range strings.Split(string(e.text), "\n") {
		rs := []rune(line)
		for i, r := range wrapRunes(rs, width) {
			out = append(out, editRow{runes: r, start: off + i*max(width, 1), cont: i > 0})
		}
		off += len(rs) + 1
	}
	return out
}

// render draws the editor after prompt (on the first row; the next rows are indented as much), with
// the cursor as a green block, in at most maxRows rows (those around the cursor).
func (e *editor) render(prompt gui.Span, width, maxRows int) []gui.Line {
	indent := len([]rune(prompt.Text))
	rows := e.rows(width - indent)
	// The row of the cursor: the last row whose start is at or before it (at the end of a full row
	// the cursor goes on the next row, if there is one).
	cur := 0
	for i, r := range rows {
		if r.start <= e.cur {
			cur = i
		}
	}
	first := max(0, min(cur-maxRows+1, len(rows)-maxRows))
	var out []gui.Line
	for i := first; i < len(rows) && i < first+maxRows; i++ {
		r := rows[i]
		l := gui.Line{gui.Plain(strings.Repeat(" ", indent))}
		if i == 0 {
			l = gui.Line{prompt}
		}
		at := e.cur - r.start
		for j, c := range r.runes {
			if i == cur && j == at {
				l = append(l, gui.Span{Text: string(shown(c)), Color: gui.Background, Bg: gui.Accent})
			} else {
				l = append(l, gui.Plain(string(shown(c))))
			}
		}
		if i == cur && at >= len(r.runes) {
			l = append(l, gui.Colored("█", gui.Accent))
		}
		out = append(out, l)
	}
	return out
}
