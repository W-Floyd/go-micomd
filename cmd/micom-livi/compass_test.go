package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCompassStep(t *testing.T) {
	for _, tc := range []struct{ deg, want float64 }{
		{0, 0}, {3.7, 0}, {3.8, 7.5}, {7.5, 7.5}, {180, 180}, {352.5, 352.5}, {356.3, 0}, {359.9, 0}, {360, 0}, {725, 7.5},
	} {
		if got := compassStep(tc.deg); got != tc.want {
			t.Errorf("%g°: %g, want %g", tc.deg, got, tc.want)
		}
	}
}

func show(p *float64) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprint(*p)
}

// By default (min speed 0) every track is the heading, stopped too, as
// hippod's is the gyro-held DR bearing. With a min speed the track counts
// only that fast, the last one holding for a while. No fix, no track or no
// gpsd blanks the compass either way.
func TestHeading(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	h := &heading{hold: 10 * time.Minute}
	f := ptr[float64]
	for _, tc := range []struct {
		name  string
		min   float64 // -heading-min-speed
		at    time.Duration
		mode  int // 0: no report at this step
		track *float64
		speed *float64
		want  string
	}{
		{"nothing yet", 0, 0, 0, nil, nil, "none"},
		{"stopped: the DR bearing", 0, time.Second, 3, f(90), f(0), "90"},
		{"no speed reported", 0, 2 * time.Second, 3, f(95), nil, "97.5"},
		{"no track", 0, 3 * time.Second, 3, nil, f(0), "none"},
		{"no fix", 0, 4 * time.Second, 1, f(95), f(0), "none"},
		{"min speed: slow, never moved", 2, 5 * time.Second, 3, f(90), f(0.5), "none"},
		{"moving", 2, 6 * time.Second, 3, f(91), f(10), "90"},
		{"a step on", 2, 7 * time.Second, 3, f(95), f(10), "97.5"},
		{"stopped: held", 2, 8 * time.Second, 3, f(200), f(0.1), "97.5"},
		{"stopped, still held", 2, 9 * time.Minute, 3, f(250), f(0), "97.5"},
		{"stopped too long", 2, 11 * time.Minute, 3, f(250), f(0), "none"},
		{"moving again", 2, 12 * time.Minute, 3, f(359.9), f(3), "0"},
		{"no track", 2, 12*time.Minute + time.Second, 3, nil, f(3), "none"},
		{"track back", 2, 12*time.Minute + 2*time.Second, 2, f(45), f(3), "45"},
		{"gpsd quiet", 2, 12*time.Minute + 8*time.Second, 0, nil, nil, "none"},
		{"no fix", 2, 12*time.Minute + 9*time.Second, 1, f(45), f(3), "none"},
		{"fix, slow: nothing held after a lost fix", 2, 12*time.Minute + 10*time.Second, 3, f(45), f(0), "none"},
	} {
		now := t0.Add(tc.at)
		h.minSpeed = tc.min
		if tc.mode != 0 {
			h.tpv(tc.mode, tc.track, tc.speed, now)
		}
		if got := show(h.compass(now)); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// With a compass the bridge also runs without guidance: cluster-sub-info
// with the route invalid, on a step change and every second; guidance adds
// the compass to its own cluster-sub-info, and its end goes out as before.
func TestBridgeCompass(t *testing.T) {
	o := navOptions{imperial: true, loc: time.UTC}
	t0 := time.Date(2026, 11, 8, 16, 1, 0, 0, time.UTC)
	compass := ptr(90.0)
	b := newBridge(time.Second)
	b.heading = func(time.Time) *float64 { return compass }
	idle := func(c string) string {
		return `{"compass":` + c + `,"est_point_type":null,"eta_hour":null,"eta_minute":null,"msg":"cluster-sub-info","remaining":null,"remaining_tenths":null,"remaining_unit":null,"time_format":3,"time_type":3}`
	}
	route := navOf(`{"active":true,"maneuverType":2,"turnSide":0,"remainDistance":482.8,"distanceToDestination":1609.344,"etaText":"16:47"}`)
	for _, tc := range []struct {
		name    string
		n       *navigation
		at      time.Duration
		compass *float64
		want    []string // substrings, one per line
	}{
		{"start: cleared, then the compass", nil, 0, ptr(90.0), []string{endGuidance, idle("90")}},
		{"same step", nil, 500 * time.Millisecond, ptr(90.0), nil},
		{"a second on", nil, time.Second, ptr(90.0), []string{idle("90")}},
		{"new step", nil, 1200 * time.Millisecond, ptr(97.5), []string{idle("97.5")}},
		{"no heading", nil, 1400 * time.Millisecond, nil, []string{idle("null")}},
		{"guidance", route, 2 * time.Second, ptr(180.0), []string{`"guiding":true`, `"maneuver":"turn_right"`, `"compass":180,"eta_hour":16`}},
		{"guidance, the compass moves", route, 2500 * time.Millisecond, ptr(187.5), []string{`"compass":187.5,"eta_hour":16`}},
		{"guidance ends", navOf(`{"active":false}`), 3 * time.Second, ptr(187.5), []string{endGuidance, idle("187.5")}},
	} {
		compass = tc.compass
		got := b.step(tc.n, t0.Add(tc.at), o)
		ok := len(got) == len(tc.want)
		for i := 0; ok && i < len(got); i++ {
			ok = strings.Contains(got[i], tc.want[i])
		}
		if !ok {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}
