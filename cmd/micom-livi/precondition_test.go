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
	minKw := func(n *navigation, kw float64) *navigation {
		n.Precondition = &preconditionPrefs{Mode: "auto", MinKw: &kw}
		return n
	}
	soc := ptr(60.0)
	p := precond{rules: precondRules{minPowerW: 50e3, maxTime: time.Hour, minDistM: 200, minSoC: 20, minArrival: 5, capacityWh: 77400, maxStarts: 2}}
	for _, tc := range []struct {
		name  string
		at    time.Duration
		n     *navigation
		soc   *float64
		want  precondOp
		why   string
		state string
	}{
		{"no route", 0, &navigation{Active: ptr(false)}, soc, opNone, "no route", "armed"},
		{"route, no charger", time.Second, &navigation{Active: ptr(true)}, soc, opNone, "not a charger", "armed"},
		{"AC charger", 2 * time.Second, stop(11, 80000, 3000, nil), soc, opNone, "11 kW", "armed"},
		{"DC, too far", 3 * time.Second, stop(350, 150000, 5400, nil), soc, opNone, "90 min away", "armed"},
		{"DC, charge unknown", 4 * time.Second, stop(350, 90000, 3500, nil), nil, opNone, "unknown", "armed"},
		{"DC, charge low", 5 * time.Second, stop(350, 90000, 3500, nil), ptr(15.0), opNone, "15 %", "armed"},
		{"DC, too little on arrival", 6 * time.Second, stop(350, 90000, 3500, i(3000)), soc, opNone, "3000 Wh", "armed"},
		{"DC within the hour: start", 7 * time.Second, stop(350, 90000, 3500, i(20000)), soc, opStart, "", "running"},
		{"running, too soon to refresh", 9 * time.Second, stop(350, 89000, 3450, i(20000)), soc, opNone, "", "running"},
		{"refresh", 12 * time.Second, stop(350, 88000, 3400, i(20000)), soc, opRefresh, "", "running"},
		{"LIVI unreadable: stop", 13 * time.Second, nil, soc, opStop, "unreadable", "armed"},
		{"back: the second start", 14 * time.Second, stop(350, 87000, 3350, i(20000)), soc, opStart, "", "running"},
		{"rerouted past the charger: stop", 15 * time.Second, &navigation{Active: ptr(true)}, soc, opStop, "not a charger", "armed"},
		{"the charger again: capped", 16 * time.Second, stop(350, 86000, 3300, i(20000)), soc, opNone, "2 starts", "armed"},
		{"the route ends", 17 * time.Second, &navigation{Active: ptr(false)}, soc, opNone, "no route", "armed"},
		{"a new route: starts again", 18 * time.Second, stop(150, 30000, 1200, nil), soc, opStart, "", "running"},
		{"arrived: stop", 19 * time.Second, stop(150, 150, 10, nil), soc, opStop, "at the charger", "armed"},
		// LIVI's settings.
		{"asked for, no route: start", 20 * time.Second, prefs(&navigation{Active: ptr(false)}, "auto", true), soc, opStart, "", "running"},
		{"asked for, refresh", 25 * time.Second, prefs(&navigation{Active: ptr(false)}, "auto", true), soc, opRefresh, "", "running"},
		{"asked for, charge low: stop", 26 * time.Second, prefs(&navigation{Active: ptr(false)}, "auto", true), ptr(15.0), opStop, "15 %", "armed"},
		{"the request ends", 27 * time.Second, prefs(&navigation{Active: ptr(false)}, "auto", false), soc, opNone, "the request in LIVI ended", "armed"},
		{"asked for again", 27500 * time.Millisecond, prefs(&navigation{Active: ptr(true)}, "auto", true), soc, opStart, "", "running"},
		{"switched off on a route: stop, and why", 27700 * time.Millisecond, prefs(&navigation{Active: ptr(true)}, "auto", false), soc, opStop, "the request in LIVI ended", "armed"},
		{"then the route's own reason", 27800 * time.Millisecond, prefs(&navigation{Active: ptr(true)}, "auto", false), soc, opNone, "not a charger", "armed"},
		{"LIVI's threshold above the charger", 27900 * time.Millisecond, minKw(stop(350, 90000, 3500, nil), 400), soc, opNone, "not above 400", "armed"},
		{"auto, a charger: start", 28 * time.Second, prefs(stop(350, 90000, 3500, nil), "auto", false), soc, opStart, "", "running"},
		{"turned off: stop", 29 * time.Second, prefs(stop(350, 89000, 3450, nil), "off", false), soc, opStop, "off in LIVI", "off"},
		{"off wins over asked for", 30 * time.Second, prefs(stop(350, 88000, 3400, nil), "off", true), soc, opNone, "off in LIVI", "off"},
		{"LIVI's threshold below the flag's", 31 * time.Second, minKw(stop(40, 30000, 1200, nil), 30), soc, opStart, "", "running"},
	} {
		got := p.step(tc.n, tc.soc, t0.Add(tc.at))
		if st := p.state(tc.n); got != tc.want || !strings.Contains(p.why, tc.why) || st != tc.state {
			t.Errorf("%s: %v (%q) %s, want %v (%q) %s", tc.name, got, p.why, st, tc.want, tc.why, tc.state)
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
