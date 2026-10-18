//go:build !linux

package micom

import (
	"errors"
	"os"
)

// OpenTTY is only implemented on Linux.
func OpenTTY(path string, baud int) (*os.File, error) {
	return nil, errors.New("micom: serial ports are only supported on Linux")
}
