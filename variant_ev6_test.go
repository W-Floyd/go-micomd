package micom

import (
	"encoding/hex"
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
	if labels[[2]byte{2, 2}] == "" || labels[[2]byte{0, 1}] == "" {
		t.Errorf("push labels missing the steering and front-column keys: %v", labels)
	}
	if want := map[byte]string{8: "Tune knob", 9: "Volume knob"}; !reflect.DeepEqual(v.knobLabels, want) {
		t.Errorf("knob labels %v, want %v", v.knobLabels, want)
	}
}

// The EV6 variant's send entries are exactly the unit's -allow, plus the
// inferred requests added since: the frames byte for byte and the volume
// pattern range for range; without -allow-inferred only the captured ones.
func TestEV6VariantSendsAsService(t *testing.T) {
	v := ev6(t)
	var al allowList
	added := []string{
		"ff880c010c01000176",                           // hev-status-request
		"ffc2430043000002013e", "ff8e0b010bc70002c772", // climate-status-request, bcan-valid-request
		"ffc14400440500013a", "ff92110111110003fafa7e", // body-status-request, usm-units-request
		"ff880c010c53000124", "ff880c010c54000123", // ev-schedule-settings-request, ev-charge-window-request
		"ff8a01010111000164", "ff8a01010110000165", // cpu-alive-start, cpu-alive-stop
		"ff8a0101017400020002", "ff8a01010116000163", // module-version-request, version-info-request
		"ff8a010101990001ec", "ff8a01010175000100", "ff8a010101890001fc", // part-number-request, vehicle-trim-request, regulation-type-request
	}
	// The base file's patterns come before the car's.
	base := []string{"ff 8c 08 01 08 02 00 02 [00-01] ..", "ff 85 02 01 02 20 00 02 [20-62] ..", "ff 8a 01 01 01 76 00 02 [00-01] .."} // monitor-backlight-set, audio-mic-volume-set, micom-charge-request
	for _, s := range append(append(base, serviceAllow...), added...) {
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
	if len(ps) != 1 || !reflect.DeepEqual(ps[0], al.patterns[3]) {
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

// clock-sec is weekday bit, hour << 3, minute, second, 80: checked against
// the stock micomd's frames of 2026-11-07, a Saturday (strace on its UART,
// the unit's clock 20:20:50 and 20:21:16), and the first capture, a Friday.
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
		{time.Date(2026, 10, 16, 20, 1, 16, 0, time.UTC), "ff880c010c09000604a00110804c"},
		// Synthetic: Sunday bit 0, Monday bit 6; midnight and 23 h.
		{time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC), "ff880c010c0900060100000080f8"},
		{time.Date(2026, 11, 9, 23, 59, 59, 0, time.UTC), "ff880c010c09000640b83b3b8001"},
	} {
		got := cs.Build(tc.at)
		want, err := ParseHexFrame(tc.strace)
		if err != nil {
			t.Fatal(err)
		}
		if !sameFrame(got, want) {
			t.Errorf("at %v: %v, stock sent %v", tc.at.Format("15:04:05"), got, want)
		}
	}
}
