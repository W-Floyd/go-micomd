# Variants

Everything car-specific in go-micomd is a variant: a YAML file in
`variants/`, embedded in the binary with `go:embed`. Supporting another car is
a pull request that adds one file, usually extending an existing one and
listing only what differs. The link itself is code and never depends on a
variant: the framing, the checksum, and the CPU-alive exchange. The micom
resets the SoC about 3.5 minutes after its alive requests go unanswered.

Statuses used below: **captured** means seen on the wire or on the live unit,
**code** means read from stock code, and **inferred** means worked out from
code or the frame rule.

## How the stock system knows the car

Three layers. The first is there before Linux starts, so micomd has it before
it says anything to the micom.

1. **Bootloader → kernel command line.**
   - The stock command line of the EV6 has `vehicle_info=0xcc country_info=0x01
     daudio_hw_ver=526087 cabin_camera_type=0` (captured).
   - Mobis' LK source for the earlier TCC897x unit shows where they come from
     (`gh-c457/lk/target/tcc897x-lcn/mobis/mobis_getdata_micom.c`, `init.c`):
     - LK raises GPIO B11 and reads an 8-byte record from the micom UART:
       `'b' 's' 'p'`, vehicle code, country code, flags (OEM, modem, engine
       type), an unknown byte, then an NMEA XOR checksum.
     - It passes the two codes as `vehicle_info` and `country_info`, or `0xff`
       when the read fails.
     - Countries: 0 KOR, 1 USA, 2 CHN, 3 GEN, 4 MIE, 5 EUR, 6 CAN, …
   - The G5W bootloader is inferred to do the same, because it writes the same
     keys.
   - 0xcc is only known to be this EV6: the G5W vehicle-code table is in no
     source we have.
2. **Android: the micom's variant coding.**
   - `SystemService.requestVariantCode()` is `sendPacket(0x8a, 1, 0x0117)`,
     retried up to 5 times. Its framing is inferred from the captured 0x010F
     alive reply: `ff 8a 01 01 01 17 00 01 62`.
   - The reply is inferred to be func 0x8117. It is cached in
     `/data/system/variantcoding.dat` and broadcast as
     `AV_VARIANT_CODING_CHANGED`.
   - Its fields are listed as format strings in the stock micomd: navigation,
     modem, AmpType, rearMonitor, radioRegion, RHD, Limo, Lexicon, AMP,
     Camera, advancedCluster, HUD, rearUSB, RRC, CDC, rearMIC, HD, DMB, CMMB,
     InternalCDP, ExternalCDP, Telematics. No code references those strings.
   - `SystemService.onVariantCode` splits the 13 bytes (EV6 V015.012
     automotive-service; the 0x0118 save packs with the same masks):
     vehicle, country, a 32-bit word of option bits (navigation, modem,
     internal amp, radio region, camera, Lexicon amp, telematics, RDS, …),
     the DVD region, the language, three CPU-variant bytes opaque to
     Android, and ADM. hkmc-gen5w decodes them all.
   - The indices code reads (4 amp, 23, 31) are `VariantCoding` indices, not
     bit positions: 23 is telematics, 31 RDS, and the modem is 3. Index 4,
     the amp type, is derived: 0 internal if W29, else 2 Lexicon if W21,
     else 1 Actune.
   - The key mapping changes with the navigation, modem and CMMB codes.
3. **The climate (DATC) variant.** It is DATC item 17, cached in
   `persist.sys.climate.variant`, which is 2 on the EV6. `persist.sys.amptype`
   is 2 on the EV6, an external amp.

The capture so far starts mid-session, so the 0x8117 reply has not been seen.
A `micomd -attach` from power-on (read-only) would show it.

Our kexec chain drops the stock command line, so on the unit layer 1 is
empty and `auto` falls back to the default. `-identify-request` asks the
micom instead, as stock Android does: once the first frame arrives it sends
the 0x0117 request, again every 3 s until a 0x8117 arrives, 5 times at most.
That request is the only frame it adds, and the flag alone allows it (it is
inferred, but needs no `-allow-inferred`). The reply's fields named by
`identify.reply.identity` (on HKMC its Vehicle and Country, the codes the
bootloader passes as `vehicle_info` and `country_info`) are matched like the
command line, and logged against the variant running: `the micom reports
vehicle_info=0xcc …, variant X; running Y: restart with -variant X` on a
mismatch. A named car is not switched live: the profile's senders, the
allowlist, the virtual keyboard's keys and the input's messages are all made
from it at start, and a variant changed under them would change what is
sent with nobody reviewing it. A family is the exception (below): the
record picks one of its own embedded cars, and micomd starts over on it as
if it had been named. `micomd -query SOCKET snapshot` shows the record and
that verdict.

## Choosing the variant

- `-variant auto` (the default): read `/proc/cmdline` and match each embedded
  variant's `match.cmdline`.
  - Every key a rule lists must match.
  - The rule with the most keys wins.
  - A tie is an error, and a test fails on it for the embedded files.
  - No match means the default, `kia-ev6-gen5w`. That is today's behaviour,
    so the deployed unit runs unchanged even when its command line lacks
    `vehicle_info`.
- `-variant NAME`: an embedded variant. If the command line matches another,
  micomd logs a warning.
- `-variant FAMILY`: an abstract variant (`hkmc-gen5w`): the car is known
  to be one of its descendants, not which.
  - A command line that matches a descendant chooses it.
  - Otherwise micomd runs the family, sends the identify request as
    `-identify-request` would, and once the record matches a descendant,
    rebuilds everything on that car: profile, allowlist, keys and input. The
    link and the sockets stay open throughout, so the alive answers never
    stop, socket clients stay connected (the car's daemon answers them) and
    keep the sockets' group and mode, and only a frame split across the
    switch can be lost. The flags apply to the car as if it had been named.
  - Until then, an `-input` line naming a message the family lacks is held,
    with its reply (16 lines at most; beyond, refused at once), then tried on
    the car's engine; refused if the record names no descendant or the
    requests go unanswered. A reply can so wait out the identify retries
    (5 x 3 s); a client that times out first should resend its state.
  - A record that matches no descendant, or none at all, leaves the family
    running: its keepalive and decoding, and nothing it does not declare.
- `-variant-file PATH`: a file on disk, for development or an unlisted car. It
  may `extends:` an embedded variant. It is logged as unreviewed.
- `-list-variants`, `-show-variant` (the resolved variant, parents merged in)
  and `-variant-check [PATH...]` print and exit without a device.
- On a running daemon, `micomd -query CTL_SOCKET snapshot` prints which
  variant runs and why: its source (`file`, `flag`, `cmdline`, `default`),
  the command line's identity keys and what they match, any load error that
  left the keepalive only, and the micom's variant record once received.

**A variant that fails to load never stops micomd.** That covers a missing
file, a malformed or unsafe one, and an unknown name. micomd logs the error
and runs with the keepalive only: no profile, no names, nothing a variant
would add. `TestMainKeepsAliveWhateverTheVariant` checks the alive answer on
each of those paths.

What a variant supplies:

| | without flags | opt-in |
|---|---|---|
| frame names and decoding (`-dump`, `-tap`, Snapshot) | yes | |
| the routine profile | yes | `-clock` adds its `opt_in: clock` frames; `-code-profile` swaps in its `opt_in: code` ones |
| key injection | no (as before: `-keyfunc`) | `-keys`; `-keyfunc` and `-keymap` override entry by entry |
| control-socket sends | no (as before: `-allow`) | `-allow-variant` (captured); `-allow-inferred` adds inferred and code entries |
| the 0x0117 variant-code request | never | `-identify-request` (alone, though inferred) |
| the 0x0c01 HEV status request | never | `-hev-status-request` (alone, though inferred) |
| to_micom messages (cluster TBT, compass, backlight, shutdown acks) | never | `-input SOCKET` (captured); `-allow-inferred` adds code and inferred ones; `-allow-persistent` those that write the micom's EEPROM ([messages.md](messages.md)) |

The device unit's flags (`-keyfunc 0x8701 -keymap … -allow …`) work as
before. `-keys -allow-variant -allow-inferred` gives the same keymap and
allowlist from the EV6 file; the tests prove it.

## The file

Decoded with go.yaml.in/yaml/v4 using `WithKnownFields()` and
`WithUniqueKeys()`, so an unknown or repeated key is an error. Numbers may be
decimal or `0x` hex. A header is `"hh hh hh hh"` or the name of one in
`headers`.

```yaml
schema: 1                    # required
name: kia-ev6-gen5w          # required; [a-z0-9-]; an embedded file's must be its file name
title: "Kia EV6 (CV), ..."   # one line, for -list-variants
extends: hkmc-gen5w-ev       # one parent, embedded; at most 4 deep, no cycles
abstract: false              # true: a family layer, never matched

match:                       # not inherited
  cmdline:                   # vehicle_info, country_info, daudio_hw_ver, cabin_camera_type
    vehicle_info: 0xcc       # a number, or a list: any of
  status: captured           # required here
  ref: "stock /proc/cmdline, 2026-11-07"   # required here

expect:                      # amp_type, climate_variant: what the micom should report.
  climate_variant: 2         # Recorded and shown; not checked yet (no decoder)

identify:
  request:                   # a send entry (rules below); sent only with -identify-request
    name: variant-code-request
    frame: "ff 8a 01 01 01 17 00 01 62"
    status: inferred
    ref: "SystemService.requestVariantCode()"
  reply:                     # named; hkmc-gen5w decodes it
    name: "Variant code"
    hdr: system_rx
    cmd: 0x17
    identity: { vehicle_info: Vehicle }   # identity key: a field of the reply, matched like the command line

headers:                     # dir: from_micom | to_micom; status and ref optional
  keys: { hdr: "07 84 01 87", dir: from_micom, status: captured }

keys:
  frame: { hdr: keys, cmd: 0x01 }        # Func 0x8701; what -keyfunc gave
  push:                                  # type 0-7, code 0-15; label for -dump
    - { type: 2, code: 6, label: "Wheel volume up", key: KEY_VOLUMEUP }   # KEY_* name or number
  knobs:                                 # type 8-11; a step is a tap of up or down
    - { type: 9, code: 1, label: "Volume knob", up: KEY_VOLUMEUP, down: KEY_VOLUMEDOWN }

signals:                     # one-byte state frames: a line of text, and maybe CarClimate state
  - name: climate-driver-set # unique; children replace or remove (remove: true) by name
    hdr: climate_panel
    cmd: [0x05, 0x07]        # one or a list; one signal per header and command
    len: 1                   # payload length (default 1); 0: any, read from the first byte
    label: "Climate driver set point"
    # exactly one kind:
    #   flag:   { on: B, off: B or "*", on_text: on, off_text: off }         state: bool fields
    #   enum:   { B: text, ... }                                             state: airflow
    #   number: { add: N, mul: 1, range: [lo, hi], format: "%d F", wrap: false }
    #           (v*mul+add; wrap takes it modulo 256)                        state: int fields
    #   fan:    { off: B, auto: B, levels: [lo, hi] }                        state: fan
    #   bits:   { always: B, none: text, names: { bit: text } }
    #   burst:  { begin: B }     # ClimateBegin on begin, ClimateEnd otherwise
    number: { add: 56, format: "%d F" }
    values: { 0x00: { text: "off", state: 0 } }   # specials first; state only for a number
    # lines: { B: "whole line" }; unknown: "code %#02x" (default; "" = no line);
    # unknown_line: "whole line %#02x"; suffix: " (repeat)"
    state: driver_set_f      # driver_set_f passenger_set_f driver_seat_heat
                             # passenger_seat_heat airflow auto ac driver_only
                             # recirculation zones_synced fan
    status: captured

enums:                       # value tables for message fields: name -> code
  distance_unit: { m: 0, km: 1, mi: 2, ft: 3, yd: 4 }

messages:                    # multi-field frames, both directions: docs/messages.md
  - name: "EV power block"   # from_micom: Decode, Snapshot readings, Describe, FrameName
    dir: from_micom
    hdr: "0c 88 ?? 8c"       # a from_micom message may leave header bytes open (??)
    cmd: 0x5c
    min_length: 29           # or length: N, exact
    when: [ { off: 0, eq: 0x57 } ]     # optional guards
    fields:
      - { name: "Climate total", off: 10, type: u16, invalid: 0xffff, unit: " W" }
    status: code
    ref: "HEVService"
  - name: tbt-turn           # to_micom: filled in from -input, with a lifecycle
    dir: to_micom
    hdr: cluster_tx
    cmd: 0x04
    length: 8
    fields: [ { name: maneuver, off: 0, enum: maneuver }, ... ]
    every: 1s
    timeout: 30s
    end: [ { msg: tbt-turn, set: { maneuver: none } } ]
    status: code
    ref: "..."

profile:                     # the only frames sent unasked: status must be captured
  - name: poll
    every: 1s
    frame: "ff 88 0c 01 0c 5c 00 01 2b"
    status: captured
    ref: "logcat MicomD 2026-10-16"
  - name: clock-sec
    every: 1s
    opt_in: clock            # only with -clock
    template: "ff 88 0c 01 0c 09 00 06 {weekday_bit} {hour_shl3} {minute} {second} 80 .."
    # placeholders: {second} {minute} {hour} {day} {month} {year2}, {weekday_bit}
    # (Sunday bit 0, Saturday bit 1 ... Monday bit 6), {hour_shl3}; payload only
    status: captured
    ref: "..."
  - name: tbt-sub-info       # a captured frame corrected from stock code
    every: 600ms
    frame: "ff 8f 0d 01 0d 03 00 0a ff ff ff 3f 03 ff ff 03 ff 47"
    opt_in: code             # only with -code-profile, in place of...
    replaces: ack            # ...this routine entry (same header and command)
    status: code             # the one profile status besides captured
    ref: "..."

send:                        # what the control socket may send, only when asked
  - { name: av-mute-set, frame: "ff 85 02 01 02 0b 00 03 02 01 70", status: captured, ref: "strace" }
  - name: main-volume
    pattern: "ff 85 02 01 02 03 00 02 [00-23] .."  # ranges over payload bytes, checksum ..
    status: captured
    ref: "strace"
```

Inheritance runs root first:

- Scalars override.
- `headers`, `expect` and `enums` merge key by key (an enum table whole).
- `identify` merges by request and by reply.
- Named lists (`signals`, `messages`, `profile`, `send`, and `keys.push` and
  `keys.knobs` by type.code) merge by name: the same name replaces the
  parent's entry, and `remove: true` deletes it.
- A frame decodes as the first `from_micom` message that matches it, so one
  that an earlier message matches every frame of is refused. To read more of
  a parent's frame, redefine its message by name with the extra fields.
- A name used twice in one file is an error.
- `match` and `abstract` are not inherited.

## Safety

A file can make frames sendable. Nothing in it can send arbitrary frames:

1. **Only payload bytes are computed, and only from declared fields.**
   - A sendable frame's header, command and length are fixed and declared.
     Only payload bytes vary, so the load-time checks below (declared
     headers, no CPU-alive, no dangerous profile frame) hold for every frame
     that can be sent.
   - A whole frame must parse with its checksum.
   - A pattern may range over payload bytes only, as `ParsePattern` already
     demands.
   - A template's placeholders are the clock fields, in the payload.
   - A to_micom message's payload is its declared fields. Each comes from an
     input or is computed (`from:` the clock or a received field), and each
     value is checked at every send against the field's type, range and enum
     ([messages.md](messages.md)).
2. **Only the three send paths.**
   - to_micom messages go out only from `-input`, and code or inferred
     ones only with `-allow-inferred`. One may stand in for a profile entry
     of the same header and command (`owns`) while a client feeds it.
   - The profile, sent unasked, takes `status: captured` frames only. The one
     exception is `status: code` with `opt_in: code`: a correction of a
     captured entry. It names that entry in `replaces`, which must be the
     same header and command, and is sent only with `-code-profile`.
   - Send entries go out only when asked on the control socket, and only with
     `-allow-variant`.
   - Inferred and code entries additionally need `-allow-inferred`.
3. **Headers.**
   - A profile, send or to_micom message frame's header must be a
     `to_micom` header the variant declares.
   - No variant may name the CPU-alive request or reply: that exchange is
     micomd's.
4. **Dangerous frames.** A send or to_micom message may carry a `dangerous`
   string (any non-empty value): it marks a frame that resets, rewrites or
   reconfigures the micom, and the string says why (shown in refusals). Such a
   frame is defined and documented like any other, but refused on every path
   — `Daemon.Send`, the control socket, the input, and `-allow`/`-allow-variant`
   — unless `-allow-dangerous` is passed (every one) or `-allow-dangerous-msg`
   names its entry (those named only), and it may never be a profile frame
   (sent unasked). The SystemService requests on `8a 01 01 01` that were once a
   hardcoded denylist are now `dangerous` entries in the base variant: func
   0x0102 SYS_POWER_C (power config, EEPROM), 0x0105 rear camera reset, 0x0107
   last-info init / factory reset, 0x0118 variant code save, 0x011e SPI sending
   stop, 0x0128 VBus off/on; the EV6 adds 0x0157 / 0x018d update / download
   reservations and 0x0167 sleep / next-boot control. Enforced at load (no
   dangerous profile frame) and on every send.
5. **Provenance.** `status` and `ref` are required on every send, profile,
   to_micom message and match entry.
6. **Tests.**
   - Every embedded file loads, and no two match ambiguously.
   - `TestLoadVariantRefuses` holds a row per refusal.
   - The EV6 equivalence tests pin its profile bytes, keymap and allowlist to
     today's, so a PR that widens what the EV6 may send fails them.

## Code and data

- **Data:**
  - headers
  - the key frame, labels and default keys
  - the signals (climate, seat heat, windows, doors, open doors, rear camera)
  - the messages: the variant code and HEV layouts, the cluster's TBT and
    compass, and their enums
  - the profile frames
  - the send frames and patterns
  - match, expect, and the identify request and reply
- **Code:**
  - framing, checksum, `Decoder`
  - the CPU-alive exchange
  - `Daemon.Send`'s allowlist and the dangerous-frame gate
  - the six signal kinds, the field codecs and template placeholders
  - the input socket and the messages' lifecycle
  - the key payload rule (type, code, press) and the `KEY_*` table
  - uinput, tap, ctl, and the command-line identity
  - `CarClimate`'s fields
  - `Stats.Countdown`, which still reads the EV's 0x8c02 frame by its header,
    as before

Files:

- `variant.go`: embedding, loading, merging, checking and compiling.
- `variant_yaml.go`: the file as written.
- `variant_use.go`: Describe, Decode, ApplyClimate, the profile and the sends.
- `message.go`, `codec.go`: messages, enums and their field codecs.
- `input.go`: the input socket and the lifecycle engine.
- `signal.go`: the six signal kinds.
- `identity.go`: the command-line identity.
- `keynames.go`: the `KEY_*` table.

The package-level `Describe`, `Decode`, `FrameName`, `DefaultProfile`,
`ClockProfile` and `CarClimate.Apply` read `DefaultVariant()`, the EV6, so
their callers (micom-livi, the Boot Manager) are unchanged. `Config.Variant`
gives a Daemon another car.

## How the EV6 moved, and how it is kept equal

The EV6's Go tables were moved to `kia-ev6-gen5w.yaml`, with HEVService's
layouts in `hkmc-gen5w-ev.yaml`. The Go code they replaced is kept,
renamed, in `legacy_test.go`. `variant_ev6_test.go` runs both, and the
package functions, over:

- every header the code knew, plus a few it did not;
- every command;
- every one-byte payload;
- samples of the longer payloads.

Describe, carDescribe, Decode, FrameName and Apply (from an empty and a full
state) must agree. The profiles must build the same bytes. The keymap, key
frame and allowlist must equal the device unit's flags.

Running these tests turned up three quirks the data keeps:

- The seat heat frames are read whatever their length (`len: 0`).
- The auto level reads 00 as 255 (`wrap: true`).
- The HEV frames match whatever the header's third byte (`??`).

Two differences are deliberate:

- The variant-code reply (01 8a 01 81, cmd 17) now has a name.
- clock-sec carries the minute: its third byte was a fixed 01, the minute of
  the capture it was read from. The stock frames of 2026-11-07 show it is
  the minute. This changes `-clock` only.

The HEV and variant-code layouts later moved from `decode:` to `messages:`
([messages.md](messages.md)); these tests passed unchanged.

## Size

micomd for linux/arm, `CGO_ENABLED=0`:

| | plain | `-s -w` |
|---|---|---|
| before variants | 3,912,594 B | 2,621,600 B |
| with variants | 5,624,084 B | 3,866,784 B |
| with messages and `-input` | 6,671,530 B | 4,587,680 B |

Variants are about +1.2 MB. go.yaml.in/yaml/v4 brings its plugins, `regexp`
and `encoding/base64`. Its encoder, used only by `-show-variant`, is 131 KB of
that. If this is too much for the u-root initramfs, one option is a
`go generate` step that converts `variants/*.yaml` to JSON at build time,
with the YAML loader behind a build tag. `-variant-file` would then take JSON
in that build.

Messages are +720 KB stripped: 130 KB the codecs, 590 KB `encoding/json`
for the input's lines. A program that already links `encoding/json` (the
Boot Manager's Snapshot, micom-livi) pays only the first. A hand parser of
flat JSON objects would save the rest; it was not written, as the standard
decoder is the safer reader of untrusted lines.

## Not done yet

- **Checking `expect` against the variant-code reply (0x8117).** The reply
  is decoded field by field (hkmc-gen5w), by stock Android's bit map, but not
  yet seen from a car. The climate variant is not in it: stock sets
  `persist.sys.climate.variant` from DATCService's `DatcBypassStatus`, a
  climate frame, so `expect: climate_variant` cannot be checked from 0x8117.
  The amp type is derived from two bits (internal, Lexicon), which a field
  cannot express ([messages.md](messages.md)), and whether
  `persist.sys.amptype` holds that code is unconfirmed. Nothing is checked.
- **Sequences.** These would be named orders of send entries for `-send`. The
  daemon would never run one, but `-send` would have to load a variant to
  expand them, so they were left out. A to_micom message's `end` is the one
  sequence the daemon runs.
- **micom-livi.** It reads frames with the default variant's
  `CarClimate.Apply` and `Decode` (speed, state of charge, remaining
  energy, range, outdoor temperature, odometer and illumination, by field
  name; the range with the climate off as a sum of two). It has no
  `-variant` yet.
- **The 0x8101 boot record.** The micom sends it only in answer to
  boot-mode-request, which `-boot-mode-request` sends once the link is up.
  Its byte 4 (illumination) and 0x12 (dim mode) are decoded; the other 148
  bytes are not. Without the flag the illumination is known only after the
  micom's first 0x810d, sent on change. The request also clears the micom's
  boot-reason flags, as stock's does at every boot.
- **The 0x8c01 HEV status.** HEVService's range with the climate on and
  off (0x8c02 ids 0x2a and 0x25) comes only on change, so a fresh micomd
  parked may see none for a long time. `-hev-status-request` sends stock's
  requestInitHevStatus (0x0c01, empty) once the link is up; the bulk 0x8c01
  answer carries both at 0x3c (u16) and 0x36. Neither frame has been seen
  on a car: the request is derived from stock code, the reply's offsets too.
- **Unknown cars.** On an unknown command line micomd runs the EV6, as before.
  Running the keepalive-only base instead is a behaviour change for its own
  PR.
- **Power from a car.** The EV6's power, ACC and backlight frames come from
  the micom firmware in emulation; the ACC-off times are its defaults, and
  what stock Android sends at each step is not captured. micomd decodes the
  states and sends each shutdown ack only in the state it answers
  ([messages.md](messages.md), "Power"); when to ack is the client's.
