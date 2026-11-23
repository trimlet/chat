package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestPlayMario plays the other person for a try by hand (block 2, docs/apps/chat.md): with
// TRIMLETCHAT_PLAY_MARIO=<the code shown by the app> and TRIMLETCHAT_SERVER set, it joins that chat,
// says hello, sends a command on three lines, then answers every message with "ricevuto: …" after
// showing the typing dots for 2 s; "fine" makes it hang up. It runs until the chat ends (at most 30
// minutes). Run with -v -timeout 40m.
func TestPlayMario(t *testing.T) {
	code := os.Getenv("TRIMLETCHAT_PLAY_MARIO")
	if code == "" || os.Getenv("TRIMLETCHAT_SERVER") == "" {
		t.Skip("TRIMLETCHAT_PLAY_MARIO and TRIMLETCHAT_SERVER not set")
	}
	gino, err := decode32(code, keyLen)
	if err != nil {
		t.Fatal("not a code:", err)
	}
	priv, _ := newJoinKey()
	mario := priv.PublicKey().Bytes()
	k, err := deriveKeys(priv, gino, gino, mario, false)
	if err != nil {
		t.Fatal(err)
	}
	conn, ok, err := newHTTPServer().join(lookup(gino), mario, k.proof("joiner"))
	if err != nil {
		t.Fatal("join:", err)
	}
	if !k.checkProof("starter", ok.proof) {
		t.Fatal("Gino's proof is wrong")
	}
	t.Log("joined")
	s := newSession(k)
	say := func(text string) {
		if data, err := s.typing(true); err == nil {
			conn.send(evTyping, map[string][]byte{"data": data})
		}
		time.Sleep(2 * time.Second)
		if _, data, err := s.message(text); err == nil {
			conn.send(evMsg, map[string][]byte{"data": data})
		}
		t.Logf("sent %q", text)
	}
	say("ciao! sono Mario, un programma di prova. Scrivi qualcosa e ti rispondo; scrivi fine per chiudere.")
	say("cd /var/www\ngit pull\nsystemctl reload caddy")

	deadline := time.After(30 * time.Minute)
	for {
		select {
		case ev, open := <-conn.events():
			if !open {
				return
			}
			switch ev.kind {
			case evMsg:
				text, n, dup, err := s.openMessage(ev.data)
				if err != nil {
					t.Log("a message was lost: hanging up")
					conn.end()
					return
				}
				if data, err := s.ack(n); err == nil {
					conn.send(evAck, map[string][]byte{"data": data})
				}
				if dup {
					continue
				}
				t.Logf("received %q", text)
				if strings.TrimSpace(strings.ToLower(text)) == "fine" {
					say("ok, chiudo. ciao!")
					time.Sleep(time.Second)
					conn.end()
					t.Log("hung up")
					return
				}
				say("ricevuto: " + text)
			case evAck:
				if n, ok := s.openAck(ev.data); ok {
					s.acked(n)
				}
			case evAway:
				t.Log("Gino's connection is lost")
			case evBack:
				t.Logf("Gino is back: sending again %d message(s)", len(s.unacked))
				for _, u := range s.unacked {
					conn.send(evMsg, map[string][]byte{"data": u.data})
				}
			case evEnded:
				t.Log("Gino ended the chat")
				return
			case evLost:
				t.Log("Gino did not come back: the chat is over")
				return
			case evDropped:
				t.Log("this program's connection was lost")
				return
			}
		case <-deadline:
			conn.end()
			return
		}
	}
}
