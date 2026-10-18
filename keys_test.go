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
	m, keys, err := parseKeyMap("5.1=116,2.3=115,")
	if err != nil {
		t.Fatal(err)
	}
	if m[keyKey{5, 1}] != 116 || m[keyKey{2, 3}] != 115 || len(m) != 2 {
		t.Errorf("map = %v", m)
	}
	if !reflect.DeepEqual(keys, []uint16{116, 115}) {
		t.Errorf("keys = %v", keys)
	}
	if _, _, err := parseKeyMap("5.1"); err == nil {
		t.Error("a bad entry parsed")
	}
	if m, keys, err := parseKeyMap(""); err != nil || len(m) != 0 || len(keys) != 0 {
		t.Errorf("empty keymap: %v %v %v", m, keys, err)
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
	h := injectKeys(func(Frame) { prev++ }, sink, map[keyKey]uint16{{0, 1}: 116}, fn, logf)

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
