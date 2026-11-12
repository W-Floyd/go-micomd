package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// LIVI's changes to settings the car holds (the schedules, schedule.go; the
// charge limits and current, charge.go): LIVI states one at GET /navigation,
// micom-livi sends it through micomd's input while parked, confirms it by the
// car's readback, and reports how it went.

// carWrite is one frame of a change: micomd's input line, the read fields the
// car must then report at these raw codes, and which of them it does not yet.
type carWrite struct {
	msg  string
	line string
	want map[string]uint64
	left func() []string
}

// writeRequest is LIVI's change: id tells a new one from the last, at (Unix
// seconds) one made before micom-livi started.
type writeRequest struct {
	ID  int64           `json:"id"`
	At  int64           `json:"at"`
	Set json.RawMessage `json:"set"`
}

// writeReport is how LIVI's change id went: "sending", then "confirmed" or
// "failed" with why.
type writeReport struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
	Why   string `json:"why,omitempty"`
}

// requestFresh is how old a change may be when first seen: one older was
// made for a car state since gone.
const requestFresh = 15 * time.Second

// writeRunner sends one kind of LIVI's changes.
type writeRunner struct {
	what    string // in the log: "ev schedule"
	flag    string // the flag that turns writes on
	key     string // the report's telemetry field
	pick    func(*navigation) *writeRequest
	plan    func(json.RawMessage) ([]carWrite, error)
	input   string
	writes  bool
	confirm time.Duration // how long the car may take to report a write back
	parked  func() (bool, string)
	logf    func(string, ...any)
	post    func(key string, v any)
	now     func() time.Time
	// send writes one line to micomd's input; nil: over its own connection
	// to input.
	send func(line string) error

	mu   sync.Mutex
	req  *writeRequest
	conn net.Conn
	rd   *bufio.Reader

	last    int64 // the id last taken up
	pending []carWrite
	sentAt  time.Time
	cur     int64
}

func (r *writeRunner) observe(n *navigation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.req = nil
	if n != nil {
		r.req = r.pick(n)
	}
}

func (r *writeRunner) run(ctx context.Context) {
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

func (r *writeRunner) report(w writeReport) {
	if w.State != "sending" {
		r.logf("%s: edit %d %s%s", r.what, w.ID, w.State, map[bool]string{true: ": " + w.Why}[w.Why != ""])
	}
	r.post(r.key, w)
}

// step takes up a new change, or checks the one sent against the car's readback.
func (r *writeRunner) step() {
	now := r.now()
	if r.pending != nil {
		left := r.pending[0].left()
		switch {
		case len(left) == 0:
			r.pending = r.pending[1:]
			if len(r.pending) > 0 {
				r.sendNext(now)
				return
			}
			r.pending = nil
			r.report(writeReport{ID: r.cur, State: "confirmed"})
		case now.Sub(r.sentAt) >= r.confirm:
			r.pending = nil
			r.report(writeReport{ID: r.cur, State: "failed",
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
	fail := func(why string) { r.report(writeReport{ID: req.ID, State: "failed", Why: why}) }
	if !r.writes {
		fail("writes are off: micom-livi runs without " + r.flag)
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
	sends, err := r.plan(req.Set)
	if err != nil {
		fail(err.Error())
		return
	}
	if len(sends) == 0 {
		// Stock sends nothing for what the car already holds.
		r.report(writeReport{ID: req.ID, State: "confirmed"})
		return
	}
	r.report(writeReport{ID: req.ID, State: "sending"})
	r.pending = sends
	r.sendNext(now)
}

func (r *writeRunner) sendNext(now time.Time) {
	s := r.pending[0]
	r.logf("%s: edit %d: %s", r.what, r.cur, s.line)
	if err := r.write(s.line); err != nil {
		r.pending = nil
		r.report(writeReport{ID: r.cur, State: "failed", Why: err.Error()})
		return
	}
	r.sentAt = now
}

func (r *writeRunner) write(line string) error {
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

// strictJSON decodes b into v, refusing a field v does not have: a LIVI newer
// than micom-livi asks for what it cannot do.
func strictJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("the edit: %v", err)
	}
	return nil
}
