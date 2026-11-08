package micom

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// The audio frames worked out from the stock system (MicomAudioService's
// sendCommand: sndId 0x85, rcvId 2, type 1; the checksum rule of micomd). Each
// has to parse: ParseHexFrame refuses a wrong checksum, so this is the check of
// the hand-worked bytes.
var audioFrames = []string{
	"ff 85 02 01 02 0b 00 03 01 00 72", // mute control, AMP release
	"ff 85 02 01 02 0b 00 03 01 01 73", // AMP set
	"ff 85 02 01 02 0b 00 03 02 00 71", // AV release
	"ff 85 02 01 02 0b 00 03 02 01 70", // AV set
	"ff 85 02 01 02 0b 00 03 03 00 70", // NAVI release
	"ff 85 02 01 02 38 00 03 02 00 42", // fadeout release
	"ff 85 02 01 02 01 00 03 09 00 70", // audio mode USB
	"ff 85 02 01 02 01 00 03 12 00 6b", // MUSIC
	"ff 85 02 01 02 01 00 03 0e 00 77", // BT stream
	"ff 85 02 01 02 01 00 03 0c 00 75", // AUX
	"ff 85 02 01 02 01 00 03 16 00 6f", // DIPO
	"ff 85 02 01 02 03 00 02 00 7a",    // main volume 0
	"ff 85 02 01 02 03 00 02 05 7f",    // 5
	"ff 85 02 01 02 03 00 02 0a 70",    // 10
	"ff 85 02 01 02 12 00 02 02 69",    // digital sound out ON
	"ff 8a 01 01 01 01 00 01 74",       // boot-mode request
}

func TestParseHexFrameStockAudio(t *testing.T) {
	for _, s := range audioFrames {
		if _, err := ParseHexFrame(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	// Contiguous, commas, 0x prefixes: the same frame.
	a, err1 := ParseHexFrame("ff 85 02 01 02 0b 00 03 02 00 71")
	b, err2 := ParseHexFrame("ff8502010 20b00030200 71")
	c, err3 := ParseHexFrame("0xff,0x85,0x02,0x01,0x02,0x0b,0x00,0x03,0x02,0x00,0x71")
	if err1 != nil || err2 != nil || err3 != nil || !sameFrame(a, b) || !sameFrame(a, c) {
		t.Errorf("spellings: %v %v %v", err1, err2, err3)
	}
	for _, bad := range []string{
		"", "ff", "zz", "ff 85 02 01 02 0b 00 03 02 00 70", // wrong checksum
		"ff 85 02 01 02 0b 00 03 02 00",       // truncated
		"ff 85 02 01 02 0b 00 03 02 00 71 ff", // trailing byte
		"ff 85 02 01 02 0b 00 03 02 00 71 ff 85",
	} {
		if _, err := ParseHexFrame(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// A frame goes out only if an -allow spec admits it: an exact frame is matched
// byte for byte, a pattern only within its ranges, never another header,
// command or payload length.
func TestAllow(t *testing.T) {
	frame := func(s string) Frame {
		f, err := ParseHexFrame(s)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	release := "ff 85 02 01 02 0b 00 03 02 00 71"
	volume := "ff 85 02 01 02 03 00 02 [00-1e] .."
	otherCmd := frame(release)
	otherCmd.Cmd = 0x0c
	longer := frame("ff 85 02 01 02 03 00 02 16 6c")
	longer.Payload = []byte{0x16, 0x00}
	for _, c := range []struct {
		name  string
		allow string
		f     Frame
		sent  bool
	}{
		{"exact", release, frame(release), true},
		{"exact, other payload", release, frame("ff 85 02 01 02 0b 00 03 02 01 70"), false},
		{"exact, other command", release, otherCmd, false},
		{"pattern, lowest", volume, frame("ff 85 02 01 02 03 00 02 00 7a"), true},
		{"pattern, captured 0x16", volume, frame("ff 85 02 01 02 03 00 02 16 6c"), true},
		{"pattern, highest", volume, frame("ff 85 02 01 02 03 00 02 1e 64"), true},
		{"pattern, above the range", volume, frame("ff 85 02 01 02 03 00 02 1f 65"), false},
		{"pattern, other command", volume, frame(release), false},
		{"pattern, longer payload", volume, longer, false},
		{"denylisted, though allowed", "ff 8a 01 01 01 07 00 01 72", frame("ff 8a 01 01 01 07 00 01 72"), false},
		{"denylisted, though a pattern allows it", "ff 8a 01 01 01 18 00 02 [00-ff] ..", frame("ff 8a 01 01 01 18 00 02 00 6e"), false},
		{"denylisted power configuration", "ff 8a 01 01 01 02 00 02 [00-ff] ..", frame("ff 8a 01 01 01 02 00 02 00 74"), false},
	} {
		var al allowList
		if err := al.Set(c.allow); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		a, b := net.Pipe()
		go io.Copy(io.Discard, b)
		d := New(Config{Port: a, Profile: []Periodic{}, Allow: al.frames, AllowPatterns: al.patterns})
		if err := d.Send(c.f); (err == nil) != c.sent {
			t.Errorf("%s: sent %v, want %v (%v)", c.name, err == nil, c.sent, err)
		}
		b.Close()
	}
}

func TestParsePatternRefuses(t *testing.T) {
	for _, bad := range []string{
		"ff 85 02 01 02 03 00 02 [00-1e]",         // no checksum
		"ff 85 02 01 02 03 00 02 .. [00-1e]",      // checksum not last
		"ff 85 02 01 02 [03-04] 00 02 [00-1e] ..", // range in the command
		"ff 85 02 01 02 03 00 03 [00-1e] ..",      // length is not the payload's
		"ff 85 02 01 02 03 00 02 [1e-00] ..",      // empty range
		"ff 85 02 01 02 03 00 02 [00-1e ..",       // unclosed
		"fe 85 02 01 02 03 00 02 [00-1e] ..",      // no start byte
		"ff 85 02 01 ..",                          // too short
	} {
		if _, err := ParsePattern(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestControlSocket(t *testing.T) {
	a, b := net.Pipe()
	var wire syncBuf
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := b.Read(buf)
			wire.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}()
	release, _ := ParseHexFrame("ff 85 02 01 02 0b 00 03 02 00 71")
	vol, _ := ParseHexFrame("ff 85 02 01 02 03 00 02 05 7f")
	d := New(Config{Port: a, Profile: []Periodic{}, Allow: []Frame{release, vol}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := tapPath(t)
	done := make(chan error, 1)
	go func() { done <- ServeControl(ctx, d, p) }()
	for i := 0; i < 200; i++ {
		if c, err := net.Dial("unix", p); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var out bytes.Buffer
	if err := SendControl(p, []string{"ff 85 02 01 02 03 00 02 05 7f", "ff 85 02 01 02 0b 00 03 02 00 71"}, 10*time.Millisecond, &out); err != nil {
		t.Fatalf("an allowed sequence: %v (%s)", err, out.String())
	}
	if got := strings.Count(out.String(), ": ok"); got != 2 {
		t.Errorf("answers %q", out.String())
	}
	want := append(vol.Encode(), release.Encode()...)
	for i := 0; i < 100 && !bytes.Equal([]byte(wire.String()), want); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if wire.String() != string(want) {
		t.Errorf("on the wire %x, want %x", wire.String(), want)
	}

	// Not in the list: refused, nothing more sent, and the run stops there.
	out.Reset()
	err := SendControl(p, []string{"ff 85 02 01 02 0b 00 03 01 00 72", "ff 85 02 01 02 03 00 02 05 7f"}, 0, &out)
	if err == nil || !strings.Contains(out.String(), "refused") {
		t.Errorf("a frame outside the allow list: %v (%q)", err, out.String())
	}
	time.Sleep(50 * time.Millisecond)
	if wire.String() != string(want) {
		t.Errorf("a refused run still sent bytes: %x", wire.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("ServeControl: %v", err)
	}
}
