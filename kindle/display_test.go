package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type shown struct {
	path string
	full bool
	data []byte
}

type fakeBackend struct {
	mu    sync.Mutex
	calls []shown
	delay time.Duration
	// If non-nil, Show announces itself on entered and then blocks until a
	// token shows up on gate. That lets a test pin the renderer inside a
	// paint and submit frames behind its back.
	entered chan struct{}
	gate    chan struct{}
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Show(path string, full bool) error {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	data, _ := os.ReadFile(path)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, shown{path: path, full: full, data: data})
	return nil
}

func (f *fakeBackend) snapshot() []shown {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]shown, len(f.calls))
	copy(out, f.calls)
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRendererPaintsFrame(t *testing.T) {
	be := &fakeBackend{}
	r := NewRenderer(be, RendererConfig{Dir: t.TempDir()})
	go r.Run()
	defer r.Close()

	r.Submit(Frame{Seq: 7, JPEG: []byte("jpeg-bytes")})

	select {
	case seq := <-r.Rendered:
		if seq != 7 {
			t.Fatalf("acked seq %d, want 7", seq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("frame was never rendered")
	}

	calls := be.snapshot()
	if len(calls) != 1 {
		t.Fatalf("backend called %d times, want 1", len(calls))
	}
	if string(calls[0].data) != "jpeg-bytes" {
		t.Fatalf("backend saw %q", calls[0].data)
	}
	if calls[0].full {
		t.Fatal("first frame should be a fast partial update")
	}
}

// The headline rule from the plan: if the Kindle is still painting 101 when
// 102 and 103 arrive, 102 is dropped and 103 is painted next.
func TestRendererDropsStaleFrames(t *testing.T) {
	be := &fakeBackend{entered: make(chan struct{}), gate: make(chan struct{})}
	r := NewRenderer(be, RendererConfig{Dir: t.TempDir()})
	go r.Run()
	defer r.Close()

	r.Submit(Frame{Seq: 101, JPEG: []byte("101")})
	<-be.entered // renderer is now pinned inside the paint of 101

	// Both arrive while 101 is still on the panel.
	r.Submit(Frame{Seq: 102, JPEG: []byte("102")})
	r.Submit(Frame{Seq: 103, JPEG: []byte("103")})

	be.gate <- struct{}{} // 101 finishes
	if seq := <-r.Rendered; seq != 101 {
		t.Fatalf("first ack was %d, want 101", seq)
	}

	<-be.entered
	be.gate <- struct{}{}
	if seq := <-r.Rendered; seq != 103 {
		t.Fatalf("second ack was %d, want 103 (102 must be dropped)", seq)
	}

	calls := be.snapshot()
	if len(calls) != 2 {
		t.Fatalf("painted %d frames, want 2", len(calls))
	}
	if string(calls[1].data) != "103" {
		t.Fatalf("second painted frame was %q, want 103", calls[1].data)
	}
	if _, dropped := r.Stats(); dropped != 1 {
		t.Fatalf("dropped counter = %d, want 1", dropped)
	}
}

func TestRendererFullRefreshEveryN(t *testing.T) {
	be := &fakeBackend{}
	r := NewRenderer(be, RendererConfig{Dir: t.TempDir(), FullEvery: 3})
	go r.Run()
	defer r.Close()

	for i := uint64(1); i <= 3; i++ {
		r.Submit(Frame{Seq: i, JPEG: []byte("x")})
		<-r.Rendered
	}
	calls := be.snapshot()
	if len(calls) != 3 {
		t.Fatalf("painted %d frames", len(calls))
	}
	if calls[0].full || calls[1].full {
		t.Fatal("frames 1 and 2 should be partial")
	}
	if !calls[2].full {
		t.Fatal("every third frame should be a full refresh")
	}
}

func TestRendererIdleHighQualityRefresh(t *testing.T) {
	be := &fakeBackend{}
	r := NewRenderer(be, RendererConfig{Dir: t.TempDir(), IdleRefresh: 30 * time.Millisecond})
	go r.Run()
	defer r.Close()

	r.Submit(Frame{Seq: 1, JPEG: []byte("x")})
	<-r.Rendered

	waitFor(t, "idle refresh", func() bool { return len(be.snapshot()) == 2 })
	calls := be.snapshot()
	if !calls[1].full {
		t.Fatal("idle repaint should be a full refresh")
	}
	if calls[1].path != calls[0].path {
		t.Fatal("idle repaint should reuse the last frame file")
	}

	// And it must happen exactly once, not on a loop.
	time.Sleep(120 * time.Millisecond)
	if n := len(be.snapshot()); n != 2 {
		t.Fatalf("idle refresh repeated: %d calls", n)
	}
}

func TestRendererAlternatesFrameFiles(t *testing.T) {
	be := &fakeBackend{}
	dir := t.TempDir()
	r := NewRenderer(be, RendererConfig{Dir: dir})
	go r.Run()
	defer r.Close()

	for i := uint64(1); i <= 2; i++ {
		r.Submit(Frame{Seq: i, JPEG: []byte("x")})
		<-r.Rendered
	}
	calls := be.snapshot()
	if calls[0].path == calls[1].path {
		t.Fatal("consecutive frames must not reuse the same file")
	}
	for _, name := range []string{"frame-a.jpg", "frame-b.jpg"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}
