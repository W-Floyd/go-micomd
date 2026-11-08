package micom

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// field is one field of a message (docs/messages.md), its codec both ways:
// read for frames from the micom, put for frames to it. Numbers follow CAN
// DBC signal semantics: raw bits at payload bit positions, then
// physical = raw * scale + offset.
type field struct {
	name, unit string
	typ        string // "int", "bool" or "str"
	signed     bool
	// pos are the payload bits, most significant first; bit k is bit k%8
	// (0 the least significant) of byte k/8, DBC's numbering.
	pos        []int
	off, width int // str: its bytes
	scale      float64
	offset     float64
	enum       string
	codes      map[string]int64
	names      map[int64]string
	hasInvalid bool
	invalid    uint64
	min, max   *float64
	def        *inValue
	utf16      bool
	pad        byte
	hasPad     bool
}

var typeWidths = map[string]int{"u8": 1, "i8": 1, "bool": 1, "u16": 2, "i16": 2, "u32": 4, "i32": 4}

func (f *field) nbits() uint { return uint(len(f.pos)) }

// rawRange is what the field can hold, as a signed or unsigned integer.
func (f *field) rawRange() (lo, hi int64) {
	n := f.nbits()
	if f.signed {
		return -1 << (n - 1), 1<<(n-1) - 1
	}
	return 0, 1<<n - 1
}

func (f *field) load(p []byte) uint64 {
	var v uint64
	for _, k := range f.pos {
		v = v<<1 | uint64(p[k/8]>>(k%8)&1)
	}
	return v
}

func (f *field) store(p []byte, v uint64) {
	n := len(f.pos)
	for i, k := range f.pos {
		bit := byte(1) << (k % 8)
		if v>>(n-1-i)&1 != 0 {
			p[k/8] |= bit
		} else {
			p[k/8] &^= bit
		}
	}
}

// signedRaw is the raw bits as the field's integer.
func (f *field) signedRaw(raw uint64) int64 {
	n := f.nbits()
	if f.signed && raw&(1<<(n-1)) != 0 {
		return int64(raw) - 1<<n
	}
	return int64(raw)
}

// read decodes the field for Decode and Describe.
func (f *field) read(p []byte) Field {
	out := Field{Name: f.name, Unit: f.unit}
	if f.typ == "str" {
		out.Text, out.Valid = f.text(p)
		return out
	}
	raw := f.load(p)
	if f.hasInvalid && raw == f.invalid {
		// The marker itself, as Decode always reported it.
		out.Value = int(raw)
		return out
	}
	r := f.signedRaw(raw)
	out.Valid = true
	out.Value = int(math.Floor(float64(r)*f.scale + f.offset + 0.5))
	if f.enum != "" {
		out.Text = f.names[r]
	}
	return out
}

// value is the field as an input would give it: nil for its invalid marker.
func (f *field) value(p []byte) any {
	if f.typ == "str" {
		if s, ok := f.text(p); ok {
			return s
		}
		return nil
	}
	raw := f.load(p)
	if f.hasInvalid && raw == f.invalid {
		return nil
	}
	r := f.signedRaw(raw)
	switch {
	case f.typ == "bool":
		return r != 0
	case f.enum != "":
		if n, ok := f.names[r]; ok {
			return n
		}
	}
	return float64(r)*f.scale + f.offset
}

// text reads a str field; ASCII is n/a when a byte is unprintable.
func (f *field) text(p []byte) (string, bool) {
	b := p[f.off : f.off+f.width]
	if f.hasPad {
		step := 1
		if f.utf16 {
			step = 2
		}
		for len(b) >= step && b[len(b)-1] == f.pad && b[len(b)-step] == f.pad {
			b = b[:len(b)-step]
		}
	}
	if f.utf16 {
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
		return string(utf16.Decode(u)), true
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return "", false
		}
	}
	return string(b), true
}

// put encodes v into p, or says why it cannot.
func (f *field) put(p []byte, v inValue) error {
	if v.kind == vNull {
		if !f.hasInvalid {
			return fmt.Errorf("%s: null: the field has no invalid marker", f.name)
		}
		f.store(p, f.invalid)
		return nil
	}
	var raw int64
	switch {
	case f.typ == "str":
		return f.putText(p, v)
	case f.typ == "bool":
		if v.kind != vBool {
			return fmt.Errorf("%s: %s is not true or false", f.name, v)
		}
		if v.b {
			raw = 1
		}
	case f.enum != "":
		if v.kind != vStr {
			return fmt.Errorf("%s: %s is not a name of enum %s", f.name, v, f.enum)
		}
		c, ok := f.codes[v.str]
		if !ok {
			return fmt.Errorf("%s: %s is not in enum %s", f.name, v, f.enum)
		}
		raw = c
	default:
		if v.kind != vNum {
			return fmt.Errorf("%s: %s is not a number", f.name, v)
		}
		x := v.num
		switch {
		case math.IsNaN(x) || math.IsInf(x, 0):
			return fmt.Errorf("%s: %s is not a number", f.name, v)
		case f.min != nil && x < *f.min:
			return fmt.Errorf("%s: %s is below the minimum %v", f.name, v, *f.min)
		case f.max != nil && x > *f.max:
			return fmt.Errorf("%s: %s is above the maximum %v", f.name, v, *f.max)
		}
		var r float64
		if f.scale == 1 && f.offset == 0 {
			if x != math.Trunc(x) {
				return fmt.Errorf("%s: %s is not an integer", f.name, v)
			}
			r = x
		} else {
			// Half up, as Java's Math.round in the stock packers.
			r = math.Floor((x-f.offset)/f.scale + 0.5)
		}
		lo, hi := f.rawRange()
		if r < float64(lo) || r > float64(hi) {
			return fmt.Errorf("%s: %s does not fit the field (raw %v, not %d..%d)", f.name, v, r, lo, hi)
		}
		raw = int64(r)
	}
	if f.hasInvalid && uint64(raw)&(1<<f.nbits()-1) == f.invalid {
		return fmt.Errorf("%s: %s encodes to the invalid marker %#x", f.name, v, f.invalid)
	}
	f.store(p, uint64(raw))
	return nil
}

func (f *field) putText(p []byte, v inValue) error {
	if v.kind != vStr {
		return fmt.Errorf("%s: %s is not text", f.name, v)
	}
	var b []byte
	if f.utf16 {
		for _, u := range utf16.Encode([]rune(v.str)) {
			b = append(b, byte(u), byte(u>>8))
		}
	} else {
		for _, c := range []byte(v.str) {
			if c < 0x20 || c > 0x7e {
				return fmt.Errorf("%s: %s is not printable ASCII", f.name, v)
			}
		}
		b = []byte(v.str)
	}
	if len(b) > f.width {
		return fmt.Errorf("%s: %s is %d bytes, more than %d", f.name, v, len(b), f.width)
	}
	n := copy(p[f.off:f.off+f.width], b)
	for i := f.off + n; i < f.off+f.width; i++ {
		p[i] = f.pad
	}
	return nil
}

// bytePositions are the bits of width bytes at off, most significant first.
func bytePositions(off, width int, little bool) []int {
	var out []int
	for i := 0; i < width; i++ {
		b := off + i
		if little {
			b = off + width - 1 - i
		}
		for k := 7; k >= 0; k-- {
			out = append(out, 8*b+k)
		}
	}
	return out
}

// dbcPositions are a DBC signal's bits, most significant first. Intel
// (little): start is the least significant bit and the rest follow upward.
// Motorola (big): start is the most significant bit, and the next is the
// one below it, wrapping to bit 7 of the following byte.
func dbcPositions(start, length int, little bool) []int {
	out := make([]int, length)
	if little {
		for i := range out {
			out[i] = start + length - 1 - i
		}
		return out
	}
	k := start
	for i := range out {
		out[i] = k
		if k%8 == 0 {
			k += 15
		} else {
			k--
		}
	}
	return out
}

// inValue is one input value, from JSON or from a variant's YAML.
type inValue struct {
	kind valKind
	num  float64
	str  string
	b    bool
}

type valKind int

const (
	vNull valKind = iota
	vNum
	vStr
	vBool
)

func (v inValue) String() string {
	switch v.kind {
	case vNum:
		return strconv.FormatFloat(v.num, 'g', -1, 64)
	case vStr:
		return strconv.Quote(v.str)
	case vBool:
		return strconv.FormatBool(v.b)
	}
	return "null"
}

// toInValue takes what encoding/json (with UseNumber) or the YAML decoder
// gives for a scalar.
func toInValue(x any) (inValue, error) {
	switch t := x.(type) {
	case nil:
		return inValue{}, nil
	case bool:
		return inValue{kind: vBool, b: t}, nil
	case string:
		return inValue{kind: vStr, str: t}, nil
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return inValue{}, fmt.Errorf("%s is not a number", t)
		}
		return inValue{kind: vNum, num: f}, nil
	case float64:
		return inValue{kind: vNum, num: t}, nil
	case int:
		return inValue{kind: vNum, num: float64(t)}, nil
	case int64:
		return inValue{kind: vNum, num: float64(t)}, nil
	case uint64:
		return inValue{kind: vNum, num: float64(t)}, nil
	}
	return inValue{}, fmt.Errorf("%v is not a number, text, true, false or null", x)
}

// parseBits reads "hi:lo" or "n".
func parseBits(s string) (hi, lo int, err error) {
	a, b, two := strings.Cut(s, ":")
	h, err1 := strconv.Atoi(strings.TrimSpace(a))
	l := h
	var err2 error
	if two {
		l, err2 = strconv.Atoi(strings.TrimSpace(b))
	}
	if err1 != nil || err2 != nil || h > 7 || l < 0 || l > h {
		return 0, 0, fmt.Errorf("bits %q is not \"hi:lo\" within a byte (7 to 0)", s)
	}
	return h, l, nil
}
