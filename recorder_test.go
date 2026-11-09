package micom

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// memRecorder records into a buffer, at the times given.
func memRecorder(t *testing.T, now func() time.Time, w io.Writer, queue int) *Recorder {
	t.Helper()
	r := newRecorder("mem", DefaultRecordMax, "kia-ev6-gen5w", t.Logf, queue)
	r.now = now
	r.start = now()
	r.open = func(string) (io.WriteCloser, int64, error) { return nopCloser{w}, 0, nil }
	if err := r.begin(); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecorderLines(t *testing.T) {
	at := time.Date(2026, 11, 8, 12, 0, 0, 123456000, time.UTC)
	unset := time.Date(1970, 1, 1, 0, 0, 5, 0, time.UTC)
	alive := Frame{Hdr: hdrAliveReq, Cmd: cmdAlive, Payload: []byte{0}}.Encode()
	for _, tc := range []struct {
		name string
		now  time.Time
		dir  string
		raw  []byte
		bad  bool
		want string
	}{
		{"rx", at, "rx", alive, false, `{"t":1794139200.123456,"dir":"rx","frame":"ff018a01810f000200f9"}`},
		{"tx", at, "tx", AliveReply(7).Encode(), false, `{"t":1794139200.123456,"dir":"tx","frame":"ff8a0101010f0002077e"}`},
		{"bad", at, "rx", []byte{0xff, 0x01, 0x8a, 0x01, 0x81, 0x0f, 0x00, 0x02, 0x00, 0x00}, true, `{"t":1794139200.123456,"dir":"rx","frame":"ff018a01810f00020000","bad":true}`},
		{"clock unset", unset, "rx", alive, false, `{"t":5.000000,"mono":0.000000,"dir":"rx","frame":"ff018a01810f000200f9"}`},
	} {
		var b syncBuffer
		r := memRecorder(t, func() time.Time { return tc.now }, &b, 16)
		r.Record(tc.dir, tc.raw, tc.bad)
		r.Close()
		lines := strings.Split(strings.TrimSpace(b.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("%s: %q", tc.name, b.String())
		}
		var head map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &head); err != nil || head["start"] != true || head["variant"] != "kia-ev6-gen5w" {
			t.Errorf("%s: header %q (%v)", tc.name, lines[0], err)
		}
		if lines[1] != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, lines[1], tc.want)
		}
	}
}

// The file is moved to PATH.1 before a line would take it past the cap;
// each file opens with a header and stays within the cap.
func TestRecorderRotates(t *testing.T) {
	frame := AliveReply(0).Encode()
	// One record line is 72 bytes at this time; the header is longer.
	for _, tc := range []struct {
		name      string
		max       int64
		records   int
		rotations uint64
	}{
		{"under the cap", 4096, 10, 0},
		{"just over", 1024, 20, 1},
		{"many times", 600, 100, 12},
	} {
		dir := filepath.Join(t.TempDir(), "new", "dir")
		p := filepath.Join(dir, "micom.jsonl")
		r := newRecorder(p, tc.max, "v", t.Logf, 64)
		r.now = func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }
		if err := r.begin(); err != nil {
			t.Fatal(err)
		}
		for range tc.records {
			r.Record("tx", frame, false)
			time.Sleep(time.Millisecond) // the queue must not overflow
		}
		r.Close()
		if st := r.State(); st.Rotations != tc.rotations || st.Dropped != 0 {
			t.Errorf("%s: %+v, want %d rotations", tc.name, st, tc.rotations)
		}
		files := []string{p}
		if tc.rotations > 0 {
			files = append(files, p+".1")
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(b)) > tc.max {
				t.Errorf("%s: %s is %d bytes, over %d", tc.name, f, len(b), tc.max)
			}
			if !strings.HasPrefix(string(b), `{"t":1794139200.000000,"start":true,"variant":"v"`) {
				t.Errorf("%s: %s starts %q", tc.name, f, b[:min(len(b), 60)])
			}
			for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if !json.Valid([]byte(l)) {
					t.Errorf("%s: %q is not JSON", tc.name, l)
				}
			}
		}
	}
}

// What is queued when the recorder closes is written, without waiting for
// the once-a-second flush.
func TestRecorderFlushesOnClose(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.jsonl")
	r, err := NewRecorder(p, DefaultRecordMax, "v", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		r.Record("rx", AliveReply(0).Encode(), false)
	}
	r.Close()
	b, _ := os.ReadFile(p)
	if n := strings.Count(string(b), `"dir":"rx"`); n != 50 {
		t.Errorf("%d records written, want 50", n)
	}
}

// blockingWriter holds every write until released.
type blockingWriter struct {
	release chan struct{}
	mu      sync.Mutex
	b       strings.Builder
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// A writer that cannot keep up loses records, never holds the link: they are
// counted, and the next line written says how many.
func TestRecorderDropsWhenBlocked(t *testing.T) {
	w := &blockingWriter{release: make(chan struct{})}
	r := newRecorder("mem", DefaultRecordMax, "v", t.Logf, 4)
	r.every = 5 * time.Millisecond
	r.open = func(string) (io.WriteCloser, int64, error) { return nopCloser{w}, 0, nil }
	if err := r.begin(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the writer is now stuck flushing the header
	done := make(chan struct{})
	go func() {
		for range 20 {
			r.Record("rx", AliveReply(0).Encode(), false)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked")
	}
	if st := r.State(); st.Dropped < 15 {
		t.Errorf("dropped %d, want at least 15", st.Dropped)
	}
	close(w.release)
	r.Close()
	if !strings.Contains(w.b.String(), `,"dropped":`) {
		t.Errorf("no line says what was dropped:\n%s", w.b.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// A write error stops the recording, logged once; Record keeps returning.
func TestRecorderStopsOnError(t *testing.T) {
	var logs syncBuffer
	r := newRecorder("mem", DefaultRecordMax, "v", func(f string, a ...any) { logs.Write([]byte(f + "\n")) }, 16)
	r.every = 5 * time.Millisecond
	r.open = func(string) (io.WriteCloser, int64, error) { return nopCloser{failingWriter{}}, 0, nil }
	r.begin()
	for range 5 {
		r.Record("rx", AliveReply(0).Encode(), false)
		time.Sleep(10 * time.Millisecond)
	}
	r.Close()
	if st := r.State(); !strings.Contains(st.Error, "no space") {
		t.Errorf("state %+v", st)
	}
	if n := strings.Count(logs.String(), "recording stopped"); n != 1 {
		t.Errorf("logged %d times: %q", n, logs.String())
	}
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"1000", 1000}, {"32M", 32 << 20}, {"32MiB", 32 << 20}, {"512k", 512 << 10}, {"1G", 1 << 30}, {"64KB", 64 << 10},
		{"", 0}, {"0", 0}, {"-1M", 0}, {"12T", 0}, {"M", 0},
	} {
		got, err := ParseSize(tc.in)
		if got != tc.want || (err == nil) != (tc.want > 0) {
			t.Errorf("%q: %d %v, want %d", tc.in, got, err, tc.want)
		}
	}
}

// The daemon records what crosses the port: the micom's frames, a bad one,
// and its own answers.
func TestDaemonRecords(t *testing.T) {
	var b syncBuffer
	r := memRecorder(t, time.Now, &b, 64)
	a, m := net.Pipe()
	d := New(Config{Port: a, Profile: []Periodic{}, Recorder: r})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	go io.Copy(io.Discard, m)
	req := Frame{Hdr: hdrAliveReq, Cmd: cmdAlive, Payload: []byte{0}}.Encode()
	bad := append([]byte(nil), req...)
	bad[len(bad)-1] ^= 0xff
	m.Write(bad)
	m.Write(req)
	for i := 0; i < 100 && d.Stats().AliveTx == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	m.Close()
	<-done
	r.Close()
	var got []string
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	for sc.Scan() {
		var l struct {
			Dir, Frame string
			Bad        bool
		}
		json.Unmarshal(sc.Bytes(), &l)
		if l.Dir != "" {
			got = append(got, l.Dir+" "+l.Frame+map[bool]string{true: " bad"}[l.Bad])
		}
	}
	want := []string{"rx ff018a01810f00020006 bad", "rx ff018a01810f000200f9", "tx ff8a0101010f00020079"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("recorded %q, want %q", got, want)
	}
	if s := d.Snapshot(); s.Recorder == nil || s.Recorder.Bytes == 0 {
		t.Errorf("snapshot recorder %+v", s.Recorder)
	}
}
