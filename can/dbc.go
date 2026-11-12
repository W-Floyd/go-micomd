package can

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// DBC is the part of a DBC file that names frames and their signals: messages
// (BO_), signals (SG_) and value tables (VAL_). Several files can be merged
// into one with Add.
type DBC struct {
	Messages []*Message
	byID     map[uint32]*Message
	byName   map[string]*Message
}

// Message is a DBC message (BO_). ID carries no flag bits; Ext marks a 29-bit ID.
type Message struct {
	ID      uint32
	Ext     bool
	Name    string
	DLC     int
	Sender  string
	Signals []*Signal
}

// Signal is a DBC signal (SG_): physical = raw*Factor + Offset.
type Signal struct {
	Name string
	// Start is the DBC start bit: the LSB for little-endian (Intel, @1)
	// signals, the MSB for big-endian (Motorola, @0) ones, in DBC numbering.
	Start, Size    int
	LittleEndian   bool
	Signed         bool
	Factor, Offset float64
	Min, Max       float64
	Unit           string
	// Multiplexer marks the multiplexor switch (M); Mux, when non-nil, the
	// switch value this signal is present for (mN).
	Multiplexer bool
	Mux         *int
	Values      map[int64]string // VAL_
}

var (
	reBO  = regexp.MustCompile(`^BO_\s+(\d+)\s+(\w+)\s*:\s*(\d+)\s+(\S+)`)
	reSG  = regexp.MustCompile(`^SG_\s+(\w+)\s*(M|m\d+)?\s*:\s*(\d+)\|(\d+)@([01])([+-])\s*\(([^,]+),([^)]+)\)\s*\[([^|]*)\|([^\]]*)\]\s*"([^"]*)"`)
	reVAL = regexp.MustCompile(`^VAL_\s+(\d+)\s+(\w+)\s+(.*);`)
	reNV  = regexp.MustCompile(`(-?\d+)\s+"([^"]*)"`)
)

// LoadDBC reads DBC files into one DBC; a later file's message replaces an
// earlier one with the same ID.
func LoadDBC(paths ...string) (*DBC, error) {
	d := &DBC{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		g, err := ParseDBC(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		d.Add(g)
	}
	return d, nil
}

// Add merges o's messages into d, o's winning on an ID or name both have.
func (d *DBC) Add(o *DBC) {
	for _, m := range o.Messages {
		if old := d.byID[m.ID]; old != nil {
			d.Messages = slices.DeleteFunc(d.Messages, func(x *Message) bool { return x == old })
			delete(d.byName, old.Name)
		}
		d.add(m)
	}
}

func (d *DBC) add(m *Message) {
	if d.byID == nil {
		d.byID, d.byName = map[uint32]*Message{}, map[string]*Message{}
	}
	d.Messages = append(d.Messages, m)
	d.byID[m.ID] = m
	d.byName[m.Name] = m
}

// ParseDBC reads one DBC file. Lines it does not use (BA_, CM_, ...) are skipped.
func ParseDBC(r io.Reader) (*DBC, error) {
	d := &DBC{}
	var cur *Message
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "BO_ "):
			m := reBO.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("line %d: bad BO_", n)
			}
			id, _ := strconv.ParseUint(m[1], 10, 32)
			dlc, _ := strconv.Atoi(m[3])
			cur = &Message{ID: uint32(id) & 0x1fffffff, Ext: id&0x80000000 != 0, Name: m[2], DLC: dlc, Sender: m[4]}
			d.add(cur)
		case strings.HasPrefix(line, "SG_ "):
			m := reSG.FindStringSubmatch(line)
			if m == nil || cur == nil {
				return nil, fmt.Errorf("line %d: bad SG_ (or one outside a BO_)", n)
			}
			s := &Signal{Name: m[1], LittleEndian: m[5] == "1", Signed: m[6] == "-", Unit: m[11]}
			s.Start, _ = strconv.Atoi(m[3])
			s.Size, _ = strconv.Atoi(m[4])
			var err error
			for _, f := range []struct {
				dst *float64
				src string
			}{{&s.Factor, m[7]}, {&s.Offset, m[8]}, {&s.Min, m[9]}, {&s.Max, m[10]}} {
				if *f.dst, err = strconv.ParseFloat(strings.TrimSpace(f.src), 64); err != nil {
					return nil, fmt.Errorf("line %d: %v", n, err)
				}
			}
			switch {
			case m[2] == "M":
				s.Multiplexer = true
			case m[2] != "":
				v, _ := strconv.Atoi(m[2][1:])
				s.Mux = &v
			}
			if s.Size < 1 || s.Size > 64 {
				return nil, fmt.Errorf("line %d: signal %s is %d bits", n, s.Name, s.Size)
			}
			cur.Signals = append(cur.Signals, s)
		case strings.HasPrefix(line, "VAL_ "):
			m := reVAL.FindStringSubmatch(line)
			if m == nil {
				continue // VAL_ for an environment variable, or malformed: names only
			}
			id, _ := strconv.ParseUint(m[1], 10, 32)
			msg := d.byID[uint32(id)&0x1fffffff]
			if msg == nil {
				continue
			}
			if s := msg.Signal(m[2]); s != nil {
				s.Values = map[int64]string{}
				for _, nv := range reNV.FindAllStringSubmatch(m[3], -1) {
					v, _ := strconv.ParseInt(nv[1], 10, 64)
					s.Values[v] = nv[2]
				}
			}
		case line == "":
			cur = nil
		}
	}
	return d, sc.Err()
}

// ByID is the message with this ID, or nil.
func (d *DBC) ByID(id uint32) *Message { return d.byID[id] }

// ByName is the message with this name, or nil.
func (d *DBC) ByName(name string) *Message { return d.byName[name] }

// Signal is the message's signal of this name, or nil.
func (m *Message) Signal(name string) *Signal {
	for _, s := range m.Signals {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// bits yields the signal's bit positions (byte*8 + bit, bit 0 the LSB of its
// byte), from the signal's MSB down to its LSB.
func (s *Signal) bits() []int {
	out := make([]int, s.Size)
	if s.LittleEndian {
		for i := range s.Size {
			out[s.Size-1-i] = s.Start + i
		}
		return out
	}
	// Motorola: Start is the MSB; walk down within the byte, then on to the
	// next byte's bit 7
	p := s.Start
	for i := range s.Size {
		out[i] = p
		if p%8 == 0 {
			p += 15
		} else {
			p--
		}
	}
	return out
}

// Raw reads the signal's raw value from data (sign-extended when Signed).
func (s *Signal) Raw(data []byte) (int64, bool) {
	var v uint64
	for _, p := range s.bits() {
		if p/8 >= len(data) {
			return 0, false
		}
		v = v<<1 | uint64(data[p/8]>>(p%8)&1)
	}
	if s.Signed && s.Size < 64 && v&(1<<(s.Size-1)) != 0 {
		v |= ^uint64(0) << s.Size
	}
	return int64(v), true
}

// Phys is the signal's physical value in data.
func (s *Signal) Phys(data []byte) (float64, bool) {
	r, ok := s.Raw(data)
	return float64(r)*s.Factor + s.Offset, ok
}

// SetRaw writes a raw value into data, which must be long enough.
func (s *Signal) SetRaw(data []byte, raw int64) error {
	if s.Size < 64 {
		lo, hi := int64(0), int64(1)<<s.Size-1
		if s.Signed {
			lo, hi = -(int64(1) << (s.Size - 1)), int64(1)<<(s.Size-1)-1
		}
		if raw < lo || raw > hi {
			return fmt.Errorf("%s: raw %d does not fit %d bits", s.Name, raw, s.Size)
		}
	}
	bits := s.bits()
	for i, p := range bits {
		if p/8 >= len(data) {
			return fmt.Errorf("%s: bit %d past the %d-byte frame", s.Name, p, len(data))
		}
		b := byte(uint64(raw)>>(len(bits)-1-i)) & 1
		data[p/8] = data[p/8]&^(1<<(p%8)) | b<<(p%8)
	}
	return nil
}

// SetValue writes a physical value, or a VAL_ name, into data.
func (s *Signal) SetValue(data []byte, v string) error {
	for raw, name := range s.Values {
		if name == v {
			return s.SetRaw(data, raw)
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fmt.Errorf("%s: %q is neither a number nor one of its values", s.Name, v)
	}
	factor := s.Factor
	if factor == 0 {
		factor = 1
	}
	return s.SetRaw(data, int64(math.Round((f-s.Offset)/factor)))
}

// Frame builds a frame of this message on bus from base (zeros when nil,
// padded to DLC) with the signals in vals (name=value, value physical or a
// VAL_ name) set.
func (m *Message) Frame(bus int, base []byte, vals map[string]string) (Frame, error) {
	data := make([]byte, m.DLC)
	copy(data, base)
	for name, v := range vals {
		s := m.Signal(name)
		if s == nil {
			return Frame{}, fmt.Errorf("%s has no signal %s", m.Name, name)
		}
		if err := s.SetValue(data, v); err != nil {
			return Frame{}, err
		}
	}
	return Frame{Bus: bus, ID: m.ID, Ext: m.Ext, Data: data}, nil
}

// Describe names a frame's message and its signals' values ("FATC11
// CF_Fatc_DefSw=1 ..."): signals of another multiplexer value are left out.
// It is "" for an ID d has no message for.
func (d *DBC) Describe(f Frame) string {
	m := d.ByID(f.ID)
	if m == nil || m.Ext != f.Ext {
		return ""
	}
	var mux *int64
	for _, s := range m.Signals {
		if s.Multiplexer {
			if r, ok := s.Raw(f.Data); ok {
				mux = &r
			}
		}
	}
	parts := []string{m.Name}
	for _, s := range m.Signals {
		if s.Mux != nil && (mux == nil || int64(*s.Mux) != *mux) {
			continue
		}
		r, ok := s.Raw(f.Data)
		if !ok {
			continue
		}
		v := strconv.FormatFloat(float64(r)*s.Factor+s.Offset, 'g', -1, 64)
		if name, ok := s.Values[r]; ok {
			v = strconv.Quote(name)
		}
		parts = append(parts, s.Name+"="+v)
	}
	return strings.Join(parts, " ")
}

// ParseSignals reads a frame given by name, "[canN] MESSAGE SIG=VALUE ...",
// with the signals not given zero. Values are physical or VAL_ names.
func (d *DBC) ParseSignals(s string) (Frame, error) {
	f := strings.Fields(s)
	bus := 0
	if len(f) > 0 && strings.HasPrefix(f[0], "can") {
		b, err := strconv.Atoi(strings.TrimPrefix(f[0], "can"))
		if err != nil || b < 0 || b > 2 {
			return Frame{}, fmt.Errorf("%q: bad bus", s)
		}
		bus, f = b, f[1:]
	}
	if len(f) == 0 {
		return Frame{}, fmt.Errorf("%q: no message name", s)
	}
	m := d.ByName(f[0])
	if m == nil {
		return Frame{}, fmt.Errorf("no message %s in the DBC", f[0])
	}
	vals := map[string]string{}
	for _, kv := range f[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return Frame{}, fmt.Errorf("%q: want SIGNAL=VALUE", kv)
		}
		vals[k] = v
	}
	return m.Frame(bus, nil, vals)
}
