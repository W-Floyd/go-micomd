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

// Pattern is a family of frames that differ only in payload values: the
// header, command and payload length are fixed, and each payload byte lies in
// [Lo[i], Hi[i]].
type Pattern struct {
	Hdr    [4]byte
	Cmd    byte
	Lo, Hi []byte
}

// ParsePattern reads a pattern written as a whole frame in hex, in which a
// payload byte may be a range "[lo-hi]" and the checksum, which follows from
// the payload, is ".." ("ff 85 02 01 02 03 00 02 [00-1e] .." is the main
// volume, 0 to 30). The length field must match the payload: a pattern varies
// values, never which frame it is.
func ParsePattern(s string) (Pattern, error) {
	clean := strings.NewReplacer(" ", "", ",", "", "\t", "", "0x", "", "0X", "").Replace(s)
	var lo, hi []byte
	sum := false
	for i := 0; i < len(clean); {
		if sum {
			return Pattern{}, fmt.Errorf("micom: %q: the checksum \"..\" comes last", s)
		}
		var a, b byte
		switch {
		case strings.HasPrefix(clean[i:], ".."):
			sum = true
			i += 2
			continue
		case clean[i] == '[':
			if len(clean) < i+7 || clean[i+3] != '-' || clean[i+6] != ']' {
				return Pattern{}, fmt.Errorf("micom: %q: a range is [lo-hi]", s)
			}
			if _, err := fmt.Sscanf(clean[i+1:i+3]+" "+clean[i+4:i+6], "%02x %02x", &a, &b); err != nil || a > b {
				return Pattern{}, fmt.Errorf("micom: %q: bad range %s", s, clean[i:i+7])
			}
			i += 7
		default:
			if len(clean) < i+2 {
				return Pattern{}, fmt.Errorf("micom: %q is not hex", s)
			}
			if _, err := fmt.Sscanf(clean[i:i+2], "%02x", &a); err != nil {
				return Pattern{}, fmt.Errorf("micom: %q is not hex", s)
			}
			b = a
			i += 2
		}
		lo = append(lo, a)
		hi = append(hi, b)
	}
	// Start, header (4), command, length (2), at least the checksum.
	if !sum || len(lo) < 8 {
		return Pattern{}, fmt.Errorf("micom: %q is not a whole frame ending in the checksum \"..\"", s)
	}
	if !bytes.Equal(lo[:8], hi[:8]) {
		return Pattern{}, fmt.Errorf("micom: %q: only payload bytes may be ranges", s)
	}
	if lo[0] != Start {
		return Pattern{}, fmt.Errorf("micom: %q does not start with %02x", s, Start)
	}
	n := len(lo) - 8
	if int(lo[6])<<8|int(lo[7]) != n+1 {
		return Pattern{}, fmt.Errorf("micom: %q: the length field is not the payload's (%d) plus the checksum", s, n)
	}
	var p Pattern
	copy(p.Hdr[:], lo[1:5])
	p.Cmd = lo[5]
	p.Lo, p.Hi = lo[8:], hi[8:]
	return p, nil
}

// Match is whether f is one of the pattern's frames.
func (p Pattern) Match(f Frame) bool {
	if f.Hdr != p.Hdr || f.Cmd != p.Cmd || len(f.Payload) != len(p.Lo) {
		return false
	}
	for i, v := range f.Payload {
		if v < p.Lo[i] || v > p.Hi[i] {
			return false
		}
	}
	return true
}

// allowList is the repeatable -allow flag: whole frames, matched exactly, and
// patterns (those with a range or the ".." checksum).
type allowList struct {
	specs    []string
	frames   []Frame
	patterns []Pattern
}

func (a *allowList) String() string { return strings.Join(a.specs, " ") }

func (a *allowList) Set(s string) error {
	if strings.ContainsAny(s, "[.") {
		p, err := ParsePattern(s)
		if err != nil {
			return err
		}
		a.patterns = append(a.patterns, p)
	} else {
		f, err := ParseHexFrame(s)
		if err != nil {
			return err
		}
		a.frames = append(a.frames, f)
	}
	a.specs = append(a.specs, s)
	return nil
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
	return serveUnix(ctx, path, func(c net.Conn) { serveControlConn(d, c) })
}

// listenUnix listens on a unix socket at path, mode 0660, replacing a stale
// socket but never a live one or another kind of file.
func listenUnix(path string) (net.Listener, error) {
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, fmt.Errorf("micom: %s is already served", path)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("micom: %s exists and is not a socket", path)
		}
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// serveUnix runs serve on each connection until ctx ends, then removes the
// socket.
func serveUnix(ctx context.Context, path string, serve func(net.Conn)) error {
	ln, err := listenUnix(path)
	if err != nil {
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
		go serve(c)
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
