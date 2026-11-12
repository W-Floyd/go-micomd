package micom

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// The embedded variants: supporting a new car is adding a file here.
//
//go:embed variants/*.yaml
var variantFS embed.FS

// Variant is everything car-specific about a head unit's micom link, loaded
// from a variant file (docs/variants.md) and checked: the frames it names,
// what it may send, how its keys and state frames read. The link itself
// (framing, checksum, the CPU-alive exchange) is code and never depends on it.
type Variant struct {
	// Name is the file's name, Title its one-line description, Extends its
	// parent ("" for a root).
	Name, Title, Extends string
	// Abstract variants are family layers: never chosen by detection.
	Abstract bool
	// Source is "embedded" or the path it was read from.
	Source string

	raw    rawVariant // resolved, for -show-variant
	match  map[string][]int
	expect map[string]int

	keyFrame   hdrCmd
	hasKeys    bool
	pushLabels map[[2]byte]string
	knobLabels map[byte]string
	keys       keymap
	keyList    []uint16

	signals  map[hdrCmd]*sigSpec
	decoders map[byte][]*decoder // by command
	// rxHeaders are the from_micom headers, by name: an encoded frame's
	// open header bytes are taken from the first that fits (emulate.go).
	rxHeaders [][4]byte
	names     map[hdrCmd]string
	messages  map[string]*msgSpec // to_micom, by name
	sent      map[hdrCmd]*msgSpec // to_micom, by frame
	required  map[string]bool     // from_micom fields a to_micom message requires
	// dangerous frames (hdr+cmd) reset/rewrite/reconfigure the micom; sent
	// only with -allow-dangerous. Value is the defining entry's name, for
	// the refusal message. Built from messages/sends marked dangerous.
	dangerous map[hdrCmd]string

	profile  []periodicSpec
	sends    []sendSpec
	identify *sendSpec
	reply    *hdrCmd // the micom's variant record, kept for Snapshot
	// replyIdentity names the record's fields by identity key.
	replyIdentity map[string]string
}

type hdrCmd struct {
	hdr [4]byte
	cmd byte
}

type decoder struct {
	name      string
	hdr, mask [4]byte
	minLen    int
	guards    []guard
	fields    []*field
}

type guard struct {
	length, off int
	hasLen      bool
	eq          byte
}

type periodicSpec struct {
	name     string
	every    time.Duration
	optIn    string
	replaces string
	hdr      [4]byte
	cmd      byte
	slots    []slot
}

// slot is one payload byte of a profile frame: fixed, or a clock field.
type slot struct {
	fixed byte
	field string
}

type sendSpec struct {
	name      string
	status    string
	frame     *Frame
	pattern   *Pattern
	dangerous string // non-empty = dangerous; the reason
}

// DefaultVariantName is the car micomd was written for, and what it runs as
// when nothing better is known.
const DefaultVariantName = "kia-ev6-gen5w"

const (
	variantSchema = 1
	// maxExtends bounds a chain of parents.
	maxExtends = 4
)

// validName is [a-z0-9][a-z0-9-]*: a variant name is also a file name.
func validName(s string) bool {
	for i, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0) {
			return false
		}
	}
	return s != ""
}

var (
	statuses = map[string]bool{"captured": true, "code": true, "inferred": true}
	// clockFields are a profile template's placeholders.
	clockFields = map[string]func(time.Time) byte{
		"second": func(t time.Time) byte { return byte(t.Second()) },
		"minute": func(t time.Time) byte { return byte(t.Minute()) },
		"hour":   func(t time.Time) byte { return byte(t.Hour()) },
		"day":    func(t time.Time) byte { return byte(t.Day()) },
		"month":  func(t time.Time) byte { return byte(t.Month()) },
		"year2":  func(t time.Time) byte { return byte(t.Year() % 100) },
		// HEVService's clock (CAN DB generation 2): the weekday as one bit,
		// Sunday bit 0 then Saturday bit 1 down to Monday bit 6, and the
		// hour in bits 7:3.
		"weekday_bit": func(t time.Time) byte { return 1 << ((7 - int(t.Weekday())) % 7) },
		"hour_shl3":   func(t time.Time) byte { return byte(t.Hour() << 3) },
	}
	expectKeys = map[string]bool{"amp_type": true, "climate_variant": true}
)

// VariantNames lists the embedded variants.
func VariantNames() []string {
	ents, _ := fs.ReadDir(variantFS, "variants")
	var out []string
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".yaml"); ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// LoadVariant loads an embedded variant by name, with its parents.
func LoadVariant(name string) (*Variant, error) {
	rv, err := embeddedRaw(name)
	if err != nil {
		return nil, err
	}
	return build(rv, "embedded")
}

// LoadVariantFile loads a variant file from disk. It may extend an embedded
// variant; it is never chosen by detection.
func LoadVariantFile(path string) (*Variant, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return loadVariantData(data, path)
}

func loadVariantData(data []byte, source string) (*Variant, error) {
	rv, err := parseVariant(data)
	if err != nil {
		return nil, fmt.Errorf("variant %s: %w", source, err)
	}
	return build(rv, source)
}

func build(rv rawVariant, source string) (*Variant, error) {
	res, err := resolve(rv)
	if err != nil {
		return nil, err
	}
	v, err := compile(res)
	if err != nil {
		return nil, err
	}
	v.Source = source
	return v, nil
}

func embeddedRaw(name string) (rawVariant, error) {
	if !validName(name) {
		return rawVariant{}, fmt.Errorf("variant %q: not a variant name", name)
	}
	data, err := variantFS.ReadFile("variants/" + name + ".yaml")
	if err != nil {
		return rawVariant{}, fmt.Errorf("variant %s: no such embedded variant (have %s)", name, strings.Join(VariantNames(), ", "))
	}
	rv, err := parseVariant(data)
	if err != nil {
		return rawVariant{}, fmt.Errorf("variant %s: %w", name, err)
	}
	if rv.Name != name {
		return rawVariant{}, fmt.Errorf("variant %s: the file names itself %q", name, rv.Name)
	}
	return rv, nil
}

// duplicates finds a name used twice in one file's list, which merging would
// otherwise fold into one entry.
func duplicates(rv rawVariant) error {
	var errs []error
	check := func(list string, names []string) {
		seen := map[string]bool{}
		for _, n := range names {
			if seen[n] {
				errs = append(errs, fmt.Errorf("variant %s: %s[%s]: a name is required, once", rv.Name, list, n))
			}
			seen[n] = true
		}
	}
	check("signals", namesOf(rv.Signals, func(x rawSignal) string { return x.Name }))
	if rv.Decode != nil {
		errs = append(errs, fmt.Errorf("variant %s: decode: moved to messages: (dir: from_micom), docs/messages.md", rv.Name))
	}
	check("messages", namesOf(rv.Messages, func(x rawMessage) string { return x.Name }))
	check("profile", namesOf(rv.Profile, func(x rawPeriodic) string { return x.Name }))
	check("send", namesOf(rv.Send, func(x rawSend) string { return x.Name }))
	if rv.Keys != nil {
		check("keys.push", namesOf(rv.Keys.Push, func(x rawPushKey) string { return fmt.Sprintf("%d.%d", x.Type, x.Code) }))
		check("keys.knobs", namesOf(rv.Keys.Knobs, func(x rawKnob) string { return fmt.Sprintf("%d.%d", x.Type, x.Code) }))
	}
	return errors.Join(errs...)
}

func namesOf[T any](l []T, name func(T) string) []string {
	out := make([]string, len(l))
	for i, x := range l {
		out[i] = name(x)
	}
	return out
}

// resolve merges rv's chain of embedded parents into it, root first.
func resolve(rv rawVariant) (rawVariant, error) {
	if err := duplicates(rv); err != nil {
		return rawVariant{}, err
	}
	chain := []rawVariant{rv}
	seen := map[string]bool{rv.Name: true}
	for cur := rv; cur.Extends != ""; {
		if len(chain) > maxExtends {
			return rawVariant{}, fmt.Errorf("variant %s: more than %d parents", rv.Name, maxExtends)
		}
		if seen[cur.Extends] {
			return rawVariant{}, fmt.Errorf("variant %s: extends %s again (a cycle)", rv.Name, cur.Extends)
		}
		p, err := embeddedRaw(cur.Extends)
		if err != nil {
			return rawVariant{}, fmt.Errorf("variant %s: extends: %w", rv.Name, err)
		}
		if err := duplicates(p); err != nil {
			return rawVariant{}, err
		}
		seen[p.Name] = true
		chain = append(chain, p)
		cur = p
	}
	out := chain[len(chain)-1]
	for i := len(chain) - 2; i >= 0; i-- {
		out = mergeVariant(out, chain[i])
	}
	return out, nil
}

// mergeVariant is child c over parent p: scalars override, maps merge by key,
// named lists by name (remove: true deletes); match and abstract are the
// child's own.
func mergeVariant(p, c rawVariant) rawVariant {
	out := p
	out.Schema, out.Name, out.Extends = c.Schema, c.Name, c.Extends
	if c.Title != "" {
		out.Title = c.Title
	}
	out.Abstract, out.Match = c.Abstract, c.Match
	out.Expect = mergeMap(p.Expect, c.Expect)
	if c.Identify != nil {
		id := rawIdentify{}
		if p.Identify != nil {
			id = *p.Identify
		}
		if c.Identify.Request != nil {
			id.Request = c.Identify.Request
		}
		if c.Identify.Reply != nil {
			id.Reply = c.Identify.Reply
		}
		out.Identify = &id
	}
	out.Headers = mergeMap(p.Headers, c.Headers)
	if c.Keys != nil {
		k := rawKeys{}
		if p.Keys != nil {
			k = *p.Keys
		}
		if c.Keys.Frame != nil {
			k.Frame = c.Keys.Frame
		}
		k.Push = mergeNamed(k.Push, c.Keys.Push, func(x rawPushKey) (string, bool) { return fmt.Sprintf("%d.%d", x.Type, x.Code), x.Remove })
		k.Knobs = mergeNamed(k.Knobs, c.Keys.Knobs, func(x rawKnob) (string, bool) { return fmt.Sprintf("%d.%d", x.Type, x.Code), x.Remove })
		out.Keys = &k
	}
	out.Signals = mergeNamed(p.Signals, c.Signals, func(x rawSignal) (string, bool) { return x.Name, x.Remove })
	out.Enums = mergeMap(p.Enums, c.Enums)
	out.Messages = mergeNamed(p.Messages, c.Messages, func(x rawMessage) (string, bool) { return x.Name, x.Remove })
	out.Profile = mergeNamed(p.Profile, c.Profile, func(x rawPeriodic) (string, bool) { return x.Name, x.Remove })
	out.Send = mergeNamed(p.Send, c.Send, func(x rawSend) (string, bool) { return x.Name, x.Remove })
	return out
}

func mergeMap[K comparable, V any](p, c map[K]V) map[K]V {
	if len(p) == 0 && len(c) == 0 {
		return nil
	}
	out := make(map[K]V, len(p)+len(c))
	for k, v := range p {
		out[k] = v
	}
	for k, v := range c {
		out[k] = v
	}
	return out
}

// mergeNamed replaces a parent's entry by the child's of the same name, drops
// it for remove: true, and appends the child's new ones. A remove: true that
// matches nothing is kept, for compile to refuse.
func mergeNamed[T any](p, c []T, key func(T) (string, bool)) []T {
	out := slices.Clone(p)
	for _, x := range c {
		k, rm := key(x)
		i := slices.IndexFunc(out, func(y T) bool { yk, _ := key(y); return yk == k })
		switch {
		case i >= 0 && rm:
			out = slices.Delete(out, i, i+1)
		case i >= 0:
			out[i] = x
		default:
			out = append(out, x)
		}
	}
	return out
}

// MatchVariant names the embedded variant whose match rule fits id best (the
// one with the most keys, all matching), "" when none does. Two equally good
// matches are an error.
func MatchVariant(id Identity) (string, error) {
	best, score, tie := "", 0, ""
	for _, n := range VariantNames() {
		rv, err := embeddedRaw(n)
		if err != nil || rv.Abstract || rv.Match == nil {
			continue
		}
		if !matches(rv.Match.Cmdline, id) {
			continue
		}
		switch s := len(rv.Match.Cmdline); {
		case s > score:
			best, score, tie = n, s, ""
		case s == score:
			tie = n
		}
	}
	if tie != "" {
		return "", fmt.Errorf("variant: %s and %s both match %v: choose one with -variant", best, tie, id)
	}
	return best, nil
}

// descends is whether the embedded variant name is family or extends it.
func descends(name, family string) bool {
	for range maxExtends + 1 {
		if name == family {
			return true
		}
		rv, err := embeddedRaw(name)
		if err != nil || rv.Extends == "" {
			return false
		}
		name = rv.Extends
	}
	return false
}

func matches(rule map[string]intList, id Identity) bool {
	if len(rule) == 0 {
		return false
	}
	for k, vs := range rule {
		v, ok := id[k]
		if !ok || !slices.Contains(vs, v) {
			return false
		}
	}
	return true
}

// ambiguousMatches is the pairs of embedded variants some command line could
// match equally well.
func ambiguousMatches() []string {
	type rule struct {
		name string
		m    map[string]intList
	}
	var rs []rule
	for _, n := range VariantNames() {
		rv, err := embeddedRaw(n)
		if err == nil && !rv.Abstract && rv.Match != nil && len(rv.Match.Cmdline) > 0 {
			rs = append(rs, rule{n, rv.Match.Cmdline})
		}
	}
	var out []string
	for i := range rs {
		for j := i + 1; j < len(rs); j++ {
			a, b := rs[i].m, rs[j].m
			if len(a) != len(b) {
				continue
			}
			overlap := true
			for k, va := range a {
				if vb, ok := b[k]; ok && !slices.ContainsFunc(va, func(x int) bool { return slices.Contains(vb, x) }) {
					overlap = false
				}
			}
			if overlap {
				out = append(out, rs[i].name+" / "+rs[j].name)
			}
		}
	}
	return out
}

// YAML is the variant as resolved (its parents merged in).
func (v *Variant) YAML() ([]byte, error) {
	return yaml.Dump(v.raw, yaml.WithIndent(2))
}

// Expect is what the micom is expected to report about this car (amp_type,
// climate_variant): a check, never a switch.
func (v *Variant) Expect() map[string]int { return v.expect }

// Match is the variant's kernel command line rule.
func (v *Variant) Match() map[string][]int { return v.match }

// checker collects a variant's problems, each with where it is.
type checker struct {
	name string
	errs []error
}

func (c *checker) errf(where, format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf("variant %s: %s: %s", c.name, where, fmt.Sprintf(format, args...)))
}

func (c *checker) err() error { return errors.Join(c.errs...) }

type headerDef struct {
	hdr [4]byte
	dir string
}

// compile checks a resolved variant and builds its lookup tables.
func compile(rv rawVariant) (*Variant, error) {
	c := &checker{name: rv.Name}
	v := &Variant{
		Name: rv.Name, Title: rv.Title, Extends: rv.Extends, Abstract: rv.Abstract, raw: rv,
		pushLabels: map[[2]byte]string{}, knobLabels: map[byte]string{},
		keys:    keymap{push: map[keyKey]uint16{}, knob: map[knobKey]uint16{}},
		signals: map[hdrCmd]*sigSpec{}, decoders: map[byte][]*decoder{}, names: map[hdrCmd]string{},
		messages: map[string]*msgSpec{}, sent: map[hdrCmd]*msgSpec{}, required: map[string]bool{},
		dangerous: map[hdrCmd]string{},
	}
	if rv.Schema != variantSchema {
		c.errf("schema", "is %d, want %d", rv.Schema, variantSchema)
	}
	if !validName(rv.Name) {
		c.errf("name", "%q is not [a-z0-9-]", rv.Name)
	}

	headers := map[string]headerDef{}
	for _, n := range sortedKeys(rv.Headers) {
		h := rv.Headers[n]
		b, err := parseHdr(h.Hdr)
		if err != nil {
			c.errf("headers["+n+"]", "%v", err)
			continue
		}
		if h.Dir != "from_micom" && h.Dir != "to_micom" {
			c.errf("headers["+n+"]", "dir is from_micom or to_micom, not %q", h.Dir)
		}
		checkStatus(c, "headers["+n+"]", h.Status, false)
		headers[n] = headerDef{b, h.Dir}
		if h.Dir == "from_micom" {
			v.rxHeaders = append(v.rxHeaders, b)
		}
	}
	hdrOf := func(where, s string) ([4]byte, bool) {
		if h, ok := headers[s]; ok {
			return h.hdr, true
		}
		b, err := parseHdr(s)
		if err != nil {
			c.errf(where, "hdr %q is neither a header name nor 4 hex bytes", s)
			return b, false
		}
		return b, true
	}
	// sendable refuses what a variant may never put on the wire. A dangerous
	// frame is allowed here (it is defined and documented like any other);
	// it is gated at send time by -allow-dangerous instead.
	sendable := func(where string, hdr [4]byte, cmd byte) {
		if (hdr == hdrAliveReply || hdr == hdrAliveReq) && cmd == cmdAlive {
			c.errf(where, "the CPU-alive exchange is micomd's own, not a variant's")
			return
		}
		for _, h := range headers {
			if h.hdr == hdr && h.dir == "to_micom" {
				return
			}
		}
		c.errf(where, "header %x is not a to_micom header of this variant", hdr)
	}

	if m := rv.Match; m != nil {
		checkStatus(c, "match", m.Status, true)
		if m.Ref == "" {
			c.errf("match", "ref is required")
		}
		if len(m.Cmdline) == 0 {
			c.errf("match", "cmdline is empty")
		}
		v.match = map[string][]int{}
		for k, vs := range m.Cmdline {
			if !isIdentityKey(k) {
				c.errf("match.cmdline", "unknown key %q (have %s)", k, strings.Join(identityKeys, ", "))
			}
			if len(vs) == 0 {
				c.errf("match.cmdline."+k, "no value")
			}
			v.match[k] = vs
		}
	}
	for k, x := range rv.Expect {
		if !expectKeys[k] {
			c.errf("expect", "unknown key %q", k)
		}
		if v.expect == nil {
			v.expect = map[string]int{}
		}
		v.expect[k] = x
	}

	if id := rv.Identify; id != nil {
		if r := id.Request; r != nil {
			if s, ok := compileSend(c, "identify.request", *r, sendable); ok {
				if s.frame == nil {
					c.errf("identify.request", "is a whole frame, not a pattern")
				}
				v.identify = &s
			}
		}
		if r := id.Reply; r != nil {
			if hdr, ok := hdrOf("identify.reply", r.Hdr); ok {
				if r.Name == "" {
					c.errf("identify.reply", "name is required")
				}
				v.names[hdrCmd{hdr, r.Cmd}] = r.Name
				v.reply = &hdrCmd{hdr, r.Cmd}
			}
			checkStatus(c, "identify.reply", r.Status, false)
			for k := range r.Identity {
				if !isIdentityKey(k) {
					c.errf("identify.reply.identity", "%q is not an identity key (%s)", k, strings.Join(identityKeys, ", "))
				}
			}
			v.replyIdentity = r.Identity
		}
	}

	if k := rv.Keys; k != nil {
		compileKeys(c, v, *k, hdrOf)
	}
	compileSignals(c, v, rv.Signals, hdrOf)
	compileMessages(c, v, rv.Messages, compileEnums(c, rv.Enums), hdrOf, sendable)
	if v.reply != nil {
		for _, k := range slices.Sorted(maps.Keys(v.replyIdentity)) {
			if !v.decodesField(*v.reply, v.replyIdentity[k]) {
				c.errf("identify.reply.identity", "%s: no from_micom field %q decodes the reply", k, v.replyIdentity[k])
			}
		}
	}

	seen := map[string]bool{}
	for _, p := range rv.Profile {
		where := "profile[" + p.Name + "]"
		if p.Name == "" || seen[p.Name] {
			c.errf(where, "a name is required, once")
		}
		seen[p.Name] = true
		if p.Remove {
			c.errf(where, "remove: nothing inherited by that name")
			continue
		}
		// A profile frame is sent unasked, so it must be captured. One known
		// only from stock code is sent only with -code-profile, in place of
		// the captured frame it corrects.
		switch {
		case p.OptIn == "code" && p.Status != "code":
			c.errf(where, "opt_in code is for status code, not %q", p.Status)
		case p.OptIn != "code" && p.Status != "captured":
			c.errf(where, "status is %q: a profile frame is sent unasked, so it must be captured (or code, with opt_in code)", p.Status)
		}
		if p.Ref == "" {
			c.errf(where, "ref is required")
		}
		if p.Every <= 0 {
			c.errf(where, "every must be positive")
		}
		switch p.OptIn {
		case "", "clock":
			if p.Replaces != "" {
				c.errf(where, "replaces is only for opt_in code")
			}
		case "code":
			if p.Replaces == "" {
				c.errf(where, "opt_in code needs replaces: the captured entry it stands in for")
			}
		default:
			c.errf(where, "opt_in is clock, code or nothing, not %q", p.OptIn)
		}
		ps := periodicSpec{name: p.Name, every: p.Every, optIn: p.OptIn, replaces: p.Replaces}
		switch {
		case (p.Frame == "") == (p.Template == ""):
			c.errf(where, "exactly one of frame and template")
			continue
		case p.Frame != "":
			f, err := ParseHexFrame(p.Frame)
			if err != nil {
				c.errf(where, "%v", err)
				continue
			}
			ps.hdr, ps.cmd = f.Hdr, f.Cmd
			for _, b := range f.Payload {
				ps.slots = append(ps.slots, slot{fixed: b})
			}
		default:
			var err error
			ps.hdr, ps.cmd, ps.slots, err = parseTemplate(p.Template)
			if err != nil {
				c.errf(where, "%v", err)
				continue
			}
		}
		sendable(where, ps.hdr, ps.cmd)
		v.profile = append(v.profile, ps)
	}
	for _, p := range v.profile {
		if p.replaces == "" {
			continue
		}
		i := slices.IndexFunc(v.profile, func(o periodicSpec) bool { return o.name == p.replaces })
		switch {
		case i < 0 || v.profile[i].optIn != "":
			c.errf("profile["+p.name+"]", "replaces %q: no routine entry by that name", p.replaces)
		case v.profile[i].hdr != p.hdr || v.profile[i].cmd != p.cmd:
			c.errf("profile["+p.name+"]", "replaces %q: a different frame (header and command must match)", p.replaces)
		}
	}

	checkMessageRefs(c, v)

	seen = map[string]bool{}
	for _, s := range rv.Send {
		where := "send[" + s.Name + "]"
		if s.Name == "" || seen[s.Name] {
			c.errf(where, "a name is required, once")
		}
		seen[s.Name] = true
		if s.Remove {
			c.errf(where, "remove: nothing inherited by that name")
			continue
		}
		if sp, ok := compileSend(c, where, s, sendable); ok {
			v.sends = append(v.sends, sp)
			if sp.dangerous != "" {
				if sp.frame != nil {
					v.dangerous[hdrCmd{sp.frame.Hdr, sp.frame.Cmd}] = sp.dangerous
				} else if sp.pattern != nil {
					v.dangerous[hdrCmd{sp.pattern.Hdr, sp.pattern.Cmd}] = sp.dangerous
				}
			}
		}
	}
	// A dangerous frame may be sent only when asked (with -allow-dangerous),
	// never put in the profile, which goes out unasked.
	for _, p := range v.profile {
		if why := v.dangerousReason(p.hdr, p.cmd); why != "" {
			c.errf("profile["+p.name+"]", "dangerous (%s): a profile frame is sent unasked, so it may not be dangerous", why)
		}
	}
	if err := c.err(); err != nil {
		return nil, err
	}
	return v, nil
}

func compileSend(c *checker, where string, s rawSend, sendable func(string, [4]byte, byte)) (sendSpec, bool) {
	checkStatus(c, where, s.Status, true)
	if s.Ref == "" {
		c.errf(where, "ref is required")
	}
	sp := sendSpec{name: s.Name, status: s.Status, dangerous: s.Dangerous}
	switch {
	case (s.Frame == "") == (s.Pattern == ""):
		c.errf(where, "exactly one of frame and pattern")
		return sp, false
	case s.Frame != "":
		f, err := ParseHexFrame(s.Frame)
		if err != nil {
			c.errf(where, "%v", err)
			return sp, false
		}
		sp.frame = &f
		sendable(where, f.Hdr, f.Cmd)
	default:
		p, err := ParsePattern(s.Pattern)
		if err != nil {
			c.errf(where, "%v", err)
			return sp, false
		}
		sp.pattern = &p
		sendable(where, p.Hdr, p.Cmd)
	}
	return sp, true
}

// dangerousReason names the entry that makes hdr+cmd dangerous, "" if it is
// not. A dangerous frame resets, rewrites or reconfigures the micom and is
// sent only with -allow-dangerous.
func (v *Variant) dangerousReason(hdr [4]byte, cmd byte) string {
	if v == nil {
		return ""
	}
	return v.dangerous[hdrCmd{hdr, cmd}]
}

func checkStatus(c *checker, where, s string, required bool) {
	switch {
	case s == "" && required:
		c.errf(where, "status is required (captured, code or inferred)")
	case s != "" && !statuses[s]:
		c.errf(where, "status %q is not captured, code or inferred", s)
	}
}

func compileKeys(c *checker, v *Variant, k rawKeys, hdrOf func(string, string) ([4]byte, bool)) {
	if k.Frame != nil {
		if hdr, ok := hdrOf("keys.frame", k.Frame.Hdr); ok {
			v.keyFrame, v.hasKeys = hdrCmd{hdr, k.Frame.Cmd}, true
		}
	}
	for _, p := range k.Push {
		where := fmt.Sprintf("keys.push[%d.%d]", p.Type, p.Code)
		switch {
		case p.Remove:
			c.errf(where, "remove: nothing inherited")
			continue
		case p.Type > TypeEject || p.Code > 15:
			c.errf(where, "a push key is type 0-7, code 0-15")
			continue
		}
		v.pushLabels[[2]byte{p.Type, p.Code}] = p.Label
		if p.Key != 0 {
			v.keys.push[keyKey{int(p.Type), int(p.Code)}] = uint16(p.Key)
			v.keyList = append(v.keyList, uint16(p.Key))
		}
	}
	for _, n := range k.Knobs {
		where := fmt.Sprintf("keys.knobs[%d.%d]", n.Type, n.Code)
		switch {
		case n.Remove:
			c.errf(where, "remove: nothing inherited")
			continue
		case n.Type < 8 || n.Type > 11 || n.Code > 15:
			c.errf(where, "a knob is type 8-11, code 0-15")
			continue
		}
		if _, ok := v.knobLabels[n.Type]; !ok {
			v.knobLabels[n.Type] = n.Label
		}
		for _, d := range []struct {
			up bool
			k  keyCode
		}{{true, n.Up}, {false, n.Down}} {
			if d.k != 0 {
				v.keys.knob[knobKey{int(n.Type), int(n.Code), d.up}] = uint16(d.k)
				v.keyList = append(v.keyList, uint16(d.k))
			}
		}
	}
}

func compileSignals(c *checker, v *Variant, raws []rawSignal, hdrOf func(string, string) ([4]byte, bool)) {
	names := map[string]bool{}
	for _, r := range raws {
		where := "signals[" + r.Name + "]"
		if r.Name == "" || names[r.Name] {
			c.errf(where, "a name is required, once")
		}
		names[r.Name] = true
		if r.Remove {
			c.errf(where, "remove: nothing inherited by that name")
			continue
		}
		checkStatus(c, where, r.Status, false)
		s := &sigSpec{name: r.Name, label: r.Label, length: 1, mul: 1,
			values: map[byte]valueSpec{}, lines: map[byte]string{},
			unknown: "code %#02x", unknownLin: r.UnknownLine, suffix: r.Suffix, state: r.State}
		if r.Len != nil {
			s.length = *r.Len
		}
		if s.length < 0 {
			c.errf(where, "len is 0 (any) or more")
		}
		kinds := 0
		if f := r.Flag; f != nil {
			kinds++
			s.kind, s.on, s.off, s.offAny = kindFlag, f.On, f.Off.v, f.Off.any
			s.onText, s.offText = cmpOr(f.OnText, "on"), cmpOr(f.OffText, "off")
		}
		if r.Enum != nil {
			kinds++
			s.kind, s.enum = kindEnum, r.Enum
		}
		if n := r.Number; n != nil {
			kinds++
			s.kind, s.add, s.format, s.wrap = kindNumber, n.Add, n.Format, n.Wrap
			if n.Mul != nil {
				s.mul = *n.Mul
			}
			s.div = 1
			if n.Div != nil {
				s.div = *n.Div
			}
			hasG := strings.Contains(n.Format, "%g")
			switch {
			case s.div < 1:
				c.errf(where, "number.div is 1 or more")
			case s.div > 1 && (!hasG || strings.Contains(n.Format, "%d") || strings.Contains(n.Format, "%#02x")):
				c.errf(where, "number.format with div is %%g")
			case s.div == 1 && hasG:
				c.errf(where, "number.format %%g is for div")
			}
			switch len(n.Range) {
			case 0:
			case 2:
				s.ranged, s.lo, s.hi = true, n.Range[0], n.Range[1]
				if s.lo > s.hi {
					c.errf(where, "range is [lo, hi]")
				}
			default:
				c.errf(where, "range is [lo, hi]")
			}
			if n.Format == "" {
				c.errf(where, "number.format is required")
			}
			if err := checkFormat(strings.Replace(n.Format, "%g", "%d", 1)); err != nil {
				c.errf(where, "%v", err)
			}
		}
		if f := r.Fan; f != nil {
			kinds++
			s.kind, s.fanOff, s.fanAuto = kindFan, f.Off, f.Auto
			if len(f.Levels) != 2 || f.Levels[0] > f.Levels[1] {
				c.errf(where, "fan.levels is [lo, hi]")
			} else {
				s.fanLo, s.fanHi = f.Levels[0], f.Levels[1]
			}
		}
		if b := r.Bits; b != nil {
			kinds++
			s.kind, s.always, s.none, s.bitNames = kindBits, b.Always, b.None, b.Names
			for bit := range b.Names {
				if bit > 7 {
					c.errf(where, "bit %d is not 0-7", bit)
				}
			}
		}
		if b := r.Burst; b != nil {
			kinds++
			s.kind, s.begin = kindBurst, b.Begin
		}
		if kinds != 1 {
			c.errf(where, "exactly one of flag, enum, number, fan, bits and burst")
			continue
		}
		if s.label == "" && s.kind != kindBurst {
			c.errf(where, "label is required")
		}
		for b, vs := range r.Values {
			if vs.State != nil && s.kind != kindNumber {
				c.errf(where, "values[%d]: only a number's values carry a state", b)
			}
			s.values[b] = vs
		}
		for b, l := range r.Lines {
			if strings.Contains(l, "%") {
				c.errf(where, "lines[%d]: a line is literal text", b)
			}
			s.lines[b] = l
		}
		if r.Unknown != nil {
			s.unknown, s.quiet = *r.Unknown, *r.Unknown == ""
		}
		for _, f := range []string{s.unknown, s.unknownLin, s.suffix} {
			if err := checkFormat(f); err != nil {
				c.errf(where, "%v", err)
			}
		}
		if strings.Contains(s.suffix, "%") {
			c.errf(where, "suffix is literal text")
		}
		if r.State != "" {
			if k, ok := stateKinds[r.State]; !ok {
				c.errf(where, "state %q is not a CarClimate field", r.State)
			} else if k != s.kind {
				c.errf(where, "state %s needs another kind of signal", r.State)
			}
		}
		if len(r.Cmd) == 0 {
			c.errf(where, "cmd is required")
		}
		hdr, ok := hdrOf(where, r.Hdr)
		if !ok {
			continue
		}
		for _, cmd := range r.Cmd {
			hc := hdrCmd{hdr, cmd}
			if other, dup := v.signals[hc]; dup {
				c.errf(where, "cmd %#02x of %x is already signal %s", cmd, hdr, other.name)
				continue
			}
			v.signals[hc] = s
		}
	}
}

// parseHdrMask reads a header with open bytes, "0c 88 ?? 8c": the bytes and
// the mask of those that must match.
func parseHdrMask(s string) (hdr, mask [4]byte, err error) {
	f := strings.Fields(s)
	if len(f) != 4 || !slices.Contains(f, "??") {
		return hdr, mask, fmt.Errorf("header %q has no open byte", s)
	}
	for i, x := range f {
		if x == "??" {
			continue
		}
		if _, err := fmt.Sscanf(x, "%02x", &hdr[i]); err != nil || len(x) != 2 {
			return hdr, mask, fmt.Errorf("header %q is not 4 hex bytes or ??", s)
		}
		mask[i] = 0xff
	}
	return hdr, mask, nil
}

// matches is whether a frame's header is the decoder's.
func (d *decoder) matches(h [4]byte) bool {
	for i := range h {
		if h[i]&d.mask[i] != d.hdr[i] {
			return false
		}
	}
	return true
}

// parseHdr reads "hh hh hh hh".
func parseHdr(s string) ([4]byte, error) {
	var b [4]byte
	f := strings.Fields(s)
	if len(f) == 1 && len(f[0]) == 8 {
		f = []string{f[0][0:2], f[0][2:4], f[0][4:6], f[0][6:8]}
	}
	if len(f) != 4 {
		return b, fmt.Errorf("header %q is not 4 hex bytes", s)
	}
	for i, x := range f {
		if _, err := fmt.Sscanf(x, "%02x", &b[i]); err != nil || len(x) != 2 {
			return b, fmt.Errorf("header %q is not 4 hex bytes", s)
		}
	}
	return b, nil
}

// parseTemplate reads a profile template: a whole frame in hex whose payload
// bytes may be clock placeholders ({second} ...), the checksum "..".
func parseTemplate(s string) (hdr [4]byte, cmd byte, slots []slot, err error) {
	toks := strings.Fields(s)
	bad := func(why string) ([4]byte, byte, []slot, error) {
		return hdr, cmd, nil, fmt.Errorf("template %q: %s", s, why)
	}
	if len(toks) < 9 || toks[len(toks)-1] != ".." {
		return bad("not a whole frame ending in the checksum \"..\"")
	}
	var fixed [8]byte
	for i := 0; i < 8; i++ {
		if _, e := fmt.Sscanf(toks[i], "%02x", &fixed[i]); e != nil || len(toks[i]) != 2 {
			return bad("start, header, command and length must be hex bytes")
		}
	}
	if fixed[0] != Start {
		return bad(fmt.Sprintf("does not start with %02x", Start))
	}
	copy(hdr[:], fixed[1:5])
	cmd = fixed[5]
	for _, t := range toks[8 : len(toks)-1] {
		if name, ok := strings.CutPrefix(t, "{"); ok {
			name, ok = strings.CutSuffix(name, "}")
			if _, known := clockFields[name]; !ok || !known {
				return bad(fmt.Sprintf("unknown placeholder %s", t))
			}
			slots = append(slots, slot{field: name})
			continue
		}
		var b byte
		if _, e := fmt.Sscanf(t, "%02x", &b); e != nil || len(t) != 2 {
			return bad(fmt.Sprintf("%q is neither a hex byte nor a placeholder", t))
		}
		slots = append(slots, slot{fixed: b})
	}
	if n := int(fixed[6])<<8 | int(fixed[7]); n != len(slots)+1 {
		return bad(fmt.Sprintf("the length field is not the payload's (%d) plus the checksum", len(slots)))
	}
	return hdr, cmd, slots, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
