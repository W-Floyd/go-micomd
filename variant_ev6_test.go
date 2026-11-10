package micom

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// What the device's systemd unit passes today
// (pmaports-kia-ev6 device/testing/device-mobis-g5w/mobis-g5w-micomd.service).
const (
	serviceKeyFunc = 0x8701
	serviceKeymap  = "2.2=163,2.3=165,2.4=164,2.5=113,2.6=115,2.7=114,2.8=183,2.10=184,2.11=191,0.1=113,0.2=165,0.3=163,0.4=187,0.5=102,1.1=28,1.2=188,1.3=189,1.4=190,1.5=192,8.1.up=185,8.1.down=186,9.1.up=115,9.1.down=114"
)

var serviceAllow = []string{
	"ff8a01010101000174", "ff85020102010003090070", "ff8502010201000312006b",
	"ff850201020b0003020071", "ff850201020b0003020170", "ff850201020b0003010072",
	"ff850201020b0003010173", "ff85020102380003020042", "ff850201020100030e0077",
	"ff85020102010003010078", "ff85020102030002[00-23]..",
}

func ev6(t *testing.T) *Variant {
	t.Helper()
	v, err := LoadVariant(DefaultVariantName)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// ev6Frames covers every header the EV6 code knows and a few it does not,
// every command, every one-byte payload, and samples of the longer ones.
func ev6Frames() []Frame {
	hdrs := []string{"07840187", "43c200c3", "55ca00d5", "44c100c4", "01c40181", "018a0181", "0c88018c", "0d8f018d", "8a010101", "0c88008c"}
	rnd := rand.New(rand.NewSource(1))
	var out []Frame
	for _, hs := range hdrs {
		var h [4]byte
		hex.Decode(h[:], []byte(hs))
		for c := 0; c < 256; c++ {
			add := func(p []byte) { out = append(out, Frame{Hdr: h, Cmd: byte(c), Payload: p}) }
			add(nil)
			for v := 0; v < 256; v++ {
				add([]byte{byte(v)})
			}
			for _, a := range []byte{0, 1, 2, 7, 8, 9, 10, 11, 0xff} {
				for _, b := range []byte{0, 0x01, 0x11, 0x1f, 0x21, 0x2b, 0x31, 0xff} {
					add([]byte{a, b})
					add([]byte{a, b, 0})
				}
			}
			for n := 3; n <= 33; n++ {
				p := make([]byte, n)
				rnd.Read(p)
				add(p)
				add(bytes.Repeat([]byte{0xff}, n))
			}
			add([]byte{0x57, 0x00, 0xf5})
			add([]byte{0x57, 0xff, 0xff})
			add([]byte{0x58, 0x00, 0xf5})
		}
	}
	return out
}

// The EV6 variant reads every frame as the Go tables did.
func TestEV6VariantDescribesAsBefore(t *testing.T) {
	v := ev6(t)
	bad := 0
	for _, f := range ev6Frames() {
		if a, b, c := legacyCarDescribe(f), v.describe(f), carDescribe(f); a != b || a != c {
			t.Errorf("%v: legacy carDescribe %q, variant %q, carDescribe %q", f, a, b, c)
			bad++
		}
		// What the Go tables knew must read the same; frames they knew
		// nothing of may be decoded since (the variant code, power state).
		added := legacyFrameName(f) == "" && legacyDescribe(f) == "" && legacyDecode(f) == nil
		if a, b, c := legacyDescribe(f), v.Describe(f), Describe(f); (a != b || a != c) && !added {
			t.Errorf("%v: legacy Describe %q, variant %q, Describe %q", f, a, b, c)
			bad++
		}
		if a, b, c := legacyDecode(f), v.Decode(f), Decode(f); (!reflect.DeepEqual(a, b) || !reflect.DeepEqual(a, c)) && !added {
			t.Errorf("%v: legacy Decode %v, variant %v, Decode %v", f, a, b, c)
			bad++
		}
		if a, b, c := legacyFrameName(f), v.FrameName(f), FrameName(f); (a != b || a != c) && !added {
			t.Errorf("%v: legacy FrameName %q, variant %q, FrameName %q", f, a, b, c)
			bad++
		}
		if bad > 20 {
			t.Fatal("too many differences")
		}
	}
}

// The EV6 variant folds every frame into the climate state as the Go code
// did, from an empty state and from a full one.
func TestEV6VariantAppliesAsBefore(t *testing.T) {
	v := ev6(t)
	full := CarClimate{
		DriverSetF: ptr(72), PassengerSetF: ptr(0), Airflow: ptr(byte(3)),
		Auto: ptr(true), AC: ptr(false), DriverOnly: ptr(true), Recirculation: ptr(false), ZonesSynced: ptr(true),
		FanLevel: ptr(4), FanAuto: ptr(false), DriverSeatHeat: ptr(2), PassengerSeatHeat: ptr(0),
	}
	bad := 0
	for _, f := range ev6Frames() {
		for _, start := range []CarClimate{{}, full} {
			a, b, c := start, start, start
			ea, eb, ec := legacyApply(&a, f), v.ApplyClimate(&b, f), c.Apply(f)
			if ea != eb || ea != ec || !reflect.DeepEqual(a, b) || !reflect.DeepEqual(a, c) {
				t.Errorf("%v: legacy Apply %d %+v, variant %d %+v, Apply %d %+v", f, ea, a, eb, b, ec, c)
				bad++
			}
		}
		if bad > 20 {
			t.Fatal("too many differences")
		}
	}
}

// The EV6 variant's profile is DefaultProfile's and ClockProfile's, frame
// for frame, at any time.
func TestEV6VariantProfileAsBefore(t *testing.T) {
	v := ev6(t)
	times := []time.Time{
		time.Date(2026, 10, 16, 20, 1, 16, 0, time.UTC),
		time.Date(2026, 11, 7, 20, 21, 16, 0, time.Local),
		time.Date(2031, 2, 28, 23, 59, 59, 0, time.UTC),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, pair := range []struct {
		name      string
		old, vari []Periodic
	}{
		{"default", legacyDefaultProfile(), v.Profile()},
		{"clock", legacyClockProfile(), v.ClockProfile()},
		{"DefaultProfile", legacyDefaultProfile(), DefaultProfile()},
		{"ClockProfile", legacyClockProfile(), ClockProfile()},
	} {
		if len(pair.old) != len(pair.vari) {
			t.Fatalf("%s: %d entries, variant %d", pair.name, len(pair.old), len(pair.vari))
		}
		for i, o := range pair.old {
			n := pair.vari[i]
			if o.Name != n.Name || o.Every != n.Every {
				t.Errorf("%s[%d]: %s every %v, variant %s every %v", pair.name, i, o.Name, o.Every, n.Name, n.Every)
			}
			for _, at := range times {
				// clock-sec's third byte was a fixed 01, the minute of the
				// capture it was read from; it is the minute now
				// (TestClockSecCarriesTheMinute).
				if o.Name == "clock-sec" && at.Minute() != 1 {
					continue
				}
				if a, b := o.Build(at).Encode(), n.Build(at).Encode(); !bytes.Equal(a, b) {
					t.Errorf("%s at %v: %x, variant %x", o.Name, at, a, b)
				}
			}
		}
	}
}

// The EV6 variant's key frame, labels and keymap are the code's and the
// unit's -keyfunc and -keymap.
func TestEV6VariantKeysAsBefore(t *testing.T) {
	v := ev6(t)
	if fn, ok := v.KeyFunc(); !ok || fn != serviceKeyFunc {
		t.Errorf("KeyFunc %#04x %v, want %#04x", fn, ok, serviceKeyFunc)
	}
	km, keys, err := parseKeyMap(serviceKeymap)
	if err != nil {
		t.Fatal(err)
	}
	vkm, vkeys := v.keymap()
	if !reflect.DeepEqual(km, vkm) {
		t.Errorf("keymap %v, variant %v", km, vkm)
	}
	slices.Sort(keys)
	vkeys = slices.Clone(vkeys)
	slices.Sort(vkeys)
	if !reflect.DeepEqual(keys, vkeys) {
		t.Errorf("keys %v, variant %v", keys, vkeys)
	}
	labels := map[[2]byte]string{}
	for k, l := range v.pushLabels {
		labels[k] = l
	}
	if !reflect.DeepEqual(legacyKeyLabels, labels) {
		t.Errorf("labels %v, variant %v", legacyKeyLabels, labels)
	}
	if want := map[byte]string{8: "Tune knob", 9: "Volume knob"}; !reflect.DeepEqual(v.knobLabels, want) {
		t.Errorf("knob labels %v, want %v", v.knobLabels, want)
	}
}

// The EV6 variant's send entries are exactly the unit's -allow: the frames
// byte for byte and the volume pattern range for range; without
// -allow-inferred only the captured ones.
func TestEV6VariantSendsAsService(t *testing.T) {
	v := ev6(t)
	var al allowList
	for _, s := range serviceAllow {
		if err := al.Set(s); err != nil {
			t.Fatal(err)
		}
	}
	hexes := func(fs []Frame) []string {
		var out []string
		for _, f := range fs {
			out = append(out, hex.EncodeToString(f.Encode()))
		}
		sort.Strings(out)
		return out
	}
	fs, ps := v.Sends(true, false)
	if a, b := hexes(al.frames), hexes(fs); !reflect.DeepEqual(a, b) {
		t.Errorf("frames:\n service %v\n variant %v", a, b)
	}
	if !reflect.DeepEqual(al.patterns, ps) {
		t.Errorf("patterns: service %+v, variant %+v", al.patterns, ps)
	}
	fs, ps = v.Sends(false, false)
	want := []string{"ff850201020b0003020170", "ff850201020100030e0077", "ff85020102010003010078", "ff85020102380003020042"}
	sort.Strings(want)
	if got := hexes(fs); !reflect.DeepEqual(got, want) {
		t.Errorf("captured frames %v, want %v", got, want)
	}
	if len(ps) != 1 || !reflect.DeepEqual(ps[0], al.patterns[0]) {
		t.Errorf("captured patterns %+v", ps)
	}
}

func TestMatchVariant(t *testing.T) {
	for _, tc := range []struct {
		name, cmdline, want string
	}{
		{"stock EV6", "daudio_hw_ver=526087 vehicle_info=0xcc country_info=0x01 cabin_camera_type=0", DefaultVariantName},
		{"EV6, other region", "vehicle_info=0xcc country_info=0x00", DefaultVariantName},
		{"another car", "vehicle_info=0x12 country_info=0x01", ""},
		{"read failed", "vehicle_info=0xff country_info=0xff", ""},
		{"no identity", "console=ttyAMA0", ""},
	} {
		got, err := MatchVariant(ParseIdentity(tc.cmdline))
		if err != nil || got != tc.want {
			t.Errorf("%s: %q %v, want %q", tc.name, got, err, tc.want)
		}
	}
	if !strings.HasPrefix(DefaultVariantName, "kia-ev6") {
		t.Error(DefaultVariantName)
	}
}

// clock-sec is ?? a0 MM SS 80: checked against the stock micomd's frames of
// 2026-11-07 (strace on its UART, the unit's clock 20:20:50 and 20:21:16),
// whose first byte, 02, differs from the 04 of the first capture and is
// left as it was.
func TestClockSecCarriesTheMinute(t *testing.T) {
	var cs Periodic
	for _, p := range ClockProfile() {
		if p.Name == "clock-sec" {
			cs = p
		}
	}
	if cs.Build == nil {
		t.Fatal("no clock-sec")
	}
	for _, tc := range []struct {
		at     time.Time
		strace string
	}{
		{time.Date(2026, 11, 7, 20, 20, 50, 0, time.UTC), "ff880c010c09000602a01432807d"},
		{time.Date(2026, 11, 7, 20, 21, 16, 0, time.UTC), "ff880c010c09000602a01510805e"},
	} {
		got := cs.Build(tc.at)
		want, err := ParseHexFrame(tc.strace)
		if err != nil {
			t.Fatal(err)
		}
		if got.Hdr != want.Hdr || got.Cmd != want.Cmd || !bytes.Equal(got.Payload[1:], want.Payload[1:]) {
			t.Errorf("at %v: %v, stock sent %v", tc.at.Format("15:04:05"), got, want)
		}
	}
}
