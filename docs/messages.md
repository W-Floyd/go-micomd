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
  is refused: it would read back as "not available". Decoded, an invalid
  field is `Valid: false` with the marker as its `Value`, as decode: was.
- **Absent** from the input: the field's current value (see Lifecycle), else its
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
- **Fractional scales**: decoded, a field whose `scale` or `offset` is not
  whole (the EV6's state of charge, 0.5 %; remaining energy, 0.002 kWh)
  also carries the exact value as `Field.Float` (JSON `float`), rounded to
  6 decimals; `Value` is it rounded half up, as before.
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
  last one sent, and always on the first input. `on_change: false`: every
  input sends, the same frame too (acks, and frames the micom forgets on a
  reset).
- **every**: while active, the last frame is repeated at that interval,
  counted from the last send.
- **min_interval**: at most one send per interval. An input sooner is
  accepted (`ok`) and held; the newest held frame goes out when the
  interval has passed, or is dropped if a later input brings the frame back
  to the one last sent.
- **persistent**: the micom writes its EEPROM for every frame of this
  message. See Persistent frames.
- **owns**: a profile entry. While M is active that entry is not sent: M's
  frame replaces it, never goes beside it. It must have M's header and
  command. Owning an entry also owns any `opt_in: code` entry that `replaces`
  it, so `-code-profile` and the input never both drive one frame. When M
  ends, the entry resumes. The daemon checks ownership under its write lock:
  once M's first input returns, no frame of the owned entry goes out.
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
  `set` alone, checked at load, so an end always works. Ending an inactive
  message still runs its steps: a client can clear the cluster after a
  restart.

The engine runs off one 50 ms tick, so `every`, `timeout` and
`min_interval` are honoured to within 50 ms.

## Requires

Some frames are safe only in a state the micom reports. A to_micom message
may require the last value micomd received of a from_micom field:

```yaml
requires:
  - { if: { stage: power_off_request }, field: "Power state", in: [ power_off_request ] }
  - { if: { stage: rail_off },          field: "Power state", in: [ rail_off ] }
```

- `field` names a from_micom field, across messages, as telemetry names its
  readings. The 0x8108 power state comes in two shapes (`[00, s, 00]`
  going down, `[01, 05]` on ACC on), which are two messages with one field
  "Power state"; keyed by message, a requirement on the first would never
  see ACC come back. Every from_micom field of that name must have the same
  type and enum.
- `in` lists the values that let the frame go: names of the field's enum,
  or numbers when it has none. A received invalid marker matches nothing.
- `if` (Kaitai's word) limits the requirement to frames whose fields have
  these values, defaults counted; without it, it gates every frame. This is
  how one message carrying both acks gates each value on its own state; a
  field-level form would gate a field, not one of its values.
- An input that fails is refused with the requirement and what was last
  received (`none received yet` before any frame). It is never held for
  later: the state may never come, or come back too late to be right.
- The check runs again whenever the frame would go out: a frame held by
  `min_interval`, a repeat, an end step. The state may have moved since the
  input.
- micomd updates the values as each frame arrives, before `-dump` and the
  tap see it. A requirement can be no more current than the micom's last
  report.
- Checked at load: the field exists on a from_micom message, the `in`
  values are its values, the `if` fields are the message's and their
  values encode; `requires` on a from_micom message is refused.

## Persistent frames

Some frames make the micom write its EEPROM every time it gets one (EV6:
the backlight dim mode, day/night level and manual level). EEPROM endures a
bounded number of writes, and a slider or a loop can spend them fast. Such
a message says `persistent: true`, and then:

- it is sendable only with `-allow-persistent`, on top of the status gate
  (`-allow-inferred` for code ones);
- it is sent only from an input, only when its frame changes: no `every`,
  no `owns`, no `on_change: false`, no `end` of its own, and no other
  message's `end` may name it (an end runs unasked, on a timeout);
- its `min_interval` is at least 1 s, so a burst of inputs is one write.

A marker on the message, with its own flag, rather than a new status:
status says how we know a frame, persistence what it costs the micom, and
the two are independent. The rules are checked at load (`TestLoadVariantRefuses`).

Some frames are not marked but still write EEPROM: the micom logs every
frame of its system and display nodes in an EEPROM ring (the stock system
does the same), and 0x0802, backlight on/off, rewrites a config byte. They
are not configuration a client chooses, so they are not gated; backlight
on/off is spaced by `min_interval: 1s`.

## Power (EV6)

From the micom firmware (V002.208) in emulation, with its defaults; the
car's EEPROM may set other times. The micom reports each step as 0x8108
"Power state" (`-dump`, `-tap`, Snapshot):

| after ACC off | 0x8108 state | micom |
|---|---|---|
| 0 | `acc_off_hold` (4), after 0x8133 `off` | screen-off hold |
| 180 s | `bus_sleep` (3), twice | CAN bus sleep |
| 360 s | `power_off_request` (1) | asks the SoC to power off; held while some modules are busy |
| 450 s | `rail_off` (0) | SoC rail off (modules 0/1) |
| 630 s | (6/7, not seen as a frame) | stop/standby, after keep-awake checks |

ACC on from 4 or 3 sends 0x8133 `on` and `acc_on` (5); the SoC resumes.
From 1 or 0 the micom resets the SoC (a guess, from code).

A clean shutdown, by the client (micomd sends nothing unasked):

1. On `power_off_request`: stop, unmount, then
   `{"msg":"shutdown-ack","stage":"power_off_request"}`. The micom moves to
   `rail_off` at once.
2. On `rail_off`: `{"msg":"shutdown-ack","stage":"rail_off"}`. The micom
   drops its power hold (GPIO 43, a guess) and goes to standby within ms.
3. Without acks nothing breaks: the micom gets there by its own timers.

Keep answering the CPU-alive requests throughout (micomd does).

**The acks are latched and survive ACC on.** One sent early (before its
state) would wait in the micom and cut the next ACC-off cycle short. micomd
enforces the order (`requires`, above): each ack goes out only while the
last 0x8108 state it received is the one it answers, and is refused
otherwise:

```
> {"msg":"shutdown-ack","stage":"power_off_request"}
< {"error":"shutdown-ack: requires Power state in [power_off_request]: it is acc_on"}
```

A client acks when it sees the state (on `-tap`, or in Snapshot), and
retries on refusal only after the next state arrives.

Backlight: off from the micom's power-up until `{"msg":"backlight","on":true}`,
which the micom refuses while its supply is low. A client sends it after
each micom start (link up, or `acc_on` after a reset).

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
{"error":"tbt-turn: end is true and takes no fields"}
{"error":"Variant code: from_micom, not an input"}
{"error":"line is not a JSON object"}
```

## Gates

- Nothing is sent from the input without `-input`.
- `status: captured` messages are sendable once `-input` is given. `code` and
  `inferred` ones also need `-allow-inferred`, the flag that already admits
  such send entries. A message is enabled only if
  every message its `end` names is too.
- `persistent: true` messages also need `-allow-persistent`.
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

**From the phone.** `micom-livi -input SOCKET` reads LIVI's route guidance
(Android Auto's or CarPlay's, `GET /navigation` on LIVI's port 4000) twice a
second and writes these lines: `tbt-guiding` when a route starts, `tbt-turn`
and `cluster-sub-info` (route fields only) on change and every second, and
`{"msg":"tbt-turn","end":true}` when guidance stops, LIVI cannot be read,
and at its start. Its maneuver table (`cmd/micom-livi/nav.go`) maps LIVI's
maneuver types to the display types above; the roundabout's angle codes and
the destination kinds other than `arrived` are not used. `tbt-points`
(the next three steps) is not sent: Android Auto does not give them.

With `-gpsd HOST:PORT` it also fills the compass from gpsd's track, in the
cluster's 7.5° steps: only at `-heading-min-speed` (2 m/s) or above, the
last heading held for `-heading-hold` (10 min) when slower, `null` (0x3f)
without a fix, a track or gpsd. It then sends `cluster-sub-info` without
guidance too, on a step change and every second: the route fields invalid
and time_type and time_format 3, the captured `ack` frame but for the
compass, so the message owns `ack` and the cluster's compass is the car's
heading rather than the capture's 105°.

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
repeat, own, timeout, end, requires); the input socket and its JSON. A variant only
describes.

## Tests

- `TestLoadVariantRefuses`: a row per refusal of the schema.
- `TestFieldCodec`: each codec form, both DBC byte orders, encoded and read
  back.
- `TestInput`: input lines, received frames and engine ticks against a test
  variant, and everything sent, owned and resumed; requires per value, and
  again when a held frame goes out.
- `TestServeInput`: the socket and a running daemon, the owned profile entry
  off the wire while its message is active, a requirement met by a frame
  the daemon received.
- `TestEV6Messages`: the cluster messages' frames, whole with checksums (the
  report's worked example, the captured `ack`, the end of route), each read
  back by `DecodeInput` into an input that encodes to it again; the power
  and backlight frames as micom-run took them; the shutdown acks refused
  before any power state, at 5/4/3, and after ACC returns, and sent at their
  own; which messages each gate admits.
- The EV6 equivalence tests: what the Go tables read is read the same.

## Open questions
- **Power, from a car**: the EEPROM's power settings and timers, what stock
  sends at each 0x8108 state and when it acks, GPIO 43, the 0x810a trigger
  and the CAN meaning of 0x810b/0x810d, auto day/night, and stock 0x0801,
  0x0807 and 0x081A traffic all need a capture.

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
- **Derived values**: Android's amp type is 0 internal if the variant code's
  W29, else 2 Lexicon if W21, else 1 Actune. A field reads one place, so the
  variant code keeps the two bits ("Internal amp", "External amp"). A
  derived field (a value table keyed by several fields) would express it;
  not added for one use.
- **Variant code bytes 9-12**: the CPU-variant bytes 9-11 are opaque to
  Android (Engineering Mode writes them); ADM reads 0 off / 1 on, but the
  firmware sets bit 7, which reads as 128.
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
