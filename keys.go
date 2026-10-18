package micom

import (
	"fmt"
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

// keyKey is a push key as the keymap names it.
type keyKey [2]int

// parseKeyMap reads "type.code=KEY,..." (KEY a decimal Linux key number): the
// map, and the list of keys the virtual keyboard has to offer.
func parseKeyMap(s string) (map[keyKey]uint16, []uint16, error) {
	m := map[keyKey]uint16{}
	var keys []uint16
	for _, kv := range strings.Split(s, ",") {
		if kv == "" {
			continue
		}
		var t, c, k int
		if _, err := fmt.Sscanf(kv, "%d.%d=%d", &t, &c, &k); err != nil {
			return nil, nil, fmt.Errorf("keymap %q: %v", kv, err)
		}
		m[keyKey{t, c}] = uint16(k)
		keys = append(keys, uint16(k))
	}
	return m, keys, nil
}

// keySink is what key events are injected into: a uinput device.
type keySink interface {
	Key(code uint16, value int32) error
}

// injectKeys wraps prev (it may be nil) so that a frame with Func fn, from the
// micom (Hdr[0] 0x0c), that parses as a mapped push key is sent to ui, as a
// press (1), release (0) or long press (2, the kernel's key repeat).
func injectKeys(prev func(Frame), ui keySink, km map[keyKey]uint16, fn uint16, logf func(string, ...any)) func(Frame) {
	return func(f Frame) {
		if prev != nil {
			prev(f)
		}
		if Func(f) != fn || f.Hdr[0] != 0x0c {
			return
		}
		e, ok := ParseKey(f.Payload)
		if !ok {
			return
		}
		k, ok := km[keyKey{e.Type, e.Code}]
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
