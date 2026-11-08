package micom

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A variant with to_micom messages on the audio header, for the engine.
const inputVariant = `schema: 1
name: t
extends: hkmc-gen5w
enums: {mode: {media: 0x0e, radio: 1}}
messages:
  - {name: mode, dir: to_micom, hdr: audio_tx, cmd: 0x01, length: 2, status: captured, ref: r,
     fields: [{name: mode, off: 0, enum: mode}, {name: level, off: 1, max: 9, invalid: 0xff}]}
  - {name: coded, dir: to_micom, hdr: audio_tx, cmd: 0x40, length: 1, status: code, ref: r, fields: [{name: a, off: 0}]}
`

// recorder collects what an engine sends, as "cmd:payload" in hex.
type recorder struct{ sent []string }

func (r *recorder) send(f Frame) error {
	r.sent = append(r.sent, fmt.Sprintf("%02x:%x", f.Cmd, f.Payload))
	return nil
}

// Each row is a sequence of input lines: what each gets back (ok, or a part
// of the error) and every frame sent.
func TestInput(t *testing.T) {
	v, err := loadVariantData([]byte(inputVariant), "test")
	if err != nil {
		t.Fatal(err)
	}
	type step struct{ line, reply string }
	for _, tc := range []struct {
		name     string
		inferred bool
		steps    []step
		frames   []string
	}{
		{"merged, sent on change", false, []step{
			{`{"msg":"mode","mode":"media","level":5}`, "ok"},
			{`{"msg":"mode","level":6}`, "ok"},
			{`{"msg":"mode","level":6}`, "ok"},
			{`{"msg":"mode","level":null}`, "ok"},
		}, []string{"01:0e05", "01:0e06", "01:0eff"}},
		{"refused", false, []step{
			{`mode media`, "line is not a JSON object"},
			{`{"msg":"mode"} {}`, "line is not a JSON object"},
			{`{"mode":"media"}`, `"msg" is required`},
			{`{"msg":"nope"}`, `no message "nope"`},
			{`{"msg":"Variant code"}`, "Variant code: from_micom, not an input"},
			{`{"msg":"mode","mode":"media","speed":1}`, "mode: speed: no such field"},
			{`{"msg":"mode","mode":"tape"}`, `mode: mode: "tape" is not in enum mode`},
			{`{"msg":"mode","mode":14}`, "mode: mode: 14 is not a name of enum mode"},
			{`{"msg":"mode","mode":"media","level":12}`, "mode: level: 12 is above the maximum 9"},
			{`{"msg":"mode","mode":"media","level":2.5}`, "mode: level: 2.5 is not an integer"},
			{`{"msg":"mode","mode":"media","level":[1]}`, "mode: level: [1] is not a number"},
			{`{"msg":"mode","mode":null}`, "mode: mode: null: the field has no invalid marker"},
			{`{"msg":"mode","level":1}`, "mode: mode: required"},
			{`{"msg":"coded","a":1}`, "coded: status code: needs -allow-inferred"},
		}, nil},
		{"a refused input changes nothing", false, []step{
			{`{"msg":"mode","mode":"media","level":12}`, "above the maximum"},
			{`{"msg":"mode","level":1}`, "mode: required"},
			{`{"msg":"mode","mode":"radio"}`, "ok"},
		}, []string{"01:01ff"}},
		{"inferred", true, []step{
			{`{"msg":"coded","a":1}`, "ok"},
		}, []string{"40:01"}},
	} {
		var r recorder
		e := newInputEngine(v, tc.inferred, r.send, func() time.Time { return time.Unix(0, 0) })
		for _, s := range tc.steps {
			got := "ok"
			if err := e.handle([]byte(s.line)); err != nil {
				got = err.Error()
			}
			if !strings.Contains(got, s.reply) {
				t.Errorf("%s: %s: %q, want %q", tc.name, s.line, got, s.reply)
			}
		}
		if !slices.Equal(r.sent, tc.frames) {
			t.Errorf("%s: sent %v, want %v", tc.name, r.sent, tc.frames)
		}
	}
}
