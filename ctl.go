package micom

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// ParseHexFrame reads a whole frame written in hex, as the stock side
// logs it ("ff 85 02 01 02 0b 00 03 02 00 71"; spaces, commas and 0x prefixes
// are ignored). It must be exactly one frame with a correct checksum, so a
// frame that is worked out by hand is checked before anything can send it.
func ParseHexFrame(s string) (Frame, error) {
	clean := strings.NewReplacer(" ", "", ",", "", "\t", "", "0x", "", "0X", "").Replace(s)
	if clean == "" || len(clean)%2 != 0 {
		return Frame{}, fmt.Errorf("micom: %q is not a hex frame", s)
	}
	b := make([]byte, len(clean)/2)
	for i := range b {
		var v byte
		if _, err := fmt.Sscanf(clean[2*i:2*i+2], "%02x", &v); err != nil {
			return Frame{}, fmt.Errorf("micom: %q is not hex", s)
		}
		b[i] = v
	}
	var d Decoder
	fs := d.Write(b)
	switch {
	case d.BadSum > 0:
		return Frame{}, fmt.Errorf("micom: %q has a bad checksum (want %02x)", s, Checksum(b[1:len(b)-1]))
	case len(fs) != 1 || d.Dropped > 0 || len(d.buf) > 0:
		return Frame{}, fmt.Errorf("micom: %q is not exactly one frame", s)
	}
	return fs[0], nil
}

// hexList is a repeatable flag of hex frames.
type hexList []string

func (h *hexList) String() string { return strings.Join(*h, " ") }

func (h *hexList) Set(s string) error {
	if _, err := ParseHexFrame(s); err != nil {
		return err
	}
	*h = append(*h, s)
	return nil
}

// frames are the parsed frames; the list was checked as it was set.
func (h hexList) frames() []Frame {
	out := make([]Frame, 0, len(h))
	for _, s := range h {
		f, _ := ParseHexFrame(s)
		out = append(out, f)
	}
	return out
}

// sameFrame is exact equality: header, command and payload.
func sameFrame(a, b Frame) bool {
	return a.Hdr == b.Hdr && a.Cmd == b.Cmd && bytes.Equal(a.Payload, b.Payload)
}

// ServeControl accepts frames, one hex frame per line, on a unix socket (mode
// 0660) and answers each with "ok" or "refused: why". A frame goes out only if
// the daemon's profile or Config.Allow lists it (Send), so what the socket
// can do is what the daemon was started with, never more. It returns when ctx
// ends, and removes the socket.
func ServeControl(ctx context.Context, d *Daemon, path string) error {
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return fmt.Errorf("micom: %s is already served", path)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("micom: %s exists and is not a socket", path)
		}
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
		os.Remove(path)
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go serveControlConn(d, c)
	}
}

func serveControlConn(d *Daemon, c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f, err := ParseHexFrame(line)
		if err == nil {
			err = d.Send(f)
		}
		if err != nil {
			fmt.Fprintf(c, "refused: %v\n", err)
			continue
		}
		d.logf("control: sent %v", f)
		fmt.Fprintln(c, "ok")
	}
}

// SendControl is the client: it sends each frame to the daemon serving path,
// gap apart, and prints the answers to w. It stops at the first refusal.
func SendControl(path string, frames []string, gap time.Duration, w io.Writer) error {
	c, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	defer c.Close()
	r := bufio.NewReader(c)
	for i, s := range frames {
		if i > 0 {
			time.Sleep(gap)
		}
		if _, err := ParseHexFrame(s); err != nil {
			return err // refused here, before it reaches the daemon
		}
		if _, err := fmt.Fprintln(c, s); err != nil {
			return err
		}
		ans, err := r.ReadString('\n')
		if err != nil {
			return errors.Join(fmt.Errorf("micom: no answer to %q", s), err)
		}
		fmt.Fprintf(w, "%s: %s", strings.TrimSpace(s), ans)
		if !strings.HasPrefix(ans, "ok") {
			return fmt.Errorf("micom: %s", strings.TrimSpace(ans))
		}
	}
	return nil
}
