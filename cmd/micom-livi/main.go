// Command micom-livi feeds the car's state, as micomd decodes it, into LIVI's
// telemetry: it reads micomd's tap socket and posts the climate (panel and seat
// heat) to LIVI's telemetry endpoint as the car's ("source": "car"), and
// speedKph, fuelPct, batteryLevelKwh, rangeKm, rangeClimateOffKm, ambientC,
// odometerKm and nightMode (the micom's illumination), each when it changes
// (carFields, liviCar).
// With -input it also reads LIVI's route guidance (GET /navigation) and shows
// it on the instrument cluster through micomd's -input socket (nav.go), with
// -gpsd the compass and, while there is a fix, the unit's position.
//
//	micom-livi [-tap /run/micomd.sock] [-url http://127.0.0.1:4000/telemetry] [-every 250ms] [-capacity-kwh N]
//
// It only reads: micomd owns the link, and nothing goes back to the car. It is
// a client of both sides, so either can restart: it reconnects to the tap, and
// posts the whole state again once LIVI answers after a failure (LIVI restores
// its own saved climate at start, which the car's must replace).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	micom "github.com/W-Floyd/go-micomd"
)

func main() {
	tap := flag.String("tap", "/run/micomd.sock", "micomd's -tap socket")
	url := flag.String("url", "http://127.0.0.1:4000/telemetry", "LIVI's telemetry endpoint")
	settle := flag.Duration("settle", 300*time.Millisecond, "wait this long after a change outside a burst before posting")
	every := flag.Duration("every", 250*time.Millisecond, "post at most this often; changes in between are posted together, the newest of each")
	capacity := flag.Float64("capacity-kwh", 0, "post this as batteryCapacityKwh, LIVI's gross capacity (77.4 for the EV6 long-range pack); 0 for none, as the micom does not report it")
	input := flag.String("input", "", "micomd's -input socket: show LIVI's route guidance on the cluster through it (needs micomd -allow-inferred); empty for none")
	navURL := flag.String("nav-url", "http://127.0.0.1:4000/navigation", "LIVI's route guidance, read with -input")
	navEvery := flag.Duration("nav-every", 500*time.Millisecond, "how often to read LIVI's route guidance")
	units := flag.String("units", "imperial", "the cluster's distances: imperial (mi, ft) or metric (km, m)")
	clock24 := flag.Bool("clock24", false, "show the arrival time on the cluster as 24 h, not 12 h")
	gpsd := flag.String("gpsd", "", "gpsd's host:port: the cluster's compass from its track and its position from the fix, with -input; empty for none")
	minSpeed := flag.Float64("heading-min-speed", 0, "m/s: take the track as the heading only at this speed or above, for a plain GNSS track; 0 takes every track (hippod's is the gyro-held DR bearing)")
	hold := flag.Duration("heading-hold", 10*time.Minute, "with -heading-min-speed, how long the last heading stays on the compass below it")
	releaseAfter := flag.Duration("release-after", 0, "give the cluster's navigation pane back once LIVI has been unreadable this long; 0 keeps it until micom-livi stops")
	precondition := flag.Bool("precondition", false, "with -input, ask the car to warm its battery while Android Auto's route leads to a fast charger (the variant's batt-cond-* messages, as stock's navigation does); the BMS decides")
	pcMinKw := flag.Float64("precondition-min-kw", 50, "kW: only for chargers above this, Android Auto giving no DC or AC")
	pcMaxTime := flag.Duration("precondition-max-time", time.Hour, "start this close to the charger")
	pcMinSoC := flag.Float64("precondition-min-soc", 20, "%: the car's charge it needs, now")
	pcMinArrival := flag.Float64("precondition-min-arrival", 5, "% of -capacity-kwh the forecast must leave on arrival")
	flag.Parse()

	lg := log.New(os.Stderr, "", log.LstdFlags)
	if *units != "imperial" && *units != "metric" {
		lg.Fatalf("-units %q: imperial or metric", *units)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	car := &carState{}
	navDone, pcDone := make(chan struct{}), make(chan struct{})
	var observe func(*navigation)
	if *input != "" && *precondition {
		r := &precondRunner{
			p: precond{rules: precondRules{
				minPowerW: *pcMinKw * 1000, maxTime: *pcMaxTime, minDistM: 200, minSoC: *pcMinSoC,
				minArrival: *pcMinArrival, capacityWh: *capacity * 1000, maxStarts: 3,
			}},
			input: *input, soc: car.soc.get, cond: car.cond.get, logf: lg.Printf,
		}
		go func() {
			r.run(ctx)
			close(pcDone)
		}()
		observe = r.observe
	} else {
		close(pcDone)
	}
	if *input != "" {
		b := newBridge(time.Second)
		b.releaseAfter, b.night, b.observe = *releaseAfter, car.night.get, observe
		if *gpsd != "" {
			h := &heading{minSpeed: *minSpeed, hold: *hold}
			pos := &position{}
			b.heading, b.position = h.compass, pos.arcsec
			go followGpsd(*gpsd, h, pos, lg.Printf)
		}
		go func() {
			runNav(ctx, *navURL, *input, *navEvery, navOptions{imperial: *units == "imperial", clock24: *clock24, loc: time.Local}, b, lg.Printf)
			close(navDone)
		}()
	} else {
		close(navDone)
	}
	p := newPoster(*url, *every, lg.Printf)
	if *capacity > 0 {
		p.set("batteryCapacityKwh", *capacity)
	}
	go p.run()
	go func() {
		for {
			if err := follow(*tap, *settle, p, car); err != nil {
				lg.Printf("tap %s: %v", *tap, err)
			}
			time.Sleep(2 * time.Second)
		}
	}()
	<-ctx.Done()
	// The pane is given back, and conditioning cancelled, on the way out.
	deadline := time.After(3 * time.Second)
	for _, done := range []chan struct{}{navDone, pcDone} {
		select {
		case <-done:
		case <-deadline:
		}
	}
}

// carState is what the tap last told of the car, for the other goroutines.
type carState struct {
	night latest[bool]
	soc   latest[float64] // %
	cond  latest[bool]    // battery conditioning running
}

// follow reads the tap until it ends, posting the climate when a burst of
// panel frames ends, or when a lone change has settled, and keeping car up
// to date.
func follow(path string, settle time.Duration, p *poster, car *carState) error {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()
	lines := make(chan string)
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			lines <- sc.Text()
		}
		done <- sc.Err()
		close(lines)
	}()
	var (
		state   micom.CarClimate
		inBurst bool
		timer   <-chan time.Time
	)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return <-done
			}
			f, ok := parseTapLine(line)
			if !ok {
				continue
			}
			fields := micom.Decode(f)
			for k, v := range liviCar(fields) {
				p.set(k, v)
				switch k {
				case "nightMode":
					car.night.set(v.(bool))
				case "fuelPct":
					car.soc.set(v.(float64))
				}
			}
			for _, fl := range fields {
				if fl.Name == "Battery conditioning" && fl.Valid {
					car.cond.set(fl.Value != 0)
					p.set("batteryConditioning", fl.Value != 0)
				}
			}
			switch state.Apply(f) {
			case micom.ClimateBegin:
				inBurst, timer = true, nil
			case micom.ClimateEnd:
				inBurst, timer = false, nil
				p.climate(liviClimate(state))
			case micom.ClimateChanged:
				if !inBurst {
					timer = time.After(settle)
				}
			}
		case <-timer:
			timer = nil
			p.climate(liviClimate(state))
		}
	}
}

// carFields are the decoded fields LIVI takes, by the variant's names, and
// the telemetry field each becomes: the variant gives them in LIVI's units.
var carFields = map[string]string{
	"Vehicle speed":            "speedKph",
	"State of charge":          "fuelPct",
	"Battery remaining energy": "batteryLevelKwh",
	"Range":                    "rangeKm",
	"Outdoor temperature":      "ambientC",
	"Odometer":                 "odometerKm",
	"Illumination":             "nightMode", // 0 is night, any other value day
}

// liviCar is what of a frame's decoded fields LIVI takes, as its telemetry
// fields. rangeClimateOffKm is a sum, the range plus what switching the
// climate off adds, read from one frame; rangeKm stays the cluster's, which
// is the same range but sent periodically rather than on change.
func liviCar(fs []micom.Field) map[string]any {
	var out map[string]any
	var on, added *int
	for _, f := range fs {
		if f.Valid {
			switch f.Name {
			case "Range, climate on":
				on = &f.Value
			case "Range added, climate off":
				added = &f.Value
			}
		}
		key, ok := carFields[f.Name]
		if !ok || !f.Valid {
			continue
		}
		var v any = float64(f.Value)
		switch {
		case key == "nightMode":
			v = f.Value == 0
		case f.Float != nil:
			v = *f.Float
		}
		if out == nil {
			out = map[string]any{}
		}
		out[key] = v
	}
	if on != nil && added != nil {
		if out == nil {
			out = map[string]any{}
		}
		out["rangeClimateOffKm"] = float64(*on + *added)
	}
	return out
}

// parseTapLine reads a received frame off one line of micomd's tap:
// "15:04:05.000 rx hdr=43c200c3 cmd=0d payload=03  # Climate airflow: feet".
// The text after # is for people and is not read.
func parseTapLine(line string) (micom.Frame, bool) {
	var f micom.Frame
	line, _, _ = strings.Cut(line, "#")
	fields := strings.Fields(line)
	if len(fields) != 5 || fields[1] != "rx" {
		return f, false
	}
	hdr, ok1 := field(fields[2], "hdr=")
	cmd, ok2 := field(fields[3], "cmd=")
	payload, ok3 := field(fields[4], "payload=")
	if !ok1 || !ok2 || !ok3 || len(hdr) != 4 || len(cmd) != 1 {
		return f, false
	}
	copy(f.Hdr[:], hdr)
	f.Cmd = cmd[0]
	f.Payload = payload
	return f, true
}

func field(s, prefix string) ([]byte, bool) {
	v, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return nil, false
	}
	b, err := hex.DecodeString(v)
	return b, err == nil
}

// liviClimate is the car's climate as LIVI's telemetry block, with only what
// the car has reported. LIVI's ranges are narrower than the car's in two
// places: set points are 16 to 28 C, its ends LO and HI (the car's own LO and
// HI go there), and the fan has 7 levels to the car's 8, so the car's top two
// both show as 7. The car's Celsius set point is exact; its Fahrenheit one is
// converted only when that is all there is.
// Which vent stands for which airflow is a reading of the names, not a capture.
func liviClimate(c micom.CarClimate) map[string]any {
	out := map[string]any{"source": "car"}
	setPoint := func(key string, halfC, f *int) {
		v, cel := halfC, 0.0
		switch {
		case halfC != nil:
			cel = float64(*halfC) / 2
		case f != nil:
			v, cel = f, float64(*f-32)*5/9
		}
		if v == nil || *v == 0 {
			return
		}
		switch *v {
		case micom.SetPointLo:
			cel = 16
		case micom.SetPointHi:
			cel = 28
		}
		out[key] = math.Min(28, math.Max(16, math.Round(cel*2)/2))
	}
	setPoint("driverTargetC", c.DriverSetHalfC, c.DriverSetF)
	setPoint("passengerTargetC", c.PassengerSetHalfC, c.PassengerSetF)
	flag := func(key string, b *bool) {
		if b != nil {
			out[key] = *b
		}
	}
	flag("auto", c.Auto)
	flag("ac", c.AC)
	flag("recirculation", c.Recirculation)
	flag("zonesSynced", c.ZonesSynced)
	flag("fanAuto", c.FanAuto)
	if c.FanLevel != nil {
		out["fanLevel"] = min(*c.FanLevel, 7)
		out["on"] = *c.FanLevel != 0 || (c.FanAuto != nil && *c.FanAuto)
	}
	if c.Airflow != nil {
		a := *c.Airflow
		out["ventsAuto"] = a == 8
		if a != 8 {
			out["ventMiddle"] = a == 1 || a == 2
			out["ventLower"] = a == 2 || a == 3 || a == 4
			out["ventUpper"] = a == 4 || a == 5
		}
	}
	if c.DriverSeatHeat != nil {
		out["driverSeatHeating"] = *c.DriverSeatHeat
	}
	if c.PassengerSeatHeat != nil {
		out["passengerSeatHeating"] = *c.PassengerSeatHeat
	}
	return out
}

// poster sends the car's telemetry to LIVI from its own goroutine (run): the
// newest value of each top-level field, only those LIVI has not already
// taken, at most once every every. While LIVI does not answer it retries, and
// once it does, sends every field again; every fullEvery too.
type poster struct {
	url    string
	every  time.Duration
	client *http.Client
	logf   func(string, ...any)
	mu     sync.Mutex
	state  map[string]any
	wake   chan struct{}
}

func newPoster(url string, every time.Duration, logf func(string, ...any)) *poster {
	return &poster{
		url:    url,
		every:  every,
		client: &http.Client{Timeout: 3 * time.Second},
		logf:   logf,
		state:  map[string]any{},
		wake:   make(chan struct{}, 1),
	}
}

// set makes v the newest value of the telemetry field key.
func (p *poster) set(key string, v any) {
	p.mu.Lock()
	p.state[key] = v
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// climate sets the climate block, once the car has reported any of it.
func (p *poster) climate(c map[string]any) {
	if len(c) > 1 {
		p.set("climate", c)
	}
}

// changes is the body of the next post, the fields whose JSON is not what
// LIVI last took (sent), and that JSON; nil when there is nothing new.
func (p *poster) changes(sent map[string]string) (map[string]any, map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var body map[string]any
	js := map[string]string{}
	for k, v := range p.state {
		b, _ := json.Marshal(v)
		if sent[k] == string(b) {
			continue
		}
		if body == nil {
			body = map[string]any{}
		}
		body[k], js[k] = v, string(b)
	}
	return body, js
}

// fullEvery is how often every field goes again: a LIVI restarted between
// two posts never fails one, and would otherwise lack what does not change,
// such as the illumination.
const fullEvery = 30 * time.Second

func (p *poster) run() {
	sent := map[string]string{} // each field's JSON as LIVI last took it
	full := time.Now()
	for {
		select {
		case <-p.wake:
		case <-time.After(5 * time.Second):
		}
		if time.Since(full) >= fullEvery {
			clear(sent)
			full = time.Now()
		}
		body, js := p.changes(sent)
		if body == nil {
			continue
		}
		b, _ := json.Marshal(body)
		if err := p.send(b); err != nil {
			p.logf("%v", err)
			clear(sent)
			continue
		}
		maps.Copy(sent, js)
		time.Sleep(p.every)
	}
}

func (p *poster) send(body []byte) error {
	resp, err := p.client.Post(p.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", p.url, resp.Status)
	}
	return nil
}
