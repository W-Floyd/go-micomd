// Package micom speaks the serial protocol between the head unit's SoC and
// its micom, the microcontroller that owns power sequencing, the vehicle
// buses and the hard keys. On the stock system the Android daemon micomd
// holds the UART; a stage that replaces Android has to take that
// conversation over, or the micom takes the SoC for hung and resets it.
//
// Everything here comes from captured traffic (logcat's MicomD lines), not a
// specification, and the package is built to say only what has been seen:
// frames can be sent only if a Profile lists them, never composed freely.
// Nothing in it flashes, resets or reconfigures the micom.
//
// # Link
//
// 115200 baud, 8N1, raw, no flow control, on the SoC's PL011 at 0x16620000
// (the vendor's /dev/ttyTCC2). That is what micomd sets: its termios setup
// leaves c_cflag at 0x1002 (B115200) with CS8, found by disassembling the
// binary from the V015.012 system image.
//
// # Frame
//
//	ff | hdr[4] | cmd | len[2, big endian] | payload | checksum
//
// len counts the payload and the checksum; checksum is the complement of
// the XOR of every byte after the leading ff up to the checksum. Verified on
// every frame of a 46-frame capture, both directions.
package micom

import "fmt"

// Start begins every frame.
const Start = 0xff

// MaxPayload bounds what a frame may carry; the longest seen is 31 bytes.
const MaxPayload = 512

// Frame is a decoded frame. Hdr is kept opaque: of its four bytes the first
// two look like peer addresses (0c 88 from the micom, 88 0c to it) and the
// last is derived from them, but that is a reading, not a fact.
type Frame struct {
	Hdr     [4]byte
	Cmd     byte
	Payload []byte
}

// Checksum is the frame checksum of b, the bytes after the leading Start up
// to, not including, the checksum itself.
func Checksum(b []byte) byte {
	var x byte
	for _, c := range b {
		x ^= c
	}
	return ^x
}

// Encode is the frame as it goes on the wire.
func (f Frame) Encode() []byte {
	n := len(f.Payload) + 1
	out := make([]byte, 0, 8+n)
	out = append(out, Start)
	out = append(out, f.Hdr[:]...)
	out = append(out, f.Cmd, byte(n>>8), byte(n))
	out = append(out, f.Payload...)
	return append(out, Checksum(out[1:]))
}

func (f Frame) String() string {
	return fmt.Sprintf("hdr=%02x cmd=%02x payload=%x", f.Hdr, f.Cmd, f.Payload)
}

// Decoder turns a byte stream into frames. It resynchronises on the next
// Start after garbage or a bad checksum, as a link that was already running
// when it was opened needs.
type Decoder struct {
	buf []byte
	// Dropped counts bytes discarded while looking for a frame, and BadSum
	// the frames refused for their checksum.
	Dropped, BadSum int
}

// Write adds bytes and returns the frames now complete.
func (d *Decoder) Write(p []byte) []Frame {
	d.buf = append(d.buf, p...)
	var out []Frame
	for {
		i := 0
		for i < len(d.buf) && d.buf[i] != Start {
			i++
		}
		d.Dropped += i
		d.buf = d.buf[i:]
		if len(d.buf) < 8 {
			return out
		}
		n := int(d.buf[6])<<8 | int(d.buf[7])
		if n < 1 || n > MaxPayload+1 {
			d.Dropped++
			d.buf = d.buf[1:]
			continue
		}
		total := 8 + n
		if len(d.buf) < total {
			return out
		}
		if Checksum(d.buf[1:total-1]) != d.buf[total-1] {
			d.BadSum++
			d.Dropped++
			d.buf = d.buf[1:]
			continue
		}
		f := Frame{Cmd: d.buf[5], Payload: append([]byte(nil), d.buf[8:total-1]...)}
		copy(f.Hdr[:], d.buf[1:5])
		out = append(out, f)
		d.buf = d.buf[total:]
	}
}
