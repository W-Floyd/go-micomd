package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
)

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

const endGuidance = `{"msg":"tbt-turn","end":true}`

// bridge turns LIVI's guidance into micomd's input lines: tbt-guiding on
// when a route starts, the maneuver and summary on change and again every
// refresh (cluster-sub-info ends 5 s after its last input), and the stock
// end of route when guidance stops, LIVI is gone, or at the start.
type bridge struct {
	refresh time.Duration
	guiding bool
	cleared bool
	sent    map[string]string
	at      map[string]time.Time
}

func newBridge(refresh time.Duration) *bridge {
	return &bridge{refresh: refresh, sent: map[string]string{}, at: map[string]time.Time{}}
}

// reset forgets what micomd was told, for a new connection to it.
func (b *bridge) reset() {
	b.guiding, b.cleared = false, false
	clear(b.sent)
	clear(b.at)
}

// step is the lines to send for n, nil when LIVI has no guidance or cannot
// be read.
func (b *bridge) step(n *navigation, now time.Time, o navOptions) []string {
	if n == nil || n.Active == nil || !*n.Active {
		if b.cleared {
			return nil
		}
		b.reset()
		b.cleared = true
		return []string{endGuidance}
	}
	var out []string
	b.cleared = false
	if !b.guiding {
		b.guiding = true
		out = append(out, `{"msg":"tbt-guiding","guiding":true}`)
	}
	turn, sub := clusterInputs(n, now, o)
	for _, in := range []map[string]any{turn, sub} {
		msg := in["msg"].(string)
		line, _ := json.Marshal(in)
		if string(line) == b.sent[msg] && now.Sub(b.at[msg]) < b.refresh {
			continue
		}
		b.sent[msg], b.at[msg] = string(line), now
		out = append(out, string(line))
	}
	return out
}

// runNav polls LIVI's guidance every poll and feeds micomd's -input socket,
// reconnecting to either; logf hears each new problem once.
func runNav(navURL, input string, poll time.Duration, o navOptions, logf func(string, ...any)) {
	client := &http.Client{Timeout: 2 * time.Second}
	b := newBridge(time.Second)
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
	for range time.Tick(poll) {
		n, err := fetchNav(client, navURL)
		note(err)
		if conn == nil {
			if conn, err = net.Dial("unix", input); err != nil {
				conn = nil
				note(err)
				continue
			}
			rd = bufio.NewReader(conn)
			b.reset()
		}
		for _, line := range b.step(n, time.Now(), o) {
			if err := sendInput(conn, rd, line); err != nil {
				note(err)
				if _, refused := err.(refusal); !refused {
					conn.Close()
					conn = nil
					break
				}
			}
		}
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
