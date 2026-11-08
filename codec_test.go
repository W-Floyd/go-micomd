package micom

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"
)

// One field, encoded from an input and read back. The bit-form rows are DBC
// signals as cantools lays them out.
func TestFieldCodec(t *testing.T) {
	for _, tc := range []struct {
		field  string
		length int
		in     any
		want   string // payload hex
	}{
		{"{name: a, start_bit: 4, bit_length: 8, endian: little}", 2, 0xab, "b00a"},
		{"{name: a, start_bit: 3, bit_length: 8}", 2, 0xab, "0ab0"},
		{"{name: a, start_bit: 4, bit_length: 4, endian: little, signed: true}", 1, -1, "f0"},
		{"{name: a, start_bit: 9, bit_length: 1, type: bool}", 2, true, "0002"},
		{"{name: a, off: 0, type: u16, endian: little}", 2, 0x1234, "3412"},
		{"{name: a, off: 0, type: u16}", 2, 0x1234, "1234"},
		{"{name: a, off: 0, type: i16}", 2, -2, "fffe"},
		{"{name: a, off: 1, type: u32}", 5, 0xdeadbeef, "00deadbeef"},
		{"{name: a, off: 0, type: i32, endian: little}", 4, -0x12345678, "88a9cbed"},
		{"{name: a, off: 0, bits: \"6:4\"}", 1, 5, "50"},
		{"{name: a, off: 0, bits: \"0\", type: bool}", 1, true, "01"},
		{"{name: a, off: 0, scale: 7.5}", 1, 217.5, "1d"},
		{"{name: a, off: 0, scale: 7.5}", 1, 105.0, "0e"},
		{"{name: a, off: 0, scale: 0.5, offset: -40}", 1, 20.0, "78"},
		{"{name: a, off: 0, invalid: 0x3f}", 1, nil, "3f"},
		{"{name: a, off: 0, enum: e}", 1, "two", "02"},
		{"{name: a, off: 0, type: str, size: 4, pad_right: 0}", 4, "ab", "61620000"},
		{"{name: a, off: 0, type: str, size: 4, encoding: UTF-16LE, pad_right: 0}", 4, "é", "e9000000"},
		{"{name: a, off: 0, type: str, size: 2}", 2, "en", "656e"},
	} {
		y := "schema: 1\nname: t\nextends: hkmc-gen5w\nenums: {e: {one: 1, two: 2}}\n" +
			fmt.Sprintf("messages: [{name: m, dir: to_micom, hdr: audio_tx, cmd: 0x40, length: %d, status: code, ref: r, fields: [%s]}]\n", tc.length, tc.field)
		v, err := loadVariantData([]byte(y), "test")
		if err != nil {
			t.Errorf("%s: %v", tc.field, err)
			continue
		}
		iv, err := toInValue(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		f, err := v.messages["m"].encode(map[string]inValue{"a": iv})
		if err != nil {
			t.Errorf("%s: encode %v: %v", tc.field, tc.in, err)
			continue
		}
		if got := hex.EncodeToString(f.Payload); got != tc.want {
			t.Errorf("%s: %v encodes to %s, want %s", tc.field, tc.in, got, tc.want)
		}
		name, back, ok := v.DecodeInput(f)
		want := tc.in
		if n, isInt := want.(int); isInt {
			want = float64(n)
		}
		if !ok || name != "m" || !reflect.DeepEqual(back["a"], want) {
			t.Errorf("%s: %s reads back as %s %v, want %v", tc.field, tc.want, name, back, want)
		}
	}
}
