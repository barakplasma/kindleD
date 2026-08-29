// Command kindled is the Kindle half of the Kindle-as-Pixel-display link.
//
// It does as little as possible: connect to the Pixel over its Wi-Fi
// hotspot, paint the JPEGs it sends with eips, and report taps and scrolls
// from the touchscreen. Everything complicated lives on the Pixel.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

// buildVersion is stamped by the release build:
//
//	go build -ldflags "-X main.buildVersion=v1.2.3"
var buildVersion = "dev"

func versionString() string {
	return "kindled " + buildVersion + " (" + protocolVersion + ")"
}

func main() {
	var (
		addr        = flag.String("addr", "", "Pixel address (host or host:port); default is the hotspot gateway")
		port        = flag.Int("port", 45831, "Pixel port")
		backendName = flag.String("backend", "eips", "display backend: eips, fbink or none")
		eipsBin     = flag.String("eips", "eips", "path to the eips binary")
		fbinkBin    = flag.String("fbink", "fbink", "path to the fbink binary")
		frameDir    = flag.String("frame-dir", "/tmp/kindled", "directory for received frames")
		width       = flag.Int("width", 0, "panel width in pixels (0 = ask eips)")
		height      = flag.Int("height", 0, "panel height in pixels (0 = ask eips)")
		idleRefresh = flag.Duration("idle-refresh", 500*time.Millisecond, "quiet period before a clean full refresh (0 disables)")
		fullEvery   = flag.Int("full-every", 16, "force a full refresh every N frames (0 disables)")
		touchPath   = flag.String("touch", "", "touchscreen device (default: autodetect)")
		swapXY      = flag.Bool("touch-swap-xy", false, "swap touchscreen axes")
		invertX     = flag.Bool("touch-invert-x", false, "invert touchscreen X axis")
		invertY     = flag.Bool("touch-invert-y", false, "invert touchscreen Y axis")
		tapSlop     = flag.Int("tap-slop", 30, "max movement (px) still counted as a tap")
		tapMax      = flag.Duration("tap-max", 400*time.Millisecond, "max duration still counted as a tap")
		scrollMin   = flag.Int("scroll-min", 60, "min vertical drag (px) reported as a scroll")
		verbose     = flag.Bool("v", false, "verbose logging")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(versionString())
		return
	}

	log.SetFlags(log.Ltime)
	logf := func(format string, args ...interface{}) { log.Printf(format, args...) }
	debugf := logf
	if !*verbose {
		debugf = func(string, ...interface{}) {}
	}
	log.Println(versionString())

	w, h := panelSize(*width, *height, *eipsBin, logf)
	log.Printf("panel %dx%d", w, h)

	backend, err := pickBackend(*backendName, *eipsBin, *fbinkBin)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("display backend: %s", backend.Name())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	renderer := NewRenderer(backend, RendererConfig{
		Dir:         *frameDir,
		IdleRefresh: *idleRefresh,
		FullEvery:   *fullEvery,
		Logf:        debugf,
	})
	go renderer.Run()
	defer renderer.Close()

	mapper := NewMapper(w, h)
	mapper.SwapXY, mapper.InvertX, mapper.InvertY = *swapXY, *invertX, *invertY

	// The touchscreen is optional: a Kindle that only mirrors the Pixel is
	// still useful, so a missing digitiser must not stop the daemon.
	gestures := make(chan Gesture, 8)
	if dev, err := openTouch(*touchPath, mapper, logf); err != nil {
		log.Printf("touch input disabled: %v", err)
	} else {
		defer dev.Close()
		cfg := GestureConfig{TapSlop: *tapSlop, TapMaxDur: *tapMax, ScrollMin: *scrollMin}
		rec := NewRecognizer(cfg, mapper)
		go func() {
			if err := dev.Run(ctx, rec, gestures, debugf); err != nil {
				log.Printf("touch reader stopped: %v", err)
			}
		}()
	}

	client := NewClient(NetConfig{
		Addr:       *addr,
		Port:       *port,
		Width:      w,
		Height:     h,
		OnGeometry: mapper.SetFrameSize,
		Logf:       logf,
	}, renderer, gestures)

	client.Run(ctx)

	painted, dropped := renderer.Stats()
	log.Printf("shutting down: %d frames painted, %d dropped", painted, dropped)
}

func pickBackend(name, eipsBin, fbinkBin string) (Backend, error) {
	switch name {
	case "eips":
		return NewEIPSBackend(eipsBin), nil
	case "fbink":
		return NewFBInkBackend(fbinkBin), nil
	case "none":
		return NewNullBackend(), nil
	default:
		return nil, fmt.Errorf("unknown backend %q (want eips, fbink or none)", name)
	}
}

func openTouch(path string, mapper *Mapper, logf func(string, ...interface{})) (*TouchDevice, error) {
	if path == "" {
		found, err := FindTouchDevice()
		if err != nil {
			return nil, err
		}
		path = found
	}
	dev, err := OpenTouchDevice(path)
	if err != nil {
		return nil, err
	}
	logf("touch device %s (%s) range x=%d..%d y=%d..%d",
		dev.Path, dev.Name, dev.MinX, dev.MaxX, dev.MinY, dev.MaxY)
	mapper.SetRawRange(dev.MinX, dev.MaxX, dev.MinY, dev.MaxY)
	return dev, nil
}

// panelSize resolves the panel geometry: explicit flags win, otherwise ask
// eips, otherwise assume a 2024 Paperwhite-class panel.
func panelSize(w, h int, eipsBin string, logf func(string, ...interface{})) (int, int) {
	if w > 0 && h > 0 {
		return w, h
	}
	out, err := exec.Command(eipsBin, "-i").CombinedOutput()
	if err == nil {
		if ew, eh, ok := parseEIPSInfo(string(out)); ok {
			return ew, eh
		}
		logf("could not parse `eips -i` output, falling back to defaults")
	} else {
		logf("`eips -i` failed (%v), falling back to defaults", err)
	}
	return 1072, 1448
}

var eipsResRe = regexp.MustCompile(`\b(xres|yres)\s+(\d+)`)

// parseEIPSInfo pulls the panel geometry out of `eips -i`, whose output is
// a loose bag of "key value" pairs that varies between firmware versions.
func parseEIPSInfo(s string) (int, int, bool) {
	var w, h int
	for _, m := range eipsResRe.FindAllStringSubmatch(s, -1) {
		v, err := strconv.Atoi(m[2])
		if err != nil || v <= 0 {
			continue
		}
		switch m[1] {
		case "xres":
			if w == 0 {
				w = v // xres comes before xres_virtual
			}
		case "yres":
			if h == 0 {
				h = v
			}
		}
	}
	return w, h, w > 0 && h > 0
}
