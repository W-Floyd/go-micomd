package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	micom "github.com/W-Floyd/go-micomd"
)

// The car's charge limits (AC, DC) and AC charging current. micom-livi posts
// the current as acChargingCurrentPct (the limits are carFields). With
// -ev-charge-writes, LIVI's change (evChargeRequest at GET /navigation) goes to
// the car as stock's EV settings send it: one setting per frame, the rest
// "unchanged" (the variant's ev-charge-limits, merge: false), nothing taken
// from the readback, nothing sent for what the car already reports; then
// confirmed by the readback (writes.go). LIVI hears how it went as
// evChargeWrite.

const (
	fLimitAC      = "Charge limit, AC"
	fLimitDC      = "Charge limit, DC"
	fCurrent      = "AC charging current"
	fChargerReady = "Charger ready"
)

// currents are the AC charging current's choices, %, as the variant's
// ev-charging-current names them.
var currents = map[int]string{100: "maximum", 90: "reduced", 60: "minimum"}

// currentPct is the car's AC charging current code as stock shows it.
func currentPct(code uint64) int {
	switch code {
	case 2:
		return 90
	case 3:
		return 60
	}
	return 100
}

// charge is what the car last reported of its charge settings, as raw codes
// by read field, from whichever message carries them (items, 0x8C01).
type charge struct {
	v   *micom.Variant
	mu  sync.Mutex
	raw map[string]uint64
}

func newCharge(v *micom.Variant) *charge { return &charge{v: v, raw: map[string]uint64{}} }

// take keeps what f reports; true when it reports the AC charging current.
func (c *charge) take(f micom.Frame) bool {
	got := false
	for _, fs := range c.v.ReceivedFields(f) {
		for _, name := range []string{fLimitAC, fLimitDC, fCurrent, fChargerReady} {
			if r, ok := fs[name]; ok && r.Valid {
				c.mu.Lock()
				c.raw[name] = r.Raw
				c.mu.Unlock()
				got = got || name == fCurrent
			}
		}
	}
	return got
}

func (c *charge) get(name string) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	x, ok := c.raw[name]
	return x, ok
}

// telemetry is the AC charging current, null until the car reports it.
func (c *charge) telemetry() map[string]any {
	var pct any
	if x, ok := c.get(fCurrent); ok {
		pct = float64(currentPct(x))
	}
	return map[string]any{"acChargingCurrentPct": pct}
}

// chargeEdit is LIVI's change: each setting it names, in %.
type chargeEdit struct {
	LimitAC *int `json:"chargeLimitAcPct,omitempty"`
	LimitDC *int `json:"chargeLimitDcPct,omitempty"`
	Current *int `json:"chargingCurrentPct,omitempty"`
}

// planRequest is what to send for LIVI's change: a frame per setting it
// changes, AC, DC, then the current.
func (c *charge) planRequest(set json.RawMessage) ([]carWrite, error) {
	var e chargeEdit
	if err := strictJSON(set, &e); err != nil {
		return nil, err
	}
	var bad []string
	var out []carWrite
	limit := func(name, field, what string, pct *int) {
		if pct == nil {
			return
		}
		// Stock's settings page offers these only.
		if *pct < 50 || *pct > 100 || *pct%10 != 0 {
			bad = append(bad, fmt.Sprintf("%s limit %d %%: 50 to 100 in tens", what, *pct))
			return
		}
		x, known := c.get(name)
		switch want := uint64(*pct * 2); {
		case !known:
			// Without it a value already held would never be reported back.
			bad = append(bad, fmt.Sprintf("the car has not reported its %s limit yet", what))
		case x != want:
			b, _ := json.Marshal(map[string]any{"msg": "ev-charge-limits", field: *pct})
			out = append(out, carWrite{"ev-charge-limits", string(b), map[string]uint64{name: want}, c.unconfirmed(name, func(got uint64) bool { return got == want })})
		}
	}
	limit(fLimitAC, "ac_limit", "AC", e.LimitAC)
	limit(fLimitDC, "dc_limit", "DC", e.LimitDC)
	if p := e.Current; p != nil {
		name, ok := currents[*p]
		ready, known := c.get(fChargerReady)
		x, held := c.get(fCurrent)
		switch {
		case !ok:
			bad = append(bad, fmt.Sprintf("charging current %d %%: 100, 90 or 60", *p))
		case !known || ready != 1:
			// Stock greys the setting until then.
			bad = append(bad, "the on-board charger is not ready")
		// Unknown is sent: the car reports it only on change or a startup read.
		case !held || currentPct(x) != *p:
			b, _ := json.Marshal(map[string]any{"msg": "ev-charging-current", "current": name})
			want := *p
			code, _ := c.v.RawCode(fCurrent, fCurrent, name)
			out = append(out, carWrite{"ev-charging-current", string(b), map[string]uint64{fCurrent: code}, c.unconfirmed(fCurrent, func(got uint64) bool { return currentPct(got) == want })})
		}
	}
	if bad != nil {
		slices.Sort(bad)
		return nil, fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	if e == (chargeEdit{}) {
		return nil, fmt.Errorf("nothing to change")
	}
	return out, nil
}

// unconfirmed is whether the car has yet to report name as ok takes it.
func (c *charge) unconfirmed(name string, ok func(uint64) bool) func() []string {
	return func() []string {
		if x, has := c.get(name); has && ok(x) {
			return nil
		}
		return []string{name}
	}
}
