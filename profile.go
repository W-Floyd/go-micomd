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

// ScaledProfile divides every Every by n, for running the daemon against a
// micom whose own clock runs at n times real speed (for example an emulator
// running it faster than real time): the daemon's send cadence then matches
// the micom's clock rather than the wall clock. n must be positive. The CPU-alive
// reply needs no scaling - it answers inline on receipt, not on a timer.
func ScaledProfile(n time.Duration, ps []Periodic) []Periodic {
	out := make([]Periodic, len(ps))
	for i, p := range ps {
		p.Every /= n
		out[i] = p
	}
	return out
}

// The CPU-alive exchange. The micom's stuck detector counts only cmd 0x0F
// addressed to its module 1, and gives up after 12 silent windows (it then
// resets the SoC). Android's SystemService answers each request from the
// micom (func 0x810F, i.e. hdr3 0x81, cmd 0x0F) with func 0x010F from 0x8a to
// module 1 and one payload byte, a counter that runs 0..99. The request is
// never in the logcat captures: both ends keep it out of their logs.
//
// It is micomd's own, not a variant's: it works whatever variant is loaded,
// or none, and no variant may name it.
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

// AliveRequest is the micom's CPU-alive request, as an emulated micom
// (cmd/micom-fake) sends it.
func AliveRequest() Frame { return Frame{Hdr: hdrAliveReq, Cmd: cmdAlive} }

// IsAliveReply reports whether f is the SoC's answer to it.
func IsAliveReply(f Frame) bool { return f.Hdr == hdrAliveReply && f.Cmd == cmdAlive }

// IsHello reports whether f is stock micomd's link-up request (func 0x0101,
// no payload, every 2 s, five times), which the micom answers with its boot
// record (0x8101).
func IsHello(f Frame) bool { return f.Hdr == hdrAliveReply && f.Cmd == 0x01 }

// IsBootRecord reports whether f is the micom's answer to it.
func IsBootRecord(f Frame) bool { return f.Hdr == hdrAliveReq && f.Cmd == 0x01 }

// ReplyHeader is the header a request's answer comes back on: the two
// peers swapped and the type's top bit set (88 0c 01 0c -> 0c 88 01 8c,
// 8a 01 01 01 -> 01 8a 01 81), as every captured request/answer pair.
func ReplyHeader(h [4]byte) [4]byte { return [4]byte{h[1], h[0], h[2], h[3] | 0x80} }

// FromMicom is the header of the micom's cmd 0x02 frame, payload 57 00 NN. It
// is HEVService's status-flag setter: id 0x57 is the climate power draw in
// W (see Describe), not a countdown, as it was first read.
var fromMicom = [4]byte{0x0c, 0x88, 0x01, 0x8c}

// DefaultProfile is the default variant's routine traffic, the frames Android
// was seen to send to the micom at a steady rate, without the clock frames,
// which carry the time and are only offered by ClockProfile.
func DefaultProfile() []Periodic { return defaultVariant().Profile() }

// ClockProfile is the default variant's frames whose payload follows the
// wall clock. They are NOT in DefaultProfile: a wrong time may be stored by
// the car, so they are only for a caller that has the real time.
func ClockProfile() []Periodic { return defaultVariant().ClockProfile() }
