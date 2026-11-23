package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// The access key (docs/apps/chat.md, as Trimlet Seal's block 5b): every installed Trimlet Chat gets
// an anonymous access key from the server by itself, with ≈2 minutes of proof of work in the
// background, started the first time the app is opened (decided: for everyone, also who only joins).
// Only who starts a chat uses it. It is kept in access.json, the only file of the app.

// Access is the file access.json.
type Access struct {
	Key string `json:"key,omitempty"` // base64, given by the server: like a password for it

	path string
}

// dataPath is access.json in the standard folder for app settings
// (~/Library/Application Support/trimletchat on Mac, %AppData%\trimletchat on Windows).
func dataPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "trimletchat", "access.json"), nil
}

func loadAccess(path string) (*Access, error) {
	a := &Access{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, a); err != nil {
		return nil, err
	}
	return a, nil
}

// save writes the file, readable only by the user, replacing it in one step.
func (a *Access) save() error {
	if a.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

func (a *Access) key() []byte {
	k, _ := base64.StdEncoding.DecodeString(a.Key)
	return k
}

// work is the long proof of work of the key, running in the background.
type work struct {
	started    time.Time // the real clock: the time left is measured on this computer
	difficulty atomic.Uint64
	tries      atomic.Uint64
	stop       atomic.Bool
}

// The time of one try before the app has measured this computer (the author's Mac), and the work of
// a key before the server has said it (≈2 minutes there).
const (
	tryTime     = 1600 * time.Microsecond
	keyWorkHint = 75_000
)

// left is the time the work should still take, from the speed of this computer.
func (w *work) left(now time.Time) time.Duration {
	d := w.difficulty.Load()
	if d == 0 {
		d = keyWorkHint
	}
	elapsed := now.Sub(w.started)
	per := tryTime
	if n := w.tries.Load(); n >= 50 {
		per = elapsed / time.Duration(n)
	}
	return time.Duration(d)*per - elapsed
}

// waitText is the line shown while Gino must wait for the key (as Seal's, without the final dot:
// docs/design.md 3c).
func (m *model) waitText() string {
	if m.workErr != "" {
		return "Setting up your secure access… " + m.workErr + ", trying again"
	}
	left := 2 * time.Minute
	if m.work != nil {
		left = m.work.left(time.Now())
	}
	switch {
	case left <= 5*time.Second:
		return "Setting up your secure access… almost done"
	case left > 90*time.Second:
		return fmt.Sprintf("Setting up your secure access… about %d minutes left", int((left+59*time.Second)/time.Minute))
	}
	secs := int((left + 9*time.Second) / (10 * time.Second) * 10)
	return fmt.Sprintf("Setting up your secure access… about %d seconds left", secs)
}

// prepare starts the work for the key if there is none and none runs.
func (m *model) prepare() {
	if m.access.Key != "" || m.work != nil || m.workErr != "" {
		return
	}
	w := &work{started: time.Now()}
	m.work = w
	m.async(func() {
		var key []byte
		ch, err := m.srv.challenge(workKey)
		if err == nil {
			w.difficulty.Store(ch.Difficulty)
			nonce, ok := powSearch(ch.Challenge, "key", ch.Difficulty, &w.tries, func() { m.ui(func() {}) }, &w.stop)
			if ok {
				key, err = m.srv.newKey(ch.Challenge, nonce)
			} else {
				err = errStopped
			}
		}
		m.ui(func() { m.workDone(w, key, err) })
	})
}

var errStopped = errors.New("stopped")

// workDone records the end of the work, then goes on with whatever waited for it.
func (m *model) workDone(w *work, key []byte, err error) {
	if m.work != w {
		return
	}
	m.work = nil
	switch {
	case errors.Is(err, errStopped):
		return
	case err != nil:
		m.workErr = "could not reach the server"
		if !errors.Is(err, errServerBusy) && !errors.Is(err, errChallenge) && !isNetwork(err) {
			m.workErr = "the server answered " + err.Error()
		}
		m.later(15*time.Second, m.retryWork)
		return
	}
	m.access.Key = base64.StdEncoding.EncodeToString(key)
	if err := m.access.save(); err != nil {
		m.workErr = "could not save the key (" + err.Error() + ")"
	}
	m.keyReady()
}

// retryWork tries again after a failed work.
func (m *model) retryWork() {
	m.workErr = ""
	m.prepare()
}

// forgetKey drops a key the server no longer knows (purged in an attack, or a year unused) and starts
// the work for a new one.
func (m *model) forgetKey() {
	m.access.Key = ""
	if err := m.access.save(); err != nil {
		m.workErr = "could not save the key (" + err.Error() + ")"
		return
	}
	m.prepare()
}

// isNetwork says whether err is the connection, not an answer of the server.
func isNetwork(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "could not reach the server")
}
