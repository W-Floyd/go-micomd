package micom

import (
	"context"
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
// prints them, one line each with the time, until it is stopped.
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
	tapPath := fs.String("tap", "", "serve every frame received, with a timestamp, on this unix socket, for -attach")
	attach := fs.String("attach", "", "print the frames of the micomd serving this -tap socket, instead of running one")
	var allow allowList
	fs.Var(&allow, "allow", "a whole frame in hex (ff 85 02 ...) the control socket may send, exactly as given, or a pattern of them: payload bytes as ranges [lo-hi] and the checksum as .. (ff 85 02 01 02 03 00 02 [00-1e] ..); repeatable. Nothing is sent unless asked on -ctl")
	ctlPath := fs.String("ctl", "", "serve a control socket here: it takes one hex frame per line and sends it if the profile or -allow lists it")
	send := fs.String("send", "", "send the frames given as arguments (hex, one per argument) to the micomd serving this -ctl socket, instead of running one")
	gap := fs.Duration("gap", 200*time.Millisecond, "the pause between the frames of -send")
	variantName := fs.String("variant", "auto", "the car: an embedded variant's name (-list-variants), or auto for the one the kernel command line matches, else "+DefaultVariantName)
	variantFile := fs.String("variant-file", "", "read the car's variant from this file instead (it may extend an embedded one)")
	listVariants := fs.Bool("list-variants", false, "list the embedded variants and exit")
	variantCheck := fs.Bool("variant-check", false, "check the variant files given as arguments, or the chosen variant, and exit")
	showVariant := fs.Bool("show-variant", false, "print the chosen variant, its parents merged in, and exit")
	keys := fs.Bool("keys", false, "inject the variant's keys: its key frame and keymap (-keyfunc and -keymap override)")
	allowVariant := fs.Bool("allow-variant", false, "the control socket may also send the variant's captured send entries")
	allowInferred := fs.Bool("allow-inferred", false, "with -allow-variant also the variant's inferred send entries, with -identify-request an inferred request, and with -input the messages known only from code or inference")
	inputPath := fs.String("input", "", "serve the structured input here: one JSON object per line, {\"msg\": NAME, field: value, ...}, encoded by the variant's to_micom messages (docs/messages.md)")
	identify := fs.Bool("identify-request", false, "ask the micom for its variant coding once, at start (the request is inferred: it needs -allow-inferred)")
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
	v, err := chooseVariant(*variantName, *variantFile, env.cmdline, lg.Printf)
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
	}

	port, err := env.open(*dev, *baud)
	if err != nil {
		lg.Print(err)
		return 1
	}
	profile := v.Profile()
	if *codeProfile {
		profile = v.CodeProfile()
	}
	if *clock {
		profile = append(profile, v.ClockProfile()...)
		lg.Printf("sending the clock frames from this machine's clock: %s", time.Now().Format(time.RFC3339))
	}
	cfg := Config{Port: port, Variant: v, Profile: profile, Logf: lg.Printf, NoAlive: *noAlive, Allow: allow.frames, AllowPatterns: allow.patterns}
	if *allowVariant {
		frames, patterns := v.Sends(*allowInferred)
		cfg.Allow = append(cfg.Allow, frames...)
		cfg.AllowPatterns = append(cfg.AllowPatterns, patterns...)
	}
	var identifyFrame *Frame
	if *identify {
		if f, err := v.IdentifyRequest(*allowInferred); err != nil {
			lg.Printf("-identify-request: %v", err)
		} else {
			identifyFrame = &f
			cfg.Allow = append(cfg.Allow, f)
		}
	}
	if *dump {
		cfg.OnFrame = func(f Frame) { io.WriteString(stdout, frameLine(v, f)) }
	}
	if *tapPath != "" {
		tap, err := ListenTap(*tapPath)
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
			if err := ServeInput(ctx, d, *inputPath, *allowInferred); err != nil {
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
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				if err := d.Send(*identifyFrame); err != nil {
					lg.Printf("-identify-request: %v", err)
				} else {
					lg.Printf("asked the micom for its variant coding: %v", *identifyFrame)
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
// the one the kernel command line matches, else the default. An error
// leaves the caller to decide; the variant is nil then.
func chooseVariant(name, file string, cmdline func() (string, error), logf func(string, ...any)) (*Variant, error) {
	if file != "" {
		v, err := LoadVariantFile(file)
		if err == nil {
			logf("variant %s from %s (not reviewed: check what it sends)", v.Name, file)
		}
		return v, err
	}
	detected := ""
	if s, err := cmdline(); err == nil {
		id := ParseIdentity(s)
		n, err := MatchVariant(id)
		switch {
		case err != nil:
			logf("%v", err)
		case n == "" && (name == "" || name == "auto"):
			logf("no variant matches %v: running as %s", id, DefaultVariantName)
		}
		detected = n
	}
	if name == "" || name == "auto" {
		name = detected
		if name == "" {
			name = DefaultVariantName
		}
	} else if detected != "" && detected != name {
		logf("variant %s chosen, but the kernel command line matches %s", name, detected)
	}
	return LoadVariant(name)
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
