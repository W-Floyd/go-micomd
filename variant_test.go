package micom

import (
	"reflect"
	"strings"
	"testing"
)

// Every embedded variant loads, and no two could match one command line
// equally well.
func TestVariantsResolve(t *testing.T) {
	names := VariantNames()
	if len(names) == 0 {
		t.Fatal("no embedded variants")
	}
	for _, n := range names {
		v, err := LoadVariant(n)
		if err != nil {
			t.Errorf("%s: %v", n, err)
			continue
		}
		if v.Name != n || v.Source != "embedded" {
			t.Errorf("%s: loaded as %q from %q", n, v.Name, v.Source)
		}
		if _, err := v.YAML(); err != nil {
			t.Errorf("%s: YAML: %v", n, err)
		}
	}
	if amb := ambiguousMatches(); len(amb) > 0 {
		t.Errorf("ambiguous matches: %v", amb)
	}
}

// Each file is refused, with the reason in the error.
func TestLoadVariantRefuses(t *testing.T) {
	const head = "schema: 1\nname: t\nextends: hkmc-gen5w\n"
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"unknown key", head + "colour: red\n", "field colour not found"},
		{"repeated key", head + "title: a\ntitle: b\n", "already defined"},
		{"schema", "schema: 2\nname: t\n", "schema: is 2"},
		{"name", "schema: 1\nname: T_1\n", "not [a-z0-9-]"},
		{"unknown parent", "schema: 1\nname: t\nextends: nope\n", "no such embedded variant"},
		{"cycle", "schema: 1\nname: hkmc-gen5w\nextends: hkmc-gen5w\n", "a cycle"},
		{"bad header", head + "headers: {x: {hdr: \"8a 01 01\", dir: to_micom}}\n", "not 4 hex bytes"},
		{"header dir", head + "headers: {x: {hdr: \"8a 01 01 02\", dir: up}}\n", "dir is from_micom or to_micom"},
		{"bad checksum", head + "send: [{name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 71\", status: captured, ref: r}]\n", "bad checksum"},
		{"range over the header", head + "send: [{name: s, pattern: \"ff 85 02 01 [02-03] 03 00 02 [00-23] ..\", status: captured, ref: r}]\n", "only payload bytes"},
		{"frame and pattern", head + "send: [{name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", pattern: \"ff 85 02 01 02 03 00 02 [00-23] ..\", status: captured, ref: r}]\n", "exactly one of frame and pattern"},
		{"alive reply", head + "send: [{name: s, frame: \"ff 8a 01 01 01 0f 00 02 00 79\", status: captured, ref: r}]\n", "CPU-alive"},
		{"denylisted reset", head + "send: [{name: s, frame: \"ff 8a 01 01 01 07 00 01 72\", status: captured, ref: r}]\n", "denylisted"},
		{"denylisted pattern", head + "send: [{name: s, pattern: \"ff 8a 01 01 01 18 00 02 [00-ff] ..\", status: inferred, ref: r}]\n", "denylisted"},
		{"undeclared header", head + "send: [{name: s, frame: \"ff 88 0c 01 0c 5c 00 01 2b\", status: captured, ref: r}]\n", "not a to_micom header"},
		{"from_micom header", head + "send: [{name: s, frame: \"ff 01 8a 01 81 01 00 01 f4\", status: captured, ref: r}]\n", "not a to_micom header"},
		{"send status", head + "send: [{name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", ref: r}]\n", "status is required"},
		{"send status value", head + "send: [{name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: seen, ref: r}]\n", "is not captured, code or inferred"},
		{"send ref", head + "send: [{name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured}]\n", "ref is required"},
		{"send twice", head + "send: [{name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured, ref: r}, {name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured, ref: r}]\n", "once"},
		{"profile inferred", head + "profile: [{name: p, every: 1s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: inferred, ref: r}]\n", "must be captured"},
		{"profile every", head + "profile: [{name: p, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured, ref: r}]\n", "every must be positive"},
		{"profile placeholder", head + "profile: [{name: p, every: 1s, template: \"ff 85 02 01 02 0b 00 03 {weekday} 01 ..\", status: captured, ref: r}]\n", "unknown placeholder"},
		{"profile placeholder in the header", head + "profile: [{name: p, every: 1s, template: \"ff 85 02 {hour} 02 0b 00 03 02 01 ..\", status: captured, ref: r}]\n", "must be hex bytes"},
		{"profile template length", head + "profile: [{name: p, every: 1s, template: \"ff 85 02 01 02 0b 00 04 02 {second} ..\", status: captured, ref: r}]\n", "length field"},
		{"profile opt-in", head + "profile: [{name: p, every: 1s, opt_in: always, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured, ref: r}]\n", "opt_in is clock"},
		{"match ref", head + "match: {cmdline: {vehicle_info: 1}, status: captured}\n", "ref is required"},
		{"match key", head + "match: {cmdline: {vin: 1}, status: captured, ref: r}\n", "unknown key \"vin\""},
		{"expect key", head + "expect: {colour: 1}\n", "unknown key \"colour\""},
		{"key name", head + "keys: {push: [{type: 0, code: 1, key: KEY_NOPE}]}\n", "unknown key name"},
		{"push type", head + "keys: {push: [{type: 9, code: 1}]}\n", "type 0-7"},
		{"knob type", head + "keys: {knobs: [{type: 2, code: 1}]}\n", "type 8-11"},
		{"key frame header", head + "keys: {frame: {hdr: nowhere, cmd: 1}}\n", "neither a header name"},
		{"two kinds", head + "signals: [{name: s, hdr: \"43 c2 00 c3\", cmd: 1, label: L, flag: {on: 1, off: 0}, enum: {1: a}}]\n", "exactly one of"},
		{"no kind", head + "signals: [{name: s, hdr: \"43 c2 00 c3\", cmd: 1, label: L}]\n", "exactly one of"},
		{"unknown state", head + "signals: [{name: s, hdr: \"43 c2 00 c3\", cmd: 1, label: L, flag: {on: 1, off: 0}, state: colour}]\n", "not a CarClimate field"},
		{"state kind", head + "signals: [{name: s, hdr: \"43 c2 00 c3\", cmd: 1, label: L, flag: {on: 1, off: 0}, state: driver_set_f}]\n", "needs another kind"},
		{"value state", head + "signals: [{name: s, hdr: \"43 c2 00 c3\", cmd: 1, label: L, flag: {on: 1, off: 0}, values: {2: {text: x, state: 1}}}]\n", "only a number's values"},
		{"format verb", head + "signals: [{name: s, hdr: \"43 c2 00 c3\", cmd: 1, label: L, number: {format: \"%s\"}}]\n", "only %d or %#02x"},
		{"two verbs", head + "signals: [{name: s, hdr: \"43 c2 00 c3\", cmd: 1, label: L, number: {format: \"%d %d\"}}]\n", "at most one verb"},
		{"unknown format", head + "signals: [{name: s, hdr: \"43 c2 00 c3\", cmd: 1, label: L, enum: {1: a}, unknown: \"%x\"}]\n", "only %d or %#02x"},
		{"signal label", head + "signals: [{name: s, hdr: \"43 c2 00 c3\", cmd: 1, enum: {1: a}}]\n", "label is required"},
		{"signal cmd taken", head + "signals: [{name: a, hdr: \"43 c2 00 c3\", cmd: 1, label: L, enum: {1: a}}, {name: b, hdr: \"43 c2 00 c3\", cmd: [2, 1], label: L, enum: {1: a}}]\n", "already signal a"},
		{"remove nothing", head + "signals: [{name: s, remove: true}]\n", "nothing inherited"},
		{"field beyond", head + "decode: [{name: d, hdr: \"0c 88 01 8c\", cmd: 0x5c, min_len: 2, fields: [{name: f, off: 1, size: 2}]}]\n", "must lie within"},
		{"field size", head + "decode: [{name: d, hdr: \"0c 88 01 8c\", cmd: 0x5c, min_len: 4, fields: [{name: f, off: 1, size: 4}]}]\n", "size is 1 or 2"},
		{"guard", head + "decode: [{name: d, hdr: \"0c 88 01 8c\", cmd: 0x5c, min_len: 4, when: [{eq: 1}], fields: [{name: f, off: 1}]}]\n", "a guard is"},
		{"no fields", head + "decode: [{name: d, hdr: \"0c 88 01 8c\", cmd: 0x5c}]\n", "fields are required"},
		{"text unit", head + "decode: [{name: d, hdr: \"0c 88 01 8c\", cmd: 0x5c, min_len: 4, fields: [{name: f, off: 1, size: 2, text: true, unit: \" W\"}]}]\n", "a text field has no"},
		{"text mask", head + "decode: [{name: d, hdr: \"0c 88 01 8c\", cmd: 0x5c, min_len: 4, fields: [{name: f, off: 1, mask: 3, text: true}]}]\n", "a text field has no"},
		{"text size", head + "decode: [{name: d, hdr: \"0c 88 01 8c\", cmd: 0x5c, min_len: 12, fields: [{name: f, off: 1, size: 9, text: true}]}]\n", "size is 1 to 8"},
		{"identify pattern", head + "identify: {request: {name: r, pattern: \"ff 8a 01 01 01 17 00 02 [00-01] ..\", status: inferred, ref: r}}\n", "not a pattern"},
	} {
		_, err := loadVariantData([]byte(tc.yaml), "test")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestParseIdentity(t *testing.T) {
	// From the stock /proc/cmdline of the EV6 unit (2026-11-07).
	stock := "console=null androidboot.mode=emmc daudio_hw_ver=526087 daudio_info_id=0 vehicle_info=0xcc country_info=0x01 cabin_camera_type=0 vmalloc=480M"
	for _, tc := range []struct {
		name, cmdline string
		want          Identity
	}{
		{"stock EV6", stock, Identity{"vehicle_info": 0xcc, "country_info": 1, "daudio_hw_ver": 526087, "cabin_camera_type": 0}},
		{"read failed", "vehicle_info=0xff country_info=0xff", Identity{}},
		{"not a number", "vehicle_info=zz country_info=1", Identity{"country_info": 1}},
		{"none", "console=ttyAMA0 root=/dev/sda1", Identity{}},
		{"empty", "", Identity{}},
	} {
		if got := ParseIdentity(tc.cmdline); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A child replaces its parent's entries by name, removes them with remove:
// true, adds its own, and keeps its own match.
func TestMergeVariant(t *testing.T) {
	parent := rawVariant{Name: "p", Title: "P", Match: &rawMatch{Ref: "p"},
		Headers: map[string]rawHeader{"a": {Hdr: "01 02 03 04"}, "b": {Hdr: "05 06 07 08"}},
		Send:    []rawSend{{Name: "x", Frame: "1"}, {Name: "y", Frame: "2"}, {Name: "z", Frame: "3"}},
	}
	child := rawVariant{Name: "c", Extends: "p",
		Headers: map[string]rawHeader{"b": {Hdr: "0a 0b 0c 0d"}},
		Send:    []rawSend{{Name: "y", Remove: true}, {Name: "x", Frame: "1b"}, {Name: "w", Frame: "4"}},
	}
	got := mergeVariant(parent, child)
	if got.Name != "c" || got.Title != "P" || got.Match != nil {
		t.Errorf("scalars: %+v", got)
	}
	if got.Headers["a"].Hdr != "01 02 03 04" || got.Headers["b"].Hdr != "0a 0b 0c 0d" {
		t.Errorf("headers: %v", got.Headers)
	}
	var sends []string
	for _, s := range got.Send {
		sends = append(sends, s.Name+"="+s.Frame)
	}
	if want := []string{"x=1b", "z=3", "w=4"}; !reflect.DeepEqual(sends, want) {
		t.Errorf("sends %v, want %v", sends, want)
	}
}
