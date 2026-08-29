# kindled wire protocol — v1

A deliberately boring protocol: one TCP connection, ASCII control lines, and
raw JPEG payloads. No WebSockets, no protobuf, no VNC, no delta encoding.

```
Kindle 2024                         Pixel 10
  kindled  ──── TCP :45831 ────►  KindleServer
           ◄─── FRAME/JPEG ─────
           ──── TAP/SCROLL ────►
```

## Transport

* TCP over the Pixel's Wi-Fi hotspot. **The Kindle always dials out**, so
  nobody has to care which address DHCP handed the Kindle.
* Default port **45831**. The Kindle connects to the hotspot's default
  gateway (the Pixel) unless an address is configured explicitly.
* `TCP_NODELAY` on both ends. Frames are latency-sensitive and small.
* Exactly **one** live session per server. A new connection replaces the old
  one; the old one is closed with `BYE replaced`.

## Framing

Two kinds of messages share the stream:

1. **Control lines** — ASCII, space-separated tokens, terminated by `\n`.
   Maximum 256 bytes including the newline. Tokens are case-sensitive and
   uppercase. Unknown verbs MUST be ignored (forward compatibility).
2. **Binary payloads** — only ever introduced by a `FRAME` control line,
   which declares the exact byte count that follows the newline.

There is no other binary data on the wire, so a receiver is always either
"reading a line" or "reading exactly N payload bytes".

## Handshake

The Kindle speaks first:

```
→ HELLO kindled/1 <width> <height>
```

`<width> <height>` is the Kindle's panel size in pixels (e.g. `1072 1448`).

The Pixel replies:

```
← READY <width> <height> <fps> [capability ...]
```

`<width> <height>` is the size of the frames the Pixel will actually send,
and the coordinate space that `TAP`/`SCROLL` must use. It is normally an
echo of the Kindle's panel size, but the Kindle MUST accept a different one
and MUST NOT rescale gesture coordinates itself. `<fps>` is the Pixel's
target frame rate, informational only.

The Kindle MUST NOT send `TAP` or `SCROLL` before `READY`: until it lands,
the coordinate space is a guess.

### Capabilities

`READY` may carry any number of trailing capability tokens. Unknown tokens
MUST be ignored, so either end can gain features without a version bump.

| Capability | Meaning |
| --- | --- |
| `no-input` | The Pixel will not act on `TAP` or `SCROLL`. The Kindle SHOULD stop sending them. |

A capability is only ever defined for behaviour that *differs* from what the
protocol did before capabilities existed. That is why the mirror build of the
phone app advertises `no-input`, rather than control builds advertising
`input`: silence keeps meaning what it always meant, so a Kindle running new
firmware still drives a phone running an older app.

`no-input` exists because Play Protect blocks the sideloading of any app that
declares an accessibility service — the only way an ordinary Android app can
synthesise touches. The mirror build ships without one, so it can render the
phone's screen but can never act on a gesture.

If the server refuses the session it answers `BYE <reason>` and closes.

## Pixel → Kindle

| Message | Meaning |
| --- | --- |
| `FRAME <seq> <length>\n<length bytes>` | A JPEG image, `<seq>` is a monotonically increasing frame counter. |
| `PING` | Keepalive. The Kindle answers `PONG`. |
| `BYE <reason>` | Server is closing the session. |

## Kindle → Pixel

| Message | Meaning |
| --- | --- |
| `ACK <seq>` | Frame `<seq>` has finished rendering on e-ink. |
| `TAP <x> <y>` | Short touch at frame coordinates. |
| `SCROLL <dy>` | Vertical drag of `<dy>` pixels. |
| `PONG` | Reply to `PING`. |
| `BYE` | Clean shutdown. |

### Coordinates

Frame pixel space, origin top-left, as declared by `READY`. `x` grows to the
right, `y` grows downward.

`SCROLL <dy>` uses **content** direction: a finger dragged from y=900 up to
y=400 means the reader wants to move 500 px further down the page, and sends
`SCROLL 500`. A downward drag sends a negative value. The Pixel turns that
into a swipe in the opposite direction of the content movement.

## Flow control — never queue frames

The single most important rule. The Pixel keeps **only the newest frame** and
sends the next one only when both are true:

* the previous `FRAME` has been `ACK`ed, or 1000 ms have passed since it was
  written (the ack deadline), **and**
* the frame interval (default 333 ms) has elapsed.

If frames 101, 102 and 103 are produced while the Kindle is still painting
101, then 102 is **dropped** and 103 is sent next. Latency beats
completeness: nobody notices a missing intermediate frame on e-ink, everybody
notices a two-second lag between swipe and redraw.

Sequence numbers therefore have gaps. That is normal and not an error.

## Keepalive and failure

* The Pixel sends `PING` after 10 s of write inactivity.
* Either side treats 30 s without any inbound bytes as a dead link, closes
  the socket, and (on the Kindle) reconnects with capped exponential backoff.
* Any protocol violation — an over-long line, a `FRAME` length above 8 MiB,
  a truncated payload — closes the connection. It never panics either side.

## Example session

```
→ HELLO kindled/1 1072 1448
← READY 1072 1448 3
← FRAME 1 84213
← <84213 bytes of JPEG>
→ ACK 1
→ SCROLL 500
← FRAME 4 91002          (2 and 3 were dropped)
← <91002 bytes of JPEG>
→ ACK 4
→ TAP 412 830
← PING
→ PONG
```

## Why plain text and not protobuf

The control channel is ASCII on purpose. This is not nostalgia; protobuf
would lose on every axis that matters here.

**The bytes protobuf would save are not the bytes we send.** A frame is a
50–150 KB JPEG. Around it we send four control lines per frame — `FRAME`,
`ACK`, and the occasional `TAP`/`SCROLL` — about 60 bytes total. Encoding
those 60 bytes more cleverly is a rounding error against the payload, and
the payload is raw bytes either way. Protobuf would compress the part that
already costs nothing. (This is also why the payload is *not* wrapped in a
message: base64 or field-tagging a JPEG costs 33% of the only thing on the
wire that is actually large.)

**Protobuf does not solve framing, it sits on top of it.** Protobuf messages
are not self-delimiting, so we would still have to design and implement a
length-prefixed framing layer — and then put a schema compiler above it. Our
`FRAME <seq> <length>` line *is* that framing layer, in the one place a
length is needed.

**The build cost is the real cost.** Adopting protobuf means `protoc` plus
generated sources plus `google.golang.org/protobuf` in the Kindle daemon
(which today is a dependency-free static binary), and the Gradle protobuf
plugin plus another codegen step on the Android side. That is two build
systems and a code generator, bought for six message types that fit on one
screen.

**It has to be debuggable in a hotel room.** The failure modes we actually
expect are travel-shaped: the hotspot dropped, the Kindle got a new IP, the
daemon is talking to nothing. On a jailbroken Kindle with busybox and no
debugger, a complete diagnostic client is:

```
nc 192.168.43.1 45831
HELLO kindled/1 1072 1448
```

and the reply is readable on the screen. `strings` on a capture shows the
whole control flow. Logs paste straight into this document. With protobuf
every one of those steps needs a decoder first.

**Text desynchronises loudly.** A garbled text stream hits a non-verb or the
256-byte line cap and drops the session immediately. A garbled protobuf
stream parses varints out of noise and hands you a plausible-looking message
with a 900 MB length field. Bounded, obvious failure is worth more here than
compactness.

**The schema-evolution argument does not apply.** Protobuf's strongest card
is coordinating independently-deployed versions. Both ends of this link are
installed by one person, usually in the same sitting. The little forward
compatibility we do want — ignore unknown verbs, refuse unknown versions —
is the `HELLO kindled/1` token and one `default:` branch.

### When to revisit

Switch to a binary schema when the shape of the traffic changes, not before:
many more message types, nested or repeated structures (per-tile metadata for
delta encoding, for example), streaming raw motion events at touch-panel
rates instead of whole gestures, or a third implementation maintained by
somebody else. If that day comes, the payload framing stays exactly as it is
and only the control lines change.

## Versioning

The version token lives in `HELLO` (`kindled/1`). A server that does not
understand the version answers `BYE unsupported-version`. New verbs may be
added in either direction at any time; both ends ignore what they do not
recognise.
