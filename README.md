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

Everything comes from captured traffic (logcat's `MicomD` lines) and a
disassembly of the vendor binary, not from a specification: it says only what has
been seen. The protocol is described in the package documentation.

## Command

    micomd -dev /dev/ttyAMA1 [-baud 115200] [-dump] [-clock] [-noalive] [-stats 10s]

`-dump` prints every frame received, decoded where known. `-clock` adds the two
frames that carry the time, from this machine's clock: only use it when that clock
is right, as the car may store it. `-noalive` stops answering the CPU-alive
requests, which makes the micom reset the SoC.

`Main(args, stdout, stderr) int` is the same command for embedding as a
subcommand of a larger multi-call program.

## Licence

MIT: see [LICENSE](LICENSE).

## History

Extracted from the Boot Manager's `internal/micom` and `tools/micomd` (commits
`34d7761` and `b6545ea` there), where it is still embedded as the `-micom` link.
