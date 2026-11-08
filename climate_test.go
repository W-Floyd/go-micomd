package micom

import (
	"reflect"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// Each frame, folded into an empty state, gives the event and the state shown;
// the values are the captured ones of TestCarDescribe.
func TestCarClimateApply(t *testing.T) {
	for _, tc := range []struct {
		hdr     string
		cmd     byte
		payload string
		event   ClimateEvent
		want    CarClimate
	}{
		{"43c200c3", 0x1b, "01", ClimateBegin, CarClimate{}},
		{"43c200c3", 0x1b, "00", ClimateEnd, CarClimate{}},
		{"43c200c3", 0x05, "10", ClimateChanged, CarClimate{DriverSetF: ptr(72)}},
		{"43c200c3", 0x0b, "00", ClimateChanged, CarClimate{PassengerSetF: ptr(0)}},
		{"43c200c3", 0x0d, "03", ClimateChanged, CarClimate{Airflow: ptr(byte(3))}},
		{"43c200c3", 0x0d, "07", ClimateNone, CarClimate{}},
		{"43c200c3", 0x0f, "01", ClimateChanged, CarClimate{Auto: ptr(true)}},
		{"43c200c3", 0x11, "00", ClimateChanged, CarClimate{AC: ptr(false)}},
		{"43c200c3", 0x15, "05", ClimateChanged, CarClimate{FanLevel: ptr(4), FanAuto: ptr(false)}},
		{"43c200c3", 0x15, "0a", ClimateChanged, CarClimate{FanAuto: ptr(true)}},
		{"43c200c3", 0x15, "00", ClimateChanged, CarClimate{FanLevel: ptr(0), FanAuto: ptr(false)}},
		{"43c200c3", 0x1e, "02", ClimateChanged, CarClimate{Recirculation: ptr(false)}},
		{"43c200c3", 0x43, "01", ClimateChanged, CarClimate{Recirculation: ptr(true)}},
		{"43c200c3", 0x27, "01", ClimateChanged, CarClimate{ZonesSynced: ptr(true)}},
		{"55ca00d5", 0x20, "08", ClimateChanged, CarClimate{DriverSeatHeat: ptr(3)}},
		{"44c100c4", 0x1b, "02", ClimateChanged, CarClimate{PassengerSeatHeat: ptr(0)}},
		{"55ca00d5", 0x20, "05", ClimateNone, CarClimate{}},
		{"07840187", 0x01, "091100", ClimateNone, CarClimate{}},
	} {
		var c CarClimate
		f := capture(t, tc.hdr, tc.cmd, tc.payload)
		if ev := c.Apply(f); ev != tc.event {
			t.Errorf("%s %02x %s: event %d, want %d", tc.hdr, tc.cmd, tc.payload, ev, tc.event)
		}
		if !reflect.DeepEqual(c, tc.want) {
			t.Errorf("%s %02x %s: state %+v, want %+v", tc.hdr, tc.cmd, tc.payload, c, tc.want)
		}
		// The same frame again changes nothing.
		if tc.event == ClimateChanged {
			if ev := c.Apply(f); ev != ClimateNone {
				t.Errorf("%s %02x %s: repeated frame gave %d", tc.hdr, tc.cmd, tc.payload, ev)
			}
		}
	}
}
