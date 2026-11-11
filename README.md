# Warning!

This is LLM generated.
That being said, I still think others might find it interesting, so here it is.

Caveat emptor.

# go-micomd

A Go replacement for `micomd`, the Android daemon that holds the serial link
between a car head unit's SoC and its **micom**, the microcontroller that owns
power sequencing, the vehicle buses and the hard keys. Written for the Kia EV6
head unit (Mobis DAudio Plus Gen5 Wide, Telechips TCC8031) running mainline
Linux in place of Android.

## Why

Something has to keep the conversation going. If the SoC stops answering the
micom's CPU-alive requests, the micom takes it for hung and resets it, after
about 3.5 minutes. A system that replaces Android has to take over that
daemon's job, or it reboots.

## What

A library first (`package micom`, importable by other modules, such as the
[Boot Manager](https://github.com/W-Floyd/bootmanager)) with a thin command,
`cmd/micomd`, around an exported `Main`.

* `micom.OpenTTY`, `micom.New`, `Daemon.Run`: the link. It answers the CPU-alive
  requests, sends a `Profile` of routine frames, and counts what it sees
  (`Daemon.Stats`, `Daemon.Snapshot`).
* `micom.Describe`: names the frames whose meaning is known.
* `micom.DefaultProfile`, `micom.ClockProfile`: everything it will ever send.
  Frames can be sent only if a `Profile` lists them, never composed freely, and
  nothing in it flashes, resets or reconfigures the micom.
* `micom.LoadVariant`, `micom.Variant`: what is car-specific (frame headers,
  keys, climate and other state frames, messages, the routine frames, what the
  control socket may send) is a YAML file per car in `variants/`, embedded.
  A new car is a new file: see [docs/variants.md](docs/variants.md).
* Messages: multi-field frames in both directions, described as data
  ([docs/messages.md](docs/messages.md)). From the micom they are decoded;
  to it (the cluster's turn-by-turn guidance and compass) a client fills
  them in by name over `-input`, and micomd validates, encodes and sends.

Everything comes from captured traffic (logcat's `MicomD` lines) and a
disassembly of the vendor binary, not from a specification: it says only what has
been seen. The protocol is described in the package documentation.

## Command

    micomd -dev /dev/ttyAMA1 [-baud 115200] [-dump] [-clock] [-noalive] [-stats 10s]

`-dump` prints every frame received, decoded where known. `-clock` adds the two
frames that carry the time, from this machine's clock: only use it when that clock
is right, as the car may store it. `-code-profile` sends the variant's
corrections of captured routine frames, known only from stock code (on the EV6:
the cluster's compass as "no heading" instead of a stale captured one).
`-noalive` stops answering the CPU-alive
requests, which makes the micom reset the SoC.

The daemon owns the UART, so nothing else can read it. `-tap /run/micomd.sock`
serves every frame it receives, with the time, on a unix socket (mode 0660); from
another shell, `micomd -attach /run/micomd.sock` prints them live until stopped,
and any number can attach. A reader that cannot keep up loses lines and never
slows the daemon.

`-record /var/log/micomd/frames.jsonl` writes every frame received and sent,
for lining up with other logs (an OBD2 export, a GPS track), one JSON object
per line:

    {"t":1794139200.123456,"start":true,"variant":"kia-ev6-gen5w","version":"3cb52ec…"}
    {"t":1794139200.200001,"dir":"rx","frame":"ff018a01810f000200f9"}
    {"t":1794139200.200150,"dir":"tx","frame":"ff8a0101010f00020079"}
    {"t":1794139201.004000,"dir":"rx","frame":"ff018a01810f00020006","bad":true,"dropped":3}

`t` is the wall clock in seconds; `bad` marks a frame with a bad checksum;
`dropped` counts records lost before the line because the writer fell
behind (the link is never held for it); `mono`, seconds since start, is
added while the clock reads before 2020. The file moves to `PATH.1` before
it would pass `-record-max` (default `32M`; K, M and G suffixes), so the
two take at most twice that. A header opens each file. Writes are buffered
and flushed every second and on exit; a write error stops the recording,
not the daemon. `-query … snapshot` shows its progress.

The control socket (`-ctl /run/micomd.ctl`) also answers read-only queries,
which never send a frame:

    micomd -query /run/micomd.ctl snapshot [-json] [-watch 1s]
    micomd -query /run/micomd.ctl mark

`snapshot` prints the variant running and why (and the micom's own variant
record, once received), the link's counters, then one line per frame kind:
key, name, count, age, latest payload, and the bytes changed since the last
`mark` as `idx:from->to×changes`. `-json` prints the Snapshot as received;
`-watch` repeats the table. `mark` takes the latest payloads as the baseline:
mark, change something on the car, and the bytes that moved are the ones
that mean it.

The car is chosen from the kernel command line's `vehicle_info` (the EV6
otherwise), or by `-variant NAME` or `-variant-file PATH`; `-list-variants`,
`-show-variant` and `-variant-check` inspect them. `-keys` injects the
variant's keys and `-allow-variant` lets the control socket send its captured
frames (`-allow-inferred` adds the inferred ones). A variant that fails to load
leaves the keepalive running. `-identify-request` asks the micom for its
variant record once the link is up (retried, 5 times at most) and logs which
variant it matches against the one running; a mismatch asks for a restart
with `-variant`, as the variant is never switched live.
`-hev-status-request` asks once for HEVService's bulk status (0x8c01), whose
range with the climate on and off is otherwise sent only on change; the
request is stock's, never tried on a car, so it is off by default. Stock's
other start-up reads are opt-in the same way: `-climate-status-request`
(the climate's whole state), `-body-status-request` (the B-CAN validity
check, then the doors, locks and 12 V charge) and `-usm-units-request` (the
car's unit settings). None commands the car. See
[docs/variants.md](docs/variants.md).

`-input /run/micomd-input.sock` takes one JSON object per line,
`{"msg":"tbt-turn","maneuver":"turn_right","distance":0,"distance_tenths":3,"unit":"km"}`,
and answers `{"ok":true}` or `{"error":"..."}`. Clients never write bytes;
the variant's messages say what each may set, and how long it stays on the
wire without input. The EV6's cluster, backlight and shutdown messages come
from stock or micom code, so they also need `-allow-inferred`; the backlight
dim mode and levels, which the micom writes to its EEPROM on every frame,
also need `-allow-persistent` and are rate-limited. See
[docs/messages.md](docs/messages.md).

For a clean power-off a client, on the micom's 0x8108 power state
`power_off_request`, finishes and sends `{"msg":"shutdown-ack","stage":"power_off_request"}`,
and on `rail_off`, `{"msg":"shutdown-ack","stage":"rail_off"}`. The micom
latches an ack across ACC on, so an early one would cut the next ACC-off
cycle short: micomd refuses each ack unless the last power state it
received is the one the ack answers. Details in
[docs/messages.md](docs/messages.md), "Power".

`Main(args, stdout, stderr) int` is the same command for embedding as a
subcommand of a larger multi-call program.

## Licence

MIT: see [LICENSE](LICENSE).

## History

Extracted from the Boot Manager's `internal/micom` and `tools/micomd` (commits
`34d7761` and `b6545ea` there), where it is still embedded as the `-micom` link.
