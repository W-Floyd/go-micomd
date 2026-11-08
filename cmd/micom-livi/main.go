// Command micom-livi feeds the car's state, as micomd decodes it, into LIVI's
// telemetry: it reads micomd's tap socket and posts the climate (panel and seat
// heat) to LIVI's telemetry endpoint as the car's ("source": "car").
//
//	micom-livi [-tap /run/micomd.sock] [-url http://127.0.0.1:4000/telemetry]
//
// It only reads: micomd owns the link, and nothing goes back to the car. It is
// a client of both sides, so either can restart: it reconnects to the tap, and
// posts the whole state again once LIVI answers after a failure (LIVI restores
// its own saved climate at start, which the car's must replace).
package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	micom "github.com/W-Floyd/go-micomd"
)

func main() {
	tap := flag.String("tap", "/run/micomd.sock", "micomd's -tap socket")
	url := flag.String("url", "http://127.0.0.1:4000/telemetry", "LIVI's telemetry endpoint")
	settle := flag.Duration("settle", 300*time.Millisecond, "wait this long after a change outside a burst before posting")
	flag.Parse()

	lg := log.New(os.Stderr, "", log.LstdFlags)
	p := newPoster(*url, lg.Printf)
	go p.run()
	for {
		if err := follow(*tap, *settle, p); err != nil {
			lg.Printf("tap %s: %v", *tap, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// follow reads the tap until it ends, posting the climate when a burst of
// panel frames ends, or when a lone change has settled.
func follow(path string, settle time.Duration, p *poster) error {
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
			switch state.Apply(f) {
			case micom.ClimateBegin:
				inBurst, timer = true, nil
			case micom.ClimateEnd:
				inBurst, timer = false, nil
				p.post(liviClimate(state))
			case micom.ClimateChanged:
				if !inBurst {
					timer = time.After(settle)
				}
			}
		case <-timer:
			timer = nil
			p.post(liviClimate(state))
		}
	}
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
// places: set points are 16 to 28 C (the car's are whole Fahrenheit degrees),
// and the fan has 7 levels to the car's 8, so the car's top two both show as 7.
// Which vent stands for which airflow is a reading of the names, not a capture.
func liviClimate(c micom.CarClimate) map[string]any {
	out := map[string]any{"source": "car"}
	setPoint := func(key string, f *int) {
		if f != nil && *f != 0 {
			cel := float64(*f-32) * 5 / 9
			out[key] = math.Min(28, math.Max(16, math.Round(cel*2)/2))
		}
	}
	setPoint("driverTargetC", c.DriverSetF)
	setPoint("passengerTargetC", c.PassengerSetF)
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

// poster sends the climate to LIVI from its own goroutine (run), skipping a
// state LIVI already took, and retries the newest one while LIVI does not
// answer.
type poster struct {
	url     string
	client  *http.Client
	logf    func(string, ...any)
	pending chan map[string]any
}

func newPoster(url string, logf func(string, ...any)) *poster {
	return &poster{
		url:     url,
		client:  &http.Client{Timeout: 3 * time.Second},
		logf:    logf,
		pending: make(chan map[string]any, 1),
	}
}

// post hands over the newest state; one still waiting is replaced, as only
// the newest matters.
func (p *poster) post(climate map[string]any) {
	if len(climate) <= 1 {
		return
	}
	select {
	case <-p.pending:
	default:
	}
	p.pending <- climate
}

func (p *poster) run() {
	var (
		body []byte // the newest state
		sent []byte // what LIVI last took
	)
	for {
		select {
		case c := <-p.pending:
			body, _ = json.Marshal(map[string]any{"climate": c})
		case <-time.After(5 * time.Second):
		}
		if body == nil || bytes.Equal(body, sent) {
			continue
		}
		if err := p.send(body); err != nil {
			p.logf("%v", err)
			sent = nil
			continue
		}
		sent = body
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
