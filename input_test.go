package micom

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// A variant with to_micom messages on the audio header, and a profile entry
// q (with a code correction) that mode owns.
const inputVariant = `schema: 1
name: t
extends: hkmc-gen5w
enums: {mode: {media: 0x0e, radio: 1}}
profile:
  - {name: q, every: 1s, frame: "ff 85 02 01 02 01 00 03 0e 00 77", status: captured, ref: r}
  - {name: q-code, every: 1s, frame: "ff 85 02 01 02 01 00 03 01 00 78", opt_in: code, replaces: q, status: code, ref: r}
messages:
  - {name: mode, dir: to_micom, hdr: audio_tx, cmd: 0x01, length: 2, status: captured, ref: r,
     fields: [{name: mode, off: 0, enum: mode}, {name: level, off: 1, max: 9, invalid: 0xff}],
     every: 1s, owns: q, timeout: 5s, end: [{msg: mode, set: {mode: radio, level: null}}, {msg: beep, set: {a: 9}}]}
  - {name: beep, dir: to_micom, hdr: audio_tx, cmd: 0x41, length: 1, status: captured, ref: r,
     fields: [{name: a, off: 0}], on_change: false, every: 500ms, timeout: 2s}
  - {name: coded, dir: to_micom, hdr: audio_tx, cmd: 0x40, length: 1, status: code, ref: r, fields: [{name: a, off: 0}]}
  - {name: tied, dir: to_micom, hdr: audio_tx, cmd: 0x42, length: 1, status: captured, ref: r,
     fields: [{name: a, off: 0}], end: [{msg: coded, set: {a: 0}}]}
`

// recorder collects what an engine does: frames sent as "cmd:payload" in
// hex, and "own ..." / "resume ..." of profile entries.
type recorder struct{ log []string }

func (r *recorder) send(f Frame) error {
	r.log = append(r.log, fmt.Sprintf("%02x:%x", f.Cmd, f.Payload))
	return nil
}

func (r *recorder) own(names []string, on bool) {
	verb := "resume"
	if on {
		verb = "own"
	}
	r.log = append(r.log, verb+" "+strings.Join(names, ","))
}

// Each row is a sequence of steps at times after the start: an input line
// and what it gets back (ok, or a part of the error), or with no line a
// tick of the engine. The log is everything sent, owned and resumed.
func TestInput(t *testing.T) {
	v, err := loadVariantData([]byte(inputVariant), "test")
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		at          time.Duration
		line, reply string
	}
	for _, tc := range []struct {
		name     string
		inferred bool
		steps    []step
		log      []string
	}{
		{"merged, sent on change", false, []step{
			{0, `{"msg":"mode","mode":"media","level":5}`, "ok"},
			{0, `{"msg":"mode","level":6}`, "ok"},
			{0, `{"msg":"mode","level":6}`, "ok"},
			{0, `{"msg":"mode","level":null}`, "ok"},
		}, []string{"own q,q-code", "01:0e05", "01:0e06", "01:0eff"}},
		{"refused", false, []step{
			{0, `mode media`, "line is not a JSON object"},
			{0, `{"msg":"mode"} {}`, "line is not a JSON object"},
			{0, `{"mode":"media"}`, `"msg" is required`},
			{0, `{"msg":"nope"}`, `no message "nope"`},
			{0, `{"msg":"Variant code"}`, "Variant code: from_micom, not an input"},
			{0, `{"msg":"mode","mode":"media","speed":1}`, "mode: speed: no such field"},
			{0, `{"msg":"mode","mode":"tape"}`, `mode: mode: "tape" is not in enum mode`},
			{0, `{"msg":"mode","mode":14}`, "mode: mode: 14 is not a name of enum mode"},
			{0, `{"msg":"mode","mode":"media","level":12}`, "mode: level: 12 is above the maximum 9"},
			{0, `{"msg":"mode","mode":"media","level":2.5}`, "mode: level: 2.5 is not an integer"},
			{0, `{"msg":"mode","mode":"media","level":[1]}`, "mode: level: [1] is not a number"},
			{0, `{"msg":"mode","mode":null}`, "mode: mode: null: the field has no invalid marker"},
			{0, `{"msg":"mode","level":1}`, "mode: mode: required"},
			{0, `{"msg":"mode","end":true,"level":1}`, "mode: end is true and takes no fields"},
			{0, `{"msg":"mode","end":1}`, "mode: end is true and takes no fields"},
			{0, `{"msg":"coded","a":1}`, "coded: status code: needs -allow-inferred"},
			{0, `{"msg":"tied","a":1}`, "tied: its end sends coded (status code: needs -allow-inferred)"},
		}, nil},
		{"a refused input changes nothing", false, []step{
			{0, `{"msg":"mode","mode":"media","level":12}`, "above the maximum"},
			{0, `{"msg":"mode","level":1}`, "mode: required"},
			{0, `{"msg":"mode","mode":"radio"}`, "ok"},
		}, []string{"own q,q-code", "01:01ff"}},
		{"inferred", true, []step{
			{0, `{"msg":"coded","a":1}`, "ok"},
			{0, `{"msg":"tied","a":1}`, "ok"},
		}, []string{"40:01", "42:01"}},
		{"repeats, then the timeout ends it", false, []step{
			{0, `{"msg":"mode","mode":"media","level":5}`, "ok"},
			{500 * time.Millisecond, "", ""},
			{time.Second, "", ""},
			{1200 * time.Millisecond, `{"msg":"mode","level":6}`, "ok"},
			{2 * time.Second, "", ""},
			{2200 * time.Millisecond, "", ""},
			{6 * time.Second, "", ""},
			{6200 * time.Millisecond, "", ""},
			{8 * time.Second, "", ""},
		}, []string{"own q,q-code", "01:0e05", "01:0e05", "01:0e06", "01:0e06", "01:0e06", "01:01ff", "41:09", "resume q,q-code"}},
		{"end on request; an active target keeps the end's values", false, []step{
			{0, `{"msg":"beep","a":1}`, "ok"},
			{0, `{"msg":"mode","mode":"media"}`, "ok"},
			{100 * time.Millisecond, `{"msg":"mode","end":true}`, "ok"},
			{600 * time.Millisecond, "", ""},
			{3 * time.Second, "", ""},
		}, []string{"41:01", "own q,q-code", "01:0eff", "01:01ff", "41:09", "resume q,q-code", "41:09"}},
		{"end of an inactive message", false, []step{
			{0, `{"msg":"mode","end":true}`, "ok"},
		}, []string{"01:01ff", "41:09"}},
		{"on_change false: the newest goes at the next repeat", false, []step{
			{0, `{"msg":"beep","a":1}`, "ok"},
			{100 * time.Millisecond, `{"msg":"beep","a":2}`, "ok"},
			{500 * time.Millisecond, "", ""},
		}, []string{"41:01", "41:02"}},
	} {
		var r recorder
		start := time.Unix(1000, 0)
		now := start
		e := newInputEngine(v, tc.inferred, r.send, r.own, func() time.Time { return now }, t.Logf)
		for _, s := range tc.steps {
			now = start.Add(s.at)
			if s.line == "" {
				e.tick(now)
				continue
			}
			got := "ok"
			if err := e.handle([]byte(s.line)); err != nil {
				got = err.Error()
			}
			if !strings.Contains(got, s.reply) {
				t.Errorf("%s: %s: %q, want %q", tc.name, s.line, got, s.reply)
			}
		}
		if !slices.Equal(r.log, tc.log) {
			t.Errorf("%s:\n got %v\nwant %v", tc.name, r.log, tc.log)
		}
	}
}

// Through the socket and the daemon: while mode is active the profile entry
// it owns stays off the wire, and comes back when mode ends.
func TestServeInput(t *testing.T) {
	v, err := loadVariantData([]byte(inputVariant), "test")
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	frames := make(chan Frame, 64)
	go func() {
		var dec Decoder
		buf := make([]byte, 256)
		for {
			n, err := b.Read(buf)
			for _, f := range dec.Write(buf[:n]) {
				frames <- f
			}
			if err != nil {
				close(frames)
				return
			}
		}
	}()
	d := New(Config{Port: a, Variant: v, Profile: ScaledProfile(10, v.Profile())})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	p := tapPath(t)
	go ServeInput(ctx, d, p, false)
	var c net.Conn
	for i := 0; i < 200 && c == nil; i++ {
		c, _ = net.Dial("unix", p)
		time.Sleep(10 * time.Millisecond)
	}
	if c == nil {
		t.Fatal("no input socket")
	}
	defer c.Close()
	r := bufio.NewReader(c)
	ask := func(line, want string) {
		t.Helper()
		fmt.Fprintln(c, line)
		if got, _ := r.ReadString('\n'); strings.TrimSpace(got) != want {
			t.Errorf("%s: %q, want %q", line, got, want)
		}
	}
	// what lists the payloads received for d, in hex.
	what := func(d time.Duration) []string {
		var out []string
		for end := time.After(d); ; {
			select {
			case f := <-frames:
				out = append(out, fmt.Sprintf("%02x:%x", f.Cmd, f.Payload))
			case <-end:
				return out
			}
		}
	}
	const q, mode = "01:0e00", "01:0e05"
	if got := what(300 * time.Millisecond); !slices.Contains(got, q) {
		t.Fatalf("no profile frame: %v", got)
	}
	ask(`{"msg":"mode","mode":"media","level":5}`, `{"ok":true}`)
	got := what(400 * time.Millisecond)
	if i := slices.Index(got, mode); i < 0 || slices.Contains(got[i:], q) {
		t.Errorf("while mode is active: %v", got)
	}
	ask(`{"msg":"mode","level":12}`, `{"error":"mode: level: 12 is above the maximum 9"}`)
	ask(`{"msg":"mode","end":true}`, `{"ok":true}`)
	got = what(300 * time.Millisecond)
	if i := slices.Index(got, "41:09"); i < 1 || got[i-1] != "01:01ff" || !slices.Contains(got[i:], q) {
		t.Errorf("after the end: %v", got)
	}
}
