// Command fakepixel stands in for the Android app.
//
// It speaks the server half of PROTOCOL.md and streams a generated test
// pattern, so the Kindle daemon can be proven end to end -- display path,
// frame pacing, reconnect, gestures -- before the phone is involved at all.
//
//	fakepixel -listen :45831
//	kindled -addr <this machine> -v
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	listen := flag.String("listen", ":45831", "address to listen on")
	interval := flag.Duration("interval", 333*time.Millisecond, "frame interval")
	ackDeadline := flag.Duration("ack-deadline", time.Second, "how long to wait for an ACK before sending anyway")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fakepixel listening on %s", ln.Addr())

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go serve(conn, *interval, *ackDeadline)
	}
}

func serve(conn net.Conn, interval, ackDeadline time.Duration) {
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetNoDelay(true)
	}
	log.Printf("kindle connected from %s", conn.RemoteAddr())

	br := bufio.NewReader(conn)
	hello, err := br.ReadString('\n')
	if err != nil {
		log.Printf("no HELLO: %v", err)
		return
	}
	fields := strings.Fields(hello)
	if len(fields) < 4 || fields[0] != "HELLO" {
		fmt.Fprintf(conn, "BYE bad-hello\n")
		return
	}
	if !strings.HasPrefix(fields[1], "kindled/") {
		fmt.Fprintf(conn, "BYE unsupported-version\n")
		return
	}
	w, _ := strconv.Atoi(fields[2])
	h, _ := strconv.Atoi(fields[3])
	if w <= 0 || h <= 0 {
		fmt.Fprintf(conn, "BYE bad-geometry\n")
		return
	}
	log.Printf("panel %dx%d", w, h)
	fmt.Fprintf(conn, "READY %d %d %d\n", w, h, int(time.Second/interval))

	var (
		mu     sync.Mutex
		acked  int64
		scroll int
	)

	go func() {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				log.Printf("kindle gone: %v", err)
				conn.Close()
				return
			}
			verb, rest := splitVerb(strings.TrimSpace(line))
			switch verb {
			case "ACK":
				n, _ := strconv.ParseInt(rest, 10, 64)
				mu.Lock()
				if n > acked {
					acked = n
				}
				mu.Unlock()
			case "TAP":
				log.Printf("TAP %s", rest)
			case "SCROLL":
				dy, _ := strconv.Atoi(rest)
				mu.Lock()
				scroll += dy
				mu.Unlock()
				log.Printf("SCROLL %d (total %d)", dy, scroll)
			case "PING":
				fmt.Fprintf(conn, "PONG\n")
			}
		}
	}()

	var seq int64
	for {
		time.Sleep(interval)

		mu.Lock()
		offset := scroll
		lastAcked := acked
		mu.Unlock()

		// Same rule as the real server: hold off until the Kindle has
		// acked, but never wait longer than the deadline.
		if seq > 0 && lastAcked < seq {
			deadline := time.Now().Add(ackDeadline)
			for time.Now().Before(deadline) {
				mu.Lock()
				lastAcked = acked
				mu.Unlock()
				if lastAcked >= seq {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if lastAcked < seq {
				log.Printf("frame %d never acked; sending anyway", seq)
			}
		}

		seq++
		jpegBytes, err := testPattern(w, h, seq, offset)
		if err != nil {
			log.Printf("encode: %v", err)
			return
		}
		if _, err := fmt.Fprintf(conn, "FRAME %d %d\n", seq, len(jpegBytes)); err != nil {
			return
		}
		if _, err := conn.Write(jpegBytes); err != nil {
			return
		}
	}
}

// testPattern draws something whose motion is obvious on e-ink: a ruled
// grid, a frame counter bar that advances every frame, and a marker that
// moves with the accumulated scroll.
func testPattern(w, h int, seq int64, scrollOffset int) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}

	line := func(x0, y0, x1, y1 int, v uint8) {
		for y := y0; y < y1 && y < h; y++ {
			for x := x0; x < x1 && x < w; x++ {
				if x >= 0 && y >= 0 {
					img.SetGray(x, y, color.Gray{Y: v})
				}
			}
		}
	}

	for y := 0; y < h; y += 100 {
		line(0, y, w, y+2, 0xc0)
	}
	for x := 0; x < w; x += 100 {
		line(x, 0, x+2, h, 0xc0)
	}

	// Frame counter: a bar that grows one step per frame and wraps.
	barW := int(seq%20) * (w / 20)
	line(0, 0, barW, 40, 0x20)

	// Scroll marker: moves up as the reader scrolls the content down.
	markerY := h/2 - scrollOffset
	for markerY < 0 {
		markerY += h
	}
	line(w/4, markerY%h, 3*w/4, markerY%h+30, 0x40)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func splitVerb(line string) (string, string) {
	if i := strings.IndexByte(line, ' '); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}
