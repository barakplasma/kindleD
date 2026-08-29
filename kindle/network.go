package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	protocolVersion = "kindled/1"
	// maxLine bounds a control line, maxFrame bounds a JPEG payload.
	maxLine  = 256
	maxFrame = 8 << 20
)

// NetConfig describes how to reach the Pixel.
type NetConfig struct {
	// Addr is an explicit host:port. Empty means "ask the default route",
	// i.e. the hotspot gateway, which is the Pixel.
	Addr string
	Port int
	// Width and Height are the Kindle panel size announced in HELLO.
	Width, Height int
	// MinBackoff/MaxBackoff bound the reconnect delay.
	MinBackoff, MaxBackoff time.Duration
	// ReadTimeout is how long a session may go without inbound bytes.
	ReadTimeout time.Duration
	// PingEvery bounds write inactivity from the Kindle.
	PingEvery time.Duration
	// OnGeometry is called with the frame size announced in READY, so the
	// touch mapper reports gestures in the space the Pixel expects.
	OnGeometry func(w, h int)
	Logf       func(format string, args ...interface{})
}

// Gesture is a touch event on its way to the Pixel.
type Gesture struct {
	Kind string // "TAP" or "SCROLL"
	X, Y int    // TAP only
	DY   int    // SCROLL only
}

// Line renders the gesture as a protocol control line (without newline).
func (g Gesture) Line() string {
	switch g.Kind {
	case "TAP":
		return fmt.Sprintf("TAP %d %d", g.X, g.Y)
	case "SCROLL":
		return fmt.Sprintf("SCROLL %d", g.DY)
	default:
		return ""
	}
}

// Client is the Kindle side of the link: dial the Pixel, feed frames to the
// renderer, push gestures back. Losing the Pixel is normal, not exceptional.
type Client struct {
	cfg      NetConfig
	renderer *Renderer
	gestures <-chan Gesture

	// mu guards everything learned from the server's READY line, which the
	// read loop writes and the gesture pump reads.
	mu sync.Mutex
	// frameW, frameH are the coordinate space gestures must use.
	frameW, frameH int
	// ready is false until READY arrives. Gestures are held back until
	// then: before READY the coordinate space is a guess.
	ready bool
	// mirrorOnly records that the Pixel advertised "no-input" in READY,
	// meaning it will not act on gestures. Absence of the token means the
	// Pixel takes input, which is what every build before the capability
	// existed did -- so an older phone app keeps working unchanged.
	mirrorOnly bool
}

// setReady records what the server told us in READY.
func (c *Client) setReady(w, h int, mirrorOnly bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frameW, c.frameH = w, h
	c.ready = true
	c.mirrorOnly = mirrorOnly
}

// clearReady forgets the previous session's terms.
func (c *Client) clearReady() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
	c.mirrorOnly = false
}

// inputAccepted reports whether sending a gesture is worth the bytes.
func (c *Client) inputAccepted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready && !c.mirrorOnly
}

// NewClient wires the network to the renderer and the touchscreen.
func NewClient(cfg NetConfig, r *Renderer, gestures <-chan Gesture) *Client {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...interface{}) {}
	}
	if cfg.MinBackoff == 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 15 * time.Second
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.PingEvery == 0 {
		cfg.PingEvery = 10 * time.Second
	}
	return &Client{cfg: cfg, renderer: r, gestures: gestures, frameW: cfg.Width, frameH: cfg.Height}
}

// Run reconnects forever: wait for the hotspot, connect, run a session,
// reconnect on failure. It returns only when ctx is cancelled.
func (c *Client) Run(ctx context.Context) {
	backoff := c.cfg.MinBackoff
	for ctx.Err() == nil {
		addr, err := c.resolve()
		if err != nil {
			c.cfg.Logf("waiting for hotspot: %v", err)
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}

		c.cfg.Logf("connecting to %s", addr)
		start := time.Now()
		err = c.session(ctx, addr)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			c.cfg.Logf("session ended: %v", err)
		default:
			c.cfg.Logf("session closed by pixel")
		}

		// A session that actually did some work resets the backoff, so a
		// hotspot that drops for a second does not cost 15.
		if time.Since(start) > 5*time.Second {
			backoff = c.cfg.MinBackoff
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff *= 2
		if backoff > c.cfg.MaxBackoff {
			backoff = c.cfg.MaxBackoff
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// resolve picks the address to dial: the configured one, or the default
// gateway of whatever network we are on, which on a Pixel hotspot is the
// Pixel itself.
func (c *Client) resolve() (string, error) {
	if c.cfg.Addr != "" {
		if _, _, err := net.SplitHostPort(c.cfg.Addr); err == nil {
			return c.cfg.Addr, nil
		}
		return net.JoinHostPort(c.cfg.Addr, strconv.Itoa(c.cfg.Port)), nil
	}
	gw, err := DefaultGateway()
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(gw, strconv.Itoa(c.cfg.Port)), nil
}

// DefaultGateway reads the IPv4 default route from /proc/net/route. On the
// Pixel's hotspot that is the Pixel.
func DefaultGateway() (string, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", err
	}
	defer f.Close()
	return parseDefaultGateway(f)
}

// parseDefaultGateway parses the /proc/net/route table. Addresses there are
// little-endian hex.
func parseDefaultGateway(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[0] == "Iface" {
			continue
		}
		if fields[1] != "00000000" { // not the default route
			continue
		}
		v, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil {
			continue
		}
		ip := net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
		if ip.Equal(net.IPv4zero) {
			continue
		}
		return ip.String(), nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("no default gateway")
}

// FrameGeometry returns the coordinate space the Pixel is streaming in.
func (c *Client) FrameGeometry() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frameW, c.frameH
}

func (c *Client) session(ctx context.Context, addr string) error {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetNoDelay(true)
	}
	c.clearReady()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close() // unblock the reader
	}()

	out := make(chan string, 32)
	writerDone := make(chan error, 1)
	go func() { writerDone <- c.writeLoop(ctx, conn, out) }()

	br := bufio.NewReaderSize(conn, 64<<10)
	hello := fmt.Sprintf("HELLO %s %d %d", protocolVersion, c.cfg.Width, c.cfg.Height)
	if err := sendLine(out, hello); err != nil {
		return err
	}

	err = c.readLoop(ctx, conn, br, out)
	cancel()
	<-writerDone
	return err
}

// sendLine queues a control line, dropping it if the writer is wedged. A
// stuck socket must never block the renderer or the touchscreen.
func sendLine(out chan<- string, line string) error {
	select {
	case out <- line:
		return nil
	default:
		return errors.New("write queue full")
	}
}

func (c *Client) writeLoop(ctx context.Context, conn net.Conn, out <-chan string) error {
	ping := time.NewTicker(c.cfg.PingEvery)
	defer ping.Stop()
	for {
		var line string
		select {
		case <-ctx.Done():
			return nil
		case line = <-out:
		case <-ping.C:
			line = "PING"
		}
		if line == "" {
			continue
		}
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(conn, line+"\n"); err != nil {
			return err
		}
	}
}

func (c *Client) readLoop(ctx context.Context, conn net.Conn, br *bufio.Reader, out chan<- string) error {
	// Gestures and render acks are pumped into the same write queue.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case seq := <-c.renderer.Rendered:
				sendLine(out, fmt.Sprintf("ACK %d", seq))
			case g := <-c.gestures:
				if !c.inputAccepted() {
					// Either the phone is mirror-only, or READY has not
					// arrived and the coordinate space is still a guess.
					continue
				}
				if line := g.Line(); line != "" {
					c.cfg.Logf("gesture %s", line)
					sendLine(out, line)
				}
			}
		}
	}()

	for {
		conn.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout))
		line, err := readLine(br)
		if err != nil {
			return err
		}
		verb, rest := splitVerb(line)
		switch verb {
		case "READY":
			w, h, ok := parseTwoInts(rest)
			if !ok {
				return fmt.Errorf("bad READY: %q", line)
			}
			mirrorOnly := hasCapability(rest, "no-input")
			c.setReady(w, h, mirrorOnly)
			if mirrorOnly {
				c.cfg.Logf("pixel is a mirror-only build: touches will not be sent")
			}
			if c.cfg.OnGeometry != nil {
				c.cfg.OnGeometry(w, h)
			}
			c.cfg.Logf("pixel ready, streaming %dx%d", w, h)
		case "FRAME":
			frame, err := readFrame(br, rest)
			if err != nil {
				return err
			}
			c.renderer.Submit(frame)
		case "PING":
			sendLine(out, "PONG")
		case "PONG":
			// keepalive answered, nothing to do
		case "BYE":
			return fmt.Errorf("pixel said BYE %s", rest)
		case "":
			// blank line, ignore
		default:
			c.cfg.Logf("ignoring unknown verb %q", verb)
		}
	}
}

// readLine reads one control line, refusing anything absurdly long so a
// desynchronised stream fails fast instead of eating memory.
func readLine(br *bufio.Reader) (string, error) {
	// ReadSlice, not ReadString: a stream without newlines then fails
	// against the fixed buffer instead of allocating without bound.
	line, err := br.ReadSlice('\n')
	if err != nil {
		if err == bufio.ErrBufferFull {
			return "", errors.New("control line too long")
		}
		return "", err
	}
	if len(line) > maxLine {
		return "", fmt.Errorf("control line too long (%d bytes)", len(line))
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// readFrame reads the payload announced by a FRAME line.
func readFrame(br *bufio.Reader, rest string) (Frame, error) {
	fields := strings.Fields(rest)
	if len(fields) != 2 {
		return Frame{}, fmt.Errorf("bad FRAME header %q", rest)
	}
	seq, err1 := strconv.ParseUint(fields[0], 10, 64)
	length, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil || length < 0 || length > maxFrame {
		return Frame{}, fmt.Errorf("bad FRAME header %q", rest)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(br, buf); err != nil {
		return Frame{}, fmt.Errorf("short frame %d: %w", seq, err)
	}
	return Frame{Seq: seq, JPEG: buf}, nil
}

func splitVerb(line string) (verb, rest string) {
	line = strings.TrimSpace(line)
	if i := strings.IndexByte(line, ' '); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

// hasCapability reports whether a READY line's trailing tokens include name.
// Capabilities start after the three fixed fields (width, height, fps), and
// unknown ones are ignored, so either end can gain features independently.
//
// Capabilities are only ever added for behaviour that differs from what the
// protocol did before they existed, which is why the mirror build advertises
// "no-input" rather than control builds advertising "input": a phone running
// a version older than this still gets its gestures.
func hasCapability(rest, name string) bool {
	fields := strings.Fields(rest)
	if len(fields) <= 3 {
		return false
	}
	for _, f := range fields[3:] {
		if f == name {
			return true
		}
	}
	return false
}

func parseTwoInts(s string) (int, int, bool) {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return 0, 0, false
	}
	a, err1 := strconv.Atoi(fields[0])
	b, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return a, b, true
}
