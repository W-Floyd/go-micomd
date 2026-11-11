package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	micom "github.com/W-Floyd/go-micomd"
)

// timed is a frame from the micom at an offset into a capture.
type timed struct {
	at time.Duration
	f  micom.Frame
}

// loadCapture reads a micomd -record file and keeps what the micom sent
// ("dir":"rx"), each at its offset from the capture's first frame. Frames
// with a bad checksum are dropped, as micomd dropped them.
func loadCapture(path string) ([]timed, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var out []timed
	var t0 float64
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		var r struct {
			T     float64 `json:"t"`
			Dir   string  `json:"dir"`
			Frame string  `json:"frame"`
			Bad   bool    `json:"bad"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %v", path, n, err)
		}
		if r.Frame == "" {
			continue
		}
		if t0 == 0 {
			t0 = r.T
		}
		if r.Dir != "rx" || r.Bad {
			continue
		}
		raw, err := hex.DecodeString(r.Frame)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %v", path, n, err)
		}
		var d micom.Decoder
		for _, f := range d.Write(raw) {
			out = append(out, timed{time.Duration((r.T - t0) * float64(time.Second)), f})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out, nil
}

// window keeps the frames between from and to (to 0: the end), shifted to
// start at 0.
func window(fs []timed, from, to time.Duration) []timed {
	var out []timed
	for _, t := range fs {
		if t.at < from || (to > 0 && t.at > to) {
			continue
		}
		t.at -= from
		out = append(out, t)
	}
	return out
}

// snapshot is the car's state at offset at: the latest frame of each kind
// before it, and how often each kind came in the capture, so it can be
// repeated as a still scene.
func snapshot(v *micom.Variant, fs []timed, at time.Duration) []periodic {
	type acc struct {
		f           micom.Frame
		n           int
		first, last time.Duration
	}
	seen := map[string]*acc{}
	var order []string
	for _, t := range fs {
		if t.at > at {
			break
		}
		k := kindOf(v, t.f)
		a := seen[k]
		if a == nil {
			a = &acc{first: t.at}
			seen[k] = a
			order = append(order, k)
		}
		a.f, a.n, a.last = t.f, a.n+1, t.at
	}
	var out []periodic
	for _, k := range order {
		a := seen[k]
		if micom.IsAliveRequest(a.f) || micom.IsBootRecord(a.f) {
			continue
		}
		every := 10 * time.Second
		if a.n > 1 {
			every = (a.last - a.first) / time.Duration(a.n-1)
		}
		every = min(max(every, 100*time.Millisecond), 10*time.Second)
		out = append(out, periodic{a.f, every})
	}
	return out
}

// kindOf groups frames that carry the same thing: header, command and the
// variant's message for them (items of one command are messages apart).
func kindOf(v *micom.Variant, f micom.Frame) string {
	name, _, _ := v.RawFields(f)
	return fmt.Sprintf("%x %02x %s", f.Hdr, f.Cmd, name)
}
