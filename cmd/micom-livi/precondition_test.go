package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// One route's life, step by step: the conditions each start, hold or stop
// conditioning, and starts per route are capped.
func TestPrecondition(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	i := func(v int64) *int64 { return &v }
	stop := func(kw, m, s int64, arrivalWh *int64) *navigation {
		return &navigation{Active: ptr(true), ChargingStop: &chargingStop{PowerW: kw * 1000, DistanceM: i(m), TimeS: i(s), ArrivalWh: arrivalWh}}
	}
	prefs := func(n *navigation, mode string, manual bool) *navigation {
		n.Precondition = &preconditionPrefs{Mode: mode, Manual: manual}
		return n
	}
	soc := ptr(60.0)
	p := precond{rules: precondRules{minPowerW: 50e3, maxTime: time.Hour, minDistM: 200, minSoC: 20, minArrival: 5, capacityWh: 77400, maxStarts: 2}}
	for _, tc := range []struct {
		name string
		at   time.Duration
		n    *navigation
		soc  *float64
		want precondOp
		why  string
	}{
		{"no route", 0, &navigation{Active: ptr(false)}, soc, opNone, "no route"},
		{"route, no charger", time.Second, &navigation{Active: ptr(true)}, soc, opNone, "not a charger"},
		{"AC charger", 2 * time.Second, stop(11, 80000, 3000, nil), soc, opNone, "11 kW"},
		{"DC, too far", 3 * time.Second, stop(350, 150000, 5400, nil), soc, opNone, "90 min away"},
		{"DC, charge unknown", 4 * time.Second, stop(350, 90000, 3500, nil), nil, opNone, "unknown"},
		{"DC, charge low", 5 * time.Second, stop(350, 90000, 3500, nil), ptr(15.0), opNone, "15 %"},
		{"DC, too little on arrival", 6 * time.Second, stop(350, 90000, 3500, i(3000)), soc, opNone, "3000 Wh"},
		{"DC within the hour: start", 7 * time.Second, stop(350, 90000, 3500, i(20000)), soc, opStart, ""},
		{"running, too soon to refresh", 9 * time.Second, stop(350, 89000, 3450, i(20000)), soc, opNone, ""},
		{"refresh", 12 * time.Second, stop(350, 88000, 3400, i(20000)), soc, opRefresh, ""},
		{"LIVI unreadable: stop", 13 * time.Second, nil, soc, opStop, "unreadable"},
		{"back: the second start", 14 * time.Second, stop(350, 87000, 3350, i(20000)), soc, opStart, ""},
		{"rerouted past the charger: stop", 15 * time.Second, &navigation{Active: ptr(true)}, soc, opStop, "not a charger"},
		{"the charger again: capped", 16 * time.Second, stop(350, 86000, 3300, i(20000)), soc, opNone, "2 starts"},
		{"the route ends", 17 * time.Second, &navigation{Active: ptr(false)}, soc, opNone, "no route"},
		{"a new route: starts again", 18 * time.Second, stop(150, 30000, 1200, nil), soc, opStart, ""},
		{"arrived: stop", 19 * time.Second, stop(150, 150, 10, nil), soc, opStop, "at the charger"},
		// LIVI's settings.
		{"asked for, no route: start", 20 * time.Second, prefs(&navigation{Active: ptr(false)}, "auto", true), soc, opStart, ""},
		{"asked for, refresh", 25 * time.Second, prefs(&navigation{Active: ptr(false)}, "auto", true), soc, opRefresh, ""},
		{"asked for, charge low: stop", 26 * time.Second, prefs(&navigation{Active: ptr(false)}, "auto", true), ptr(15.0), opStop, "15 %"},
		{"the request ends", 27 * time.Second, prefs(&navigation{Active: ptr(false)}, "auto", false), soc, opNone, "the request in LIVI ended"},
		{"asked for again", 27500 * time.Millisecond, prefs(&navigation{Active: ptr(true)}, "auto", true), soc, opStart, ""},
		{"switched off on a route: stop, and why", 27700 * time.Millisecond, prefs(&navigation{Active: ptr(true)}, "auto", false), soc, opStop, "the request in LIVI ended"},
		{"then the route's own reason", 27800 * time.Millisecond, prefs(&navigation{Active: ptr(true)}, "auto", false), soc, opNone, "not a charger"},
		{"auto, a charger: start", 28 * time.Second, prefs(stop(350, 90000, 3500, nil), "auto", false), soc, opStart, ""},
		{"turned off: stop", 29 * time.Second, prefs(stop(350, 89000, 3450, nil), "off", false), soc, opStop, "off in LIVI"},
		{"off wins over asked for", 30 * time.Second, prefs(stop(350, 88000, 3400, nil), "off", true), soc, opNone, "off in LIVI"},
	} {
		if got := p.step(tc.n, tc.soc, t0.Add(tc.at)); got != tc.want || !strings.Contains(p.why, tc.why) {
			t.Errorf("%s: %v (%q), want %v (%q)", tc.name, got, p.why, tc.want, tc.why)
		}
	}
}

// Stock's fields cap below their "none" values.
func TestRouteInputs(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		m, s int64
		want []string
	}{
		{90000, 3500, []string{`{"msg":"batt-cond-route-distance","km":90}`, `{"msg":"batt-cond-remain-time","minutes":59}`}},
		{450, 61, []string{`{"msg":"batt-cond-route-distance","km":0}`, `{"msg":"batt-cond-remain-time","minutes":2}`}},
		{5000000, 200000, []string{`{"msg":"batt-cond-route-distance","km":2046}`, `{"msg":"batt-cond-remain-time","minutes":1022}`}},
	} {
		if got := routeInputs(&chargingStop{DistanceM: i(tc.m), TimeS: i(tc.s)}); !slices.Equal(got, tc.want) {
			t.Errorf("%d m, %d s: %q, want %q", tc.m, tc.s, got, tc.want)
		}
	}
}
