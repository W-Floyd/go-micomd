package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"sync"
	"time"
)

// tpvStale is how long a gpsd report stands: gpsd sends one a second.
const tpvStale = 5 * time.Second

// heading is the car's heading for the cluster's compass, from gpsd's track.
// On the head unit the track is hippod's dead-reckoning bearing, gyro-held,
// so it is good when slow or stopped and minSpeed is 0. With a minSpeed (a
// plain GNSS track wanders when slow) the last track at that speed is held
// for hold, then the compass goes blank. Without a fix or a track, or
// without gpsd, it is blank at once.
type heading struct {
	minSpeed float64 // m/s
	hold     time.Duration

	mu      sync.Mutex
	deg     float64
	goodAt  time.Time // when deg was last taken, moving
	tpvAt   time.Time
	tracked bool // the last report had a fix and a track
}

// tpv takes one gpsd TPV report: its mode, and its track (degrees true) and
// speed (m/s) when it has them.
func (h *heading) tpv(mode int, track, speed *float64, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tpvAt = now
	h.tracked = mode >= 2 && track != nil
	switch {
	case mode < 2:
		h.goodAt = time.Time{}
	case h.tracked && (h.minSpeed <= 0 || speed != nil && *speed >= h.minSpeed):
		h.deg, h.goodAt = *track, now
	}
}

// compass is the heading in the cluster's steps of 7.5°, nil for none.
func (h *heading) compass(now time.Time) *float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.tracked || now.Sub(h.tpvAt) > tpvStale || h.goodAt.IsZero() || now.Sub(h.goodAt) > h.hold {
		return nil
	}
	d := compassStep(h.deg)
	return &d
}

// position is the unit's GPS fix for the cluster (micomd's cluster-position),
// from gpsd's reports: none without a fix, or once gpsd goes quiet, as stock
// sends nothing without one.
type position struct {
	mu       sync.Mutex
	lat, lon float64
	at       time.Time // of the last report with a fix; zero after one without
}

// tpv takes one gpsd TPV report: its mode, and its latitude and longitude
// when it has them.
func (p *position) tpv(mode int, lat, lon *float64, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if mode < 2 || lat == nil || lon == nil {
		p.at = time.Time{}
		return
	}
	p.lat, p.lon, p.at = *lat, *lon, now
}

// arcsec is the fix as stock's arc-seconds, ok false for none.
func (p *position) arcsec(now time.Time) (lat, lon int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.at.IsZero() || now.Sub(p.at) > tpvStale {
		return 0, 0, false
	}
	return stockArcsec(p.lat), stockArcsec(p.lon), true
}

// stockArcsec is degrees in whole arc-seconds as stock packs them: the
// degrees, minutes and seconds of the magnitude, each truncated, with only
// the degrees taking the sign (-1.5 is -1° + 30', -1800).
func stockArcsec(deg float64) int {
	a := math.Abs(deg)
	d := math.Floor(a)
	m := math.Floor((a - d) * 60)
	sec := math.Trunc(((a-d)*60 - m) * 60)
	if deg < 0 {
		d = -d
	}
	return int(d)*3600 + int(m)*60 + int(sec)
}

// compassStep rounds degrees to the cluster's 48 steps (stock round(deg /
// 7.5)), 360 being 0.
func compassStep(deg float64) float64 {
	return float64((int(math.Round(math.Mod(deg, 360)/7.5))%48+48)%48) * 7.5
}

// tpvReport is the part of a gpsd TPV report micom-livi reads.
type tpvReport struct {
	Class string   `json:"class"`
	Mode  int      `json:"mode"`
	Track *float64 `json:"track"`
	Speed *float64 `json:"speed"`
	Lat   *float64 `json:"lat"`
	Lon   *float64 `json:"lon"`
}

// followGpsd watches gpsd at addr into h and p, reconnecting (1 s, doubling to
// 30 s); logf hears each new problem once.
func followGpsd(addr string, h *heading, p *position, logf func(string, ...any)) {
	wait, said := time.Second, ""
	for {
		connected, err := watchGpsd(addr, func(r tpvReport, now time.Time) {
			h.tpv(r.Mode, r.Track, r.Speed, now)
			p.tpv(r.Mode, r.Lat, r.Lon, now)
		})
		if s := fmt.Sprint(err); s != said {
			logf("gpsd %s: %v", addr, err)
			said = s
		}
		if connected {
			wait = time.Second
		}
		time.Sleep(wait)
		wait = min(2*wait, 30*time.Second)
	}
}

// watchGpsd reads gpsd's reports until the connection fails; connected says
// whether it got as far as one.
func watchGpsd(addr string, take func(tpvReport, time.Time)) (connected bool, err error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return false, err
	}
	defer c.Close()
	if _, err := fmt.Fprint(c, `?WATCH={"enable":true,"json":true};`); err != nil {
		return false, err
	}
	sc := bufio.NewScanner(c)
	for {
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return true, err
			}
			return true, fmt.Errorf("closed")
		}
		var r tpvReport
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Class == "TPV" {
			take(r, time.Now())
		}
	}
}
