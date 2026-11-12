package micom

import (
	"encoding/hex"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"
)

// Each row edits the EV6's schedules read-modify-write: the input built from
// the car's reports with the edit over them, the frame micomd sends for it,
// and what the car then reports (Reflect). The invariant: the frame carries
// the edit, and every reflected field reads back as edited or as it was.
func TestReflectedInputEditsOnlyWhatChanged(t *testing.T) {
	v := ev6(t)
	hdr := [4]byte{0x0c, 0x88, 0x01, 0x8c}
	const (
		departures = "0ebd770c0c000000" // 0x8C55, live: both 12:00, no days
		settings   = "0014100200400050" // 0x8C53 as captured: off-peak 20:00-16:00, mode off, climate off, temp code 20
		window     = "00010040ffffff0f" // 0x8C54: no defrost, unit 1
	)
	for _, tc := range []struct {
		name  string
		held  map[byte]string // cmd -> payload
		msg   string
		edit  map[string]uint64
		frame string // payload sent, hex
		err   string
	}{
		{"departure 1 07:30 Mon Wed on", map[byte]string{0x55: departures, 0x53: settings, 0x54: window}, "ev-departures",
			map[string]uint64{"Departure 1 hour": 7, "Departure 1 minute bits": 3, "Departure 1 Monday": 1, "Departure 1 Wednesday": 1, "Departure 1 enabled": 1},
			"3ba16000291480", ""},
		{"departure 2 Friday on: 0x8C53 keeps the off-peak window", map[byte]string{0x55: departures, 0x53: settings, 0x54: window}, "ev-departures",
			map[string]uint64{"Departure 2 Friday": 1, "Departure 2 enabled": 1},
			"60006009291480", ""},
		{"climate on with defrost, the temperature as the car has it", map[byte]string{0x55: departures, 0x53: settings, 0x54: window}, "ev-departures",
			map[string]uint64{"Scheduled climate": 1, "Scheduled climate defrost": 1},
			"60006000a99480", ""},
		{"off-peak 22:30-06:00 charge only (stock's frame)", map[byte]string{0x55: departures, 0x53: settings, 0x54: window}, "ev-off-peak",
			map[string]uint64{"Off-peak start hour": 22, "Off-peak start minute bits": 3, "Off-peak end hour": 6, "Off-peak end minute bits": 0, "Scheduled charging mode": 3},
			"b3300000c0", ""},
		{"mode 1 is not written back", map[byte]string{0x55: departures, 0x53: "0014100200410050", 0x54: window}, "ev-off-peak",
			map[string]uint64{"Off-peak start hour": 22}, "", "code 1, which enum sched_charge_mode does not name"},
		{"nothing of 0x8C53 yet", map[byte]string{0x55: departures, 0x54: window}, "ev-departures",
			map[string]uint64{"Departure 1 enabled": 1}, "", "nothing received yet of Departure 2 Friday"},
		{"a 2-bit day reading 2 does not fit the write's bit", map[byte]string{0x55: "0ebd770c0c020000", 0x53: settings, 0x54: window}, "ev-departures",
			map[string]uint64{"Departure 1 enabled": 1}, "", "Departure 1 Monday reads 2, wider than d1_mon"},
		{"stock's unset hour 31 is refused", map[byte]string{0x55: "0ebd77ff0c000000", 0x53: settings, 0x54: window}, "ev-departures",
			map[string]uint64{"Departure 2 enabled": 1}, "", "above the maximum 23"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []Frame
			e := newInputEngine(v, InputGates{Inferred: true, Dangerous: true}, func(f Frame) error { sent = append(sent, f); return nil },
				func([]string, bool) {}, time.Now, t.Logf)
			held := map[string][]byte{}
			now := map[string]uint64{}
			for cmd, p := range tc.held {
				b, _ := hex.DecodeString(p)
				f := Frame{Hdr: hdr, Cmd: cmd, Payload: b}
				e.observe(f)
				for msg, fs := range v.ReceivedFields(f) {
					held[msg] = b
					for k, r := range fs {
						now[k] = r.Raw
					}
				}
			}
			was := maps.Clone(now)
			maps.Copy(now, tc.edit)
			in, err := v.ReflectedInput(tc.msg, func(_, field string) (uint64, bool) { x, ok := now[field]; return x, ok })
			if tc.err != "" || err != nil {
				if err == nil || tc.err == "" || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error %v, want %q", err, tc.err)
				}
				return
			}
			in["msg"] = tc.msg
			line, _ := json.Marshal(in)
			if err := e.handle(line); err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			if len(sent) != 1 || hex.EncodeToString(sent[0].Payload) != tc.frame {
				t.Fatalf("%s sent %v, want %s", line, sent, tc.frame)
			}
			back, err := v.Reflect(sent[0], func(msg string) []byte { return held[msg] })
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]uint64{}
			for _, f := range back {
				for _, fs := range v.ReceivedFields(f) {
					for k, r := range fs {
						got[k] = r.Raw
					}
				}
			}
			for _, r := range v.Reflections(tc.msg) {
				if _, ok := got[r.From]; !ok {
					t.Errorf("%s not reported back", r.From)
				}
			}
			for k, x := range got {
				want := was[k]
				if e, ok := tc.edit[k]; ok {
					want = e
				}
				if x != want {
					t.Errorf("%s reads back %d, want %d", k, x, want)
				}
			}
		})
	}
}
