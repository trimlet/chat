package main

import (
	"github.com/trimlet/gui"
)

type line = gui.Line

func (m *model) Render(cols, rows int) []line {
	m.cols, m.rows = cols, rows
	var lines []line
	switch m.screen {
	case screenMenu:
		lines = m.renderMenu()
	case screenStart:
		lines = m.renderStart()
	case screenJoin:
		lines = m.renderJoin()
	case screenChat:
		lines = m.renderChat()
	}
	if m.dialog != nil {
		return gui.WithDialog(lines, m.dialog, cols, rows)
	}
	return lines
}

// footer is the status, if any, and the hint.
func (m *model) footer(hint string) []line {
	lines := []line{nil}
	if m.status != nil {
		lines = append(lines, m.status, nil)
	}
	return append(lines, line{gui.Colored(hint, gui.Muted)})
}

// renderMenu: no list above it, since nothing is kept (decided).
func (m *model) renderMenu() []line {
	lines := []line{
		{gui.Plain("  "), gui.Strong("1", gui.Accent), gui.Plain("  Start a chat")},
		{gui.Plain("  "), gui.Strong("2", gui.Accent), gui.Plain("  Join a chat")},
	}
	return append(lines, m.footer("1 start a chat · 2 join a chat · esc quit")...)
}

// renderJoin: Mario pastes the code (screen mine, to review in the app).
func (m *model) renderJoin() []line {
	lines := []line{{gui.Strong("Join a chat", nil)}, nil, {gui.Plain("Paste the code you received:")}}
	if m.joining {
		lines = append(lines, line{gui.Strong("> ", gui.Accent), gui.Plain(m.joinCode.String())}, nil,
			line{gui.Colored("Joining…", gui.Muted)})
		return append(lines, m.footer("esc cancel")...)
	}
	lines = append(lines, m.joinCode.render(gui.Strong("> ", gui.Accent), max(m.cols, 10), 3)...)
	if m.joinErr != "" {
		lines = append(lines, nil, line{gui.Strong(m.joinErr, gui.Red)})
	}
	return append(lines, m.footer("enter join · esc back")...)
}

func (m *model) renderStart() []line {
	lines := []line{{gui.Strong("Start a chat", nil)}, nil}
	muted := func(t string) line { return line{gui.Colored(t, gui.Muted)} }
	switch m.phase {
	case phaseKey:
		lines = append(lines, muted(m.waitText()))
		return append(lines, m.footer("esc cancel")...)
	case phaseMaking:
		lines = append(lines, muted("Making the code…"))
		return append(lines, m.footer("esc cancel")...)
	case phaseWaiting:
		lines = append(lines,
			line{gui.Plain("Send this code to the person you want to chat with.")},
			nil,
			line{gui.Plain("   "), gui.Colored(m.code, gui.ValueFg), gui.Plain("   "), gui.Strong("c", gui.Accent), gui.Colored(" copy", gui.Muted)},
			nil,
			muted("Waiting to be joined…  "+countdown(m.deadline.Sub(m.now()))),
		)
		if m.reconnecting { // mine: the code stays, the line says why nothing can happen now
			lines = append(lines, nil, m.reconnectRow())
		}
		return append(lines, m.footer("c copy · esc cancel")...)
	case phaseExpired:
		lines = append(lines, line{gui.Strong("✖ No one joined: the code has expired.", gui.Red)})
		return append(lines, m.footer("enter new code · esc back")...)
	case phaseLost:
		lines = append(lines, line{gui.Strong("✖ The connection was lost: the code no longer works.", gui.Red)})
		return append(lines, m.footer("enter new code · esc back")...)
	default: // phaseError
		lines = append(lines, line{gui.Strong(m.errLine, gui.Red)})
		if m.retry {
			return append(lines, m.footer("enter try again · esc back")...)
		}
		return append(lines, m.footer("esc back")...)
	}
}
