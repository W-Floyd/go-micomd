package main

import (
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
