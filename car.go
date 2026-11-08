package micom

import "sync"

// The front panel, climate, seat, door, window and rear camera frames are a
// car's, so they are read by its variant (variants/*.yaml); the EV6's were
// read off live captures of the car (every value seen by pressing the
// control, none from a specification).

var defaultV struct {
	once sync.Once
	v    *Variant
	err  error
}

// DefaultVariant is the embedded DefaultVariantName, loaded once: what the
// package-level Describe, Decode, CarClimate.Apply and DefaultProfile read.
func DefaultVariant() (*Variant, error) {
	defaultV.once.Do(func() {
		defaultV.v, defaultV.err = LoadVariant(DefaultVariantName)
	})
	return defaultV.v, defaultV.err
}

// defaultVariant never fails: without the default variant (a broken build;
// the tests load it) it is one that knows no frames.
func defaultVariant() *Variant {
	if v, err := DefaultVariant(); err == nil {
		return v
	}
	return emptyVariant
}

// emptyVariant knows no frames and sends nothing of its own: the keepalive,
// which is code, is all that is left.
var emptyVariant = &Variant{Name: "none", Title: "no variant: the keepalive only"}

// carDescribe reads a key or state frame into a line of text, "" for a frame
// it does not know.
func carDescribe(f Frame) string { return defaultVariant().describe(f) }
