# Messages

A variant's `messages:` describe multi-field frames in both directions with
one schema: frames the micom sends (`dir: from_micom`), which micomd decodes
for `-dump`, `-tap` and Snapshot, and frames the head unit sends
(`dir: to_micom`), which a client fills in by name over one structured input
socket. The client never sees bytes. micomd validates the input against the
variant, encodes it, sends it and keeps it alive or ends it by the message's
lifecycle.

The first users are the instrument cluster's turn-by-turn guidance (TBT) and
its compass. Their layouts are from stock code (`ClusterService$NaviCluster`,
`GpmNaviTBTManager`), not captured, so they are `status: code` and need
`-allow-inferred`.

The rules of [variants.md](variants.md) still hold: nothing in a file can send
a frame the gates do not allow, and the link (framing, checksum, CPU-alive)
is code.

## Schema

```yaml
enums:                       # named tables: name -> code; merged by table name
  distance_unit: { m: 0, km: 1, mi: 2, ft: 3, yd: 4 }
  maneuver: { none: 0, straight: 65, turn_right: 67, ... }

messages:                    # a named list: merged by name, remove: true deletes
  # from_micom: what decode: was.
  - name: "EV power block"   # FrameName, and the readings' frame
    dir: from_micom
    hdr: "0c 88 ?? 8c"       # a header name or bytes; ?? (open) only from_micom
    cmd: 0x5c
    min_length: 29           # from_micom: length (exact) or min_length
    when: [ { off: 0, eq: 0x57 } ]   # optional guards on payload bytes
    fields:
      - { name: "Climate total", off: 10, type: u16, invalid: 0xffff, unit: " W" }
    status: code             # optional from_micom
    ref: "HEVService"

  # to_micom: filled in from the input socket.
  - name: tbt-turn           # [a-z0-9-]: the input's "msg"
    dir: to_micom
    hdr: cluster_tx          # a to_micom header the variant declares
    cmd: 0x04
    length: 8                # payload length, exact; every bit covered once at most
    fill: 0x00               # bytes no field covers (default 0x00)
    transport: single        # default; see Transports
    fields:                  # names [a-z][a-z0-9_]*: the input's keys
      - { name: maneuver, off: 0, enum: maneuver }                 # no default, no invalid: required
      - { name: side_street, off: 1, type: u16, invalid: 0xffff }  # absent: invalid
      - { name: direction, off: 3, scale: 7.5, max: 360, default: 0 }
      - { name: distance, off: 4, type: u16, invalid: 0xffff }
      - { name: unit, off: 6, bits: "7:4", enum: distance_unit, invalid: 15 }
      - { name: bar, off: 7, bits: "7:4", scale: 10, max: 100, invalid: 15 }
      - { name: distance_tenths, off: 7, bits: "3:0", max: 9, invalid: 15 }
    on_change: true          # send when the encoded frame changes (default true)
    every: 1s                # and/or repeat the last frame while active
    # owns: ack              # a profile entry this replaces while active (cluster-sub-info)
    timeout: 30s             # no input for this long: run end
    end:                     # sent in order when the message ends
      - { msg: tbt-turn, set: { maneuver: none, distance: null } }
      - { msg: tbt-guiding, set: { guiding: false } }
    status: code             # required to_micom
    ref: "stock ClusterService$NaviCluster TBTInfo packer"
```

`decode:` is gone: its entries are `messages:` with `dir: from_micom` (see
Migration). `signals`, `profile`, `send`, `keys` and `identify` are unchanged.

## Field codecs

One field spec decodes (from_micom) and encodes (to_micom).

Numbers are **CAN DBC signals**: raw bits at a place in the payload, signed
or unsigned, in a byte order; physical = raw × scale + offset when decoding,
raw = round((physical − offset) / scale) when encoding; min and max refused
on encode; a unit; value tables (our enums). So cantools or opendbc serve as
a mental test oracle: a field written as `start_bit`/`bit_length`/`endian`
lays out bits exactly as the same DBC signal would (`TestFieldCodec` has
rows of both byte orders). The DBC file format itself is not used: its
29-bit IDs are not our 4-byte header and command, and it has no text,
transports or lifecycle.

Strings, and later the chunked transport and any conditional layout,
borrow **Kaitai Struct**'s vocabulary where it fits (`type: str`, `size`,
`encoding: ASCII`/`UTF-16LE`, `pad_right`; `repeat`, `if`, `switch-on`
when needed), as data, never expressions that run.

A field is placed one of two ways:

| key | meaning |
|---|---|
| `off` + `type` | byte form: `u8` (default), `u16`, `u32`, `i8`, `i16`, `i32`, `bool` at payload byte `off` |
| `bits` | with `off`: `"hi:lo"` within that byte, for one-byte types; `"3"` is one bit |
| `start_bit` + `bit_length` | bit form, as DBC: bit k is bit k%8 of byte k/8; 1 to 32 bits; `signed: true` for signed; `type: bool` allowed |
| `endian` | `big` (default; every layout seen so far; DBC's Motorola, `start_bit` is the most significant bit) or `little` (Intel, `start_bit` the least) |

and coded by:

| key | meaning |
|---|---|
| `scale`, `offset` | physical = raw × scale + offset |
| `enum` | a table name: the input is a name, the frame its code |
| `invalid` | the raw marker for "not available" (0xff, 0xffff, 0x3f, 15) |
| `min`, `max` | the input's range, inclusive, before scaling; the raw value must also fit the field |
| `default` | the input value when none was given |
| `unit` | appended to a decoded value, as before |
| `type: str` | `off`, `size` (bytes), `encoding` (`ASCII`, the default, or `UTF-16LE`), `pad_right` (byte) |

Rules:

- **Rounding** is half up (Java's `Math.round`, which the stock packers use):
  heading 217.5° / 7.5 = 29 exactly, 359° rounds to 48.
- **Integers only** where there is no scale or offset: 2.5 for a `u8` is
  refused, not truncated.
- **Invalid**: JSON `null` (YAML `null` in `set:`) sends the marker. A field
  without `invalid` refuses `null`. A value whose raw form equals the marker
  is refused: it would read back as "not available".
- **Absent** from the input: the field's current value (see State), else its
  `default`, else its `invalid` marker, else the input is refused as missing a
  required field.
- **Enums**: a name not in the table is refused; numbers are refused (codes
  are the variant's business). Every code must fit the field and differ from
  its marker, checked at load. Decoding gives the code as `Value` and the name
  as `Text`.
- **bool**: `true`/`false` in, 1/0 out.
- **str**: refused if longer than `size` or not encodable (ASCII: printable
  only); the rest is `pad_right` (default 0x00). Decoding drops trailing
  `pad_right` bytes when one is declared and, for ASCII, reads n/a when a
  byte is unprintable, as before.
- **32-bit ARM**: `int` is 32 bits there, and `Field.Value` is an `int`. The
  codec computes in 64 bits; a from_micom field must decode into an `int32`
  (so no `u32` from the micom, and scaled fields are bounded at load).
  Inputs are JSON numbers (float64), exact to 2⁵³.
- **Layout checks at load**: every field lies within `length` (or the
  `min_length` a from_micom message promises); to_micom fields never share a
  bit; `bits` only on one-byte types; a `default` must encode.

The organisation is opendbc's: shared base definitions (`hkmc-gen5w`) and
per-car files that extend them, framing and checksum in code, the car
identified at startup (the kernel command line). The lifecycle and the
safety gates below are ours; no prior art covers them.

## Transports

- `single` (the default): the payload is one frame, at most 512 bytes.
- `chunked`: the cluster's long strings (crossroad and destination names, lane
  and turn lists) go as TP frames on `c3 4d 80 4d` with function byte
  `0x80 | funcValue`. `funcValue` is in `UNIClusterProtocol` in framework.jar,
  which we do not have, so the schema accepts the kind and the loader refuses
  it ("needs the captured TP function byte"). The fields it will need:

  ```yaml
  transport: { kind: chunked, func: 0x??, prefix: 0xF0 }   # not loadable yet
  ```

  The multiplexed UCLU_9 payloads (0xF1 lanes: a count, then 3 bytes per
  lane; 0xF2 turn list: 56 bytes per step) are Kaitai's `repeat` and
  `switch-on` on the prefix byte; they get those keys, as data, when the
  transport is known. A strace of the stock micomd while the stock
  navigation guides gives the byte and whether long payloads are split.
  Until then no str field over this transport can be sent, and nothing is
  guessed.

## Lifecycle

Each to_micom message has a state: inactive, or active with its merged field
values.

- **Input** for message M validates every key, merges the values into M's
  state (keys not given keep theirs), encodes, and on success makes M active.
  A refused input changes nothing.
- **on_change** (default true): the frame goes out when it differs from the
  last one sent, and always on the first input.
- **every**: while active, the last frame is repeated at that interval. With
  `on_change: false` the first input still sends at once.
- **owns**: a profile entry. While M is active that entry is not sent: M's
  frame replaces it, never goes beside it. It must have M's header and
  command. Owning an entry also owns any `opt_in: code` entry that `replaces`
  it, so `-code-profile` and the input never both drive one frame. When M
  ends, the entry resumes.
- **timeout**: no input for M in this long ends M. Required with `every` or
  `owns`, so a client that dies cannot leave a frame repeating or a profile
  entry silenced. A client keeps M alive by repeating its last input.
- **end**: the steps run, in order, when M ends: by timeout, or by the input
  `{"msg":"M","end":true}`. Each step merges `set` into its target's state
  and sends the target once:
  - an active target keeps its new values (and its repeats);
  - an inactive one is encoded from its defaults plus `set`, sent, and stays
    inactive.

  Then M becomes inactive: its state is cleared, its repeats stop, and its
  owned entry resumes. A step must encode from its target's defaults plus
  `set` alone, checked at load, so an end always works.

The engine runs off one 50 ms tick, so `every` and `timeout` are honoured to
within 50 ms.

## Input

`micomd ... -input /run/micomd-input.sock` serves a unix socket (mode 0660).
Each line is one JSON object; each gets one line back.

```
> {"msg":"tbt-guiding","guiding":true}
< {"ok":true}
> {"msg":"tbt-turn","maneuver":"turn_right","direction":90,"distance":0,"distance_tenths":3,"unit":"km","bar":30}
< {"ok":true}
> {"msg":"cluster-sub-info","compass":217.5}
< {"ok":true}
> {"msg":"tbt-turn","end":true}
< {"ok":true}
```

The second line sends `ff 8f 0d 01 0d 04 00 09 43 ff ff 0c 00 00 10 33 10`,
the report's "turn right in 0.3 km".

Refusals name the message and the field:

```
{"error":"no message \"tbt-trun\""}
{"error":"tbt-turn: status code: needs -allow-inferred"}
{"error":"tbt-turn: maneuver: \"left\" is not in enum maneuver"}
{"error":"tbt-turn: bar: 120 is above the maximum 100"}
{"error":"tbt-turn: speed: no such field"}
{"error":"tbt-turn: maneuver: required"}
{"error":"tbt-turn: distance_tenths: 2.5 is not an integer"}
{"error":"tbt-turn: end takes no fields"}
{"error":"Variant code: from_micom, not an input"}
{"error":"line is not a JSON object"}
```

## Gates

- Nothing is sent from the input without `-input`.
- `status: captured` messages are sendable once `-input` is given. `code` and
  `inferred` ones also need `-allow-inferred`, the flag that already admits
  such send entries and the identify request. A message is enabled only if
  every message its `end` names is too.
- At load, a to_micom message's header must be a declared `to_micom` header;
  the CPU-alive header and command, and the denylist, are refused, as for
  send entries. The denylist is checked again on every frame the input sends.
- `owns` must name a routine profile entry of the same header and command.
- No existing flag changes; without `-input` micomd behaves exactly as before.

## The cluster messages (EV6)

All on `8f 0d 01 0d` (`cluster_tx`, which was `ack_tx`), `status: code`:

| message | func | fields | lifecycle |
|---|---|---|---|
| `tbt-turn` | 0x0D04 TBTInfo | maneuver, side_street, direction, distance, unit, side_street_combi, bar, distance_tenths | change + 1 s, timeout 30 s, end below |
| `tbt-points` | 0x0D02 TBTSubInfo2C | p1..p3 `_unit`, `_tenths`, `_distance` | change + 1 s, timeout 30 s, end: all invalid |
| `cluster-sub-info` | 0x0D03 TBTSubInfo | remaining_unit, remaining_tenths, eta_hour, eta_minute, compass, time_type, remaining, time_format, est_point_type | change + 600 ms, owns `ack`, timeout 5 s |
| `tbt-guiding` | 0x0D11 OperationStatus | guiding | change; end: false |

`tbt-turn`'s end is the stock end of route: 0x0D04 type 0 with the distance
invalid, 0x0D02 and the route fields of 0x0D03 invalid, then 0x0D11 = 0.

**The compass and the profile.** 0x0D03 carries both the route summary and
the compass, so it is one message whose fields two clients may fill: a gpsd
bridge sends `{"msg":"cluster-sub-info","compass":deg}`, the navigation
sends the route fields. With every field at its default and the compass at
105° it encodes to the captured `ack` frame byte for byte; with no compass, to
`-code-profile`'s `tbt-sub-info` frame. So:

- without `-input`, nothing changes: `ack` (or, with `-code-profile`,
  `tbt-sub-info`) runs as before;
- with `-input`, while `cluster-sub-info` is active it owns `ack` and with it
  `tbt-sub-info`, which `replaces: ack`: one 0x0D03 stream at 600 ms, never
  two;
- 5 s after the last input it ends and the profile entry resumes.

The TBT end invalidates only the route fields of 0x0D03, not the compass: the
stock end of route also sends the compass as 3f, but here the compass has its
own source.

These messages are in the EV6 file, not the family base, though they come
from the shared ClusterService: `cluster-sub-info` owns the EV6's own profile
entry, and `tbt-turn`'s end names it. The enums are in the base.

## Migration

- Every `decode:` entry became a `dir: from_micom` message: `min_len` is
  `min_length`; a `{len: N}` guard is `length: N`; `size: 2` is `type: u16`;
  the implicit 0xff / 0xffff "n/a" is an explicit `invalid:`, and `na: none`
  is its absence; `mask`/`shift` are `bits`; `text: true` is `type: str`.
  Describe, Decode, FrameName and the profile bytes of the EV6 are unchanged
  (the equivalence tests).
- `decode:` is not accepted alongside: one schema, one code path. A file
  that still has it is refused with "decode: moved to messages: (dir:
  from_micom), docs/messages.md". All such files are in this repository;
  `-variant-file` is for development. `schema` stays 1: nothing else changed.
- `signals`, `profile`, `send` stay. Signals print lines and fold into
  CarClimate; the profile is sent unasked under a stricter gate; send entries
  are exact frames for the control socket. Each could be a message one day,
  but none needs a field codec now, and moving them risks the EV6's pinned
  bytes for no new capability.
- to_micom frames are not described by `Describe`/`Decode`/`FrameName`
  (those read what the micom sends, and the EV6 equivalence pins them).
  `Variant.DecodeInput(f)` reads a to_micom frame back into its input, for
  tests and tools.

## What stays code

Framing and checksum; the CPU-alive exchange; the denylist; the codec types,
rounding and the gates; the transports; the lifecycle engine (state, merge,
repeat, own, timeout, end); the input socket and its JSON. A variant only
describes.

## Open questions

- **Endianness** of 0x0D02/03/04's int16s: big-endian is a guess
  (`Utils.shortToBytes` is in framework.jar). The worked example has distance
  0, so it does not tell. One flip of `endian:` in the file fixes it.
- **Repeat rates**: stock sends TBT on change; whether the cluster drops
  guidance without repeats is unknown. 1 s is a guess, 600 ms is the captured
  `ack` rate.
- **time_type / time_format** default to the captured 3 and 3; the stock
  packer's values are 1 (ETA) and 1/2 (12/24 h). What 3 means is unknown.
- **The end sequence's** unit and tenths nibbles (stock "distanceErr -1"):
  the unit is sent invalid, the tenths 0. Guessed.
- **bar's second marker** 0xE ("err -2") is not modelled; only 0xF.
- **Roundabout codes** 96-106 are exit-angle buckets whose angles are not
  known: the enum names them `roundabout_96` and so on. The destination codes
  113-117 are not named, except 115 `arrived` (112 + 3), a guess.
- **Navi-alive**: the cluster may need `responseNaviAliveStatus` answered
  before it shows guidance. Not modelled.
- **Per-field freshness**: the two clients of `cluster-sub-info` share one
  timeout, so a dead compass client's last heading stays while the navigation
  keeps the message alive. A client that stops should send its fields as
  `null`.
- **Chunked transport**: see Transports.
- **opendbc's Hyundai/Kia DBCs** may name CAN signals the micom relays
  (speed, gear, state of charge). Cross-referencing their layouts and value
  tables against our undecoded frames (0x8D2D, 0x8D43, 0x8D47, the 0x4D17
  pair) could decode some of them. Not done: nothing was fetched.

## Refs

- The NEMESIS report (`nemesis-report.md`, nura-G5W-Android session notes,
  2026-11; not in this repository): frame layouts of
  0x0D02/03/04/11 on `8f 0d 01 0d`, display-type codes, the worked example,
  the end-of-route sequence, the TP header `c3 4d 80 4d`; from stock
  `ClusterService$NaviCluster.smali` and NEMESIS's decompiled
  `GpmNaviTBTManager`.
- The captured `ack` frame: logcat MicomD 2026-10-16.
