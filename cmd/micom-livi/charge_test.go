package main

import (
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	micom "github.com/W-Floyd/go-micomd"
)

const (
	acAt100    = "2100c8" // item 0x21
	dcAt100    = "2200c8" // item 0x22
	obcReady   = "128e510100000000"
	obcUnready = "128e510000000000" // 0x8C4F [3] b1:0
)

// liveBulk is the 0x8C01 reply on the car, 2026-11-12: limits AC and DC 100 %.
const liveBulk = "003cffff003c02ff02001d000000ffffff030000330c9e060405a0ffff014f021201002bff32ff0001010006d78101a001a0c8c81e0000ff0000ffff00610001ff0000209fffffffffffff972cffffffffffffff0101000108010100000001ffff00ba0000000000000000000000010028ffff002e00ff0500000000ff0000000047010000000000000000580a02000000000000000000000000000000000000000000000520ffffffffffff7fff00ff00ffffffffff"

// chargeFrame is an item (cmd 0x02), 0x8C4F or 0x8C01 frame from its hex
// payload.
func chargeFrame(t *testing.T, p string) micom.Frame {
	t.Helper()
	b, err := hex.DecodeString(p)
	if err != nil {
		t.Fatal(err)
	}
	cmd := byte(0x02)
	switch {
	case len(b) == 8:
		cmd = 0x4f
	case len(b) > 8:
		cmd = 0x01
	}
	return micom.Frame{Hdr: schedHdr, Cmd: cmd, Payload: b}
}

// The AC charging current reaches LIVI as stock shows it.
func TestChargeTelemetry(t *testing.T) {
	v, err := micom.DefaultVariant()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		frames []string
		want   any
	}{
		{nil, nil},
		{[]string{acAt100, obcReady}, nil},
		{[]string{"510000"}, 100.0},
		{[]string{"510001"}, 100.0},
		{[]string{"510002"}, 90.0},
		{[]string{"510003"}, 60.0},
	} {
		c := newCharge(v)
		for _, p := range tc.frames {
			c.take(chargeFrame(t, p))
		}
		if got := c.telemetry()["acChargingCurrentPct"]; got != tc.want {
			t.Errorf("%v: %v, want %v", tc.frames, got, tc.want)
		}
	}
}

// Each row is one change from LIVI, through the runner: what it sends to
// micomd, then how it reports to LIVI. With car the car takes each write and
// reports it back as its item; without, it says nothing.
func TestChargeEdits(t *testing.T) {
	v, err := micom.DefaultVariant()
	if err != nil {
		t.Fatal(err)
	}
	held := []string{acAt100, dcAt100, obcReady}
	parked := func() (bool, string) { return true, "" }
	items := map[string]byte{fLimitAC: 0x21, fLimitDC: 0x22, fCurrent: 0x51}
	for _, tc := range []struct {
		name   string
		held   []string
		writes bool
		parked func() (bool, string)
		set    string
		car    bool
		lines  []string
		states []string // evChargeWrite's state: why
	}{
		{"AC 80 %: AC alone, the rest left to the car", held, true, parked, `{"chargeLimitAcPct":80}`, true,
			[]string{`{"ac_limit":80,"msg":"ev-charge-limits"}`}, []string{"sending", "confirmed"}},
		{"DC 50 %", held, true, parked, `{"chargeLimitDcPct":50}`, true,
			[]string{`{"dc_limit":50,"msg":"ev-charge-limits"}`}, []string{"sending", "confirmed"}},
		{"both: a frame each, AC first", held, true, parked, `{"chargeLimitDcPct":90,"chargeLimitAcPct":70}`, true,
			[]string{`{"ac_limit":70,"msg":"ev-charge-limits"}`, `{"dc_limit":90,"msg":"ev-charge-limits"}`}, []string{"sending", "confirmed"}},
		{"the value the car holds: nothing sent", held, true, parked, `{"chargeLimitAcPct":100}`, true,
			nil, []string{"confirmed"}},
		{"the limits from the start-up read", []string{liveBulk, obcReady}, true, parked, `{"chargeLimitAcPct":100,"chargeLimitDcPct":80}`, true,
			[]string{`{"dc_limit":80,"msg":"ev-charge-limits"}`}, []string{"sending", "confirmed"}},
		{"one held, one not: only the change", held, true, parked, `{"chargeLimitAcPct":100,"chargeLimitDcPct":80}`, true,
			[]string{`{"dc_limit":80,"msg":"ev-charge-limits"}`}, []string{"sending", "confirmed"}},
		{"a limit the car has not reported", []string{dcAt100}, true, parked, `{"chargeLimitAcPct":80}`, true,
			nil, []string{"failed: the car has not reported its AC limit yet"}},
		{"off the page's steps", held, true, parked, `{"chargeLimitAcPct":75,"chargeLimitDcPct":40}`, true,
			nil, []string{"failed: AC limit 75 %: 50 to 100 in tens; DC limit 40 %: 50 to 100 in tens"}},
		{"current 90 %, not reported yet: sent", held, true, parked, `{"chargingCurrentPct":90}`, true,
			[]string{`{"current":"reduced","msg":"ev-charging-current"}`}, []string{"sending", "confirmed"}},
		{"current 60 % from 100 %", append([]string{"510001"}, held...), true, parked, `{"chargingCurrentPct":60}`, true,
			[]string{`{"current":"minimum","msg":"ev-charging-current"}`}, []string{"sending", "confirmed"}},
		{"current 100 %, the car's 0 being 100 %", append([]string{"510000"}, held...), true, parked, `{"chargingCurrentPct":100}`, true,
			nil, []string{"confirmed"}},
		{"current while the charger is not ready", []string{obcUnready}, true, parked, `{"chargingCurrentPct":90}`, true,
			nil, []string{"failed: the on-board charger is not ready"}},
		{"current off the choices", held, true, parked, `{"chargingCurrentPct":80}`, true,
			nil, []string{"failed: charging current 80 %: 100, 90 or 60"}},
		{"the car does not report it back", held, true, parked, `{"chargeLimitAcPct":80}`, false,
			[]string{`{"ac_limit":80,"msg":"ev-charge-limits"}`}, []string{"sending", "failed: the car did not report it back within 10s (Charge limit, AC)"}},
		{"writes off", held, false, parked, `{"chargeLimitAcPct":80}`, true,
			nil, []string{"failed: writes are off: micom-livi runs without -ev-charge-writes"}},
		{"not parked", held, true, func() (bool, string) { return false, "the car is in D, not parked" }, `{"chargeLimitAcPct":80}`, true,
			nil, []string{"failed: the car is in D, not parked"}},
		{"nothing", held, true, parked, `{}`, true, nil, []string{"failed: nothing to change"}},
		{"winter mode is not LIVI's", held, true, parked, `{"winterMode":1}`, true,
			nil, []string{`failed: the edit: json: unknown field "winterMode"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCharge(v)
			for _, p := range tc.held {
				c.take(chargeFrame(t, p))
			}
			clock := time.Unix(1_800_000_000, 0)
			var lines, states []string
			r := &writeRunner{what: "ev charge", flag: "-ev-charge-writes", key: "evChargeWrite", plan: c.planRequest,
				pick:   func(n *navigation) *writeRequest { return n.EvChargeRequest },
				writes: tc.writes, confirm: 10 * time.Second, parked: tc.parked, logf: t.Logf, now: func() time.Time { return clock }}
			r.post = func(k string, v any) {
				if k != "evChargeWrite" {
					t.Errorf("posted %s", k)
				}
				w := v.(writeReport)
				states = append(states, strings.TrimSuffix(w.State+": "+w.Why, ": "))
			}
			r.send = func(line string) error {
				lines = append(lines, line)
				if tc.car {
					for name, x := range r.pending[0].want {
						c.take(micom.Frame{Hdr: schedHdr, Cmd: 0x02, Payload: []byte{items[name], 0, byte(x)}})
					}
				}
				return nil
			}
			r.observe(&navigation{EvChargeRequest: &writeRequest{ID: 1, At: clock.Unix(), Set: json.RawMessage(tc.set)}})
			for range 4 {
				r.step()
				clock = clock.Add(5 * time.Second)
			}
			if !reflect.DeepEqual(lines, tc.lines) {
				t.Errorf("sent %q, want %q", lines, tc.lines)
			}
			if !reflect.DeepEqual(states, tc.states) {
				t.Errorf("reported %q, want %q", states, tc.states)
			}
		})
	}
}
