package micom

import (
	"fmt"
	"strings"
)

// A signal is a one-byte state frame of a variant: how it reads as a line of
// text, and which CarClimate field, if any, it sets. The kinds are fixed here;
// a variant file only chooses one and gives its values.
type signalKind int

const (
	kindFlag signalKind = iota + 1
	kindEnum
	kindNumber
	kindFan
	kindBits
	kindBurst
)

type sigSpec struct {
	name, label string
	length      int
	kind        signalKind

	on, off    byte // flag
	offAny     bool
	onText     string
	offText    string
	enum       map[byte]string // enum
	add, mul   int             // number
	ranged     bool
	wrap       bool
	lo, hi     byte
	format     string
	fanOff     byte // fan
	fanAuto    byte
	fanLo      byte
	fanHi      byte
	always     byte // bits
	none       string
	bitNames   map[byte]string
	begin      byte // burst
	values     map[byte]valueSpec
	lines      map[byte]string
	unknown    string // "" with quiet: not described
	quiet      bool
	unknownLin string
	suffix     string
	state      string
}

// describe is the line for value v, "" when there is none.
func (s *sigSpec) describe(v byte) string {
	if l, ok := s.lines[v]; ok {
		return l
	}
	text, ok := s.text(v)
	if !ok {
		switch {
		case s.unknownLin != "":
			return format1(s.unknownLin, v)
		case s.quiet:
			return ""
		}
		text = format1(s.unknown, v)
	}
	return s.label + ": " + text + s.suffix
}

func (s *sigSpec) text(v byte) (string, bool) {
	if vs, ok := s.values[v]; ok {
		return vs.Text, true
	}
	switch s.kind {
	case kindFlag:
		switch {
		case v == s.on:
			return s.onText, true
		case s.offAny || v == s.off:
			return s.offText, true
		}
	case kindEnum:
		t, ok := s.enum[v]
		return t, ok
	case kindNumber:
		if n, ok := s.number(v); ok {
			return format1(s.format, n), true
		}
	case kindFan:
		switch {
		case v == s.fanOff:
			return "off", true
		case v == s.fanAuto:
			return "auto", true
		case v >= s.fanLo && v <= s.fanHi:
			return fmt.Sprintf("level %d", int(v-s.fanLo)+1), true
		}
	case kindBits:
		var set []string
		rest := v &^ s.always
		for bit := byte(0); bit < 8; bit++ {
			if n, ok := s.bitNames[bit]; ok && v&(1<<bit) != 0 {
				set = append(set, n)
				rest &^= 1 << bit
			}
		}
		t := s.none
		if len(set) > 0 {
			t = strings.Join(set, ", ")
		}
		if rest != 0 {
			t += fmt.Sprintf(" (other bits %#02x)", rest)
		}
		return t, true
	}
	return "", false
}

// number is a number signal's value for v, false outside its range.
func (s *sigSpec) number(v byte) (int, bool) {
	if s.ranged && (v < s.lo || v > s.hi) {
		return 0, false
	}
	n := int(v)*s.mul + s.add
	if s.wrap {
		n = int(byte(n))
	}
	return n, true
}

// format1 applies a format checked by checkFormat: no verb, or one %d or %#02x.
func format1(f string, v any) string {
	if !strings.Contains(f, "%") {
		return f
	}
	return fmt.Sprintf(f, v)
}

// checkFormat allows at most one verb, %d or %#02x, and no other %.
func checkFormat(f string) error {
	rest := strings.NewReplacer("%#02x", "", "%d", "").Replace(f)
	if strings.Contains(rest, "%") {
		return fmt.Errorf("format %q: only %%d or %%#02x may be used", f)
	}
	if strings.Count(f, "%d")+strings.Count(f, "%#02x") > 1 {
		return fmt.Errorf("format %q: at most one verb", f)
	}
	return nil
}

// apply folds v into c.
func (s *sigSpec) apply(c *CarClimate, v byte) ClimateEvent {
	if s.kind == kindBurst {
		if v == s.begin {
			return ClimateBegin
		}
		return ClimateEnd
	}
	switch s.state {
	case "driver_set_f", "passenger_set_f", "driver_seat_heat", "passenger_seat_heat":
		field := c.intField(s.state)
		if vs, ok := s.values[v]; ok {
			if vs.State == nil {
				return ClimateNone
			}
			return set(field, *vs.State)
		}
		if n, ok := s.number(v); ok {
			return set(field, n)
		}
	case "airflow":
		if _, ok := s.enum[v]; ok {
			return set(&c.Airflow, v)
		}
	case "auto", "ac", "driver_only", "recirculation", "zones_synced":
		field := c.boolField(s.state)
		switch {
		case v == s.on:
			return set(field, true)
		case s.offAny || v == s.off:
			return set(field, false)
		}
	case "fan":
		switch {
		case v == s.fanAuto:
			return set(&c.FanAuto, true)
		case v == s.fanOff:
			return max(set(&c.FanLevel, 0), set(&c.FanAuto, false))
		case v >= s.fanLo && v <= s.fanHi:
			return max(set(&c.FanLevel, int(v-s.fanLo)+1), set(&c.FanAuto, false))
		}
	}
	return ClimateNone
}

// stateKinds are the CarClimate fields a signal may set, and the kind each
// needs.
var stateKinds = map[string]signalKind{
	"driver_set_f": kindNumber, "passenger_set_f": kindNumber,
	"driver_seat_heat": kindNumber, "passenger_seat_heat": kindNumber,
	"airflow": kindEnum,
	"auto":    kindFlag, "ac": kindFlag, "driver_only": kindFlag,
	"recirculation": kindFlag, "zones_synced": kindFlag,
	"fan": kindFan,
}

func (c *CarClimate) intField(name string) **int {
	switch name {
	case "driver_set_f":
		return &c.DriverSetF
	case "passenger_set_f":
		return &c.PassengerSetF
	case "driver_seat_heat":
		return &c.DriverSeatHeat
	}
	return &c.PassengerSeatHeat
}

func (c *CarClimate) boolField(name string) **bool {
	switch name {
	case "auto":
		return &c.Auto
	case "ac":
		return &c.AC
	case "driver_only":
		return &c.DriverOnly
	case "recirculation":
		return &c.Recirculation
	}
	return &c.ZonesSynced
}
