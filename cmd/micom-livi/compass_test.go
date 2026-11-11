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
// gpsd blanks the compass either way. The position is the last fix's, in
// stock's arc-seconds, none without a fix or once gpsd is quiet.
func TestHeading(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	h := &heading{hold: 10 * time.Minute}
	p := &position{}
	f := ptr[float64]
	for _, tc := range []struct {
		name     string
		min      float64 // -heading-min-speed
		at       time.Duration
		mode     int // 0: no report at this step
		track    *float64
		speed    *float64
		want     string
		lat, lon *float64
		wantPos  string
	}{
		{"nothing yet", 0, 0, 0, nil, nil, "none", nil, nil, "none"},
		{"stopped: the DR bearing", 0, time.Second, 3, f(90), f(0), "90", f(1.5), f(2.25), "5400 8100"},
		{"no speed reported", 0, 2 * time.Second, 3, f(95), nil, "97.5", f(0.0001), f(-0.0001), "0 0"},
		// Stock negates only the degrees: -1.5 is -1° + 30'.
		{"no track", 0, 3 * time.Second, 3, nil, f(0), "none", f(-1.5), f(-10.75), "-1800 -33300"},
		{"no fix", 0, 4 * time.Second, 1, f(95), f(0), "none", f(1), f(1), "none"},
		{"min speed: slow, never moved", 2, 5 * time.Second, 3, f(90), f(0.5), "none", f(1), nil, "none"},
		{"moving", 2, 6 * time.Second, 3, f(91), f(10), "90", f(1), f(1), "3600 3600"},
		{"a step on", 2, 7 * time.Second, 3, f(95), f(10), "97.5", f(1), f(1), "3600 3600"},
		{"stopped: held", 2, 8 * time.Second, 3, f(200), f(0.1), "97.5", f(1), f(1), "3600 3600"},
		{"stopped, still held", 2, 9 * time.Minute, 3, f(250), f(0), "97.5", f(1), f(1), "3600 3600"},
		{"stopped too long", 2, 11 * time.Minute, 3, f(250), f(0), "none", f(1), f(1), "3600 3600"},
		{"moving again", 2, 12 * time.Minute, 3, f(359.9), f(3), "0", f(1), f(1), "3600 3600"},
		{"no track", 2, 12*time.Minute + time.Second, 3, nil, f(3), "none", f(1), f(1), "3600 3600"},
		{"track back", 2, 12*time.Minute + 2*time.Second, 2, f(45), f(3), "45", f(1), f(1), "3600 3600"},
		{"gpsd quiet", 2, 12*time.Minute + 8*time.Second, 0, nil, nil, "none", nil, nil, "none"},
		{"no fix", 2, 12*time.Minute + 9*time.Second, 1, f(45), f(3), "none", f(1), f(1), "none"},
		{"fix, slow: nothing held after a lost fix", 2, 12*time.Minute + 10*time.Second, 3, f(45), f(0), "none", f(1), f(1), "3600 3600"},
	} {
		now := t0.Add(tc.at)
		h.minSpeed = tc.min
		if tc.mode != 0 {
			h.tpv(tc.mode, tc.track, tc.speed, now)
			p.tpv(tc.mode, tc.lat, tc.lon, now)
		}
		if got := show(h.compass(now)); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
		pos := "none"
		if lat, lon, ok := p.arcsec(now); ok {
			pos = fmt.Sprint(lat, " ", lon)
		}
		if pos != tc.wantPos {
			t.Errorf("%s: position %s, want %s", tc.name, pos, tc.wantPos)
		}
	}
}

// The compass is in the idle cluster-sub-info (the route invalid), on a
// step change and every second, and in guidance's own; a route's end goes
// out as stock's, then the compass pane again.
func TestBridgeCompass(t *testing.T) {
	o := navOptions{imperial: true, loc: time.UTC}
	t0 := time.Date(2026, 11, 8, 16, 1, 0, 0, time.UTC)
	compass := ptr(90.0)
	var pos *[2]int
	b := newBridge(time.Second)
	b.heading = func(time.Time) *float64 { return compass }
	b.position = func(time.Time) (int, int, bool) {
		if pos == nil {
			return 0, 0, false
		}
		return pos[0], pos[1], true
	}
	at := func(lat, lon int) *[2]int { return &[2]int{lat, lon} }
	position := `{"lat_arcsec":3600,"lon_arcsec":-3600,"msg":"cluster-position"}`
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
		pos     *[2]int
	}{
		// The pane's compass is the turn's direction, counter-clockwise.
		{"start: claimed, not alive, then the compass pane", nil, 0, ptr(90.0), []string{freeAlive, claimOwn, `"direction":270,`, idle("90")}, nil},
		{"same step", nil, 500 * time.Millisecond, ptr(90.0), nil, nil},
		{"a second on", nil, time.Second, ptr(90.0), []string{`"direction":270,`, idle("90")}, nil},
		{"new step", nil, 1200 * time.Millisecond, ptr(97.5), []string{`"direction":262.5,`, idle("97.5")}, nil},
		{"north", nil, 1300 * time.Millisecond, ptr(0.0), []string{`"direction":0,`, idle("0")}, nil},
		{"no heading: north, as just sent", nil, 1400 * time.Millisecond, nil, []string{idle("null")}, nil},
		{"guidance", route, 2 * time.Second, ptr(180.0), []string{claimAlive, `"maneuver":"turn_right"`, `"compass":180,"eta_hour":16`}, nil},
		{"guidance, the compass moves", route, 2500 * time.Millisecond, ptr(187.5), []string{`"compass":187.5,"eta_hour":16`}, nil},
		{"guidance ends", navOf(`{"active":false}`), 3 * time.Second, ptr(187.5), []string{endGuidance, freeAlive, `"no_route_guide"`, idle("187.5")}, nil},
		// The position, with a fix only: on change and every second.
		{"a fix", nil, 3200 * time.Millisecond, ptr(187.5), []string{position}, at(3600, -3600)},
		{"the same fix", nil, 3500 * time.Millisecond, ptr(187.5), nil, at(3600, -3600)},
		{"a second on", nil, 4200 * time.Millisecond, ptr(187.5), []string{position, `"no_route_guide"`, idle("187.5")}, at(3600, -3600)},
		{"no fix: nothing", nil, 5500 * time.Millisecond, ptr(187.5), []string{`"no_route_guide"`, idle("187.5")}, nil},
	} {
		compass, pos = tc.compass, tc.pos
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
