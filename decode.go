package micom

import (
	"fmt"
	"strconv"
)

// Field is one value read out of a frame.
type Field struct {
	Name string `json:"name"`
	// Value is the number, or Valid false when the frame carried its "not
	// available" marker (0xff / 0xffff).
	Value int    `json:"value"`
	Valid bool   `json:"valid"`
	Unit  string `json:"unit"`
	// Text is set, and Value 0, for a field the frame carries as ASCII.
	Text string `json:"text,omitempty"`
	// Float is the exact value of a field whose scale or offset is not
	// whole (0.5 %, 0.002 kWh); Value is it rounded half up.
	Float *float64 `json:"float,omitempty"`
}

func (f Field) String() string {
	switch {
	case !f.Valid:
		return f.Name + "=n/a"
	case f.Text != "":
		return f.Name + "=" + f.Text
	case f.Float != nil:
		return f.Name + "=" + strconv.FormatFloat(*f.Float, 'f', -1, 64) + f.Unit
	}
	return fmt.Sprintf("%s=%d%s", f.Name, f.Value, f.Unit)
}

// Func is the Android-side function code of a frame from the micom: its
// header's fourth byte and its command.
func Func(f Frame) uint16 { return uint16(f.Hdr[3])<<8 | uint16(f.Cmd) }

// FrameName names the frames whose meaning is known, "" for the rest, as the
// default variant knows them.
func FrameName(f Frame) string { return defaultVariant().FrameName(f) }

// SentName names a frame we send to the micom, by the default variant's
// declared to_micom messages; "" for the rest. See Variant.SentName: this is
// not FrameName's table, and the two never fall back to each other.
func SentName(f Frame) string { return defaultVariant().SentName(f) }

// DescribeSent is a one-line reading of f, a frame sent to the micom, for
// the to_micom messages whose layout the default variant declares with
// fields; "" for the rest.
func DescribeSent(f Frame) string { return defaultVariant().DescribeSent(f) }

// Decode reads the fields of a frame whose layout the default variant knows;
// nil for the rest. On the EV6 the layouts are Android's HEVService's (see
// variants/hkmc-gen5w-ev.yaml).
func Decode(f Frame) []Field { return defaultVariant().Decode(f) }

// Signal reads a state frame of the default variant (a door, the gear, a
// switch); false for the rest.
func Signal(f Frame) (SignalReading, bool) { return defaultVariant().Signal(f) }

// Describe is a one-line reading of f for the frames whose layout the
// default variant knows, "" for the rest.
func Describe(f Frame) string { return defaultVariant().Describe(f) }

func be16(b []byte) int { return int(b[0])<<8 | int(b[1]) }
