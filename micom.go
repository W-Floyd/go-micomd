package micom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Config is what a Daemon needs. Port is required: an open UART (OpenTTY),
// or any stream, which is how the tests run it. Run closes it on the way out.
type Config struct {
	Port io.ReadWriteCloser
	// Profile is every frame this daemon may send, and when. Nil means
	// DefaultProfile. It is the allowlist: Send refuses what is not in it.
	Profile []Periodic
	// OnFrame sees every frame received, valid by its checksum. It runs on
	// the reader's goroutine: keep it short.
	OnFrame func(Frame)
	// Logf, when set, gets the daemon's own messages.
	Logf func(format string, args ...any)
	// Now is the clock the Profile's builders see; time.Now when nil.
	Now func() time.Time
	// NoAlive turns off the answer to the micom's CPU-alive requests, which
	// is otherwise always sent: without it the micom resets the SoC.
	NoAlive bool
	// Allow lists frames Send may also put on the wire, each matched exactly
	// (header, command and payload), and sent only when asked to (the control
	// socket, ServeControl): nothing here is sent on a schedule. For frames
	// worked out from the stock system that a person tries one at a time.
	Allow []Frame
}

// Stats is a snapshot of the link.
type Stats struct {
	// RxFrames and TxFrames count frames; RxByCmd counts received ones by
	// command byte.
	RxFrames, TxFrames uint64
	RxByCmd            map[byte]uint64
	// Dropped bytes and BadSum frames are what the decoder refused.
	Dropped, BadSum int
	// Countdown is the micom's last cmd 02 value (payload 57 00 NN), -1 until
	// one is seen. Despite the name it is the climate power draw in W, not a
	// countdown (the name is kept for the callers that already use it).
	Countdown int
	// LastRx is when a frame last arrived; zero before the first.
	LastRx time.Time
	// AliveRx and AliveTx count the micom's CPU-alive requests and our replies.
	AliveRx, AliveTx uint64
}

// String is the counters on one line, for a log.
func (s Stats) String() string {
	var cmds []int
	for c := range s.RxByCmd {
		cmds = append(cmds, int(c))
	}
	sort.Ints(cmds)
	by := ""
	for _, c := range cmds {
		by += fmt.Sprintf(" %02x:%d", c, s.RxByCmd[byte(c)])
	}
	return fmt.Sprintf("rx %d tx %d alive %d/%d badsum %d dropped %d by cmd:%s", s.RxFrames, s.TxFrames, s.AliveRx, s.AliveTx, s.BadSum, s.Dropped, by)
}

// Daemon is the SoC side of the micom link.
type Daemon struct {
	cfg     Config
	profile []Periodic

	wmu sync.Mutex // serialises writes to the port

	mu      sync.Mutex
	dec     Decoder
	rxCmds  map[byte]uint64
	count   int
	lastRx  time.Time
	rxTotal atomic.Uint64
	txTotal atomic.Uint64

	aliveRx, aliveTx atomic.Uint64
	aliveCount       byte // reader goroutine only
	telem            *telemetry
}

// New makes a Daemon; Run starts it.
func New(cfg Config) *Daemon {
	if cfg.Profile == nil {
		cfg.Profile = DefaultProfile()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Daemon{cfg: cfg, profile: cfg.Profile, rxCmds: map[byte]uint64{}, count: -1, telem: newTelemetry()}
}

func (d *Daemon) logf(format string, args ...any) {
	if d.cfg.Logf != nil {
		d.cfg.Logf(format, args...)
	}
}

// allowed reports whether f is one the profile can produce: same header and
// command as an entry's frame now.
func (d *Daemon) allowed(f Frame) bool {
	if !d.cfg.NoAlive && f.Hdr == hdrAliveReply && f.Cmd == cmdAlive {
		return true
	}
	now := d.cfg.Now()
	for _, p := range d.profile {
		g := p.Build(now)
		if g.Hdr == f.Hdr && g.Cmd == f.Cmd {
			return true
		}
	}
	for _, a := range d.cfg.Allow {
		if sameFrame(a, f) {
			return true
		}
	}
	return false
}

// Send puts f on the wire if the profile allows it, else returns an error:
// the daemon cannot be made to send what it was not configured to.
func (d *Daemon) Send(f Frame) error {
	if !d.allowed(f) {
		return fmt.Errorf("micom: %v is not in the profile", f)
	}
	return d.write(f)
}

func (d *Daemon) write(f Frame) error {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	if _, err := d.cfg.Port.Write(f.Encode()); err != nil {
		return err
	}
	d.txTotal.Add(1)
	return nil
}

// Run reads, decodes and sends on the profile's schedule until ctx ends or
// the port fails, and closes the port. It returns ctx's error, or the port's.
func (d *Daemon) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1+len(d.profile))
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		errc <- d.read()
	}()
	for _, p := range d.profile {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(p.Every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := d.write(p.Build(d.cfg.Now())); err != nil {
						errc <- fmt.Errorf("micom: %s: %w", p.Name, err)
						return
					}
				}
			}
		}()
	}

	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errc:
	}
	cancel()
	d.cfg.Port.Close()
	wg.Wait()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, errClosed) {
		err = nil
	}
	return err
}

func (d *Daemon) read() error {
	buf := make([]byte, 1024)
	for {
		n, err := d.cfg.Port.Read(buf)
		if n > 0 {
			d.mu.Lock()
			frames := d.dec.Write(buf[:n])
			now := d.cfg.Now()
			for _, f := range frames {
				d.rxCmds[f.Cmd]++
				d.lastRx = now
				if f.Hdr == fromMicom && f.Cmd == 0x02 && len(f.Payload) == 3 && f.Payload[0] == 0x57 {
					d.count = int(f.Payload[2])
				}
			}
			d.mu.Unlock()
			d.rxTotal.Add(uint64(len(frames)))
			for _, f := range frames {
				d.telem.add(f, now)
				if d.cfg.OnFrame != nil {
					d.cfg.OnFrame(f)
				}
				if !d.cfg.NoAlive && IsAliveRequest(f) {
					d.aliveRx.Add(1)
					if err := d.Send(AliveReply(d.aliveCount)); err != nil {
						return fmt.Errorf("micom: alive reply: %w", err)
					}
					d.aliveTx.Add(1)
					d.aliveCount = (d.aliveCount + 1) % 100
				}
			}
		}
		if err != nil {
			return err
		}
	}
}

// Stats returns the counters now.
func (d *Daemon) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	by := make(map[byte]uint64, len(d.rxCmds))
	for k, v := range d.rxCmds {
		by[k] = v
	}
	return Stats{
		RxFrames:  d.rxTotal.Load(),
		TxFrames:  d.txTotal.Load(),
		RxByCmd:   by,
		Dropped:   d.dec.Dropped,
		BadSum:    d.dec.BadSum,
		Countdown: d.count,
		LastRx:    d.lastRx,
		AliveRx:   d.aliveRx.Load(),
		AliveTx:   d.aliveTx.Load(),
	}
}

var errClosed = errors.New("use of closed file")
