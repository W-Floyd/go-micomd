package micom

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Frames from the logcat capture of 2026-10-16 20:01 (MicomD lines).
var captured = []string{
	"ff 88 0c 01 0c 5c 00 01 2b",
	"ff 8f 0d 01 0d 03 00 0a ff ff ff 0e 03 ff ff 03 ff 76",
	"ff 88 0c 01 0c 09 00 06 04 a0 01 10 80 4c",
	"ff c2 43 00 43 33 00 0c 02 02 14 01 10 0a 01 1a 10 00 02 04",
	"ff 0c 88 01 8c 02 00 04 57 00 54 f3",
	"ff 0c 88 01 8c 55 00 09 f8 b3 97 48 ff 55 05 00 91",
	"ff 0c 88 01 8c 5c 00 1f 11 ff ff ff ff ff ff ff ff ff 02 db 00 00 00 00 00 00 00 00 00 00 21 00 53 00 00 00 00 ff 0f",
}

func TestDecodeEncodeCaptured(t *testing.T) {
	for _, s := range captured {
		raw := unhex(t, s)
		var d Decoder
		fs := d.Write(raw)
		if len(fs) != 1 || d.BadSum != 0 || d.Dropped != 0 {
			t.Fatalf("%s: %d frames, badsum %d, dropped %d", s, len(fs), d.BadSum, d.Dropped)
		}
		if got := fs[0].Encode(); !bytes.Equal(got, raw) {
			t.Errorf("re-encode of %s = %x", s, got)
		}
	}
}

func TestProfilesMatchCapture(t *testing.T) {
	// 20:01:16 on 2026-10-16, when the captured clock frames were sent.
	now := time.Date(2026, 10, 16, 20, 1, 16, 0, time.UTC)
	want := map[string]string{
		"poll":      captured[0],
		"ack":       captured[1],
		"clock-sec": captured[2],
		"clock":     captured[3],
		// Not captured: ack with the compass as 3f, from stock code.
		"tbt-sub-info": "ff 8f 0d 01 0d 03 00 0a ff ff ff 3f 03 ff ff 03 ff 47",
	}
	code := defaultVariant().CodeProfile()
	for _, p := range slices.Concat(DefaultProfile(), ClockProfile(), code) {
		w, ok := want[p.Name]
		if !ok {
			t.Fatalf("no capture for %s", p.Name)
		}
		if got := p.Build(now).Encode(); !bytes.Equal(got, unhex(t, w)) {
			t.Errorf("%s: built %x, captured %s", p.Name, got, w)
		}
	}
	// -code-profile sends the corrected frame instead of, not beside, ack.
	names := []string{}
	for _, p := range code {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"poll", "tbt-sub-info"}) {
		t.Errorf("code profile %v", names)
	}
}

func TestDecoderResyncs(t *testing.T) {
	good := unhex(t, captured[4])
	bad := append([]byte(nil), unhex(t, captured[5])...)
	bad[len(bad)-1] ^= 0x01 // corrupt the checksum
	stream := append(append(append([]byte{0x00, 0x13, 0xff}, bad...), []byte{0x7a}...), good...)
	var d Decoder
	var got []Frame
	for i := 0; i < len(stream); i += 3 { // arrives in pieces
		end := min(i+3, len(stream))
		got = append(got, d.Write(stream[i:end])...)
	}
	if len(got) != 1 || got[0].Cmd != 0x02 {
		t.Fatalf("got %v, want the one good frame", got)
	}
	if d.BadSum != 1 {
		t.Errorf("BadSum = %d, want 1", d.BadSum)
	}
}

func TestSendAllowlist(t *testing.T) {
	a, b := net.Pipe()
	d := New(Config{Port: a})
	defer b.Close()
	if err := d.Send(Frame{Hdr: [4]byte{1, 2, 3, 4}, Cmd: 0x99}); err == nil {
		t.Fatal("a frame outside the profile was sent")
	}
	// What the profile builds is allowed (the payload may differ in time).
	go func() {
		buf := make([]byte, 64)
		b.Read(buf)
	}()
	if err := d.Send(Frame{Hdr: hdrPoll, Cmd: 0x5c}); err != nil {
		t.Fatalf("a profile frame was refused: %v", err)
	}
	// The clock frames are not in the default profile.
	if err := d.Send(Frame{Hdr: hdrClock, Cmd: 0x33}); err == nil {
		t.Fatal("a clock frame was sent without the clock profile")
	}
}

func TestDescribeLive(t *testing.T) {
	// Frames seen live on the stage, 2026-10-16; the variant-code replies are
	// the EV6 and a Korean micom build's, in emulation.
	for _, c := range []struct{ hdr, cmd, payload, want string }{
		{"018a0181", "17", "cc01a2180080 00 656e 00000000", "Vehicle=cvev Country=us Navigation=1 Modem=0 Internal amp=1 Rear monitor=0 Radio region=khz_10_200 DMB=0"},
		{"018a0181", "17", "cc01a2180080 00 656e 00000000", "External amp=actune Camera=avm"},
		{"018a0181", "17", "cc01a2180080 00 656e 00000000", "Telematics=0 Diranarom=1 DAB=0 HU type=daudio Camera mode=analog"},
		{"018a0181", "17", "cc01a2180080 00 656e 00000000", "RDS=0 DVD region=worldwide Language=en CPU variant 9=0 CPU variant 10=0 CPU variant 11=0 ADM=off"},
		{"018a0181", "17", "c800a0180080 00 656e 00000080", "Vehicle=200 Country=korea"},
		{"018a0181", "17", "c800a0180080 00 656e 00000080", "ADM=128"},
		{"018a0181", "17", "c800a0180080 00 6b6f 00000000", "Language=ko"},
		{"018a0181", "17", "c800a0180080 00 0000 00000000", "Language=n/a"},
		{"0c88018c", "02", "5700f5", "Climate power draw=245 W"},
		{"0c88018c", "02", "2a00f308", "Range, climate: Range, climate on=243 km Range added, climate off=8 km"},
		{"0c88018c", "02", "25016b07", "Range, climate on=363 km Range added, climate off=7 km"},
		// Live, 2026-11-12: the EV6's bulk reply, 182 bytes, while charging at 7.1 kW.
		{"0c88018c", "01", "003cffff003c02ff02001d000000ffffff030000330c9e060405a0ffff014f021201002bff32ff0001010006d78101a001a0c8c81e0000ff0000ffff00610001ff0000209fffffffffffff972cffffffffffffff0101000108010100000001ffff00ba0000000000000000000000010028ffff002e00ff0500000000ff0000000047010000000000000000580a02000000000000000000000000000000000000000000000520ffffffffffff7fff00ff00ffffffffff", "HEV status: Range, climate on="},
		{"0c88018c", "01", "003cffff003c02ff02001d000000ffffff030000330c9e060405a0ffff014f021201002bff32ff0001010006d78101a001a0c8c81e0000ff0000ffff00610001ff0000209fffffffffffff972cffffffffffffff0101000108010100000001ffff00ba0000000000000000000000010028ffff002e00ff0500000000ff0000000047010000000000000000580a02000000000000000000000000000000000000000000000520ffffffffffff7fff00ff00ffffffffff", "Charge limit, AC=100 % Charge limit, DC=100 % Charging power=7.1 kW"},
		{"0c88018c", "5c", "15ffffffffffffffffff01b1000000000000000000000400ff00000000ff", "Climate total=433 W"},
		{"0c88018c", "5c", "15ffffffffffffffffff01b1000000000000000000000400ff00000000ff", "Climate power=255 W"},
		{"0c88018c", "5c", "15ffffffffffffffffff01b1000000000000000000000400ff00000000ff", "AVN motor power=n/a"},
		{"0c88018c", "55", "4d749148ff550500", "Departure 1 hour=8"},
		// Live, 2026-11: both 12:00, no days, departure 1 off.
		{"0c88018c", "55", "f52c350c0c000000", "Departure timers: Departure 1 hour=12 Departure 1 minute bits=0 Departure 2 hour=12 Departure 2 minute bits=0 Departure 1 Monday=0 Departure 1 Tuesday=0 Departure 1 Wednesday=0 Departure 1 Thursday=0 Departure 1 enabled=0 Departure 1 Friday=0 Departure 1 Saturday=0 Departure 1 Sunday=0 Departure 2 Monday=0"},
		// Synthetic, stock's layout: departure 1 07:30 Monday to Friday, on.
		{"0c88018c", "55", "f52c35670c550500", "Departure 1 hour=7 Departure 1 minute bits=3 Departure 2 hour=12 Departure 2 minute bits=0 Departure 1 Monday=1 Departure 1 Tuesday=1 Departure 1 Wednesday=1 Departure 1 Thursday=1 Departure 1 enabled=1 Departure 1 Friday=1 Departure 1 Saturday=0 Departure 1 Sunday=0 Departure 2 Monday=0"},
		{"0c88018c", "4f", "2d3a200100000000", "Charger ready=1 On-board charger charging=0"},
		// Live, 2026-11-12, an AC charge at 7.2 kW: starting, then charging.
		{"0c88018c", "4f", "093a351100000000", "Charger ready=1 On-board charger charging=0"},
		{"0c88018c", "4f", "128e515100000000", "Charger ready=1 On-board charger charging=1"},
		{"0c88018c", "02", "5b0048", "Charging power: Charging power=7.2 kW"},
		{"0c88018c", "02", "5b0000", "Charging power=0 kW"},
		{"0c88018c", "02", "1b0001", "Charging cable: Charging cable connected=1"},
		{"0c88018c", "02", "140280", "Charge time to full=640 min"},
		{"0c88018c", "02", "140028", "Charge time to full: Charge time to full=40 min"},
		{"0c88018c", "02", "0e001f", "Charge time estimate, DC=31 min"},
		{"0c88018c", "02", "1300dc", "Charge time estimate, AC=220 min"},
		{"0c88018c", "02", "61001e", "Charge time estimate, DC custom=30 min"},
		{"0c88018c", "02", "2100a0", "Charge limit, AC=80 %"},
		{"0c88018c", "02", "2200c8", "Charge limit, DC=100 %"},
		{"0c88018c", "02", "1f019e", "Range at charge limit, AC=414 km"},
		{"0c88018c", "02", "200190", "Range at charge limit, DC=400 km"},
		{"0c88018c", "02", "5e0168", "Trip energy=36 kWh"},
		{"0c88018c", "02", "060029", "Average efficiency=4.1"},
		// Synthetic: the battery conditioning items were never seen.
		{"0c88018c", "02", "630002", "Battery conditioning mode: Battery conditioning mode=on"},
		{"0c88018c", "02", "630001", "Battery conditioning mode=off"},
		{"0c88018c", "02", "630000", "Battery conditioning mode=default"},
		{"0c88018c", "02", "790001", "Battery conditioning failure: Battery conditioning failure=range"},
		{"0c88018c", "02", "790002", "Battery conditioning failure=temperature_optimal"},
		{"0c88018c", "02", "790003", "Battery conditioning failure=heat_management"},
		{"0c88018c", "01", "00000000000000000000290000000000000000001f000000000000000000dc002800000000000000000000000000019e0190a0c800000a000000000000f300000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000000000000000000001", "HEV status (full): Range, climate on=243 km"},
		{"0c88018c", "01", "00000000000000000000290000000000000000001f000000000000000000dc002800000000000000000000000000019e0190a0c800000a000000000000f300000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000000000000000000000001", "Charge limit, DC=100 % Battery conditioning mode=on Battery conditioning failure=range"},
		{"0c88018c", "01", "003cffff003c02ff02001d000000ffffff030000330c9e060405a0ffff014f021201002bff32ff0001010006d78101a001a0c8c81e0000ff0000ffff00610001ff0000209fffffffffffff972cffffffffffffff0101000108010100000001ffff00ba0000000000000000000000010028ffff002e00ff0500000000ff0000000047010000000000000000580a02000000000000000000000000000000000000000000000520ffffffffffff7fff00ff00ffffffffff", "Charge time to full=530 min"},
		// The micom firmware's power and display frames, in emulation.
		{"018a0181", "08", "000400", "Power state: Power state=acc_off_hold"},
		{"018a0181", "08", "000300", "Power state: Power state=bus_sleep"},
		{"018a0181", "08", "000100", "Power state: Power state=power_off_request"},
		{"018a0181", "08", "000000", "Power state: Power state=rail_off"},
		{"018a0181", "08", "0105", "Power state (ACC on): Power state=acc_on"},
		{"018a0181", "33", "01", "ACC edge: ACC=off"},
		{"018a0181", "33", "02", "ACC edge: ACC=on"},
		{"018a0181", "0a", "01", "IGN1=1"},
		{"018a0181", "0b", "00", "Vehicle ready=0"},
		{"018a0181", "0c", "00", "Charge warning=0"},
		{"018a0181", "0d", "00", "Illumination: Illumination=night"},
		{"018a0181", "0d", "01", "Illumination=day"},
		{"018a0181", "0d", "05", "Illumination=5"},
		// The EV6's, captured at night, its first 0x13 bytes.
		{"018a0181", "01", "0100000000010eca020519222e06544b4b1500", "Boot record: Boot mode=1 Reset reason=0 Illumination=night Dim mode=auto"},
		// Synthetic: a whole boot record, woken to charge, in D at night.
		{"018a0181", "01", "08000100010000ca020000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000200000000000000000001000000000000000000000000000000000001", "Boot record (full): Boot mode=8 Reset reason=0 Driving regulation=1 Illumination=day ACC=1 IGN1=1 Vehicle ready=0 IGN3=1 Reverse=0 Parking brake=1 Dim mode=auto CAN DB generation=2 Light sensor fitted=1 Driving regulation B=1"},
		{"018a0181", "89", "000002ff", "Regulation inputs: In P=0 Parking brake lever=0 EPB state=2"},
		// Synthetic: the start-up reads' replies.
		{"43c200c3", "00", "0000100000000000000000000000000100000500000000000000000000000000000000000000000000000000000000000000000000000000000000", "Climate snapshot: Snapshot climate on (OpSts)=1 Snapshot driver set point (C raw)=16 Snapshot fan (raw)=5"},
		{"0b8e018b", "c7", "c70100", "B-CAN validity: B-CAN valid=1 MM-CAN=0"},
		{"44c100c4", "0c", "00000001000000000000000000000000000000570000000000000000000000000000000000", "Body snapshot: Snapshot driver door open=1 Snapshot driver door lock=0 Snapshot 12 V charge=87 %"},
		{"11920191", "11", "fa0201000000000000", "Units (USM): Distance unit code=2 Temperature unit code=1"},
		{"018a0181", "45", "0a", "Rheostat level=10"},
		{"018a0181", "9b", "0000000000000058", "12 V battery charge=88 %"},
		{"44c100c4", "14", "57", "12 V battery charge (body)=87 %"},
		{"088c0188", "0c", "02", "Display 0x880C=2"},
		{"088c0188", "10", "14", "Rheostat: Rheostat step=20"},
		{"088c0188", "13", "32", "Display 0x8813=50"},
		{"088c0188", "1b", "00c8", "Current brightness=200"},
		{"088c0188", "1b", "00c80123", "Current brightness=200"},
		// A drive against an OBD2 logger.
		{"44c100c4", "20", "0086", "Vehicle speed: Vehicle speed=107.826048 km/h Vehicle speed (mph)=67 mph"},
		{"44c100c4", "20", "0085", "Vehicle speed (mph)=66.5 mph"},
		{"44c100c4", "20", "0000", "Vehicle speed=0 km/h"},
		{"44c100c4", "53", "01", "Battery conditioning=1"},
		{"0d8f018d", "47", "9b166f54000066", "Battery: State of charge=77.5 % Battery minimum temperature=22 °C Battery remaining energy=57 kWh Battery inlet temperature=11 °C"},
		{"0d8f018d", "47", "c8fbffff000050", "State of charge=100 % Battery minimum temperature=-5 °C Battery remaining energy=131.07 kWh Battery inlet temperature=0 °C"},
		// As captured, all 11 bytes.
		{"0d8f018d", "47", "ac12780f00005f00015907", "State of charge=86 % Battery minimum temperature=18 °C Battery remaining energy=61.47 kWh Battery inlet temperature=7.5 °C"},
		{"018a0181", "31", "0077", "Vehicle speed (whole km/h)=119 km/h"},
		{"40c780c0", "ad", "415bcc", "OBD pack remaining=80 %"},
		{"0d8f018d", "46", "02", "Cruise control: Cruise control=active"},
		{"0d8f018d", "46", "00", "Cruise control=inactive"},
		// Against the car's cluster.
		{"0d8f018d", "43", "02007500bc", "Range: Range=188 km Range (mi)=117 mi"},
		{"0d8f018d", "2d", "f36382000000000000000000000000000000000203000020ffff", "Speed limit: Speed limit=0 mph"},
		{"0d8f018d", "2d", "34ba6e006e6e00000000000000003c00000203000020ffff", "Speed limit: Speed limit=60 mph"},
		{"44c100c4", "22", "06c40c", "Odometer: Odometer=44340.4 km"},
		{"43c200c3", "02", "5e", "Outdoor temperature: Outdoor temperature=12.222222 C Outdoor temperature (F)=54 F"},
		{"43c200c3", "02", "00", "Outdoor temperature=-40 C Outdoor temperature (F)=-40 F"},
		// Gear shifts, live on the EV6, 2026-11-10 (P/D/N/R, each more than
		// once; see variants/kia-ev6-gen5w.yaml's comments for the full
		// correlation — not yet written up in docs/messages.md).
		{"50c400d0", "4a", "00", "Gear: park"},
		{"50c400d0", "4a", "05", "Gear: drive"},
		{"50c400d0", "4a", "06", "Gear: neutral"},
		{"50c400d0", "4a", "07", "Gear: reverse"},
		{"018a0181", "03", "01", "Driving regulation: on"},
		{"018a0181", "03", "00", "Driving regulation: off"},
		{"018a0181", "26", "01", "Out of park: on (gear status)"},
		{"018a0181", "9a", "01", "Driving regulation B: on"},
		{"018a0181", "29", "00", "Parking brake: off"},
		{"018a0181", "95", "01", "Light sensor: day"},
		{"018a0181", "95", "02", "Light sensor: night"},
		{"43c200c3", "12", "01", "Climate: on"},
		{"43c200c3", "12", "02", "Climate: off"},
		{"43c200c3", "16", "01", "Climate key: pressed"},
		{"44c100c4", "49", "02", "Charge port: closed"},
		{"44c100c4", "49", "01", "Charge port: open"},
		{"44c100c4", "0e", "01", "Rear defrost: on"},
		{"44c100c4", "0d", "01", "Steering wheel heat: state 1"},
		{"44c100c4", "1d", "07", "Seat heat, rear left: heat level 2"},
		{"44c100c4", "1f", "02", "Seat heat, rear right: off"},
		{"44c100c4", "10", "00", "Out of park: off (body node)"},
		{"018a0181", "1b", "01", "Reverse: on"},
		{"018a0181", "1b", "00", "Reverse: off"},
		{"01c40181", "51", "01", "Rear camera: on"},
	} {
		var f Frame
		copy(f.Hdr[:], unhex(t, c.hdr))
		f.Cmd = unhex(t, c.cmd)[0]
		f.Payload = unhex(t, c.payload)
		if got := Describe(f); !strings.Contains(got, c.want) {
			t.Errorf("%s %s: %q lacks %q", c.hdr, c.cmd, got, c.want)
		}
	}
	if Describe(Frame{Hdr: [4]byte{0x0d, 0x8f, 0x01, 0x8d}, Cmd: 0x2d}) != "" {
		t.Error("unknown frame described")
	}
}

// TestFrameNameKnowsSignals: FrameName used to check only v.names and
// v.decoders, so a signals: frame (door locks, climate switches, ...)
// described fine but came back "" from FrameName — live traffic surfaced
// this at a gear change, 2026-11-10 (the door locks firing on Park->Drive).
func TestFrameNameKnowsSignals(t *testing.T) {
	for _, c := range []struct{ hdr, cmd, payload, name string }{
		{"44c100c4", "00", "00", "door-front-right-lock"},
		{"44c100c4", "02", "01", "door-driver-lock"},
		{"43c200c3", "05", "03", "climate-driver-set-c"},
		{"43c200c3", "07", "03", "climate-driver-set"},
		{"50c400d0", "4a", "05", "gear"},
		{"018a0181", "03", "01", "driving-regulation"},
		{"018a0181", "26", "01", "gear-status"},
		{"018a0181", "9a", "01", "driving-regulation-b"},
		{"44c100c4", "10", "01", "out-of-park-body"},
		{"018a0181", "1b", "01", "reverse-active"},
		{"018a0181", "29", "01", "parking-brake"},
	} {
		var f Frame
		copy(f.Hdr[:], unhex(t, c.hdr))
		f.Cmd = unhex(t, c.cmd)[0]
		f.Payload = unhex(t, c.payload)
		if got := FrameName(f); got != c.name {
			t.Errorf("%s %s: FrameName %q, want %q", c.hdr, c.cmd, got, c.name)
		}
		if Describe(f) == "" {
			t.Errorf("%s %s: named but not described", c.hdr, c.cmd)
		}
	}
}

// TestDescribeSentLive mirrors TestDescribeLive for SentName/DescribeSent:
// FrameName/Describe never name these frames (compileMessages builds the
// decoders of from_micom messages, the encoders of to_micom ones), so this
// is the other table, not more rows of that one.
func TestDescribeSentLive(t *testing.T) {
	for _, c := range []struct{ hdr, cmd, payload, name, want string }{
		// captured[1]: the ack profile frame (ClusterService), idle (no route).
		{"8f0d010d", "03", "ffffff0e03ffff03ff", "cluster-sub-info", "remaining_unit=n/a"},
		// The EV6, live, 2026-11-10 (no active turn-by-turn guidance).
		{"8f0d010d", "04", "08000000ffff00f0", "tbt-turn", "maneuver=no_route_guide"},
		{"8f0d010d", "11", "00", "nav-operation", "status=released"},
		// Synthetic: 0,0 and 1 degree north, 1 degree west.
		{"c34d004d", "17", "07ffffff07ffffff", "cluster-position", "lat_arcsec=0"},
		{"c34d004d", "17", "08057e3f07fa81bf", "cluster-position", "lon_arcsec=-3600"},
	} {
		var f Frame
		copy(f.Hdr[:], unhex(t, c.hdr))
		f.Cmd = unhex(t, c.cmd)[0]
		f.Payload = unhex(t, c.payload)
		if got := SentName(f); got != c.name {
			t.Errorf("%s %s: SentName %q, want %q", c.hdr, c.cmd, got, c.name)
		}
		if got := DescribeSent(f); !strings.Contains(got, c.want) {
			t.Errorf("%s %s: %q lacks %q", c.hdr, c.cmd, got, c.want)
		}
	}
	// A from_micom frame: SentName/DescribeSent never fall back to
	// FrameName/Describe's table.
	rx := Frame{Hdr: [4]byte{0x0d, 0x8f, 0x01, 0x8d}, Cmd: 0x46}
	if got := SentName(rx); got != "" {
		t.Errorf("from_micom frame named as sent: %q", got)
	}
}

func TestSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 16, 22, 0, 0, 0, time.UTC)
	d := New(Config{Port: nil, Now: func() time.Time { return now }})
	frame := func(hdr, cmd, payload string) Frame {
		var f Frame
		copy(f.Hdr[:], unhex(t, hdr))
		f.Cmd = unhex(t, cmd)[0]
		f.Payload = unhex(t, payload)
		return f
	}
	d.telem.add(frame("0c88018c", "02", "5700f5"), now.Add(-2*time.Second))
	d.telem.add(frame("0c88018c", "02", "5700ff"), now.Add(-time.Second))
	d.telem.add(frame("0d8f018d", "2d", "00"), now)
	d.telem.add(frame("018a0181", "0f", ""), now.Add(-3*time.Second))
	d.telem.add(frame("0c88018c", "55", "f52c350c0c000000"), now.Add(-3*time.Second)) // live, 2026-11
	s := d.Snapshot()
	got := map[string]Reading{}
	for _, r := range s.Readings {
		got[r.Name] = r
	}
	if r := got["Climate power draw"]; r.Value != 255 || r.AgeMs != 1000 {
		t.Errorf("readings = %+v, want the latest climate power, 1 s old", s.Readings)
	}
	// One 0x8C55 gives its times and its days.
	for name, want := range map[string]int{"Departure 1 hour": 12, "Departure 2 hour": 12, "Departure 1 Monday": 0, "Departure 1 enabled": 0, "Departure 2 Thursday": 0} {
		if r, ok := got[name]; !ok || r.Value != want || !r.Valid {
			t.Errorf("%s = %+v (%v), want %d", name, r, ok, want)
		}
	}
	if s.LastAliveAgeMs != 3000 {
		t.Errorf("LastAliveAgeMs = %d, want 3000", s.LastAliveAgeMs)
	}
	if len(s.Frames) != 4 || s.Frames[0].Key != "0c88018c 02" || s.Frames[0].Count != 2 || s.Frames[0].Name != "Status flag" {
		t.Errorf("frames = %+v", s.Frames)
	}
}

func TestMarkAndDiffs(t *testing.T) {
	now := time.Date(2026, 10, 16, 22, 0, 0, 0, time.UTC)
	d := New(Config{Now: func() time.Time { return now }})
	frame := func(payload string) Frame {
		f := Frame{Hdr: [4]byte{0x0d, 0x8f, 0x01, 0x8d}, Cmd: 0x2d}
		f.Payload = unhex(t, payload)
		return f
	}
	diffsOf := func() []ByteDiff {
		for _, f := range d.Snapshot().Frames {
			if f.Key == "0d8f018d 2d" {
				return f.Diffs
			}
		}
		t.Fatal("no frame")
		return nil
	}
	// Before a mark: a counter in byte 0 changes with every frame.
	for i, p := range []string{"01 00 00", "02 00 00", "03 00 00"} {
		d.telem.add(frame(p), now.Add(time.Duration(i)*time.Second))
	}
	if s := d.Snapshot(); s.MarkedAgeMs != -1 {
		t.Errorf("MarkedAgeMs = %d before a mark", s.MarkedAgeMs)
	}
	if got := diffsOf(); len(got) != 1 || got[0].Idx != 0 || got[0].Changes != 2 || got[0].From != -1 {
		t.Errorf("diffs before a mark = %+v", got)
	}
	// Mark, then the counter moves again and byte 2 changes once.
	now = now.Add(10 * time.Second)
	d.Mark()
	if got := diffsOf(); len(got) != 0 {
		t.Errorf("diffs right after the mark = %+v", got)
	}
	d.telem.add(frame("04 00 00"), now.Add(time.Second))
	d.telem.add(frame("05 00 07"), now.Add(2*time.Second))
	d.telem.add(frame("06 00 07"), now.Add(3*time.Second))
	now = now.Add(5 * time.Second)
	got := diffsOf()
	if len(got) != 2 {
		t.Fatalf("diffs = %+v", got)
	}
	if b := got[0]; b.Idx != 0 || b.From != 3 || b.To != 6 || b.Changes != 3 {
		t.Errorf("counter byte = %+v", b)
	}
	if b := got[1]; b.Idx != 2 || b.From != 0 || b.To != 7 || b.Changes != 1 || b.AgeMs != 3000 {
		t.Errorf("switch byte = %+v", b)
	}
	if s := d.Snapshot(); s.MarkedAgeMs != 5000 {
		t.Errorf("MarkedAgeMs = %d, want 5000", s.MarkedAgeMs)
	}
}

// The table says which variant runs and why, and gives each frame kind one
// line: key, name, count, age, payload, changed bytes.
func TestSnapshotTable(t *testing.T) {
	counter := FrameCount{Key: "0d8f018d 2d", Count: 3, AgeMs: 1500, Payload: "060007",
		Diffs: []ByteDiff{{Idx: 0, From: 3, To: 6, Changes: 3}, {Idx: 2, From: 0, To: 7, Changes: 1}}}
	climate := FrameCount{Key: "0c88018c 02", Name: "Status flag", Count: 40, AgeMs: 100, Payload: "5700ff", Diffs: []ByteDiff{}}
	unmarked := FrameCount{Key: "0d8f018d 2d", Count: 3, AgeMs: 0, Payload: "06",
		Diffs: []ByteDiff{{Idx: 0, From: -1, To: 6, Changes: 2}}}
	ev6ID := Identity{"vehicle_info": 0xcc, "country_info": 1}
	for _, tc := range []struct {
		name string
		s    Snapshot
		want []string
	}{
		{"marked", Snapshot{Up: true, RxFrames: 43, TxFrames: 9, AliveRx: 4, AliveTx: 4, LastAliveAgeMs: 500, MarkedAgeMs: 5000,
			Frames:  []FrameCount{climate, counter},
			Variant: VariantChoice{Name: DefaultVariantName, Source: "cmdline", Cmdline: ev6ID, Matched: DefaultVariantName}},
			[]string{
				"variant kia-ev6-gen5w (kernel command line country_info=0x1 vehicle_info=0xcc)",
				"link up rx 43 tx 9 alive 4/4, last 0.5s ago badsum 0 dropped 0",
				"mark 5.0s ago",
				"KEY NAME COUNT AGE PAYLOAD CHANGED",
				"0c88018c 02 Status flag 40 0.1s 5700ff",
				"0d8f018d 2d - 3 1.5s 060007 0:03->06×3 2:00->07×1",
			}},
		{"no mark, default", Snapshot{LastAliveAgeMs: -1, MarkedAgeMs: -1, Frames: []FrameCount{unmarked},
			Variant: VariantChoice{Name: DefaultVariantName, Source: "default"}},
			[]string{
				"variant kia-ev6-gen5w (default: no variant matches (no identity keys))",
				"link down rx 0 tx 0 alive 0/0, last never badsum 0 dropped 0",
				"mark none: changes since the start",
				"0d8f018d 2d - 3 0.0s 06 0:??->06×2",
			}},
		{"flag over the command line", Snapshot{MarkedAgeMs: -1, Variant: VariantChoice{Name: "hkmc-gen5w", Source: "flag", Cmdline: ev6ID, Matched: DefaultVariantName}},
			[]string{"variant hkmc-gen5w (-variant; the kernel command line matches kia-ev6-gen5w)"}},
		{"keepalive only", Snapshot{MarkedAgeMs: -1, Variant: VariantChoice{Name: "none", Source: "file", File: "/x.yaml", Note: "open /x.yaml: no such file"}},
			[]string{"variant none (from /x.yaml; open /x.yaml: no such file)"}},
		{"micom record", Snapshot{MarkedAgeMs: -1, Variant: VariantChoice{Name: DefaultVariantName, Source: "default",
			Reply: &VariantReply{Payload: "cc01", AgeMs: 2000, Fields: []Field{{Name: "Vehicle", Value: 0xcc, Valid: true}}, Identity: ev6ID, Matched: DefaultVariantName, Verdict: "the micom reports it"}}},
			[]string{"micom the micom reports it", "record 2.0s ago: cc01 Vehicle=204"}},
		{"recording", Snapshot{MarkedAgeMs: -1, Recorder: &RecorderState{Path: "/var/log/micom.jsonl", Bytes: 4096, Rotations: 2, Dropped: 1}},
			[]string{"recording /var/log/micom.jsonl 4096 bytes rotations 2 dropped 1"}},
		{"recording stopped", Snapshot{MarkedAgeMs: -1, Recorder: &RecorderState{Path: "/r", Error: "disk full"}},
			[]string{"recording /r 0 bytes rotations 0 dropped 0 stopped: disk full"}},
	} {
		var b bytes.Buffer
		if err := tc.s.WriteTable(&b); err != nil {
			t.Fatal(err)
		}
		lines := map[string]bool{}
		for _, l := range strings.Split(b.String(), "\n") {
			lines[strings.Join(strings.Fields(l), " ")] = true
		}
		for _, w := range tc.want {
			if !lines[w] {
				t.Errorf("%s: no line %q in\n%s", tc.name, w, b.String())
			}
		}
	}
}

// The identify request goes out once the link is up, and again until the
// variant record arrives, a bounded number of times.
func TestAskVariant(t *testing.T) {
	req, err := ParseHexFrame("ff 8a 01 01 01 17 00 01 62")
	if err != nil {
		t.Fatal(err)
	}
	record := Frame{Hdr: hdrAliveReq, Cmd: 0x17, Payload: unhex(t, "cc 01 00 00 00 00 00 65 6e 00 00 00 00")}
	for _, tc := range []struct {
		name     string
		linkUp   bool
		answerAt int // the request the micom answers, 0 for none
		want     int
		log      string
	}{
		{"answered at once", true, 1, 1, ""},
		{"answered on a retry", true, 2, 2, ""},
		{"never answered", true, 0, 3, "no variant record after 3 requests"},
		{"link never up", false, 1, 0, ""},
	} {
		a, b := net.Pipe()
		var logs syncBuffer
		d := New(Config{Port: a, Profile: []Periodic{}, Allow: []Frame{req}, Logf: func(f string, args ...any) { fmt.Fprintf(&logs, f+"\n", args...) }})
		if tc.linkUp {
			d.lastRx = time.Now()
		}
		var sent atomic.Int32
		go func() {
			var dec Decoder
			buf := make([]byte, 64)
			for {
				n, err := b.Read(buf)
				if err != nil {
					return
				}
				for _, f := range dec.Write(buf[:n]) {
					if sameFrame(f, req) && int(sent.Add(1)) == tc.answerAt {
						d.telem.add(record, time.Now())
					}
				}
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if !tc.linkUp {
			cancel()
		}
		d.askVariant(ctx, req, 3, 50*time.Millisecond)
		cancel()
		b.Close()
		if got := int(sent.Load()); got != tc.want {
			t.Errorf("%s: %d requests, want %d", tc.name, got, tc.want)
		}
		if !strings.Contains(logs.String(), tc.log) {
			t.Errorf("%s: logged %q, want %q", tc.name, logs.String(), tc.log)
		}
	}
}

func TestAliveReplyBytes(t *testing.T) {
	// Derived from SystemService.respondCPUAlive, not captured.
	if got, want := AliveReply(0).Encode(), unhex(t, "ff 8a 01 01 01 0f 00 02 00 79"); !bytes.Equal(got, want) {
		t.Errorf("AliveReply(0) = %x, want %x", got, want)
	}
	req := Frame{Hdr: hdrAliveReq, Cmd: 0x0f, Payload: []byte{0}}
	if !IsAliveRequest(req) || IsAliveRequest(Frame{Hdr: hdrPoll, Cmd: 0x0f}) {
		t.Error("IsAliveRequest")
	}
}

func TestAnswersAliveRequests(t *testing.T) {
	a, b := net.Pipe()
	d := New(Config{Port: a, Profile: []Periodic{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	req := Frame{Hdr: hdrAliveReq, Cmd: 0x0f, Payload: []byte{0}}.Encode()
	var dec Decoder
	buf := make([]byte, 64)
	for want := byte(0); want < 3; want++ {
		if _, err := b.Write(req); err != nil {
			t.Fatal(err)
		}
		b.SetReadDeadline(time.Now().Add(time.Second))
		n, err := b.Read(buf)
		if err != nil {
			t.Fatalf("no reply: %v", err)
		}
		fs := dec.Write(buf[:n])
		if len(fs) != 1 || fs[0].Hdr != hdrAliveReply || fs[0].Cmd != 0x0f || len(fs[0].Payload) != 1 || fs[0].Payload[0] != want {
			t.Fatalf("reply %d: %v", want, fs)
		}
	}
	deadline := time.Now().Add(time.Second)
	for d.Stats().AliveTx != 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s := d.Stats(); s.AliveRx != 3 || s.AliveTx != 3 {
		t.Errorf("alive counters %d/%d, want 3/3", s.AliveRx, s.AliveTx)
	}
	cancel()
	<-done
}

func TestRunSendsAndReceives(t *testing.T) {
	a, b := net.Pipe()
	now := time.Date(2026, 10, 16, 20, 1, 16, 0, time.UTC)
	seen := make(chan Frame, 8)
	d := New(Config{
		Port:    a,
		Profile: []Periodic{{Name: "poll", Every: 5 * time.Millisecond, Build: DefaultProfile()[0].Build}},
		Now:     func() time.Time { return now },
		OnFrame: func(f Frame) { seen <- f },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// The micom end: read what the daemon sends, send a countdown frame.
	go func() {
		b.Write(unhex(t, captured[4]))
	}()
	var dec Decoder
	buf := make([]byte, 256)
	var polls int
	deadline := time.After(2 * time.Second)
	for polls < 2 {
		b.SetReadDeadline(time.Now().Add(time.Second))
		n, err := b.Read(buf)
		if err != nil {
			t.Fatalf("micom end read: %v", err)
		}
		for _, f := range dec.Write(buf[:n]) {
			if f.Cmd == 0x5c {
				polls++
			}
		}
		select {
		case <-deadline:
			t.Fatal("no polls")
		default:
		}
	}
	select {
	case f := <-seen:
		if f.Cmd != 0x02 {
			t.Errorf("OnFrame got %v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("the countdown frame was not delivered")
	}
	if c := d.Stats().Countdown; c != 0x54 {
		t.Errorf("Countdown = %#x, want 0x54", c)
	}
	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Errorf("Run = %v", err)
	}
}
