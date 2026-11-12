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
	// A to_micom message m (audio_tx, cmd 0x40, 2 bytes) and a from_micom
	// one; extra is more keys of the message, or more messages after it.
	tx := func(fields, extra string) string {
		return head + "messages: [{name: m, dir: to_micom, hdr: audio_tx, cmd: 0x40, length: 2, status: code, ref: r, fields: [" + fields + "]" + extra + "]\n"
	}
	rx := func(fields, extra string) string {
		return head + "messages: [{name: M, dir: from_micom, hdr: system_rx, cmd: 0x40, fields: [" + fields + "]" + extra + "]\n"
	}
	const f0 = "{name: a, off: 0}"
	const prof = "profile: [{name: q, every: 1s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured, ref: r}]\n"
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
		{"dangerous in profile", head + "profile: [{name: p, every: 1s, frame: \"ff 8a 01 01 01 07 00 01 72\", status: captured, ref: r}]\n", "a profile frame is sent unasked"},
		{"undeclared header", head + "send: [{name: s, frame: \"ff 88 0c 01 0c 5c 00 01 2b\", status: captured, ref: r}]\n", "not a to_micom header"},
		{"from_micom header", head + "send: [{name: s, frame: \"ff 01 8a 01 81 01 00 01 f4\", status: captured, ref: r}]\n", "not a to_micom header"},
		{"send status", head + "send: [{name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", ref: r}]\n", "status is required"},
		{"send status value", head + "send: [{name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: seen, ref: r}]\n", "is not captured, code or inferred"},
		{"send ref", head + "send: [{name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured}]\n", "ref is required"},
		{"send twice", head + "send: [{name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured, ref: r}, {name: s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured, ref: r}]\n", "once"},
		{"profile inferred", head + "profile: [{name: p, every: 1s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: inferred, ref: r}]\n", "must be captured"},
		{"profile code", head + "profile: [{name: p, every: 1s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: code, ref: r}]\n", "must be captured"},
		{"opt_in code captured", head + "profile: [{name: p, every: 1s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", opt_in: code, replaces: q, status: captured, ref: r}]\n", "opt_in code is for status code"},
		{"opt_in code alone", head + "profile: [{name: p, every: 1s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", opt_in: code, status: code, ref: r}]\n", "needs replaces"},
		{"replaces nothing", head + "profile: [{name: p, every: 1s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", opt_in: code, replaces: q, status: code, ref: r}]\n", "no routine entry"},
		{"replaces another frame", head + "profile: [{name: q, every: 1s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured, ref: r}, {name: p, every: 1s, frame: \"ff 85 02 01 02 0c 00 03 02 01 77\", opt_in: code, replaces: q, status: code, ref: r}]\n", "a different frame"},
		{"replaces without code", head + "profile: [{name: q, every: 1s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", status: captured, ref: r}, {name: p, every: 1s, frame: \"ff 85 02 01 02 0b 00 03 02 01 70\", replaces: q, status: captured, ref: r}]\n", "only for opt_in code"},
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
		{"decode", head + "decode: [{name: d, hdr: \"0c 88 01 8c\", cmd: 0x5c, min_len: 2, fields: [{name: f, off: 1}]}]\n", "decode: moved to messages:"},
		{"from_micom guard", rx(f0, ", min_length: 2, when: [{eq: 1}]}"), "a guard is {off"},
		{"from_micom shadowed", rx(f0, ", min_length: 2}, {name: N, dir: from_micom, hdr: system_rx, cmd: 0x40, min_length: 3, fields: [{name: b, off: 2}]}"), `never decodes: "M", earlier`},
		{"from_micom shadowed, open header", head + "messages: [{name: M, dir: from_micom, hdr: \"01 8a ?? 81\", cmd: 0x40, length: 3, fields: [" + f0 + "]}, " +
			"{name: N, dir: from_micom, hdr: system_rx, cmd: 0x40, length: 3, when: [{off: 1, eq: 2}], fields: [{name: b, off: 2}]}]\n", "never decodes"},
		{"from_micom str unit", rx("{name: a, off: 0, type: str, size: 2, unit: \" W\"}", ", min_length: 2}"), "a str field has only"},
		{"message dir", head + "messages: [{name: m, dir: up, cmd: 1, fields: [" + f0 + "]}]\n", "dir is from_micom or to_micom"},
		{"message twice", tx(f0, "}, {name: m, dir: to_micom, hdr: audio_tx, cmd: 0x41, length: 1, status: code, ref: r, fields: ["+f0+"]}"), "once"},
		{"message remove nothing", head + "messages: [{name: m, remove: true}]\n", "nothing inherited"},
		{"to_micom name", head + "messages: [{name: M m, dir: to_micom, hdr: audio_tx, cmd: 0x40, length: 1, status: code, ref: r, fields: [" + f0 + "]}]\n", "a to_micom name is"},
		{"to_micom status", head + "messages: [{name: m, dir: to_micom, hdr: audio_tx, cmd: 0x40, length: 1, ref: r, fields: [" + f0 + "]}]\n", "status is required"},
		{"to_micom ref", head + "messages: [{name: m, dir: to_micom, hdr: audio_tx, cmd: 0x40, length: 1, status: code, fields: [" + f0 + "]}]\n", "ref is required"},
		{"to_micom length", head + "messages: [{name: m, dir: to_micom, hdr: audio_tx, cmd: 0x40, status: code, ref: r, fields: [" + f0 + "]}]\n", "length is required"},
		{"to_micom open header", head + "messages: [{name: m, dir: to_micom, hdr: \"85 02 ?? 02\", cmd: 0x40, length: 1, status: code, ref: r, fields: [" + f0 + "]}]\n", "neither a header name"},
		{"to_micom from_micom header", head + "messages: [{name: m, dir: to_micom, hdr: system_rx, cmd: 0x40, length: 1, status: code, ref: r, fields: [" + f0 + "]}]\n", "not a to_micom header"},
		{"to_micom alive", head + "messages: [{name: m, dir: to_micom, hdr: system_tx, cmd: 0x0f, length: 1, status: code, ref: r, fields: [" + f0 + "]}]\n", "CPU-alive"},
		{"dangerous on from_micom", head + "messages: [{name: m, dir: from_micom, hdr: system_rx, cmd: 0x40, dangerous: x, status: code, ref: r, fields: [" + f0 + "]}]\n", "dangerous and requires are for to_micom"},
		{"to_micom guard", tx(f0, ", when: [{off: 0, eq: 1}]}"), "are for from_micom"},
		{"two messages, one frame", tx(f0, "}, {name: n, dir: to_micom, hdr: audio_tx, cmd: 0x40, length: 1, status: code, ref: r, fields: ["+f0+"]}"), "already sends"},
		{"chunked", tx(f0, ", transport: {kind: chunked, prefix: 0xf0}}"), "needs the captured TP function byte"},
		{"transport kind", tx(f0, ", transport: double}"), "transport is single or chunked"},
		{"min_interval", tx(f0, ", min_interval: -1s}"), "are positive"},
		{"persistent every", tx(f0, ", persistent: true, min_interval: 1s, every: 1s, timeout: 5s}"), "persistent: no every, owns or end"},
		{"persistent owns", prof + tx(f0, ", persistent: true, min_interval: 1s, owns: q, timeout: 5s}"), "persistent: no every, owns or end"},
		{"persistent end", tx(f0, ", persistent: true, min_interval: 1s, end: [{msg: m, set: {a: 0}}]}"), "persistent: no every, owns or end"},
		{"persistent on every input", tx(f0, ", persistent: true, min_interval: 1s, on_change: false}"), "persistent: on_change only"},
		{"persistent unspaced", tx(f0, ", persistent: true}"), "persistent: min_interval of at least 1s"},
		{"persistent too often", tx(f0, ", persistent: true, min_interval: 500ms}"), "persistent: min_interval of at least 1s"},
		{"persistent in an end", tx(f0, ", end: [{msg: n, set: {a: 0}}]}, {name: n, dir: to_micom, hdr: audio_tx, cmd: 0x41, length: 1, status: code, ref: r, fields: ["+f0+"], persistent: true, min_interval: 1s}"), "n is persistent: an end"},
		{"from_micom persistent", rx(f0, ", min_length: 2, persistent: true}"), "are for to_micom"},
		{"requires on from_micom", rx(f0, ", min_length: 2, requires: [{field: Navigation, in: [1]}]}"), "are for to_micom"},
		{"requires an unknown field", tx(f0, ", requires: [{field: Nope, in: [1]}]}"), `no from_micom field "Nope"`},
		{"requires a to_micom field", tx(f0, ", requires: [{field: a, in: [1]}]}"), `no from_micom field "a"`},
		{"requires a name not in the enum", tx(f0, ", requires: [{field: Vehicle, in: [ev9]}]}"), "ev9 is not a value of Vehicle"},
		{"requires a code of an enum", tx(f0, ", requires: [{field: Vehicle, in: [204]}]}"), "204 is not a value of Vehicle"},
		{"requires a name of a number", tx(f0, ", requires: [{field: Navigation, in: [on]}]}"), "on is not a value of Navigation"},
		{"requires nothing", tx(f0, ", requires: [{field: Navigation, in: []}]}"), "in lists the values"},
		{"requires a str", tx(f0, ", requires: [{field: Language, in: [en]}]}"), "a str field cannot be required"},
		{"requires, if an unknown field", tx(f0, ", requires: [{if: {b: 1}, field: Navigation, in: [1]}]}"), "if: b: no such field"},
		{"reflects on from_micom", rx("{name: a, off: 0, reflects: {msg: M, field: a}}", ", min_length: 2}"), "reflects is for to_micom"},
		{"reflects an unknown message", tx("{name: a, off: 0, reflects: {msg: Nope, field: a}}", "}"), `reflects "Nope": no from_micom message`},
		{"reflects an unknown field", tx("{name: a, off: 0, reflects: {msg: \"Variant code\", field: Nope}}", "}"), `has no field "Nope"`},
		{"from on from_micom", rx("{name: a, off: 0, from: {clock: hour}}", ", min_length: 2}"), "from is for to_micom"},
		{"from neither", tx("{name: a, off: 0, from: {}}", "}"), "from is {clock: NAME} or {field: NAME}"},
		{"from both", tx("{name: a, off: 0, from: {clock: hour, field: Vehicle}}", "}"), "from is {clock: NAME} or {field: NAME}"},
		{"from an unknown clock", tx("{name: a, off: 0, from: {clock: fortnight}}", "}"), `"fortnight" is not one of`},
		{"from clock weekday, a number", tx("{name: a, off: 0, from: {clock: weekday}}", "}"), "an enum field whose table names sunday"},
		{"from clock hour, a bool", tx("{name: a, off: 0, type: bool, from: {clock: hour}}", "}"), "from clock hour: a number field"},
		{"from an unknown field", tx("{name: a, off: 0, from: {field: Nope}}", "}"), `from: no from_micom field "Nope"`},
		{"from a field of another kind", tx("{name: a, off: 0, from: {field: Vehicle}}", "}"), `from "Vehicle": its values are enum, this field takes int`},
		{"end sets a computed field", tx("{name: a, off: 0, from: {clock: hour}}", ", end: [{msg: m, set: {a: 1}}]}"), "a: computed from clock hour, not set"},
		{"end with nothing received", tx("{name: a, off: 0, enum: vehicle, from: {field: Vehicle}}", ", end: [{msg: m}]}"), `a: from "Vehicle": no value`},
		{"requires, if a bad value", tx("{name: a, off: 0, max: 3}", ", requires: [{if: {a: 9}, field: Navigation, in: [1]}]}"), "if: a: 9 is above the maximum 3"},
		{"requires fields that differ", head + "enums: {e: {x: 1}}\nmessages: [{name: A, dir: from_micom, hdr: system_rx, cmd: 0x41, length: 1, fields: [{name: X, off: 0}]}, " +
			"{name: B, dir: from_micom, hdr: system_rx, cmd: 0x42, length: 1, fields: [{name: X, off: 0, enum: e}]}, " +
			"{name: m, dir: to_micom, hdr: audio_tx, cmd: 0x40, length: 1, status: code, ref: r, fields: [" + f0 + "], requires: [{field: X, in: [1]}]}]\n", "differ in type or enum"},
		{"every without timeout", tx(f0, ", every: 1s}"), "need timeout"},
		{"owns without timeout", tx(f0, ", owns: q}"), "need timeout"},
		{"owns nothing", tx(f0, ", owns: q, timeout: 1s}"), "no routine profile entry"},
		{"owns another frame", prof + tx(f0, ", owns: q, timeout: 1s}"), "a different frame"},
		{"end message", tx(f0, ", end: [{msg: n}]}"), "not a to_micom message"},
		{"end field", tx(f0, ", end: [{msg: m, set: {b: 1}}]}"), "b: no such field"},
		{"end required", tx(f0, ", end: [{msg: m}]}"), "a: required"},
		{"end value", tx("{name: a, off: 0, max: 9, default: 0}", ", end: [{msg: m, set: {a: 10}}]}"), "above the maximum"},
		{"field type", tx("{name: a, off: 0, type: u64}", "}"), "type is u8"},
		{"field beyond", tx("{name: a, off: 1, type: u16}", "}"), "must lie within"},
		{"field overlap", tx("{name: a, off: 0, bits: \"7:4\"}, {name: b, off: 0, bits: \"5:0\"}", "}"), "overlaps"},
		{"field twice", tx(f0+", {name: a, off: 1}", "}"), "used once"},
		{"field name", tx("{name: Speed, off: 0}", "}"), "[a-z][a-z0-9_]*"},
		{"field name msg", tx("{name: msg, off: 0}", "}"), "not msg or end"},
		{"bits on u16", tx("{name: a, off: 0, type: u16, bits: \"3:0\"}", "}"), "bits are within one byte"},
		{"bits order", tx("{name: a, off: 0, bits: \"4:7\"}", "}"), "is not \"hi:lo\""},
		{"invalid too wide", tx("{name: a, off: 0, bits: \"3:0\", invalid: 0x1f}", "}"), "does not fit the field's 4 bits"},
		{"endian", tx("{name: a, off: 0, type: u16, endian: middle}", "}"), "endian is big or little"},
		{"no enum", tx("{name: a, off: 0, enum: nope}", "}"), "no enum"},
		{"enum code too big", "enums: {e: {x: 16}}\n" + tx("{name: a, off: 0, bits: \"3:0\", enum: e}", "}"), "does not fit the field"},
		{"enum code is invalid", "enums: {e: {x: 15}}\n" + tx("{name: a, off: 0, bits: \"3:0\", enum: e, invalid: 15}", "}"), "its invalid marker"},
		{"enum code twice", "enums: {e: {x: 1, y: 1}}\n" + tx(f0, "}"), "are both 1"},
		{"enum name", "enums: {e: {X: 1}}\n" + tx(f0, "}"), "name \"X\""},
		{"enum bool", "enums: {e: {x: 1}}\n" + tx("{name: a, off: 0, type: bool, enum: e}", "}"), "an enum is an integer field"},
		{"scale zero", tx("{name: a, off: 0, scale: 0}", "}"), "scale is a finite number"},
		{"scale on enum", "enums: {e: {x: 1}}\n" + tx("{name: a, off: 0, enum: e, scale: 2}", "}"), "are for numbers"},
		{"min above max", tx("{name: a, off: 0, min: 5, max: 4}", "}"), "min is above max"},
		{"default range", tx("{name: a, off: 0, max: 9, default: 12}", "}"), "default: a: 12 is above the maximum 9"},
		{"default marker", tx("{name: a, off: 0, invalid: 0xff, default: 255}", "}"), "encodes to the invalid marker"},
		{"default fraction", tx("{name: a, off: 0, default: 1.5}", "}"), "is not an integer"},
		{"no placement", tx("{name: a}", "}"), "placed by off, or by start_bit"},
		{"bit_length", tx("{name: a, start_bit: 0, bit_length: 33}", "}"), "bit_length is 1 to 32"},
		{"start_bit and off", tx("{name: a, off: 0, start_bit: 0, bit_length: 4}", "}"), "without off or bits"},
		{"start_bit type", tx("{name: a, start_bit: 0, bit_length: 4, type: u8}", "}"), "type is bool or absent"},
		{"signed byte field", tx("{name: a, off: 0, signed: true}", "}"), "are for start_bit fields"},
		{"intel beyond", tx("{name: a, start_bit: 15, bit_length: 2, endian: little}", "}"), "must lie within"},
		{"motorola beyond", tx("{name: a, start_bit: 7, bit_length: 17}", "}"), "must lie within"},
		{"bit overlap", tx("{name: a, off: 0, bits: \"7:4\"}, {name: b, start_bit: 4, bit_length: 2}", "}"), "overlaps another field at byte 0 bit 4"},
		{"str size", tx("{name: a, off: 0, type: str}", "}"), "needs a size"},
		{"str start_bit", tx("{name: a, start_bit: 0, type: str, size: 1}", "}"), "placed by off"},
		{"str bits", tx("{name: a, off: 0, type: str, size: 2, bits: \"3:0\"}", "}"), "a str field has only"},
		{"str encoding", tx("{name: a, off: 0, type: str, size: 2, encoding: latin1}", "}"), "encoding is ASCII or UTF-16LE"},
		{"utf16 size", tx("{name: a, off: 0, type: str, size: 1, encoding: UTF-16LE}", "}"), "size is even"},
		{"size on a number", tx("{name: a, off: 0, size: 2}", "}"), "are for str"},
		{"from_micom u32", rx("{name: a, off: 0, type: u32}", ", min_length: 4}"), "must fit 32 bits"},
		{"from_micom scaled past 32 bits", rx("{name: a, off: 0, type: i32, scale: 2}", ", min_length: 4}"), "must fit 32 bits"},
		{"from_micom default", rx("{name: a, off: 0, default: 1}", ", min_length: 2}"), "are for to_micom"},
		{"from_micom every", rx(f0, ", min_length: 2, every: 1s}"), "are for to_micom"},
		{"from_micom both lengths", rx(f0, ", min_length: 2, length: 2}"), "not both"},
		{"from_micom len guard", rx(f0, ", min_length: 2, when: [{len: 2}]}"), "a guard is {off"},
		{"from_micom guard beyond", rx(f0, ", min_length: 2, when: [{off: 2, eq: 1}]}"), "beyond the 2 bytes"},
		{"from_micom beyond", rx("{name: a, off: 0, type: u16}", ", min_length: 1}"), "must lie within"},
		{"from_micom fields", head + "messages: [{name: M, dir: from_micom, hdr: system_rx, cmd: 0x40}]\n", "fields are required"},
		{"identify pattern", head + "identify: {request: {name: r, pattern: \"ff 8a 01 01 01 17 00 02 [00-01] ..\", status: inferred, ref: r}}\n", "not a pattern"},
		{"reply identity key", head + "identify: {reply: {name: V, hdr: system_rx, cmd: 0x17, status: code, identity: {vin: Vehicle}}}\n", "\"vin\" is not an identity key"},
		{"reply identity field", head + "identify: {reply: {name: V, hdr: system_rx, cmd: 0x17, status: code, identity: {vehicle_info: Colour}}}\n", "no from_micom field \"Colour\""},
	} {
		_, err := loadVariantData([]byte(tc.yaml), "test")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
}

// The identity is read from the kernel command line, or from the micom's
// variant record (0x8117), whose leading codes are the same.
func TestParseIdentity(t *testing.T) {
	// From the stock /proc/cmdline of the EV6 unit (2026-11-07).
	stock := "console=null androidboot.mode=emmc daudio_hw_ver=526087 daudio_info_id=0 vehicle_info=0xcc country_info=0x01 cabin_camera_type=0 vmalloc=480M"
	v := ev6(t)
	for _, tc := range []struct {
		name, cmdline string
		record        string // a variant record's payload in hex, read in place of cmdline
		want          Identity
	}{
		{"stock EV6", stock, "", Identity{"vehicle_info": 0xcc, "country_info": 1, "daudio_hw_ver": 526087, "cabin_camera_type": 0}},
		{"read failed", "vehicle_info=0xff country_info=0xff", "", Identity{}},
		{"not a number", "vehicle_info=zz country_info=1", "", Identity{"country_info": 1}},
		{"none", "console=ttyAMA0 root=/dev/sda1", "", Identity{}},
		{"empty", "", "", Identity{}},
		{"record, EV6 US", "", "cc 01 a0 00 00 00 00 65 6e 00 00 00 00", Identity{"vehicle_info": 0xcc, "country_info": 1}},
		{"record, Ioniq 5 Korea", "", "bd 00 00 00 00 00 00 6b 6f 00 00 00 00", Identity{"vehicle_info": 0xbd, "country_info": 0}},
		{"record unread", "", "ff ff 00 00 00 00 00 65 6e 00 00 00 00", Identity{}},
	} {
		got := ParseIdentity(tc.cmdline)
		if tc.record != "" {
			var ok bool
			got, ok = v.ReplyIdentity(Frame{Hdr: hdrAliveReq, Cmd: 0x17, Payload: unhex(t, tc.record)})
			if !ok {
				t.Errorf("%s: not read as the variant record", tc.name)
			}
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, ok := v.ReplyIdentity(Frame{Hdr: hdrAliveReq, Cmd: cmdAlive, Payload: []byte{0}}); ok {
		t.Error("the CPU-alive request read as the variant record")
	}
}

// The record is matched like the command line, and a mismatch asks for a
// restart: the variant is never switched live.
func TestRecordVerdict(t *testing.T) {
	ev6ID := Identity{"vehicle_info": 0xcc, "country_info": 1}
	for _, tc := range []struct {
		name, running string
		id            Identity
		matched, msg  string
	}{
		{"as running", DefaultVariantName, ev6ID, DefaultVariantName, "variant kia-ev6-gen5w, as running"},
		{"another running", "hkmc-gen5w", ev6ID, DefaultVariantName, "running hkmc-gen5w: restart with -variant kia-ev6-gen5w"},
		{"unknown car", DefaultVariantName, Identity{"vehicle_info": 0xbd}, "", "vehicle_info=0xbd, which no variant matches; running kia-ev6-gen5w"},
		{"nothing read", DefaultVariantName, Identity{}, "", "(no identity keys), which no variant matches"},
	} {
		matched, msg := recordVerdict(tc.running, tc.id)
		if matched != tc.matched || !strings.Contains(msg, tc.msg) {
			t.Errorf("%s: %q %q, want %q and %q", tc.name, matched, msg, tc.matched, tc.msg)
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
