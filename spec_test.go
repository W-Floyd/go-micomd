package micom

import (
	"strings"
	"testing"
)

func TestSpecMarkdown(t *testing.T) {
	for _, name := range VariantNames() {
		got, err := SpecMarkdown(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The pre-commit hook regenerates docs/spec: the same variant must
		// render the same bytes, or every commit churns them.
		for range 20 {
			if again, _ := SpecMarkdown(name); again != got {
				t.Fatalf("%s: renders differently each time", name)
			}
		}
		for _, want := range []string{"# ", "## Headers", "| --- |"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: no %q", name, want)
			}
		}
	}
	got, _ := SpecMarkdown("kia-ev6-gen5w")
	if !strings.Contains(got, "| Speed limit | byte 14 | mph |") {
		t.Error("the merged EV6 spec lacks the speed limit row")
	}
}
