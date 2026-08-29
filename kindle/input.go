package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Linux input event codes we care about. The Kindle touchscreen speaks
// multitouch protocol B; BTN_TOUCH/ABS_X/ABS_Y are handled too so a
// single-touch digitiser also works.
const (
	evSyn = 0x00
	evKey = 0x01
	evAbs = 0x03

	synReport = 0x00
	btnTouch  = 0x14a

	absX             = 0x00
	absY             = 0x01
	absMTSlot        = 0x2f
	absMTPositionX   = 0x35
	absMTPositionY   = 0x36
	absMTTrackingID  = 0x39
	absMax           = 0x3f
	absBitmaskBytes  = (absMax + 8) / 8
	inputEventExtras = 8 // type(2) + code(2) + value(4)
)

// eventSize matches the kernel's struct input_event for this process's ABI:
// a timeval followed by type, code and value.
var eventSize = int(unsafe.Sizeof(syscall.Timeval{})) + inputEventExtras

// nativeEndian is what the kernel writes into the event stream.
var nativeEndian binary.ByteOrder = func() binary.ByteOrder {
	var probe uint16 = 0x0102
	if (*[2]byte)(unsafe.Pointer(&probe))[0] == 0x01 {
		return binary.BigEndian
	}
	return binary.LittleEndian
}()

// inputEvent is the decoded form of one kernel event.
type inputEvent struct {
	Type  uint16
	Code  uint16
	Value int32
}

// decodeEvent decodes one struct input_event. The timestamp is discarded:
// gesture timing uses the local clock, which avoids caring whether the
// kernel hands us monotonic or realtime stamps.
func decodeEvent(b []byte) (inputEvent, error) {
	if len(b) < eventSize {
		return inputEvent{}, fmt.Errorf("short input event: %d bytes", len(b))
	}
	off := eventSize - inputEventExtras
	return inputEvent{
		Type:  nativeEndian.Uint16(b[off : off+2]),
		Code:  nativeEndian.Uint16(b[off+2 : off+4]),
		Value: int32(nativeEndian.Uint32(b[off+4 : off+8])),
	}, nil
}

// Mapper turns raw touchscreen coordinates into frame coordinates. The
// touch digitiser rarely shares the panel's resolution or orientation.
type Mapper struct {
	mu sync.RWMutex

	rawMinX, rawMaxX int
	rawMinY, rawMaxY int
	width, height    int

	SwapXY  bool
	InvertX bool
	InvertY bool
}

// NewMapper builds a mapper for a panel of w x h pixels.
func NewMapper(w, h int) *Mapper {
	return &Mapper{width: w, height: h}
}

// SetRawRange records the digitiser's reported axis ranges.
func (m *Mapper) SetRawRange(minX, maxX, minY, maxY int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rawMinX, m.rawMaxX, m.rawMinY, m.rawMaxY = minX, maxX, minY, maxY
}

// SetFrameSize updates the coordinate space gestures are reported in. The
// Pixel declares it in READY, so it can change on reconnect.
func (m *Mapper) SetFrameSize(w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.width, m.height = w, h
}

// FrameSize returns the current frame coordinate space.
func (m *Mapper) FrameSize() (int, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.width, m.height
}

// Map converts a raw touch point to frame coordinates. Axes are normalised,
// then swapped, then inverted, then scaled.
func (m *Mapper) Map(rx, ry int) (int, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	nx := normalize(rx, m.rawMinX, m.rawMaxX, m.width)
	ny := normalize(ry, m.rawMinY, m.rawMaxY, m.height)
	if m.SwapXY {
		nx, ny = ny, nx
	}
	if m.InvertX {
		nx = 1 - nx
	}
	if m.InvertY {
		ny = 1 - ny
	}
	return clamp(int(nx*float64(m.width)), 0, m.width-1),
		clamp(int(ny*float64(m.height)), 0, m.height-1)
}

// normalize maps raw into 0..1. Without a usable range from the driver the
// raw value is assumed to already be in panel pixels.
func normalize(raw, min, max, span int) float64 {
	if max <= min {
		if span <= 1 {
			return 0
		}
		return float64(raw) / float64(span-1)
	}
	v := float64(raw-min) / float64(max-min)
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// GestureConfig tunes touch interpretation. Deliberately coarse: e-ink is
// not the place for pixel-accurate flick physics.
type GestureConfig struct {
	// TapSlop is how far a finger may wander and still count as a tap.
	TapSlop int
	// TapMaxDur is the longest touch that still counts as a tap.
	TapMaxDur time.Duration
	// ScrollMin is the shortest vertical drag reported as a scroll.
	ScrollMin int
	// Now is the clock, overridable in tests.
	Now func() time.Time
}

// DefaultGestureConfig is tuned for a 1072x1448 panel.
func DefaultGestureConfig() GestureConfig {
	return GestureConfig{TapSlop: 30, TapMaxDur: 400 * time.Millisecond, ScrollMin: 60}
}

// Recognizer turns a stream of kernel events into whole gestures. Only
// completed gestures leave the Kindle: streaming every motion event would
// swamp a 3 FPS panel for no benefit.
type Recognizer struct {
	cfg    GestureConfig
	mapper *Mapper

	slot                   int32
	rawX, rawY             int
	haveRaw                bool
	pendingDown, pendingUp bool

	active         bool
	startX, startY int
	curX, curY     int
	startAt        time.Time
}

// NewRecognizer returns a Recognizer reporting in the mapper's frame space.
func NewRecognizer(cfg GestureConfig, m *Mapper) *Recognizer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TapMaxDur == 0 {
		cfg.TapMaxDur = DefaultGestureConfig().TapMaxDur
	}
	return &Recognizer{cfg: cfg, mapper: m}
}

// Feed consumes one event and returns a gesture when one completes.
func (r *Recognizer) Feed(ev inputEvent) *Gesture {
	switch ev.Type {
	case evAbs:
		switch ev.Code {
		case absMTSlot:
			r.slot = ev.Value
		case absMTTrackingID:
			if r.slot != 0 {
				return nil
			}
			if ev.Value >= 0 {
				r.pendingDown = true
			} else {
				r.pendingUp = true
			}
		case absMTPositionX, absX:
			if r.slot == 0 {
				r.rawX, r.haveRaw = int(ev.Value), true
			}
		case absMTPositionY, absY:
			if r.slot == 0 {
				r.rawY, r.haveRaw = int(ev.Value), true
			}
		}
	case evKey:
		if ev.Code == btnTouch {
			if ev.Value != 0 {
				r.pendingDown = true
			} else {
				r.pendingUp = true
			}
		}
	case evSyn:
		if ev.Code == synReport {
			return r.report()
		}
	}
	return nil
}

// report applies everything accumulated since the last SYN_REPORT.
func (r *Recognizer) report() *Gesture {
	x, y := r.curX, r.curY
	if r.haveRaw {
		x, y = r.mapper.Map(r.rawX, r.rawY)
	}

	if r.pendingDown && !r.active {
		r.active = true
		r.startX, r.startY = x, y
		r.startAt = r.cfg.Now()
	}
	if r.active {
		r.curX, r.curY = x, y
	}
	r.pendingDown = false
	r.haveRaw = false

	if !r.pendingUp {
		return nil
	}
	r.pendingUp = false
	if !r.active {
		return nil
	}
	r.active = false
	return r.classify()
}

// classify decides what the finished touch meant.
func (r *Recognizer) classify() *Gesture {
	dx := r.curX - r.startX
	// Content direction: a finger dragged upward (900 -> 400) asks for the
	// page to advance, and reports a positive scroll.
	dy := r.startY - r.curY
	dur := r.cfg.Now().Sub(r.startAt)

	if abs(dx) <= r.cfg.TapSlop && abs(dy) <= r.cfg.TapSlop {
		if dur <= r.cfg.TapMaxDur {
			return &Gesture{Kind: "TAP", X: r.startX, Y: r.startY}
		}
		return nil // long press with no movement: nothing to say yet
	}
	if abs(dy) > abs(dx) && abs(dy) >= r.cfg.ScrollMin {
		return &Gesture{Kind: "SCROLL", DY: dy}
	}
	return nil // horizontal drags are not part of protocol v1
}

// TouchDevice is an opened /dev/input/eventN.
type TouchDevice struct {
	f    *os.File
	Path string
	Name string
	// Raw axis ranges as reported by the driver, zero when unavailable.
	MinX, MaxX, MinY, MaxY int
}

// OpenTouchDevice opens an evdev node and reads its geometry.
func OpenTouchDevice(path string) (*TouchDevice, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	d := &TouchDevice{f: f, Path: path, Name: deviceName(f.Fd())}
	if info, err := absInfo(f.Fd(), absMTPositionX); err == nil && info.Maximum > info.Minimum {
		d.MinX, d.MaxX = int(info.Minimum), int(info.Maximum)
	} else if info, err := absInfo(f.Fd(), absX); err == nil {
		d.MinX, d.MaxX = int(info.Minimum), int(info.Maximum)
	}
	if info, err := absInfo(f.Fd(), absMTPositionY); err == nil && info.Maximum > info.Minimum {
		d.MinY, d.MaxY = int(info.Minimum), int(info.Maximum)
	} else if info, err := absInfo(f.Fd(), absY); err == nil {
		d.MinY, d.MaxY = int(info.Minimum), int(info.Maximum)
	}
	return d, nil
}

// Close releases the device, which also unblocks Run.
func (d *TouchDevice) Close() error { return d.f.Close() }

// Run reads events until the context is cancelled or the device disappears,
// emitting completed gestures on out. Gestures are dropped rather than
// queued when the link is congested.
func (d *TouchDevice) Run(ctx context.Context, rec *Recognizer, out chan<- Gesture, logf func(string, ...interface{})) error {
	go func() {
		<-ctx.Done()
		d.f.Close()
	}()

	buf := make([]byte, eventSize*64)
	var carry []byte
	for {
		n, err := d.f.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		data := buf[:n]
		if len(carry) > 0 {
			data = append(carry, data...)
			carry = nil
		}
		for len(data) >= eventSize {
			ev, err := decodeEvent(data[:eventSize])
			data = data[eventSize:]
			if err != nil {
				continue
			}
			if g := rec.Feed(ev); g != nil {
				select {
				case out <- *g:
				default:
					if logf != nil {
						logf("dropping gesture %s: queue full", g.Line())
					}
				}
			}
		}
		if len(data) > 0 {
			// Fresh slice: data can alias the buffer carry was built from.
			carry = append([]byte(nil), data...)
		}
	}
}

// FindTouchDevice picks the first /dev/input/event* that reports absolute
// touch coordinates, so nobody has to hardcode an event number that Amazon
// is free to renumber in the next firmware update.
func FindTouchDevice() (string, error) {
	paths, err := filepath.Glob("/dev/input/event*")
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	fallback := ""
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		bits, err := absBits(f.Fd())
		f.Close()
		if err != nil {
			continue
		}
		if bitSet(bits, absMTPositionX) && bitSet(bits, absMTPositionY) {
			return p, nil
		}
		if fallback == "" && bitSet(bits, absX) && bitSet(bits, absY) {
			fallback = p
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", errors.New("no touchscreen found under /dev/input")
}

// absInfoData mirrors struct input_absinfo.
type absInfoData struct {
	Value, Minimum, Maximum, Fuzz, Flat, Resolution int32
}

// ioctl request encoding, asm-generic layout (what ARM and x86 both use).
func iocRead(typ, nr, size uintptr) uintptr {
	const dirRead = 2
	return dirRead<<30 | size<<16 | typ<<8 | nr
}

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// absInfo runs EVIOCGABS for one axis.
func absInfo(fd uintptr, axis uintptr) (absInfoData, error) {
	var info absInfoData
	req := iocRead('E', 0x40+axis, unsafe.Sizeof(info))
	if err := ioctl(fd, req, unsafe.Pointer(&info)); err != nil {
		return info, err
	}
	return info, nil
}

// absBits runs EVIOCGBIT(EV_ABS) to learn which absolute axes exist.
func absBits(fd uintptr) ([]byte, error) {
	bits := make([]byte, absBitmaskBytes)
	req := iocRead('E', 0x20+evAbs, uintptr(len(bits)))
	if err := ioctl(fd, req, unsafe.Pointer(&bits[0])); err != nil {
		return nil, err
	}
	return bits, nil
}

// deviceName runs EVIOCGNAME, for logging only.
func deviceName(fd uintptr) string {
	buf := make([]byte, 128)
	req := iocRead('E', 0x06, uintptr(len(buf)))
	if err := ioctl(fd, req, unsafe.Pointer(&buf[0])); err != nil {
		return "unknown"
	}
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

func bitSet(bits []byte, bit int) bool {
	idx := bit / 8
	return idx < len(bits) && bits[idx]&(1<<(uint(bit)%8)) != 0
}
