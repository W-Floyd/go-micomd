package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// latest is a value the tap last told, shared between goroutines.
type latest[T any] struct {
	mu sync.Mutex
	v  *T
}

func (s *latest[T]) set(v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.v = &v
}

// get is nil while unknown.
func (s *latest[T]) get() *T {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v
}

// nightState is the car's day or night (the micom's illumination), for the
// cluster's look.
type nightState = latest[bool]

// navigation is the part of LIVI's GET /navigation the cluster takes: the
// phone's route guidance, Android Auto's or CarPlay's, in one model.
// maneuverType is LIVI's ManeuverType (CarPlay's numbering, which LIVI maps
// Android Auto onto); distances are metres, times seconds.
type navigation struct {
	Active                *bool    `json:"active"`
	ManeuverType          *int     `json:"maneuverType"`
	TurnSide              *int     `json:"turnSide"` // 0 right, 1 left
	TurnAngle             *int     `json:"turnAngle"`
	RemainDistance        *float64 `json:"remainDistance"` // to the next maneuver
	DistanceToDestination *float64 `json:"distanceToDestination"`
	TimeToDestination     *float64 `json:"timeToDestination"`
	Eta                   *float64 `json:"eta"`     // Unix seconds (CarPlay)
	EtaText               *string  `json:"etaText"` // "16:47" (Android Auto)
	// ChargingStop is the next stop when it is a charger (Android Auto).
	ChargingStop *chargingStop `json:"chargingStop"`
	// Precondition is LIVI's preconditioning settings, nil from a LIVI
	// without them (auto).
	Precondition *preconditionPrefs `json:"precondition"`
}

// navOptions are how the cluster shows distances and the time.
type navOptions struct {
	imperial bool
	clock24  bool
	loc      *time.Location
}

// clusterManeuver is the cluster's display type (the variant's enum
// maneuver) for LIVI's maneuver type and turn side. The cluster's codes are
// GpmNaviTBTManager's (the NEMESIS report): where it has no match the
// closest generic turn stands in, and the roundabout's exit-angle codes
// (96-106) are not used, their angles being unknown.
func clusterManeuver(t, side *int) string {
	if t == nil {
		return "unknown"
	}
	left := side != nil && *side == 1
	right := side != nil && *side == 0
	pick := func(l, r, neither string) string {
		switch {
		case left:
			return l
		case right:
			return r
		}
		return neither
	}
	switch *t {
	case 0, 3, 5: // no turn, straight, follow the road
		return "straight"
	case 1, 20: // left, end of road left
		return "turn_left"
	case 2, 21:
		return "turn_right"
	// A U-turn's side is the way it bends; without one, a U-turn in
	// right-hand traffic bends left. Which of 72/73 is which is a guess.
	case 4, 18, 19, 26:
		return pick("uturn_left", "uturn_right", "uturn_left")
	// Roundabouts by side, counter-clockwise (exit right) without one; the
	// exit number has no field.
	case 6, 7:
		return pick("roundabout_exit_left", "roundabout_exit_right", "roundabout_exit_right")
	case 8:
		return pick("off_ramp_left", "off_ramp_right", "off_ramp")
	case 22:
		return "off_ramp_left"
	case 23:
		return "off_ramp_right"
	case 9, 51: // on ramp, change highway: stock's on-ramp codes are also its fork
		return pick("on_ramp_left", "on_ramp_right", "on_ramp")
	case 13, 52:
		return "on_ramp_left"
	case 14, 53:
		return "on_ramp_right"
	case 10, 12, 24, 25, 27: // the end, arrived (left, right): only 115 is known to be arrived
		return "arrived"
	case 11: // proceed to the route: stock's depart, sent with no distance
		return "depart"
	case 15, 16, 17:
		return "ferry"
	case 47:
		return "sharp_left"
	case 48:
		return "sharp_right"
	case 49:
		return "slight_left"
	case 50:
		return "slight_right"
	}
	if *t >= 28 && *t <= 46 { // roundabout exit 1-19
		return pick("roundabout_exit_left", "roundabout_exit_right", "roundabout_exit_right")
	}
	return "unknown"
}

// clusterDistance is a distance in metres as the cluster shows it: whole
// units, tenths, and the unit. Feet below 0.1 mi and metres below 1 km, in
// tens; miles or km with their tenths otherwise (the cluster draws the tenth
// at any distance).
func clusterDistance(m float64, imperial bool) (whole, tenths int, unit string) {
	small, big, step := "m", "km", 1000.0
	if imperial {
		small, big, step = "ft", "mi", 1609.344
	}
	switch {
	case imperial && m < step/10:
		return min(int(math.Round(m*3.28084/10))*10, 65534), 0, small
	case !imperial && m < step:
		return min(int(math.Round(m/10))*10, 65534), 0, small
	}
	t := int(math.Round(m / step * 10))
	if t/10 > 65534 {
		return 65534, 0, big
	}
	return t / 10, t % 10, big
}

// clusterEta is the arrival time on the unit's clock.
func clusterEta(n *navigation, now time.Time, loc *time.Location) (h, m int, ok bool) {
	if n.Eta != nil && *n.Eta > 0 {
		t := time.Unix(int64(*n.Eta), 0).In(loc)
		return t.Hour(), t.Minute(), true
	}
	if n.EtaText != nil {
		s := strings.TrimSpace(*n.EtaText)
		for _, layout := range []string{"15:04", "3:04 PM", "3:04PM", "3:04 pm", "3:04pm"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.Hour(), t.Minute(), true
			}
		}
	}
	if n.TimeToDestination != nil && *n.TimeToDestination >= 0 {
		t := now.Add(time.Duration(*n.TimeToDestination) * time.Second).In(loc)
		return t.Hour(), t.Minute(), true
	}
	return 0, 0, false
}

// clusterInputs are micomd's -input lines for the guidance now: the next
// maneuver (tbt-turn) and the route summary (cluster-sub-info, without the
// compass, which has its own source).
func clusterInputs(n *navigation, now time.Time, o navOptions) (turn, sub map[string]any) {
	turn = map[string]any{"msg": "tbt-turn", "maneuver": clusterManeuver(n.ManeuverType, n.TurnSide),
		"side_street": nil, "direction": 0, "distance": nil, "unit": nil, "distance_tenths": nil, "bar": nil}
	if n.TurnAngle != nil {
		turn["direction"] = ((*n.TurnAngle % 360) + 360) % 360
	}
	if n.RemainDistance != nil && *n.RemainDistance >= 0 && turn["maneuver"] != "depart" {
		turn["distance"], turn["distance_tenths"], turn["unit"] = clusterDistance(*n.RemainDistance, o.imperial)
	}
	timeFormat := 1 // stock: 1 12 h, 2 24 h
	if o.clock24 {
		timeFormat = 2
	}
	sub = map[string]any{"msg": "cluster-sub-info", "time_type": 1, "time_format": timeFormat,
		"remaining": nil, "remaining_unit": nil, "remaining_tenths": nil, "eta_hour": nil, "eta_minute": nil}
	if n.DistanceToDestination != nil && *n.DistanceToDestination >= 0 {
		sub["remaining"], sub["remaining_tenths"], sub["remaining_unit"] = clusterDistance(*n.DistanceToDestination, o.imperial)
	}
	if h, m, ok := clusterEta(n, now, o.loc); ok {
		sub["eta_hour"], sub["eta_minute"] = h, m
	}
	return turn, sub
}

// The stock lines: GPM's end of route (micomd's tbt-turn end), the claim of
// the cluster's navigation pane (alive, then operating) and its release.
const (
	endGuidance = `{"msg":"tbt-turn","end":true}`
	claimAlive  = `{"msg":"tbt-alive","alive":true}`
	claimOwn    = `{"msg":"nav-operation","status":"operating"}`
	freeAlive   = `{"msg":"tbt-alive","alive":false}`
	freeOwn     = `{"msg":"nav-operation","status":"released"}`
)

// bridge turns LIVI's guidance into micomd's input lines. It claims the
// cluster's navigation pane (operating) and keeps it. tbt-alive is on only
// while a route is: stock's own navigation never sends it, and with it on
// the EV6 cluster drops the no-route compass after a while, while with it
// off and operating the compass stays. With no route the pane shows the
// compass (display type no_route_guide), with one the maneuver and summary;
// these go on change and again every refresh (cluster-sub-info ends 5 s
// after its last input). A route's end is stock's end frames, then the
// compass pane again. The pane is released only on release: at exit, or
// when LIVI has been gone for releaseAfter.
type bridge struct {
	refresh time.Duration
	// releaseAfter, when positive, releases the pane once LIVI has been
	// unreadable this long.
	releaseAfter time.Duration
	// heading, when set, is the compass in degrees (nil for none).
	heading func(time.Time) *float64
	// night, when set, is the cluster's day or night look, nil while unknown.
	night func() *bool
	// observe, when set, hears each read of LIVI's guidance, nil when it
	// could not be read.
	observe func(*navigation)

	owned    bool
	routing  bool
	alive    bool // tbt-alive as last sent while owned
	released bool // by releaseAfter: claimed again when LIVI is back
	seen     time.Time
	sent     map[string]string
	at       map[string]time.Time
}

// idleTurn is tbt-turn with no route: stock's "no route guidance (compass)"
// symbol, in the bytes of stock's end of route. The EV6 cluster draws this
// pane's compass from the turn's direction, counter-clockwise (direction 90
// showed west, 270 east), not from cluster-sub-info's compass byte: so the
// heading goes in as 360 - heading. North while there is no heading.
func idleTurn(compass *float64) map[string]any {
	direction := 0.0
	if compass != nil {
		direction = math.Mod(360-*compass, 360)
	}
	return map[string]any{"msg": "tbt-turn", "maneuver": "no_route_guide", "side_street": 0, "direction": direction,
		"distance": nil, "unit": "m", "side_street_combi": 0, "bar": nil, "distance_tenths": 0}
}

// idleSubInfo is cluster-sub-info with no route: the route fields invalid
// and time_type and time_format the captured 3, which is the captured ack
// frame (the stock system's idle 0x0D03 on the wire), the compass aside.
func idleSubInfo(compass *float64) map[string]any {
	return map[string]any{"msg": "cluster-sub-info", "compass": compass, "time_type": 3, "time_format": 3,
		"remaining": nil, "remaining_unit": nil, "remaining_tenths": nil, "eta_hour": nil, "eta_minute": nil, "est_point_type": nil}
}

// emit adds in's line to out when it changed, or with refresh when it is
// due again.
func (b *bridge) emit(out []string, in map[string]any, now time.Time, refresh bool) []string {
	msg := in["msg"].(string)
	line, _ := json.Marshal(in)
	if string(line) == b.sent[msg] && (!refresh || now.Sub(b.at[msg]) < b.refresh) {
		return out
	}
	b.sent[msg], b.at[msg] = string(line), now
	return append(out, string(line))
}

func newBridge(refresh time.Duration) *bridge {
	return &bridge{refresh: refresh, sent: map[string]string{}, at: map[string]time.Time{}}
}

// reset forgets what micomd was told, for a new connection to it.
func (b *bridge) reset() {
	b.owned, b.routing = false, false
	clear(b.sent)
	clear(b.at)
}

// release is the lines that give the pane back, nil when it is not held.
func (b *bridge) release() []string {
	if !b.owned {
		return nil
	}
	b.reset()
	return []string{freeAlive, freeOwn}
}

// step is the lines to send for n, which is nil when LIVI cannot be read.
func (b *bridge) step(n *navigation, now time.Time, o navOptions) []string {
	if n != nil || b.seen.IsZero() {
		b.seen = now
	}
	if n == nil && b.releaseAfter > 0 && now.Sub(b.seen) >= b.releaseAfter {
		if b.owned {
			b.released = true
		}
		return b.release()
	}
	if b.released && n == nil {
		return nil
	}
	b.released = false
	var out []string
	route := n != nil && n.Active != nil && *n.Active
	if !b.owned {
		b.owned = true
		if route {
			out = append(out, claimAlive, claimOwn)
		} else {
			out = append(out, freeAlive, claimOwn)
		}
		b.alive = route
	}
	if b.night != nil {
		if night := b.night(); night != nil {
			mode := "day"
			if *night {
				mode = "night"
			}
			out = b.emit(out, map[string]any{"msg": "cluster-day-night", "mode": mode}, now, false)
		}
	}
	var compass *float64
	if b.heading != nil {
		compass = b.heading(now)
	}
	if !route {
		if b.routing {
			b.routing = false
			delete(b.sent, "tbt-turn")
			delete(b.sent, "cluster-sub-info")
			out = append(out, endGuidance)
		}
		if b.alive {
			b.alive = false
			out = append(out, freeAlive)
		}
		return b.emit(b.emit(out, idleTurn(compass), now, true), idleSubInfo(compass), now, true)
	}
	b.routing = true
	if !b.alive {
		b.alive = true
		out = append(out, claimAlive)
	}
	turn, sub := clusterInputs(n, now, o)
	if b.heading != nil {
		sub["compass"] = compass
	}
	return b.emit(b.emit(out, turn, now, true), sub, now, true)
}

// runNav polls LIVI's guidance every poll and feeds micomd's -input socket
// through b, reconnecting to either, until ctx ends; then it releases the
// pane. logf hears each new problem once.
func runNav(ctx context.Context, navURL, input string, poll time.Duration, o navOptions, b *bridge, logf func(string, ...any)) {
	client := &http.Client{Timeout: 2 * time.Second}
	var (
		conn net.Conn
		rd   *bufio.Reader
		said string
	)
	note := func(err error) {
		if s := fmt.Sprint(err); err != nil && s != said {
			logf("navigation: %v", err)
			said = s
		} else if err == nil {
			said = ""
		}
	}
	send := func(lines []string) {
		for _, line := range lines {
			if err := sendInput(conn, rd, line); err != nil {
				note(err)
				if _, refused := err.(refusal); !refused {
					conn.Close()
					conn = nil
					return
				}
			}
		}
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if conn != nil {
				send(b.release())
				if conn != nil {
					conn.Close()
				}
			}
			return
		case <-tick.C:
		}
		n, err := fetchNav(client, navURL)
		note(err)
		if b.observe != nil {
			b.observe(n)
		}
		if conn == nil {
			if conn, err = net.Dial("unix", input); err != nil {
				conn = nil
				note(err)
				continue
			}
			rd = bufio.NewReader(conn)
			b.reset()
		}
		send(b.step(n, time.Now(), o))
	}
}

func fetchNav(c *http.Client, url string) (*navigation, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	var n navigation
	if err := json.NewDecoder(resp.Body).Decode(&n); err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	return &n, nil
}

// refusal is micomd's answer to a line it would not send.
type refusal string

func (r refusal) Error() string { return string(r) }

func sendInput(c net.Conn, rd *bufio.Reader, line string) error {
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprintln(c, line); err != nil {
		return err
	}
	ans, err := rd.ReadString('\n')
	if err != nil {
		return err
	}
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(ans), &r) != nil || !r.OK {
		return refusal(fmt.Sprintf("micomd refused %s: %s", line, strings.TrimSpace(ans)))
	}
	return nil
}
