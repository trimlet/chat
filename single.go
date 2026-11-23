package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"time"
)

// One copy of the app (decided: a second copy does not open, it brings the open window to the front
// and closes itself; on Windows a double click on the .exe would otherwise open a second one, with a
// second code and a second chat). How (mine): a Unix socket in the app's folder, readable only by the
// user, which says only "front"; no file kept, no server. Windows 10 and later have Unix sockets too.

const socketName = "chat.sock"

// oneCopy is called at the opening. If another copy answers, it is asked to come to the front and
// oneCopy returns false: this copy must close. Otherwise this copy listens, and front is called each
// time a later copy asks (from another goroutine).
func oneCopy(dir string, front func()) bool {
	path := filepath.Join(dir, socketName)
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Write([]byte("front\n"))
		c.Close()
		return false
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return true // the app opens anyway
	}
	os.Remove(path) // left by a copy that did not close cleanly
	ln, err := net.Listen("unix", path)
	if err != nil {
		return true
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			if line, _ := bufio.NewReader(c).ReadString('\n'); line == "front\n" {
				front()
			}
			c.Close()
		}
	}()
	return true
}
