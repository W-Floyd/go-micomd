package micom

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
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
// -keyfunc turns the micom's hard-key frames into events of a virtual keyboard
// (uinput, Linux only); it is off unless the Func of the key frames is given,
// which a -dump capture shows. Keys without a -keymap entry are logged.
//
// What it sends is DefaultProfile and nothing else. -clock adds the two
// frames that carry the time, from this machine's clock: only use it when
// that clock is right, as the car may store it.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("micomd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dev := fs.String("dev", "", "the micom's serial device (the vendor's /dev/ttyTCC2, a PL011 at 0x16620000)")
	baud := fs.Int("baud", 115200, "baud rate of the link; 115200 8N1 raw is what Android's micomd sets (its tcsetattr: c_cflag 0x1002 = B115200 | CS8)")
	dump := fs.Bool("dump", false, "print every frame received")
	clock := fs.Bool("clock", false, "also send the two clock frames, from this machine's clock")
	noAlive := fs.Bool("noalive", false, "do not answer the micom's CPU-alive requests (it resets the SoC if they go unanswered)")
	keyFunc := fs.Uint("keyfunc", 0, "Func (hdr[3]<<8|cmd) of the micom's key frames, from a -dump capture; 0 leaves key injection off")
	keyMap := fs.String("keymap", "", "type.code=KEYNUM,... (decimal Linux key numbers, e.g. 5.1=116), for -keyfunc")
	every := fs.Duration("stats", 10*time.Second, "how often to print the link's counters; 0 for never")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *dev == "" {
		fs.Usage()
		return 2
	}
	lg := log.New(stderr, "", log.LstdFlags)

	port, err := OpenTTY(*dev, *baud)
	if err != nil {
		lg.Print(err)
		return 1
	}
	profile := DefaultProfile()
	if *clock {
		profile = append(profile, ClockProfile()...)
		lg.Printf("sending the clock frames from this machine's clock: %s", time.Now().Format(time.RFC3339))
	}
	cfg := Config{Port: port, Profile: profile, Logf: lg.Printf, NoAlive: *noAlive}
	if *dump {
		cfg.OnFrame = func(f Frame) {
			if s := Describe(f); s != "" {
				fmt.Fprintf(stdout, "rx %v  # %s\n", f, s)
				return
			}
			fmt.Fprintf(stdout, "rx %v\n", f)
		}
	}
	if *keyFunc != 0 {
		km, keys, err := parseKeyMap(*keyMap)
		if err != nil {
			lg.Print(err)
			return 2
		}
		ui, err := OpenUinput("micom keys", keys)
		if err != nil {
			lg.Print(err)
			return 1
		}
		defer ui.Close()
		cfg.OnFrame = injectKeys(cfg.OnFrame, ui, km, uint16(*keyFunc), lg.Printf)
	}
	d := New(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	if err := d.Run(ctx); err != nil && ctx.Err() == nil {
		lg.Print(err)
		return 1
	}
	return 0
}
