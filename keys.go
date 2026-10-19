package micom

import (
	"fmt"
	"strconv"
	"strings"
)

// Hard-key frames, from Android's ModeKeyDispatcher.handleKey(byte[]) and the
// stock micomd binary's log strings. Payload p: p[0] is the key type, the low
// nibble of p[1] the key code, and p[1]>>4 the press (0 release, 1 short, 2
// long). Types 0-7 are push keys, 8-11 knobs. Which Func carries it is NOT
// known: take it from a -dump capture and give it to -keyfunc.

// Press is how a key was pressed.
const (
	PressRelease = 0
	PressShort   = 1
	PressLong    = 2
)

// Key types (PushKeyFromMicom.TYPECODE_*).
const (
	TypeFrontCol1 = 0
	TypeFrontCol2 = 1
	TypeSteering  = 2
	TypeRear      = 3
	TypeMTS       = 4
	TypeCCP       = 5
	TypeRRC       = 6
	TypeEject     = 7
)

// KeyEvent is one push-key event.
type KeyEvent struct {
	Type, Code, Press int
}

// ParseKey reads a push-key payload; ok is false for knobs and short data.
func ParseKey(p []byte) (e KeyEvent, ok bool) {
	if len(p) < 2 || p[0] > TypeEject {
		return e, false
	}
	return KeyEvent{Type: int(p[0]), Code: int(p[1] & 0xf), Press: int(p[1] >> 4)}, true
}

// KnobEvent is one step of a rotary knob (types 8 to 11): up is clockwise.
type KnobEvent struct {
	Type, Code int
	Up         bool
}

// ParseKnob reads a knob payload (type 8 the tune knob, 9 the volume knob on
// the EV6 unit), the direction the high nibble of the second byte: 1 up, 0
// down. ok is false for push keys, other values and short data.
func ParseKnob(p []byte) (e KnobEvent, ok bool) {
	if len(p) < 2 || p[0] < 8 || p[0] > 11 || p[1]>>4 > 1 {
		return e, false
	}
	return KnobEvent{Type: int(p[0]), Code: int(p[1] & 0xf), Up: p[1]>>4 == 1}, true
}

// keyKey is a push key as the keymap names it, knobKey one direction of a knob.
type keyKey [2]int

type knobKey struct {
	typ, code int
	up        bool
}

// keymap is what -keymap names: push keys by type and code, knob steps by
// type, code and direction.
type keymap struct {
	push map[keyKey]uint16
	knob map[knobKey]uint16
}

// parseKeyMap reads "type.code=KEY,..." for push keys and
// "type.code.up=KEY,type.code.down=KEY" for knobs (KEY a decimal Linux key
// number): the map, and the list of keys the virtual keyboard has to offer.
func parseKeyMap(s string) (keymap, []uint16, error) {
	m := keymap{push: map[keyKey]uint16{}, knob: map[knobKey]uint16{}}
	var keys []uint16
	for _, kv := range strings.Split(s, ",") {
		if kv == "" {
			continue
		}
		lhs, rhs, ok := strings.Cut(kv, "=")
		parts := strings.Split(lhs, ".")
		k, kerr := strconv.Atoi(rhs)
		if !ok || kerr != nil || len(parts) < 2 || len(parts) > 3 {
			return keymap{}, nil, fmt.Errorf("keymap %q: want type.code=KEY or type.code.up|down=KEY", kv)
		}
		t, terr := strconv.Atoi(parts[0])
		c, cerr := strconv.Atoi(parts[1])
		if terr != nil || cerr != nil {
			return keymap{}, nil, fmt.Errorf("keymap %q: type and code are numbers", kv)
		}
		if len(parts) == 2 {
			m.push[keyKey{t, c}] = uint16(k)
		} else {
			if parts[2] != "up" && parts[2] != "down" {
				return keymap{}, nil, fmt.Errorf("keymap %q: a knob's direction is up or down", kv)
			}
			m.knob[knobKey{t, c, parts[2] == "up"}] = uint16(k)
		}
		keys = append(keys, uint16(k))
	}
	return m, keys, nil
}

// keySink is what key events are injected into: a uinput device.
type keySink interface {
	Key(code uint16, value int32) error
}

// injectKeys wraps prev (it may be nil) so that a frame with Func fn, from the
// micom, that parses as a mapped push key is sent to ui, as a
// press (1), release (0) or long press (2, the kernel's key repeat); a knob
// step is a tap of its key (press and release).
func injectKeys(prev func(Frame), ui keySink, km keymap, fn uint16, logf func(string, ...any)) func(Frame) {
	return func(f Frame) {
		if prev != nil {
			prev(f)
		}
		if Func(f) != fn {
			return
		}
		if ke, ok := ParseKnob(f.Payload); ok {
			k, ok := km.knob[knobKey{ke.Type, ke.Code, ke.Up}]
			if !ok {
				logf("unmapped knob type %d code %d up %v", ke.Type, ke.Code, ke.Up)
				return
			}
			for _, v := range []int32{1, 0} {
				if err := ui.Key(k, v); err != nil {
					logf("uinput: %v", err)
					return
				}
			}
			return
		}
		e, ok := ParseKey(f.Payload)
		if !ok {
			return
		}
		k, ok := km.push[keyKey{e.Type, e.Code}]
		if !ok {
			logf("unmapped key type %d code %d press %d", e.Type, e.Code, e.Press)
			return
		}
		v := int32(1)
		switch e.Press {
		case PressRelease:
			v = 0
		case PressLong:
			v = 2
		}
		if err := ui.Key(k, v); err != nil {
			logf("uinput: %v", err)
		}
	}
}
