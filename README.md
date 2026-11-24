# Trimlet Chat

A written phone call, in a small window driven only by the keyboard: one makes a code and sends it to
someone on any channel; the other has 5 minutes to join; they write to each other, text only; when
one hangs up, nothing is kept.

What it does: start a chat, join it with the code, write (several lines, exact
text), a lost connection has 30 seconds to come back, the chat ends after 15 minutes of silence or 3
hours, alerts without the message text when the window is behind.

- No accounts, no nickname, no contacts, no history.
- The server never receives the code, only a hash of it, so it cannot read the chat nor pose as
  either side. It keeps the chats in memory only, while they last.

The crypto uses standard pieces (X25519, HKDF, HMAC, XChaCha20-Poly1305) and has not been reviewed by
an expert yet.

## Download

The app for **macOS** (Mac with Apple chip: M1 and later) and **Windows** is on the
[Releases](https://github.com/trimlet/chat/releases) page. It is a beta (0.x), and not signed yet: the first
time, macOS asks to confirm (right click on the app → Open), Windows shows SmartScreen (More info →
Run anyway).

## Build

Written in Go with [Fyne](https://fyne.io). To build it, with Go installed (and on a Mac the Xcode
command line tools):

```
go run fyne.io/tools/cmd/fyne@v1.7.3 package --release
```

or `go run .` to try it. The console window comes from [trimlet/gui](https://github.com/trimlet/gui),
which `go` fetches by itself.

## How it is made

The code is written with Claude Opus (Anthropic), under the direction and review of a developer with
almost 30 years of experience. Every feature starts from a real, repeated need, and most ideas are
turned down: the goal is to stay small.

## License

GPL-3.0, see [LICENSE](LICENSE).
