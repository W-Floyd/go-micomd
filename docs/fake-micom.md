# micom-fake: an emulated micom

`cmd/micom-fake` plays the micom's end of the SoC's serial link, so an emulated head unit gets a car:
the stock system under QEMU (`tools/tcc803x-qemu` in LIVI, its micomd on `/dev/ttyTCC2`) or LIVI's
own go-micomd in a VM. It never opens a real micom or CAN interface: it is the micom.

## Run

```sh
go build -o micom-fake ./cmd/micom-fake
# QEMU serves the UART on a socket (tcc803x-qemu: MICOM=unix:$SOCK,server=on,wait=off)
micom-fake -connect unix:$SOCK -replay $CAPTURE -loop -record $OUT.jsonl -ctl $CTL
micom-fake -connect unix:$SOCK -replay $CAPTURE -still 30s -scenario cmd/micom-fake/scenarios/ev6-parked.json
micom-fake -connect unix:$SOCK -scenario cmd/micom-fake/scenarios/ev6-parked.json   # no capture at all
echo '{"msg": "Charge limit, AC", "set": {"Charge limit, AC": 70}}' | nc -U $CTL
```

- `-replay` takes a `micomd -record` file and sends its `rx` frames at their times (`-from`, `-to`,
  `-speed`, `-loop`). `-still T` instead holds the car's state at `T` into it: the latest frame of each
  kind, each repeated at the rate it came. Captures carry the car's identity: keep them out of git.
- `-scenario` is JSON entries `{"msg": <from_micom message>, "set": {<field>: <value>}, "every": "10s"}`
  or `{"frame": "<hex>"}`, values as micomd's `-input` takes them (units, enum names). A `Boot record
  (full)` entry answers the hello. `-ctl` takes the same entries, one per line, sent at once.
- `-connect` dials (a QEMU `server=on` chardev, `tty:` a pty), `-listen` waits; it reconnects.
- `-record` logs both directions in `-record` format seen from the SoC (`rx` = sent by the fake).

## What it does

| | |
|---|---|
| link | 115200 8N1 framing (`frame.go`). Stock micomd sends the hello `8a 01 01 01` cmd 01 every 2 s, five times; the fake answers with the boot record (`01 8a 01 81` cmd 01, 149 bytes; its byte 120, the CAN DB generation, picks stock's layouts) and also sends it at connect, as the car's micom does unasked. Then the CPU-alive request every 15 s. |
| car data | the capture's frames, or the scenario's, encoded by the variant (`Variant.EncodeReceived`) |
| reads | an empty SoC frame (stock's start-up reads 0x0C01, 0x0C4A-0x0C55, 0x0C5F, the 0x0C5C poll) is answered on the reply header (`ReplyHeader`) with the frame held, else the variant's longest message for it filled from the fields held by name (the 0x8C01 bulk from the items) |
| writes | the variant's `reflects:` (docs/messages.md): ev-charge-limits, ev-charging-current, ev-off-peak, ev-departures, batt-cond-mode, ev-v2l-limit come back as the readbacks stock decodes; from then on the replay sends the written state for those frames, not the capture's |
| everything else | logged once per change, named by the variant |

Seen with stock V015 under QEMU (2026-11): `onMicomReady` runs, the EV app and launcher show the
capture's state of charge and range, the EV settings' charge limits, AC current and battery
conditioning round-trip (writes `0C18 00 00 c0 00 ff 8c ff` for DC 70 %, `0C5B 02` then `00` for the
second current, `0C62 02` then `00`). The schedules round-trip too once the system time is real (GPS
from go-hippod's `hippo-fake`; the EV page enables its departure and schedule tiles only then) and the
driving regulation is off (`0x8103 [00]`, `Regulation inputs` in P; in North America the dialogs refuse
under it): departure 1 12:00 AM Mon Wed `0C06 00 a1 ff 00 27 13 80`, off-peak 10 PM-6 AM charge only
`0C05 b0 30 00 00 c0` (prioritize `.. 80`), each shown back by stock from the reflected readbacks.
`scenarios/ev6-parked.json` sets the regulation off.

## Faked or missing

- No CAN layer: the frames are the micom's, not the buses'. Nothing simulates the car (a charge does not
  progress unless the capture does; a departure timer does not start anything).
- The answers to reads not in the capture are built from the variant's layouts, zero where no field is
  known; stock may read bytes the variant does not name.
- No variant code (0x8117), power-state, ACC or shutdown sequencing; no hard keys unless a capture has them.
- `reflects:` copies raw codes: the temperature, minute and mode encodings of the schedules are as stock
  packs them, not checked against a car.

## Unknown

- Whether the car accepts the writes as packed, and what it reports back (layouts from stock code only).
- Scheduled charging mode 1; the schedule temperature beyond two points (19 = 75 °F, 20 = 76 °F with unit 1);
  the departure 2-bit day values other than 0/1; the car's computed charge window (0x8C54), which the fake
  does not compute.
- Winter mode values; the V2L limit's encoding; utility mode's on value (not modelled: it changes the car's power state).
- What stock does with fields of the boot record the variant leaves at 0.
