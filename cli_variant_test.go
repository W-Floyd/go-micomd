package micom

import (
	"bytes"
	"context"
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
		"send: [{name: reset, frame: \"ff 8a 01 01 01 07 00 01 72\", status: captured, ref: r}]\n")
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
		{"check a bad file", []string{"-variant-check", good, bad}, 1, good + ": ok", "denylisted"},
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
		name, flag, cmdline, want, log string
	}{
		{"auto on the EV6", "auto", ev6Line, DefaultVariantName, ""},
		{"auto elsewhere", "auto", "vehicle_info=0x12", DefaultVariantName, "no variant matches"},
		{"auto without identity", "", "", DefaultVariantName, "no variant matches"},
		{"named", "hkmc-gen5w", ev6Line, "hkmc-gen5w", "but the kernel command line matches kia-ev6-gen5w"},
		{"named, as detected", DefaultVariantName, ev6Line, DefaultVariantName, ""},
	} {
		var logs []string
		logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
		v, err := chooseVariant(tc.flag, "", func() (string, error) { return tc.cmdline, nil }, logf)
		if err != nil || v.Name != tc.want {
			t.Errorf("%s: %v %v, want %s", tc.name, v, err, tc.want)
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
