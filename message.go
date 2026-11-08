package micom

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"go.yaml.in/yaml/v4"
)

// The messages: and enums: of a variant file (docs/messages.md).

type rawMessage struct {
	Name      string        `yaml:"name"`
	Dir       string        `yaml:"dir,omitempty"`
	Hdr       string        `yaml:"hdr,omitempty"`
	Cmd       uint8         `yaml:"cmd"`
	Length    *int          `yaml:"length,omitempty"`
	MinLength *int          `yaml:"min_length,omitempty"`
	When      []rawGuard    `yaml:"when,omitempty"`
	Fill      *uint8        `yaml:"fill,omitempty"`
	Transport *rawTransport `yaml:"transport,omitempty"`
	Fields    []rawMsgField `yaml:"fields,omitempty"`
	OnChange  *bool         `yaml:"on_change,omitempty"`
	Every     time.Duration `yaml:"every,omitempty"`
	Owns      string        `yaml:"owns,omitempty"`
	Timeout   time.Duration `yaml:"timeout,omitempty"`
	End       []rawEndStep  `yaml:"end,omitempty"`
	// MinInterval spaces sends; inputs within it are coalesced, the newest
	// sent when it has passed.
	MinInterval time.Duration `yaml:"min_interval,omitempty"`
	// Persistent: the micom writes its EEPROM for every frame.
	Persistent bool   `yaml:"persistent,omitempty"`
	Status     string `yaml:"status,omitempty"`
	Ref        string `yaml:"ref,omitempty"`
	Note       string `yaml:"note,omitempty"`
	Remove     bool   `yaml:"remove,omitempty"`
}

type rawMsgField struct {
	Name      string   `yaml:"name"`
	Off       *int     `yaml:"off,omitempty"`
	StartBit  *int     `yaml:"start_bit,omitempty"`
	BitLength int      `yaml:"bit_length,omitempty"`
	Signed    bool     `yaml:"signed,omitempty"`
	Type      string   `yaml:"type,omitempty"`
	Endian    string   `yaml:"endian,omitempty"`
	Bits      string   `yaml:"bits,omitempty"`
	Scale     *float64 `yaml:"scale,omitempty"`
	Offset    float64  `yaml:"offset,omitempty"`
	Enum      string   `yaml:"enum,omitempty"`
	Invalid   *uint64  `yaml:"invalid,omitempty"`
	Min       *float64 `yaml:"min,omitempty"`
	Max       *float64 `yaml:"max,omitempty"`
	Default   any      `yaml:"default,omitempty"`
	Unit      string   `yaml:"unit,omitempty"`
	Size      int      `yaml:"size,omitempty"`
	Encoding  string   `yaml:"encoding,omitempty"`
	PadRight  *uint8   `yaml:"pad_right,omitempty"`
}

type rawEndStep struct {
	Msg string         `yaml:"msg"`
	Set map[string]any `yaml:"set,omitempty"`
}

// rawTransport is "single", or a map for the kinds that need parameters.
type rawTransport struct {
	Kind   string `yaml:"kind"`
	Func   *uint8 `yaml:"func,omitempty"`
	Prefix *uint8 `yaml:"prefix,omitempty"`
}

func (t *rawTransport) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*t = rawTransport{Kind: n.Value}
		return nil
	}
	type plain rawTransport
	return n.Load((*plain)(t), yaml.WithKnownFields())
}

// msgSpec is a to_micom message: how an input becomes its frame, and its
// lifecycle.
type msgSpec struct {
	name, status   string
	hdr            [4]byte
	cmd            byte
	length         int
	base           []byte // fill in the bytes no field covers
	fields         []*field
	byName         map[string]*field
	onChange       bool
	every, timeout time.Duration
	minInterval    time.Duration
	persistent     bool
	owns           string
	end            []endStep
}

// minPersistentInterval bounds how often an input can make the micom write
// its EEPROM.
const minPersistentInterval = time.Second

type endStep struct {
	msg string
	set map[string]inValue
}

// encode builds the frame from the values given; the rest take their
// default or invalid marker.
func (m *msgSpec) encode(vals map[string]inValue) (Frame, error) {
	p := slices.Clone(m.base)
	for _, f := range m.fields {
		v, ok := vals[f.name]
		switch {
		case ok:
		case f.def != nil:
			v = *f.def
		case f.hasInvalid:
			v = inValue{}
		default:
			return Frame{}, fmt.Errorf("%s: required", f.name)
		}
		if err := f.put(p, v); err != nil {
			return Frame{}, err
		}
	}
	return Frame{Hdr: m.hdr, Cmd: m.cmd, Payload: p}, nil
}

// parse checks an input's values field by field.
func (m *msgSpec) parse(in map[string]any) (map[string]inValue, error) {
	out := make(map[string]inValue, len(in))
	scratch := make([]byte, m.length)
	for _, k := range slices.Sorted(maps.Keys(in)) {
		f := m.byName[k]
		if f == nil {
			return nil, fmt.Errorf("%s: no such field", k)
		}
		v, err := toInValue(in[k])
		if err != nil {
			return nil, fmt.Errorf("%s: %v", k, err)
		}
		if err := f.put(scratch, v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// DecodeInput reads a frame to the micom back into the input that makes
// it: the message's name and its fields, nil where the frame carries the
// invalid marker. False for a frame no to_micom message describes.
func (v *Variant) DecodeInput(f Frame) (string, map[string]any, bool) {
	m := v.sent[hdrCmd{f.Hdr, f.Cmd}]
	if m == nil || len(f.Payload) != m.length {
		return "", nil, false
	}
	out := make(map[string]any, len(m.fields))
	for _, fs := range m.fields {
		out[fs.name] = fs.value(f.Payload)
	}
	return m.name, out, true
}

// validFieldName is [a-z][a-z0-9_]*: an input's JSON key.
func validFieldName(s string) bool {
	for i, r := range s {
		if !(r >= 'a' && r <= 'z' || i > 0 && (r >= '0' && r <= '9' || r == '_')) {
			return false
		}
	}
	return s != "" && s != "msg" && s != "end"
}

func compileEnums(c *checker, raw map[string]map[string]int64) map[string]map[string]int64 {
	for _, t := range sortedKeys(raw) {
		if !validFieldName(t) {
			c.errf("enums["+t+"]", "a table name is [a-z][a-z0-9_]*")
		}
		seen := map[int64]string{}
		for _, n := range sortedKeys(raw[t]) {
			code := raw[t][n]
			if !validFieldName(n) {
				c.errf("enums["+t+"]", "name %q is not [a-z][a-z0-9_]*", n)
			}
			if o, dup := seen[code]; dup {
				c.errf("enums["+t+"]", "%s and %s are both %d", o, n, code)
			}
			seen[code] = n
		}
	}
	return raw
}

// compileMessages builds the decoders of from_micom messages and the
// encoders of to_micom ones. Cross-references (end, owns) are checked by
// checkMessageRefs once the profile is known.
func compileMessages(c *checker, v *Variant, raws []rawMessage, enums map[string]map[string]int64,
	hdrOf func(string, string) ([4]byte, bool), sendable func(string, [4]byte, byte)) {
	names := map[string]bool{}
	for _, r := range raws {
		where := "messages[" + r.Name + "]"
		if r.Name == "" || names[r.Name] {
			c.errf(where, "a name is required, once")
		}
		names[r.Name] = true
		if r.Remove {
			c.errf(where, "remove: nothing inherited by that name")
			continue
		}
		switch r.Dir {
		case "from_micom":
			compileFromMicom(c, v, where, r, enums, hdrOf)
		case "to_micom":
			compileToMicom(c, v, where, r, enums, hdrOf, sendable)
		default:
			c.errf(where, "dir is from_micom or to_micom, not %q", r.Dir)
		}
	}
}

func compileFromMicom(c *checker, v *Variant, where string, r rawMessage, enums map[string]map[string]int64, hdrOf func(string, string) ([4]byte, bool)) {
	checkStatus(c, where, r.Status, false)
	if r.Fill != nil || r.Transport != nil || r.OnChange != nil || r.Every != 0 || r.Owns != "" || r.Timeout != 0 || len(r.End) > 0 || r.MinInterval != 0 || r.Persistent {
		c.errf(where, "fill, transport, on_change, every, owns, timeout, end, min_interval and persistent are for to_micom")
	}
	d := &decoder{name: r.Name}
	need := 0
	switch {
	case r.Length != nil && r.MinLength != nil:
		c.errf(where, "length or min_length, not both")
	case r.Length != nil:
		d.guards = append(d.guards, guard{length: *r.Length, hasLen: true})
		need = *r.Length
	case r.MinLength != nil:
		d.minLen, need = *r.MinLength, *r.MinLength
	}
	for _, g := range r.When {
		if g.Len != nil || g.Off == nil || g.Eq == nil {
			c.errf(where, "a guard is {off: N, eq: B} (the length is length:)")
			continue
		}
		if *g.Off < 0 || *g.Off >= need {
			c.errf(where, "guard off %d is beyond the %d bytes length or min_length promises", *g.Off, need)
			continue
		}
		d.guards = append(d.guards, guard{off: *g.Off, eq: *g.Eq})
	}
	if len(r.Fields) == 0 {
		c.errf(where, "fields are required")
	}
	for _, rf := range r.Fields {
		if f, ok := compileField(c, where, rf, enums, need, false); ok {
			d.fields = append(d.fields, f)
		}
	}
	// A decoder may leave header bytes open ("0c 88 ?? 8c"): it only reads.
	if hdr, mask, err := parseHdrMask(r.Hdr); err == nil {
		d.hdr, d.mask = hdr, mask
	} else if hdr, ok := hdrOf(where, r.Hdr); ok {
		d.hdr, d.mask = hdr, [4]byte{0xff, 0xff, 0xff, 0xff}
	} else {
		return
	}
	v.decoders[r.Cmd] = append(v.decoders[r.Cmd], d)
}

func compileToMicom(c *checker, v *Variant, where string, r rawMessage, enums map[string]map[string]int64,
	hdrOf func(string, string) ([4]byte, bool), sendable func(string, [4]byte, byte)) {
	if !validName(r.Name) {
		c.errf(where, "a to_micom name is [a-z0-9-]: it is the input's msg")
	}
	checkStatus(c, where, r.Status, true)
	if r.Ref == "" {
		c.errf(where, "ref is required")
	}
	if r.MinLength != nil || len(r.When) > 0 {
		c.errf(where, "min_length and when are for from_micom")
	}
	if t := r.Transport; t != nil {
		switch t.Kind {
		case "single":
		case "chunked":
			c.errf(where, "transport chunked needs the captured TP function byte: not implemented")
		default:
			c.errf(where, "transport is single or chunked, not %q", t.Kind)
		}
	}
	m := &msgSpec{name: r.Name, status: r.Status, cmd: r.Cmd, byName: map[string]*field{},
		onChange: r.OnChange == nil || *r.OnChange, every: r.Every, timeout: r.Timeout, owns: r.Owns,
		minInterval: r.MinInterval, persistent: r.Persistent}
	if r.Length == nil || *r.Length < 1 || *r.Length > MaxPayload {
		c.errf(where, "length is required, 1 to %d", MaxPayload)
		return
	}
	m.length = *r.Length
	switch {
	case r.Every < 0 || r.Timeout < 0 || r.MinInterval < 0:
		c.errf(where, "every, timeout and min_interval are positive")
	case (r.Every > 0 || r.Owns != "") && r.Timeout == 0:
		c.errf(where, "every and owns need timeout, so a client that stops cannot leave them running")
	}
	// Each frame wears the micom's EEPROM: only an input sends one, only
	// when it changes, and not more often than minPersistentInterval.
	switch {
	case !r.Persistent:
	case r.Every != 0 || r.Owns != "" || len(r.End) > 0:
		c.errf(where, "persistent: no every, owns or end (each frame writes the micom's EEPROM)")
	case !m.onChange:
		c.errf(where, "persistent: on_change only (each frame writes the micom's EEPROM)")
	case r.MinInterval < minPersistentInterval:
		c.errf(where, "persistent: min_interval of at least %v (each frame writes the micom's EEPROM)", minPersistentInterval)
	}
	if len(r.Fields) == 0 {
		c.errf(where, "fields are required")
	}
	used := make([]byte, m.length)
	for _, rf := range r.Fields {
		f, ok := compileField(c, where, rf, enums, m.length, true)
		if !ok {
			continue
		}
		if m.byName[f.name] != nil {
			c.errf(where+"."+f.name, "a field name is used once")
			continue
		}
		for _, k := range f.pos {
			if used[k/8]&(1<<(k%8)) != 0 {
				c.errf(where+"."+f.name, "overlaps another field at byte %d bit %d", k/8, k%8)
				break
			}
			used[k/8] |= 1 << (k % 8)
		}
		m.fields = append(m.fields, f)
		m.byName[f.name] = f
	}
	fill := byte(0)
	if r.Fill != nil {
		fill = *r.Fill
	}
	m.base = make([]byte, m.length)
	for i, u := range used {
		if u == 0 {
			m.base[i] = fill
		}
	}
	hdr, ok := hdrOf(where, r.Hdr)
	if !ok {
		return
	}
	m.hdr = hdr
	sendable(where, hdr, r.Cmd)
	hc := hdrCmd{hdr, r.Cmd}
	if o := v.sent[hc]; o != nil {
		c.errf(where, "message %s already sends %x cmd %#02x", o.name, hdr, r.Cmd)
		return
	}
	for _, s := range r.End {
		st := endStep{msg: s.Msg, set: map[string]inValue{}}
		for k, x := range s.Set {
			iv, err := toInValue(x)
			if err != nil {
				c.errf(where+".end["+s.Msg+"]", "%s: %v", k, err)
			}
			st.set[k] = iv
		}
		m.end = append(m.end, st)
	}
	v.sent[hc] = m
	v.messages[m.name] = m
}

// checkMessageRefs checks end steps and owns, once every message and the
// profile are compiled.
func checkMessageRefs(c *checker, v *Variant) {
	for _, n := range sortedKeys(v.messages) {
		m := v.messages[n]
		where := "messages[" + n + "]"
		for _, s := range m.end {
			t := v.messages[s.msg]
			if t == nil {
				c.errf(where+".end", "%q is not a to_micom message", s.msg)
				continue
			}
			if t.persistent {
				c.errf(where+".end", "%s is persistent: an end, sent unasked, may not write the micom's EEPROM", s.msg)
			}
			bad := false
			for _, k := range sortedKeys(s.set) {
				if t.byName[k] == nil {
					c.errf(where+".end["+s.msg+"]", "%s: no such field", k)
					bad = true
				}
			}
			if _, err := t.encode(s.set); err != nil && !bad {
				c.errf(where+".end["+s.msg+"]", "%v", err)
			}
		}
		if m.owns == "" {
			continue
		}
		i := slices.IndexFunc(v.profile, func(p periodicSpec) bool { return p.name == m.owns })
		switch {
		case i < 0 || v.profile[i].optIn != "":
			c.errf(where, "owns %q: no routine profile entry by that name", m.owns)
		case v.profile[i].hdr != m.hdr || v.profile[i].cmd != m.cmd:
			c.errf(where, "owns %q: a different frame (header and command must match)", m.owns)
		}
	}
}

// owned is the profile entries a message silences while active: the one it
// owns, and any opt_in code entry that stands in for it.
func (v *Variant) owned(m *msgSpec) []string {
	if m.owns == "" {
		return nil
	}
	out := []string{m.owns}
	for _, p := range v.profile {
		if p.replaces == m.owns {
			out = append(out, p.name)
		}
	}
	return out
}

// compileField places a field (off and type, maybe bits; or DBC's
// start_bit and bit_length) within need bytes and checks its codec.
func compileField(c *checker, where string, r rawMsgField, enums map[string]map[string]int64, need int, toMicom bool) (*field, bool) {
	w := where + "." + r.Name
	n := len(c.errs)
	f := &field{name: r.Name, unit: r.Unit, scale: 1, offset: r.Offset, enum: r.Enum, min: r.Min, max: r.Max}
	switch {
	case r.Name == "":
		c.errf(w, "a field needs a name")
	case toMicom && !validFieldName(r.Name):
		c.errf(w, "a to_micom field name is [a-z][a-z0-9_]* and not msg or end: it is the input's key")
	}
	little := false
	switch r.Endian {
	case "", "big":
	case "little":
		little = true
	default:
		c.errf(w, "endian is big or little, not %q", r.Endian)
	}
	switch {
	case r.Type == "str":
		f.typ = "str"
		if r.Off == nil || r.StartBit != nil {
			c.errf(w, "a str field is placed by off")
			return nil, false
		}
		f.off, f.width = *r.Off, r.Size
		if r.Size < 1 {
			c.errf(w, "a str field needs a size")
		}
		switch r.Encoding {
		case "", "ASCII":
		case "UTF-16LE":
			f.utf16 = true
			if r.Size%2 != 0 {
				c.errf(w, "a UTF-16LE field's size is even")
			}
		default:
			c.errf(w, "encoding is ASCII or UTF-16LE, not %q", r.Encoding)
		}
		if r.PadRight != nil {
			f.hasPad, f.pad = true, *r.PadRight
		}
		if r.Endian != "" || r.Bits != "" || r.BitLength != 0 || r.Signed || r.Scale != nil || r.Offset != 0 || r.Enum != "" || r.Invalid != nil || r.Min != nil || r.Max != nil || r.Unit != "" {
			c.errf(w, "a str field has only off, size, encoding, pad_right and default")
		}
		f.pos = bytePositions(f.off, f.width, false)
	case r.StartBit != nil:
		if r.Off != nil || r.Bits != "" {
			c.errf(w, "start_bit and bit_length place a field without off or bits")
		}
		switch r.Type {
		case "":
			f.typ, f.signed = "int", r.Signed
		case "bool":
			f.typ = "bool"
		default:
			c.errf(w, "a start_bit field's type is bool or absent (signed: true for a signed one)")
		}
		if r.BitLength < 1 || r.BitLength > 32 {
			c.errf(w, "bit_length is 1 to 32")
			return nil, false
		}
		f.pos = dbcPositions(*r.StartBit, r.BitLength, little)
	default:
		if r.Off == nil {
			c.errf(w, "a field is placed by off, or by start_bit and bit_length")
			return nil, false
		}
		typ := r.Type
		if typ == "" {
			typ = "u8"
		}
		width, ok := typeWidths[typ]
		if !ok {
			c.errf(w, "type is u8, u16, u32, i8, i16, i32, bool or str, not %q", typ)
			return nil, false
		}
		if r.Signed || r.BitLength != 0 {
			c.errf(w, "signed and bit_length are for start_bit fields (i8, i16, i32 are signed)")
		}
		f.typ, f.signed = "int", typ[0] == 'i'
		if typ == "bool" {
			f.typ = "bool"
		}
		if r.Bits != "" {
			hi, lo, err := parseBits(r.Bits)
			switch {
			case err != nil:
				c.errf(w, "%v", err)
			case width != 1:
				c.errf(w, "bits are within one byte: u8, i8 or bool")
			}
			for k := hi; k >= lo; k-- {
				f.pos = append(f.pos, 8**r.Off+k)
			}
		} else {
			f.pos = bytePositions(*r.Off, width, little)
		}
	}
	if f.typ != "str" && (r.Size != 0 || r.Encoding != "" || r.PadRight != nil) {
		c.errf(w, "size, encoding and pad_right are for str")
	}
	for _, k := range f.pos {
		if k < 0 || k/8 >= need {
			c.errf(w, "must lie within the %d bytes the message's length promises", need)
			return nil, false
		}
	}
	if r.Scale != nil {
		f.scale = *r.Scale
	}
	if (f.typ != "int" || f.enum != "") && (r.Scale != nil || r.Offset != 0 || r.Min != nil || r.Max != nil) {
		c.errf(w, "scale, offset, min and max are for numbers, not str, bool or enum")
	}
	if f.scale == 0 || math.IsNaN(f.scale) || math.IsInf(f.scale, 0) {
		c.errf(w, "scale is a finite number, not 0")
	}
	if r.Min != nil && r.Max != nil && *r.Min > *r.Max {
		c.errf(w, "min is above max")
	}
	if r.Invalid != nil && f.typ != "str" {
		if *r.Invalid >= 1<<f.nbits() {
			c.errf(w, "invalid %#x does not fit the field's %d bits", *r.Invalid, f.nbits())
		}
		f.hasInvalid, f.invalid = true, *r.Invalid
	}
	if f.enum != "" {
		t, ok := enums[f.enum]
		if !ok {
			c.errf(w, "no enum %q", f.enum)
		}
		if f.typ != "int" {
			c.errf(w, "an enum is an integer field")
		}
		f.codes, f.names = t, map[int64]string{}
		lo, hi := f.rawRange()
		for _, name := range sortedKeys(t) {
			code := t[name]
			if code < lo || code > hi || f.hasInvalid && uint64(code)&(1<<f.nbits()-1) == f.invalid {
				c.errf(w, "enum %s: %s (%d) does not fit the field or is its invalid marker", f.enum, name, code)
			}
			f.names[code] = name
		}
	}
	if !toMicom {
		// Field.Value is an int: 32 bits on arm.
		lo, hi := f.rawRange()
		a, b := float64(lo)*f.scale+f.offset, float64(hi)*f.scale+f.offset
		if f.typ != "str" && math.Max(math.Abs(a), math.Abs(b)) > math.MaxInt32 {
			c.errf(w, "a value from the micom must fit 32 bits (Field.Value is an int)")
		}
		if r.Default != nil || r.Min != nil || r.Max != nil {
			c.errf(w, "default, min and max are for to_micom")
		}
	}
	if len(c.errs) > n {
		return nil, false
	}
	if r.Default != nil {
		d, err := toInValue(r.Default)
		if err == nil {
			err = f.put(make([]byte, need), d)
		}
		if err != nil {
			c.errf(w, "default: %v", err)
			return nil, false
		}
		f.def = &d
	}
	return f, true
}
