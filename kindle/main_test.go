package main

import (
	"strings"
	"testing"
)

func TestVersionStringCarriesTheProtocol(t *testing.T) {
	v := versionString()
	if !strings.Contains(v, protocolVersion) {
		t.Fatalf("version %q should name the protocol it speaks", v)
	}
	if !strings.Contains(v, buildVersion) {
		t.Fatalf("version %q should carry the build stamp", v)
	}
}

func TestParseEIPSInfo(t *testing.T) {
	// Shape of `eips -i` on a Kindle: loose key/value pairs, and the
	// virtual resolution shows up right after the real one.
	const out = `
 xres 1072 yres 1448 xres_virtual 1088 yres_virtual 1448
 bpp 8 rotate 0
`
	w, h, ok := parseEIPSInfo(out)
	if !ok || w != 1072 || h != 1448 {
		t.Fatalf("got %dx%d ok=%v, want 1072x1448", w, h, ok)
	}
}

func TestParseEIPSInfoGarbage(t *testing.T) {
	for _, s := range []string{"", "eips: not found", "xres abc yres 100"} {
		if _, _, ok := parseEIPSInfo(s); ok {
			t.Fatalf("parseEIPSInfo(%q) should not have succeeded", s)
		}
	}
}

func TestPickBackend(t *testing.T) {
	for name, want := range map[string]string{"eips": "eips", "fbink": "fbink", "none": "null"} {
		b, err := pickBackend(name, "eips", "fbink")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b.Name() != want {
			t.Fatalf("%s gave backend %s", name, b.Name())
		}
	}
	if _, err := pickBackend("vnc", "eips", "fbink"); err == nil {
		t.Fatal("unknown backend should be rejected")
	}
}

func TestBackendArgv(t *testing.T) {
	eips := NewEIPSBackend("/usr/sbin/eips").(*execBackend)
	if got := eips.argv("/tmp/f.jpg", false); got[0] != "/usr/sbin/eips" || got[1] != "-g" {
		t.Fatalf("partial eips argv = %v", got)
	}
	if got := eips.argv("/tmp/f.jpg", true); got[1] != "-f" {
		t.Fatalf("full eips argv = %v", got)
	}
	fbink := NewFBInkBackend("fbink").(*execBackend)
	if got := fbink.argv("/tmp/f.jpg", true); got[len(got)-1] != "file=/tmp/f.jpg" {
		t.Fatalf("fbink argv = %v", got)
	}
}
