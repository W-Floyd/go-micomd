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
// messages (docs/messages.md): it validates, merges, encodes and sends.
type inputEngine struct {
	v    *Variant
	send func(Frame) error
	now  func() time.Time
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
}

func newInputEngine(v *Variant, inferred bool, send func(Frame) error, now func() time.Time) *inputEngine {
	e := &inputEngine{v: v, send: send, now: now, refused: map[string]string{}, state: map[string]*msgState{}}
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
	if st == nil {
		st = &msgState{}
		e.state[name] = st
	}
	st.vals, st.lastInput = merged, e.now()
	if st.sent && (!m.onChange || sameFrame(f, st.last)) {
		st.last = f
		return nil
	}
	st.last, st.sent = f, true
	if err := e.send(f); err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	return nil
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
	e := newInputEngine(d.cfg.Variant, inferred, d.sendMessage, d.cfg.Now)
	ok, refused := e.enabled()
	d.logf("input %s: messages %s; refused: %s", path, listOrNone(ok), listOrNone(refused))
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
