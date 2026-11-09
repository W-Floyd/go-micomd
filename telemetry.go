package micom

import (
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
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
	// Variant is the variant running and why it was chosen.
	Variant VariantChoice `json:"variant"`
	// Recorder is -record's progress, nil when not recording.
	Recorder *RecorderState `json:"recorder,omitempty"`
}

// VariantChoice is which variant a daemon runs, and why.
type VariantChoice struct {
	// Name is the variant's, "none" when it failed to load and only the
	// keepalive runs.
	Name string `json:"name"`
	// Source is how it was chosen: "file" (-variant-file), "flag"
	// (-variant), "cmdline" (the kernel command line matched it) or
	// "default" (nothing matched); "" when the caller gave Config.Variant.
	Source string `json:"source"`
	File   string `json:"file,omitempty"`
	// Cmdline is the identity keys the kernel command line carried, and
	// Matched the variant they match, "" for none.
	Cmdline Identity `json:"cmdline"`
	Matched string   `json:"matched,omitempty"`
	// Note is what went wrong: an unreadable command line, a tied match, or
	// the load error that left the keepalive only.
	Note string `json:"note,omitempty"`
	// Reply is the micom's own variant record (the variant's identify
	// reply, 0x8117 on HKMC), nil until one arrives: it is not asked for
	// without -identify-request.
	Reply *VariantReply `json:"reply,omitempty"`
}

// VariantReply is the latest variant record the micom sent.
type VariantReply struct {
	Payload string  `json:"payload"`
	AgeMs   int64   `json:"age_ms"`
	Fields  []Field `json:"fields"`
	// Identity is the record read as the command line would be, Matched the
	// variant it matches ("" for none), and Verdict what that says of the
	// variant running.
	Identity Identity `json:"identity"`
	Matched  string   `json:"matched,omitempty"`
	Verdict  string   `json:"verdict"`
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
	variant   *Variant // names and decodes the frames
	reply     *Frame   // the micom's variant record, the latest
	replyAt   time.Time
}

func newTelemetry(v *Variant) *telemetry {
	return &telemetry{fields: map[string]*seenField{}, frames: map[string]*seenFrame{}, variant: v}
}

func (t *telemetry) add(f Frame, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := fmt.Sprintf("%02x%02x%02x%02x %02x", f.Hdr[0], f.Hdr[1], f.Hdr[2], f.Hdr[3], f.Cmd)
	sf := t.frames[key]
	if sf == nil {
		sf = &seenFrame{name: t.variant.FrameName(f), first: len(t.frames)}
		t.frames[key] = sf
	}
	sf.n++
	sf.at = now
	sf.note(f.Payload, now)
	if IsAliveRequest(f) {
		t.lastAlive = now
	}
	if r := t.variant.reply; r != nil && f.Hdr == r.hdr && f.Cmd == r.cmd {
		t.reply = &Frame{Hdr: f.Hdr, Cmd: f.Cmd, Payload: append([]byte(nil), f.Payload...)}
		t.replyAt = now
	}
	for _, fl := range t.variant.Decode(f) {
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
	choice := d.cfg.Choice
	if choice.Name == "" {
		choice.Name = d.cfg.Variant.Name
	}
	d.telem.mu.Lock()
	if !d.telem.markedAt.IsZero() {
		marked = now.Sub(d.telem.markedAt).Milliseconds()
	}
	var reply *Frame
	if d.telem.reply != nil {
		choice.Reply = &VariantReply{Payload: hex.EncodeToString(d.telem.reply.Payload), AgeMs: now.Sub(d.telem.replyAt).Milliseconds()}
		reply = d.telem.reply
	}
	d.telem.mu.Unlock()
	if r := choice.Reply; r != nil {
		r.Fields = d.cfg.Variant.Decode(*reply)
		r.Identity, _ = d.cfg.Variant.ReplyIdentity(*reply)
		r.Matched, r.Verdict = recordVerdict(choice.Name, r.Identity)
	}
	var rec *RecorderState
	if r := d.cfg.Recorder; r != nil {
		st := r.State()
		rec = &st
	}
	return Snapshot{
		Recorder:       rec,
		Variant:        choice,
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

// WriteTable writes s for a person: the variant and why, the link, then one
// line per frame kind, its bytes changed since the mark as
// idx:from->to×changes (from ?? when there was no mark).
func (s Snapshot) WriteTable(w io.Writer) error {
	v := s.Variant
	why := ""
	switch v.Source {
	case "file":
		why = "from " + v.File
	case "flag":
		why = "-variant"
	case "cmdline":
		why = "kernel command line " + identityString(v.Cmdline)
	case "default":
		why = "default: no variant matches " + identityString(v.Cmdline)
	default:
		why = "given by the caller"
	}
	if v.Source != "cmdline" && v.Matched != "" {
		why += "; the kernel command line matches " + v.Matched
	}
	if v.Note != "" {
		why += "; " + v.Note
	}
	fmt.Fprintf(w, "variant  %s (%s)\n", v.Name, why)
	if r := v.Reply; r != nil {
		var fs []string
		for _, f := range r.Fields {
			fs = append(fs, f.String())
		}
		fmt.Fprintf(w, "micom    %s\n", r.Verdict)
		fmt.Fprintf(w, "record   %s ago: %s  %s\n", age(r.AgeMs), r.Payload, strings.Join(fs, " "))
	}
	up := "down"
	if s.Up {
		up = "up"
	}
	lastAlive := "never"
	if s.LastAliveAgeMs >= 0 {
		lastAlive = age(s.LastAliveAgeMs) + " ago"
	}
	fmt.Fprintf(w, "link     %s  rx %d  tx %d  alive %d/%d, last %s  badsum %d  dropped %d\n",
		up, s.RxFrames, s.TxFrames, s.AliveRx, s.AliveTx, lastAlive, s.BadSum, s.Dropped)
	mark := "none: changes since the start"
	if s.MarkedAgeMs >= 0 {
		mark = age(s.MarkedAgeMs) + " ago"
	}
	fmt.Fprintf(w, "mark     %s\n", mark)
	if r := s.Recorder; r != nil {
		fmt.Fprintf(w, "recording %s  %d bytes  rotations %d  dropped %d", r.Path, r.Bytes, r.Rotations, r.Dropped)
		if r.Error != "" {
			fmt.Fprintf(w, "  stopped: %s", r.Error)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tNAME\tCOUNT\tAGE\tPAYLOAD\tCHANGED")
	for _, f := range s.Frames {
		var ds []string
		for _, d := range f.Diffs {
			from := "??"
			if d.From >= 0 {
				from = fmt.Sprintf("%02x", d.From)
			}
			ds = append(ds, fmt.Sprintf("%d:%s->%02x×%d", d.Idx, from, d.To, d.Changes))
		}
		name := f.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", f.Key, name, f.Count, age(f.AgeMs), f.Payload, strings.Join(ds, " "))
	}
	return tw.Flush()
}

// age is milliseconds as seconds.
func age(ms int64) string { return fmt.Sprintf("%.1fs", float64(ms)/1000) }

func identityString(id Identity) string {
	if len(id) == 0 {
		return "(no identity keys)"
	}
	var ks []string
	for _, k := range slices.Sorted(maps.Keys(id)) {
		ks = append(ks, fmt.Sprintf("%s=%#x", k, id[k]))
	}
	return strings.Join(ks, " ")
}
