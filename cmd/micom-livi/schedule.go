package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	micom "github.com/W-Floyd/go-micomd"
)

// The car's EV schedules (its two departure timers, the off-peak window and
// charging mode, the climate at departure), posted to LIVI as the car reports
// them. With -ev-schedule-writes, LIVI's edits (evScheduleRequest at GET
// /navigation) go back to the car read-modify-write: the variant's
// ev-departures and ev-off-peak rebuilt from what their fields reflect, only
// the edited fields changed (Variant.ReflectedInput), then confirmed by the
// car's readback (writes.go). LIVI hears how it went as evScheduleWrite.

// scheduleWrites are the variant's messages that set the schedules, in the
// order an edit touching both sends them.
var scheduleWrites = []string{"ev-off-peak", "ev-departures"}

// days are the variant's departure day names, Monday first as LIVI's.
var days = []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// The read fields an edit names, by the variant's from_micom names.
const (
	fOffPeakStartHour = "Off-peak start hour"
	fOffPeakStartMin  = "Off-peak start minute bits"
	fOffPeakEndHour   = "Off-peak end hour"
	fOffPeakEndMin    = "Off-peak end minute bits"
	fChargingMode     = "Scheduled charging mode"
	fClimate          = "Scheduled climate"
	fClimateTemp      = "Scheduled climate temperature"
	fClimateDefrost   = "Scheduled climate defrost"
)

func departureField(n int, what string) string { return fmt.Sprintf("Departure %d %s", n, what) }

// minuteStep is what a minute-bits code counts: stock's pickers' tens.
const minuteStep = 10

// schedule is what the car last reported of its schedules: each read field
// a write reflects, as its raw code.
type schedule struct {
	v    *micom.Variant
	msgs map[string]bool   // the from_micom messages the writes reflect
	of   map[string]string // read field -> its from_micom message
	by   map[string]string // read field -> the write that sets it

	mu  sync.Mutex
	raw map[string]uint64 // by read field
}

func newSchedule(v *micom.Variant) *schedule {
	s := &schedule{v: v, msgs: map[string]bool{}, of: map[string]string{}, by: map[string]string{}, raw: map[string]uint64{}}
	for _, w := range scheduleWrites {
		for _, r := range v.Reflections(w) {
			s.msgs[r.Msg], s.of[r.From], s.by[r.From] = true, r.Msg, w
		}
	}
	return s
}

// take keeps what f reports of the schedules; true when it is one of theirs.
func (s *schedule) take(f micom.Frame) bool {
	got := false
	for msg, fs := range s.v.ReceivedFields(f) {
		if !s.msgs[msg] {
			continue
		}
		got = true
		s.mu.Lock()
		for name, r := range fs {
			if s.of[name] == msg && r.Valid {
				s.raw[name] = r.Raw
			}
		}
		s.mu.Unlock()
	}
	return got
}

func (s *schedule) snapshot() map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.raw)
}

// telemetry is the schedules as LIVI's fields, null where the car has not
// said, or said what no setting means (stock's hour 31 and minute 7 for none).
func (s *schedule) telemetry() map[string]any {
	raw := s.snapshot()
	pick := func(name string, ok func(uint64) bool, as func(uint64) any) any {
		if x, has := raw[name]; has && ok(x) {
			return as(x)
		}
		return nil
	}
	hour := func(name string) any {
		return pick(name, func(x uint64) bool { return x <= 23 }, func(x uint64) any { return float64(x) })
	}
	minute := func(name string) any {
		return pick(name, func(x uint64) bool { return x*minuteStep < 60 }, func(x uint64) any { return float64(x * minuteStep) })
	}
	flag := func(name string) any {
		return pick(name, func(x uint64) bool { return x <= 1 }, func(x uint64) any { return x == 1 })
	}
	departure := func(n int) any {
		ds := make([]any, len(days))
		for i, d := range days {
			ds[i] = flag(departureField(n, d))
		}
		return map[string]any{
			"on": flag(departureField(n, "enabled")), "hour": hour(departureField(n, "hour")),
			"minute": minute(departureField(n, "minute bits")), "days": ds,
		}
	}
	var mode any
	if x, has := raw[fChargingMode]; has {
		mode = fmt.Sprintf("code %d", x)
		if f := s.decoded(fChargingMode, x); f.Text != "" {
			mode = f.Text
		}
	}
	var temp any
	if x, has := raw[fClimateTemp]; has {
		temp = float64(x)
	}
	return map[string]any{
		"departure1": departure(1),
		"departure2": departure(2),
		"offPeak": map[string]any{
			"startHour": hour(fOffPeakStartHour), "startMinute": minute(fOffPeakStartMin),
			"endHour": hour(fOffPeakEndHour), "endMinute": minute(fOffPeakEndMin),
		},
		"scheduledChargingMode":    mode,
		"scheduledClimate":         flag(fClimate),
		"scheduledClimateDefrost":  flag(fClimateDefrost),
		"scheduledClimateTempCode": temp,
	}
}

// decoded is the read field name at raw code x, as the variant reads it.
func (s *schedule) decoded(name string, x uint64) micom.Field {
	msg := s.of[name]
	fr, err := s.v.EncodeReceivedRaw(msg, nil, map[string]uint64{name: x})
	if err != nil {
		return micom.Field{}
	}
	return s.v.ReceivedFields(fr)[msg][name].Field
}

// scheduleEdit is LIVI's change to the schedules: only what it names
// changes. Minutes are whole tens; the climate's temperature is not here,
// its encoding being unknown.
type scheduleEdit struct {
	Departure1     *departureEdit `json:"departure1,omitempty"`
	Departure2     *departureEdit `json:"departure2,omitempty"`
	OffPeak        *offPeakEdit   `json:"offPeak,omitempty"`
	ChargingMode   *string        `json:"chargingMode,omitempty"`
	Climate        *bool          `json:"climate,omitempty"`
	ClimateDefrost *bool          `json:"climateDefrost,omitempty"`
}

type departureEdit struct {
	On     *bool    `json:"on,omitempty"`
	Hour   *int     `json:"hour,omitempty"`
	Minute *int     `json:"minute,omitempty"`
	Days   *[7]bool `json:"days,omitempty"` // Monday first
}

type offPeakEdit struct {
	StartHour   *int `json:"startHour,omitempty"`
	StartMinute *int `json:"startMinute,omitempty"`
	EndHour     *int `json:"endHour,omitempty"`
	EndMinute   *int `json:"endMinute,omitempty"`
}

// codes is e as raw codes of the read fields it changes.
func (s *schedule) codes(e scheduleEdit) (map[string]uint64, error) {
	out := map[string]uint64{}
	var bad []string
	hour := func(name string, h *int) {
		switch {
		case h == nil:
		case *h < 0 || *h > 23:
			bad = append(bad, fmt.Sprintf("hour %d", *h))
		default:
			out[name] = uint64(*h)
		}
	}
	minute := func(name string, m *int) {
		switch {
		case m == nil:
		case *m < 0 || *m >= 60 || *m%minuteStep != 0:
			bad = append(bad, fmt.Sprintf("minute %d: the car takes tens", *m))
		default:
			out[name] = uint64(*m / minuteStep)
		}
	}
	flag := func(name string, b *bool) {
		if b != nil {
			out[name] = map[bool]uint64{false: 0, true: 1}[*b]
		}
	}
	for n, d := range []*departureEdit{e.Departure1, e.Departure2} {
		if d == nil {
			continue
		}
		flag(departureField(n+1, "enabled"), d.On)
		hour(departureField(n+1, "hour"), d.Hour)
		minute(departureField(n+1, "minute bits"), d.Minute)
		if d.Days != nil {
			for i, day := range days {
				flag(departureField(n+1, day), &d.Days[i])
			}
		}
	}
	if o := e.OffPeak; o != nil {
		hour(fOffPeakStartHour, o.StartHour)
		minute(fOffPeakStartMin, o.StartMinute)
		hour(fOffPeakEndHour, o.EndHour)
		minute(fOffPeakEndMin, o.EndMinute)
	}
	if e.ChargingMode != nil {
		x, err := s.v.RawCode(s.of[fChargingMode], fChargingMode, *e.ChargingMode)
		if err != nil {
			bad = append(bad, err.Error())
		}
		out[fChargingMode] = x
	}
	flag(fClimate, e.Climate)
	flag(fClimateDefrost, e.ClimateDefrost)
	for name := range out {
		if s.by[name] == "" {
			bad = append(bad, name+": no write sets it")
		}
	}
	if bad != nil {
		slices.Sort(bad)
		return nil, fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("nothing to change")
	}
	return out, nil
}

// plan is what to send for e: each write that sets a field e changes, built
// from the car's state with e over it.
func (s *schedule) plan(e scheduleEdit) ([]carWrite, error) {
	changes, err := s.codes(e)
	if err != nil {
		return nil, err
	}
	now := s.snapshot()
	maps.Copy(now, changes)
	var out []carWrite
	for _, w := range scheduleWrites {
		touched := false
		for name := range changes {
			touched = touched || s.by[name] == w
		}
		if !touched {
			continue
		}
		in, err := s.v.ReflectedInput(w, func(_, field string) (uint64, bool) {
			x, ok := now[field]
			return x, ok
		})
		if err != nil {
			return nil, err
		}
		in["msg"] = w
		b, _ := json.Marshal(in)
		want := map[string]uint64{}
		for _, r := range s.v.Reflections(w) {
			want[r.From] = now[r.From]
		}
		out = append(out, carWrite{w, string(b), want, func() []string { return s.unconfirmed(want) }})
	}
	return out, nil
}

// unconfirmed lists the fields of want the car does not report as wanted.
func (s *schedule) unconfirmed(want map[string]uint64) []string {
	raw := s.snapshot()
	var out []string
	for name, x := range want {
		if got, ok := raw[name]; !ok || got != x {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// planRequest is plan for LIVI's request body.
func (s *schedule) planRequest(set json.RawMessage) ([]carWrite, error) {
	var e scheduleEdit
	if err := strictJSON(set, &e); err != nil {
		return nil, err
	}
	return s.plan(e)
}

// parkedFrom is whether the car is known to be in Park: its driving
// regulation (the lockout it sets out of Park) known off, and no gear but P.
func parkedFrom(car *carState) func() (bool, string) {
	return func() (bool, string) {
		reg, gear := car.regulation.get(), car.gear.get()
		switch {
		case gear != nil && *gear != "P":
			return false, "the car is in " + *gear + ", not parked"
		case reg == nil:
			return false, "the car has not said it is parked"
		case *reg:
			return false, "the car is not parked"
		}
		return true, ""
	}
}
