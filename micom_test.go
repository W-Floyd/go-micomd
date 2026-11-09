package micom

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync/atomic"
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
		// Not captured: ack with the compass as 3f, from stock code.
		"tbt-sub-info": "ff 8f 0d 01 0d 03 00 0a ff ff ff 3f 03 ff ff 03 ff 47",
	}
	code := defaultVariant().CodeProfile()
	for _, p := range slices.Concat(DefaultProfile(), ClockProfile(), code) {
		w, ok := want[p.Name]
		if !ok {
			t.Fatalf("no capture for %s", p.Name)
		}
		if got := p.Build(now).Encode(); !bytes.Equal(got, unhex(t, w)) {
			t.Errorf("%s: built %x, captured %s", p.Name, got, w)
		}
	}
	// -code-profile sends the corrected frame instead of, not beside, ack.
	names := []string{}
	for _, p := range code {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"poll", "tbt-sub-info", "status"}) {
		t.Errorf("code profile %v", names)
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
	// Frames seen live on the stage, 2026-10-16; the variant-code replies are
	// the EV6 and a Korean micom build's, in emulation.
	for _, c := range []struct{ hdr, cmd, payload, want string }{
		{"018a0181", "17", "cc01a2180080 00 656e 00000000", "Vehicle=cvev Country=us Navigation=1 Modem=0 Internal amp=1 Rear monitor=0 Radio region=khz_10_200 DMB=0"},
		{"018a0181", "17", "cc01a2180080 00 656e 00000000", "External amp=actune Camera=avm"},
		{"018a0181", "17", "cc01a2180080 00 656e 00000000", "Telematics=0 Diranarom=1 DAB=0 HU type=daudio Camera mode=analog"},
		{"018a0181", "17", "cc01a2180080 00 656e 00000000", "RDS=0 DVD region=worldwide Language=en CPU variant 9=0 CPU variant 10=0 CPU variant 11=0 ADM=off"},
		{"018a0181", "17", "c800a0180080 00 656e 00000080", "Vehicle=200 Country=korea"},
		{"018a0181", "17", "c800a0180080 00 656e 00000080", "ADM=128"},
		{"018a0181", "17", "c800a0180080 00 6b6f 00000000", "Language=ko"},
		{"018a0181", "17", "c800a0180080 00 0000 00000000", "Language=n/a"},
		{"0c88018c", "02", "5700f5", "Climate power draw=245 W"},
		{"0c88018c", "5c", "15ffffffffffffffffff01b1000000000000000000000400ff00000000ff", "Climate total=433 W"},
		{"0c88018c", "5c", "15ffffffffffffffffff01b1000000000000000000000400ff00000000ff", "Climate power=255 W"},
		{"0c88018c", "5c", "15ffffffffffffffffff01b1000000000000000000000400ff00000000ff", "AVN motor power=n/a"},
		{"0c88018c", "55", "4d749148ff550500", "Departure 1 hour=8"},
		{"0c88018c", "4f", "2d3a200100000000", "Charger ready bits=0"},
		// The micom firmware's power and display frames, in emulation.
		{"018a0181", "08", "000400", "Power state: Power state=acc_off_hold"},
		{"018a0181", "08", "000300", "Power state: Power state=bus_sleep"},
		{"018a0181", "08", "000100", "Power state: Power state=power_off_request"},
		{"018a0181", "08", "000000", "Power state: Power state=rail_off"},
		{"018a0181", "08", "0105", "Power state (ACC on): Power state=acc_on"},
		{"018a0181", "33", "01", "ACC edge: ACC=off"},
		{"018a0181", "33", "02", "ACC edge: ACC=on"},
		{"018a0181", "0a", "01", "CAN rheostat selected=1"},
		{"018a0181", "0b", "00", "CAN rheostat active=0"},
		{"018a0181", "0c", "00", "Flag 0x810c=0"},
		{"018a0181", "0d", "00", "Illumination: Illumination=night"},
		{"018a0181", "0d", "01", "Illumination=day"},
		{"018a0181", "0d", "05", "Illumination=5"},
		// The EV6's, captured at night, its first 0x13 bytes.
		{"018a0181", "01", "0100000000010eca020519222e06544b4b1500", "Boot record: Boot reason=1 Illumination=night Dim mode=auto"},
		{"018a0181", "45", "0a", "Rheostat level=10"},
		{"018a0181", "9b", "0000000000000058", "12 V battery charge=88 %"},
		{"44c100c4", "14", "57", "12 V battery charge (body)=87 %"},
		{"088c0188", "0c", "02", "Display 0x880C=2"},
		{"088c0188", "10", "14", "Rheostat: Rheostat step=20"},
		{"088c0188", "13", "32", "Display 0x8813=50"},
		{"088c0188", "1b", "00c8", "Current brightness=200"},
		{"088c0188", "1b", "00c80123", "Current brightness=200"},
		// A drive against an OBD2 logger.
		{"44c100c4", "20", "0086", "Vehicle speed: Vehicle speed=107.826048 km/h Vehicle speed (mph)=67 mph"},
		{"44c100c4", "20", "0085", "Vehicle speed (mph)=66.5 mph"},
		{"44c100c4", "20", "0000", "Vehicle speed=0 km/h"},
		{"0d8f018d", "47", "9b166f54", "Battery: State of charge=77.5 % Battery minimum temperature=22 °C Battery remaining energy=57 kWh"},
		{"0d8f018d", "47", "c8fbffff", "State of charge=100 % Battery minimum temperature=-5 °C Battery remaining energy=131.07 kWh"},
		{"0d8f018d", "46", "02", "Cruise control: Cruise control=active"},
		{"0d8f018d", "46", "00", "Cruise control=inactive"},
		// Against the car's cluster.
		{"0d8f018d", "43", "02007500bc", "Range: Range=188 km Range (mi)=117 mi"},
		{"44c100c4", "22", "06c40c", "Odometer: Odometer=44340.4 km"},
		{"43c200c3", "02", "5e", "Outdoor temperature: Outdoor temperature=12.222222 C Outdoor temperature (F)=54 F"},
		{"43c200c3", "02", "00", "Outdoor temperature=-40 C Outdoor temperature (F)=-40 F"},
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

// The table says which variant runs and why, and gives each frame kind one
// line: key, name, count, age, payload, changed bytes.
func TestSnapshotTable(t *testing.T) {
	counter := FrameCount{Key: "0d8f018d 2d", Count: 3, AgeMs: 1500, Payload: "060007",
		Diffs: []ByteDiff{{Idx: 0, From: 3, To: 6, Changes: 3}, {Idx: 2, From: 0, To: 7, Changes: 1}}}
	climate := FrameCount{Key: "0c88018c 02", Name: "Status flag", Count: 40, AgeMs: 100, Payload: "5700ff", Diffs: []ByteDiff{}}
	unmarked := FrameCount{Key: "0d8f018d 2d", Count: 3, AgeMs: 0, Payload: "06",
		Diffs: []ByteDiff{{Idx: 0, From: -1, To: 6, Changes: 2}}}
	ev6ID := Identity{"vehicle_info": 0xcc, "country_info": 1}
	for _, tc := range []struct {
		name string
		s    Snapshot
		want []string
	}{
		{"marked", Snapshot{Up: true, RxFrames: 43, TxFrames: 9, AliveRx: 4, AliveTx: 4, LastAliveAgeMs: 500, MarkedAgeMs: 5000,
			Frames:  []FrameCount{climate, counter},
			Variant: VariantChoice{Name: DefaultVariantName, Source: "cmdline", Cmdline: ev6ID, Matched: DefaultVariantName}},
			[]string{
				"variant kia-ev6-gen5w (kernel command line country_info=0x1 vehicle_info=0xcc)",
				"link up rx 43 tx 9 alive 4/4, last 0.5s ago badsum 0 dropped 0",
				"mark 5.0s ago",
				"KEY NAME COUNT AGE PAYLOAD CHANGED",
				"0c88018c 02 Status flag 40 0.1s 5700ff",
				"0d8f018d 2d - 3 1.5s 060007 0:03->06×3 2:00->07×1",
			}},
		{"no mark, default", Snapshot{LastAliveAgeMs: -1, MarkedAgeMs: -1, Frames: []FrameCount{unmarked},
			Variant: VariantChoice{Name: DefaultVariantName, Source: "default"}},
			[]string{
				"variant kia-ev6-gen5w (default: no variant matches (no identity keys))",
				"link down rx 0 tx 0 alive 0/0, last never badsum 0 dropped 0",
				"mark none: changes since the start",
				"0d8f018d 2d - 3 0.0s 06 0:??->06×2",
			}},
		{"flag over the command line", Snapshot{MarkedAgeMs: -1, Variant: VariantChoice{Name: "hkmc-gen5w", Source: "flag", Cmdline: ev6ID, Matched: DefaultVariantName}},
			[]string{"variant hkmc-gen5w (-variant; the kernel command line matches kia-ev6-gen5w)"}},
		{"keepalive only", Snapshot{MarkedAgeMs: -1, Variant: VariantChoice{Name: "none", Source: "file", File: "/x.yaml", Note: "open /x.yaml: no such file"}},
			[]string{"variant none (from /x.yaml; open /x.yaml: no such file)"}},
		{"micom record", Snapshot{MarkedAgeMs: -1, Variant: VariantChoice{Name: DefaultVariantName, Source: "default",
			Reply: &VariantReply{Payload: "cc01", AgeMs: 2000, Fields: []Field{{Name: "Vehicle", Value: 0xcc, Valid: true}}, Identity: ev6ID, Matched: DefaultVariantName, Verdict: "the micom reports it"}}},
			[]string{"micom the micom reports it", "record 2.0s ago: cc01 Vehicle=204"}},
		{"recording", Snapshot{MarkedAgeMs: -1, Recorder: &RecorderState{Path: "/var/log/micom.jsonl", Bytes: 4096, Rotations: 2, Dropped: 1}},
			[]string{"recording /var/log/micom.jsonl 4096 bytes rotations 2 dropped 1"}},
		{"recording stopped", Snapshot{MarkedAgeMs: -1, Recorder: &RecorderState{Path: "/r", Error: "disk full"}},
			[]string{"recording /r 0 bytes rotations 0 dropped 0 stopped: disk full"}},
	} {
		var b bytes.Buffer
		if err := tc.s.WriteTable(&b); err != nil {
			t.Fatal(err)
		}
		lines := map[string]bool{}
		for _, l := range strings.Split(b.String(), "\n") {
			lines[strings.Join(strings.Fields(l), " ")] = true
		}
		for _, w := range tc.want {
			if !lines[w] {
				t.Errorf("%s: no line %q in\n%s", tc.name, w, b.String())
			}
		}
	}
}

// The identify request goes out once the link is up, and again until the
// variant record arrives, a bounded number of times.
func TestAskVariant(t *testing.T) {
	req, err := ParseHexFrame("ff 8a 01 01 01 17 00 01 62")
	if err != nil {
		t.Fatal(err)
	}
	record := Frame{Hdr: hdrAliveReq, Cmd: 0x17, Payload: unhex(t, "cc 01 00 00 00 00 00 65 6e 00 00 00 00")}
	for _, tc := range []struct {
		name     string
		linkUp   bool
		answerAt int // the request the micom answers, 0 for none
		want     int
		log      string
	}{
		{"answered at once", true, 1, 1, ""},
		{"answered on a retry", true, 2, 2, ""},
		{"never answered", true, 0, 3, "no variant record after 3 requests"},
		{"link never up", false, 1, 0, ""},
	} {
		a, b := net.Pipe()
		var logs syncBuffer
		d := New(Config{Port: a, Profile: []Periodic{}, Allow: []Frame{req}, Logf: func(f string, args ...any) { fmt.Fprintf(&logs, f+"\n", args...) }})
		if tc.linkUp {
			d.lastRx = time.Now()
		}
		var sent atomic.Int32
		go func() {
			var dec Decoder
			buf := make([]byte, 64)
			for {
				n, err := b.Read(buf)
				if err != nil {
					return
				}
				for _, f := range dec.Write(buf[:n]) {
					if sameFrame(f, req) && int(sent.Add(1)) == tc.answerAt {
						d.telem.add(record, time.Now())
					}
				}
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if !tc.linkUp {
			cancel()
		}
		d.askVariant(ctx, req, 3, 50*time.Millisecond)
		cancel()
		b.Close()
		if got := int(sent.Load()); got != tc.want {
			t.Errorf("%s: %d requests, want %d", tc.name, got, tc.want)
		}
		if !strings.Contains(logs.String(), tc.log) {
			t.Errorf("%s: logged %q, want %q", tc.name, logs.String(), tc.log)
		}
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
