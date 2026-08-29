package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Frame is a single JPEG image received from the Pixel.
type Frame struct {
	Seq  uint64
	JPEG []byte
}

// Backend paints an image file onto the e-ink panel. eips is the default
// because it is the already-proven path on a jailbroken Kindle; fbink is a
// drop-in replacement if eips turns out to be too slow or too dumb.
type Backend interface {
	Name() string
	// Show paints path. full requests a clean, flashing, high quality
	// refresh instead of a fast partial one.
	Show(path string, full bool) error
}

type execBackend struct {
	name string
	argv func(path string, full bool) []string
}

func (b *execBackend) Name() string { return b.name }

func (b *execBackend) Show(path string, full bool) error {
	argv := b.argv(path, full)
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", b.name, err, trimOutput(out))
	}
	return nil
}

func trimOutput(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// NewEIPSBackend renders with the stock Kindle eips utility.
//
//	eips -g <file>        fast, partial-ish update
//	eips -f -g <file>     full flashing refresh
func NewEIPSBackend(bin string) Backend {
	return &execBackend{name: "eips", argv: func(path string, full bool) []string {
		if full {
			return []string{bin, "-f", "-g", path}
		}
		return []string{bin, "-g", path}
	}}
}

// NewFBInkBackend renders with FBInk, the step-10 upgrade path.
func NewFBInkBackend(bin string) Backend {
	return &execBackend{name: "fbink", argv: func(path string, full bool) []string {
		if full {
			return []string{bin, "-q", "-f", "-g", "file=" + path}
		}
		return []string{bin, "-q", "-g", "file=" + path}
	}}
}

// NewNullBackend writes frames to disk but paints nothing. Useful on a
// developer machine and for the tests.
func NewNullBackend() Backend {
	return &execBackend{name: "null", argv: func(path string, full bool) []string {
		return []string{"true"}
	}}
}

// RendererConfig tunes the e-ink refresh policy.
type RendererConfig struct {
	// Dir holds the frame files handed to the backend.
	Dir string
	// IdleRefresh is how long the link must be quiet before the last frame
	// is repainted with a clean, high quality refresh. Zero disables it.
	IdleRefresh time.Duration
	// FullEvery forces a full refresh every N frames to shake off e-ink
	// ghosting. Zero disables it.
	FullEvery int
	// Logf receives human readable progress. May be nil.
	Logf func(format string, args ...interface{})
}

// Renderer owns the e-ink panel. It accepts frames from the network and
// paints them one at a time, keeping only the newest: if a frame arrives
// while another is being painted, any frame already waiting is dropped.
type Renderer struct {
	cfg     RendererConfig
	backend Backend

	mu      sync.Mutex
	cond    *sync.Cond
	pending *Frame
	closed  bool

	// Rendered is signalled with the sequence number of every frame that
	// actually reached the panel, so the network layer can ACK it.
	Rendered chan uint64

	// Dropped counts frames that were superseded before being painted.
	dropped uint64
	painted uint64
}

// NewRenderer returns a Renderer that is not running yet; call Run.
func NewRenderer(backend Backend, cfg RendererConfig) *Renderer {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...interface{}) {}
	}
	r := &Renderer{
		cfg:      cfg,
		backend:  backend,
		Rendered: make(chan uint64, 8),
	}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// Submit hands a frame to the renderer. It never blocks and never queues:
// a frame that is still waiting when a newer one arrives is discarded.
func (r *Renderer) Submit(f Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.pending != nil {
		r.dropped++
		r.cfg.Logf("drop frame %d (superseded by %d)", r.pending.Seq, f.Seq)
	}
	cp := f
	r.pending = &cp
	r.cond.Broadcast()
}

// Close stops the render loop after the frame in flight.
func (r *Renderer) Close() {
	r.mu.Lock()
	r.closed = true
	r.pending = nil
	r.mu.Unlock()
	r.cond.Broadcast()
}

// Stats reports painted and dropped frame counts.
func (r *Renderer) Stats() (painted, dropped uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.painted, r.dropped
}

// take blocks until a frame is pending or the renderer is closed. It returns
// (nil, false) once closed.
func (r *Renderer) take() (*Frame, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.pending == nil && !r.closed {
		r.cond.Wait()
	}
	if r.closed {
		return nil, false
	}
	f := r.pending
	r.pending = nil
	return f, true
}

// takeTimeout is take with a deadline; ok is false on close, timedOut is true
// when the deadline expired with nothing pending.
func (r *Renderer) takeTimeout(d time.Duration) (f *Frame, ok bool, timedOut bool) {
	// The callback takes the lock so it cannot slip in between the loop's
	// deadline check and cond.Wait, which would lose the wakeup.
	timer := time.AfterFunc(d, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.cond.Broadcast()
	})
	defer timer.Stop()
	deadline := time.Now().Add(d)

	r.mu.Lock()
	defer r.mu.Unlock()
	for r.pending == nil && !r.closed && time.Now().Before(deadline) {
		r.cond.Wait()
	}
	if r.closed {
		return nil, false, false
	}
	if r.pending == nil {
		return nil, true, true
	}
	got := r.pending
	r.pending = nil
	return got, true, false
}

// Run paints frames until Close is called. It is meant to be the only
// goroutine touching the panel.
func (r *Renderer) Run() {
	if err := os.MkdirAll(r.cfg.Dir, 0o755); err != nil {
		r.cfg.Logf("cannot create %s: %v", r.cfg.Dir, err)
	}
	// Two buffers, used alternately: the backend may still have the
	// previous file open when we write the next one.
	paths := [2]string{
		filepath.Join(r.cfg.Dir, "frame-a.jpg"),
		filepath.Join(r.cfg.Dir, "frame-b.jpg"),
	}
	slot := 0
	lastPath := ""
	dirty := false // a partial refresh is on screen and could be cleaned up

	for {
		var f *Frame
		var ok bool
		if dirty && r.cfg.IdleRefresh > 0 {
			var timedOut bool
			f, ok, timedOut = r.takeTimeout(r.cfg.IdleRefresh)
			if ok && timedOut {
				// The link went quiet: repaint what is already on
				// screen, properly this time.
				if lastPath != "" {
					if err := r.backend.Show(lastPath, true); err != nil {
						r.cfg.Logf("idle refresh failed: %v", err)
					} else {
						r.cfg.Logf("idle refresh")
					}
				}
				dirty = false
				continue
			}
		} else {
			f, ok = r.take()
		}
		if !ok {
			return
		}

		path := paths[slot]
		slot ^= 1
		if err := os.WriteFile(path, f.JPEG, 0o644); err != nil {
			r.cfg.Logf("write %s: %v", path, err)
			continue
		}

		r.mu.Lock()
		r.painted++
		n := r.painted
		r.mu.Unlock()

		full := r.cfg.FullEvery > 0 && n%uint64(r.cfg.FullEvery) == 0
		start := time.Now()
		if err := r.backend.Show(path, full); err != nil {
			r.cfg.Logf("show frame %d: %v", f.Seq, err)
		} else {
			r.cfg.Logf("frame %d painted in %v (%d bytes, full=%v)",
				f.Seq, time.Since(start).Round(time.Millisecond), len(f.JPEG), full)
		}
		lastPath = path
		dirty = !full

		select {
		case r.Rendered <- f.Seq:
		default: // nobody listening; ACKs are best effort
		}
	}
}
