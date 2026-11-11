package micom

import (
	"fmt"
	"maps"
	"slices"
)

// The micom's side of a variant, for an emulated micom (cmd/micom-fake):
// from_micom messages built from names and values, and what a frame sent to
// the micom changes in what it reports (a to_micom field's reflects:).
// micomd itself only decodes from_micom frames and encodes to_micom ones.

type reflectSpec struct {
	msg, field string
	unless     []uint64
}

func (v *Variant) decoderNamed(name string) *decoder {
	for _, c := range slices.Sorted(maps.Keys(v.decoders)) {
		for _, d := range v.decoders[c] {
			if d.name == name {
				return d
			}
		}
	}
	return nil
}

func (d *decoder) field(name string) *field {
	for _, f := range d.fields {
		if f.name == name {
			return f
		}
	}
	return nil
}

// length is what the message's frame carries: its length, else its minimum.
func (d *decoder) length() int {
	for _, g := range d.guards {
		if g.hasLen {
			return g.length
		}
	}
	return d.minLen
}

// ReceivedMessages names the from_micom messages for a header and command,
// the longest first: what a micom answering a read of it may send.
func (v *Variant) ReceivedMessages(hdr [4]byte, cmd byte) []string {
	var ds []*decoder
	for _, d := range v.decoders[cmd] {
		if d.matches(hdr) {
			ds = append(ds, d)
		}
	}
	slices.SortStableFunc(ds, func(a, b *decoder) int { return b.length() - a.length() })
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.name
	}
	return out
}

// FieldNames lists a from_micom message's fields.
func (v *Variant) FieldNames(msg string) []string {
	d := v.decoderNamed(msg)
	if d == nil {
		return nil
	}
	out := make([]string, len(d.fields))
	for i, f := range d.fields {
		out[i] = f.name
	}
	return out
}

// RawFields reads a frame from the micom as its message's name and the raw
// code of each field of every message that matches it (two messages may
// read one frame's bytes apart): the state an emulator keeps, by field
// name, to answer reads with. False for a frame no from_micom message
// describes.
func (v *Variant) RawFields(f Frame) (string, map[string]uint64, bool) {
	name, out := "", map[string]uint64(nil)
	for _, d := range v.decoders[f.Cmd] {
		if !d.matches(f.Hdr) || !d.match(f.Payload) {
			continue
		}
		if out == nil {
			name, out = d.name, map[string]uint64{}
		}
		for _, fs := range d.fields {
			if fs.typ != "str" {
				out[fs.name] = fs.load(f.Payload)
			}
		}
	}
	return name, out, out != nil
}

// EncodeReceived builds the frame the from_micom message msg would arrive
// as: base (a payload of it held before; nil for zeros) with its guards'
// bytes and the given fields set, as inputs give them (numbers in the
// field's unit, enum names, true or false, nil for the invalid marker).
func (v *Variant) EncodeReceived(msg string, base []byte, vals map[string]any) (Frame, error) {
	return v.encodeReceived(msg, base, func(d *decoder, p []byte) error {
		for _, k := range sortedKeys(vals) {
			f := d.field(k)
			if f == nil {
				return fmt.Errorf("%s: no field %q", msg, k)
			}
			x, err := toInValue(vals[k])
			if err != nil {
				return fmt.Errorf("%s: %v", k, err)
			}
			if err := f.put(p, x); err != nil {
				return err
			}
		}
		return nil
	})
}

// EncodeReceivedRaw is EncodeReceived with raw codes, as RawFields reads
// them; names the message lacks are skipped.
func (v *Variant) EncodeReceivedRaw(msg string, base []byte, raw map[string]uint64) (Frame, error) {
	return v.encodeReceived(msg, base, func(d *decoder, p []byte) error {
		for _, f := range d.fields {
			if x, ok := raw[f.name]; ok && f.typ != "str" {
				f.store(p, x&(1<<f.nbits()-1))
			}
		}
		return nil
	})
}

func (v *Variant) encodeReceived(msg string, base []byte, set func(*decoder, []byte) error) (Frame, error) {
	d := v.decoderNamed(msg)
	if d == nil {
		return Frame{}, fmt.Errorf("%q is not a from_micom message", msg)
	}
	hdr, ok := v.fullHeader(d)
	if !ok {
		return Frame{}, fmt.Errorf("%s: header %x/%x fits no from_micom header of the variant", msg, d.hdr, d.mask)
	}
	p := make([]byte, max(d.length(), len(base)))
	copy(p, base)
	for _, g := range d.guards {
		if !g.hasLen {
			p[g.off] = g.eq
		}
	}
	if err := set(d, p); err != nil {
		return Frame{}, err
	}
	return Frame{Hdr: hdr, Cmd: d.cmd(v), Payload: p}, nil
}

// cmd is the command the decoder is filed under.
func (d *decoder) cmd(v *Variant) byte {
	for c, ds := range v.decoders {
		if slices.Contains(ds, d) {
			return c
		}
	}
	return 0
}

// fullHeader fills a decoder's open header bytes ("0c 88 ?? 8c") from the
// first from_micom header that fits it.
func (v *Variant) fullHeader(d *decoder) ([4]byte, bool) {
	if d.mask == [4]byte{0xff, 0xff, 0xff, 0xff} {
		return d.hdr, true
	}
	for _, h := range v.rxHeaders {
		if d.matches(h) {
			return h, true
		}
	}
	return [4]byte{}, false
}

// Reflect is what the frame f, sent to the micom, changes in what the micom
// reports, by the to_micom fields' reflects: each from_micom message a field
// of f reflects into, rebuilt from base(msg) (the payload held; nil: zeros)
// with the fields' raw values. Values in unless and invalid markers set
// nothing. None for a frame no to_micom message describes.
func (v *Variant) Reflect(f Frame, base func(msg string) []byte) ([]Frame, error) {
	m := v.sent[hdrCmd{f.Hdr, f.Cmd}]
	if m == nil || len(f.Payload) != m.length {
		return nil, nil
	}
	set := map[string]map[string]uint64{}
	var order []string
	for _, fs := range m.fields {
		r := fs.reflect
		if r == nil {
			continue
		}
		x := fs.load(f.Payload)
		if fs.hasInvalid && x == fs.invalid || slices.Contains(r.unless, x) {
			continue
		}
		if set[r.msg] == nil {
			set[r.msg] = map[string]uint64{}
			order = append(order, r.msg)
		}
		set[r.msg][r.field] = x
	}
	// Messages that are one frame (header, command, guards) build one
	// payload, each over the last.
	var out []Frame
	at := map[string]int{}
	for _, msg := range order {
		probe, err := v.EncodeReceived(msg, nil, nil)
		if err != nil {
			return out, err
		}
		k := fmt.Sprintf("%x %02x %x", probe.Hdr, probe.Cmd, v.guardBytes(msg, probe.Payload))
		i, seen := at[k]
		b := base(msg)
		if seen {
			b = out[i].Payload
		}
		fr, err := v.EncodeReceivedRaw(msg, b, set[msg])
		if err != nil {
			return out, err
		}
		if seen {
			out[i] = fr
		} else {
			at[k] = len(out)
			out = append(out, fr)
		}
	}
	return out, nil
}

// guardBytes are the bytes msg's guards fix in p.
func (v *Variant) guardBytes(msg string, p []byte) []byte {
	var out []byte
	for _, g := range v.decoderNamed(msg).guards {
		if !g.hasLen {
			out = append(out, byte(g.off), p[g.off])
		}
	}
	return out
}
