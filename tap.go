package micom

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Tap streams text lines to whoever is connected to a unix socket, so a running
// daemon can be watched without stopping it: the one process that owns the UART
// shares what it reads. A client too slow to keep up loses lines, never holds
// the daemon back.
type Tap struct {
	path    string
	ln      net.Listener
	mu      sync.Mutex
	clients map[chan string]struct{}
}

// ListenTap serves path, which is mode 0660. A leftover socket nobody listens
// on is replaced; one that is served, or anything else at path, is an error.
func ListenTap(path string) (*Tap, error) {
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
	t := &Tap{path: path, ln: ln, clients: map[chan string]struct{}{}}
	go t.accept()
	return t, nil
}

func (t *Tap) accept() {
	for {
		c, err := t.ln.Accept()
		if err != nil {
			return
		}
		ch := make(chan string, 256)
		t.mu.Lock()
		t.clients[ch] = struct{}{}
		t.mu.Unlock()
		go t.serve(c, ch)
	}
}

func (t *Tap) serve(c net.Conn, ch chan string) {
	defer c.Close()
	// A client that hangs up is noticed here, not on the next line.
	go func() {
		io.Copy(io.Discard, c)
		t.drop(ch)
	}()
	for line := range ch {
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, line); err != nil {
			t.drop(ch)
			return
		}
	}
}

func (t *Tap) drop(ch chan string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.clients[ch]; ok {
		delete(t.clients, ch)
		close(ch)
	}
}

// Send gives line to every client, without waiting for any.
func (t *Tap) Send(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for ch := range t.clients {
		select {
		case ch <- line:
		default:
		}
	}
}

// Close stops serving, disconnects the clients and removes the socket.
func (t *Tap) Close() {
	t.ln.Close()
	os.Remove(t.path)
	t.mu.Lock()
	chs := make([]chan string, 0, len(t.clients))
	for ch := range t.clients {
		chs = append(chs, ch)
	}
	t.mu.Unlock()
	for _, ch := range chs {
		t.drop(ch)
	}
}

// Attach copies what the daemon at path streams to w until either side ends.
func Attach(path string, w io.Writer) error {
	c, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = io.Copy(w, c)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return err
}
