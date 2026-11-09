package micom

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.b)
}

// A socket path is limited to ~100 bytes: t.TempDir can be longer.
func tapPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func TestTapStreamsToClients(t *testing.T) {
	p := tapPath(t)
	tap, err := ListenTap(p)
	if err != nil {
		t.Fatal(err)
	}
	defer tap.Close()
	var rs []*bufio.Reader
	for range 2 {
		c, err := net.Dial("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		rs = append(rs, bufio.NewReader(c))
	}
	// Both clients are registered by the time the second line is sent.
	deadline := time.Now().Add(5 * time.Second)
	for {
		tap.mu.Lock()
		n := len(tap.clients)
		tap.mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	tap.Send("one\n")
	for i, r := range rs {
		if got, err := r.ReadString('\n'); err != nil || got != "one\n" {
			t.Errorf("client %d: %q, %v", i, got, err)
		}
	}
}

// A new client gets the state so far before anything live; a client that
// joined earlier does not get it again.
func TestTapReplaysTheLatestToNewClients(t *testing.T) {
	p := tapPath(t)
	tap, err := ListenTap(p)
	if err != nil {
		t.Fatal(err)
	}
	defer tap.Close()
	telem := newTelemetry(&Variant{})
	t0 := time.Unix(100, 0)
	telem.add(Frame{Hdr: [4]byte{1, 2, 3, 4}, Cmd: 5, Payload: []byte{1}}, t0)
	telem.add(Frame{Hdr: [4]byte{1, 2, 3, 4}, Cmd: 6, Payload: []byte{2}}, t0.Add(time.Second))
	telem.add(Frame{Hdr: [4]byte{1, 2, 3, 4}, Cmd: 5, Payload: []byte{3}}, t0.Add(2*time.Second))
	tap.SetReplay(func() []string {
		var out []string
		for _, f := range telem.latest() {
			out = append(out, fmt.Sprintf("%d %v\n", f.At.Unix(), f.Frame))
		}
		return out
	})
	c, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	// The replay is queued with the client, so the first replay line read
	// means it has joined.
	var got []string
	for range 2 {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, l)
	}
	tap.Send("live\n")
	l, _ := r.ReadString('\n')
	got = append(got, l)
	want := []string{"101 hdr=01020304 cmd=06 payload=02\n", "102 hdr=01020304 cmd=05 payload=03\n", "live\n"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTapSlowClientDoesNotBlock(t *testing.T) {
	p := tapPath(t)
	tap, err := ListenTap(p)
	if err != nil {
		t.Fatal(err)
	}
	defer tap.Close()
	c, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() // never read
	done := make(chan struct{})
	go func() {
		for range 100000 {
			tap.Send("a line that nobody reads, repeated until the buffers are full\n")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Send blocked on a client that does not read")
	}
}

func TestTapSocketHandling(t *testing.T) {
	p := tapPath(t)
	tap, err := ListenTap(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ListenTap(p); err == nil {
		t.Error("a served socket was taken over")
	}
	tap.Close()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}

	// A stale socket (its server gone without cleaning up) is replaced.
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	tap, err = ListenTap(p)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	tap.Close()

	// A file that is not a socket is left alone.
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenTap(p); err == nil {
		t.Error("a regular file was replaced")
	}
	if b, _ := os.ReadFile(p); string(b) != "x" {
		t.Error("the regular file was touched")
	}
}

func TestAttachPrintsTheTap(t *testing.T) {
	p := tapPath(t)
	tap, err := ListenTap(p)
	if err != nil {
		t.Fatal(err)
	}
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- Attach(p, &out) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tap.mu.Lock()
		n := len(tap.clients)
		tap.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	tap.Send("hello\n")
	for out.String() != "hello\n" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	tap.Close() // ends the attach
	if err := <-done; err != nil {
		t.Errorf("Attach: %v", err)
	}
	if out.String() != "hello\n" {
		t.Errorf("attached output %q", out.String())
	}
}
