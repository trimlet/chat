package main

import (
	"bytes"
	"testing"
)

// twoSides makes the keys of one chat as Gino's and Mario's apps do.
func twoSides(t *testing.T) (gino, mario *chatKeys) {
	t.Helper()
	g, _ := newJoinKey()
	m, _ := newJoinKey()
	G, M := g.PublicKey().Bytes(), m.PublicKey().Bytes()
	gino, err := deriveKeys(g, M, G, M, true)
	if err != nil {
		t.Fatal(err)
	}
	mario, err = deriveKeys(m, G, G, M, false)
	if err != nil {
		t.Fatal(err)
	}
	return gino, mario
}

func TestBothSidesMakeTheSameKeys(t *testing.T) {
	g, m := twoSides(t)
	if !bytes.Equal(g.out, m.in) || !bytes.Equal(g.in, m.out) || bytes.Equal(g.out, g.in) {
		t.Fatal("one key per direction, the same on both sides")
	}
	if !g.checkProof("joiner", m.proof("joiner")) || !m.checkProof("starter", g.proof("starter")) {
		t.Fatal("the proofs must match")
	}
	if g.checkProof("joiner", g.proof("starter")) {
		t.Fatal("a starter's proof must not pass as a joiner's")
	}
	// Someone with another key (the server, which never has the code) cannot make the proof.
	_, other := twoSides(t)
	if g.checkProof("joiner", other.proof("joiner")) {
		t.Fatal("a proof of another chat must not pass")
	}
}

func TestAPointOfSmallOrderIsRefused(t *testing.T) {
	g, _ := newJoinKey()
	G := g.PublicKey().Bytes()
	if _, err := deriveKeys(g, make([]byte, 32), G, make([]byte, 32), true); err == nil {
		t.Fatal("an all-zero public key must be refused")
	}
}

func TestFrames(t *testing.T) {
	g, m := twoSides(t)
	gs, ms := newSession(g), newSession(m)
	_, a, _ := gs.message("first")
	_, b, _ := gs.message("second")
	_, c, _ := gs.message("third")
	if len(a) != 8+2+maxMessage+16 || len(a) != len(c) {
		t.Fatalf("every message has the same size: %d", len(a))
	}
	if text, n, dup, err := ms.openMessage(a); text != "first" || n != 0 || dup || err != nil {
		t.Fatalf("%q %d %v %v", text, n, dup, err)
	}
	if _, _, dup, _ := ms.openMessage(a); !dup {
		t.Fatal("a message sent again is a duplicate")
	}
	if _, _, _, err := ms.openMessage(c); err != errGap {
		t.Fatal("the third before the second is a gap")
	}
	changed := append([]byte(nil), b...)
	changed[20] ^= 1
	if _, _, _, err := ms.openMessage(changed); err != errGap {
		t.Fatal("a changed frame must not open")
	}
	if text, _, _, err := ms.openMessage(b); text != "second" || err != nil {
		t.Fatal(err)
	}
	// Gino's own message sent back to him does not open (one key per direction).
	if _, _, _, err := gs.openMessage(b); err != errGap {
		t.Fatal("a message sent back must not open")
	}
	// The number cannot be changed: it is in the nonce and the associated data.
	renumbered := append([]byte(nil), c...)
	renumbered[7] = 1
	if _, _, _, err := ms.openMessage(renumbered); err != errGap {
		t.Fatal("a renumbered frame must not open")
	}

	ack, _ := ms.ack(1)
	if n, ok := gs.openAck(ack); !ok || n != 1 {
		t.Fatal("the acknowledgement")
	}
	if _, ok := gs.openAck(a); ok {
		t.Fatal("a message is not an acknowledgement")
	}
	on, _ := gs.typing(true)
	off, _ := gs.typing(false)
	if v, ok := ms.openTyping(off); !ok || v {
		t.Fatal("typing off")
	}
	if _, ok := ms.openTyping(on); ok {
		t.Fatal("an older typing frame is ignored")
	}

	if _, _, err := gs.message(string(make([]byte, maxMessage+1))); err == nil {
		t.Fatal("over 4 KB must be refused")
	}
}
