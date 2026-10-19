package micom

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    []byte
		want KeyEvent
		ok   bool
	}{
		{"short press", []byte{TypeCCP, 0x11}, KeyEvent{TypeCCP, 1, PressShort}, true},
		{"long press", []byte{TypeSteering, 0x23}, KeyEvent{TypeSteering, 3, PressLong}, true},
		{"release", []byte{TypeEject, 0x00}, KeyEvent{TypeEject, 0, PressRelease}, true},
		{"knob type", []byte{8, 0x11}, KeyEvent{}, false},
		{"too short", []byte{TypeCCP}, KeyEvent{}, false},
		{"empty", nil, KeyEvent{}, false},
	} {
		got, ok := ParseKey(tc.p)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: ParseKey = %+v, %v; want %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseKeyMap(t *testing.T) {
	m, keys, err := parseKeyMap("5.1=116,2.3=115,9.1.up=115,9.1.down=114,")
	if err != nil {
		t.Fatal(err)
	}
	if m.push[keyKey{5, 1}] != 116 || m.push[keyKey{2, 3}] != 115 || len(m.push) != 2 {
		t.Errorf("push map = %v", m.push)
	}
	if m.knob[knobKey{9, 1, true}] != 115 || m.knob[knobKey{9, 1, false}] != 114 || len(m.knob) != 2 {
		t.Errorf("knob map = %v", m.knob)
	}
	if !reflect.DeepEqual(keys, []uint16{116, 115, 115, 114}) {
		t.Errorf("keys = %v", keys)
	}
	for _, bad := range []string{"5.1", "5=116", "5.1.sideways=1", "a.b=1", "5.1=x", "1.2.3.4=5"} {
		if _, _, err := parseKeyMap(bad); err == nil {
			t.Errorf("a bad entry parsed: %q", bad)
		}
	}
	if m, keys, err := parseKeyMap(""); err != nil || len(m.push) != 0 || len(m.knob) != 0 || len(keys) != 0 {
		t.Errorf("empty keymap: %v %v %v", m, keys, err)
	}
}

func TestParseKnob(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    []byte
		want KnobEvent
		ok   bool
	}{
		{"volume up", []byte{9, 0x11, 0}, KnobEvent{9, 1, true}, true},
		{"volume down", []byte{9, 0x01, 0}, KnobEvent{9, 1, false}, true},
		{"tune up", []byte{8, 0x11, 0}, KnobEvent{8, 1, true}, true},
		{"tune down", []byte{8, 0x01, 0}, KnobEvent{8, 1, false}, true},
		{"a push key", []byte{0, 0x11, 0}, KnobEvent{}, false},
		{"a direction we do not know", []byte{9, 0x21, 0}, KnobEvent{}, false},
		{"too short", []byte{9}, KnobEvent{}, false},
	} {
		got, ok := ParseKnob(tc.p)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: ParseKnob = %+v, %v; want %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

type fakeSink struct {
	got []int32
	err error
}

func (s *fakeSink) Key(code uint16, value int32) error {
	s.got = append(s.got, int32(code)<<8|value)
	return s.err
}

func TestInjectKeys(t *testing.T) {
	// The key frames of the unit: hdr 07 84 01 87, cmd 01 (captured), Func 0x8701.
	const fn = 0x8701
	sink := &fakeSink{}
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, f) }
	var prev int
	km := keymap{push: map[keyKey]uint16{{0, 1}: 116}, knob: map[knobKey]uint16{{9, 1, true}: 115, {9, 1, false}: 114}}
	h := injectKeys(func(Frame) { prev++ }, sink, km, fn, logf)

	key := func(typ, code, press byte) Frame {
		return Frame{Hdr: [4]byte{0x07, 0x84, 0x01, 0x87}, Cmd: 0x01, Payload: []byte{typ, code | press<<4, 0}}
	}
	h(key(0, 1, PressShort))
	h(key(0, 1, PressLong))
	h(key(0, 1, PressRelease))
	h(key(0, 2, PressShort)) // unmapped: logged
	other := key(0, 1, PressShort)
	other.Cmd = 0x02 // another Func: ignored
	h(other)

	want := []int32{116<<8 | 1, 116<<8 | 2, 116<<8 | 0}
	if !reflect.DeepEqual(sink.got, want) {
		t.Errorf("injected %v, want %v", sink.got, want)
	}
	if prev != 5 {
		t.Errorf("the wrapped handler saw %d frames, want 5", prev)
	}
	if len(logged) != 1 {
		t.Errorf("logged %v, want the one unmapped key", logged)
	}

	sink.err = errors.New("boom")
	logged = nil
	h(key(0, 1, PressShort))
	if len(logged) != 1 {
		t.Errorf("a sink error was not logged: %v", logged)
	}
}

// A knob step is a tap of its key: the press and the release, and nothing for a
// step the keymap does not name.
func TestInjectKnobs(t *testing.T) {
	sink := &fakeSink{}
	var logged []string
	km := keymap{knob: map[knobKey]uint16{{9, 1, true}: 115, {9, 1, false}: 114}}
	h := injectKeys(nil, sink, km, 0x8701, func(f string, a ...any) { logged = append(logged, f) })
	knob := func(typ, dirAndCode byte) Frame {
		return Frame{Hdr: [4]byte{0x07, 0x84, 0x01, 0x87}, Cmd: 0x01, Payload: []byte{typ, dirAndCode, 0}}
	}
	h(knob(9, 0x11)) // volume up
	h(knob(9, 0x01)) // volume down
	h(knob(8, 0x11)) // tune up: unmapped
	want := []int32{115<<8 | 1, 115<<8 | 0, 114<<8 | 1, 114<<8 | 0}
	if !reflect.DeepEqual(sink.got, want) {
		t.Errorf("injected %v, want %v", sink.got, want)
	}
	if len(logged) != 1 {
		t.Errorf("logged %v, want the one unmapped knob", logged)
	}
}
