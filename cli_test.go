package micom

import (
	"bytes"
	"strings"
	"testing"
)

func TestMainUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		err  string
	}{
		{"no device", nil, 2, "-dev"},
		{"unknown flag", []string{"-nope"}, 2, "flag provided but not defined"},
		{"help", []string{"-h"}, 0, "-dev"},
	} {
		var out, errb bytes.Buffer
		if code := Main(tc.args, &out, &errb); code != tc.code {
			t.Errorf("%s: exit %d, want %d", tc.name, code, tc.code)
		}
		if !strings.Contains(errb.String(), tc.err) {
			t.Errorf("%s: stderr %q lacks %q", tc.name, errb.String(), tc.err)
		}
		if out.Len() != 0 {
			t.Errorf("%s: wrote %q to stdout", tc.name, out.String())
		}
	}
}

func TestMainNoSuchDevice(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"-dev", "/dev/does-not-exist"}, &out, &errb); code != 1 {
		t.Errorf("exit %d, want 1; stderr %q", code, errb.String())
	}
}
