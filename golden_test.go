package micom

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden-*.txt")

var goldenVariants = []string{"hkmc-gen5w", "hkmc-gen5w-ev", "kia-ev6-gen5w"}

// messageHdr is a from_micom message's header with its open bytes as 01.
func messageHdr(v *Variant, m rawMessage) ([4]byte, bool) {
	h := m.Hdr
	if r, ok := v.raw.Headers[h]; ok {
		h = r.Hdr
	}
	if h == "" {
		return [4]byte{}, false
	}
	b, err := parseHdr(strings.ReplaceAll(h, "??", "01"))
	return b, err == nil
}

// goldenFrames are the corpus (frames emulated micoms sent) and one frame per
// from_micom message: as long as it must be, zeros, its guards set.
func goldenFrames(t *testing.T, v *Variant) []Frame {
	t.Helper()
	var out []Frame
	f, err := os.Open("testdata/corpus.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || l[0] == '#' {
			continue
		}
		var h, c, p string
		if n, _ := fmt.Sscanf(l, "%s %s %s", &h, &c, &p); n < 2 {
			t.Fatalf("corpus line %q", l)
		}
		hb, err := parseHdr(h)
		if err != nil {
			t.Fatal(err)
		}
		var cmd byte
		fmt.Sscanf(c, "%02x", &cmd)
		pl, err := hex.DecodeString(p)
		if err != nil {
			t.Fatalf("corpus payload %q: %v", p, err)
		}
		out = append(out, Frame{Hdr: hb, Cmd: cmd, Payload: pl})
	}
	for _, m := range v.raw.Messages {
		if m.Dir == "to_micom" || m.Transport != nil {
			continue
		}
		h, ok := messageHdr(v, m)
		if !ok {
			continue
		}
		n := 1
		if m.Length != nil {
			n = *m.Length
		} else if m.MinLength != nil {
			n = *m.MinLength
		}
		p := make([]byte, n)
		for _, g := range m.When {
			if g.Off != nil && g.Eq != nil {
				for len(p) <= *g.Off {
					p = append(p, 0)
				}
				p[*g.Off] = *g.Eq
			}
		}
		out = append(out, Frame{Hdr: h, Cmd: m.Cmd, Payload: p})
	}
	return out
}

// Every frame in the corpus, and a zero frame of every message, reads as the
// golden file says. Regenerate with -update and read the diff.
func TestGolden(t *testing.T) {
	for _, name := range goldenVariants {
		t.Run(name, func(t *testing.T) {
			v, err := LoadVariant(name)
			if err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for _, f := range goldenFrames(t, v) {
				fmt.Fprintf(&b, "%x %02x %x\t%s\t%s\n", f.Hdr, f.Cmd, f.Payload, v.FrameName(f), strings.ReplaceAll(v.Describe(f), "\n", " / "))
			}
			path := "testdata/golden-" + name + ".txt"
			if *update {
				if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -run TestGolden -update)", err)
			}
			if got := b.String(); got != string(want) {
				gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
				for i := 0; i < len(gl) && i < len(wl); i++ {
					if gl[i] != wl[i] {
						t.Errorf("line %d:\n got  %s\n want %s", i+1, gl[i], wl[i])
						break
					}
				}
				t.Errorf("%s differs (%d lines, was %d); run go test -run TestGolden -update and read the diff", path, len(gl), len(wl))
			}
		})
	}
}

// Every example in a message reads as it says.
func TestExamples(t *testing.T) {
	n := 0
	for _, name := range goldenVariants {
		v, err := LoadVariant(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range v.raw.Messages {
			for _, ex := range m.Examples {
				n++
				h, ok := messageHdr(v, m)
				pl, err := hex.DecodeString(strings.ReplaceAll(ex.Payload, " ", ""))
				if !ok || err != nil {
					t.Errorf("%s: %s: bad header or payload %q", name, m.Name, ex.Payload)
					continue
				}
				f := Frame{Hdr: h, Cmd: m.Cmd, Payload: pl}
				got := map[string]Field{}
				for _, fl := range v.Decode(f) {
					got[fl.Name] = fl
				}
				for fname, want := range ex.Expect {
					g, ok := got[fname]
					if !ok {
						t.Errorf("%s: %s %s: no field %q in %v", name, m.Name, ex.Payload, fname, v.Decode(f))
						continue
					}
					switch w := want.(type) {
					case string:
						if w == "n/a" {
							if g.Valid {
								t.Errorf("%s: %s %s: %s = %v, want n/a", name, m.Name, ex.Payload, fname, g)
							}
						} else if g.Text != w {
							t.Errorf("%s: %s %s: %s text %q, want %q", name, m.Name, ex.Payload, fname, g.Text, w)
						}
					default:
						x, ok := toFloat(w)
						val := float64(g.Value)
						if g.Float != nil {
							val = *g.Float
						}
						if !ok || !g.Valid || math.Abs(val-x) > 0.01 {
							t.Errorf("%s: %s %s: %s = %v, want %v", name, m.Name, ex.Payload, fname, g, w)
						}
					}
				}
			}
		}
	}
	if n == 0 {
		t.Error("no examples in any variant")
	}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}
