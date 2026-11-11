// Command micom-fake plays the micom's side of the SoC link, for emulators:
// the stock head unit under QEMU (its micomd on /dev/ttyTCC2) or LIVI's
// go-micomd in a VM. It never talks to a real micom or car: it is the micom.
//
// It answers the link (the SoC's hello with a boot record, the CPU-alive
// exchange), replays a micomd -record capture or holds a still scene taken
// from one, plays a scenario of named messages, answers reads with what it
// holds, and takes writes into its state as the variant's reflects: say the
// car reports them, so settings screens round-trip. Everything car-specific
// comes from the variant. See docs/fake-micom.md.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	micom "github.com/W-Floyd/go-micomd"
)

type periodic struct {
	f     micom.Frame
	every time.Duration
}

type fake struct {
	v      *micom.Variant
	logf   func(string, ...any)
	scene  []periodic
	replay []timed
	loop   bool
	speed  float64

	mu     sync.Mutex
	w      io.Writer
	rec    *json.Encoder
	up     bool
	held   map[string]micom.Frame // latest frame of each kind sent (kindOf)
	fields map[string]uint64      // latest raw value of each field sent, by name
	owned  map[string]bool        // kinds a SoC write set: sent from held, not the capture
	last   map[string]string      // per SoC frame kind, the payload last logged
}

func newFake(v *micom.Variant, logf func(string, ...any)) *fake {
	return &fake{v: v, logf: logf, speed: 1, held: map[string]micom.Frame{}, fields: map[string]uint64{},
		owned: map[string]bool{}, last: map[string]string{}}
}

// hold keeps what a frame from the micom says. Called with mu held.
func (f *fake) hold(fr micom.Frame) {
	f.held[kindOf(f.v, fr)] = fr
	if _, raw, ok := f.v.RawFields(fr); ok {
		for k, x := range raw {
			f.fields[k] = x
		}
	}
}

func (f *fake) send(fr micom.Frame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold(fr)
	if f.w == nil {
		return nil
	}
	f.record("rx", fr)
	_, err := f.w.Write(fr.Encode())
	return err
}

// sendCar sends a frame of the car's (replay, scene) unless a SoC write set
// what it carries: then the frame held, as a car that took the setting
// would report it.
func (f *fake) sendCar(fr micom.Frame) error {
	f.mu.Lock()
	if k := kindOf(f.v, fr); f.owned[k] {
		fr = f.held[k]
	}
	f.mu.Unlock()
	return f.send(fr)
}

// record writes in micomd -record's format, seen from the SoC: "rx" is what
// the fake sends, "tx" what the SoC sent. Called with mu held.
func (f *fake) record(dir string, fr micom.Frame) {
	if f.rec != nil {
		f.rec.Encode(map[string]any{"t": float64(time.Now().UnixMicro()) / 1e6, "dir": dir, "frame": hex.EncodeToString(fr.Encode())})
	}
}

// base is the payload held for a from_micom message, nil if none.
func (f *fake) base(msg string) []byte {
	probe, err := f.v.EncodeReceived(msg, nil, nil)
	if err != nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.held[kindOf(f.v, probe)]; ok {
		return append([]byte(nil), h.Payload...)
	}
	return nil
}

// answer is the frame that answers a read of hdr/cmd: the one held for it,
// else the variant's longest message for it filled from the fields held.
func (f *fake) answer(hdr [4]byte, cmd byte) (micom.Frame, bool) {
	msgs := f.v.ReceivedMessages(hdr, cmd)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range append(msgs, "") {
		if h, ok := f.held[fmt.Sprintf("%x %02x %s", hdr, cmd, m)]; ok {
			return h, true
		}
	}
	if len(msgs) == 0 {
		return micom.Frame{}, false
	}
	fr, err := f.v.EncodeReceivedRaw(msgs[0], nil, f.fields)
	return fr, err == nil
}

// handle answers one frame from the SoC.
func (f *fake) handle(fr micom.Frame) {
	f.mu.Lock()
	f.record("tx", fr)
	f.mu.Unlock()
	switch {
	case micom.IsHello(fr):
		f.logf("SoC hello")
		f.wake()
		return
	case micom.IsAliveReply(fr):
		return
	case len(fr.Payload) == 0:
		if a, ok := f.answer(micom.ReplyHeader(fr.Hdr), fr.Cmd); ok {
			f.other(fr, "answered")
			f.send(a)
			return
		}
	default:
		out, err := f.v.Reflect(fr, f.base)
		if err != nil {
			f.logf("SoC %x %02x: %v", fr.Hdr, fr.Cmd, err)
		}
		if len(out) > 0 {
			name, vals, _ := f.v.DecodeInput(fr)
			f.logf("SoC %s %v: reflected in %d frame(s)", name, vals, len(out))
			for _, o := range out {
				f.mu.Lock()
				f.owned[kindOf(f.v, o)] = true
				f.mu.Unlock()
				f.send(o)
			}
			return
		}
	}
	f.other(fr, "")
}

// other logs a SoC frame once per change, so periodic frames do not flood
// the log.
func (f *fake) other(fr micom.Frame, note string) {
	k, p := fmt.Sprintf("%x %02x", fr.Hdr, fr.Cmd), hex.EncodeToString(fr.Payload)
	f.mu.Lock()
	same := f.last[k] == p
	f.last[k] = p
	f.mu.Unlock()
	if same {
		return
	}
	s := f.v.DescribeSent(fr)
	if name, vals, ok := f.v.DecodeInput(fr); ok {
		s = fmt.Sprintf("%s %v", name, vals)
	}
	if s == "" {
		s = fr.String()
	}
	if note != "" {
		s += ": " + note
	}
	f.logf("SoC %s", s)
}

// wake brings the link up: the boot record, then, the first time, the scene.
func (f *fake) wake() {
	f.mu.Lock()
	first := !f.up
	f.up = true
	f.mu.Unlock()
	if br, ok := f.answer(micom.ReplyHeader(micom.AliveReply(0).Hdr), 0x01); ok {
		f.send(br)
	}
	if first {
		for _, p := range f.scene {
			f.sendCar(p.f)
		}
	}
}

// serve runs the link over one connection until it closes.
func (f *fake) serve(ctx context.Context, c io.ReadWriter) error {
	f.mu.Lock()
	f.w, f.up = c, false
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.w = nil; f.mu.Unlock() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The car's micom sends its boot record on its own once the SoC's UART is
	// up (go-micomd, which never says hello, receives it); stock micomd
	// gives up its hello after five tries.
	f.wake()
	go f.every(ctx, micom.AliveRequest(), 15*time.Second, f.send)
	for _, p := range f.scene {
		go f.every(ctx, p.f, p.every, f.sendCar)
	}
	if len(f.replay) > 0 {
		go f.replayLoop(ctx)
	}
	var d micom.Decoder
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		for _, fr := range d.Write(buf[:n]) {
			f.handle(fr)
		}
		if err != nil {
			return err
		}
	}
}

func (f *fake) linkUp() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.up }

// every sends fr every d once the link is up (the alive request every 15 s,
// as the car's micom).
func (f *fake) every(ctx context.Context, fr micom.Frame, d time.Duration, send func(micom.Frame) error) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if f.linkUp() {
				send(fr)
			}
		}
	}
}

func (f *fake) replayLoop(ctx context.Context) {
	for {
		start := time.Now()
		for _, t := range f.replay {
			if micom.IsAliveRequest(t.f) || micom.IsBootRecord(t.f) {
				continue
			}
			if wait := time.Duration(float64(t.at)/f.speed) - time.Since(start); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
			if f.linkUp() {
				f.sendCar(t.f)
			}
		}
		if !f.loop {
			f.logf("replay done")
			return
		}
	}
}

// scenarioEntry is one entry of a scenario: a from_micom message by the
// variant's name with the fields to set (as inputs give them), or a raw
// frame; repeated every Every (default 10 s) once the link is up.
type scenarioEntry struct {
	Msg   string         `json:"msg,omitempty"`
	Set   map[string]any `json:"set,omitempty"`
	Frame string         `json:"frame,omitempty"`
	Every string         `json:"every,omitempty"`
}

// load takes a scenario (JSON entries, an array or one object after
// another) into the scene; a boot record is held for the hello, not repeated.
func (f *fake) load(r io.Reader) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	var es []scenarioEntry
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		var more []scenarioEntry
		if strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
			if err := unmarshal(raw, &more); err != nil {
				return err
			}
		} else {
			var e scenarioEntry
			if err := unmarshal(raw, &e); err != nil {
				return err
			}
			more = append(more, e)
		}
		es = append(es, more...)
	}
	for _, e := range es {
		fr, err := f.entry(e)
		if err != nil {
			return err
		}
		every := 10 * time.Second
		if e.Every != "" {
			if every, err = time.ParseDuration(e.Every); err != nil {
				return err
			}
		}
		f.mu.Lock()
		f.hold(fr)
		f.mu.Unlock()
		if !micom.IsBootRecord(fr) {
			f.scene = append(f.scene, periodic{fr, every})
		}
	}
	return nil
}

func unmarshal(raw []byte, v any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	return d.Decode(v)
}

func (f *fake) entry(e scenarioEntry) (micom.Frame, error) {
	if e.Frame != "" {
		raw, err := hex.DecodeString(strings.ReplaceAll(e.Frame, " ", ""))
		if err != nil {
			return micom.Frame{}, err
		}
		var d micom.Decoder
		if fs := d.Write(raw); len(fs) == 1 {
			return fs[0], nil
		}
		return micom.Frame{}, errors.New("frame: not one valid frame")
	}
	return f.v.EncodeReceived(e.Msg, f.base(e.Msg), e.Set)
}

// ctl takes scenario entries, one JSON object per line, on a unix socket,
// and sends each at once.
func (f *fake) ctl(ctx context.Context, path string) error {
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			return nil
		}
		go func() {
			defer c.Close()
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				var e scenarioEntry
				err := unmarshal(sc.Bytes(), &e)
				var fr micom.Frame
				if err == nil {
					fr, err = f.entry(e)
				}
				if err == nil {
					err = f.send(fr)
				}
				if err != nil {
					fmt.Fprintf(c, "{\"error\":%q}\n", err.Error())
				} else {
					fmt.Fprintln(c, `{"ok":true}`)
				}
			}
		}()
	}
}

func dial(addr string) (io.ReadWriteCloser, error) {
	kind, rest, _ := strings.Cut(addr, ":")
	switch kind {
	case "unix", "tcp":
		return net.Dial(kind, rest)
	case "tty":
		return micom.OpenTTY(rest, 115200)
	}
	return nil, fmt.Errorf("address %q: want unix:PATH, tcp:HOST:PORT or tty:DEV", addr)
}

func accept(ctx context.Context, addr string) (io.ReadWriteCloser, error) {
	kind, rest, _ := strings.Cut(addr, ":")
	if kind == "unix" {
		os.Remove(rest)
	}
	l, err := net.Listen(kind, rest)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	go func() { <-ctx.Done(); l.Close() }()
	return l.Accept()
}

func main() {
	connect := flag.String("connect", "", "the SoC end: unix:PATH or tcp:HOST:PORT (a QEMU chardev server), tty:DEV (a pty or serial port)")
	listen := flag.String("listen", "", "or wait for the SoC end to connect: unix:PATH or tcp:ADDR")
	replay := flag.String("replay", "", "micomd -record capture to replay (its \"rx\" frames, at their times)")
	from := flag.Duration("from", 0, "start the replay this far into the capture")
	to := flag.Duration("to", 0, "end the replay this far into the capture (0: the end)")
	loop := flag.Bool("loop", false, "replay the window again and again")
	speed := flag.Float64("speed", 1, "replay speed")
	still := flag.Duration("still", -1, "instead of replaying, hold the car's state at this offset into the capture, each frame repeated at its own rate")
	scenario := flag.String("scenario", "", "scenario: JSON entries {msg, set, every} or {frame}, applied after the capture's state")
	record := flag.String("record", "", "write both directions here (micomd -record format, \"rx\" = from the fake)")
	ctlPath := flag.String("ctl", "", "unix socket taking scenario entries, one JSON object per line, sent at once")
	variant := flag.String("variant", micom.DefaultVariantName, "go-micomd variant: the car's messages")
	flag.Parse()
	logger := log.New(os.Stderr, "micom-fake: ", log.Lmicroseconds)
	if (*connect == "") == (*listen == "") {
		logger.Fatal("give one of -connect or -listen")
	}
	v, err := micom.LoadVariant(*variant)
	if err != nil {
		logger.Fatal(err)
	}
	f := newFake(v, logger.Printf)
	f.loop, f.speed = *loop, *speed
	if *replay != "" {
		capture, err := loadCapture(*replay)
		if err != nil {
			logger.Fatal(err)
		}
		for _, t := range capture {
			if micom.IsBootRecord(t.f) {
				f.hold(t.f)
				logger.Printf("boot record from the capture")
				break
			}
		}
		if *still >= 0 {
			f.scene = snapshot(v, capture, *still)
			for _, p := range f.scene {
				f.hold(p.f)
			}
			logger.Printf("still scene at %v: %d frame kinds", *still, len(f.scene))
		} else if f.replay = window(capture, *from, *to); len(f.replay) > 0 {
			logger.Printf("replaying %d frames over %v", len(f.replay), f.replay[len(f.replay)-1].at)
		}
	}
	if *scenario != "" {
		fh, err := os.Open(*scenario)
		if err != nil {
			logger.Fatal(err)
		}
		err = f.load(fh)
		fh.Close()
		if err != nil {
			logger.Fatalf("%s: %v", *scenario, err)
		}
	}
	if *record != "" {
		fh, err := os.Create(*record)
		if err != nil {
			logger.Fatal(err)
		}
		defer fh.Close()
		f.rec = json.NewEncoder(fh)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *ctlPath != "" {
		go f.ctl(ctx, *ctlPath)
	}
	for ctx.Err() == nil {
		var c io.ReadWriteCloser
		if *connect != "" {
			c, err = dial(*connect)
		} else {
			c, err = accept(ctx, *listen)
		}
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		logger.Printf("connected")
		go func() { <-ctx.Done(); c.Close() }()
		err = f.serve(ctx, c)
		c.Close()
		logger.Printf("disconnected: %v", err)
	}
}
