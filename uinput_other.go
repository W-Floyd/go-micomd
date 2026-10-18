//go:build !linux

package micom

import "errors"

// Uinput is a virtual keyboard: Linux only.
type Uinput struct{}

// OpenUinput always fails off Linux.
func OpenUinput(name string, keys []uint16) (*Uinput, error) {
	return nil, errors.New("uinput: Linux only")
}

// Key does nothing.
func (u *Uinput) Key(code uint16, value int32) error { return errors.New("uinput: Linux only") }

// Close does nothing.
func (u *Uinput) Close() error { return nil }
