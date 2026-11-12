package micom

import (
	"fmt"
	"strings"
	"time"
)

// Describe is a one-line reading of f for the frames whose layout the
// variant knows, "" for the rest.
func (v *Variant) Describe(f Frame) string {
	if s := v.describe(f); s != "" {
		return s
	}
	fs, name := v.decode(f)
	if fs == nil {
		return ""
	}
	parts := make([]string, len(fs))
	for i, x := range fs {
		parts[i] = x.String()
	}
	return name + ": " + strings.Join(parts, " ")
}

// describe reads the key and state frames, "" for the rest.
func (v *Variant) describe(f Frame) string {
	hc := hdrCmd{f.Hdr, f.Cmd}
	if v.hasKeys && hc == v.keyFrame {
		return v.describeKey(f.Payload)
	}
	s := v.signals[hc]
	switch {
	case s == nil:
		return ""
	case s.length == 0 && len(f.Payload) == 0:
		return s.label + ": " + s.suffix
	case s.length == 0 || len(f.Payload) == s.length:
		return s.describe(f.Payload[0])
	}
	return ""
}

// describeKey: p[0] the type, p[1] the code (low nibble) and the press or a
// knob's direction (high nibble).
func (v *Variant) describeKey(p []byte) string {
	if len(p) < 2 {
		return ""
	}
	typ, code, hi := p[0], p[1]&0xf, p[1]>>4
	if name, ok := v.knobLabels[typ]; ok {
		times := ""
		if code > 1 {
			times = fmt.Sprintf(" ×%d", code) // the detents turned
		}
		switch hi {
		case 1:
			return name + " up" + times
		case 0:
			return name + " down" + times
		}
		return fmt.Sprintf("%s turn %d code %d", name, hi, code)
	}
	label, ok := v.pushLabels[[2]byte{typ, code}]
	if !ok || label == "" {
		label = fmt.Sprintf("Key %d.%d", typ, code)
	}
	press := fmt.Sprintf("press %d", hi)
	switch hi {
	case PressRelease:
		press = "release"
	case PressShort:
		press = "press"
	case PressLong:
		press = "long press"
	}
	return label + " " + press
}

// FrameName names the frames the variant knows by layout, "" for the rest.
// Signal frames (signals:, e.g. door-driver-lock) describe fine via Describe
// but were missing here: Describe tries v.describe (signals, then keys)
// before v.decode, and this skipped straight to the decoders table.
func (v *Variant) FrameName(f Frame) string {
	if IsAliveRequest(f) {
		return "CPU alive request"
	}
	if n, ok := v.names[hdrCmd{f.Hdr, f.Cmd}]; ok {
		return n
	}
	if s := v.signals[hdrCmd{f.Hdr, f.Cmd}]; s != nil {
		return s.name
	}
	for _, d := range v.decoders[f.Cmd] {
		if d.matches(f.Hdr) {
			return d.name
		}
	}
	return ""
}

// SignalReading is a state frame (a variant's signal) as its variant reads
// it.
type SignalReading struct {
	// Name is the signal's, Text its value's ("open", "drive", "heat level 2").
	Name, Text string
	Raw        byte
	// Number is a number signal's value, or the state of its special value;
	// nil for other kinds and for a special value without one.
	Number *int
}

// Signal reads f if it is one of the variant's signals with a value it
// knows; false for the rest, and for a burst's brackets.
func (v *Variant) Signal(f Frame) (SignalReading, bool) {
	s := v.signals[hdrCmd{f.Hdr, f.Cmd}]
	if s == nil || s.kind == kindBurst || len(f.Payload) == 0 || s.length != 0 && len(f.Payload) != s.length {
		return SignalReading{}, false
	}
	b := f.Payload[0]
	text, ok := s.text(b)
	if !ok {
		return SignalReading{}, false
	}
	r := SignalReading{Name: s.name, Text: text, Raw: b}
	if s.kind == kindNumber {
		if vs, ok := s.values[b]; ok {
			r.Number = vs.State
		} else if n, ok := s.number(b); ok {
			r.Number = &n
		}
	}
	return r, true
}

// SentName is the name of the to_micom message f matches, by frame, as the
// variant declared it with fields (a messages: entry); "" for the rest. A
// frame that is only a headers: alias, or a literal identify.request, is not
// one: this names what we send, the way FrameName names what the micom
// sends, but the two tables are built separately (compileMessages: decoders
// of from_micom messages, encoders of to_micom ones), so neither falls back
// to the other.
func (v *Variant) SentName(f Frame) string {
	if m := v.sent[hdrCmd{f.Hdr, f.Cmd}]; m != nil {
		return m.name
	}
	return ""
}

// DescribeSent is a one-line reading of f, a frame sent to the micom, for
// the to_micom messages whose layout is declared with fields; "" for the
// rest, including a length that does not match the declaration.
func (v *Variant) DescribeSent(f Frame) string {
	m := v.sent[hdrCmd{f.Hdr, f.Cmd}]
	if m == nil || len(f.Payload) != m.length {
		return ""
	}
	parts := make([]string, len(m.fields))
	for i, fl := range m.fields {
		parts[i] = fl.read(f.Payload).String()
	}
	return m.name + ": " + strings.Join(parts, " ")
}

// Decode reads the fields of a frame whose layout is known; nil for the rest.
func (v *Variant) Decode(f Frame) []Field {
	fs, _ := v.decode(f)
	return fs
}

// decode is Decode and the name of the message that read f, which, when
// guards tell apart frames of one header and command, is not FrameName's.
func (v *Variant) decode(f Frame) ([]Field, string) {
	p := f.Payload
	for _, d := range v.decoders[f.Cmd] {
		if !d.matches(f.Hdr) || !d.match(p) {
			continue
		}
		out := make([]Field, 0, len(d.fields))
		for _, fs := range d.fields {
			out = append(out, fs.read(p))
		}
		return out, d.name
	}
	return nil, ""
}

func (d *decoder) match(p []byte) bool {
	if len(p) < d.minLen {
		return false
	}
	for _, g := range d.guards {
		if g.hasLen && len(p) != g.length || !g.hasLen && (g.off >= len(p) || p[g.off] != g.eq) {
			return false
		}
	}
	return true
}

// ApplyClimate folds f into c, by the variant's state frames.
func (v *Variant) ApplyClimate(c *CarClimate, f Frame) ClimateEvent {
	if len(f.Payload) != 1 {
		return ClimateNone
	}
	s := v.signals[hdrCmd{f.Hdr, f.Cmd}]
	if s == nil || s.length > 1 || s.state == "" && s.kind != kindBurst {
		return ClimateNone
	}
	return s.apply(c, f.Payload[0])
}

// Profile is the routine traffic the variant sends unasked, without the
// frames that need -clock.
func (v *Variant) Profile() []Periodic { return v.periodic("") }

// ClockProfile is the variant's frames that carry this machine's time: only
// for a caller whose clock is right.
func (v *Variant) ClockProfile() []Periodic { return v.periodic("clock") }

// CodeProfile is Profile with each opt_in code entry, known only from stock
// code, in place of the captured entry it replaces: for -code-profile.
func (v *Variant) CodeProfile() []Periodic {
	by := map[string]periodicSpec{}
	for _, p := range v.profile {
		if p.optIn == "code" {
			by[p.replaces] = p
		}
	}
	out := v.periodic("")
	for i, p := range out {
		if r, ok := by[p.Name]; ok {
			out[i] = Periodic{Name: r.name, Every: r.every, Build: r.build}
		}
	}
	return out
}

func (v *Variant) periodic(optIn string) []Periodic {
	out := []Periodic{}
	for _, p := range v.profile {
		if p.optIn != optIn {
			continue
		}
		out = append(out, Periodic{Name: p.name, Every: p.every, Build: p.build})
	}
	return out
}

func (p periodicSpec) build(now time.Time) Frame {
	f := Frame{Hdr: p.hdr, Cmd: p.cmd}
	if len(p.slots) > 0 {
		f.Payload = make([]byte, len(p.slots))
	}
	for i, s := range p.slots {
		if s.field != "" {
			f.Payload[i] = clockFields[s.field](now)
		} else {
			f.Payload[i] = s.fixed
		}
	}
	return f
}

// KeyFunc is the Func (hdr[3]<<8 | cmd) of the variant's key frames, for
// injectKeys; false when it has none.
func (v *Variant) KeyFunc() (uint16, bool) {
	return uint16(v.keyFrame.hdr[3])<<8 | uint16(v.keyFrame.cmd), v.hasKeys
}

// keymap is the variant's default keymap and the keys it uses.
func (v *Variant) keymap() (keymap, []uint16) { return v.keys, v.keyList }

// Sends is what the control socket may send for this variant: captured
// entries, and with inferred also those read from code or worked out. A
// dangerous entry is left out unless dangerous is set (-allow-dangerous) or
// dangerousMsgs names it (-allow-dangerous-msg).
func (v *Variant) Sends(inferred, dangerous bool, dangerousMsgs ...string) ([]Frame, []Pattern) {
	g := dangerGate{dangerous, dangerousMsgs}
	var fs []Frame
	var ps []Pattern
	for _, s := range v.sends {
		if s.status != "captured" && !inferred {
			continue
		}
		if g.refuses(s.name, s.dangerous) != "" {
			continue
		}
		if s.frame != nil {
			fs = append(fs, *s.frame)
		} else {
			ps = append(ps, *s.pattern)
		}
	}
	return fs, ps
}

// ReplyIdentity reads the identity out of the micom's variant record, by the
// variant's identify.reply.identity; false when f is not that record. Like
// ParseIdentity it leaves out an invalid field and the 0xff of an unread
// vehicle or country code.
func (v *Variant) ReplyIdentity(f Frame) (Identity, bool) {
	if v.reply == nil || f.Hdr != v.reply.hdr || f.Cmd != v.reply.cmd {
		return nil, false
	}
	byName := map[string]Field{}
	for _, fl := range v.Decode(f) {
		byName[fl.Name] = fl
	}
	id := Identity{}
	for k, name := range v.replyIdentity {
		fl, ok := byName[name]
		if !ok || !fl.Valid || (k == "vehicle_info" || k == "country_info") && fl.Value == 0xff {
			continue
		}
		id[k] = fl.Value
	}
	return id, true
}

// decodesField is whether a from_micom message decoding frames of hc has a
// field called name.
func (v *Variant) decodesField(hc hdrCmd, name string) bool {
	for _, d := range v.decoders[hc.cmd] {
		if !d.matches(hc.hdr) {
			continue
		}
		for _, f := range d.fields {
			if f.name == name {
				return true
			}
		}
	}
	return false
}

// The send entry -boot-mode-request sends, and the from_micom panel that is
// the micom's answer.
const (
	bootModeRequest = "boot-mode-request"
	bootRecordName  = "Boot record"
)

// The send entry -hev-status-request sends, and the from_micom message that
// is the micom's answer.
const (
	hevStatusRequest = "hev-status-request"
	hevStatusName    = "HEV status"
)

// The other start-up reads' send entries and answers.
const (
	climateStatusRequest = "climate-status-request"
	climateSnapshotName  = "Climate snapshot"
	bcanValidRequest     = "bcan-valid-request"
	bodyStatusRequest    = "body-status-request"
	bodySnapshotName     = "Body snapshot"
	usmUnitsRequest      = "usm-units-request"
	usmUnitsName         = "Units (USM)"
	evSettingsRequest    = "ev-schedule-settings-request"
	evSettingsName       = "Schedule settings"
	evWindowRequest      = "ev-charge-window-request"
	evWindowName         = "Scheduled charge window"
)

// SendNamed is the whole-frame send entry called name, whatever its status.
func (v *Variant) SendNamed(name string) (Frame, bool) {
	for _, s := range v.sends {
		if s.name == name && s.frame != nil {
			return *s.frame, true
		}
	}
	return Frame{}, false
}

// IdentifyRequest is the frame that asks the micom for its variant coding.
// One that was not captured is refused unless inferred is set.
func (v *Variant) IdentifyRequest(inferred bool) (Frame, error) {
	if v.identify == nil {
		return Frame{}, fmt.Errorf("variant %s has no identify request", v.Name)
	}
	if v.identify.status != "captured" && !inferred {
		return Frame{}, fmt.Errorf("variant %s: the identify request is %s, not captured: it needs -allow-inferred", v.Name, v.identify.status)
	}
	return *v.identify.frame, nil
}
