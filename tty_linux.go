//go:build linux

package micom

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var bauds = map[int]uint32{
	9600:    unix.B9600,
	19200:   unix.B19200,
	38400:   unix.B38400,
	57600:   unix.B57600,
	115200:  unix.B115200,
	230400:  unix.B230400,
	460800:  unix.B460800,
	921600:  unix.B921600,
	1000000: unix.B1000000,
}

// OpenTTY opens a serial device raw, 8N1, no flow control, at baud. The
// file is non-blocking, so closing it ends a Read in progress.
func OpenTTY(path string, baud int) (*os.File, error) {
	speed, ok := bauds[baud]
	if !ok {
		return nil, fmt.Errorf("micom: unsupported baud rate %d", baud)
	}
	f, err := os.OpenFile(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("micom: %s: tcgetattr: %w", path, err)
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF | unix.IXANY
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS | unix.CBAUD
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL | speed
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		f.Close()
		return nil, fmt.Errorf("micom: %s: tcsetattr: %w", path, err)
	}
	return f, nil
}
