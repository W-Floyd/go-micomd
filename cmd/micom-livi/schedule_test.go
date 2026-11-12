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

var schedHdr = [4]byte{0x0c, 0x88, 0x01, 0x8c}

const (
	liveDepartures = "0ebd770c0c000000" // 0x8C55 on the car, 2026-11: both 12:00, no days
	seenSettings   = "0014100200400050" // 0x8C53 as captured: off-peak 20:00-16:00, mode off, climate off
	seenWindow     = "00010040ffffff0f" // 0x8C54: no defrost
)

func schedFrames(t *testing.T, held map[byte]string) []micom.Frame {
	t.Helper()
	var out []micom.Frame
	for _, cmd := range []byte{0x55, 0x53, 0x54} {
		if p, ok := held[cmd]; ok {
			b, err := hex.DecodeString(p)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, micom.Frame{Hdr: schedHdr, Cmd: cmd, Payload: b})
		}
	}
	return out
}

func newTestSchedule(t *testing.T) *schedule {
	t.Helper()
	v, err := micom.DefaultVariant()
	if err != nil {
		t.Fatal(err)
	}
	return newSchedule(v)
}

// What the car reports reaches LIVI as its fields; what it has not said, or
// says as stock's "none" (hour 31, minute bits 7), is null, never 00:00.
func TestScheduleTelemetry(t *testing.T) {
	none := []any{nil, nil, nil, nil, nil, nil, nil}
	off := []any{false, false, false, false, false, false, false}
	for _, tc := range []struct {
		name string
		held map[byte]string
		want map[string]any
	}{
		{"nothing yet", nil, map[string]any{
			"departure1":            map[string]any{"on": nil, "hour": nil, "minute": nil, "days": none},
			"offPeak":               map[string]any{"startHour": nil, "startMinute": nil, "endHour": nil, "endMinute": nil},
			"scheduledChargingMode": nil, "scheduledClimate": nil, "scheduledClimateTempCode": nil,
		}},
		{"0x8C55 alone: departure 2's Friday to Sunday and enable are 0x8C53's", map[byte]string{0x55: liveDepartures}, map[string]any{
			"departure1": map[string]any{"on": false, "hour": 12.0, "minute": 0.0, "days": off},
			"departure2": map[string]any{"on": nil, "hour": 12.0, "minute": 0.0, "days": []any{false, false, false, false, nil, nil, nil}},
		}},
		{"all three as seen", map[byte]string{0x55: liveDepartures, 0x53: seenSettings, 0x54: seenWindow}, map[string]any{
			"departure2":            map[string]any{"on": false, "hour": 12.0, "minute": 0.0, "days": off},
			"offPeak":               map[string]any{"startHour": 20.0, "startMinute": 0.0, "endHour": 16.0, "endMinute": 0.0},
			"scheduledChargingMode": "off", "scheduledClimate": false, "scheduledClimateDefrost": false, "scheduledClimateTempCode": 20.0,
		}},
		{"set: 07:30 Mon Wed, off-peak only, climate on", map[byte]string{0x55: "0ebd77670c110100", 0x53: "00b3300200430051", 0x54: "01010040ffffff0f"}, map[string]any{
			"departure1":            map[string]any{"on": true, "hour": 7.0, "minute": 30.0, "days": []any{true, false, true, false, false, false, false}},
			"offPeak":               map[string]any{"startHour": 19.0, "startMinute": 50.0, "endHour": 16.0, "endMinute": 10.0},
			"scheduledChargingMode": "off_peak_only", "scheduledClimate": true, "scheduledClimateDefrost": true,
		}},
		{"stock's none, an unnamed mode and a 2-bit day of 2", map[byte]string{0x55: "0ebd77ff0c020000", 0x53: "00ffff0200410050"}, map[string]any{
			"departure1":            map[string]any{"on": false, "hour": nil, "minute": nil, "days": []any{nil, false, false, false, false, false, false}},
			"offPeak":               map[string]any{"startHour": nil, "startMinute": nil, "endHour": nil, "endMinute": nil},
			"scheduledChargingMode": "code 1",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSchedule(t)
			for _, f := range schedFrames(t, tc.held) {
				if !s.take(f) {
					t.Fatalf("%v not taken", f)
				}
			}
			got := s.telemetry()
			for k, want := range tc.want {
				if !reflect.DeepEqual(got[k], want) {
					t.Errorf("%s = %#v, want %#v", k, got[k], want)
				}
			}
		})
	}
}

// Each row is one edit from LIVI, through the runner: what it sends to
// micomd, then how it reports to LIVI. With car the car takes each write and
// reports it back; without, it says nothing.
func TestScheduleEdits(t *testing.T) {
	all := map[byte]string{0x55: liveDepartures, 0x53: seenSettings, 0x54: seenWindow}
	edit := func(js string) scheduleEdit {
		var e scheduleEdit
		if err := json.Unmarshal([]byte(js), &e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	parked := func() (bool, string) { return true, "" }
	for _, tc := range []struct {
		name   string
		held   map[byte]string
		writes bool
		parked func() (bool, string)
		set    string
		age    time.Duration
		car    bool
		lines  []string
		states []string // evScheduleWrite's state: why
	}{
		{"departure 1 on at 07:30 Mon Wed", all, true, parked,
			`{"departure1":{"on":true,"hour":7,"minute":30,"days":[true,false,true,false,false,false,false]}}`, 0, true,
			[]string{`{"d1_climate":0,"d1_defrost":0,"d1_fri":0,"d1_hour":7,"d1_min_bits":3,"d1_mon":1,"d1_on":1,"d1_sat":0,"d1_sun":0,"d1_temp":20,"d1_temp_unit":1,"d1_thu":0,"d1_tue":0,"d1_wed":1,"d2_fri":0,"d2_hour":12,"d2_min_bits":0,"d2_mon":0,"d2_on":0,"d2_sat":0,"d2_sun":0,"d2_thu":0,"d2_tue":0,"d2_wed":0,"msg":"ev-departures"}`},
			[]string{"sending", "confirmed"}},
		{"off-peak only, 22:30 to 06:00", all, true, parked,
			`{"offPeak":{"startHour":22,"startMinute":30,"endHour":6,"endMinute":0},"chargingMode":"off_peak_only"}`, 0, true,
			[]string{`{"end_hour":6,"end_min_bits":0,"mode":"off_peak_only","msg":"ev-off-peak","start_hour":22,"start_min_bits":3}`},
			[]string{"sending", "confirmed"}},
		{"an edit of both sends both, off-peak first", all, true, parked,
			`{"climate":true,"offPeak":{"endHour":7}}`, 0, true,
			[]string{
				`{"end_hour":7,"end_min_bits":0,"mode":"off","msg":"ev-off-peak","start_hour":20,"start_min_bits":0}`,
				`{"d1_climate":1,"d1_defrost":0,"d1_fri":0,"d1_hour":12,"d1_min_bits":0,"d1_mon":0,"d1_on":0,"d1_sat":0,"d1_sun":0,"d1_temp":20,"d1_temp_unit":1,"d1_thu":0,"d1_tue":0,"d1_wed":0,"d2_fri":0,"d2_hour":12,"d2_min_bits":0,"d2_mon":0,"d2_on":0,"d2_sat":0,"d2_sun":0,"d2_thu":0,"d2_tue":0,"d2_wed":0,"msg":"ev-departures"}`,
			},
			[]string{"sending", "confirmed"}},
		{"the car does not report it back", all, true, parked, `{"climateDefrost":true}`, 0, false,
			[]string{`{"d1_climate":0,"d1_defrost":1,"d1_fri":0,"d1_hour":12,"d1_min_bits":0,"d1_mon":0,"d1_on":0,"d1_sat":0,"d1_sun":0,"d1_temp":20,"d1_temp_unit":1,"d1_thu":0,"d1_tue":0,"d1_wed":0,"d2_fri":0,"d2_hour":12,"d2_min_bits":0,"d2_mon":0,"d2_on":0,"d2_sat":0,"d2_sun":0,"d2_thu":0,"d2_tue":0,"d2_wed":0,"msg":"ev-departures"}`},
			[]string{"sending", "failed: the car did not report it back within 10s (Scheduled climate defrost)"}},
		{"writes off", all, false, parked, `{"climate":true}`, 0, true, nil,
			[]string{"failed: writes are off: micom-livi runs without -ev-schedule-writes"}},
		{"not parked", all, true, func() (bool, string) { return false, "the car is in D, not parked" }, `{"climate":true}`, 0, true, nil,
			[]string{"failed: the car is in D, not parked"}},
		{"an old edit", all, true, parked, `{"climate":true}`, time.Minute, true, nil,
			[]string{"failed: the edit is 60 s old"}},
		{"0x8C53 not reported yet", map[byte]string{0x55: liveDepartures, 0x54: seenWindow}, true, parked, `{"climate":true}`, 0, true, nil,
			[]string{"failed: ev-departures: nothing received yet of Departure 2 Friday, Departure 2 Saturday, Departure 2 Sunday, Departure 2 enabled, Scheduled climate temperature"}},
		{"the car's mode 1 is never written back", map[byte]string{0x55: liveDepartures, 0x53: "0014100200410050", 0x54: seenWindow}, true, parked,
			`{"offPeak":{"startHour":21}}`, 0, true, nil,
			[]string{"failed: ev-off-peak: Scheduled charging mode reads code 1, which enum sched_charge_mode does not name"}},
		{"minutes in tens, a known mode", all, true, parked, `{"departure2":{"minute":15},"chargingMode":"fastest"}`, 0, true, nil,
			[]string{"failed: Scheduled charging mode: \"fastest\" is not in enum sched_charge_mode; minute 15: the car takes tens"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSchedule(t)
			held, cmdOf := map[byte][]byte{}, map[string]byte{}
			for _, f := range schedFrames(t, tc.held) {
				s.take(f)
				held[f.Cmd] = f.Payload
				for msg := range s.v.ReceivedFields(f) {
					cmdOf[msg] = f.Cmd
				}
			}
			clock := time.Unix(1_800_000_000, 0)
			var lines, states []string
			r := &scheduleRunner{s: s, writes: tc.writes, confirm: 10 * time.Second, parked: tc.parked, logf: t.Logf,
				now: func() time.Time { return clock }}
			r.post = func(k string, v any) {
				w := v.(scheduleWrite)
				states = append(states, strings.TrimSuffix(w.State+": "+w.Why, ": "))
			}
			r.send = func(line string) error {
				lines = append(lines, line)
				if !tc.car {
					return nil
				}
				// The car takes it: its readback, over what it held. Two messages
				// may read one frame.
				byMsg := map[string]map[string]uint64{}
				for k, x := range r.pending[0].want {
					m := s.of[k]
					if byMsg[m] == nil {
						byMsg[m] = map[string]uint64{}
					}
					byMsg[m][k] = x
				}
				for m, raw := range byMsg {
					f, err := s.v.EncodeReceivedRaw(m, held[cmdOf[m]], raw)
					if err != nil {
						t.Fatal(err)
					}
					held[f.Cmd] = f.Payload
					s.take(f)
				}
				return nil
			}
			r.observe(&navigation{EvScheduleRequest: &scheduleRequest{ID: 1, At: clock.Add(-tc.age).Unix(), Set: edit(tc.set)}})
			for range 3 {
				r.step()
				clock = clock.Add(5 * time.Second)
			}
			if !reflect.DeepEqual(lines, tc.lines) {
				t.Errorf("sent %q, want %q", lines, tc.lines)
			}
			if !reflect.DeepEqual(states, tc.states) {
				t.Errorf("reported %q, want %q", states, tc.states)
			}
			// The same id is not taken up again.
			n := len(lines)
			r.step()
			if len(lines) != n {
				t.Errorf("sent again: %q", lines[n:])
			}
		})
	}
}

// Parked is the driving regulation known off and no gear but P.
func TestParkedFrom(t *testing.T) {
	for _, tc := range []struct {
		reg  *bool
		gear *string
		ok   bool
	}{
		{nil, nil, false},
		{ptr(false), nil, true},
		{ptr(false), ptr("P"), true},
		{ptr(false), ptr("D"), false},
		{ptr(true), ptr("P"), false},
		{nil, ptr("P"), false},
	} {
		car := &carState{}
		if tc.reg != nil {
			car.regulation.set(*tc.reg)
		}
		if tc.gear != nil {
			car.gear.set(*tc.gear)
		}
		if ok, why := parkedFrom(car)(); ok != tc.ok {
			t.Errorf("regulation %v gear %v: %v (%s), want %v", tc.reg, tc.gear, ok, why, tc.ok)
		}
	}
}
