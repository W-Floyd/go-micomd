package micom

import (
	"bytes"
	"context"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Frames from the logcat capture of 2026-10-16 20:01 (MicomD lines).
var captured = []string{
	"ff 88 0c 01 0c 5c 00 01 2b",
	"ff 8f 0d 01 0d 03 00 0a ff ff ff 0e 03 ff ff 03 ff 76",
	"ff c3 4d 00 4d 17 00 09 08 d5 fc db 05 ff 81 2b 88",
	"ff 88 0c 01 0c 09 00 06 04 a0 01 10 80 4c",
	"ff c2 43 00 43 33 00 0c 02 02 14 01 10 0a 01 1a 10 00 02 04",
	"ff 0c 88 01 8c 02 00 04 57 00 54 f3",
	"ff 0c 88 01 8c 55 00 09 f8 b3 97 48 ff 55 05 00 91",
	"ff 0c 88 01 8c 5c 00 1f 11 ff ff ff ff ff ff ff ff ff 02 db 00 00 00 00 00 00 00 00 00 00 21 00 53 00 00 00 00 ff 0f",
}

func TestDecodeEncodeCaptured(t *testing.T) {
	for _, s := range captured {
		raw := unhex(t, s)
		var d Decoder
		fs := d.Write(raw)
		if len(fs) != 1 || d.BadSum != 0 || d.Dropped != 0 {
			t.Fatalf("%s: %d frames, badsum %d, dropped %d", s, len(fs), d.BadSum, d.Dropped)
		}
		if got := fs[0].Encode(); !bytes.Equal(got, raw) {
			t.Errorf("re-encode of %s = %x", s, got)
		}
	}
}

func TestProfilesMatchCapture(t *testing.T) {
	// 20:01:16 on 2026-10-16, when the captured clock frames were sent.
	now := time.Date(2026, 10, 16, 20, 1, 16, 0, time.UTC)
	want := map[string]string{
		"poll":      captured[0],
		"ack":       captured[1],
		"status":    captured[2],
		"clock-sec": captured[3],
		"clock":     captured[4],
	}
	for _, p := range append(DefaultProfile(), ClockProfile()...) {
		w, ok := want[p.Name]
		if !ok {
			t.Fatalf("no capture for %s", p.Name)
		}
		if got := p.Build(now).Encode(); !bytes.Equal(got, unhex(t, w)) {
			t.Errorf("%s: built %x, captured %s", p.Name, got, w)
		}
	}
}

func TestDecoderResyncs(t *testing.T) {
	good := unhex(t, captured[5])
	bad := append([]byte(nil), unhex(t, captured[6])...)
	bad[len(bad)-1] ^= 0x01 // corrupt the checksum
	stream := append(append(append([]byte{0x00, 0x13, 0xff}, bad...), []byte{0x7a}...), good...)
	var d Decoder
	var got []Frame
	for i := 0; i < len(stream); i += 3 { // arrives in pieces
		end := min(i+3, len(stream))
		got = append(got, d.Write(stream[i:end])...)
	}
	if len(got) != 1 || got[0].Cmd != 0x02 {
		t.Fatalf("got %v, want the one good frame", got)
	}
	if d.BadSum != 1 {
		t.Errorf("BadSum = %d, want 1", d.BadSum)
	}
}

func TestSendAllowlist(t *testing.T) {
	a, b := net.Pipe()
	d := New(Config{Port: a})
	defer b.Close()
	if err := d.Send(Frame{Hdr: [4]byte{1, 2, 3, 4}, Cmd: 0x99}); err == nil {
		t.Fatal("a frame outside the profile was sent")
	}
	// What the profile builds is allowed (the payload may differ in time).
	go func() {
		buf := make([]byte, 64)
		b.Read(buf)
	}()
	if err := d.Send(Frame{Hdr: hdrPoll, Cmd: 0x5c}); err != nil {
		t.Fatalf("a profile frame was refused: %v", err)
	}
	// The clock frames are not in the default profile.
	if err := d.Send(Frame{Hdr: hdrClock, Cmd: 0x33}); err == nil {
		t.Fatal("a clock frame was sent without the clock profile")
	}
}

func TestDescribeLive(t *testing.T) {
	// Frames seen live on the stage, 2026-10-16.
	for _, c := range []struct{ hdr, cmd, payload, want string }{
		{"0c88018c", "02", "5700f5", "Climate power=245 W"},
		{"0c88018c", "5c", "15ffffffffffffffffff01b1000000000000000000000400ff00000000ff", "Climate total=433 W"},
		{"0c88018c", "5c", "15ffffffffffffffffff01b1000000000000000000000400ff00000000ff", "Climate power=255 W"},
		{"0c88018c", "5c", "15ffffffffffffffffff01b1000000000000000000000400ff00000000ff", "AVN motor power=n/a"},
		{"0c88018c", "55", "4d749148ff550500", "Departure 1 hour=8"},
		{"0c88018c", "4f", "2d3a200100000000", "Charger ready bits=0"},
	} {
		var f Frame
		copy(f.Hdr[:], unhex(t, c.hdr))
		f.Cmd = unhex(t, c.cmd)[0]
		f.Payload = unhex(t, c.payload)
		if got := Describe(f); !strings.Contains(got, c.want) {
			t.Errorf("%s %s: %q lacks %q", c.hdr, c.cmd, got, c.want)
		}
	}
	if Describe(Frame{Hdr: [4]byte{0x0d, 0x8f, 0x01, 0x8d}, Cmd: 0x2d}) != "" {
		t.Error("unknown frame described")
	}
}

func TestSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 16, 22, 0, 0, 0, time.UTC)
	d := New(Config{Port: nil, Now: func() time.Time { return now }})
	frame := func(hdr, cmd, payload string) Frame {
		var f Frame
		copy(f.Hdr[:], unhex(t, hdr))
		f.Cmd = unhex(t, cmd)[0]
		f.Payload = unhex(t, payload)
		return f
	}
	d.telem.add(frame("0c88018c", "02", "5700f5"), now.Add(-2*time.Second))
	d.telem.add(frame("0c88018c", "02", "5700ff"), now.Add(-time.Second))
	d.telem.add(frame("0d8f018d", "2d", "00"), now)
	d.telem.add(frame("018a0181", "0f", ""), now.Add(-3*time.Second))
	s := d.Snapshot()
	if len(s.Readings) != 1 || s.Readings[0].Value != 255 || s.Readings[0].AgeMs != 1000 {
		t.Errorf("readings = %+v, want the latest climate power, 1 s old", s.Readings)
	}
	if s.LastAliveAgeMs != 3000 {
		t.Errorf("LastAliveAgeMs = %d, want 3000", s.LastAliveAgeMs)
	}
	if len(s.Frames) != 3 || s.Frames[0].Key != "0c88018c 02" || s.Frames[0].Count != 2 || s.Frames[0].Name != "Status flag" {
		t.Errorf("frames = %+v", s.Frames)
	}
}

func TestMarkAndDiffs(t *testing.T) {
	now := time.Date(2026, 10, 16, 22, 0, 0, 0, time.UTC)
	d := New(Config{Now: func() time.Time { return now }})
	frame := func(payload string) Frame {
		f := Frame{Hdr: [4]byte{0x0d, 0x8f, 0x01, 0x8d}, Cmd: 0x2d}
		f.Payload = unhex(t, payload)
		return f
	}
	diffsOf := func() []ByteDiff {
		for _, f := range d.Snapshot().Frames {
			if f.Key == "0d8f018d 2d" {
				return f.Diffs
			}
		}
		t.Fatal("no frame")
		return nil
	}
	// Before a mark: a counter in byte 0 changes with every frame.
	for i, p := range []string{"01 00 00", "02 00 00", "03 00 00"} {
		d.telem.add(frame(p), now.Add(time.Duration(i)*time.Second))
	}
	if s := d.Snapshot(); s.MarkedAgeMs != -1 {
		t.Errorf("MarkedAgeMs = %d before a mark", s.MarkedAgeMs)
	}
	if got := diffsOf(); len(got) != 1 || got[0].Idx != 0 || got[0].Changes != 2 || got[0].From != -1 {
		t.Errorf("diffs before a mark = %+v", got)
	}
	// Mark, then the counter moves again and byte 2 changes once.
	now = now.Add(10 * time.Second)
	d.Mark()
	if got := diffsOf(); len(got) != 0 {
		t.Errorf("diffs right after the mark = %+v", got)
	}
	d.telem.add(frame("04 00 00"), now.Add(time.Second))
	d.telem.add(frame("05 00 07"), now.Add(2*time.Second))
	d.telem.add(frame("06 00 07"), now.Add(3*time.Second))
	now = now.Add(5 * time.Second)
	got := diffsOf()
	if len(got) != 2 {
		t.Fatalf("diffs = %+v", got)
	}
	if b := got[0]; b.Idx != 0 || b.From != 3 || b.To != 6 || b.Changes != 3 {
		t.Errorf("counter byte = %+v", b)
	}
	if b := got[1]; b.Idx != 2 || b.From != 0 || b.To != 7 || b.Changes != 1 || b.AgeMs != 3000 {
		t.Errorf("switch byte = %+v", b)
	}
	if s := d.Snapshot(); s.MarkedAgeMs != 5000 {
		t.Errorf("MarkedAgeMs = %d, want 5000", s.MarkedAgeMs)
	}
}

func TestAliveReplyBytes(t *testing.T) {
	// Derived from SystemService.respondCPUAlive, not captured.
	if got, want := AliveReply(0).Encode(), unhex(t, "ff 8a 01 01 01 0f 00 02 00 79"); !bytes.Equal(got, want) {
		t.Errorf("AliveReply(0) = %x, want %x", got, want)
	}
	req := Frame{Hdr: hdrAliveReq, Cmd: 0x0f, Payload: []byte{0}}
	if !IsAliveRequest(req) || IsAliveRequest(Frame{Hdr: hdrPoll, Cmd: 0x0f}) {
		t.Error("IsAliveRequest")
	}
}

func TestAnswersAliveRequests(t *testing.T) {
	a, b := net.Pipe()
	d := New(Config{Port: a, Profile: []Periodic{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	req := Frame{Hdr: hdrAliveReq, Cmd: 0x0f, Payload: []byte{0}}.Encode()
	var dec Decoder
	buf := make([]byte, 64)
	for want := byte(0); want < 3; want++ {
		if _, err := b.Write(req); err != nil {
			t.Fatal(err)
		}
		b.SetReadDeadline(time.Now().Add(time.Second))
		n, err := b.Read(buf)
		if err != nil {
			t.Fatalf("no reply: %v", err)
		}
		fs := dec.Write(buf[:n])
		if len(fs) != 1 || fs[0].Hdr != hdrAliveReply || fs[0].Cmd != 0x0f || len(fs[0].Payload) != 1 || fs[0].Payload[0] != want {
			t.Fatalf("reply %d: %v", want, fs)
		}
	}
	deadline := time.Now().Add(time.Second)
	for d.Stats().AliveTx != 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s := d.Stats(); s.AliveRx != 3 || s.AliveTx != 3 {
		t.Errorf("alive counters %d/%d, want 3/3", s.AliveRx, s.AliveTx)
	}
	cancel()
	<-done
}

func TestRunSendsAndReceives(t *testing.T) {
	a, b := net.Pipe()
	now := time.Date(2026, 10, 16, 20, 1, 16, 0, time.UTC)
	seen := make(chan Frame, 8)
	d := New(Config{
		Port:    a,
		Profile: []Periodic{{Name: "poll", Every: 5 * time.Millisecond, Build: DefaultProfile()[0].Build}},
		Now:     func() time.Time { return now },
		OnFrame: func(f Frame) { seen <- f },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// The micom end: read what the daemon sends, send a countdown frame.
	go func() {
		b.Write(unhex(t, captured[5]))
	}()
	var dec Decoder
	buf := make([]byte, 256)
	var polls int
	deadline := time.After(2 * time.Second)
	for polls < 2 {
		b.SetReadDeadline(time.Now().Add(time.Second))
		n, err := b.Read(buf)
		if err != nil {
			t.Fatalf("micom end read: %v", err)
		}
		for _, f := range dec.Write(buf[:n]) {
			if f.Cmd == 0x5c {
				polls++
			}
		}
		select {
		case <-deadline:
			t.Fatal("no polls")
		default:
		}
	}
	select {
	case f := <-seen:
		if f.Cmd != 0x02 {
			t.Errorf("OnFrame got %v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("the countdown frame was not delivered")
	}
	if c := d.Stats().Countdown; c != 0x54 {
		t.Errorf("Countdown = %#x, want 0x54", c)
	}
	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Errorf("Run = %v", err)
	}
}
