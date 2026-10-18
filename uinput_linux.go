//go:build linux

package micom

import (
	"encoding/binary"
	"os"
	"syscall"
	"unsafe"
)

const (
	uiSetEvBit   = 0x40045564
	uiSetKeyBit  = 0x40045565
	uiDevCreate  = 0x5501
	uiDevDestroy = 0x5502
	evSyn, evKey = 0, 1
)

// Uinput is a virtual keyboard.
type Uinput struct{ f *os.File }

// OpenUinput creates the device with the given key codes enabled.
func OpenUinput(name string, keys []uint16) (*Uinput, error) {
	f, err := os.OpenFile("/dev/uinput", os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	ioc := func(req, v uintptr) error {
		_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, v)
		if e != 0 {
			return e
		}
		return nil
	}
	if err := ioc(uiSetEvBit, evKey); err != nil {
		f.Close()
		return nil, err
	}
	for _, k := range keys {
		if err := ioc(uiSetKeyBit, uintptr(k)); err != nil {
			f.Close()
			return nil, err
		}
	}
	// struct uinput_user_dev: name[80], input_id{bus,vendor,product,version},
	// ff_effects_max, then absmax, absmin, absfuzz and absflat of 64 ints each.
	var ud [80 + 8 + 4 + 4*64*4]byte
	copy(ud[:], name)
	binary.LittleEndian.PutUint16(ud[80:], 0x06) // BUS_VIRTUAL
	if _, err := f.Write(ud[:]); err != nil {
		f.Close()
		return nil, err
	}
	if err := ioc(uiDevCreate, 0); err != nil {
		f.Close()
		return nil, err
	}
	return &Uinput{f}, nil
}

// event is a struct input_event: a timeval (zero: the kernel stamps it), the
// type, the code and the value, little-endian.
func inputEvent(t, c uint16, v int32) []byte {
	tv := int(unsafe.Sizeof(syscall.Timeval{}))
	b := make([]byte, tv+8)
	binary.LittleEndian.PutUint16(b[tv:], t)
	binary.LittleEndian.PutUint16(b[tv+2:], c)
	binary.LittleEndian.PutUint32(b[tv+4:], uint32(v))
	return b
}

// Key reports a press (value 1), release (0) or repeat (2), then SYN_REPORT.
func (u *Uinput) Key(code uint16, value int32) error {
	if _, err := u.f.Write(inputEvent(evKey, code, value)); err != nil {
		return err
	}
	_, err := u.f.Write(inputEvent(evSyn, 0, 0))
	return err
}

// Close destroys the device.
func (u *Uinput) Close() error {
	syscall.Syscall(syscall.SYS_IOCTL, u.f.Fd(), uiDevDestroy, 0)
	return u.f.Close()
}
