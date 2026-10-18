package micom

import "time"

// Periodic is one frame the daemon sends on a schedule. A Profile is the
// whole of what a Daemon may ever put on the wire: Send refuses anything
// that is not one of its entries (same header and command).
type Periodic struct {
	Name  string
	Every time.Duration
	// Build makes the frame for the moment now.
	Build func(now time.Time) Frame
}

// Headers seen in the captured traffic. "To micom" headers are those of the
// frames Android sent.
var (
	hdrPoll   = [4]byte{0x88, 0x0c, 0x01, 0x0c}
	hdrAck    = [4]byte{0x8f, 0x0d, 0x01, 0x0d}
	hdrStatus = [4]byte{0xc3, 0x4d, 0x00, 0x4d}
	hdrClock  = [4]byte{0xc2, 0x43, 0x00, 0x43}
)

// The CPU-alive exchange. The micom's stuck detector counts only cmd 0x0F
// addressed to its module 1, and gives up after 12 silent windows (it then
// resets the SoC). Android's SystemService answers each request from the
// micom (func 0x810F, i.e. hdr3 0x81, cmd 0x0F) with func 0x010F from 0x8a to
// module 1 and one payload byte, a counter that runs 0..99. The request is
// never in the logcat captures: both ends keep it out of their logs.
var (
	hdrAliveReq   = [4]byte{0x01, 0x8a, 0x01, 0x81}
	hdrAliveReply = [4]byte{0x8a, 0x01, 0x01, 0x01}
)

const cmdAlive = 0x0f

// AliveReply is the answer to a CPU-alive request, with counter n.
func AliveReply(n byte) Frame {
	return Frame{Hdr: hdrAliveReply, Cmd: cmdAlive, Payload: []byte{n}}
}

// IsAliveRequest reports whether f is the micom's CPU-alive request.
func IsAliveRequest(f Frame) bool {
	return f.Hdr == hdrAliveReq && f.Cmd == cmdAlive
}

// FromMicom is the header of the micom's cmd 0x02 frame, payload 57 00 NN. It
// is HEVService's status-flag setter: id 0x57 is the climate power draw in
// W (see Describe), not a countdown, as it was first read.
var fromMicom = [4]byte{0x0c, 0x88, 0x01, 0x8c}

// DefaultProfile is the routine traffic that was seen to go from Android to
// the micom at a steady rate, without the two clock frames, which carry the
// time and are only offered by ClockProfile. These three are sent as seen;
// what each means, and which the micom needs, is not known.
func DefaultProfile() []Periodic {
	return []Periodic{
		{Name: "poll", Every: time.Second, Build: func(time.Time) Frame {
			// cmd 5c, no payload. Answered by a 31-byte status frame with a
			// counter that rises a step a second.
			return Frame{Hdr: hdrPoll, Cmd: 0x5c}
		}},
		{Name: "ack", Every: 600 * time.Millisecond, Build: func(time.Time) Frame {
			// cmd 03, constant payload, about every 0.6 s.
			return Frame{Hdr: hdrAck, Cmd: 0x03, Payload: []byte{0xff, 0xff, 0xff, 0x0e, 0x03, 0xff, 0xff, 0x03, 0xff}}
		}},
		{Name: "status", Every: time.Second, Build: func(time.Time) Frame {
			// cmd 17, constant payload, about once a second.
			return Frame{Hdr: hdrStatus, Cmd: 0x17, Payload: []byte{0x08, 0xd5, 0xfc, 0xdb, 0x05, 0xff, 0x81, 0x2b}}
		}},
	}
}

// ClockProfile is the two frames Android sent once a second whose payload
// follows the wall clock: the second counter ran 0x10, 0x11 at :16, :17, and
// the other fields matched 20:01 on 2026-10-16 (hour 0x14, minute 0x01, month
// 0x0a, year 0x1a = 26, day 0x10). The field order is inferred from one
// minute of capture. They are NOT in DefaultProfile: a wrong time may be
// stored by the car, so they are only for a caller that has the real time.
func ClockProfile() []Periodic {
	return []Periodic{
		{Name: "clock-sec", Every: time.Second, Build: func(now time.Time) Frame {
			return Frame{Hdr: hdrPoll, Cmd: 0x09, Payload: []byte{0x04, 0xa0, 0x01, byte(now.Second()), 0x80}}
		}},
		{Name: "clock", Every: time.Second, Build: func(now time.Time) Frame {
			return Frame{Hdr: hdrClock, Cmd: 0x33, Payload: []byte{
				0x02, 0x02, byte(now.Hour()), byte(now.Minute()), byte(now.Second()),
				byte(now.Month()), 0x01, byte(now.Year() % 100), byte(now.Day()), 0x00, 0x02,
			}}
		}},
	}
}
