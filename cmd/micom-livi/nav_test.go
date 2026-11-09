package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	micom "github.com/W-Floyd/go-micomd"
)

// LIVI's maneuver types (CarPlay's numbering) and turn sides, as the
// cluster's display types.
func TestClusterManeuver(t *testing.T) {
	l, r := ptr(1), ptr(0)
	for _, tc := range []struct {
		typ  *int
		side *int
		want string
	}{
		{nil, nil, "unknown"},
		{ptr(0), nil, "straight"},
		{ptr(1), l, "turn_left"},
		{ptr(2), r, "turn_right"},
		{ptr(3), nil, "straight"},
		{ptr(4), nil, "uturn_left"},
		{ptr(4), r, "uturn_right"},
		{ptr(5), nil, "straight"},
		{ptr(6), nil, "roundabout_exit_right"},
		{ptr(7), l, "roundabout_exit_left"},
		{ptr(8), nil, "off_ramp"},
		{ptr(8), r, "off_ramp_right"},
		{ptr(9), nil, "on_ramp"},
		{ptr(10), nil, "arrived"},
		{ptr(11), nil, "depart"},
		{ptr(12), nil, "arrived"},
		{ptr(13), nil, "on_ramp_left"},
		{ptr(14), nil, "on_ramp_right"},
		{ptr(15), nil, "ferry"},
		{ptr(20), l, "turn_left"},
		{ptr(21), r, "turn_right"},
		{ptr(22), l, "off_ramp_left"},
		{ptr(23), r, "off_ramp_right"},
		{ptr(25), r, "arrived"},
		{ptr(26), l, "uturn_left"},
		{ptr(28), nil, "roundabout_exit_right"},
		{ptr(46), l, "roundabout_exit_left"},
		{ptr(47), l, "sharp_left"},
		{ptr(48), r, "sharp_right"},
		{ptr(49), l, "slight_left"},
		{ptr(50), r, "slight_right"},
		{ptr(51), r, "on_ramp_right"},
		{ptr(53), r, "on_ramp_right"},
		{ptr(54), nil, "unknown"},
	} {
		if got := clusterManeuver(tc.typ, tc.side); got != tc.want {
			t.Errorf("type %v side %v: %s, want %s", deref(tc.typ), deref(tc.side), got, tc.want)
		}
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestClusterDistance(t *testing.T) {
	for _, tc := range []struct {
		m        float64
		imperial bool
		whole    int
		tenths   int
		unit     string
	}{
		{0, true, 0, 0, "ft"},
		{152.4, true, 500, 0, "ft"},
		{161, true, 0, 1, "mi"},
		{482.8, true, 0, 3, "mi"},
		{70799, true, 44, 0, "mi"},
		{130907, true, 81, 3, "mi"},
		{15000, true, 9, 3, "mi"},
		{40, false, 40, 0, "m"},
		{999, false, 1000, 0, "m"},
		{1000, false, 1, 0, "km"},
		{300 + 1000, false, 1, 3, "km"},
		{78940, false, 78, 9, "km"},
		{1e9, false, 65534, 0, "km"},
	} {
		w, tn, u := clusterDistance(tc.m, tc.imperial)
		if w != tc.whole || tn != tc.tenths || u != tc.unit {
			t.Errorf("%g m (imperial %v): %d.%d %s, want %d.%d %s", tc.m, tc.imperial, w, tn, u, tc.whole, tc.tenths, tc.unit)
		}
	}
}

func navOf(js string) *navigation {
	var n navigation
	if err := json.Unmarshal([]byte(js), &n); err != nil {
		panic(err)
	}
	return &n
}

// The guidance of LIVI's log, "maneuver=4 ... dest=78940m eta=16:47
// step=70799m", start to stop. The pane is claimed at the start (alive,
// then operating) and kept: no route is the compass pane (no_route_guide),
// a route its maneuver and summary on change and every second, a route's
// end stock's end frames and the compass pane again; LIVI gone is no
// route. The day/night look goes on change. Release only with release()
// (exit) or after releaseAfter without LIVI; a new micomd connection
// claims afresh.
func TestBridgeSequence(t *testing.T) {
	o := navOptions{imperial: true, loc: time.UTC}
	t0 := time.Date(2026, 11, 8, 16, 1, 0, 0, time.UTC)
	exit := navOf(`{"active":true,"maneuverType":23,"turnSide":0,"remainDistance":70799,"distanceToDestination":78940,"timeToDestination":2733,"etaText":"16:47"}`)
	near := navOf(`{"active":true,"maneuverType":23,"turnSide":0,"remainDistance":150,"distanceToDestination":8290,"timeToDestination":420,"etaText":"16:47"}`)
	off := navOf(`{"active":false}`)
	turn := `{"bar":null,"direction":0,"distance":44,"distance_tenths":0,"maneuver":"off_ramp_right","msg":"tbt-turn","side_street":null,"unit":"mi"}`
	sub := `{"eta_hour":16,"eta_minute":47,"msg":"cluster-sub-info","remaining":49,"remaining_tenths":1,"remaining_unit":"mi","time_format":1,"time_type":1}`
	idle := `{"bar":null,"direction":0,"distance":null,"distance_tenths":0,"maneuver":"no_route_guide","msg":"tbt-turn","side_street":0,"side_street_combi":0,"unit":"m"}`
	idleSub := `{"compass":null,"est_point_type":null,"eta_hour":null,"eta_minute":null,"msg":"cluster-sub-info","remaining":null,"remaining_tenths":null,"remaining_unit":null,"time_format":3,"time_type":3}`
	day := `{"mode":"day","msg":"cluster-day-night"}`
	night := `{"mode":"night","msg":"cluster-day-night"}`
	var isNight *bool
	b := newBridge(time.Second)
	b.releaseAfter = time.Minute
	b.night = func() *bool { return isNight }
	for _, tc := range []struct {
		name  string
		n     *navigation
		at    time.Duration
		night *bool
		want  []string
	}{
		{"start, day/night unknown: claim, not alive, the compass pane", nil, 0, nil, []string{freeAlive, claimOwn, idle, idleSub}},
		{"nothing new", off, 500 * time.Millisecond, nil, nil},
		{"illumination: day", off, 700 * time.Millisecond, ptr(false), []string{day}},
		{"a second on, refreshed", off, 1200 * time.Millisecond, ptr(false), []string{idle, idleSub}},
		{"guidance starts: alive", exit, 1500 * time.Millisecond, ptr(false), []string{claimAlive, turn, sub}},
		{"nothing new", exit, 2 * time.Second, ptr(false), nil},
		{"a second on, refreshed", exit, 2500 * time.Millisecond, ptr(false), []string{turn, sub}},
		{"closer, at night", near, 3 * time.Second, ptr(true), []string{night,
			`{"bar":null,"direction":0,"distance":490,"distance_tenths":0,"maneuver":"off_ramp_right","msg":"tbt-turn","side_street":null,"unit":"ft"}`,
			`{"eta_hour":16,"eta_minute":47,"msg":"cluster-sub-info","remaining":5,"remaining_tenths":2,"remaining_unit":"mi","time_format":1,"time_type":1}`,
		}},
		{"guidance stops: stock's end, not alive, the pane kept", off, 4 * time.Second, ptr(true), []string{endGuidance, freeAlive, idle, idleSub}},
		{"LIVI gone: still the compass pane", nil, 4500 * time.Millisecond, ptr(true), nil},
		{"guidance again", exit, 5 * time.Second, ptr(true), []string{claimAlive, turn, sub}},
		{"LIVI gone while guiding", nil, 5500 * time.Millisecond, ptr(true), []string{endGuidance, freeAlive, idle, idleSub}},
		{"LIVI gone a minute: released", nil, 65500 * time.Millisecond, ptr(true), []string{freeAlive, freeOwn}},
		{"still gone", nil, 70 * time.Second, ptr(true), nil},
		{"LIVI back: claimed again", off, 71 * time.Second, ptr(true), []string{freeAlive, claimOwn, night, idle, idleSub}},
	} {
		isNight = tc.night
		got := b.step(tc.n, t0.Add(tc.at), o)
		if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
	if got := b.release(); strings.Join(got, "\n") != freeAlive+"\n"+freeOwn {
		t.Errorf("release at exit: %q", got)
	}
	if got := b.release(); got != nil {
		t.Errorf("released twice: %q", got)
	}
	b.step(exit, t0.Add(80*time.Second), o)
	b.reset()
	if got := b.step(exit, t0.Add(81*time.Second), o); len(got) != 5 || got[0] != claimAlive {
		t.Errorf("after a reconnection: %q", got)
	}
}

func TestClusterEta(t *testing.T) {
	now := time.Date(2026, 11, 8, 16, 1, 0, 0, time.UTC)
	for _, tc := range []struct {
		js   string
		h, m int
		ok   bool
	}{
		{`{"etaText":"16:47"}`, 16, 47, true},
		{`{"etaText":"4:47 PM"}`, 16, 47, true},
		{`{"eta":1794157620}`, 17, 7, true},
		{`{"timeToDestination":2733}`, 16, 46, true},
		{`{"etaText":"soon"}`, 0, 0, false},
		{`{}`, 0, 0, false},
	} {
		h, m, ok := clusterEta(navOf(tc.js), now, time.UTC)
		if h != tc.h || m != tc.m || ok != tc.ok {
			t.Errorf("%s: %d:%02d %v, want %d:%02d %v", tc.js, h, m, ok, tc.h, tc.m, tc.ok)
		}
	}
}

// Every line the bridge writes is one micomd's EV6 variant takes, with
// -allow-inferred and nothing more.
func TestBridgeLinesAreTakenByMicomd(t *testing.T) {
	a, port := net.Pipe()
	go io.Copy(io.Discard, port)
	d := micom.New(micom.Config{Port: a, Profile: []micom.Periodic{}})
	dir, err := os.MkdirTemp("", "nav")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "in")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go micom.ServeInput(ctx, d, p, micom.InputGates{Inferred: true})
	var c net.Conn
	for range 200 {
		if c, err = net.Dial("unix", p); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rd := bufio.NewReader(c)
	now := time.Date(2026, 11, 8, 16, 1, 0, 0, time.UTC)
	b := newBridge(time.Second)
	// With a compass: 352.5, then none, then 0, an hour apart.
	bc := newBridge(time.Second)
	bc.heading = func(t time.Time) *float64 {
		if h := t.Sub(now).Hours(); int(h)%3 == 1 {
			return nil
		} else if int(h)%3 == 2 {
			return ptr(0.0)
		}
		return ptr(352.5)
	}
	bc.night = func() *bool { return ptr(true) }
	var lines []string
	for i, js := range []string{
		`{"active":false}`,
		`{"active":true}`,
		`{"active":true,"maneuverType":11,"remainDistance":0}`,
		`{"active":true,"maneuverType":2,"turnSide":0,"turnAngle":-90,"remainDistance":482.8,"distanceToDestination":78940,"etaText":"16:47"}`,
		`{"active":true,"maneuverType":31,"turnSide":1,"remainDistance":5,"distanceToDestination":1e9,"eta":1794157620}`,
		`{"active":true,"maneuverType":12,"remainDistance":20,"distanceToDestination":20,"timeToDestination":30}`,
		`{"active":false}`,
	} {
		for _, imperial := range []bool{true, false} {
			o := navOptions{imperial: imperial, clock24: !imperial, loc: time.UTC}
			lines = append(lines, b.step(navOf(js), now.Add(time.Duration(i)*time.Hour), o)...)
			lines = append(lines, bc.step(navOf(js), now.Add(time.Duration(i)*time.Hour), o)...)
		}
	}
	lines = append(lines, b.release()...)
	for _, l := range lines {
		if err := sendInput(c, rd, l); err != nil {
			t.Errorf("%s: %v", l, err)
		}
	}
	if len(lines) < 10 {
		t.Errorf("only %d lines: %q", len(lines), lines)
	}
}
