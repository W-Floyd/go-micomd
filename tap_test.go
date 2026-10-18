package micom

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
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
