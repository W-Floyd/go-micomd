package micom

import (
	"fmt"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// The variant file as written (docs/variants.md has the annotated schema).
// It is decoded strictly: an unknown key is an error, as is a repeated one.

type rawVariant struct {
	Schema   int                  `yaml:"schema"`
	Name     string               `yaml:"name"`
	Title    string               `yaml:"title,omitempty"`
	Extends  string               `yaml:"extends,omitempty"`
	Abstract bool                 `yaml:"abstract,omitempty"`
	Match    *rawMatch            `yaml:"match,omitempty"`
	Expect   map[string]int       `yaml:"expect,omitempty"`
	Identify *rawIdentify         `yaml:"identify,omitempty"`
	Headers  map[string]rawHeader `yaml:"headers,omitempty"`
	Keys     *rawKeys             `yaml:"keys,omitempty"`
	Signals  []rawSignal          `yaml:"signals,omitempty"`
	// Decode is refused, with where it went: it is now messages:.
	Decode   any                         `yaml:"decode,omitempty"`
	Enums    map[string]map[string]int64 `yaml:"enums,omitempty"`
	Messages []rawMessage                `yaml:"messages,omitempty"`
	Profile  []rawPeriodic               `yaml:"profile,omitempty"`
	Send     []rawSend                   `yaml:"send,omitempty"`
}

type rawMatch struct {
	Cmdline map[string]intList `yaml:"cmdline"`
	Status  string             `yaml:"status"`
	Ref     string             `yaml:"ref"`
}

type rawIdentify struct {
	Request *rawSend  `yaml:"request,omitempty"`
	Reply   *rawReply `yaml:"reply,omitempty"`
}

type rawReply struct {
	Name string `yaml:"name"`
	Hdr  string `yaml:"hdr"`
	Cmd  uint8  `yaml:"cmd"`
	// Identity names, by command-line identity key, the reply's fields that
	// carry the same codes, so the reply is matched like the command line.
	Identity map[string]string `yaml:"identity,omitempty"`
	Status   string            `yaml:"status,omitempty"`
	Ref      string            `yaml:"ref,omitempty"`
}

type rawHeader struct {
	Hdr    string `yaml:"hdr"`
	Dir    string `yaml:"dir"`
	Status string `yaml:"status,omitempty"`
	Ref    string `yaml:"ref,omitempty"`
}

type rawKeys struct {
	Frame *rawFrameRef `yaml:"frame,omitempty"`
	Push  []rawPushKey `yaml:"push,omitempty"`
	Knobs []rawKnob    `yaml:"knobs,omitempty"`
}

type rawFrameRef struct {
	Hdr string `yaml:"hdr"`
	Cmd uint8  `yaml:"cmd"`
}

type rawPushKey struct {
	Type   uint8   `yaml:"type"`
	Code   uint8   `yaml:"code"`
	Label  string  `yaml:"label,omitempty"`
	Key    keyCode `yaml:"key,omitempty"`
	Remove bool    `yaml:"remove,omitempty"`
}

type rawKnob struct {
	Type   uint8   `yaml:"type"`
	Code   uint8   `yaml:"code"`
	Label  string  `yaml:"label,omitempty"`
	Up     keyCode `yaml:"up,omitempty"`
	Down   keyCode `yaml:"down,omitempty"`
	Remove bool    `yaml:"remove,omitempty"`
}

type rawSignal struct {
	Name        string              `yaml:"name"`
	Hdr         string              `yaml:"hdr,omitempty"`
	Cmd         byteList            `yaml:"cmd,omitempty"`
	Len         *int                `yaml:"len,omitempty"`
	Label       string              `yaml:"label,omitempty"`
	Flag        *rawFlag            `yaml:"flag,omitempty"`
	Enum        map[uint8]string    `yaml:"enum,omitempty"`
	Number      *rawNumber          `yaml:"number,omitempty"`
	Fan         *rawFan             `yaml:"fan,omitempty"`
	Bits        *rawBits            `yaml:"bits,omitempty"`
	Burst       *rawBurst           `yaml:"burst,omitempty"`
	Values      map[uint8]valueSpec `yaml:"values,omitempty"`
	Lines       map[uint8]string    `yaml:"lines,omitempty"`
	Unknown     *string             `yaml:"unknown,omitempty"`
	UnknownLine string              `yaml:"unknown_line,omitempty"`
	Suffix      string              `yaml:"suffix,omitempty"`
	State       string              `yaml:"state,omitempty"`
	Status      string              `yaml:"status,omitempty"`
	Ref         string              `yaml:"ref,omitempty"`
	Note        string              `yaml:"note,omitempty"`
	Remove      bool                `yaml:"remove,omitempty"`
}

type rawFlag struct {
	On      uint8   `yaml:"on"`
	Off     flagOff `yaml:"off"`
	OnText  string  `yaml:"on_text,omitempty"`
	OffText string  `yaml:"off_text,omitempty"`
}

type rawNumber struct {
	Add int  `yaml:"add,omitempty"`
	Mul *int `yaml:"mul,omitempty"`
	// Div divides the result for the text only (format %g): the state keeps
	// the whole number, in 1/Div units.
	Div    *int    `yaml:"div,omitempty"`
	Range  []uint8 `yaml:"range,omitempty"`
	Format string  `yaml:"format"`
	// Wrap takes the result modulo 256, as byte arithmetic does.
	Wrap bool `yaml:"wrap,omitempty"`
}

type rawFan struct {
	Off    uint8   `yaml:"off"`
	Auto   uint8   `yaml:"auto"`
	Levels []uint8 `yaml:"levels"`
}

type rawBits struct {
	Always uint8            `yaml:"always,omitempty"`
	None   string           `yaml:"none"`
	Names  map[uint8]string `yaml:"names"`
}

type rawBurst struct {
	Begin uint8 `yaml:"begin"`
}

// rawGuard is a from_micom message's guard; len is refused there (it is
// length:), kept so the error says so.
type rawGuard struct {
	Len *int   `yaml:"len,omitempty"`
	Off *int   `yaml:"off,omitempty"`
	Eq  *uint8 `yaml:"eq,omitempty"`
}

type rawPeriodic struct {
	Name     string        `yaml:"name"`
	Every    time.Duration `yaml:"every,omitempty"`
	Frame    string        `yaml:"frame,omitempty"`
	Template string        `yaml:"template,omitempty"`
	OptIn    string        `yaml:"opt_in,omitempty"`
	Replaces string        `yaml:"replaces,omitempty"`
	Status   string        `yaml:"status,omitempty"`
	Ref      string        `yaml:"ref,omitempty"`
	Note     string        `yaml:"note,omitempty"`
	Remove   bool          `yaml:"remove,omitempty"`
}

type rawSend struct {
	Name      string `yaml:"name"`
	Frame     string `yaml:"frame,omitempty"`
	Pattern   string `yaml:"pattern,omitempty"`
	Status    string `yaml:"status,omitempty"`
	Ref       string `yaml:"ref,omitempty"`
	Note      string `yaml:"note,omitempty"`
	Dangerous string `yaml:"dangerous,omitempty"`
	Remove    bool   `yaml:"remove,omitempty"`
}

// parseVariant decodes one file.
func parseVariant(data []byte) (rawVariant, error) {
	var rv rawVariant
	if err := yaml.Load(data, &rv, yaml.WithKnownFields(), yaml.WithUniqueKeys()); err != nil {
		return rawVariant{}, err
	}
	return rv, nil
}

// byteList is a command: one byte, or a list of them.
type byteList []uint8

func (b *byteList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		var l []uint8
		if err := n.Load(&l, yaml.WithKnownFields()); err != nil {
			return err
		}
		*b = l
		return nil
	}
	var v uint8
	if err := n.Load(&v); err != nil {
		return err
	}
	*b = byteList{v}
	return nil
}

func (b byteList) MarshalYAML() (any, error) {
	if len(b) == 1 {
		return b[0], nil
	}
	return []uint8(b), nil
}

// intList is a match value: one number, or a list of them (any of).
type intList []int

func (l *intList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		var v []int
		if err := n.Load(&v); err != nil {
			return err
		}
		*l = v
		return nil
	}
	var v int
	if err := n.Load(&v); err != nil {
		return err
	}
	*l = intList{v}
	return nil
}

func (l intList) MarshalYAML() (any, error) {
	if len(l) == 1 {
		return l[0], nil
	}
	return []int(l), nil
}

// flagOff is a flag's off value: a byte, or "*" for any value but on.
type flagOff struct {
	any bool
	v   uint8
}

func (f *flagOff) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Value == "*" {
		*f = flagOff{any: true}
		return nil
	}
	var v uint8
	if err := n.Load(&v); err != nil {
		return err
	}
	*f = flagOff{v: v}
	return nil
}

func (f flagOff) MarshalYAML() (any, error) {
	if f.any {
		return "*", nil
	}
	return f.v, nil
}

// valueSpec is a special value of a signal: its text, and for a number, the
// state it stands for.
type valueSpec struct {
	Text  string `yaml:"text"`
	State *int   `yaml:"state,omitempty"`
}

func (v *valueSpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*v = valueSpec{Text: n.Value}
		return nil
	}
	type plain valueSpec
	return n.Load((*plain)(v), yaml.WithKnownFields())
}

func (v valueSpec) MarshalYAML() (any, error) {
	if v.State == nil {
		return v.Text, nil
	}
	type plain valueSpec
	return plain(v), nil
}

// keyCode is a Linux key: a KEY_* name micomd knows (keyNames), or its number.
type keyCode uint16

func (k *keyCode) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && strings.HasPrefix(n.Value, "KEY_") {
		c, ok := keyNames[n.Value]
		if !ok {
			return fmt.Errorf("line %d: unknown key name %s", n.Line, n.Value)
		}
		*k = keyCode(c)
		return nil
	}
	var v uint16
	if err := n.Load(&v); err != nil {
		return err
	}
	*k = keyCode(v)
	return nil
}
