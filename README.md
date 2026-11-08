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

The car is chosen from the kernel command line's `vehicle_info` (the EV6
otherwise), or by `-variant NAME` or `-variant-file PATH`; `-list-variants`,
`-show-variant` and `-variant-check` inspect them. `-keys` injects the
variant's keys and `-allow-variant` lets the control socket send its captured
frames (`-allow-inferred` adds the inferred ones). A variant that fails to load
leaves the keepalive running.

`-input /run/micomd-input.sock` takes one JSON object per line,
`{"msg":"tbt-turn","maneuver":"turn_right","distance":0,"distance_tenths":3,"unit":"km"}`,
and answers `{"ok":true}` or `{"error":"..."}`. Clients never write bytes;
the variant's messages say what each may set, and how long it stays on the
wire without input. The EV6's cluster, backlight and shutdown messages come
from stock or micom code, so they also need `-allow-inferred`; the backlight
dim mode and levels, which the micom writes to its EEPROM on every frame,
also need `-allow-persistent` and are rate-limited. See
[docs/messages.md](docs/messages.md).

A clean power-off is the client's job: on the micom's 0x8108 power state
`power_off_request`, finish and send `{"msg":"shutdown-ack","stage":"power_off_request"}`;
on `rail_off`, `{"msg":"shutdown-ack","stage":"rail_off"}`. Never earlier:
the micom latches an ack across ACC on, so an early one cuts the next
ACC-off cycle short. Details in [docs/messages.md](docs/messages.md), "Power".

`Main(args, stdout, stderr) int` is the same command for embedding as a
subcommand of a larger multi-call program.

## Licence

MIT: see [LICENSE](LICENSE).

## History

Extracted from the Boot Manager's `internal/micom` and `tools/micomd` (commits
`34d7761` and `b6545ea` there), where it is still embedded as the `-micom` link.
