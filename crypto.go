package main

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"unicode/utf8"

	"golang.org/x/crypto/chacha20poly1305"
)

// The crypto of Trimlet Chat (docs/apps/chat.md section 3): standard pieces only, X25519,
// HKDF-SHA256, HMAC-SHA256, XChaCha20-Poly1305. "v1" names this version: the labels below must never
// change for v1.
//
//   - Gino's code is G (his public key for this chat); Mario makes M. dh = X25519(g, M) = X25519(m, G).
//   - From dh, with HKDF-SHA256 and salt "trimletchat-v1" ‖ G ‖ M: one key per direction and one to
//     confirm. Whoever joins proves to have the code with HMAC(confirm, "… joiner"); Gino's app answers
//     HMAC(confirm, "… starter"). The server never has G, so it can make neither proof.
//   - Every frame is encrypted with the key of its direction; its nonce is the kind and the frame's
//     number (each direction has its own key, so a nonce is never used twice), and both are also
//     associated data: a frame changed, repeated or out of order does not open. A message is always
//     4 KB before encrypting (2 bytes of length, the text, zeros), so the server does not learn its
//     length.

const version = "trimletchat-v1"

// maxMessage is the longest message, in bytes (decided: 4 KB, as Seal, so that no file fits).
const maxMessage = 4096

// The kinds of encrypted frames; each direction numbers each kind on its own, from 0.
const (
	kindMsg    byte = 1 // a message
	kindTyping byte = 2 // the other is typing (1) or stopped (0); gaps allowed
	kindAck    byte = 3 // "I have received your messages up to n"
)

// chatKeys are the keys of one chat, seen from one side.
type chatKeys struct {
	out, in, confirm []byte
}

var errCannotOpen = errors.New("cannot be opened")

// deriveKeys makes the keys of the chat from this side's private key and the other side's public
// key. gino and mario are G and M; starter says whether this side is Gino's.
func deriveKeys(priv *ecdh.PrivateKey, peerPub, gino, mario []byte, starter bool) (*chatKeys, error) {
	peer, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, err
	}
	dh, err := priv.ECDH(peer) // an all-zero result (a point of small order) is refused here
	if err != nil {
		return nil, err
	}
	salt := append(append([]byte(version), gino...), mario...)
	prk, err := hkdf.Extract(sha256.New, dh, salt)
	if err != nil {
		return nil, err
	}
	key := func(label string) ([]byte, error) { return hkdf.Expand(sha256.New, prk, version+" "+label, 32) }
	gm, err := key("gino to mario")
	if err != nil {
		return nil, err
	}
	mg, err := key("mario to gino")
	if err != nil {
		return nil, err
	}
	c, err := key("confirm")
	if err != nil {
		return nil, err
	}
	if starter {
		return &chatKeys{out: gm, in: mg, confirm: c}, nil
	}
	return &chatKeys{out: mg, in: gm, confirm: c}, nil
}

// proof is what each side shows to prove it has the chat's keys: role is "joiner" or "starter".
func (k *chatKeys) proof(role string) []byte {
	h := hmac.New(sha256.New, k.confirm)
	h.Write([]byte(version + " " + role))
	return h.Sum(nil)
}

func (k *chatKeys) checkProof(role string, p []byte) bool {
	return hmac.Equal(k.proof(role), p)
}

func frameNonce(kind byte, n uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	nonce[0] = kind
	binary.BigEndian.PutUint64(nonce[16:], n)
	return nonce
}

func frameAD(kind byte, n uint64) []byte {
	return binary.BigEndian.AppendUint64(append([]byte(version), kind), n)
}

// sealFrame encrypts plain as frame n of kind: n (8 bytes) ‖ ciphertext.
func sealFrame(key []byte, kind byte, n uint64, plain []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	out := binary.BigEndian.AppendUint64(nil, n)
	return aead.Seal(out, frameNonce(kind, n), plain, frameAD(kind, n)), nil
}

// openFrame decrypts a frame of kind and gives its number.
func openFrame(key []byte, kind byte, data []byte) (uint64, []byte, error) {
	if len(data) < 8+chacha20poly1305.Overhead {
		return 0, nil, errCannotOpen
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint64(data)
	plain, err := aead.Open(nil, frameNonce(kind, n), data[8:], frameAD(kind, n))
	if err != nil {
		return 0, nil, errCannotOpen
	}
	return n, plain, nil
}

// padText is the text of a message, always 2 + 4,096 bytes.
func padText(text string) ([]byte, error) {
	if len(text) > maxMessage {
		return nil, errors.New("longer than 4 KB")
	}
	out := make([]byte, 2+maxMessage)
	binary.BigEndian.PutUint16(out, uint16(len(text)))
	copy(out[2:], text)
	return out, nil
}

func unpadText(p []byte) (string, error) {
	if len(p) != 2+maxMessage {
		return "", errCannotOpen
	}
	n := int(binary.BigEndian.Uint16(p))
	if n > maxMessage || !utf8.Valid(p[2:2+n]) {
		return "", errCannotOpen
	}
	return string(p[2 : 2+n]), nil
}

// newJoinKey makes Mario's key pair for one chat.
func newJoinKey() (*ecdh.PrivateKey, error) { return ecdh.X25519().GenerateKey(rand.Reader) }
