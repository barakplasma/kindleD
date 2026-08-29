package main

import (
	"encoding/binary"
	"testing"
	"time"
	"unsafe"
)

// encodeEvent builds a kernel struct input_event for this ABI.
func encodeEvent(typ, code uint16, value int32) []byte {
	b := make([]byte, eventSize)
	off := eventSize - inputEventExtras
	nativeEndian.PutUint16(b[off:], typ)
	nativeEndian.PutUint16(b[off+2:], code)
	nativeEndian.PutUint32(b[off+4:], uint32(value))
	return b
}

func TestEventSizeMatchesKernelABI(t *testing.T) {
	// timeval + type + code + value, with the struct's natural alignment.
	want := int(unsafe.Sizeof(struct {
		tv    [2]uint64
		typ   uint16
		code  uint16
		value int32
	}{}))
	if unsafe.Sizeof(uintptr(0)) == 4 {
		want = 16
	}
	if eventSize != want {
		t.Fatalf("eventSize = %d, want %d", eventSize, want)
	}
	if nativeEndian != binary.LittleEndian && nativeEndian != binary.BigEndian {
		t.Fatal("native endianness not detected")
	}
}

func TestDecodeEvent(t *testing.T) {
	ev, err := decodeEvent(encodeEvent(evAbs, absMTTrackingID, -1))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != evAbs || ev.Code != absMTTrackingID || ev.Value != -1 {
		t.Fatalf("decoded %+v", ev)
	}
	if _, err := decodeEvent(make([]byte, eventSize-1)); err == nil {
		t.Fatal("a truncated event must be rejected")
	}
}

// clock is a controllable time source for gesture timing.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestRecognizer() (*Recognizer, *clock) {
	c := &clock{t: time.Unix(1000, 0)}
	m := NewMapper(1072, 1448) // raw == panel pixels, no driver ranges
	cfg := DefaultGestureConfig()
	cfg.Now = c.now
	return NewRecognizer(cfg, m), c
}

// touch replays a protocol-B contact: down at (x0,y0), moves, then up.
func touch(r *Recognizer, points [][2]int) []Gesture {
	var out []Gesture
	feed := func(ev inputEvent) {
		if g := r.Feed(ev); g != nil {
			out = append(out, *g)
		}
	}
	for i, p := range points {
		if i == 0 {
			feed(inputEvent{Type: evAbs, Code: absMTSlot, Value: 0})
			feed(inputEvent{Type: evAbs, Code: absMTTrackingID, Value: 77})
		}
		feed(inputEvent{Type: evAbs, Code: absMTPositionX, Value: int32(p[0])})
		feed(inputEvent{Type: evAbs, Code: absMTPositionY, Value: int32(p[1])})
		feed(inputEvent{Type: evSyn, Code: synReport})
	}
	feed(inputEvent{Type: evAbs, Code: absMTTrackingID, Value: -1})
	feed(inputEvent{Type: evSyn, Code: synReport})
	return out
}

func TestRecognizerTap(t *testing.T) {
	r, _ := newTestRecognizer()
	got := touch(r, [][2]int{{412, 830}, {414, 832}})
	if len(got) != 1 {
		t.Fatalf("got %d gestures, want 1: %+v", len(got), got)
	}
	if got[0].Kind != "TAP" || got[0].X != 412 || got[0].Y != 830 {
		t.Fatalf("got %+v, want TAP 412 830", got[0])
	}
}

// The plan's worked example: 900 -> 400 must produce SCROLL +500.
func TestRecognizerScrollUp(t *testing.T) {
	r, _ := newTestRecognizer()
	got := touch(r, [][2]int{{500, 900}, {500, 700}, {500, 400}})
	if len(got) != 1 || got[0].Kind != "SCROLL" || got[0].DY != 500 {
		t.Fatalf("got %+v, want SCROLL 500", got)
	}
}

func TestRecognizerScrollDownIsNegative(t *testing.T) {
	r, _ := newTestRecognizer()
	got := touch(r, [][2]int{{500, 400}, {500, 900}})
	if len(got) != 1 || got[0].DY != -500 {
		t.Fatalf("got %+v, want SCROLL -500", got)
	}
}

func TestRecognizerIgnoresShortDragAndHorizontal(t *testing.T) {
	r, _ := newTestRecognizer()
	if got := touch(r, [][2]int{{500, 500}, {900, 505}}); len(got) != 0 {
		t.Fatalf("horizontal drag should be ignored in v1, got %+v", got)
	}

	r2, _ := newTestRecognizer()
	// Past tap slop but below the scroll threshold: nothing useful to say.
	if got := touch(r2, [][2]int{{500, 500}, {500, 455}}); len(got) != 0 {
		t.Fatalf("sub-threshold drag should be ignored, got %+v", got)
	}
}

func TestRecognizerLongPressIsNotATap(t *testing.T) {
	r, c := newTestRecognizer()
	var out []Gesture
	feed := func(ev inputEvent) {
		if g := r.Feed(ev); g != nil {
			out = append(out, *g)
		}
	}
	feed(inputEvent{Type: evAbs, Code: absMTTrackingID, Value: 5})
	feed(inputEvent{Type: evAbs, Code: absMTPositionX, Value: 100})
	feed(inputEvent{Type: evAbs, Code: absMTPositionY, Value: 100})
	feed(inputEvent{Type: evSyn, Code: synReport})
	c.add(3 * time.Second)
	feed(inputEvent{Type: evAbs, Code: absMTTrackingID, Value: -1})
	feed(inputEvent{Type: evSyn, Code: synReport})

	if len(out) != 0 {
		t.Fatalf("a 3s press is not a tap, got %+v", out)
	}
}

func TestRecognizerIgnoresSecondFinger(t *testing.T) {
	r, _ := newTestRecognizer()
	var out []Gesture
	feed := func(ev inputEvent) {
		if g := r.Feed(ev); g != nil {
			out = append(out, *g)
		}
	}
	feed(inputEvent{Type: evAbs, Code: absMTSlot, Value: 1})
	feed(inputEvent{Type: evAbs, Code: absMTTrackingID, Value: 9})
	feed(inputEvent{Type: evAbs, Code: absMTPositionX, Value: 10})
	feed(inputEvent{Type: evAbs, Code: absMTPositionY, Value: 900})
	feed(inputEvent{Type: evSyn, Code: synReport})
	feed(inputEvent{Type: evAbs, Code: absMTTrackingID, Value: -1})
	feed(inputEvent{Type: evSyn, Code: synReport})

	if len(out) != 0 {
		t.Fatalf("only the first contact drives the UI, got %+v", out)
	}
}

func TestRecognizerSingleTouchDevice(t *testing.T) {
	r, c := newTestRecognizer()
	var out []Gesture
	feed := func(ev inputEvent) {
		if g := r.Feed(ev); g != nil {
			out = append(out, *g)
		}
	}
	feed(inputEvent{Type: evKey, Code: btnTouch, Value: 1})
	feed(inputEvent{Type: evAbs, Code: absX, Value: 300})
	feed(inputEvent{Type: evAbs, Code: absY, Value: 1000})
	feed(inputEvent{Type: evSyn, Code: synReport})
	c.add(50 * time.Millisecond)
	feed(inputEvent{Type: evKey, Code: btnTouch, Value: 0})
	feed(inputEvent{Type: evSyn, Code: synReport})

	if len(out) != 1 || out[0].Kind != "TAP" || out[0].X != 300 || out[0].Y != 1000 {
		t.Fatalf("got %+v, want TAP 300 1000", out)
	}
}

func TestMapperScalesAndOrients(t *testing.T) {
	m := NewMapper(1072, 1448)
	m.SetRawRange(0, 4095, 0, 4095)

	x, y := m.Map(0, 0)
	if x != 0 || y != 0 {
		t.Fatalf("origin mapped to %d,%d", x, y)
	}
	x, y = m.Map(4095, 4095)
	if x != 1071 || y != 1447 {
		t.Fatalf("far corner mapped to %d,%d", x, y)
	}
	x, y = m.Map(2048, 2048)
	if abs(x-536) > 2 || abs(y-724) > 2 {
		t.Fatalf("centre mapped to %d,%d", x, y)
	}

	m.InvertY = true
	if _, y := m.Map(0, 0); y != 1447 {
		t.Fatalf("inverted Y origin mapped to %d", y)
	}

	m.InvertY = false
	m.SwapXY = true
	// A digitiser mounted rotated: raw Y drives screen X.
	if x, _ := m.Map(0, 4095); x != 1071 {
		t.Fatalf("swapped axes gave x=%d", x)
	}
}

func TestMapperOutOfRangeIsClamped(t *testing.T) {
	m := NewMapper(100, 200)
	m.SetRawRange(0, 999, 0, 999)
	if x, y := m.Map(-50, 5000); x != 0 || y != 199 {
		t.Fatalf("clamp failed: %d,%d", x, y)
	}
}

func TestMapperFrameSizeFollowsReady(t *testing.T) {
	m := NewMapper(1072, 1448)
	m.SetRawRange(0, 1071, 0, 1447)
	m.SetFrameSize(536, 724)
	if x, y := m.Map(1071, 1447); x != 535 || y != 723 {
		t.Fatalf("after resize got %d,%d", x, y)
	}
	m.SetFrameSize(0, 0) // nonsense from the wire is ignored
	if w, h := m.FrameSize(); w != 536 || h != 724 {
		t.Fatalf("frame size changed to %dx%d", w, h)
	}
}
