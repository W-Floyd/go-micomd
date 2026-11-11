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

// The car's fields reach LIVI in its units, read through the default
// variant's messages and signals; other fields do not. One reader takes the
// lines in order: once the light sensor has spoken, the illumination is
// left out.
func TestLiviCar(t *testing.T) {
	var rd reader
	for _, tc := range []struct {
		line string
		want map[string]any
	}{
		{"15:04:05.000 rx hdr=44c100c4 cmd=20 payload=0086", map[string]any{"speedKph": 107.826048}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=20 payload=0000", map[string]any{"speedKph": 0.0}},
		{"15:04:05.000 rx hdr=0d8f018d cmd=47 payload=9b006f54000066", map[string]any{"fuelPct": 77.5, "batteryLevelKwh": 57.0}},
		{"15:04:05.000 rx hdr=0d8f018d cmd=43 payload=02007500bc", map[string]any{"rangeKm": 188.0}},
		// The range plus what the climate off would add, from one frame.
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=2a00f308", map[string]any{"rangeClimateOffKm": 251.0}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=25016b07", map[string]any{"rangeClimateOffKm": 370.0}},
		{"15:04:05.000 rx hdr=0c88018c cmd=01 payload=0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000a000000000000f30000", map[string]any{"rangeClimateOffKm": 253.0,
			"chargeLimitAcPct": 0.0, "chargeLimitDcPct": 0.0, "chargeTimeAcMin": 0.0, "chargeTimeDcMin": 0.0, "chargeTimeToFullMin": 0.0,
			"efficiencyAvg": 0.0, "rangeAtChargeLimitAcKm": 0.0, "rangeAtChargeLimitDcKm": 0.0}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=22 payload=06c40c", map[string]any{"odometerKm": 44340.4}},
		{"15:04:05.000 rx hdr=43c200c3 cmd=02 payload=5e", map[string]any{"ambientC": 12.222222}},
		{"15:04:05.000 rx hdr=018a0181 cmd=0d payload=00", map[string]any{"nightMode": true}},
		{"15:04:05.000 rx hdr=018a0181 cmd=0d payload=01", map[string]any{"nightMode": false}},
		{"15:04:05.000 rx hdr=018a0181 cmd=0d payload=05", map[string]any{"nightMode": false}},
		// The boot record seeds it.
		{"15:04:05.000 rx hdr=018a0181 cmd=01 payload=0100000000010eca020519222e06544b4b1500", map[string]any{"nightMode": true}},
		{"15:04:05.000 rx hdr=0d8f018d cmd=46 payload=02", nil}, // cruise: not a LIVI field
		{"15:04:05.000 rx hdr=0d8f018d cmd=47 payload=9b", nil}, // too short to decode
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=5700f5", map[string]any{"climatePowerW": 245.0}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=140028", map[string]any{"chargeTimeToFullMin": 40.0}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=1300dc", map[string]any{"chargeTimeAcMin": 220.0}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=2100a0", map[string]any{"chargeLimitAcPct": 80.0}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=1f019e", map[string]any{"rangeAtChargeLimitAcKm": 414.0}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=5e0168", map[string]any{"tripEnergyKwh": 36.0}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=630002", map[string]any{"batteryConditioningMode": "on"}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=630001", map[string]any{"batteryConditioningMode": "off"}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=630000", map[string]any{"batteryConditioningMode": nil}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=790003", map[string]any{"batteryConditioningError": 3.0}},
		{"15:04:05.000 rx hdr=0c88018c cmd=02 payload=790000", map[string]any{"batteryConditioningError": nil}},
		{"15:04:05.000 rx hdr=0c88018c cmd=4f payload=2d3a200100000000", map[string]any{"chargerReady": true}},
		{"15:04:05.000 rx hdr=0c88018c cmd=5c payload=15ffffffffffffffffff01b1000000000000000000000400ff00000000ff", map[string]any{"batteryHeaterW": 0.0, "lowVoltageLoad": 4.0}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=14 payload=57", map[string]any{"auxBatteryPct": 87.0}},
		{"15:04:05.000 rx hdr=50c400d0 cmd=4a payload=07", map[string]any{"gear": "R"}},
		{"15:04:05.000 rx hdr=50c400d0 cmd=4a payload=09", nil}, // a gear not known
		{"15:04:05.000 rx hdr=018a0181 cmd=1b payload=01", map[string]any{"reverse": true}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=03 payload=01", map[string]any{"doorDriverOpen": true}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=00 payload=00", map[string]any{"doorPassengerLocked": true}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=11 payload=00", map[string]any{"hoodOpen": false}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=49 payload=02", map[string]any{"chargePortOpen": false}},
		{"15:04:05.000 rx hdr=018a0181 cmd=03 payload=01", map[string]any{"drivingRegulation": true}},
		{"15:04:05.000 rx hdr=018a0181 cmd=9a payload=00", map[string]any{"drivingRegulationB": false}},
		{"15:04:05.000 rx hdr=018a0181 cmd=29 payload=01", map[string]any{"parkingBrake": true}},
		{"15:04:05.000 rx hdr=018a0181 cmd=0a payload=01", map[string]any{"ignition": true}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=1d payload=07", map[string]any{"rearLeftSeatHeating": 2}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=1f payload=02", map[string]any{"rearRightSeatHeating": 0}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=0d payload=01", map[string]any{"steeringWheelHeating": 1}},
		{"15:04:05.000 rx hdr=43c200c3 cmd=16 payload=01", map[string]any{"climateKey": true}},
		// The whole boot record seeds the state the frames send on change.
		{"15:04:05.000 rx hdr=018a0181 cmd=01 payload=08000100010000ca020000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000200000000000000000001000000000000000000000000000000000000", map[string]any{"nightMode": false, "ignition": true, "vehicleReady": false,
			"drivingRegulation": true, "drivingRegulationB": false, "parkingBrake": true, "reverse": false}},
		// The start-up snapshots, split into their single-signal frames.
		{"15:04:05.000 rx hdr=43c200c3 cmd=00 payload=005e10000f000f000f0001000000010100000500000000000000000000000000000000000000000000000000000000000000000000000000000000", map[string]any{"ambientC": 12.222222}},
		{"15:04:05.000 rx hdr=44c100c4 cmd=0c payload=0100000100000000000000000001000000000057000000000000000000000000000006c40c", map[string]any{"doorPassengerLocked": false, "doorPassengerOpen": false,
			"doorDriverLocked": true, "doorDriverOpen": true, "doorRearLeftLocked": true, "doorRearLeftOpen": false, "trunkOpen": false,
			"doorRearRightLocked": true, "doorRearRightOpen": false, "hoodOpen": false, "auxBatteryPct": 87.0, "odometerKm": 44340.4, "steeringWheelHeating": 0}},
		// The light sensor, then the illumination no more.
		{"15:04:05.000 rx hdr=018a0181 cmd=95 payload=02", map[string]any{"nightMode": true}},
		{"15:04:05.000 rx hdr=018a0181 cmd=0d payload=01", nil},
		{"15:04:05.000 rx hdr=018a0181 cmd=95 payload=01", map[string]any{"nightMode": false}},
	} {
		f, ok := parseTapLine(tc.line)
		if !ok {
			t.Fatalf("%q does not parse", tc.line)
		}
		var got map[string]any
		parts, _ := unbundle(f)
		for _, part := range parts {
			for k, v := range rd.read(part) {
				if got == nil {
					got = map[string]any{}
				}
				got[k] = v
			}
		}
		if !reflect.DeepEqual(got, tc.want) {
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
		{"Celsius wins over Fahrenheit, exact",
			micom.CarClimate{DriverSetF: ptr(71), DriverSetHalfC: ptr(43), PassengerSetF: ptr(72), PassengerSetHalfC: ptr(0)},
			map[string]any{"source": "car", "driverTargetC": 21.5}},
		{"LO and HI are LIVI's ends",
			micom.CarClimate{DriverSetHalfC: ptr(micom.SetPointLo), PassengerSetF: ptr(micom.SetPointHi)},
			map[string]any{"source": "car", "driverTargetC": 16.0, "passengerTargetC": 28.0}},
		{"fan level 8 is LIVI's 7", micom.CarClimate{FanLevel: ptr(8), FanAuto: ptr(false)},
			map[string]any{"source": "car", "fanLevel": 7, "fanAuto": false, "on": true}},
		{"fan off", micom.CarClimate{FanLevel: ptr(0), FanAuto: ptr(false)},
			map[string]any{"source": "car", "fanLevel": 0, "fanAuto": false, "on": false}},
		{"the car's own on/off over the fan's, rear defrost",
			micom.CarClimate{FanLevel: ptr(0), FanAuto: ptr(false), On: ptr(true), RearDefrost: ptr(true)},
			map[string]any{"source": "car", "fanLevel": 0, "fanAuto": false, "on": true, "rearWindowHeating": 1}},
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
