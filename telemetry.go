package micom

import (
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Reading is a Field and how long ago it was last received.
type Reading struct {
	Field
	// AgeMs is milliseconds since the frame that carried it arrived.
	AgeMs int64 `json:"age_ms"`
}

// FrameCount is how many frames of one kind arrived, and when the last did.
// Frames without a known layout are listed too, by header and command.
type FrameCount struct {
	// Key is "hdr cmd" in hex, e.g. "0c88018c 55".
	Key string `json:"key"`
	// Name is what the frame is, "" when not known.
	Name  string `json:"name"`
	Count uint64 `json:"count"`
	AgeMs int64  `json:"age_ms"`
	// Payload is the latest payload, in hex.
	Payload string `json:"payload"`
	// Diffs are the payload bytes that changed since the last Mark (or since
	// the start, before one), for finding what a byte means: change
	// something on the car and see which bytes move.
	Diffs []ByteDiff `json:"diffs"`
}

// ByteDiff is a payload byte that changed.
type ByteDiff struct {
	// Idx is the byte's offset in the payload.
	Idx int `json:"idx"`
	// From is its value at the mark, -1 when there was none (no mark, or the
	// payload was shorter then); To its latest.
	From int `json:"from"`
	To   int `json:"to"`
	// Changes is how many times it changed since the mark: a counter or a
	// timer changes constantly, a door or a switch once.
	Changes uint32 `json:"changes"`
	AgeMs   int64  `json:"age_ms"`
}

// Snapshot is everything the daemon knows, for a screen or an RPC.
type Snapshot struct {
	// Up is true while frames are arriving (the last within 5 s).
	Up bool `json:"up"`
	// RxFrames, TxFrames, BadSum and Dropped are the link's counters.
	RxFrames uint64 `json:"rx_frames"`
	TxFrames uint64 `json:"tx_frames"`
	BadSum   int    `json:"bad_sum"`
	Dropped  int    `json:"dropped"`
	// AliveRx and AliveTx are the micom's CPU-alive requests and our answers.
	AliveRx uint64 `json:"alive_rx"`
	AliveTx uint64 `json:"alive_tx"`
	// LastAliveAgeMs is milliseconds since the last request, -1 before one.
	LastAliveAgeMs int64 `json:"last_alive_age_ms"`
	// Readings are the decoded values, in the order they were first seen.
	Readings []Reading `json:"readings"`
	// Frames are per-kind counts, the most frequent first.
	Frames []FrameCount `json:"frames"`
	// MarkedAgeMs is milliseconds since the last Mark, -1 before one.
	MarkedAgeMs int64 `json:"marked_age_ms"`
}

type seenField struct {
	f  Field
	at time.Time
}

type seenFrame struct {
	name  string
	n     uint64
	at    time.Time
	first int
	// last and base are the latest payload and the one at the mark; changes
	// and changedAt count and date each byte's changes since the mark.
	last, base []byte
	changes    []uint32
	changedAt  []time.Time
}

// note records a payload: which bytes differ from the last.
func (sf *seenFrame) note(p []byte, now time.Time) {
	for len(sf.changes) < len(p) {
		sf.changes = append(sf.changes, 0)
		sf.changedAt = append(sf.changedAt, time.Time{})
	}
	if sf.last != nil {
		for i := range p {
			if i >= len(sf.last) || p[i] != sf.last[i] {
				sf.changes[i]++
				sf.changedAt[i] = now
			}
		}
	}
	sf.last = append(sf.last[:0], p...)
}

// telemetry keeps the latest value of each field and a count per frame kind.
type telemetry struct {
	mu        sync.Mutex
	fields    map[string]*seenField
	order     []string
	frames    map[string]*seenFrame
	lastAlive time.Time
	markedAt  time.Time
}

func newTelemetry() *telemetry {
	return &telemetry{fields: map[string]*seenField{}, frames: map[string]*seenFrame{}}
}

func (t *telemetry) add(f Frame, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := fmt.Sprintf("%02x%02x%02x%02x %02x", f.Hdr[0], f.Hdr[1], f.Hdr[2], f.Hdr[3], f.Cmd)
	sf := t.frames[key]
	if sf == nil {
		sf = &seenFrame{name: FrameName(f), first: len(t.frames)}
		t.frames[key] = sf
	}
	sf.n++
	sf.at = now
	sf.note(f.Payload, now)
	if IsAliveRequest(f) {
		t.lastAlive = now
	}
	for _, fl := range Decode(f) {
		sv := t.fields[fl.Name]
		if sv == nil {
			sv = &seenField{}
			t.fields[fl.Name] = sv
			t.order = append(t.order, fl.Name)
		}
		sv.f, sv.at = fl, now
	}
}

func (t *telemetry) snapshot(now time.Time) (readings []Reading, frames []FrameCount, aliveAge int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, name := range t.order {
		sv := t.fields[name]
		readings = append(readings, Reading{Field: sv.f, AgeMs: now.Sub(sv.at).Milliseconds()})
	}
	for k, sf := range t.frames {
		fc := FrameCount{Key: k, Name: sf.name, Count: sf.n, AgeMs: now.Sub(sf.at).Milliseconds(), Payload: hex.EncodeToString(sf.last), Diffs: []ByteDiff{}}
		for i, n := range sf.changes {
			if n == 0 || i >= len(sf.last) {
				continue
			}
			d := ByteDiff{Idx: i, From: -1, To: int(sf.last[i]), Changes: n, AgeMs: now.Sub(sf.changedAt[i]).Milliseconds()}
			if i < len(sf.base) {
				d.From = int(sf.base[i])
			}
			fc.Diffs = append(fc.Diffs, d)
		}
		frames = append(frames, fc)
	}
	sort.Slice(frames, func(i, j int) bool {
		if frames[i].Count != frames[j].Count {
			return frames[i].Count > frames[j].Count
		}
		return frames[i].Key < frames[j].Key
	})
	aliveAge = -1
	if !t.lastAlive.IsZero() {
		aliveAge = now.Sub(t.lastAlive).Milliseconds()
	}
	return
}

// mark takes the latest payloads as the baseline and starts counting changes
// from zero.
func (t *telemetry) mark(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, sf := range t.frames {
		sf.base = append([]byte(nil), sf.last...)
		for i := range sf.changes {
			sf.changes[i] = 0
		}
	}
	t.markedAt = now
}

// Mark sets the baseline for Snapshot's byte diffs: call it, change
// something on the car, and the bytes that moved are the ones that mean it.
func (d *Daemon) Mark() { d.telem.mark(d.cfg.Now()) }

// Snapshot is the link's state and everything decoded from it, now.
func (d *Daemon) Snapshot() Snapshot {
	now := d.cfg.Now()
	st := d.Stats()
	readings, frames, aliveAge := d.telem.snapshot(now)
	marked := int64(-1)
	d.telem.mu.Lock()
	if !d.telem.markedAt.IsZero() {
		marked = now.Sub(d.telem.markedAt).Milliseconds()
	}
	d.telem.mu.Unlock()
	return Snapshot{
		MarkedAgeMs:    marked,
		Up:             !st.LastRx.IsZero() && now.Sub(st.LastRx) < 5*time.Second,
		RxFrames:       st.RxFrames,
		TxFrames:       st.TxFrames,
		BadSum:         st.BadSum,
		Dropped:        st.Dropped,
		AliveRx:        st.AliveRx,
		AliveTx:        st.AliveTx,
		LastAliveAgeMs: aliveAge,
		Readings:       readings,
		Frames:         frames,
	}
}
