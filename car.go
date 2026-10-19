package micom

import (
	"fmt"
	"strings"
)

// Frames of the head unit's front panel and climate controls, read off live
// captures of a Kia EV6 head unit (every value below was seen by pressing the
// control; none is from a specification). The micom relays them from other
// nodes of the car, each with its own header:
//
//	07 84 01 87  cmd 01  keys and knobs
//	43 c2 00 c3          climate panel (a burst of changed fields, bracketed by 1b 01 ... 1b 00)
//	55 ca 00 d5          seat heat command; 44 c1 00 c4 repeats it
//
// carDescribe reads one into a line of text, "" for a frame it does not know.
func carDescribe(f Frame) string {
	h, p := f.Hdr, f.Payload
	switch {
	case h == [4]byte{0x07, 0x84, 0x01, 0x87} && f.Cmd == 0x01:
		return describeKey(p)
	case h == [4]byte{0x43, 0xc2, 0x00, 0xc3}:
		return describeClimate(f.Cmd, p)
	case h == [4]byte{0x01, 0xc4, 0x01, 0x81} && f.Cmd == 0x51 && len(p) == 1:
		// The rear camera button: 09 when pressed on, 00 off, with cmd 52 (six
		// bytes, the last 02 or 00) following. Two presses seen, so the other
		// values of 51 are unknown.
		switch p[0] {
		case 0:
			return "Rear camera: off"
		case 9:
			return "Rear camera: on"
		}
		return fmt.Sprintf("Rear camera mode: code %#02x", p[0])
	case h == [4]byte{0x55, 0xca, 0x00, 0xd5}:
		switch f.Cmd {
		case 0x20:
			return "Seat heat, driver: " + seatHeat(p)
		case 0x21:
			return "Seat heat, passenger: " + seatHeat(p)
		}
	case h == [4]byte{0x01, 0x8a, 0x01, 0x81} && f.Cmd == 0x2f && len(p) == 1:
		return describeOpen(p[0])
	case h == [4]byte{0x44, 0xc1, 0x00, 0xc4}:
		if s := describeDoor(f.Cmd, p); s != "" {
			return s
		}
		switch f.Cmd {
		case 0x19:
			return "Seat heat, driver (repeat): " + seatHeat(p)
		case 0x1b:
			return "Seat heat, passenger (repeat): " + seatHeat(p)
		}
	}
	return ""
}

// Doors, on the body node (44 c1 00 c4): each door has a lock command and the
// next number is its open/closed one. Locks 00 locked, 01 unlocked; doors 01
// open, 00 closed. The trunk (06) and the hood (11) have no lock command.
var doors = []struct {
	name       string
	lock, open byte
}{
	{"front right", 0x00, 0x01}, {"driver", 0x02, 0x03},
	{"rear left", 0x04, 0x05}, {"rear right", 0x07, 0x08},
}

func describeDoor(cmd byte, p []byte) string {
	if len(p) != 1 || p[0] > 1 {
		return ""
	}
	switch cmd {
	case 0x06, 0x11: // the trunk (also bit 1 of 2f) and the hood (no 2f bit)
		name := "Trunk"
		if cmd == 0x11 {
			name = "Hood"
		}
		if p[0] == 1 {
			return name + ": open"
		}
		return name + ": closed"
	}
	for _, d := range doors {
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

// describeOpen reads the system node's bitmask of what keeps the micom awake
// (cmd 2f): bit 0 always set, then one bit per open door (front right 2, driver
// 3, rear right 4, rear left 5) and the trunk (1). The other bits are not known.
func describeOpen(v byte) string {
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

// Push keys are type.code (type 0 and 1 the two front columns). The knobs are
// type 8 (tune) and 9 (volume), code 1, their direction the high nibble of the
// second byte; a push key's is its press state (0 release, 1 press, 2 the one
// long-press frame, sent about 0.8 s into a hold, no repeat).
var keyLabels = map[[2]byte]string{
	{0, 1}: "Volume knob", {0, 2}: "Seek <", {0, 3}: "Track >",
	{0, 4}: "Map", {0, 5}: "Nav",
	{1, 1}: "Tune knob", {1, 2}: "Radio", {1, 3}: "Media",
	{1, 4}: "Star", {1, 5}: "Setup",
}

func describeKey(p []byte) string {
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
	label, ok := keyLabels[[2]byte{typ, code}]
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

// seatHeat: 02 off, 06 to 08 heat levels 1 to 3.
func seatHeat(p []byte) string {
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

var airflow = map[byte]string{
	0: "off", 1: "face", 2: "face and feet", 3: "feet", 4: "feet and defrost",
	5: "defrost", 8: "auto",
}

func onOff(v byte, on, off byte) string {
	switch v {
	case on:
		return "on"
	case off:
		return "off"
	}
	return fmt.Sprintf("code %#02x", v)
}

// setPoint is a temperature field: degrees Fahrenheit less 56, 0 with the zone
// off (driver-only for the passenger's).
func setPoint(v byte) string {
	if v == 0 {
		return "off"
	}
	return fmt.Sprintf("%d F", int(v)+56)
}

func describeClimate(cmd byte, p []byte) string {
	if len(p) != 1 {
		return ""
	}
	v := p[0]
	switch cmd {
	case 0x05, 0x07:
		return "Climate driver set point: " + setPoint(v)
	case 0x09, 0x0b:
		return "Climate passenger set point: " + setPoint(v)
	case 0x0d:
		if s, ok := airflow[v]; ok {
			return "Climate airflow: " + s
		}
		return fmt.Sprintf("Climate airflow: code %#02x", v)
	case 0x0f:
		return "Climate auto: " + onOff(v, 1, 0)
	case 0x11:
		return "Climate A/C: " + onOff(v, 1, 0)
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
		return "Climate driver only: " + onOff(v, 1, 0)
	case 0x1b:
		if v == 1 {
			return "Climate update begins"
		}
		return "Climate update ends"
	case 0x1e, 0x43:
		return "Climate recirculation: " + onOff(v, 1, 2)
	case 0x27:
		if v == 1 {
			return "Climate zones: synced"
		}
		return "Climate zones: independent"
	case 0x33:
		return "Climate heat: " + onOff(v, 1, 0)
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
