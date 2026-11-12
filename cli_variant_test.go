package micom

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer the daemon's goroutines may write to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The micom's CPU-alive requests are answered whatever becomes of the
// variant: a missing, broken or unsafe file, or an unknown name, leaves the
// keepalive running (the unit resets after about 3.5 minutes without it).
func TestMainKeepsAliveWhateverTheVariant(t *testing.T) {
	broken := writeFile(t, "broken.yaml", "schema: 1\nname: broken\ncolour: red\n")
	unsafe := writeFile(t, "unsafe.yaml", "schema: 1\nname: unsafe\nextends: hkmc-gen5w\n"+
		"profile: [{name: reset, every: 1s, frame: \"ff 8a 01 01 01 07 00 01 72\", status: captured, ref: r}]\n")
	for _, tc := range []struct {
		name string
		args []string
		log  string
	}{
		{"default", nil, ""},
		{"missing file", []string{"-variant-file", "/does/not/exist.yaml"}, "keepalive only"},
		{"broken file", []string{"-variant-file", broken}, "keepalive only"},
		{"unsafe file", []string{"-variant-file", unsafe, "-allow-variant"}, "keepalive only"},
		{"unknown name", []string{"-variant", "nope"}, "keepalive only"},
		{"keys from no variant", []string{"-variant", "nope", "-keys", "-allow-variant", "-clock"}, "has no key frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			var stdout, stderr syncBuffer
			done := make(chan int, 1)
			env := runEnv{
				open:    func(string, int) (io.ReadWriteCloser, error) { return a, nil },
				ctx:     ctx,
				cmdline: func() (string, error) { return "", nil },
			}
			go func() {
				done <- run(append([]string{"-dev", "pipe", "-stats", "0"}, tc.args...), &stdout, &stderr, env)
			}()

			req := Frame{Hdr: hdrAliveReq, Cmd: cmdAlive, Payload: []byte{0}}.Encode()
			if _, err := b.Write(req); err != nil {
				t.Fatal(err)
			}
			var dec Decoder
			buf := make([]byte, 256)
			deadline := time.Now().Add(3 * time.Second)
			answered := false
			for !answered && time.Now().Before(deadline) {
				b.SetReadDeadline(deadline)
				n, err := b.Read(buf)
				if err != nil {
					break
				}
				for _, f := range dec.Write(buf[:n]) {
					if f.Hdr == hdrAliveReply && f.Cmd == cmdAlive {
						answered = true
					}
				}
			}
			if !answered {
				t.Errorf("no answer to the CPU-alive request; stderr %q", stderr.String())
			}
			cancel()
			b.Close()
			if code := <-done; code != 0 {
				t.Errorf("exit %d; stderr %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.log) {
				t.Errorf("stderr %q lacks %q", stderr.String(), tc.log)
			}
		})
	}
}

// -identify-request alone (no -allow-inferred) asks the micom once the link
// is up; its record is logged against the variant running, and -query
// reports the choice and the record from another process. A family asks
// unbidden and moves to the car its record names, on the same link.
func TestMainIdentifyAndQuery(t *testing.T) {
	const (
		ev6     = "cc 01 00 00 00 00 00 65 6e 00 00 00 00" // the EV6's codes
		unknown = "12 01 00 00 00 00 00 65 6e 00 00 00 00"
	)
	for _, tc := range []struct {
		name    string
		args    []string
		record  string
		log     string
		header  string // the table's variant line
		verdict string // the table's record line
		car     string // the record's match, and whose profile runs: "" the family's
	}{
		{"auto, as the micom says", nil, ev6, "variant kia-ev6-gen5w, as running",
			"variant kia-ev6-gen5w (default: no variant matches (no identity keys))", "variant kia-ev6-gen5w, as running", DefaultVariantName},
		{"a family, narrowed by the record", []string{"-variant", "hkmc-gen5w"}, ev6, "the micom's variant record matches kia-ev6-gen5w, of hkmc-gen5w: switching to it",
			"variant kia-ev6-gen5w (the micom's variant record, of -variant hkmc-gen5w)", "variant kia-ev6-gen5w, as running", DefaultVariantName},
		{"a family, the record names no car", []string{"-variant", "hkmc-gen5w"}, unknown, "which no variant matches; running hkmc-gen5w",
			"variant hkmc-gen5w (-variant, a family: until the micom's variant record names the car)", "which no variant matches; running hkmc-gen5w", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctl := tapPath(t)
			socks := []string{ctl, ctl + ".tap", ctl + ".input"}
			var stdout, stderr syncBuffer
			done := make(chan int, 1)
			env := runEnv{
				open:    func(string, int) (io.ReadWriteCloser, error) { return a, nil },
				ctx:     ctx,
				cmdline: func() (string, error) { return "", nil },
			}
			go func() {
				done <- run(append([]string{"-dev", "pipe", "-stats", "0", "-ctl", ctl, "-tap", socks[1], "-input", socks[2], "-identify-request", "-allow-inferred"}, tc.args...), &stdout, &stderr, env)
			}()
			asked, polled, night := make(chan struct{}, 8), make(chan struct{}, 64), make(chan struct{}, 8)
			go func() {
				var dec Decoder
				buf := make([]byte, 256)
				for {
					n, err := b.Read(buf)
					if err != nil {
						return
					}
					for _, f := range dec.Write(buf[:n]) {
						switch {
						case f.Hdr == hdrAliveReply && f.Cmd == 0x17:
							asked <- struct{}{}
						case f.Hdr == [4]byte{0x88, 0x0c, 0x01, 0x0c} && f.Cmd == 0x5c:
							select {
							case polled <- struct{}{}:
							default:
							}
						case f.Hdr == [4]byte{0x8f, 0x0d, 0x01, 0x0d} && f.Cmd == 0x0f && bytes.Equal(f.Payload, []byte{1}):
							select {
							case night <- struct{}{}:
							default:
							}
						}
					}
				}
			}()
			// The micom speaks first: the request waits for the link.
			b.Write(Frame{Hdr: hdrAliveReq, Cmd: cmdAlive, Payload: []byte{0}}.Encode())
			select {
			case <-asked:
			case <-time.After(3 * time.Second):
				t.Fatalf("no identify request; stderr %q", stderr.String())
			}
			// As a service gives them a group: kept whatever the record says.
			before := make([]os.FileInfo, len(socks))
			for i, p := range socks {
				for j := 0; j < 100; j++ {
					if fi, err := os.Stat(p); err == nil && fi.Mode().Perm() == 0o660 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err := os.Chmod(p, 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				if before[i], err = os.Stat(p); err != nil {
					t.Fatal(err)
				}
			}
			conn, err := net.Dial("unix", ctl)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			// A car's message sent before the record: a family holds it, and
			// its reply, until the record says whose engine takes it.
			in, err := net.Dial("unix", socks[2])
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			inr := bufio.NewReader(in)
			fmt.Fprintln(in, `{"msg":"cluster-day-night","mode":"night"}`)
			if len(tc.args) > 0 {
				in.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				if line, err := inr.ReadString('\n'); err == nil {
					t.Errorf("input answered before the record: %q", line)
				}
				in.SetReadDeadline(time.Time{})
			}
			b.Write(Frame{Hdr: hdrAliveReq, Cmd: 0x17, Payload: unhex(t, tc.record)}.Encode())
			in.SetReadDeadline(time.Now().Add(3 * time.Second))
			reply, err := inr.ReadString('\n')
			want := `{"ok":true}`
			if tc.car == "" {
				want = `{"error":"no message \"cluster-day-night\""}`
			}
			if err != nil || strings.TrimSpace(reply) != want {
				t.Errorf("input reply %q (%v), want %s", reply, err, want)
			}
			select {
			case <-night:
				if tc.car == "" {
					t.Error("the family sent the car's message")
				}
			case <-time.After(time.Second):
				if tc.car != "" {
					t.Error("the car's message never reached the micom")
				}
			}
			for i := 0; i < 100 && !strings.Contains(stderr.String(), tc.log); i++ {
				time.Sleep(10 * time.Millisecond)
			}
			if !strings.Contains(stderr.String(), tc.log) {
				t.Errorf("stderr %q lacks %q", stderr.String(), tc.log)
			}
			// The EV6's profile polls; the family's does not.
			select {
			case <-polled:
				if tc.car == "" {
					t.Errorf("an EV6 poll from the family; stderr %q", stderr.String())
				}
			case <-time.After(3 * time.Second):
				if tc.car != "" {
					t.Errorf("no EV6 poll; stderr %q", stderr.String())
				}
			}
			for i, p := range socks {
				fi, err := os.Stat(p)
				if err != nil || !os.SameFile(fi, before[i]) || fi.Mode().Perm() != 0o600 {
					t.Errorf("%s: not the socket served before the record (%v)", p, err)
				}
			}
			// A client connected before the record is answered by the car's daemon.
			fmt.Fprintln(conn, "snapshot")
			line, err := bufio.NewReader(conn).ReadString('\n')
			var cs Snapshot
			if err != nil || json.Unmarshal([]byte(line), &cs) != nil || cs.Variant.Reply == nil || cs.Variant.Reply.Matched != tc.car {
				t.Errorf("snapshot over the earlier connection: %v %q", err, line)
			}

			var out, errb bytes.Buffer
			if code := Main([]string{"-query", ctl, "snapshot"}, &out, &errb); code != 0 {
				t.Fatalf("-query snapshot: exit %d, %q", code, errb.String())
			}
			norm := strings.Join(strings.Fields(out.String()), " ")
			for _, w := range []string{tc.header, tc.verdict, "record ", "Country=us"} {
				if !strings.Contains(norm, w) {
					t.Errorf("table lacks %q:\n%s", w, out.String())
				}
			}
			out.Reset()
			if code := Main([]string{"-query", ctl, "snapshot", "-json"}, &out, &errb); code != 0 {
				t.Fatalf("-query snapshot -json: exit %d, %q", code, errb.String())
			}
			var s Snapshot
			if err := json.Unmarshal(out.Bytes(), &s); err != nil || s.Variant.Reply == nil || s.Variant.Reply.Matched != tc.car {
				t.Errorf("-json: %v %+v", err, s.Variant)
			}
			out.Reset()
			if code := Main([]string{"-query", ctl, "mark"}, &out, &errb); code != 0 || out.String() != "ok\n" {
				t.Errorf("-query mark: exit %d, %q %q", code, out.String(), errb.String())
			}
			cancel()
			b.Close()
			if code := <-done; code != 0 {
				t.Errorf("exit %d; stderr %q", code, stderr.String())
			}
		})
	}
}

// The variant commands print and exit, without a device.
func TestMainVariantCommands(t *testing.T) {
	good := writeFile(t, "good.yaml", "schema: 1\nname: good\nextends: kia-ev6-gen5w\ntitle: An EV6 with a test title\n")
	bad := writeFile(t, "bad.yaml", "schema: 1\nname: bad\nextends: kia-ev6-gen5w\nprofile: [{name: p, every: 1s, frame: \"ff 8a 01 01 01 07 00 01 72\", status: captured, ref: r}]\n")
	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		out, err string
	}{
		{"list", []string{"-list-variants"}, 0, "kia-ev6-gen5w\tKia EV6 (CV), Mobis DAudio Plus Gen5 Wide (extends hkmc-gen5w-ev; matches vehicle_info=[0xcc])", ""},
		{"list the base", []string{"-list-variants"}, 0, "hkmc-gen5w\t", ""},
		{"show the default", []string{"-show-variant"}, 0, "name: kia-ev6-gen5w", ""},
		{"show merged", []string{"-show-variant"}, 0, "name: EV power block", ""},
		{"show a named one", []string{"-show-variant", "-variant", "hkmc-gen5w"}, 0, "abstract: true", ""},
		{"show a file", []string{"-show-variant", "-variant-file", good}, 0, "title: An EV6 with a test title", ""},
		{"show an unknown one", []string{"-show-variant", "-variant", "nope"}, 1, "", "no such embedded variant"},
		{"check files", []string{"-variant-check", good}, 0, good + ": ok", ""},
		{"check a bad file", []string{"-variant-check", good, bad}, 1, good + ": ok", "dangerous"},
		{"check the chosen", []string{"-variant-check", "-variant", "kia-ev6-gen5w"}, 0, "kia-ev6-gen5w: ok", ""},
	} {
		var out, errb bytes.Buffer
		if code := Main(tc.args, &out, &errb); code != tc.code {
			t.Errorf("%s: exit %d, want %d; stderr %q", tc.name, code, tc.code, errb.String())
		}
		if !strings.Contains(out.String(), tc.out) {
			t.Errorf("%s: stdout %q lacks %q", tc.name, out.String(), tc.out)
		}
		if !strings.Contains(errb.String(), tc.err) {
			t.Errorf("%s: stderr %q lacks %q", tc.name, errb.String(), tc.err)
		}
	}
}

func TestChooseVariant(t *testing.T) {
	const ev6Line = "vehicle_info=0xcc country_info=0x01"
	for _, tc := range []struct {
		name, flag, cmdline, want, log, source, family string
	}{
		{"auto on the EV6", "auto", ev6Line, DefaultVariantName, "", "cmdline", ""},
		{"auto elsewhere", "auto", "vehicle_info=0x12", DefaultVariantName, "no variant matches", "default", ""},
		{"auto without identity", "", "", DefaultVariantName, "no variant matches", "default", ""},
		{"a family, narrowed by the command line", "hkmc-gen5w", ev6Line, DefaultVariantName, "variant kia-ev6-gen5w, of hkmc-gen5w, from the kernel command line", "cmdline", "hkmc-gen5w"},
		{"a family, left to the record", "hkmc-gen5w", "vehicle_info=0x12", "hkmc-gen5w", "", "flag", "hkmc-gen5w"},
		{"named, as detected", DefaultVariantName, ev6Line, DefaultVariantName, "", "flag", ""},
	} {
		var logs []string
		logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
		v, ch, err := chooseVariant(tc.flag, "", func() (string, error) { return tc.cmdline, nil }, logf)
		if err != nil || v.Name != tc.want || ch.Name != tc.want || ch.Source != tc.source || ch.Family != tc.family {
			t.Errorf("%s: %v %+v %v, want %s from %s", tc.name, v, ch, err, tc.want, tc.source)
			continue
		}
		if all := strings.Join(logs, "|"); tc.log == "" && all != "" || !strings.Contains(all, tc.log) {
			t.Errorf("%s: logged %q, want %q", tc.name, all, tc.log)
		}
	}
}

// With -keys the variant gives the key frame and keymap and the flags win;
// without, the flags are all there is, as before.
func TestKeySetup(t *testing.T) {
	v := ev6(t)
	serviceMap, _, _ := parseKeyMap(serviceKeymap)
	for _, tc := range []struct {
		name    string
		fn      uint16
		flagMap string
		keys    bool
		wantFn  uint16
		check   func(keymap) bool
	}{
		{"flags only", 0x8701, "2.6=115", false, 0x8701, func(m keymap) bool { return len(m.push) == 1 && len(m.knob) == 0 }},
		{"no flags, no -keys", 0, "", false, 0, func(m keymap) bool { return len(m.push) == 0 }},
		{"-keys", 0, "", true, 0x8701, func(m keymap) bool { return reflect.DeepEqual(m, serviceMap) }},
		{"-keys, a flag wins", 0, "2.6=1,9.1.up=2", true, 0x8701, func(m keymap) bool {
			return m.push[keyKey{2, 6}] == 1 && m.knob[knobKey{9, 1, true}] == 2 && m.push[keyKey{2, 7}] == 114
		}},
		{"-keys, -keyfunc wins", 0x1234, "", true, 0x1234, func(m keymap) bool { return reflect.DeepEqual(m, serviceMap) }},
	} {
		fn, km, keys, err := keySetup(v, tc.fn, tc.flagMap, tc.keys)
		if err != nil || fn != tc.wantFn || !tc.check(km) {
			t.Errorf("%s: fn %#x keymap %v keys %v err %v", tc.name, fn, km, keys, err)
		}
	}
	// The variant's maps are not changed by an override.
	if vm, _ := v.keymap(); vm.push[keyKey{2, 6}] != 115 {
		t.Error("the variant's keymap was changed")
	}
}

// A micom reset since a SoC last asked sends nothing until the boot-mode
// request arrives, so that request goes out with nothing received; the
// others wait for the micom to speak, then go.
func TestMainStartupRequestsOnASilentLink(t *testing.T) {
	for _, tc := range []struct {
		flag string
		hdr  [4]byte
		cmd  byte
		sent bool // with nothing received
	}{
		{"-boot-mode-request", hdrAliveReply, 0x01, true},
		{"-identify-request", hdrAliveReply, 0x17, false},
		{"-ev-schedule-request", [4]byte{0x88, 0x0c, 0x01, 0x0c}, 0x53, false},
		{"-ev-schedule-request", [4]byte{0x88, 0x0c, 0x01, 0x0c}, 0x54, false},
	} {
		t.Run(fmt.Sprintf("%s %#02x", tc.flag, tc.cmd), func(t *testing.T) {
			a, b := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			var stdout, stderr syncBuffer
			done := make(chan int, 1)
			env := runEnv{
				open:    func(string, int) (io.ReadWriteCloser, error) { return a, nil },
				ctx:     ctx,
				cmdline: func() (string, error) { return "", nil },
			}
			go func() { done <- run([]string{"-dev", "pipe", "-stats", "0", tc.flag}, &stdout, &stderr, env) }()
			asked := make(chan struct{}, 8)
			go func() {
				var dec Decoder
				buf := make([]byte, 256)
				for {
					n, err := b.Read(buf)
					if err != nil {
						return
					}
					for _, f := range dec.Write(buf[:n]) {
						if f.Hdr == tc.hdr && f.Cmd == tc.cmd {
							asked <- struct{}{}
						}
					}
				}
			}()
			select {
			case <-asked:
				if !tc.sent {
					t.Errorf("sent before the micom spoke")
				}
			case <-time.After(time.Second):
				if tc.sent {
					t.Errorf("not sent; stderr %q", stderr.String())
				}
			}
			if !tc.sent {
				b.Write(Frame{Hdr: hdrAliveReq, Cmd: cmdAlive, Payload: []byte{0}}.Encode())
				select {
				case <-asked:
				case <-time.After(time.Second):
					t.Errorf("not sent once the micom spoke; stderr %q", stderr.String())
				}
			}
			cancel()
			b.Close()
			<-done
		})
	}
}
