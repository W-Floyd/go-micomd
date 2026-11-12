package micom

import (
	"fmt"
	"slices"
	"strings"
)

// A client's read-modify-write of a setting the micom holds: a to_micom
// message whose fields reflect from_micom fields is rebuilt from what
// the micom last reported, with only the changed fields different, and the
// change is confirmed when the reports match it (docs/messages.md,
// "Reflects").

// Received is a from_micom field as received: decoded, and its raw code.
type Received struct {
	Field
	Raw uint64
}

// ReceivedFields reads f as every from_micom message that matches it (two may
// read one frame's bytes apart, which Decode does not), by message and
// field. Nil for a frame no from_micom message describes.
func (v *Variant) ReceivedFields(f Frame) map[string]map[string]Received {
	var out map[string]map[string]Received
	for _, d := range v.decoders[f.Cmd] {
		if !d.matches(f.Hdr) || !d.match(f.Payload) {
			continue
		}
		if out == nil {
			out = map[string]map[string]Received{}
		}
		fs := map[string]Received{}
		for _, x := range d.fields {
			if x.typ != "str" {
				fs[x.name] = Received{x.read(f.Payload), x.load(f.Payload)}
			}
		}
		out[d.name] = fs
	}
	return out
}

// RawCode is the raw code value (as an input gives it: a number in the
// field's unit, an enum name, true or false) takes in the from_micom field
// msg/field.
func (v *Variant) RawCode(msg, field string, value any) (uint64, error) {
	d := v.decoderNamed(msg)
	if d == nil {
		return 0, fmt.Errorf("%q is not a from_micom message", msg)
	}
	f := d.field(field)
	if f == nil || f.typ == "str" {
		return 0, fmt.Errorf("%s: no field %q", msg, field)
	}
	x, err := toInValue(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %v", field, err)
	}
	p := make([]byte, max(d.length(), slices.Max(f.pos)/8+1))
	if err := f.put(p, x); err != nil {
		return 0, err
	}
	return f.load(p), nil
}

// Reflection is a to_micom field and the from_micom field (message Msg,
// field From) the micom reports its value back in.
type Reflection struct{ Field, Msg, From string }

// Reflections lists what the to_micom message msg's fields reflect.
func (v *Variant) Reflections(msg string) []Reflection {
	m := v.messages[msg]
	if m == nil {
		return nil
	}
	var out []Reflection
	for _, f := range m.fields {
		if r := f.reflect; r != nil {
			out = append(out, Reflection{f.name, r.msg, r.field})
		}
	}
	return out
}

// ReflectedInput is the input (without "msg") for the to_micom message msg
// that sends back what its fields reflect, raw(msg, field) giving each
// from_micom field's raw code: the micom's own state, with whatever the
// caller changed in it. Fields that reflect nothing are left out, to their
// default or source. It fails, naming the fields, when a reflected field has
// no value, or one its to_micom field cannot take: wider than it, the
// invalid marker, an unless code, an enum code without a name, or outside
// min/max.
func (v *Variant) ReflectedInput(msg string, raw func(msg, field string) (uint64, bool)) (map[string]any, error) {
	m := v.messages[msg]
	if m == nil {
		return nil, fmt.Errorf("%q is not a to_micom message", msg)
	}
	out := map[string]any{}
	var missing, bad []string
	for _, f := range m.fields {
		r := f.reflect
		if r == nil {
			continue
		}
		x, ok := raw(r.msg, r.field)
		switch {
		case !ok:
			missing = append(missing, r.field)
			continue
		case x > 1<<f.nbits()-1:
			bad = append(bad, fmt.Sprintf("%s reads %d, wider than %s", r.field, x, f.name))
			continue
		case f.hasInvalid && x == f.invalid:
			bad = append(bad, fmt.Sprintf("%s reads %d, %s's invalid marker", r.field, x, f.name))
			continue
		case slices.Contains(r.unless, x):
			bad = append(bad, fmt.Sprintf("%s reads %d, which %s does not set", r.field, x, f.name))
			continue
		}
		p := make([]byte, m.length)
		f.store(p, x)
		val := f.value(p)
		if _, named := val.(string); f.enum != "" && !named {
			bad = append(bad, fmt.Sprintf("%s reads code %d, which enum %s does not name", r.field, x, f.enum))
			continue
		}
		out[f.name] = val
	}
	if missing != nil {
		bad = append(bad, "nothing received yet of "+strings.Join(missing, ", "))
	}
	if bad != nil {
		return nil, fmt.Errorf("%s: %s", msg, strings.Join(bad, "; "))
	}
	if _, err := m.parse(out); err != nil {
		return nil, fmt.Errorf("%s: %v", msg, err)
	}
	return out, nil
}
