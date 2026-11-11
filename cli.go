package micom

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Main is the micomd command: it takes over the SoC side of the head unit's
// micom serial link from Android's daemon of the same name. It decodes what
// the micom sends and sends the routine frames Android did, so the micom does
// not take the SoC for hung. args are os.Args[1:]. It returns the exit code
// and writes only to the writers it is given, so a larger program (the Boot
// Manager) can embed it as a subcommand.
//
//	micomd -dev /dev/ttyAMA1 [-baud 115200] [-dump] [-clock] [-keyfunc 0xHHCC -keymap type.code=N,...]
//
// The car is a variant (docs/variants.md): by default the one the kernel
// command line's vehicle code matches, else the EV6; -variant NAME or
// -variant-file PATH chooses. It names the frames, gives the routine
// frames, and with -keys and -allow-variant the keys and the frames the
// control socket may send. A variant that fails to load leaves the keepalive
// only: the CPU-alive answer never depends on one.
//
// -keyfunc turns the micom's hard-key frames into events of a virtual keyboard
// (uinput, Linux only); it is off unless the Func of the key frames is given,
// which a -dump capture shows, or -keys takes it from the variant. Keys
// without a -keymap entry are logged.
//
// -tap SOCKET lets another process watch the frames of a running daemon, which
// owns the UART and so cannot be read from otherwise: micomd -attach SOCKET
// prints them, one line each with the time, until it is stopped. micomd
// -query SOCKET snapshot asks the daemon serving -ctl SOCKET what it has
// received, per frame kind, and which variant it runs and why; -query
// SOCKET mark restarts the count of changed bytes. Neither sends a frame.
//
// -record PATH writes every frame received and sent to a file of JSON lines
// with the wall-clock time (Recorder), rotated at -record-max.
//
// What it sends is the variant's profile and nothing else. -clock adds the
// two frames that carry the time, from this machine's clock: only use it when
// that clock is right, as the car may store it.
func Main(args []string, stdout, stderr io.Writer) int {
	return run(args, stdout, stderr, runEnv{})
}

// runEnv is what the tests replace: the port, the context and the kernel
// command line.
type runEnv struct {
	open    func(dev string, baud int) (io.ReadWriteCloser, error)
	ctx     context.Context // nil: until SIGINT or SIGTERM
	cmdline func() (string, error)
}

func run(args []string, stdout, stderr io.Writer, env runEnv) int {
	if env.open == nil {
		env.open = func(dev string, baud int) (io.ReadWriteCloser, error) { return OpenTTY(dev, baud) }
	}
	if env.cmdline == nil {
		env.cmdline = func() (string, error) {
			b, err := os.ReadFile("/proc/cmdline")
			return string(b), err
		}
	}
	fs := flag.NewFlagSet("micomd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dev := fs.String("dev", "", "the micom's serial device (the vendor's /dev/ttyTCC2, a PL011 at 0x16620000)")
	baud := fs.Int("baud", 115200, "baud rate of the link; 115200 8N1 raw is what Android's micomd sets (its tcsetattr: c_cflag 0x1002 = B115200 | CS8)")
	dump := fs.Bool("dump", false, "print every frame received")
	clock := fs.Bool("clock", false, "also send the two clock frames, from this machine's clock")
	codeProfile := fs.Bool("code-profile", false, "send the variant's opt_in code profile entries, known only from stock code, in place of the captured ones they correct")
	noAlive := fs.Bool("noalive", false, "do not answer the micom's CPU-alive requests (it resets the SoC if they go unanswered)")
	keyFunc := fs.Uint("keyfunc", 0, "Func (hdr[3]<<8|cmd) of the micom's key frames, from a -dump capture; 0 leaves key injection off unless -keys")
	keyMap := fs.String("keymap", "", "type.code=KEYNUM for push keys and type.code.up|down=KEYNUM for knobs, comma separated (decimal Linux key numbers: 2.6=115,2.7=114,9.1.up=115,9.1.down=114), for -keyfunc; with -keys, entries over the variant's")
	every := fs.Duration("stats", 10*time.Second, "how often to print the link's counters; 0 for never")
	recordPath := fs.String("record", "", "record every frame received and sent to this file, one JSON object per line with the wall-clock time, rotated to PATH.1 at -record-max")
	recordMax := fs.String("record-max", "32M", "the size at which -record's file is rotated (bytes, or with K, M or G); the two files take at most twice this")
	tapPath := fs.String("tap", "", "serve every frame received, with a timestamp, on this unix socket, for -attach")
	attach := fs.String("attach", "", "print the frames of the micomd serving this -tap socket, instead of running one")
	var allow allowList
	fs.Var(&allow, "allow", "a whole frame in hex (ff 85 02 ...) the control socket may send, exactly as given, or a pattern of them: payload bytes as ranges [lo-hi] and the checksum as .. (ff 85 02 01 02 03 00 02 [00-1e] ..); repeatable. Nothing is sent unless asked on -ctl")
	ctlPath := fs.String("ctl", "", "serve a control socket here: it takes one hex frame per line and sends it if the profile or -allow lists it")
	send := fs.String("send", "", "send the frames given as arguments (hex, one per argument) to the micomd serving this -ctl socket, instead of running one")
	gap := fs.Duration("gap", 200*time.Millisecond, "the pause between the frames of -send")
	query := fs.String("query", "", "ask the micomd serving this -ctl socket a read-only question given as the argument, instead of running one: snapshot (a table of what it has received, and its variant) or mark (start counting changed bytes afresh); never sends a frame")
	jsonOut := fs.Bool("json", false, "with -query snapshot, print the raw JSON")
	watch := fs.Duration("watch", 0, "with -query snapshot, repeat it this often until stopped")
	variantName := fs.String("variant", "auto", "the car: an embedded variant's name (-list-variants), or auto for the one the kernel command line matches, else "+DefaultVariantName)
	variantFile := fs.String("variant-file", "", "read the car's variant from this file instead (it may extend an embedded one)")
	listVariants := fs.Bool("list-variants", false, "list the embedded variants and exit")
	variantCheck := fs.Bool("variant-check", false, "check the variant files given as arguments, or the chosen variant, and exit")
	showVariant := fs.Bool("show-variant", false, "print the chosen variant, its parents merged in, and exit")
	keys := fs.Bool("keys", false, "inject the variant's keys: its key frame and keymap (-keyfunc and -keymap override)")
	allowVariant := fs.Bool("allow-variant", false, "the control socket may also send the variant's captured send entries")
	allowInferred := fs.Bool("allow-inferred", false, "with -allow-variant also the variant's inferred send entries, and with -input the messages known only from code or inference")
	inputPath := fs.String("input", "", "serve the structured input here: one JSON object per line, {\"msg\": NAME, field: value, ...}, encoded by the variant's to_micom messages (docs/messages.md)")
	allowPersistent := fs.Bool("allow-persistent", false, "with -input also the messages whose every frame the micom writes to its EEPROM (rate-limited; each write wears it)")
	allowDangerous := fs.Bool("allow-dangerous", false, "allow the variant's dangerous frames (those that reset, rewrite or reconfigure the micom: power config, factory reset, variant-code save, update/download reservations). Off, they are refused on every path")
	identify := fs.Bool("identify-request", false, "ask the micom for its variant coding once the link is up, retried up to 5 times 3 s apart until it answers; the answer is logged against the variant running and reported by -query snapshot. The only frame it adds is the variant's identify request (inferred on HKMC: this flag alone allows it)")
	bootMode := fs.Bool("boot-mode-request", false, "ask the micom for its boot record once the link is up, retried up to 4 times 2 s apart until it answers, as stock does at every boot: the record carries the state the micom otherwise sends only on change (the EV6's illumination). The only frame it adds is the variant's boot-mode-request send entry; the micom clears its boot-reason flags on it")
	climateStatus := fs.Bool("climate-status-request", false, "ask the climate for its whole state once the link is up (stock's All_Datc_Req), retried once 1.5 s later as stock does: the reply carries the panel's state, otherwise sent only on change. The only frame it adds is the variant's climate-status-request send entry (inferred, never tried on a car: this flag alone allows it)")
	bodyStatus := fs.Bool("body-status-request", false, "ask the body for its whole state once the link is up: stock's B-CAN validity check, every 1 s up to 5 times until valid, then its All_B_CAN_Req; the reply carries the doors, locks and 12 V charge, otherwise sent only on change. The only frames it adds are the variant's bcan-valid-request and body-status-request send entries (inferred, never tried on a car: this flag alone allows them)")
	usmUnits := fs.Bool("usm-units-request", false, "ask the micom for the car's unit settings once the link is up (stock's read-all of the units group), retried up to 3 times 2 s apart. The only frame it adds is the variant's usm-units-request send entry (inferred, never tried on a car: this flag alone allows it)")
	hevStatus := fs.Bool("hev-status-request", false, "ask the micom for its HEV status once the link is up, retried up to 3 times 2 s apart until it answers, as stock does at start: the bulk 0x8c01 reply carries the range with the climate on and off, otherwise sent only on change. The only frame it adds is the variant's hev-status-request send entry (inferred, never tried on a car: this flag alone allows it)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *send != "" {
		if fs.NArg() == 0 {
			fmt.Fprintln(stderr, "micomd -send SOCKET 'ff ...' ['ff ...']: no frame given")
			return 2
		}
		if err := SendControl(*send, fs.Args(), *gap, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if *query != "" {
		req := fs.Arg(0)
		// Flags may follow the request too: -query S snapshot -json.
		if fs.NArg() > 0 {
			if err := fs.Parse(fs.Args()[1:]); err != nil {
				return 2
			}
		}
		if req == "" || fs.NArg() != 0 {
			fmt.Fprintln(stderr, "micomd -query SOCKET snapshot|mark: give one request")
			return 2
		}
		if err := runQuery(env.ctx, *query, req, *jsonOut, *watch, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if *attach != "" {
		if err := Attach(*attach, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if *listVariants {
		for _, n := range VariantNames() {
			v, err := LoadVariant(n)
			if err != nil {
				fmt.Fprintf(stdout, "%s\tBROKEN: %v\n", n, err)
				continue
			}
			fmt.Fprintf(stdout, "%s\t%s%s\n", n, v.Title, variantNote(v))
		}
		return 0
	}
	if *variantCheck && fs.NArg() > 0 {
		code := 0
		for _, p := range fs.Args() {
			if _, err := LoadVariantFile(p); err != nil {
				fmt.Fprintln(stderr, err)
				code = 1
				continue
			}
			fmt.Fprintf(stdout, "%s: ok\n", p)
		}
		return code
	}

	if *dev == "" && !*variantCheck && !*showVariant {
		fs.Usage()
		return 2
	}
	lg := log.New(stderr, "", log.LstdFlags)
	v, choice, err := chooseVariant(*variantName, *variantFile, env.cmdline, lg.Printf)
	if *variantCheck || *showVariant {
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if *variantCheck {
			fmt.Fprintf(stdout, "%s: ok\n", v.Name)
			return 0
		}
		out, err := v.YAML()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		stdout.Write(out)
		return 0
	}
	if err != nil {
		// Never fatal: without an answer to its CPU-alive requests the micom
		// resets the unit, so a bad variant leaves the keepalive running.
		lg.Printf("%v; running the keepalive only", err)
		v = emptyVariant
		choice.Name = v.Name
		choice.Note = strings.TrimPrefix(choice.Note+"; ", "; ") + err.Error()
	}

	recMax, err := ParseSize(*recordMax)
	if err != nil {
		lg.Printf("-record-max: %v", err)
		return 2
	}
	port, err := env.open(*dev, *baud)
	if err != nil {
		lg.Print(err)
		return 1
	}
	var rec *Recorder
	if *recordPath != "" {
		// Never fatal: the link matters more than its record.
		if rec, err = NewRecorder(*recordPath, recMax, v.Name, lg.Printf); err != nil {
			lg.Printf("-record: %v; not recording", err)
			rec = nil
		} else {
			defer rec.Close()
		}
	}
	profile := v.Profile()
	if *codeProfile {
		profile = v.CodeProfile()
	}
	if *clock {
		profile = append(profile, v.ClockProfile()...)
		lg.Printf("sending the clock frames from this machine's clock: %s", time.Now().Format(time.RFC3339))
	}
	cfg := Config{Port: port, Variant: v, Choice: choice, Recorder: rec, Profile: profile, Logf: lg.Printf, NoAlive: *noAlive, Allow: allow.frames, AllowPatterns: allow.patterns, AllowDangerous: *allowDangerous}
	if *allowVariant {
		frames, patterns := v.Sends(*allowInferred, *allowDangerous)
		cfg.Allow = append(cfg.Allow, frames...)
		cfg.AllowPatterns = append(cfg.AllowPatterns, patterns...)
	}
	var identifyFrame *Frame
	if *identify {
		// The flag is the opt-in for this one frame, inferred or not, so a
		// service can ask without -allow-inferred's other frames.
		if f, err := v.IdentifyRequest(true); err != nil {
			lg.Printf("-identify-request: %v", err)
		} else {
			identifyFrame = &f
			cfg.Allow = append(cfg.Allow, f)
		}
	}
	var bootModeFrame *Frame
	var bootRecord atomic.Bool
	if *bootMode {
		if f, ok := v.SendNamed(bootModeRequest); !ok {
			lg.Printf("-boot-mode-request: variant %s has no %s send entry", v.Name, bootModeRequest)
		} else {
			bootModeFrame = &f
			cfg.Allow = append(cfg.Allow, f)
		}
	}
	var hevStatusFrame *Frame
	var hevStatusReply atomic.Bool
	if *hevStatus {
		if f, ok := v.SendNamed(hevStatusRequest); !ok {
			lg.Printf("-hev-status-request: variant %s has no %s send entry", v.Name, hevStatusRequest)
		} else {
			hevStatusFrame = &f
			cfg.Allow = append(cfg.Allow, f)
		}
	}
	// Stock's other start-up reads, each its own opt-in. A chain's reads go
	// in order, each once the one before was answered.
	var chains [][]*startupRead
	read := func(flag, send, reply, what string, tries int, wait time.Duration, answers func(Frame) bool) *startupRead {
		f, ok := v.SendNamed(send)
		if !ok {
			lg.Printf("%s: variant %s has no %s send entry", flag, v.Name, send)
			return nil
		}
		cfg.Allow = append(cfg.Allow, f)
		if answers == nil {
			answers = func(f Frame) bool { _, name := v.decode(f); return name == reply }
		}
		return &startupRead{flag: flag, frame: f, what: what, reply: reply, tries: tries, wait: wait, answers: answers}
	}
	if *climateStatus {
		if r := read("-climate-status-request", climateStatusRequest, climateSnapshotName, "the climate's state", 2, 1500*time.Millisecond, nil); r != nil {
			chains = append(chains, []*startupRead{r})
		}
	}
	if *bodyStatus {
		valid := read("-body-status-request", bcanValidRequest, "B-CAN valid", "B-CAN's validity", 5, time.Second, func(f Frame) bool {
			for _, fl := range v.Decode(f) {
				if fl.Name == "B-CAN valid" && fl.Valid && fl.Value != 0 {
					return true
				}
			}
			return false
		})
		body := read("-body-status-request", bodyStatusRequest, bodySnapshotName, "the body's state", 2, 2*time.Second, nil)
		if valid != nil && body != nil {
			chains = append(chains, []*startupRead{valid, body})
		}
	}
	if *usmUnits {
		if r := read("-usm-units-request", usmUnitsRequest, usmUnitsName, "the unit settings", 3, 2*time.Second, nil); r != nil {
			chains = append(chains, []*startupRead{r})
		}
	}
	if *dump {
		cfg.OnFrame = func(f Frame) { io.WriteString(stdout, frameLine(v, f)) }
	}
	if len(chains) > 0 {
		prev := cfg.OnFrame
		cfg.OnFrame = func(f Frame) {
			if prev != nil {
				prev(f)
			}
			for _, c := range chains {
				for _, r := range c {
					if r.answers(f) {
						r.answered.Store(true)
					}
				}
			}
		}
	}
	if bootModeFrame != nil {
		prev := cfg.OnFrame
		cfg.OnFrame = func(f Frame) {
			if prev != nil {
				prev(f)
			}
			if _, name := v.decode(f); strings.HasPrefix(name, bootRecordName) {
				bootRecord.Store(true)
			}
		}
	}
	if hevStatusFrame != nil {
		prev := cfg.OnFrame
		cfg.OnFrame = func(f Frame) {
			if prev != nil {
				prev(f)
			}
			if _, name := v.decode(f); name == hevStatusName {
				hevStatusReply.Store(true)
			}
		}
	}
	// The micom's variant record, whoever asked for it, is logged against the
	// variant running, once per distinct verdict.
	{
		prev, said := cfg.OnFrame, ""
		cfg.OnFrame = func(f Frame) {
			if prev != nil {
				prev(f)
			}
			if id, ok := v.ReplyIdentity(f); ok {
				if _, msg := recordVerdict(choice.Name, id); msg != said {
					lg.Print(msg)
					said = msg
				}
			}
		}
	}
	var tap *Tap
	if *tapPath != "" {
		var err error
		tap, err = ListenTap(*tapPath)
		if err != nil {
			lg.Print(err)
			return 1
		}
		defer tap.Close()
		prev := cfg.OnFrame
		cfg.OnFrame = func(f Frame) {
			if prev != nil {
				prev(f)
			}
			tap.Send(time.Now().Format("15:04:05.000 ") + frameLine(v, f))
		}
	}
	if *keyFunc != 0 || *keys {
		fn, km, keyList, err := keySetup(v, uint16(*keyFunc), *keyMap, *keys)
		switch {
		case err != nil:
			lg.Print(err)
			return 2
		case fn == 0:
			lg.Printf("-keys: variant %s has no key frame; give -keyfunc", v.Name)
		default:
			ui, err := OpenUinput("micom keys", keyList)
			if err != nil {
				lg.Print(err)
				return 1
			}
			defer ui.Close()
			cfg.OnFrame = injectKeys(cfg.OnFrame, ui, km, fn, lg.Printf)
		}
	}
	d := New(cfg)
	if tap != nil {
		// A new client gets the last frame of each kind, at the time it came.
		tap.SetReplay(func() []string {
			var lines []string
			for _, f := range d.Latest() {
				lines = append(lines, f.At.Format("15:04:05.000 ")+frameLine(v, f.Frame))
			}
			return lines
		})
	}

	ctx := env.ctx
	if ctx == nil {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	if *ctlPath != "" {
		go func() {
			if err := ServeControl(ctx, d, *ctlPath); err != nil {
				lg.Printf("control socket: %v", err)
			}
		}()
	}
	if *inputPath != "" {
		go func() {
			if err := ServeInput(ctx, d, *inputPath, InputGates{Inferred: *allowInferred, Persistent: *allowPersistent, Dangerous: *allowDangerous}); err != nil {
				lg.Printf("input socket: %v", err)
			}
		}()
	}
	if *every > 0 {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(*every):
					lg.Print(d.Stats())
				}
			}
		}()
	}
	if identifyFrame != nil {
		go d.askVariant(ctx, *identifyFrame, identifyTries, identifyWait)
	}
	if bootModeFrame != nil {
		go d.ask(ctx, *bootModeFrame, "-boot-mode-request", "its boot record", "boot record", bootRecord.Load, bootModeTries, bootModeWait)
	}
	if hevStatusFrame != nil {
		go d.ask(ctx, *hevStatusFrame, "-hev-status-request", "its HEV status", "HEV status", hevStatusReply.Load, hevStatusTries, hevStatusWait)
	}
	for _, c := range chains {
		go func() {
			for _, r := range c {
				d.ask(ctx, r.frame, r.flag, r.what, r.reply, r.answered.Load, r.tries, r.wait)
				if !r.answered.Load() {
					return
				}
			}
		}()
	}
	if err := d.Run(ctx); err != nil && ctx.Err() == nil {
		lg.Print(err)
		return 1
	}
	return 0
}

// chooseVariant is the car: the file, else the named variant, else (auto)
// the one the kernel command line matches, else the default, and why, for
// Snapshot. An error leaves the caller to decide; the variant is nil then.
func chooseVariant(name, file string, cmdline func() (string, error), logf func(string, ...any)) (*Variant, VariantChoice, error) {
	var ch VariantChoice
	// Read even with a file, so the snapshot shows what the car says.
	s, cmdErr := cmdline()
	var matchErr error
	if cmdErr != nil {
		ch.Note = fmt.Sprintf("kernel command line: %v", cmdErr)
	} else {
		ch.Cmdline = ParseIdentity(s)
		if ch.Matched, matchErr = MatchVariant(ch.Cmdline); matchErr != nil {
			ch.Note = matchErr.Error()
		}
	}
	if file != "" {
		ch.Source, ch.File = "file", file
		v, err := LoadVariantFile(file)
		if err == nil {
			ch.Name = v.Name
			logf("variant %s from %s (not reviewed: check what it sends)", v.Name, file)
		}
		return v, ch, err
	}
	auto := name == "" || name == "auto"
	switch {
	case matchErr != nil:
		logf("%v", matchErr)
	case cmdErr == nil && ch.Matched == "" && auto:
		logf("no variant matches %v: running as %s", ch.Cmdline, DefaultVariantName)
	}
	switch {
	case !auto:
		ch.Source = "flag"
		if ch.Matched != "" && ch.Matched != name {
			logf("variant %s chosen, but the kernel command line matches %s", name, ch.Matched)
		}
	case ch.Matched != "":
		ch.Source, name = "cmdline", ch.Matched
	default:
		ch.Source, name = "default", DefaultVariantName
	}
	ch.Name = name
	v, err := LoadVariant(name)
	return v, ch, err
}

// runQuery prints the answer to req from the daemon at path: a snapshot as a
// table, or its JSON; with watch, again every watch until ctx ends (nil:
// until SIGINT or SIGTERM).
func runQuery(ctx context.Context, path, req string, raw bool, watch time.Duration, w io.Writer) error {
	if watch > 0 && ctx == nil {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	for {
		ans, err := Query(path, req)
		if err != nil {
			return err
		}
		if req != "snapshot" || raw {
			fmt.Fprintln(w, ans)
		} else {
			var s Snapshot
			if err := json.Unmarshal([]byte(ans), &s); err != nil {
				return fmt.Errorf("micom: the snapshot: %w", err)
			}
			if watch > 0 {
				io.WriteString(w, "\x1b[H\x1b[2J") // home and clear: each table replaces the last
			}
			if err := s.WriteTable(w); err != nil {
				return err
			}
		}
		if watch <= 0 || req != "snapshot" {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(watch):
		}
	}
}

// keySetup is the key frame's Func, the keymap and its keys: -keyfunc and
// -keymap as given, or with -keys the variant's, the flags winning.
func keySetup(v *Variant, fn uint16, flagMap string, useVariant bool) (uint16, keymap, []uint16, error) {
	fm, fkeys, err := parseKeyMap(flagMap)
	if err != nil || !useVariant {
		return fn, fm, fkeys, err
	}
	if vfn, ok := v.KeyFunc(); ok && fn == 0 {
		fn = vfn
	}
	vm, vkeys := v.keymap()
	km := keymap{push: maps.Clone(vm.push), knob: maps.Clone(vm.knob)}
	if km.push == nil {
		km.push = map[keyKey]uint16{}
	}
	if km.knob == nil {
		km.knob = map[knobKey]uint16{}
	}
	maps.Copy(km.push, fm.push)
	maps.Copy(km.knob, fm.knob)
	keyList := slices.Concat(vkeys, fkeys)
	slices.Sort(keyList)
	return fn, km, slices.Compact(keyList), nil
}

func variantNote(v *Variant) string {
	var notes []string
	if v.Abstract {
		notes = append(notes, "abstract")
	}
	if v.Extends != "" {
		notes = append(notes, "extends "+v.Extends)
	}
	if m := v.Match(); len(m) > 0 {
		var ks []string
		for _, k := range slices.Sorted(maps.Keys(m)) {
			ks = append(ks, fmt.Sprintf("%s=%#x", k, m[k]))
		}
		notes = append(notes, "matches "+strings.Join(ks, " "))
	}
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, "; ") + ")"
}

// frameLine is a received frame as a line of text, decoded where known.
func frameLine(v *Variant, f Frame) string {
	if s := v.Describe(f); s != "" {
		return fmt.Sprintf("rx %v  # %s\n", f, s)
	}
	return fmt.Sprintf("rx %v\n", f)
}
