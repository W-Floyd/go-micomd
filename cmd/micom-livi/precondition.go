package main

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"net"
	"sync"
	"time"
)

// chargingStop is LIVI's next stop of the route when it is a charger, as
// Android Auto's guidance gives it: power in W, distances in m, times in s,
// energies in Wh.
type chargingStop struct {
	PowerW      int64  `json:"powerW"`
	DistanceM   *int64 `json:"distanceM"`
	TimeS       *int64 `json:"timeS"`
	ChargeS     *int64 `json:"chargeS"`
	DepartureWh *int64 `json:"departureWh"`
	ArrivalWh   *int64 `json:"arrivalWh"`
}

// precondRules are when the car is asked to warm its battery for the next
// charging stop. Android Auto says nothing of DC or AC: a charger's power
// is the only tell.
type precondRules struct {
	minPowerW float64       // DC: chargers above this
	maxTime   time.Duration // start this close to the stop, not before
	minDistM  float64       // closer than this is arrived
	minSoC    float64       // %, the car's displayed charge, now
	// minArrival is the % of capacityWh the forecast must leave on
	// arrival; the BMS would spend it warming.
	minArrival float64
	capacityWh float64 // 0: no arrival check
	maxStarts  int     // per route, so a stop that flaps cannot cycle the heater
}

// precond decides, step by step, whether conditioning should run; it sends
// nothing itself.
type precond struct {
	rules   precondRules
	running bool
	starts  int
	sentAt  time.Time
	why     string // the last reason it is not running, for the log
}

type precondOp int

const (
	opNone    precondOp = iota
	opStart             // the route, then the station event
	opRefresh           // the route again, every precondRefresh
	opStop              // the cancel
)

// precondRefresh is how often the route goes again while running, as
// stock's navigation and Launchpad send it.
const precondRefresh = 5 * time.Second

// want is whether conditioning should run for n with the car at soc (nil
// while unknown), and why not.
func (p *precond) want(n *navigation, soc *float64) (bool, string) {
	r := p.rules
	switch {
	case n == nil:
		return false, "LIVI unreadable"
	case n.Active == nil || !*n.Active:
		return false, "no route"
	case n.ChargingStop == nil:
		return false, "the next stop is not a charger"
	}
	cs := n.ChargingStop
	switch {
	case float64(cs.PowerW) <= r.minPowerW:
		return false, fmt.Sprintf("the charger is %d kW, not above %g", cs.PowerW/1000, r.minPowerW/1000)
	case cs.TimeS == nil || cs.DistanceM == nil:
		return false, "no time or distance to the charger"
	case time.Duration(*cs.TimeS)*time.Second > r.maxTime:
		return false, fmt.Sprintf("the charger is %d min away", *cs.TimeS/60)
	case float64(*cs.DistanceM) <= r.minDistM:
		return false, "at the charger"
	case soc == nil:
		return false, "state of charge unknown"
	case *soc < r.minSoC:
		return false, fmt.Sprintf("state of charge %g %% is below %g %%", *soc, r.minSoC)
	case r.capacityWh > 0 && cs.ArrivalWh != nil && float64(*cs.ArrivalWh) < r.minArrival/100*r.capacityWh:
		return false, fmt.Sprintf("%d Wh forecast on arrival", *cs.ArrivalWh)
	}
	return true, ""
}

// step is what to send now.
func (p *precond) step(n *navigation, soc *float64, now time.Time) precondOp {
	if n != nil && (n.Active == nil || !*n.Active) {
		p.starts = 0 // a new route starts afresh
	}
	ok, why := p.want(n, soc)
	p.why = why
	switch {
	case p.running && !ok:
		p.running = false
		return opStop
	case p.running && now.Sub(p.sentAt) >= precondRefresh:
		p.sentAt = now
		return opRefresh
	case !p.running && ok && p.starts < p.rules.maxStarts:
		p.running, p.sentAt = true, now
		p.starts++
		return opStart
	case !p.running && ok:
		p.why = fmt.Sprintf("%d starts on this route already", p.starts)
	}
	return opNone
}

// routeInputs are micomd's batt-cond route lines for the stop: stock's
// fields cap at 2046 km and 1022 min, 2047 and 1023 meaning none.
func routeInputs(cs *chargingStop) []string {
	km := min(math.Round(float64(*cs.DistanceM)/1000), 2046)
	minutes := min(math.Ceil(float64(*cs.TimeS)/60), 1022)
	return []string{
		fmt.Sprintf(`{"msg":"batt-cond-route-distance","km":%d}`, int(km)),
		fmt.Sprintf(`{"msg":"batt-cond-remain-time","minutes":%d}`, int(minutes)),
	}
}

const (
	stationStart   = `{"msg":"batt-cond-ev-station","flag":2,"type":3}`
	stationStop    = `{"msg":"batt-cond-ev-station","flag":0,"type":0}`
	stationInvalid = `{"msg":"batt-cond-ev-station","flag":7,"type":7}`
	// Stock's HEVService sends station 07 07 this long after every station
	// event: the event is a pulse.
	stationPulse = 120 * time.Millisecond
)

var routeNone = []string{`{"msg":"batt-cond-route-distance","km":2047}`, `{"msg":"batt-cond-remain-time","minutes":1023}`}

// precondRunner runs precond against LIVI's latest guidance (observe) and
// the car's state of charge, over its own connection to micomd's -input.
type precondRunner struct {
	p     precond
	input string
	soc   func() *float64
	cond  func() *bool // the car's own conditioning status, nil unknown
	logf  func(string, ...any)

	mu  sync.Mutex
	nav *navigation
	seen bool
}

func (r *precondRunner) observe(n *navigation) {
	r.mu.Lock()
	r.nav, r.seen = n, true
	r.mu.Unlock()
}

func (r *precondRunner) run(ctx context.Context) {
	var (
		conn    net.Conn
		rd      *bufio.Reader
		said    string
		carSaid *bool
		askedAt time.Time // the last start
		warned  bool      // that the car has not followed it
	)
	send := func(lines ...string) error {
		for _, l := range lines {
			if conn == nil {
				c, err := net.Dial("unix", r.input)
				if err != nil {
					return err
				}
				conn, rd = c, bufio.NewReader(c)
			}
			if l == "" {
				time.Sleep(stationPulse)
				continue
			}
			if err := sendInput(conn, rd, l); err != nil {
				if _, refused := err.(refusal); !refused {
					conn.Close()
					conn = nil
				}
				return err
			}
		}
		return nil
	}
	exec := func(op precondOp, cs *chargingStop) {
		var err error
		switch op {
		case opStart:
			r.logf("battery conditioning: start, the %d kW charger %d m and %d s away", cs.PowerW/1000, *cs.DistanceM, *cs.TimeS)
			err = send(append(routeInputs(cs), stationStart, "", stationInvalid)...)
		case opRefresh:
			err = send(routeInputs(cs)...)
		case opStop:
			r.logf("battery conditioning: stop (%s)", r.p.why)
			err = send(append(routeNone, stationStop, "", stationInvalid)...)
		}
		if err != nil {
			r.logf("battery conditioning: %v", err)
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if r.p.running {
				r.p.why = "micom-livi stopping"
				exec(opStop, nil)
			}
			if conn != nil {
				conn.Close()
			}
			return
		case <-tick.C:
		}
		r.mu.Lock()
		n, seen := r.nav, r.seen
		r.mu.Unlock()
		if !seen {
			continue
		}
		op := r.p.step(n, r.soc(), time.Now())
		var cs *chargingStop
		if n != nil {
			cs = n.ChargingStop
		}
		exec(op, cs)
		if !r.p.running && r.p.why != said {
			r.logf("battery conditioning: not asked: %s", r.p.why)
			said = r.p.why
		}
		now := time.Now()
		if op == opStart {
			askedAt, warned = now, false
		}
		c := r.cond()
		if c != nil && (carSaid == nil || *carSaid != *c) {
			msg := "battery conditioning: the car reports it " + map[bool]string{true: "on", false: "off"}[*c]
			switch {
			case *c && r.p.running:
				msg += fmt.Sprintf(", %d s after the request", int(now.Sub(askedAt).Seconds()))
			case *c:
				msg += ", not asked by micom-livi"
			}
			r.logf("%s", msg)
			carSaid = c
		}
		if r.p.running && !warned && (c == nil || !*c) && now.Sub(askedAt) >= confirmWithin {
			r.logf("battery conditioning: asked %d s ago and the car has not started: the pack warm enough, conditioning off in its EV settings, or not READY", int(now.Sub(askedAt).Seconds()))
			warned = true
		}
	}
}

// confirmWithin is how long the car may take to start conditioning before
// the log says it has not: it took 68 s parked, READY, at 18 °C.
const confirmWithin = 2 * time.Minute
