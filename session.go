package main

import (
	"encoding/binary"
	"errors"
)

// session is one side of a chat, once it has started: it numbers what it sends and checks what it
// receives (docs/apps/chat.md section 3). It knows nothing about the screen or the network.
type session struct {
	keys     *chatKeys
	nextOut  [4]uint64 // per kind: the number of the next frame sent
	nextMsg  uint64    // the number of the next message expected from the other
	nextType uint64    // typing frames below this are old: ignored
	unacked  []sent    // my messages the other app has not acknowledged yet, in order
}

// sent is one of my messages, kept encrypted until the other app has it: after a lost connection it
// is sent again as it is (the same number; the other drops it if it already has it).
type sent struct {
	n    uint64
	data []byte
}

func newSession(k *chatKeys) *session { return &session{keys: k} }

// errGap: a message of the other is missing (removed on purpose by whoever controls the server:
// decided, the chat ends), or a frame did not open.
var errGap = errors.New("a message was lost")

func (s *session) seal(kind byte, plain []byte) (uint64, []byte, error) {
	n := s.nextOut[kind]
	data, err := sealFrame(s.keys.out, kind, n, plain)
	if err != nil {
		return 0, nil, err
	}
	s.nextOut[kind]++
	return n, data, nil
}

// message encrypts a message to send and keeps it until it is acknowledged; n is its number.
func (s *session) message(text string) (n uint64, data []byte, err error) {
	p, err := padText(text)
	if err != nil {
		return 0, nil, err
	}
	n, data, err = s.seal(kindMsg, p)
	if err == nil {
		s.unacked = append(s.unacked, sent{n, data})
	}
	return n, data, err
}

// acked forgets the messages the other app has, up to n.
func (s *session) acked(n uint64) {
	i := 0
	for i < len(s.unacked) && s.unacked[i].n <= n {
		i++
	}
	s.unacked = s.unacked[i:]
}

func (s *session) typing(on bool) ([]byte, error) {
	b := byte(0)
	if on {
		b = 1
	}
	_, data, err := s.seal(kindTyping, []byte{b})
	return data, err
}

// ack says to the other that its messages up to n have arrived.
func (s *session) ack(n uint64) ([]byte, error) {
	_, data, err := s.seal(kindAck, binary.BigEndian.AppendUint64(nil, n))
	return data, err
}

// openMessage reads a message of the other. dup: an old one sent again (after a reconnection), to
// ignore but acknowledge again (n is then the last one received). errGap: one is missing, or the frame
// was changed.
func (s *session) openMessage(data []byte) (text string, n uint64, dup bool, err error) {
	n, plain, err := openFrame(s.keys.in, kindMsg, data)
	if err != nil {
		return "", 0, false, errGap
	}
	switch {
	case n < s.nextMsg:
		return "", s.nextMsg - 1, true, nil
	case n > s.nextMsg:
		return "", n, false, errGap
	}
	text, err = unpadText(plain)
	if err != nil {
		return "", n, false, errGap
	}
	s.nextMsg++
	return text, n, false, nil
}

// openAck gives the number up to which the other has received my messages.
func (s *session) openAck(data []byte) (uint64, bool) {
	_, plain, err := openFrame(s.keys.in, kindAck, data)
	if err != nil || len(plain) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(plain), true
}

// openTyping says whether the other is typing; ok is false for a frame to ignore.
func (s *session) openTyping(data []byte) (on, ok bool) {
	n, plain, err := openFrame(s.keys.in, kindTyping, data)
	if err != nil || len(plain) != 1 || n < s.nextType {
		return false, false
	}
	s.nextType = n + 1
	return plain[0] == 1, true
}
