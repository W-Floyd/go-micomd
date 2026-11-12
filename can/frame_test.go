package can

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // String() of the parsed frame; "" = error
	}{
		{"can0 383#5555555555555555", "can0 383#5555555555555555"},
		{"can2:7df#0210", "can2 7DF#0210"},
		{"(1700000000.123456) can1 102#", "can1 102#"},
		{"can0 18DAF110#01", "can0 18DAF110#01"},
		{"can3 123#00", ""},
		{"can0 800#00", ""},
		{"can0 123#000000000000000000", ""},
		{"can0 123", ""},
		{"vcan0 123#00", ""},
	} {
		f, err := Parse(tc.in)
		if got := f.String(); (err != nil) != (tc.want == "") || (err == nil && got != tc.want) {
			t.Errorf("Parse(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			continue
		}
		if err == nil {
			if g, err := Parse(f.String()); err != nil || g.String() != f.String() {
				t.Errorf("round trip of %q: %q, %v", tc.in, g.String(), err)
			}
		}
	}
}
