package micom

import (
	"fmt"
	"strings"
)

// Field is one value read out of a frame.
type Field struct {
	Name string `json:"name"`
	// Value is the number, or Valid false when the frame carried its "not
	// available" marker (0xff / 0xffff).
	Value int    `json:"value"`
	Valid bool   `json:"valid"`
	Unit  string `json:"unit"`
}

func (f Field) String() string {
	if !f.Valid {
		return f.Name + "=n/a"
	}
	return fmt.Sprintf("%s=%d%s", f.Name, f.Value, f.Unit)
}

// Func is the Android-side function code of a frame from the micom: its
// header's fourth byte and its command.
func Func(f Frame) uint16 { return uint16(f.Hdr[3])<<8 | uint16(f.Cmd) }

// FrameName names the frames whose meaning is known, "" for the rest.
func FrameName(f Frame) string {
	if IsAliveRequest(f) {
		return "CPU alive request"
	}
	if f.Hdr[0] != 0x0c || f.Hdr[1] != 0x88 {
		return ""
	}
	switch Func(f) {
	case 0x8c02:
		return "Status flag"
	case 0x8c5c:
		return "EV power block"
	case 0x8c55:
		return "Departure timers"
	case 0x8c4f:
		return "Charger state"
	}
	return ""
}

// Decode reads the fields of a frame whose layout is known; nil for the rest.
// The layouts are Android's HEVService (func = hdr[3]<<8 | cmd, payload
// offsets as its handlers index them, integers big endian), checked against
// live traffic only where noted.
func Decode(f Frame) []Field {
	if f.Hdr[0] != 0x0c || f.Hdr[1] != 0x88 {
		return nil
	}
	p := f.Payload
	switch Func(f) {
	case 0x8c02:
		// Status flag setter: id, then a 16-bit value. Only id 0x57 (the
		// HEVFlag 0x4f, climate power draw) has been seen.
		if len(p) == 3 && p[0] == 0x57 {
			return []Field{{Name: "Climate power", Value: be16(p[1:]), Valid: true, Unit: " W"}}
		}
	case 0x8c5c:
		if len(p) >= 29 {
			out := make([]Field, 0, len(powerFields))
			for _, pf := range powerFields {
				out = append(out, pf.read(p))
			}
			return out
		}
	case 0x8c55:
		if len(p) >= 5 {
			return []Field{
				{Name: "Departure 1 hour", Value: int(p[3] & 0x1f), Valid: true},
				{Name: "Departure 1 minute bits", Value: int(p[3] >> 5), Valid: true},
				{Name: "Departure 2 hour", Value: int(p[4] & 0x1f), Valid: true},
				{Name: "Departure 2 minute bits", Value: int(p[4] >> 5), Valid: true},
			}
		}
	case 0x8c4f:
		if len(p) >= 4 {
			return []Field{{Name: "Charger ready bits", Value: int(p[3] >> 6), Valid: true}}
		}
	}
	return nil
}

// Describe is a one-line reading of f for the frames whose layout is known,
// "" for the rest.
func Describe(f Frame) string {
	if s := carDescribe(f); s != "" {
		return s
	}
	fs := Decode(f)
	if fs == nil {
		return ""
	}
	parts := make([]string, len(fs))
	for i, x := range fs {
		parts[i] = x.String()
	}
	return FrameName(f) + ": " + strings.Join(parts, " ")
}

func be16(b []byte) int { return int(b[0])<<8 | int(b[1]) }

type powerField struct {
	name, unit string
	off, n     int
}

// The 0x8c5c block, from HEVService's setHEVStatusValue calls. The units are
// the HEVFlag names' (_W, _WH, KW); the scale of the rest is not known.
var powerFields = []powerField{
	{"Other ECUs", "", 0, 1}, {"AVN motor power", "", 1, 2}, {"E-compressor", "", 3, 2},
	{"PTC heater", "", 8, 2}, {"Climate total", " W", 10, 2},
	{"Battery heater", " W", 12, 1}, {"Battery heater total", " Wh", 13, 1},
	{"MG1 power", " kW", 14, 2}, {"MG2 power", " kW", 16, 2},
	{"MG1 energy", " Wh", 18, 2}, {"MG2 energy", " Wh", 20, 2},
	{"LDC load", "", 22, 1}, {"Climate power", " W", 23, 2},
	{"Battery compressor", " W", 25, 2}, {"Battery energy", " Wh", 27, 2},
}

func (pf powerField) read(p []byte) Field {
	f := Field{Name: pf.name, Unit: pf.unit}
	if pf.n == 1 {
		f.Value, f.Valid = int(p[pf.off]), p[pf.off] != 0xff
		return f
	}
	f.Value = be16(p[pf.off:])
	f.Valid = f.Value != 0xffff
	return f
}
