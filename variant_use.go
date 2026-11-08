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
	fs := v.Decode(f)
	if fs == nil {
		return ""
	}
	parts := make([]string, len(fs))
	for i, x := range fs {
		parts[i] = x.String()
	}
	return v.FrameName(f) + ": " + strings.Join(parts, " ")
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
		switch hi {
		case 1:
			return name + " up"
		case 0:
			return name + " down"
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
func (v *Variant) FrameName(f Frame) string {
	if IsAliveRequest(f) {
		return "CPU alive request"
	}
	if n, ok := v.names[hdrCmd{f.Hdr, f.Cmd}]; ok {
		return n
	}
	for _, d := range v.decoders[f.Cmd] {
		if d.matches(f.Hdr) {
			return d.name
		}
	}
	return ""
}

// Decode reads the fields of a frame whose layout is known; nil for the rest.
func (v *Variant) Decode(f Frame) []Field {
	p := f.Payload
	for _, d := range v.decoders[f.Cmd] {
		if !d.matches(f.Hdr) || !d.match(p) {
			continue
		}
		out := make([]Field, 0, len(d.fields))
		for _, fs := range d.fields {
			out = append(out, fs.read(p))
		}
		return out
	}
	return nil
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

func (fs fieldSpec) read(p []byte) Field {
	f := Field{Name: fs.name, Unit: fs.unit}
	switch {
	case fs.text:
		b := p[fs.off : fs.off+fs.size]
		f.Text, f.Valid = string(b), true
		for _, c := range b {
			if c < 0x20 || c > 0x7e {
				f.Text, f.Valid = "", false
			}
		}
	case fs.bit:
		f.Value, f.Valid = int(p[fs.off]>>fs.shift&fs.mask), true
	case fs.size == 1:
		f.Value, f.Valid = int(p[fs.off]), fs.naNone || p[fs.off] != 0xff
	default:
		f.Value = be16(p[fs.off:])
		f.Valid = fs.naNone || f.Value != 0xffff
	}
	return f
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
// entries, and with inferred also those read from code or worked out.
func (v *Variant) Sends(inferred bool) ([]Frame, []Pattern) {
	var fs []Frame
	var ps []Pattern
	for _, s := range v.sends {
		if s.status != "captured" && !inferred {
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
