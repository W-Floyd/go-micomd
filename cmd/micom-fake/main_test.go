package main

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	micom "github.com/W-Floyd/go-micomd"
)

// sink collects what the fake sends.
type sink struct {
	d   micom.Decoder
	out []micom.Frame
}

func (s *sink) Write(p []byte) (int, error) {
	s.out = append(s.out, s.d.Write(p)...)
	return len(p), nil
}

func frame(t *testing.T, s string) micom.Frame {
	t.Helper()
	raw, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return micom.Frame{Hdr: [4]byte(raw[:4]), Cmd: raw[4], Payload: raw[5:]}
}

// Each row sends the SoC's frames and wants, in what comes back, each named
// field at its raw value: the invariant is that the fake answers as the
// variant says the car reports.
func TestFakeAnswersAsTheVariant(t *testing.T) {
	scenario := `[
		{"msg": "Boot record (full)", "set": {"ACC": true, "IGN1": true, "IGN3": true, "CAN DB generation": 2}},
		{"msg": "Charge limit, AC", "set": {"Charge limit, AC": 80}},
		{"msg": "Charge limit, DC", "set": {"Charge limit, DC": 90}}
	]`
	for _, tc := range []struct {
		name string
		soc  []string // hdr cmd payload, hex
		want map[string]map[string]uint64
	}{
		{"hello: the boot record", []string{"8a010101 01"},
			map[string]map[string]uint64{"Boot record (full)": {"CAN DB generation": 2, "IGN3": 1}}},
		{"bulk read: filled from the items held", []string{"880c010c 01"},
			map[string]map[string]uint64{"HEV status (full)": {"Charge limit, AC": 160, "Charge limit, DC": 180}}},
		{"off-peak 22:30-06:00, charge only", []string{"880c010c 05 b3 30 00 00 c0"},
			map[string]map[string]uint64{"Schedule settings": {"Off-peak start hour": 22, "Off-peak start minute bits": 3, "Off-peak end hour": 6, "Off-peak end minute bits": 0, "Scheduled charging mode": 3}}},
		{"departure 1 07:30 Mon Wed on, climate 0x14", []string{"880c010c 06 3b a1 60 00 a8 00 00"},
			map[string]map[string]uint64{
				"Departure timers":  {"Departure 1 hour": 7, "Departure 1 minute bits": 3, "Departure 2 hour": 12},
				"Departure days":    {"Departure 1 Monday": 1, "Departure 1 Tuesday": 0, "Departure 1 Wednesday": 1, "Departure 1 enabled": 1},
				"Schedule settings": {"Scheduled climate": 1, "Scheduled climate temperature": 0x14}}},
		{"off-peak, then departure 2 Friday on: 0x8C53 holds both", []string{"880c010c 05 b3 30 00 00 c0", "880c010c 06 60 00 60 09 29 14 80"},
			map[string]map[string]uint64{"Schedule settings": {"Off-peak start hour": 22, "Scheduled charging mode": 3, "Departure 2 Friday": 1, "Departure 2 enabled": 1, "Scheduled climate temperature": 20}}},
		{"AC limit 70 %, the rest unchanged", []string{"880c010c 18 00 00 00 00 8c ff ff"},
			map[string]map[string]uint64{"Charge limit, AC": {"Charge limit, AC": 140}, "Winter mode": {"Winter mode": 0}}},
		{"conditioning on, then the pulse's default", []string{"880c010c 62 02", "880c010c 62 00"},
			map[string]map[string]uint64{"Battery conditioning mode": {"Battery conditioning mode": 2}}},
		{"read 0x0C53 after a write: the written state", []string{"880c010c 05 b3 30 00 00 40", "880c010c 53"},
			map[string]map[string]uint64{"Schedule settings": {"Off-peak start hour": 22}}},
		{"-ev-schedule-request's reads: the records held", []string{"880c010c 53", "880c010c 54"},
			map[string]map[string]uint64{
				"Schedule settings":       {"Off-peak start hour": 20, "Off-peak end hour": 16, "Scheduled climate temperature": 20},
				"Scheduled charge window": {"Temperature unit": 1, "Scheduled charge start day": 7}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := micom.LoadVariant(micom.DefaultVariantName)
			if err != nil {
				t.Fatal(err)
			}
			f := newFake(v, t.Logf)
			if err := f.load(bytes.NewBufferString(scenario)); err != nil {
				t.Fatal(err)
			}
			// The 0x8C55 the car sends with nothing set: 12:00; 0x8C53 and
			// 0x8C54 as captured.
			f.hold(frame(t, "0c88018c 55 cdeb7b0c0c000000"))
			f.hold(frame(t, "0c88018c 53 0014100200400050"))
			f.hold(frame(t, "0c88018c 54 00010040ffffff0f"))
			s := &sink{}
			f.w = s
			for _, x := range tc.soc {
				f.handle(frame(t, x))
			}
			// What the frames sent say, field by field, the last frame winning.
			got := map[string]uint64{}
			for _, o := range s.out {
				if _, raw, ok := v.RawFields(o); ok {
					for k, x := range raw {
						got[k] = x
					}
				}
			}
			for msg, fields := range tc.want {
				for k, x := range fields {
					if g, ok := got[k]; !ok || g != x {
						t.Errorf("%s: %s = %v (%v), want %d; sent %v", msg, k, g, ok, x, s.out)
					}
				}
			}
			if len(s.out) == 0 {
				t.Error("nothing sent")
			}
		})
	}
}
