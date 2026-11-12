// Package can reads and writes classic CAN frames in candump's compact form
// and names them, message and signals, from DBC files read at run time.
package can

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Frame is one classic CAN frame on one of the micom's three CAN buses.
type Frame struct {
	Bus  int // FlexCAN module 0..2
	ID   uint32
	Ext  bool // 29-bit ID
	Data []byte
}

// String is candump's compact form: can0 123#11223344, an 8-digit ID for an
// extended frame.
func (f Frame) String() string {
	id := fmt.Sprintf("%03X", f.ID)
	if f.Ext {
		id = fmt.Sprintf("%08X", f.ID)
	}
	return fmt.Sprintf("can%d %s#%X", f.Bus, id, f.Data)
}

// Parse reads a frame in candump's compact form. It also takes "can0:123#.."
// and a candump -L line, whose leading "(timestamp)" it drops.
func Parse(s string) (Frame, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "(") {
		if i := strings.IndexByte(s, ')'); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
		}
	}
	ifc, frame, ok := strings.Cut(strings.Replace(s, ":", " ", 1), " ")
	bus, err := strconv.Atoi(strings.TrimPrefix(ifc, "can"))
	if !ok || !strings.HasPrefix(ifc, "can") || err != nil || bus < 0 || bus > 2 {
		return Frame{}, fmt.Errorf("CAN frame %q: want can0..can2 ID#HEX", s)
	}
	idStr, data, ok := strings.Cut(strings.TrimSpace(frame), "#")
	id, err := strconv.ParseUint(idStr, 16, 32)
	if !ok || err != nil {
		return Frame{}, fmt.Errorf("CAN frame %q: bad ID", s)
	}
	b, err := hex.DecodeString(data)
	if err != nil || len(b) > 8 {
		return Frame{}, fmt.Errorf("CAN frame %q: data must be 0..8 hex bytes", s)
	}
	ext := len(idStr) > 3
	if (!ext && id > 0x7ff) || id > 0x1fffffff {
		return Frame{}, fmt.Errorf("CAN frame %q: ID out of range", s)
	}
	return Frame{Bus: bus, ID: uint32(id), Ext: ext, Data: b}, nil
}
