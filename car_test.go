package micom

import (
	"encoding/hex"
	"testing"
)

// Lines are the captured ones: hdr, cmd and payload as the tap prints them.
func capture(t *testing.T, hdr string, cmd byte, payload string) Frame {
	t.Helper()
	h, err := hex.DecodeString(hdr)
	if err != nil || len(h) != 4 {
		t.Fatalf("hdr %q", hdr)
	}
	p, err := hex.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload %q", payload)
	}
	return Frame{Hdr: [4]byte(h), Cmd: cmd, Payload: p}
}

func TestCarDescribe(t *testing.T) {
	for _, tc := range []struct {
		hdr     string
		cmd     byte
		payload string
		want    string
	}{
		{"07840187", 0x01, "091100", "Volume knob up"},
		{"07840187", 0x01, "090100", "Volume knob down"},
		{"07840187", 0x01, "081100", "Tune knob up"},
		{"07840187", 0x01, "080100", "Tune knob down"},
		{"07840187", 0x01, "001400", "Map press"},
		{"07840187", 0x01, "002400", "Map long press"},
		{"07840187", 0x01, "000400", "Map release"},
		{"07840187", 0x01, "011500", "Setup press"},
		{"07840187", 0x01, "011100", "Tune knob press"},
		{"07840187", 0x01, "001100", "Volume knob press"},
		{"07840187", 0x01, "001f00", "Key 0.15 press"},
		{"43c200c3", 0x05, "19", "Climate driver set point: 81 F"},
		{"43c200c3", 0x09, "0f", "Climate passenger set point: 71 F"},
		{"43c200c3", 0x09, "00", "Climate passenger set point: off"},
		{"43c200c3", 0x0d, "03", "Climate airflow: feet"},
		{"43c200c3", 0x0d, "08", "Climate airflow: auto"},
		{"43c200c3", 0x0f, "01", "Climate auto: on"},
		{"43c200c3", 0x11, "00", "Climate A/C: off"},
		{"43c200c3", 0x15, "04", "Climate fan: level 3"},
		{"43c200c3", 0x15, "09", "Climate fan: level 8"},
		{"43c200c3", 0x15, "0a", "Climate fan: auto"},
		{"43c200c3", 0x19, "01", "Climate driver only: on"},
		{"43c200c3", 0x1e, "02", "Climate recirculation: off"},
		{"43c200c3", 0x27, "00", "Climate zones: independent"},
		{"43c200c3", 0x33, "01", "Climate heat: on"},
		{"43c200c3", 0x4f, "04", "Climate auto level: 3"},
		{"43c200c3", 0x50, "02", "Climate auto level (inverse): 3"},
		{"43c200c3", 0x4f, "01", "Climate auto level: off"},
		{"43c200c3", 0x1b, "01", "Climate update begins"},
		{"43c200c3", 0x1b, "00", "Climate update ends"},
		{"55ca00d5", 0x20, "08", "Seat heat, driver: heat level 3"},
		{"55ca00d5", 0x21, "02", "Seat heat, passenger: off"},
		{"44c100c4", 0x19, "06", "Seat heat, driver (repeat): heat level 1"},
		{"44c100c4", 0x1b, "07", "Seat heat, passenger (repeat): heat level 2"},
		{"44c100c4", 0x02, "00", "Door driver: locked"},
		{"44c100c4", 0x00, "01", "Door front right: unlocked"},
		{"44c100c4", 0x07, "00", "Door rear right: locked"},
		{"44c100c4", 0x03, "01", "Door driver: open"},
		{"44c100c4", 0x05, "00", "Door rear left: closed"},
		{"44c100c4", 0x01, "01", "Door front right: open"},
		{"44c100c4", 0x08, "01", "Door rear right: open"},
		{"44c100c4", 0x06, "01", "Trunk: open"},
		{"018a0181", 0x2f, "03", "Open doors: trunk"},
		{"44c100c4", 0x11, "01", "Hood: open"},
		{"44c100c4", 0x11, "00", "Hood: closed"},
		{"018a0181", 0x2f, "01", "Open doors: none"},
		{"018a0181", 0x2f, "09", "Open doors: driver"},
		{"018a0181", 0x2f, "21", "Open doors: rear left"},
		{"018a0181", 0x2f, "05", "Open doors: front right"},
		{"018a0181", 0x2f, "11", "Open doors: rear right"},
		{"018a0181", 0x2f, "0d", "Open doors: front right, driver"},
		{"018a0181", 0x2f, "81", "Open doors: none (other bits 0x80)"},
		{"01c40181", 0x51, "09", "Rear camera: on"},
		{"01c40181", 0x51, "00", "Rear camera: off"},
		{"01c40181", 0x52, "010d0f010002", ""},
		{"43c200c3", 0x77, "01", ""},
		{"43c200c3", 0x15, "0100", ""},
		{"0c88018c", 0x4f, "01", ""},
	} {
		if got := carDescribe(capture(t, tc.hdr, tc.cmd, tc.payload)); got != tc.want {
			t.Errorf("%s/%02x %s: %q, want %q", tc.hdr, tc.cmd, tc.payload, got, tc.want)
		}
	}
}
