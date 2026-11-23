package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
)

// The code of a chat (docs/apps/chat.md section 3): Gino's X25519 public key for this chat only,
// 32 bytes, written in Crockford base32 (digits and capital letters without I, L, O, U) in groups of
// 4, as in Trimlet Seal. When read back, case, dashes and spaces are ignored, and I, L are read as 1,
// O as 0. The server never receives the code, only its lookup.

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	keyLen  = 32 // bytes of an X25519 key
	codeLen = 52 // characters of the code: 256 bits, 5 per character
)

// encode32 writes b in Crockford base32, 5 bits per character, the last one padded with zeros.
func encode32(b []byte) string {
	var out strings.Builder
	var acc uint
	bits := 0
	for _, c := range b {
		acc = acc<<8 | uint(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out.WriteByte(crockford[(acc>>uint(bits))&31])
		}
	}
	if bits > 0 {
		out.WriteByte(crockford[(acc<<uint(5-bits))&31])
	}
	return out.String()
}

var errBadChar = errors.New("a character that is not in the code")

// normalize turns what was typed or pasted into the plain characters of a code.
func normalize(s string) (string, error) {
	var out strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r':
			continue
		case r == 'I' || r == 'L':
			r = '1'
		case r == 'O':
			r = '0'
		}
		if !strings.ContainsRune(crockford, r) {
			return "", errBadChar
		}
		out.WriteRune(r)
	}
	return out.String(), nil
}

// decode32 reads n bytes back from their Crockford base32 characters.
func decode32(s string, n int) ([]byte, error) {
	s, err := normalize(s)
	if err != nil {
		return nil, err
	}
	if len(s) != (n*8+4)/5 {
		return nil, errors.New("wrong length")
	}
	out := make([]byte, 0, n)
	var acc uint
	bits := 0
	for _, r := range s {
		acc = acc<<5 | uint(strings.IndexRune(crockford, r))
		bits += 5
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>uint(bits)))
		}
	}
	if acc&(1<<uint(bits)-1) != 0 {
		return nil, errors.New("not a code") // the padding bits must be zero
	}
	return out[:n], nil
}

// group puts a dash every 4 characters: QXJM-8VR2-…
func group(s string) string {
	var out strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			out.WriteByte('-')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// lookupLabel is the domain of the lookup: the only thing the server learns of a code.
const lookupLabel = "trimletchat-v1 lookup"

// lookup is SHA-256("trimletchat-v1 lookup" ‖ code): enough for the server to find the chat, not
// to compute its keys (docs/apps/chat.md section 1).
func lookup(pub []byte) []byte {
	h := sha256.Sum256(append([]byte(lookupLabel), pub...))
	return h[:]
}

// newChatKey makes Gino's key pair for one chat; the code is its public key. The private key lives
// only in memory.
func newChatKey() (code string, priv *ecdh.PrivateKey, err error) {
	priv, err = ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	return group(encode32(priv.PublicKey().Bytes())), priv, nil
}
