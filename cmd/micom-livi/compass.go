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

// compassStep rounds degrees to the cluster's 48 steps (stock round(deg /
// 7.5)), 360 being 0.
func compassStep(deg float64) float64 {
	return float64((int(math.Round(math.Mod(deg, 360)/7.5))%48+48)%48) * 7.5
}

// followGpsd watches gpsd at addr into h, reconnecting (1 s, doubling to 30 s);
// logf hears each new problem once.
func followGpsd(addr string, h *heading, logf func(string, ...any)) {
	wait, said := time.Second, ""
	for {
		connected, err := watchGpsd(addr, h)
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
func watchGpsd(addr string, h *heading) (connected bool, err error) {
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
		var r struct {
			Class string   `json:"class"`
			Mode  int      `json:"mode"`
			Track *float64 `json:"track"`
			Speed *float64 `json:"speed"`
		}
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Class == "TPV" {
			h.tpv(r.Mode, r.Track, r.Speed, time.Now())
		}
	}
}
