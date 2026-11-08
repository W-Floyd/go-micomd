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
}

type msgState struct {
	vals      map[string]inValue // as given, defaults left out
	last      Frame
	sent      bool
	lastInput time.Time
	nextSend  time.Time
}

// tickEvery bounds how late a repeat or a timeout runs.
const tickEvery = 50 * time.Millisecond

func newInputEngine(v *Variant, inferred bool, send func(Frame) error, own func([]string, bool), now func() time.Time, logf func(string, ...any)) *inputEngine {
	e := &inputEngine{v: v, send: send, own: own, now: now, logf: logf, refused: map[string]string{}, state: map[string]*msgState{}}
	for n, m := range v.messages {
		if m.status != "captured" && !inferred {
			e.refused[n] = "status " + m.status + ": needs -allow-inferred"
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
		return fmt.Errorf("no message %q", name)
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
	f, err := m.encode(merged)
	if err != nil {
		return fmt.Errorf("%s: %v", name, err)
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
	st.vals, st.lastInput = merged, now
	if st.sent && (!m.onChange || sameFrame(f, st.last)) {
		st.last = f
		return nil
	}
	st.last, st.sent, st.nextSend = f, true, now.Add(m.every)
	if err := e.send(f); err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	return nil
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
		f, err := t.encode(vals)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %v", t.name, err))
			continue
		}
		if ts != nil && t != m {
			// Another active message keeps the new values, and repeats them.
			ts.vals, ts.last = vals, f
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
		if m.every > 0 && !now.Before(st.nextSend) {
			st.nextSend = now.Add(m.every)
			if err := e.send(st.last); err != nil {
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

// ServeInput serves the structured input on a unix socket (mode 0660): one
// JSON object per line, {"msg": NAME, field: value, ...}, each answered
// with {"ok":true} or {"error":"why"}. Only the daemon's variant's to_micom
// messages are sent, those not captured only with inferred; the denylist
// and the CPU-alive frame are refused whatever the variant says. It
// returns when ctx ends, and removes the socket.
func ServeInput(ctx context.Context, d *Daemon, path string, inferred bool) error {
	e := newInputEngine(d.cfg.Variant, inferred, d.sendMessage, d.own, d.cfg.Now, d.logf)
	ok, refused := e.enabled()
	d.logf("input %s: messages %s; refused: %s", path, listOrNone(ok), listOrNone(refused))
	go e.run(ctx)
	return serveUnix(ctx, path, func(c net.Conn) { serveInputConn(e, c) })
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// serveInputConn answers each line; refusals go to the client, not the log,
// which a client at 1 Hz would flood.
func serveInputConn(e *inputEngine, c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		reply := []byte(`{"ok":true}`)
		if err := e.handle(line); err != nil {
			reply, _ = json.Marshal(map[string]string{"error": err.Error()})
		}
		c.Write(append(reply, '\n'))
	}
}
