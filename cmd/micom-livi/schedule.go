package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	micom "github.com/W-Floyd/go-micomd"
)

// The car's EV schedules (its two departure timers, the off-peak window and
// charging mode, the climate at departure), posted to LIVI as the car reports
// them. With -ev-schedule-writes, LIVI's edits (evScheduleRequest at GET
// /navigation) go back to the car read-modify-write: the variant's
// ev-departures and ev-off-peak rebuilt from what their fields reflect, only
// the edited fields changed (Variant.ReflectedInput), then confirmed by the
// car's readback. LIVI hears how it went as evScheduleWrite.

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

// scheduleSend is one write of an edit: micomd's input line, and the read
// fields the car must then report, at these codes.
type scheduleSend struct {
	msg  string
	line string
	want map[string]uint64
}

// plan is what to send for e: each write that sets a field e changes, built
// from the car's state with e over it.
func (s *schedule) plan(e scheduleEdit) ([]scheduleSend, error) {
	changes, err := s.codes(e)
	if err != nil {
		return nil, err
	}
	now := s.snapshot()
	maps.Copy(now, changes)
	var out []scheduleSend
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
		out = append(out, scheduleSend{w, string(b), want})
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

// scheduleRequest is LIVI's edit at GET /navigation: id tells a new one from
// the last, at (Unix seconds) one made before micom-livi started.
type scheduleRequest struct {
	ID  int64        `json:"id"`
	At  int64        `json:"at"`
	Set scheduleEdit `json:"set"`
}

// scheduleWrite is evScheduleWrite, how LIVI's edit id went: "sending",
// then "confirmed" or "failed" with why.
type scheduleWrite struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
	Why   string `json:"why,omitempty"`
}

// requestFresh is how old an edit may be when first seen: one older was
// made for a car state since gone.
const requestFresh = 15 * time.Second

// scheduleRunner sends LIVI's edits.
type scheduleRunner struct {
	s       *schedule
	input   string
	writes  bool          // -ev-schedule-writes
	confirm time.Duration // how long the car may take to report a write back
	parked  func() (bool, string)
	logf    func(string, ...any)
	post    func(key string, v any)
	now     func() time.Time
	// send writes one line to micomd's input; nil: over its own connection
	// to input.
	send func(line string) error

	mu   sync.Mutex
	req  *scheduleRequest
	conn net.Conn
	rd   *bufio.Reader

	last    int64 // the id last taken up
	pending []scheduleSend
	sentAt  time.Time
	cur     int64
}

func (r *scheduleRunner) observe(n *navigation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.req = nil
	if n != nil {
		r.req = n.EvScheduleRequest
	}
}

func (r *scheduleRunner) run(ctx context.Context) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if r.conn != nil {
				r.conn.Close()
			}
			return
		case <-tick.C:
			r.step()
		}
	}
}

func (r *scheduleRunner) report(w scheduleWrite) {
	if w.State != "sending" {
		r.logf("ev schedule: edit %d %s%s", w.ID, w.State, map[bool]string{true: ": " + w.Why}[w.Why != ""])
	}
	r.post("evScheduleWrite", w)
}

// step takes up a new edit, or checks the one sent against the car's readback.
func (r *scheduleRunner) step() {
	now := r.now()
	if r.pending != nil {
		left := r.s.unconfirmed(r.pending[0].want)
		switch {
		case len(left) == 0:
			r.pending = r.pending[1:]
			if len(r.pending) > 0 {
				r.sendNext(now)
				return
			}
			r.pending = nil
			r.report(scheduleWrite{ID: r.cur, State: "confirmed"})
		case now.Sub(r.sentAt) >= r.confirm:
			r.pending = nil
			r.report(scheduleWrite{ID: r.cur, State: "failed",
				Why: fmt.Sprintf("the car did not report it back within %s (%s)", r.confirm, strings.Join(left, ", "))})
		}
		return
	}
	r.mu.Lock()
	req := r.req
	r.mu.Unlock()
	if req == nil || req.ID == r.last {
		return
	}
	r.last, r.cur = req.ID, req.ID
	fail := func(why string) { r.report(scheduleWrite{ID: req.ID, State: "failed", Why: why}) }
	if !r.writes {
		fail("writes are off: micom-livi runs without -ev-schedule-writes")
		return
	}
	if age := now.Sub(time.Unix(req.At, 0)); age > requestFresh {
		fail(fmt.Sprintf("the edit is %d s old", int(age.Seconds())))
		return
	}
	if ok, why := r.parked(); !ok {
		fail(why)
		return
	}
	sends, err := r.s.plan(req.Set)
	if err != nil {
		fail(err.Error())
		return
	}
	r.report(scheduleWrite{ID: req.ID, State: "sending"})
	r.pending = sends
	r.sendNext(now)
}

func (r *scheduleRunner) sendNext(now time.Time) {
	s := r.pending[0]
	r.logf("ev schedule: edit %d: %s", r.cur, s.line)
	if err := r.write(s.line); err != nil {
		r.pending = nil
		r.report(scheduleWrite{ID: r.cur, State: "failed", Why: err.Error()})
		return
	}
	r.sentAt = now
}

func (r *scheduleRunner) write(line string) error {
	if r.send != nil {
		return r.send(line)
	}
	if r.conn == nil {
		c, err := net.Dial("unix", r.input)
		if err != nil {
			return err
		}
		r.conn, r.rd = c, bufio.NewReader(c)
	}
	err := sendInput(r.conn, r.rd, line)
	if _, refused := err.(refusal); err != nil && !refused {
		r.conn.Close()
		r.conn = nil
	}
	return err
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
