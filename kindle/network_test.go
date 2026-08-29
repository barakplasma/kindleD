package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseDefaultGateway(t *testing.T) {
	// Real /proc/net/route from a device on a phone hotspot: gateway
	// 192.168.43.1 encoded little-endian as 012BA8C0.
	const table = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
wlan0	00000000	012BA8C0	0003	0	0	0	00000000	0	0	0
wlan0	002BA8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`
	gw, err := parseDefaultGateway(strings.NewReader(table))
	if err != nil {
		t.Fatal(err)
	}
	if gw != "192.168.43.1" {
		t.Fatalf("gateway = %s, want 192.168.43.1", gw)
	}
}

func TestParseDefaultGatewayNone(t *testing.T) {
	const table = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
wlan0	002BA8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`
	if _, err := parseDefaultGateway(strings.NewReader(table)); err == nil {
		t.Fatal("expected an error when the hotspot is not up yet")
	}
}

func TestReadFrame(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("hello-jpeg"))
	f, err := readFrame(br, "42 10")
	if err != nil {
		t.Fatal(err)
	}
	if f.Seq != 42 || string(f.JPEG) != "hello-jpeg" {
		t.Fatalf("got seq=%d payload=%q", f.Seq, f.JPEG)
	}

	for _, bad := range []string{"42", "42 -1", "x 10", fmt.Sprintf("1 %d", maxFrame+1)} {
		br := bufio.NewReader(strings.NewReader("hello-jpeg"))
		if _, err := readFrame(br, bad); err == nil {
			t.Fatalf("readFrame(%q) should have failed", bad)
		}
	}
}

func TestReadLineRejectsUnterminatedFlood(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader(strings.Repeat("A", 40<<10)), 4096)
	if _, err := readLine(br); err == nil {
		t.Fatal("a line without a newline must be refused")
	}
}

func TestGestureLine(t *testing.T) {
	if got := (Gesture{Kind: "TAP", X: 412, Y: 830}).Line(); got != "TAP 412 830" {
		t.Fatalf("tap line = %q", got)
	}
	if got := (Gesture{Kind: "SCROLL", DY: 500}).Line(); got != "SCROLL 500" {
		t.Fatalf("scroll line = %q", got)
	}
	if got := (Gesture{Kind: "WAT"}).Line(); got != "" {
		t.Fatalf("unknown gesture should render empty, got %q", got)
	}
}

// testClient spins up a loopback listener plus a wired-up Client.
type testClient struct {
	ln       net.Listener
	renderer *Renderer
	backend  *fakeBackend
	gestures chan Gesture
	client   *Client
	cancel   context.CancelFunc
}

func newTestClient(t *testing.T) *testClient {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	be := &fakeBackend{}
	r := NewRenderer(be, RendererConfig{Dir: t.TempDir()})
	go r.Run()
	gestures := make(chan Gesture, 4)
	c := NewClient(NetConfig{
		Addr:        ln.Addr().String(),
		Width:       1072,
		Height:      1448,
		MinBackoff:  10 * time.Millisecond,
		MaxBackoff:  20 * time.Millisecond,
		ReadTimeout: 2 * time.Second,
		PingEvery:   time.Hour,
	}, r, gestures)

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	t.Cleanup(func() {
		cancel()
		r.Close()
		ln.Close()
	})
	return &testClient{ln: ln, renderer: r, backend: be, gestures: gestures, client: c, cancel: cancel}
}

func (tc *testClient) accept(t *testing.T) (net.Conn, *bufio.Reader) {
	t.Helper()
	tc.ln.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	conn, err := tc.ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewReader(conn)
}

func expectLine(t *testing.T, br *bufio.Reader, want string) {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read %q: %v", want, err)
	}
	if got := strings.TrimSpace(line); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSessionFrameAckAndGestures(t *testing.T) {
	tc := newTestClient(t)
	conn, br := tc.accept(t)

	expectLine(t, br, "HELLO kindled/1 1072 1448")
	fmt.Fprintf(conn, "READY 1072 1448 3\n")

	jpeg := []byte("\xff\xd8pretend-jpeg\xff\xd9")
	fmt.Fprintf(conn, "FRAME 1 %d\n", len(jpeg))
	conn.Write(jpeg)
	expectLine(t, br, "ACK 1")

	if calls := tc.backend.snapshot(); len(calls) != 1 || string(calls[0].data) != string(jpeg) {
		t.Fatalf("renderer did not get the frame: %+v", calls)
	}
	if w, h := tc.client.FrameGeometry(); w != 1072 || h != 1448 {
		t.Fatalf("frame geometry = %dx%d", w, h)
	}

	// Keepalive.
	fmt.Fprintf(conn, "PING\n")
	expectLine(t, br, "PONG")

	// Unknown verbs are ignored, not fatal.
	fmt.Fprintf(conn, "FUTURE 1 2 3\n")

	tc.gestures <- Gesture{Kind: "SCROLL", DY: 500}
	expectLine(t, br, "SCROLL 500")
	tc.gestures <- Gesture{Kind: "TAP", X: 412, Y: 830}
	expectLine(t, br, "TAP 412 830")
}

func TestClientReconnects(t *testing.T) {
	tc := newTestClient(t)

	conn, br := tc.accept(t)
	expectLine(t, br, "HELLO kindled/1 1072 1448")
	fmt.Fprintf(conn, "READY 1072 1448 3\n")
	conn.Close() // the hotspot blinks

	conn2, br2 := tc.accept(t)
	expectLine(t, br2, "HELLO kindled/1 1072 1448")
	fmt.Fprintf(conn2, "READY 800 600 3\n")

	jpeg := []byte("second-session")
	fmt.Fprintf(conn2, "FRAME 9 %d\n", len(jpeg))
	conn2.Write(jpeg)
	expectLine(t, br2, "ACK 9")

	if w, h := tc.client.FrameGeometry(); w != 800 || h != 600 {
		t.Fatalf("geometry after reconnect = %dx%d, want 800x600", w, h)
	}
}

func TestSessionSurvivesBadFrameHeader(t *testing.T) {
	tc := newTestClient(t)
	conn, br := tc.accept(t)
	expectLine(t, br, "HELLO kindled/1 1072 1448")
	fmt.Fprintf(conn, "READY 1072 1448 3\n")
	fmt.Fprintf(conn, "FRAME not-a-number\n")

	// The client drops the session and dials again rather than wedging.
	conn2, br2 := tc.accept(t)
	expectLine(t, br2, "HELLO kindled/1 1072 1448")
	_ = conn2
}

func TestHasCapability(t *testing.T) {
	cases := []struct {
		rest string
		want bool
	}{
		{"1072 1448 3", false},         // pre-capability server
		{"1072 1448 3 no-input", true}, // mirror build
		{"1072 1448 3 future no-input", true},
		{"1072 1448 3 input", false}, // unrelated token
		{"1072 1448", false},         // truncated
		{"", false},
	}
	for _, c := range cases {
		if got := hasCapability(c.rest, "no-input"); got != c.want {
			t.Errorf("hasCapability(%q) = %v, want %v", c.rest, got, c.want)
		}
	}
}

// sync sends a frame and waits for its ack, which proves the client has
// finished processing everything sent before it -- READY included.
func (tc *testClient) sync(t *testing.T, conn net.Conn, br *bufio.Reader, seq int) {
	t.Helper()
	jpeg := []byte("frame")
	fmt.Fprintf(conn, "FRAME %d %d\n", seq, len(jpeg))
	conn.Write(jpeg)
	expectLine(t, br, fmt.Sprintf("ACK %d", seq))
}

// A mirror-only phone cannot act on touches, so the Kindle should not spend
// the link sending them.
func TestMirrorOnlyPixelSuppressesGestures(t *testing.T) {
	tc := newTestClient(t)
	conn, br := tc.accept(t)
	expectLine(t, br, "HELLO kindled/1 1072 1448")
	fmt.Fprintf(conn, "READY 1072 1448 3 no-input\n")
	tc.sync(t, conn, br, 1)

	tc.gestures <- Gesture{Kind: "SCROLL", DY: 500}
	tc.gestures <- Gesture{Kind: "TAP", X: 1, Y: 2}

	// Frames keep flowing; only the gestures are dropped. Had either been
	// sent it would arrive ahead of this ack.
	tc.sync(t, conn, br, 2)
}

// The phone app released before capabilities existed says nothing, and must
// keep receiving gestures.
func TestPreCapabilityPixelStillGetsGestures(t *testing.T) {
	tc := newTestClient(t)
	conn, br := tc.accept(t)
	expectLine(t, br, "HELLO kindled/1 1072 1448")
	fmt.Fprintf(conn, "READY 1072 1448 3\n")
	tc.sync(t, conn, br, 1)

	tc.gestures <- Gesture{Kind: "SCROLL", DY: 500}
	expectLine(t, br, "SCROLL 500")
}

// Until READY lands the frame geometry is a guess, so a gesture that beats
// it would carry coordinates in the wrong space.
func TestGesturesHeldUntilReady(t *testing.T) {
	tc := newTestClient(t)
	conn, br := tc.accept(t)
	expectLine(t, br, "HELLO kindled/1 1072 1448")

	tc.gestures <- Gesture{Kind: "TAP", X: 5, Y: 5}
	time.Sleep(20 * time.Millisecond)

	fmt.Fprintf(conn, "READY 1072 1448 3\n")
	tc.sync(t, conn, br, 1)

	tc.gestures <- Gesture{Kind: "SCROLL", DY: 7}
	expectLine(t, br, "SCROLL 7") // the pre-READY tap never appears
}
