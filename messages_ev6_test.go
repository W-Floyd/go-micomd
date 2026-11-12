package micom

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// The EV6's to_micom messages: input lines and the frames they send, whole
// and with their checksums. A line "rx FRAME" is a frame received; "after
// D" the clock moving on D, then a tick; "INPUT => ERROR" an input refused
// with that error. Every frame sent also reads
// back (DecodeInput) into an input that encodes to it again.
func TestEV6Messages(t *testing.T) {
	v := ev6(t)
	const (
		ack         = "ff 8f 0d 01 0d 03 00 0a ff ff ff 0e 03 ff ff 03 ff 76" // captured
		subInfoCode = "ff 8f 0d 01 0d 03 00 0a ff ff ff 3f 03 ff ff 03 ff 47" // -code-profile's tbt-sub-info
		turnRight   = `{"msg":"tbt-turn","maneuver":"turn_right","direction":90,"distance":0,"distance_tenths":3,"unit":"km","bar":30}`
		ackOff      = `{"msg":"shutdown-ack","stage":"power_off_request"}`
		ackRail     = `{"msg":"shutdown-ack","stage":"rail_off"}`
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
		// Stock's claim of the pane (DiPO u.java): alive, then operating.
		{"claim", []string{`{"msg":"tbt-alive","alive":true}`, `{"msg":"nav-operation","status":"operating"}`},
			[]string{"ff 8f 0d 01 0d 0e 00 02 01 7c", "ff 8f 0d 01 0d 11 00 02 01 63"}},
		{"release", []string{`{"msg":"tbt-alive","alive":true}`, `{"msg":"nav-operation","status":"operating"}`, `{"msg":"tbt-alive","end":true}`, `{"msg":"nav-operation","end":true}`},
			[]string{"ff 8f 0d 01 0d 0e 00 02 01 7c", "ff 8f 0d 01 0d 11 00 02 01 63", "ff 8f 0d 01 0d 0e 00 02 00 7d", "ff 8f 0d 01 0d 11 00 02 00 62"}},
		{"night, then day", []string{`{"msg":"cluster-day-night","mode":"night"}`, `{"msg":"cluster-day-night","mode":"day"}`},
			[]string{"ff 8f 0d 01 0d 0f 00 02 01 7d", "ff 8f 0d 01 0d 0f 00 02 00 7c"}},
		{"no route: the compass pane", []string{`{"msg":"tbt-turn","maneuver":"no_route_guide","side_street":0,"distance":null,"unit":"m","bar":null,"distance_tenths":0}`},
			[]string{"ff 8f 0d 01 0d 04 00 09 08 00 00 00 ff ff 00 f0 84"}},
		// GPM's releaseGuidance, bytes as stock; the pane stays claimed.
		{"end of route", []string{turnRight, `{"msg":"cluster-sub-info","compass":105}`, `{"msg":"tbt-turn","end":true}`}, []string{
			"ff 8f 0d 01 0d 04 00 09 43 ff ff 0c 00 00 10 33 10",
			ack,
			"ff 8f 0d 01 0d 04 00 09 00 00 00 00 ff ff 00 f0 8c",
			"ff 8f 0d 01 0d 02 00 0a ff ff ff ff ff ff ff ff ff 86",
			"ff 8f 0d 01 0d 03 00 0a 00 00 00 0e 00 ff ff 00 00 76", // the compass kept
		}},
		// The micom firmware's frames, as the emulated firmware sent them.
		{"backlight on", []string{`{"msg":"backlight","on":true}`}, []string{"ff 8c 08 01 08 02 00 02 01 73"}},
		{"backlight off", []string{`{"msg":"backlight","on":false}`}, []string{"ff 8c 08 01 08 02 00 02 00 72"}},
		{"shutdown acks, each only in its power state", []string{
			ackOff + " => requires Power state in [power_off_request]: none received yet",
			ackRail + " => requires Power state in [rail_off]: none received yet",
			"rx ff 01 8a 01 81 08 00 03 01 05 fb", // ACC on
			ackOff + " => it is acc_on",
			"rx ff 01 8a 01 81 08 00 04 00 04 00 fc",
			ackOff + " => it is acc_off_hold",
			"rx ff 01 8a 01 81 08 00 04 00 03 00 fb",
			ackOff + " => it is bus_sleep",
			ackRail + " => it is bus_sleep",
			"rx ff 01 8a 01 81 08 00 04 00 01 00 f9",
			ackRail + " => it is power_off_request",
			ackOff, ackOff, // an ack repeated is still sent
			"rx ff 01 8a 01 81 08 00 04 00 00 00 f8",
			ackOff + " => it is rail_off",
			ackRail,
			"rx ff 01 8a 01 81 08 00 03 01 05 fb", // ACC back on
			ackOff + " => it is acc_on",
			ackRail + " => it is acc_on",
		}, []string{"ff 8a 01 01 01 09 00 02 01 7e", "ff 8a 01 01 01 09 00 02 01 7e", "ff 8a 01 01 01 09 00 02 02 7d"}},
		{"ACC back on before the ack", []string{
			"rx ff 01 8a 01 81 08 00 04 00 01 00 f9",
			"rx ff 01 8a 01 81 08 00 03 01 05 fb",
			ackOff + " => it is acc_on",
		}, nil},
		{"dim mode manual", []string{`{"msg":"backlight-dim-mode","mode":"manual"}`}, []string{"ff 8c 08 01 08 01 00 02 03 72"}},
		{"night level 5", []string{`{"msg":"backlight-period-level","period":"night","level":5}`}, []string{"ff 8c 08 01 08 07 00 03 02 05 71"}},
		{"manual level 500", []string{`{"msg":"backlight-manual-level","level":500}`}, []string{"ff 8c 08 01 08 1a 00 03 01 f4 9e"}},
		// Launchpad's start and stop, through stock's HEVService packers; every
		// repeat goes out.
		{"battery conditioning start", []string{
			`{"msg":"batt-cond-route-distance","km":9}`, `{"msg":"batt-cond-remain-time","minutes":18}`,
			`{"msg":"batt-cond-ev-station","flag":2,"type":3}`, `{"msg":"batt-cond-ev-station","flag":7,"type":7}`,
			`{"msg":"batt-cond-route-distance","km":9}`,
		}, []string{
			"ff 88 0c 01 0c 66 00 03 00 09 1a", "ff 88 0c 01 0c 65 00 03 00 12 02",
			"ff 88 0c 01 0c 68 00 03 02 03 1c", "ff 88 0c 01 0c 68 00 03 07 07 1d",
			"ff 88 0c 01 0c 66 00 03 00 09 1a",
		}},
		// 1 degree north and west, as stock's arc-seconds.
		{"cluster position", []string{
			`{"msg":"cluster-position","lat_arcsec":3600,"lon_arcsec":-3600}`,
			`{"msg":"cluster-position","lat_arcsec":324001,"lon_arcsec":0} => above the maximum 324000`,
		}, []string{"ff c3 4d 00 4d 17 00 09 08 05 7e 3f 07 fa 81 bf ad"}},
		{"battery conditioning stop", []string{
			`{"msg":"batt-cond-route-distance","km":2047}`, `{"msg":"batt-cond-remain-time","minutes":1023}`,
			`{"msg":"batt-cond-ev-station","flag":0,"type":0}`,
			`{"msg":"batt-cond-route-distance","km":2048} => above the maximum 2047`,
		}, []string{"ff 88 0c 01 0c 66 00 03 07 ff eb", "ff 88 0c 01 0c 65 00 03 03 ff ec", "ff 88 0c 01 0c 68 00 03 00 00 1d"}},
		// Stock's EV settings toggle: the mode, then default 120 ms on.
		{"battery conditioning mode on, then off", []string{
			`{"msg":"batt-cond-mode","mode":"on"}`, "after 50ms", "after 100ms",
			`{"msg":"batt-cond-mode","mode":"off"}`, "after 150ms",
			`{"msg":"batt-cond-mode","on":true} => on: no such field`,
		}, []string{"ff 88 0c 01 0c 62 00 02 02 14", "ff 88 0c 01 0c 62 00 02 00 16", "ff 88 0c 01 0c 62 00 02 01 17", "ff 88 0c 01 0c 62 00 02 00 16"}},
		// Stock's EV settings under QEMU (2026-11-12): one limit per frame,
		// winter mode 3 and the other limits 0xff; the current, then 0. An
		// input keeps the fields it does not name, so the second gives null.
		{"charge limits as stock sends them", []string{
			`{"msg":"ev-charge-limits","ac_limit":90}`, `{"msg":"ev-charge-limits","ac_limit":null,"dc_limit":50}`,
			`{"msg":"ev-charge-limits","ac_limit":45} => below the minimum 50`,
		}, []string{"ff 88 0c 01 0c 18 00 08 00 00 c0 00 b4 ff ff 12", "ff 88 0c 01 0c 18 00 08 00 00 c0 00 ff 64 ff c2"}},
		{"AC charging current 60 %, then the default", []string{`{"msg":"ev-charging-current","current":"minimum"}`, "after 150ms"},
			[]string{"ff 88 0c 01 0c 5b 00 02 03 2c", "ff 88 0c 01 0c 5b 00 02 00 2f"}},
	} {
		var sent []Frame
		send := func(f Frame) error { sent = append(sent, f); return nil }
		clock := time.Unix(0, 0)
		now := func() time.Time { return clock }
		all := InputGates{Inferred: true, Persistent: true, Dangerous: true}
		e := newInputEngine(v, all, send, func([]string, bool) {}, now, t.Logf)
		for _, l := range tc.lines {
			if s, ok := strings.CutPrefix(l, "after "); ok {
				d, err := time.ParseDuration(s)
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				clock = clock.Add(d)
				e.tick(clock)
				continue
			}
			if s, ok := strings.CutPrefix(l, "rx "); ok {
				f, err := ParseHexFrame(s)
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				e.observe(f)
				continue
			}
			l, want, _ := strings.Cut(l, " => ")
			got := ""
			if err := e.handle([]byte(l)); err != nil {
				got = err.Error()
			}
			if want == "" && got != "" || !strings.Contains(got, want) {
				t.Errorf("%s: %s: %q, want %q", tc.name, l, got, want)
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
			// Through JSON, as a client would send it back; not through an
			// engine, whose requires would need the state replayed.
			line, _ := json.Marshal(in)
			var back map[string]any
			dec := json.NewDecoder(bytes.NewReader(line))
			dec.UseNumber()
			dec.Decode(&back)
			m := v.messages[name]
			vals, err := m.parse(back)
			var again Frame
			if err == nil {
				again, err = m.encode(vals, nil)
			}
			if err != nil || !sameFrame(again, sent[i]) {
				t.Errorf("%s: %s %s encodes to %v (%v), want %v", tc.name, name, line, again, err, sent[i])
			}
		}
	}
	// All are code: refused without -allow-inferred. The EEPROM writers also
	// need -allow-persistent, the dangerous -allow-dangerous(-msg).
	for _, tc := range []struct {
		g    InputGates
		want []string
	}{
		{InputGates{}, nil},
		{InputGates{Persistent: true}, nil},
		// -allow-dangerous-msg: the named only, and still -allow-inferred.
		{InputGates{DangerousMsgs: []string{"ev-departures", "ev-off-peak"}}, nil},
		{InputGates{Inferred: true, DangerousMsgs: []string{"ev-departures", "ev-off-peak"}}, []string{"backlight", "batt-cond-ev-station", "batt-cond-mode", "batt-cond-remain-time", "batt-cond-route-distance", "cluster-day-night", "cluster-position", "cluster-sub-info", "ev-departures", "ev-off-peak", "nav-operation", "shutdown-ack", "tbt-alive", "tbt-points", "tbt-turn"}},
		{InputGates{Inferred: true}, []string{"backlight", "batt-cond-ev-station", "batt-cond-mode", "batt-cond-remain-time", "batt-cond-route-distance", "cluster-day-night", "cluster-position", "cluster-sub-info", "nav-operation", "shutdown-ack", "tbt-alive", "tbt-points", "tbt-turn"}},
		{InputGates{Inferred: true, Persistent: true}, []string{"backlight", "backlight-dim-mode", "backlight-manual-level", "backlight-period-level", "batt-cond-ev-station", "batt-cond-mode", "batt-cond-remain-time", "batt-cond-route-distance", "cluster-day-night", "cluster-position", "cluster-sub-info", "nav-operation", "shutdown-ack", "tbt-alive", "tbt-points", "tbt-turn"}},
	} {
		e := newInputEngine(v, tc.g, func(Frame) error { return nil }, func([]string, bool) {}, time.Now, t.Logf)
		if ok, _ := e.enabled(); !slices.Equal(ok, tc.want) {
			t.Errorf("%+v: enabled %v, want %v", tc.g, ok, tc.want)
		}
	}
	// While cluster-sub-info is active it owns ack and the code entry that
	// replaces it.
	if got := v.owned(v.messages["cluster-sub-info"]); len(got) != 2 || got[0] != "ack" || got[1] != "tbt-sub-info" {
		t.Errorf("cluster-sub-info owns %v", got)
	}
}
