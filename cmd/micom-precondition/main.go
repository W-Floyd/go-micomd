// micom-precondition asks the car to condition (warm) its battery, as stock's
// navigation does when it routes to a fast charger: through micomd's -input
// (which needs -allow-inferred), the EV6 variant's batt-cond-* messages.
//
//	micom-precondition [flags] start   # until -for ends or a signal, then stop
//	micom-precondition [flags] stop
//
// The BMS decides: the car READY, battery conditioning enabled in its EV
// settings, the pack cold. While it runs, the pack's minimum temperature is
// logged from micomd's -tap.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The route stock's navigation would report, and how often; Launchpad's
// (github.com/paytondevs/gen5w-preconditioning), which the BMS accepts.
const (
	routeKm      = 9
	routeMinutes = 18
	refresh      = 5 * time.Second
	// Stock's HEVService sends station 07 07 (invalid) 120 ms after every
	// station event: the event is a pulse.
	pulse = 120 * time.Millisecond
)

type client struct {
	c  net.Conn
	rd *bufio.Reader
	lg *log.Logger
}

func (c *client) send(line string) error {
	c.c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprintln(c.c, line); err != nil {
		return err
	}
	ans, err := c.rd.ReadString('\n')
	if err != nil {
		return err
	}
	if ans = strings.TrimSpace(ans); strings.Contains(ans, `"error"`) {
		return fmt.Errorf("%s: %s", line, ans)
	}
	c.lg.Printf("sent %s", line)
	return nil
}

func (c *client) route(km, minutes int) error {
	if err := c.send(fmt.Sprintf(`{"msg":"batt-cond-route-distance","km":%d}`, km)); err != nil {
		return err
	}
	return c.send(fmt.Sprintf(`{"msg":"batt-cond-remain-time","minutes":%d}`, minutes))
}

func (c *client) station(flag, typ int) error {
	if err := c.send(fmt.Sprintf(`{"msg":"batt-cond-ev-station","flag":%d,"type":%d}`, flag, typ)); err != nil {
		return err
	}
	time.Sleep(pulse)
	return c.send(`{"msg":"batt-cond-ev-station","flag":7,"type":7}`)
}

// start is stock's order: the route, then the station event.
func (c *client) start() error {
	if err := c.route(routeKm, routeMinutes); err != nil {
		return err
	}
	time.Sleep(10 * time.Millisecond)
	return c.station(2, 3)
}

// stop is every part of the cancel, even after one fails.
func (c *client) stop() error {
	err := c.route(2047, 1023)
	if e := c.station(0, 0); err == nil {
		err = e
	}
	return err
}

// battery is the pack as micomd's tap decodes it, sent on soc whenever the
// state of charge is read.
type battery struct {
	soc  chan float64
	temp string
}

// tapField is the value of name=value in a tap line's description.
func tapField(line, name string) (string, bool) {
	_, rest, ok := strings.Cut(line, name+"=")
	if !ok {
		return "", false
	}
	v, _, _ := strings.Cut(rest, " ")
	return v, true
}

// watch reads the pack's state of charge and minimum temperature off the tap
// until ctx ends, logging the temperature on change.
func (b *battery) watch(ctx context.Context, tap string, lg *log.Logger) error {
	conn, err := net.Dial("unix", tap)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); conn.Close() }()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		if v, ok := tapField(sc.Text(), "Battery minimum temperature"); ok && v != b.temp {
			lg.Printf("battery minimum temperature %s °C", v)
			b.temp = v
		}
		if v, ok := tapField(sc.Text(), "State of charge"); ok {
			if soc, err := strconv.ParseFloat(v, 64); err == nil {
				select {
				case b.soc <- soc:
				default:
				}
			}
		}
	}
	return nil
}

func main() {
	input := flag.String("input", "/run/micomd.input", "micomd's -input socket")
	tap := flag.String("tap", "/run/micomd.sock", "micomd's -tap socket: the state of charge and the pack temperature")
	dur := flag.Duration("for", 30*time.Minute, "with start: stop after this long")
	minSoC := flag.Float64("min-soc", 20, "with start: % state of charge to start at and keep; below it, stop")
	flag.Parse()
	lg := log.New(os.Stderr, "", log.LstdFlags)
	if flag.NArg() != 1 || (flag.Arg(0) != "start" && flag.Arg(0) != "stop") {
		fmt.Fprintln(os.Stderr, "usage: micom-precondition [flags] start|stop")
		os.Exit(2)
	}
	conn, err := net.Dial("unix", *input)
	if err != nil {
		lg.Fatal(err)
	}
	c := &client{c: conn, rd: bufio.NewReader(conn), lg: lg}
	if flag.Arg(0) == "stop" {
		if err := c.stop(); err != nil {
			lg.Fatal(err)
		}
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancelFor := context.WithTimeout(ctx, *dur)
	defer cancelFor()
	// The state of charge is the car's own, read before starting: the floor
	// is ours, whatever the BMS keeps.
	b := &battery{soc: make(chan float64, 1)}
	go func() {
		if err := b.watch(ctx, *tap, lg); err != nil {
			lg.Printf("tap %s: %v", *tap, err)
		}
	}()
	// The battery frame comes only on a change: 3-14 s apart parked.
	lg.Print("waiting for the state of charge")
	select {
	case soc := <-b.soc:
		if soc < *minSoC {
			lg.Fatalf("state of charge %g %% is below -min-soc %g %%: not starting", soc, *minSoC)
		}
		lg.Printf("state of charge %g %%", soc)
	case <-time.After(60 * time.Second):
		lg.Fatal("no state of charge on the tap in 60 s: not starting")
	case <-ctx.Done():
		return
	}
	if err := c.start(); err != nil {
		lg.Print(err)
		if err := c.stop(); err != nil {
			lg.Print(err)
		}
		os.Exit(1)
	}
	tick := time.NewTicker(refresh)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			lg.Print("stopping")
			if err := c.stop(); err != nil {
				lg.Fatal(err)
			}
			return
		case soc := <-b.soc:
			if soc < *minSoC {
				lg.Printf("state of charge %g %% is below -min-soc %g %%: stopping", soc, *minSoC)
				if err := c.stop(); err != nil {
					lg.Fatal(err)
				}
				return
			}
		case <-tick.C:
			// The route again, never the station event.
			if err := c.route(routeKm, routeMinutes); err != nil {
				lg.Print(err)
			}
		}
	}
}
