// Trimlet Chat: a written phone call. One code, 5 minutes to join, nothing kept.
// See docs/apps/chat.md and docs/design.md section 3c.
package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"

	"github.com/trimlet/gui"
)

func main() {
	// One copy only: a second one asks the first to come to the front, and closes.
	var window atomic.Pointer[fyne.Window]
	if path, err := dataPath(); err == nil {
		if !oneCopy(filepath.Dir(path), func() {
			if w := window.Load(); w != nil {
				fyne.Do(func() { gui.BringToFront(*w) })
			}
		}) {
			os.Exit(0)
		}
	}
	a, w := gui.NewWindow("local.trimletchat", "Trimlet Chat")
	window.Store(&w)
	gui.UsePalette("brown") // Chat's color (decided 2026-11-23, Bat's before)
	path, err := dataPath()
	var access *Access
	if err == nil {
		access, err = loadAccess(path)
	}
	if err != nil {
		msg := canvas.NewText("✘ Could not read the access key: "+err.Error(), gui.Red)
		msg.TextSize = gui.TextSize
		msg.TextStyle.Monospace = true
		w.SetContent(container.NewPadded(container.NewVBox(msg)))
		gui.Run(a, w)
		return
	}

	m := newModel(newHTTPServer())
	m.access = access
	m.copy = func(text string) { a.Clipboard().SetContent(text) }
	m.quit = a.Quit
	// The alerts only when the window is not in front (decided): the icon, and a notification with
	// no message text (the system keeps notifications in its notification centre).
	var inFront atomic.Bool
	inFront.Store(true)
	a.Lifecycle().SetOnEnteredForeground(func() { inFront.Store(true) })
	a.Lifecycle().SetOnExitedForeground(func() { inFront.Store(false) })
	m.alert = func(text string) {
		if inFront.Load() {
			return
		}
		gui.RequestAttention(w)
		a.SendNotification(fyne.NewNotification("Trimlet Chat", text))
	}
	m.askToNotify = gui.AskToNotify
	m.still = gui.ReduceMotion()
	var screen *gui.Screen
	m.ui = func(f func()) { screen.Do(f) }
	m.later = func(d time.Duration, f func()) { time.AfterFunc(d, func() { screen.Do(f) }) }
	screen = gui.NewScreen(a, w, m)
	// Closing the app in any way (the window's X, Cmd+Q, Yes to Quit) ends an open chat at once.
	a.Lifecycle().SetOnStopped(m.shutdown)
	// The first time, the work for the access key starts at once, in the background.
	gui.Run(a, w, func() { screen.Do(m.start) })
}
