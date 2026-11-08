package micom

import (
	"fmt"
	"strings"
)

// The Go code the EV6 variant replaced, kept unchanged but renamed (legacy*),
// so the equivalence tests (variant_ev6_test.go) keep checking the variant
// against it.

func legacyCarDescribe(f Frame) string {
	h, p := f.Hdr, f.Payload
	switch {
	case h == [4]byte{0x07, 0x84, 0x01, 0x87} && f.Cmd == 0x01:
		return legacyDescribeKey(p)
	case h == [4]byte{0x43, 0xc2, 0x00, 0xc3}:
		return legacyDescribeClimate(f.Cmd, p)
	case h == [4]byte{0x01, 0xc4, 0x01, 0x81} && f.Cmd == 0x51 && len(p) == 1:
		switch p[0] {
		case 0:
			return "Rear camera: off"
		case 9:
			return "Rear camera: on"
		}
		return fmt.Sprintf("Rear camera mode: code %#02x", p[0])
	case h == [4]byte{0x55, 0xca, 0x00, 0xd5}:
		if s := legacyDescribeWindow(f.Cmd, 0x08, 0x0b, 0x06, 0x09, p); s != "" {
			return s
		}
		switch f.Cmd {
		case 0x20:
			return "Seat heat, driver: " + legacySeatHeat(p)
		case 0x21:
			return "Seat heat, passenger: " + legacySeatHeat(p)
		}
	case h == [4]byte{0x01, 0x8a, 0x01, 0x81} && f.Cmd == 0x2f && len(p) == 1:
		return legacyDescribeOpen(p[0])
	case h == [4]byte{0x44, 0xc1, 0x00, 0xc4}:
		if s := legacyDescribeDoor(f.Cmd, p); s != "" {
			return s
		}
		if s := legacyDescribeWindow(f.Cmd, 0x26, 0x27, 0x24, 0x25, p); s != "" {
			return s + " (repeat)"
		}
		switch f.Cmd {
		case 0x19:
			return "Seat heat, driver (repeat): " + legacySeatHeat(p)
		case 0x1b:
			return "Seat heat, passenger (repeat): " + legacySeatHeat(p)
		}
	}
	return ""
}

var legacyDoors = []struct {
	name       string
	lock, open byte
}{
	{"front right", 0x00, 0x01}, {"driver", 0x02, 0x03},
	{"rear left", 0x04, 0x05}, {"rear right", 0x07, 0x08},
}

func legacyDescribeDoor(cmd byte, p []byte) string {
	if len(p) != 1 || p[0] > 1 {
		return ""
	}
	switch cmd {
	case 0x06, 0x11:
		name := "Trunk"
		if cmd == 0x11 {
			name = "Hood"
		}
		if p[0] == 1 {
			return name + ": open"
		}
		return name + ": closed"
	}
	for _, d := range legacyDoors {
		switch cmd {
		case d.lock:
			if p[0] == 0 {
				return "Door " + d.name + ": locked"
			}
			return "Door " + d.name + ": unlocked"
		case d.open:
			if p[0] == 1 {
				return "Door " + d.name + ": open"
			}
			return "Door " + d.name + ": closed"
		}
	}
	return ""
}

func legacyDescribeWindow(cmd, drvSw, pasSw, drvPos, pasPos byte, p []byte) string {
	if len(p) != 1 {
		return ""
	}
	var who string
	var sw bool
	switch cmd {
	case drvSw:
		who, sw = "driver", true
	case pasSw:
		who, sw = "passenger", true
	case drvPos:
		who = "driver"
	case pasPos:
		who = "passenger"
	default:
		return ""
	}
	v := p[0]
	if !sw {
		if v > 3 {
			return fmt.Sprintf("Window %s: position code %#02x", who, v)
		}
		s := fmt.Sprintf("Window %s: position %d of 3", who, v)
		switch v {
		case 0:
			s += " (closed)"
		case 3:
			s += " (fully down)"
		}
		return s
	}
	switch v {
	case 0:
		return "Window " + who + ": switch released"
	case 1:
		return "Window " + who + ": switch down"
	case 2:
		return "Window " + who + ": switch up"
	case 5:
		return "Window " + who + ": switch hard down (auto)"
	case 6:
		return "Window " + who + ": switch hard up (auto)"
	}
	return fmt.Sprintf("Window %s: switch code %#02x", who, v)
}

func legacyDescribeOpen(v byte) string {
	names := []struct {
		bit  uint
		name string
	}{{1, "trunk"}, {2, "front right"}, {3, "driver"}, {4, "rear right"}, {5, "rear left"}}
	var open []string
	rest := v &^ 1
	for _, n := range names {
		if v&(1<<n.bit) != 0 {
			open = append(open, n.name)
			rest &^= 1 << n.bit
		}
	}
	s := "Open doors: none"
	if len(open) > 0 {
		s = "Open doors: " + strings.Join(open, ", ")
	}
	if rest != 0 {
		s += fmt.Sprintf(" (other bits %#02x)", rest)
	}
	return s
}

var legacyKeyLabels = map[[2]byte]string{
	{0, 1}: "Volume knob", {0, 2}: "Seek <", {0, 3}: "Track >",
	{0, 4}: "Map", {0, 5}: "Nav",
	{1, 1}: "Tune knob", {1, 2}: "Radio", {1, 3}: "Media",
	{1, 4}: "Star", {1, 5}: "Setup",
	{2, 2}: "Wheel up", {2, 3}: "Wheel down", {2, 4}: "Wheel mode",
	{2, 5}: "Wheel volume push", {2, 6}: "Wheel volume up", {2, 7}: "Wheel volume down",
	{2, 8}: "Wheel voice", {2, 10}: "Wheel call", {2, 11}: "Wheel star",
}

func legacyDescribeKey(p []byte) string {
	if len(p) < 2 {
		return ""
	}
	typ, code, hi := p[0], p[1]&0xf, p[1]>>4
	if typ == 8 || typ == 9 {
		name := "Tune knob"
		if typ == 9 {
			name = "Volume knob"
		}
		switch hi {
		case 1:
			return name + " up"
		case 0:
			return name + " down"
		}
		return fmt.Sprintf("%s turn %d code %d", name, hi, code)
	}
	label, ok := legacyKeyLabels[[2]byte{typ, code}]
	if !ok {
		label = fmt.Sprintf("Key %d.%d", typ, code)
	}
	press := fmt.Sprintf("press %d", hi)
	switch hi {
	case 0:
		press = "release"
	case 1:
		press = "press"
	case 2:
		press = "long press"
	}
	return label + " " + press
}

func legacySeatHeat(p []byte) string {
	if len(p) < 1 {
		return ""
	}
	switch v := p[0]; {
	case v == 2:
		return "off"
	case v >= 6 && v <= 8:
		return fmt.Sprintf("heat level %d", v-5)
	default:
		return fmt.Sprintf("code %#02x", v)
	}
}

var legacyAirflow = map[byte]string{
	0: "off", 1: "face", 2: "face and feet", 3: "feet", 4: "feet and defrost",
	5: "defrost", 8: "auto",
}

func legacyOnOff(v byte, on, off byte) string {
	switch v {
	case on:
		return "on"
	case off:
		return "off"
	}
	return fmt.Sprintf("code %#02x", v)
}

func legacySetPoint(v byte) string {
	if v == 0 {
		return "off"
	}
	return fmt.Sprintf("%d F", legacySetPointF(v))
}

func legacyDescribeClimate(cmd byte, p []byte) string {
	if len(p) != 1 {
		return ""
	}
	v := p[0]
	switch cmd {
	case 0x05, 0x07:
		return "Climate driver set point: " + legacySetPoint(v)
	case 0x09, 0x0b:
		return "Climate passenger set point: " + legacySetPoint(v)
	case 0x0d:
		if s, ok := legacyAirflow[v]; ok {
			return "Climate airflow: " + s
		}
		return fmt.Sprintf("Climate airflow: code %#02x", v)
	case 0x0f:
		return "Climate auto: " + legacyOnOff(v, 1, 0)
	case 0x11:
		return "Climate A/C: " + legacyOnOff(v, 1, 0)
	case 0x15:
		switch {
		case v == 0:
			return "Climate fan: off"
		case v == 0x0a:
			return "Climate fan: auto"
		case v >= 2 && v <= 9:
			return fmt.Sprintf("Climate fan: level %d", v-1)
		}
		return fmt.Sprintf("Climate fan: code %#02x", v)
	case 0x19:
		return "Climate driver only: " + legacyOnOff(v, 1, 0)
	case 0x1b:
		if v == 1 {
			return "Climate update begins"
		}
		return "Climate update ends"
	case 0x1e, 0x43:
		return "Climate recirculation: " + legacyOnOff(v, 1, 2)
	case 0x27:
		if v == 1 {
			return "Climate zones: synced"
		}
		return "Climate zones: independent"
	case 0x33:
		return "Climate heat: " + legacyOnOff(v, 1, 0)
	case 0x4f:
		if v == 1 {
			return "Climate auto level: off"
		}
		return fmt.Sprintf("Climate auto level: %d", v-1)
	case 0x50:
		if v == 1 {
			return "Climate auto level (inverse): off"
		}
		return fmt.Sprintf("Climate auto level (inverse): %d", 5-int(v))
	}
	return ""
}

// climate.go's Apply.

var (
	legacyHdrClimatePanel = [4]byte{0x43, 0xc2, 0x00, 0xc3}
	legacyHdrSeatHeat     = [4]byte{0x55, 0xca, 0x00, 0xd5}
	legacyHdrSeatRepeat   = [4]byte{0x44, 0xc1, 0x00, 0xc4}
)

func legacyApply(c *CarClimate, f Frame) ClimateEvent {
	if len(f.Payload) != 1 {
		return ClimateNone
	}
	v := f.Payload[0]
	switch f.Hdr {
	case legacyHdrSeatHeat:
		switch f.Cmd {
		case 0x20:
			return legacySetSeat(&c.DriverSeatHeat, v)
		case 0x21:
			return legacySetSeat(&c.PassengerSeatHeat, v)
		}
	case legacyHdrSeatRepeat:
		switch f.Cmd {
		case 0x19:
			return legacySetSeat(&c.DriverSeatHeat, v)
		case 0x1b:
			return legacySetSeat(&c.PassengerSeatHeat, v)
		}
	case legacyHdrClimatePanel:
		return legacyApplyPanel(c, f.Cmd, v)
	}
	return ClimateNone
}

func legacyApplyPanel(c *CarClimate, cmd, v byte) ClimateEvent {
	flag := func(field **bool, on, off byte) ClimateEvent {
		switch v {
		case on:
			return set(field, true)
		case off:
			return set(field, false)
		}
		return ClimateNone
	}
	switch cmd {
	case 0x1b:
		if v == 1 {
			return ClimateBegin
		}
		return ClimateEnd
	case 0x05, 0x07:
		return set(&c.DriverSetF, legacySetPointF(v))
	case 0x09, 0x0b:
		return set(&c.PassengerSetF, legacySetPointF(v))
	case 0x0d:
		if _, ok := legacyAirflow[v]; ok {
			return set(&c.Airflow, v)
		}
	case 0x0f:
		return flag(&c.Auto, 1, 0)
	case 0x11:
		return flag(&c.AC, 1, 0)
	case 0x15:
		switch {
		case v == 0x0a:
			return set(&c.FanAuto, true)
		case v == 0:
			return max(set(&c.FanLevel, 0), set(&c.FanAuto, false))
		case v >= 2 && v <= 9:
			return max(set(&c.FanLevel, int(v)-1), set(&c.FanAuto, false))
		}
	case 0x19:
		return flag(&c.DriverOnly, 1, 0)
	case 0x1e, 0x43:
		return flag(&c.Recirculation, 1, 2)
	case 0x27:
		return set(&c.ZonesSynced, v == 1)
	}
	return ClimateNone
}

func legacySetPointF(v byte) int {
	if v == 0 {
		return 0
	}
	return int(v) + 56
}

func legacySetSeat(field **int, v byte) ClimateEvent {
	switch {
	case v == 2:
		return set(field, 0)
	case v >= 6 && v <= 8:
		return set(field, int(v)-5)
	}
	return ClimateNone
}

// decode.go's FrameName, Decode and Describe.

func legacyFrameName(f Frame) string {
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

func legacyDecode(f Frame) []Field {
	if f.Hdr[0] != 0x0c || f.Hdr[1] != 0x88 {
		return nil
	}
	p := f.Payload
	switch Func(f) {
	case 0x8c02:
		if len(p) == 3 && p[0] == 0x57 {
			return []Field{{Name: "Climate power draw", Value: be16(p[1:]), Valid: true, Unit: " W"}}
		}
	case 0x8c5c:
		if len(p) >= 29 {
			out := make([]Field, 0, len(legacyPowerFields))
			for _, pf := range legacyPowerFields {
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

func legacyDescribe(f Frame) string {
	if s := legacyCarDescribe(f); s != "" {
		return s
	}
	fs := legacyDecode(f)
	if fs == nil {
		return ""
	}
	parts := make([]string, len(fs))
	for i, x := range fs {
		parts[i] = x.String()
	}
	return legacyFrameName(f) + ": " + strings.Join(parts, " ")
}

type legacyPowerField struct {
	name, unit string
	off, n     int
}

var legacyPowerFields = []legacyPowerField{
	{"Other ECUs", "", 0, 1}, {"AVN motor power", "", 1, 2}, {"E-compressor", "", 3, 2},
	{"PTC heater", "", 8, 2}, {"Climate total", " W", 10, 2},
	{"Battery heater", " W", 12, 1}, {"Battery heater total", " Wh", 13, 1},
	{"MG1 power", " kW", 14, 2}, {"MG2 power", " kW", 16, 2},
	{"MG1 energy", " Wh", 18, 2}, {"MG2 energy", " Wh", 20, 2},
	{"LDC load", "", 22, 1}, {"Climate power", " W", 23, 2},
	{"Battery compressor", " W", 25, 2}, {"Battery energy", " Wh", 27, 2},
}

func (pf legacyPowerField) read(p []byte) Field {
	f := Field{Name: pf.name, Unit: pf.unit}
	if pf.n == 1 {
		f.Value, f.Valid = int(p[pf.off]), p[pf.off] != 0xff
		return f
	}
	f.Value = be16(p[pf.off:])
	f.Valid = f.Value != 0xffff
	return f
}
