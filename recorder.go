package micom

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder writes every frame the link carries, both ways, to a file of
// JSON lines, for lining up with other logs (an OBD2 export, a GPS track):
//
//	{"t":1762600000.123456,"dir":"rx","frame":"ff0c88018c0200045700f5.."}
//
// t is the wall clock, in seconds; "bad":true marks a frame refused for its
// checksum; "mono" (seconds since the recorder started) is added while the
// clock reads before 2020, so lines can be realigned once it is set; and
// "dropped":N says N records were lost before this line. A header line,
// {"t":..,"start":true,"variant":..,"version":..}, opens each file.
//
// The file is rotated to PATH.1 when the next line would take it past max,
// so the two take at most 2×max. Record never blocks the link: a record
// that finds the queue full is dropped and counted. A write error is logged
// once and stops the recording, never the daemon.
type Recorder struct {
	path    string
	max     int64
	variant string
	logf    func(string, ...any)
	now     func() time.Time
	start   time.Time // its monotonic reading gives "mono"
	every   time.Duration

	ch      chan record
	done    chan struct{}
	exited  chan struct{}
	closing sync.Once
	stopped atomic.Bool

	pending atomic.Uint64 // dropped since the last line written
	dropped atomic.Uint64
	written atomic.Int64
	rotated atomic.Uint64
	errMsg  atomic.Value // string

	// open and rotate are the file's; the tests replace them.
	open   func(path string) (io.WriteCloser, int64, error)
	rotate func(path string) error
}

type record struct {
	t   time.Time
	dir string
	raw []byte
	bad bool
}

// RecorderState is a recorder's progress, for Snapshot.
type RecorderState struct {
	Path string `json:"path"`
	// Bytes is what the current file holds; Rotations how often it was
	// moved to PATH.1.
	Bytes     int64  `json:"bytes"`
	Rotations uint64 `json:"rotations"`
	Dropped   uint64 `json:"dropped"`
	// Error is the write error that stopped the recording.
	Error string `json:"error,omitempty"`
}

// DefaultRecordMax is -record-max's default.
const DefaultRecordMax = 32 << 20

// NewRecorder records to path, creating its directory, up to max bytes a
// file. variant names the variant for the header lines.
func NewRecorder(path string, max int64, variant string, logf func(string, ...any)) (*Recorder, error) {
	r := newRecorder(path, max, variant, logf, 4096)
	if err := r.begin(); err != nil {
		return nil, err
	}
	return r, nil
}

func newRecorder(path string, max int64, variant string, logf func(string, ...any), queue int) *Recorder {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Recorder{
		path: path, max: max, variant: variant, logf: logf,
		now: time.Now, start: time.Now(), every: time.Second,
		ch: make(chan record, queue), done: make(chan struct{}), exited: make(chan struct{}),
		open: func(p string) (io.WriteCloser, int64, error) {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, 0, err
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			if err != nil {
				return nil, 0, err
			}
			fi, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, 0, err
			}
			return f, fi.Size(), nil
		},
		rotate: func(p string) error { return os.Rename(p, p+".1") },
	}
}

// begin opens the file and starts the writer.
func (r *Recorder) begin() error {
	w, size, err := r.open(r.path)
	if err != nil {
		return err
	}
	go r.run(w, size)
	return nil
}

// Record queues a frame as it went on or came off the wire. raw is not
// copied: the caller must not change it.
func (r *Recorder) Record(dir string, raw []byte, bad bool) {
	if r == nil || r.stopped.Load() {
		return
	}
	select {
	case r.ch <- record{t: r.now(), dir: dir, raw: raw, bad: bad}:
	default:
		r.pending.Add(1)
		r.dropped.Add(1)
	}
}

// Close writes what is queued, flushes and closes the file.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.closing.Do(func() { close(r.done) })
	<-r.exited
}

// State is the recorder's progress now.
func (r *Recorder) State() RecorderState {
	s := RecorderState{Path: r.path, Bytes: r.written.Load(), Rotations: r.rotated.Load(), Dropped: r.dropped.Load()}
	if e, ok := r.errMsg.Load().(string); ok {
		s.Error = e
	}
	return s
}

func (r *Recorder) run(w io.WriteCloser, size int64) {
	defer close(r.exited)
	bw := bufio.NewWriterSize(w, 64<<10)
	r.written.Store(size)
	failed := false
	fail := func(err error) {
		if failed {
			return
		}
		failed = true
		r.stopped.Store(true)
		r.errMsg.Store(err.Error())
		r.logf("-record %s: %v; recording stopped", r.path, err)
		w.Close()
	}
	write := func(line []byte) {
		if failed {
			return
		}
		if r.written.Load()+int64(len(line)) > r.max && r.written.Load() > 0 {
			err := bw.Flush()
			if err == nil {
				err = w.Close()
			}
			if err == nil {
				err = r.rotate(r.path)
			}
			if err == nil {
				w, size, err = r.open(r.path)
			}
			if err != nil {
				fail(err)
				return
			}
			bw.Reset(w)
			r.written.Store(size)
			r.rotated.Add(1)
			head := r.header()
			if _, err := bw.Write(head); err != nil {
				fail(err)
				return
			}
			r.written.Add(int64(len(head)))
		}
		if _, err := bw.Write(line); err != nil {
			fail(err)
			return
		}
		r.written.Add(int64(len(line)))
	}
	write(r.header())
	tick := time.NewTicker(r.every)
	defer tick.Stop()
	for {
		select {
		case rec := <-r.ch:
			write(r.line(rec))
		case <-tick.C:
			if !failed {
				if err := bw.Flush(); err != nil {
					fail(err)
				}
			}
		case <-r.done:
			for len(r.ch) > 0 {
				write(r.line(<-r.ch))
			}
			if !failed {
				if err := bw.Flush(); err != nil {
					fail(err)
					return
				}
				if err := w.Close(); err != nil {
					fail(err)
				}
			}
			return
		}
	}
}

// stamp is the start of a line: the time and, before 2020, "mono".
func (r *Recorder) stamp(t time.Time) []byte {
	b := []byte(`{"t":`)
	b = strconv.AppendFloat(b, float64(t.UnixMicro())/1e6, 'f', 6, 64)
	if t.Year() < 2020 {
		b = append(b, `,"mono":`...)
		b = strconv.AppendFloat(b, t.Sub(r.start).Seconds(), 'f', 6, 64)
	}
	return b
}

func (r *Recorder) line(rec record) []byte {
	b := r.stamp(rec.t)
	b = append(b, `,"dir":"`...)
	b = append(b, rec.dir...)
	b = append(b, `","frame":"`...)
	b = hex.AppendEncode(b, rec.raw)
	b = append(b, '"')
	if rec.bad {
		b = append(b, `,"bad":true`...)
	}
	return r.finish(b)
}

func (r *Recorder) header() []byte {
	b := r.stamp(r.now())
	b = append(b, `,"start":true,"variant":`...)
	v, _ := json.Marshal(r.variant)
	b = append(b, v...)
	b = append(b, `,"version":`...)
	v, _ = json.Marshal(buildVersion())
	b = append(b, v...)
	return r.finish(b)
}

// finish adds the records dropped since the last line, and ends the line.
func (r *Recorder) finish(b []byte) []byte {
	if n := r.pending.Swap(0); n > 0 {
		b = append(b, `,"dropped":`...)
		b = strconv.AppendUint(b, n, 10)
	}
	return append(b, "}\n"...)
}

// buildVersion is the VCS revision the binary was built from, "+dirty" if
// the tree was modified, else the module version.
func buildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return bi.Main.Version
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}

// ParseSize reads a byte count: a plain number, or one with a K, M or G
// suffix (powers of 1024; "KiB", "MB" and the like read the same).
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(strings.TrimSuffix(t, "B"), "I")
	mul := int64(1)
	if n := len(t); n > 0 {
		switch t[n-1] {
		case 'K':
			mul = 1 << 10
		case 'M':
			mul = 1 << 20
		case 'G':
			mul = 1 << 30
		}
		if mul > 1 {
			t = t[:n-1]
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mul {
		return 0, fmt.Errorf("micom: %q is not a size (a byte count, or with K, M or G)", s)
	}
	return n * mul, nil
}
