package micom

import (
	"encoding/json"
	"testing"
	"time"
)

// The EV6's cluster messages: input lines and the frames they send, whole
// and with their checksums. Every frame sent also reads back (DecodeInput)
// into an input that sends it again.
func TestEV6Messages(t *testing.T) {
	v := ev6(t)
	const (
		ack         = "ff 8f 0d 01 0d 03 00 0a ff ff ff 0e 03 ff ff 03 ff 76" // captured
		subInfoCode = "ff 8f 0d 01 0d 03 00 0a ff ff ff 3f 03 ff ff 03 ff 47" // -code-profile's tbt-sub-info
		turnRight   = `{"msg":"tbt-turn","maneuver":"turn_right","direction":90,"distance":0,"distance_tenths":3,"unit":"km","bar":30}`
	)
	for _, tc := range []struct {
		name   string
		lines  []string
		frames []string
	}{
		{"turn right in 0.3 km (the report's worked example)", []string{turnRight},
			[]string{"ff 8f 0d 01 0d 04 00 09 43 ff ff 0c 00 00 10 33 10"}},
		{"the compass of the captured ack", []string{`{"msg":"cluster-sub-info","compass":105}`}, []string{ack}},
		{"no compass", []string{`{"msg":"cluster-sub-info","compass":null}`}, []string{subInfoCode}},
		{"the compass of the 2026-11-07 strace", []string{`{"msg":"cluster-sub-info","compass":217.5}`},
			[]string{"ff 8f 0d 01 0d 03 00 0a ff ff ff 1d 03 ff ff 03 ff 65"}},
		{"route summary", []string{`{"msg":"cluster-sub-info","remaining_unit":"km","remaining_tenths":0,"eta_hour":5,"eta_minute":8,"compass":105,"time_type":1,"remaining":50,"time_format":2}`},
			[]string{"ff 8f 0d 01 0d 03 00 0a 10 05 08 0e 01 00 32 02 ff a5"}},
		{"next point", []string{`{"msg":"tbt-points","p1_unit":"km","p1_tenths":0,"p1_distance":200}`},
			[]string{"ff 8f 0d 01 0d 02 00 0a 10 00 c8 ff ff ff ff ff ff a1"}},
		{"guiding", []string{`{"msg":"tbt-guiding","guiding":true}`}, []string{"ff 8f 0d 01 0d 11 00 02 01 63"}},
		{"end of route", []string{turnRight, `{"msg":"cluster-sub-info","compass":105}`, `{"msg":"tbt-turn","end":true}`}, []string{
			"ff 8f 0d 01 0d 04 00 09 43 ff ff 0c 00 00 10 33 10",
			ack,
			"ff 8f 0d 01 0d 04 00 09 00 ff ff 00 ff ff f0 f0 7c",
			"ff 8f 0d 01 0d 02 00 0a ff ff ff ff ff ff ff ff ff 86",
			ack, // the compass kept
			"ff 8f 0d 01 0d 11 00 02 00 62",
		}},
	} {
		var sent []Frame
		send := func(f Frame) error { sent = append(sent, f); return nil }
		now := func() time.Time { return time.Unix(0, 0) }
		all := InputGates{Inferred: true, Persistent: true}
		e := newInputEngine(v, all, send, func([]string, bool) {}, now, t.Logf)
		for _, l := range tc.lines {
			if err := e.handle([]byte(l)); err != nil {
				t.Errorf("%s: %s: %v", tc.name, l, err)
			}
		}
		if len(sent) != len(tc.frames) {
			t.Errorf("%s: sent %v, want %d frames", tc.name, sent, len(tc.frames))
			continue
		}
		for i, s := range tc.frames {
			want, err := ParseHexFrame(s)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !sameFrame(sent[i], want) {
				t.Errorf("%s: frame %d %x, want %s", tc.name, i, sent[i].Encode(), s)
			}
			name, in, ok := v.DecodeInput(sent[i])
			if !ok {
				t.Errorf("%s: %v does not read back", tc.name, sent[i])
				continue
			}
			in["msg"] = name
			line, _ := json.Marshal(in)
			var again []Frame
			e2 := newInputEngine(v, all, func(f Frame) error { again = append(again, f); return nil }, func([]string, bool) {}, now, t.Logf)
			if err := e2.handle(line); err != nil || len(again) != 1 || !sameFrame(again[0], sent[i]) {
				t.Errorf("%s: %s sends %v (%v), want %v", tc.name, line, again, err, sent[i])
			}
		}
	}
	// The cluster messages are code: refused without -allow-inferred.
	e := newInputEngine(v, InputGates{}, func(Frame) error { return nil }, func([]string, bool) {}, time.Now, t.Logf)
	if ok, _ := e.enabled(); len(ok) != 0 {
		t.Errorf("enabled without -allow-inferred: %v", ok)
	}
	// While cluster-sub-info is active it owns ack and the code entry that
	// replaces it.
	if got := v.owned(v.messages["cluster-sub-info"]); len(got) != 2 || got[0] != "ack" || got[1] != "tbt-sub-info" {
		t.Errorf("cluster-sub-info owns %v", got)
	}
}
