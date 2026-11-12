package can

import (
	"fmt"
	"strings"
	"testing"
)

const testDBC = `VERSION ""

BO_ 899 FATC11: 8 DATC
 SG_ Intel8 : 12|8@1+ (1,0) [0|255] ""  X
 SG_ Moto16 : 7|16@0+ (1,0) [0|65535] ""  X
 SG_ Temp : 32|8@1+ (0.5,-40) [-40|60] "deg"  X
 SG_ Signed : 40|8@1- (1,0) [-128|127] ""  X
 SG_ Mode : 48|2@1+ (1,0) [0|3] ""  X

BO_ 1088 MUXED: 8 X
 SG_ Sel M : 0|8@1+ (1,0) [0|255] ""  X
 SG_ A m0 : 8|8@1+ (1,0) [0|255] ""  X
 SG_ B m1 : 8|8@1+ (1,0) [0|255] ""  X

BO_ 2566844672 EXT: 2 X
 SG_ MotoNib : 3|8@0+ (1,0) [0|255] ""  X

VAL_ 899 Mode 0 "off" 1 "auto" 2 "manual" ;
`

// TestDBC encodes named frames and decodes them back: every row's frame must
// have the given bytes, and Describe must give the values back.
func TestDBC(t *testing.T) {
	d, err := ParseDBC(strings.NewReader(testDBC))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		in   string
		want string // Frame.String(); "" = error
		desc string // in Describe's output
	}{
		{"FATC11 Moto16=4660", "can0 383#1234000000000000", "Moto16=4660"},
		{"FATC11 Intel8=205", "can0 383#00D00C0000000000", "Intel8=205"},
		{"can2 FATC11 Temp=21.5", "can2 383#000000007B000000", "Temp=21.5"},
		{"FATC11 Signed=-2", "can0 383#0000000000FE0000", "Signed=-2"},
		{"FATC11 Mode=manual", "can0 383#0000000000000200", `Mode="manual"`},
		{"FATC11 Mode=1", "can0 383#0000000000000100", `Mode="auto"`},
		{"MUXED Sel=1 B=7", "can0 440#0107000000000000", "Sel=1 B=7"},
		{"EXT MotoNib=171", "can0 18FEF100#0AB0", "MotoNib=171"},
		{"FATC11 Mode=4", "", ""},
		{"FATC11 Mode=sport", "", ""},
		{"FATC11 Nope=1", "", ""},
		{"NOPE", "", ""},
		{"can3 FATC11", "", ""},
	} {
		f, err := d.ParseSignals(tc.in)
		if (err != nil) != (tc.want == "") || (err == nil && f.String() != tc.want) {
			t.Errorf("%q: got %v, %v; want %q", tc.in, f, err, tc.want)
			continue
		}
		if err != nil {
			continue
		}
		if got := d.Describe(f); !strings.Contains(got, tc.desc) {
			t.Errorf("%q: Describe = %q, want it to contain %q", tc.in, got, tc.desc)
		}
		if strings.HasPrefix(tc.in, "MUXED") && strings.Contains(d.Describe(f), " A=") {
			t.Errorf("%q: Describe shows the other multiplexer value's signal: %q", tc.in, d.Describe(f))
		}
	}
	if d.Describe(Frame{ID: 0x123}) != "" {
		t.Error("Describe of an unknown ID is not empty")
	}
}

// TestDBCAdd: a later file's message replaces an earlier one with its ID.
func TestDBCAdd(t *testing.T) {
	a, _ := ParseDBC(strings.NewReader(testDBC))
	b, _ := ParseDBC(strings.NewReader("BO_ 899 OTHER: 1 X\n SG_ S : 0|8@1+ (1,0) [0|255] \"\" X\n"))
	a.Add(b)
	if m := a.ByID(0x383); m == nil || m.Name != "OTHER" || a.ByName("OTHER") != m || a.ByName("FATC11") != nil {
		t.Errorf("after Add, 0x383 is %v", m)
	}
	if n := len(a.Messages); n != 3 {
		t.Errorf("%d messages, want 3: %v", n, fmt.Sprint(a.Messages))
	}
}
