package micom

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// inputEngine turns structured inputs into frames by the variant's to_micom
// messages (docs/messages.md): it validates, merges, encodes and sends, and
// runs each message's lifecycle (repeat, own, timeout, end).
type inputEngine struct {
	v    *Variant
	send func(Frame) error
	// own silences (true) or resumes (false) profile entries by name.
	own  func(names []string, on bool)
	now  func() time.Time
	logf func(string, ...any)
	// refused says why a message may not be sent, by name.
	refused map[string]string

	mu    sync.Mutex
	state map[string]*msgState // active messages

	// seen is the last value received of each from_micom field a
	// requirement or a computed field names; the reader updates it, apart
	// from mu, so a send waiting on the wire never holds frames back.
	seenMu sync.Mutex
	seen   map[string]seenValue
}

type seenValue struct {
	valid bool // false: the field carried its invalid marker
	code  int64
	val   any // as decoded: a number, enum name, bool or text
}

type msgState struct {
	vals      map[string]inValue // as given, defaults left out
	last      Frame
	sent      bool
	lastInput time.Time
	nextSend  time.Time
	sentFrame Frame
	lastSent  time.Time
	pending   bool // last waits for min_interval
}

// tickEvery bounds how late a repeat, a timeout or a coalesced send runs.
const tickEvery = 50 * time.Millisecond

// InputGates are what the input may send beyond captured messages.
type InputGates struct {
	// Inferred admits messages known only from code or inference
	// (-allow-inferred).
	Inferred bool
	// Persistent admits messages whose every frame the micom writes to its
	// EEPROM (-allow-persistent).
	Persistent bool
	// Dangerous admits messages that reset, rewrite or reconfigure the micom
	// (-allow-dangerous).
	Dangerous bool
}

func newInputEngine(v *Variant, g InputGates, send func(Frame) error, own func([]string, bool), now func() time.Time, logf func(string, ...any)) *inputEngine {
	e := &inputEngine{v: v, send: send, own: own, now: now, logf: logf, refused: map[string]string{}, state: map[string]*msgState{}, seen: map[string]seenValue{}}
	for n, m := range v.messages {
		var why []string
		if m.status != "captured" && !g.Inferred {
			why = append(why, "status "+m.status+": needs -allow-inferred")
		}
		if m.persistent && !g.Persistent {
			why = append(why, "persistent (writes the micom's EEPROM): needs -allow-persistent")
		}
		if m.dangerous != "" && !g.Dangerous {
			why = append(why, "dangerous ("+m.dangerous+"): needs -allow-dangerous")
		}
		if why != nil {
			e.refused[n] = strings.Join(why, "; ")
		}
	}
	// A message whose end would send a refused one is refused too.
	for changed := true; changed; {
		changed = false
		for _, n := range sortedKeys(v.messages) {
			if e.refused[n] != "" {
				continue
			}
			for _, s := range v.messages[n].end {
				if why := e.refused[s.msg]; why != "" {
					e.refused[n] = "its end sends " + s.msg + " (" + why + ")"
					changed = true
					break
				}
			}
		}
	}
	return e
}

// enabled lists the messages the input may send, and the refused ones with why.
func (e *inputEngine) enabled() (ok, refused []string) {
	for _, n := range sortedKeys(e.v.messages) {
		if why := e.refused[n]; why != "" {
			refused = append(refused, n+" ("+why+")")
		} else {
			ok = append(ok, n)
		}
	}
	return ok, refused
}

// handle takes one input line: a JSON object naming its message in "msg".
func (e *inputEngine) handle(line []byte) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil || dec.More() {
		return errors.New("line is not a JSON object")
	}
	name, ok := obj["msg"].(string)
	if !ok {
		return errors.New(`"msg" is required: a message name`)
	}
	delete(obj, "msg")
	m := e.v.messages[name]
	switch {
	case m == nil && e.v.receives(name):
		return fmt.Errorf("%s: from_micom, not an input", name)
	case m == nil:
		return noMessage(name)
	case e.refused[name] != "":
		return fmt.Errorf("%s: %s", name, e.refused[name])
	}
	if end, ok := obj["end"]; ok {
		if end != true || len(obj) != 1 {
			return fmt.Errorf("%s: end is true and takes no fields", name)
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if err := e.end(m); err != nil {
			return fmt.Errorf("%s: end: %v", name, err)
		}
		return nil
	}
	vals, err := m.parse(obj)
	if err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.state[name]
	merged := map[string]inValue{}
	if st != nil {
		maps.Copy(merged, st.vals)
	}
	maps.Copy(merged, vals)
	f, err := m.encode(merged, e.resolve)
	if err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	// Refused outright, never held for later: the state may never come.
	if why := e.unmet(m, merged); why != "" {
		return fmt.Errorf("%s: %s", name, why)
	}
	now := e.now()
	if st == nil {
		st = &msgState{}
		e.state[name] = st
		// Before the first frame, so the profile never sends beside it.
		if o := e.v.owned(m); o != nil {
			e.own(o, true)
		}
	}
	st.vals, st.lastInput, st.last = merged, now, f
	switch {
	case m.onChange && st.sent && sameFrame(f, st.sentFrame):
		st.pending = false
		return nil
	case m.minInterval > 0 && st.sent && now.Before(st.lastSent.Add(m.minInterval)):
		st.pending = true
		return nil
	}
	if err := e.sendNow(m, st, now); err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	return nil
}

// sendNow sends st.last. e.mu is held.
func (e *inputEngine) sendNow(m *msgSpec, st *msgState, now time.Time) error {
	// Again here: a held or repeated frame goes out after the state moved.
	if why := e.unmet(m, st.vals); why != "" {
		st.pending = false
		return fmt.Errorf("not sent: %s", why)
	}
	if m.computed {
		// Computed values are taken now, and checked like an input's.
		f, err := m.encode(st.vals, e.resolve)
		if err != nil {
			st.pending = false
			return fmt.Errorf("not sent: %v", err)
		}
		st.last = f
	}
	st.sentFrame, st.sent, st.pending = st.last, true, false
	st.lastSent, st.nextSend = now, now.Add(m.every)
	return e.send(st.last)
}

// unmet says which requirement of m stops a frame with vals now, "" when
// none does.
func (e *inputEngine) unmet(m *msgSpec, vals map[string]inValue) string {
	for _, r := range m.requires {
		if !r.applies(m, vals) {
			continue
		}
		e.seenMu.Lock()
		s, ok := e.seen[r.field]
		e.seenMu.Unlock()
		want := make([]string, len(r.in))
		for i, c := range r.in {
			want[i] = r.name(c)
		}
		req := fmt.Sprintf("requires %s in [%s]", r.field, strings.Join(want, ", "))
		switch {
		case !ok:
			return req + ": none received yet"
		case !s.valid:
			return req + ": it is n/a"
		case !slices.Contains(r.in, s.code):
			return req + ": it is " + r.name(s.code)
		}
	}
	return ""
}

// observe keeps the last value of each required field f carries; the
// daemon calls it for every frame received.
func (e *inputEngine) observe(f Frame) {
	if len(e.v.required) == 0 {
		return
	}
	for _, d := range e.v.decoders[f.Cmd] {
		if !d.matches(f.Hdr) || !d.match(f.Payload) {
			continue
		}
		for _, fs := range d.fields {
			if !e.v.required[fs.name] {
				continue
			}
			raw := fs.load(f.Payload)
			s := seenValue{valid: !fs.hasInvalid || raw != fs.invalid, code: fs.signedRaw(raw), val: fs.value(f.Payload)}
			e.seenMu.Lock()
			e.seen[fs.name] = s
			e.seenMu.Unlock()
		}
		return // the decoder Decode would use
	}
}

// resolve gives a computed field's value now: the clock, or the last valid
// value received of its from_micom field.
func (e *inputEngine) resolve(f *field) (inValue, bool) {
	if f.src.clock != "" {
		return clockOnly(e.now())(f)
	}
	e.seenMu.Lock()
	s, ok := e.seen[f.src.field]
	e.seenMu.Unlock()
	if !ok || !s.valid || s.val == nil {
		return inValue{}, false
	}
	v, err := toInValue(s.val)
	return v, err == nil
}

// end runs m's end steps, then makes it inactive and resumes what it owned.
// e.mu is held.
func (e *inputEngine) end(m *msgSpec) error {
	var errs []error
	for _, s := range m.end {
		t := e.v.messages[s.msg]
		ts := e.state[t.name]
		vals := map[string]inValue{}
		if ts != nil {
			maps.Copy(vals, ts.vals)
		}
		maps.Copy(vals, s.set)
		f, err := t.encode(vals, e.resolve)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", t.name, err))
			continue
		}
		if why := e.unmet(t, vals); why != "" {
			errs = append(errs, fmt.Errorf("%s: not sent: %s", t.name, why))
			continue
		}
		if ts != nil && t != m {
			// Another active message keeps the new values, and repeats them.
			ts.vals, ts.last, ts.sentFrame, ts.pending = vals, f, f, false
		}
		if err := e.send(f); err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", t.name, err))
		}
	}
	if e.state[m.name] != nil {
		delete(e.state, m.name)
		if o := e.v.owned(m); o != nil {
			e.own(o, false)
		}
	}
	return errors.Join(errs...)
}

// tick repeats what is due and ends what has gone stale.
func (e *inputEngine) tick(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range sortedKeys(e.state) {
		st := e.state[n]
		if st == nil {
			continue // ended by an earlier message's end
		}
		m := e.v.messages[n]
		if m.timeout > 0 && now.Sub(st.lastInput) >= m.timeout {
			if err := e.end(m); err != nil {
				e.logf("input: %s: end after %v without input: %v", n, m.timeout, err)
			}
			continue
		}
		due := st.pending && !now.Before(st.lastSent.Add(m.minInterval)) ||
			m.every > 0 && !now.Before(st.nextSend)
		if due {
			if err := e.sendNow(m, st, now); err != nil {
				e.logf("input: %s: %v", n, err)
			}
		}
	}
}

func (e *inputEngine) run(ctx context.Context) {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.tick(e.now())
		}
	}
}

// receives is whether name is one of the variant's from_micom messages.
func (v *Variant) receives(name string) bool {
	for _, ds := range v.decoders {
		if slices.ContainsFunc(ds, func(d *decoder) bool { return d.name == name }) {
			return true
		}
	}
	return false
}

// noMessage refuses a name the variant has neither way.
type noMessage string

func (n noMessage) Error() string { return fmt.Sprintf("no message %q", string(n)) }

// ServeInput serves the structured input on a unix socket (mode 0660): one
// JSON object per line, {"msg": NAME, field: value, ...}, each answered
// with {"ok":true} or {"error":"why"}. Only the daemon's variant's to_micom
// messages are sent, those not captured, persistent or dangerous only as g
// allows; the CPU-alive frame is refused whatever the variant says.
// It returns when ctx ends, and removes the socket.
func ServeInput(ctx context.Context, d *Daemon, path string, g InputGates) error {
	e := startInput(ctx, d, path, g)
	return serveUnix(ctx, path, func(c net.Conn) { serveInputConn(func() *inputEngine { return e }, nil, c) })
}

// inputHold holds lines naming messages the engine lacks until settle: until
// the micom's variant record names the car, a family's engine lacks the car's
// messages, and a client sending state only on change would not send it
// again.
type inputHold struct {
	settled chan struct{}
	once    sync.Once
	slots   chan struct{} // bounds the lines held, whose clients may be gone
}

func newInputHold(n int) *inputHold {
	return &inputHold{settled: make(chan struct{}), slots: make(chan struct{}, n)}
}

func (h *inputHold) settle() { h.once.Do(func() { close(h.settled) }) }

// wait blocks until settle, false at once when settled or full.
func (h *inputHold) wait() bool {
	if h == nil {
		return false
	}
	select {
	case <-h.settled:
		return false
	default:
	}
	select {
	case h.slots <- struct{}{}:
	default:
		return false
	}
	defer func() { <-h.slots }()
	<-h.settled
	return true
}

// startInput runs d's input engine until ctx ends; path is for the log.
func startInput(ctx context.Context, d *Daemon, path string, g InputGates) *inputEngine {
	e := newInputEngine(d.cfg.Variant, g, d.sendMessage, d.own, d.cfg.Now, d.logf)
	d.observe(e.observe)
	ok, refused := e.enabled()
	d.logf("input %s: messages %s; refused: %s", path, listOrNone(ok), listOrNone(refused))
	go e.run(ctx)
	return e
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// serveInputConn answers each line, with the engine cur gives then (a client
// stays connected across a variant switch); refusals go to the client, not
// the log, which a client at 1 Hz would flood. A line naming a message the
// engine lacks may wait in hold, with its reply and the lines after it, and
// is then tried on the engine then.
func serveInputConn(cur func() *inputEngine, hold *inputHold, c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		reply := []byte(`{"ok":true}`)
		err := cur().handle(line)
		if _, ok := err.(noMessage); ok && hold.wait() {
			err = cur().handle(line)
		}
		if err != nil {
			reply, _ = json.Marshal(map[string]string{"error": err.Error()})
		}
		c.Write(append(reply, '\n'))
	}
}
