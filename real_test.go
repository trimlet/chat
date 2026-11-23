package main

import (
	"os"
	"testing"
	"time"
)

// TestRealServer runs against a real trimletchat-server, only when TRIMLETCHAT_SERVER is set, e.g.
// TRIMLETCHAT_SERVER=http://127.0.0.1:8691 go test -run Real ./... (it makes a real access key: about
// 2 minutes, unless the server runs with a smaller TRIMLETCHAT_KEY_WORK). It checks that the app and
// the server agree on the proof of work and on the frames.
func TestRealServer(t *testing.T) {
	if os.Getenv("TRIMLETCHAT_SERVER") == "" {
		t.Skip("TRIMLETCHAT_SERVER not set")
	}
	srv := newHTTPServer()
	ch, err := srv.challenge(workKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := srv.newKey(ch.Challenge, powSolve(ch.Challenge, "key", ch.Difficulty))
	if err != nil {
		t.Fatal(err)
	}

	start := func() (chatConn, waiting) {
		t.Helper()
		_, priv, _ := newChatKey()
		look := lookup(priv.PublicKey().Bytes())
		ch, err := srv.challenge(workCode)
		if err != nil {
			t.Fatal(err)
		}
		conn, w, err := srv.start(key, look, ch.Challenge, powSolve(ch.Challenge, b64(look), ch.Difficulty))
		if err != nil {
			t.Fatal(err)
		}
		return conn, w
	}
	first, w := start()
	if w.SecondsLeft <= 0 || len(w.Token) != 32 {
		t.Fatalf("waiting: %+v", w)
	}
	// A new chat of the same key ends the first one.
	second, _ := start()
	if ev := <-first.events(); ev.kind != evReplaced {
		t.Fatalf("the first chat got %q", ev.kind)
	}
	second.end()

	// An unknown key is refused.
	_, priv, _ := newChatKey()
	look := lookup(priv.PublicKey().Bytes())
	ch, _ = srv.challenge(workCode)
	if _, _, err := srv.start(make([]byte, 32), look, ch.Challenge, powSolve(ch.Challenge, b64(look), ch.Difficulty)); err != errUnknownKey {
		t.Fatalf("an unknown key: %v", err)
	}
}

// TestRealServerChat: Gino and Mario through a real trimletchat-server (TRIMLETCHAT_SERVER set; the
// server with a small TRIMLETCHAT_KEY_WORK, or it takes 2 minutes): join, the proofs, a message each
// way with its acknowledgement, the end.
func TestRealServerChat(t *testing.T) {
	if os.Getenv("TRIMLETCHAT_SERVER") == "" {
		t.Skip("TRIMLETCHAT_SERVER not set")
	}
	srv := newHTTPServer()
	ch, _ := srv.challenge(workKey)
	key, err := srv.newKey(ch.Challenge, powSolve(ch.Challenge, "key", ch.Difficulty))
	if err != nil {
		t.Fatal(err)
	}
	code, g, _ := newChatKey()
	G := g.PublicKey().Bytes()
	look := lookup(G)
	ch, _ = srv.challenge(workCode)
	gconn, _, err := srv.start(key, look, ch.Challenge, powSolve(ch.Challenge, b64(look), ch.Difficulty))
	if err != nil {
		t.Fatal(err)
	}

	type joined struct {
		conn chatConn
		k    *chatKeys
		err  error
	}
	done := make(chan joined, 1)
	go func() {
		pub, _ := decode32(code, keyLen)
		m, _ := newJoinKey()
		M := m.PublicKey().Bytes()
		k, _ := deriveKeys(m, pub, pub, M, false)
		conn, ok, err := srv.join(lookup(pub), M, k.proof("joiner"))
		if err == nil && !k.checkProof("starter", ok.proof) {
			err = errInvalidCode
		}
		done <- joined{conn, k, err}
	}()
	ev := <-gconn.events()
	if ev.kind != evJoined {
		t.Fatalf("Gino got %q", ev.kind)
	}
	gk, err := deriveKeys(g, ev.pub, G, ev.pub, true)
	if err != nil || !gk.checkProof("joiner", ev.proof) {
		t.Fatal("Mario's proof")
	}
	gconn.send("confirm", map[string][]byte{"proof": gk.proof("starter")})
	j := <-done
	if j.err != nil {
		t.Fatal(j.err)
	}
	gs, ms := newSession(gk), newSession(j.k)

	_, data, _ := gs.message("ciao Mario")
	gconn.send(evMsg, map[string][]byte{"data": data})
	ev = <-j.conn.events()
	text, n, _, err := ms.openMessage(ev.data)
	if err != nil || text != "ciao Mario" {
		t.Fatalf("%q %v", text, err)
	}
	ack, _ := ms.ack(n)
	j.conn.send(evAck, map[string][]byte{"data": ack})
	if ev = <-gconn.events(); ev.kind != evAck {
		t.Fatalf("Gino got %q", ev.kind)
	}
	_, data, _ = ms.message("ciao Gino")
	j.conn.send(evMsg, map[string][]byte{"data": data})
	ev = <-gconn.events()
	if text, _, _, err := gs.openMessage(ev.data); err != nil || text != "ciao Gino" {
		t.Fatalf("%q %v", text, err)
	}
	j.conn.end()
	if ev = <-gconn.events(); ev.kind != evEnded {
		t.Fatalf("Gino got %q", ev.kind)
	}
}

// TestRealServerResume: a lost connection comes back with its token (block 3), against a real server.
func TestRealServerResume(t *testing.T) {
	if os.Getenv("TRIMLETCHAT_SERVER") == "" {
		t.Skip("TRIMLETCHAT_SERVER not set")
	}
	srv := newHTTPServer()
	ch, _ := srv.challenge(workKey)
	key, err := srv.newKey(ch.Challenge, powSolve(ch.Challenge, "key", ch.Difficulty))
	if err != nil {
		t.Fatal(err)
	}
	_, g, _ := newChatKey()
	G := g.PublicKey().Bytes()
	look := lookup(G)
	ch, _ = srv.challenge(workCode)
	gconn, w, err := srv.start(key, look, ch.Challenge, powSolve(ch.Challenge, b64(look), ch.Difficulty))
	if err != nil {
		t.Fatal(err)
	}
	// Gino's line drops while he waits, and comes back.
	gconn.(*wsConn).c.CloseNow()
	if ev := <-gconn.events(); ev.kind != evDropped {
		t.Fatalf("got %q", ev.kind)
	}
	time.Sleep(200 * time.Millisecond)
	gconn, away, err := srv.resume(look, w.Token)
	if err != nil || away {
		t.Fatalf("resume: %v %v", away, err)
	}
	// A wrong token is refused.
	if _, _, err := srv.resume(look, make([]byte, 32)); err != errInvalidCode {
		t.Fatalf("a wrong token: %v", err)
	}
	gconn.end()
}
