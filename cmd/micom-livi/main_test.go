package main

import (
	"maps"
	"reflect"
	"testing"

	micom "github.com/W-Floyd/go-micomd"
)

func TestParseTapLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		ok   bool
		want micom.Frame
	}{
		{"15:04:05.000 rx hdr=43c200c3 cmd=0d payload=03  # Climate airflow: feet", true,
			micom.Frame{Hdr: [4]byte{0x43, 0xc2, 0x00, 0xc3}, Cmd: 0x0d, Payload: []byte{3}}},
		{"15:04:05.000 rx hdr=0c88018c cmd=5c payload=15ff", true,
			micom.Frame{Hdr: [4]byte{0x0c, 0x88, 0x01, 0x8c}, Cmd: 0x5c, Payload: []byte{0x15, 0xff}}},
		{"15:04:05.000 tx hdr=880c010c cmd=5c payload=00", false, micom.Frame{}},
		{"15:04:05.000 rx hdr=43c2 cmd=0d payload=03", false, micom.Frame{}},
		{"15:04:05.000 rx hdr=43c200c3 cmd=0d payload=zz", false, micom.Frame{}},
		{"garbage", false, micom.Frame{}},
	} {
		f, ok := parseTapLine(tc.line)
		if ok != tc.ok || (ok && !reflect.DeepEqual(f, tc.want)) {
			t.Errorf("%q: %+v %v, want %+v %v", tc.line, f, ok, tc.want, tc.ok)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// The speed, state of charge and remaining energy reach LIVI in its units,
// read through the default variant's messages; other fields do not.
func TestLiviCar(t *testing.T) {
	for _, tc := range []struct {
		line string
		want map[string]any
	}{
		{"15:04:05.000 rx hdr=44c100c4 cmd=20 payload=0086", map[string]any{"speedKph": 107.826048}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=20 payload=0000", map[string]any{"speedKph": 0.0}},
		{"15:04:05.000 rx hdr=0d8f018d cmd=47 payload=9b006f54", map[string]any{"fuelPct": 77.5, "batteryLevelKwh": 57.0}},
		{"15:04:05.000 rx hdr=0d8f018d cmd=46 payload=02", nil},     // cruise: not a LIVI field
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=5700f5", nil}, // climate power draw
		{"15:04:05.000 rx hdr=0d8f018d cmd=47 payload=9b", nil},     // too short to decode
	} {
		f, ok := parseTapLine(tc.line)
		if !ok {
			t.Fatalf("%q does not parse", tc.line)
		}
		if got := liviCar(micom.Decode(f)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: %v, want %v", tc.line, got, tc.want)
		}
	}
}

// A post carries only the fields LIVI has not taken as they are now, and
// after a failure (sent cleared) all of them.
func TestPosterChanges(t *testing.T) {
	p := newPoster("", 0, t.Logf)
	sent := map[string]string{}
	for _, tc := range []struct {
		name string
		set  map[string]any
		fail bool
		want map[string]any
	}{
		{"first values", map[string]any{"speedKph": 10.0, "fuelPct": 80.0}, false, map[string]any{"speedKph": 10.0, "fuelPct": 80.0}},
		{"speed only changes", map[string]any{"speedKph": 12.0, "fuelPct": 80.0}, false, map[string]any{"speedKph": 12.0}},
		{"nothing changes", map[string]any{"speedKph": 12.0}, false, nil},
		{"the climate is one field", map[string]any{"climate": map[string]any{"source": "car", "ac": true}}, true, map[string]any{"climate": map[string]any{"source": "car", "ac": true}}},
		{"after a failure, everything", nil, false, map[string]any{"speedKph": 12.0, "fuelPct": 80.0, "climate": map[string]any{"source": "car", "ac": true}}},
	} {
		for k, v := range tc.set {
			p.set(k, v)
		}
		body, js := p.changes(sent)
		if !reflect.DeepEqual(body, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, body, tc.want)
		}
		if tc.fail {
			clear(sent)
			continue
		}
		maps.Copy(sent, js)
	}
	// The climate is set only once the car has reported some of it.
	p.climate(map[string]any{"source": "car"})
	if body, _ := p.changes(sent); body != nil {
		t.Errorf("an empty climate posted: %v", body)
	}
}

func TestLiviClimate(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   micom.CarClimate
		want map[string]any
	}{
		{"nothing reported", micom.CarClimate{}, map[string]any{"source": "car"}},
		{"set points to half degrees C, in LIVI's range, zone off left out",
			micom.CarClimate{DriverSetF: ptr(72), PassengerSetF: ptr(0)},
			map[string]any{"source": "car", "driverTargetC": 22.0}},
		{"set point above LIVI's range", micom.CarClimate{DriverSetF: ptr(90)},
			map[string]any{"source": "car", "driverTargetC": 28.0}},
		{"fan level 8 is LIVI's 7", micom.CarClimate{FanLevel: ptr(8), FanAuto: ptr(false)},
			map[string]any{"source": "car", "fanLevel": 7, "fanAuto": false, "on": true}},
		{"fan off", micom.CarClimate{FanLevel: ptr(0), FanAuto: ptr(false)},
			map[string]any{"source": "car", "fanLevel": 0, "fanAuto": false, "on": false}},
		{"airflow feet and defrost", micom.CarClimate{Airflow: ptr(byte(4))},
			map[string]any{"source": "car", "ventsAuto": false, "ventMiddle": false, "ventLower": true, "ventUpper": true}},
		{"airflow auto", micom.CarClimate{Airflow: ptr(byte(8))},
			map[string]any{"source": "car", "ventsAuto": true}},
		{"seat heat and flags",
			micom.CarClimate{DriverSeatHeat: ptr(3), PassengerSeatHeat: ptr(0), AC: ptr(true), Recirculation: ptr(false)},
			map[string]any{"source": "car", "driverSeatHeating": 3, "passengerSeatHeating": 0, "ac": true, "recirculation": false}},
	} {
		if got := liviClimate(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
