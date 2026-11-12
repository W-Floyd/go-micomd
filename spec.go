package micom

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// SpecMarkdown renders a variant, its parents merged in, as Markdown tables.
// name is an embedded variant's name, or a path to a variant file.
func SpecMarkdown(name string) (string, error) {
	var rv rawVariant
	var err error
	if strings.HasSuffix(name, ".yaml") || strings.ContainsRune(name, '/') {
		var data []byte
		if data, err = os.ReadFile(name); err == nil {
			rv, err = parseVariant(data)
		}
	} else {
		rv, err = embeddedRaw(name)
	}
	if err != nil {
		return "", err
	}
	if rv, err = resolve(rv); err != nil {
		return "", err
	}
	return renderSpec(rv), nil
}

type table struct {
	head []string
	rows [][]string
}

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t table) write(b *strings.Builder) {
	if len(t.rows) == 0 {
		return
	}
	// Drop columns that are empty in every row.
	var keep []int
	for i := range t.head {
		for _, r := range t.rows {
			if r[i] != "" {
				keep = append(keep, i)
				break
			}
		}
	}
	line := func(cells []string) {
		b.WriteString("|")
		for _, i := range keep {
			c := strings.ReplaceAll(strings.ReplaceAll(noMath(cells[i]), "|", `\|`), "\n", " ")
			b.WriteString(" " + c + " |")
		}
		b.WriteString("\n")
	}
	line(t.head)
	b.WriteString("|" + strings.Repeat(" --- |", len(keep)) + "\n")
	for _, r := range t.rows {
		line(r)
	}
	b.WriteString("\n")
}

// noMath stops a $ (Java inner classes, ClusterService$NaviCluster) from
// opening a LaTeX span in GitHub and VS Code. Inside a code span it is
// literal already, so such text is left alone.
func noMath(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '`':
			in = !in
		case r == '$' && !in:
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func code(s string) string {
	if s == "" {
		return ""
	}
	return "`" + s + "`"
}

func hex8(b uint8) string { return fmt.Sprintf("0x%02x", b) }

func opt[T any](p *T, f func(T) string) string {
	if p == nil {
		return ""
	}
	return f(*p)
}

func itoa(i int) string { return fmt.Sprint(i) }

func renderSpec(rv rawVariant) string {
	var b strings.Builder
	title := rv.Title
	if title == "" {
		title = rv.Name
	}
	fmt.Fprintf(&b, "# %s\n\nVariant `%s`", title, rv.Name)
	if rv.Extends != "" {
		fmt.Fprintf(&b, ", extending `%s` (merged here)", rv.Extends)
	}
	b.WriteString(".\n\n")

	// hdr resolves a header name to its bytes; a literal stays as written.
	hdr := func(s string) string {
		if h, ok := rv.Headers[s]; ok {
			return code(h.Hdr) + " (" + s + ")"
		}
		return code(s)
	}
	names := func(m map[string]rawHeader) []string {
		k := make([]string, 0, len(m))
		for n := range m {
			k = append(k, n)
		}
		slices.Sort(k)
		return k
	}

	b.WriteString("## How to read this\n\n" +
		"Every frame on the serial link has this shape; the payload is what the message tables below describe, byte 0 first.\n\n" + frameDiagram +
		"The 4 header bytes are `sender receiver type func`; a header and its reverse (`0d 8f ..` and `8f 0d ..`) are the two directions of one conversation.\n\n" +
		legend +
		"A message is a header plus a cmd. Find the header, then the cmd under it; its field table says which payload byte holds what.\n\n")

	b.WriteString(confidence(rv))

	if m := rv.Match; m != nil {
		b.WriteString("## Match\n\n")
		t := table{head: []string{"Key", "Value"}}
		keys := make([]string, 0, len(m.Cmdline))
		for k := range m.Cmdline {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			t.add(k, fmt.Sprint([]int(m.Cmdline[k])))
		}
		t.add("status", badge(m.Status))
		t.add("ref", m.Ref)
		t.write(&b)
	}

	if len(rv.Headers) > 0 {
		b.WriteString("## Headers\n\n")
		t := table{head: []string{"Name", "Header", "Direction", "Status", "Ref"}}
		for _, n := range names(rv.Headers) {
			h := rv.Headers[n]
			t.add(n, code(h.Hdr), h.Dir, badge(h.Status), h.Ref)
		}
		t.write(&b)
	}

	if len(rv.Enums) > 0 {
		b.WriteString("## Enums\n\n")
		usedBy := map[string][]string{}
		for _, m := range rv.Messages {
			for _, f := range m.Fields {
				if f.Enum != "" {
					usedBy[f.Enum] = append(usedBy[f.Enum], m.Name+": "+f.Name)
				}
			}
		}
		en := make([]string, 0, len(rv.Enums))
		for n := range rv.Enums {
			en = append(en, n)
		}
		slices.Sort(en)
		sorted := func(n string) []string {
			vals := make([]string, 0, len(rv.Enums[n]))
			for v := range rv.Enums[n] {
				vals = append(vals, v)
			}
			slices.SortFunc(vals, func(a, c string) int { return int(rv.Enums[n][a] - rv.Enums[n][c]) })
			return vals
		}
		const long = 8 // more values than this get a table of their own
		t := table{head: []string{"Enum", "Values", "Used by"}}
		var big []string
		for _, n := range en {
			vals := sorted(n)
			use := strings.Join(usedBy[n], "; ")
			if len(vals) > long {
				big = append(big, n)
				t.add(n, fmt.Sprintf("%d values, below", len(vals)), use)
				continue
			}
			for i, v := range vals {
				vals[i] = fmt.Sprintf("**%d** %s", rv.Enums[n][v], v)
			}
			t.add(n, strings.Join(vals, "<br>"), use)
		}
		t.write(&b)
		for _, n := range big {
			fmt.Fprintf(&b, "### %s\n\n", n)
			bt := table{head: []string{"Value", "Name"}}
			for _, v := range sorted(n) {
				bt.add(fmt.Sprint(rv.Enums[n][v]), v)
			}
			bt.write(&b)
		}
	}

	for _, dir := range []string{"from_micom", "to_micom"} {
		var msgs []rawMessage
		for _, m := range rv.Messages {
			d := m.Dir
			if d == "" {
				d = "from_micom"
			}
			if d == dir {
				msgs = append(msgs, m)
			}
		}
		if len(msgs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## Messages %s\n\n", strings.Replace(dir, "_", " ", 1))
		last := "\x00"
		for _, m := range msgs {
			if m.Hdr != last {
				last = m.Hdr
				fmt.Fprintf(&b, "### %s\n\n%s\n\n", m.Hdr, hdrNote(rv, m.Hdr))
			}
			l := ""
			if m.Length != nil {
				l = ", length " + itoa(*m.Length)
			} else if m.MinLength != nil {
				l = ", length ≥ " + itoa(*m.MinLength)
			}
			fmt.Fprintf(&b, "#### %s\n\ncmd `%s`%s", m.Name, hex8(m.Cmd), l)
			if m.Status != "" {
				b.WriteString(" · " + badge(m.Status))
			}
			b.WriteString("\n\n")
			if m.Note != "" {
				b.WriteString(noMath(m.Note) + "\n\n")
			}
			if m.Ref != "" {
				b.WriteString("> " + noMath(m.Ref) + "\n\n")
			}
			if len(m.Fields) == 0 {
				continue
			}
			ft := table{head: []string{"Field", "At", "Type", "Scale", "Offset", "Unit", "Enum", "Range", "Notes"}}
			for _, f := range m.Fields {
				at := ""
				switch {
				case f.Off != nil:
					at = "byte " + itoa(*f.Off)
				case f.StartBit != nil:
					at = fmt.Sprintf("bit %d, %d long", *f.StartBit, f.BitLength)
				}
				if f.Bits != "" {
					at += " bits " + f.Bits
				}
				typ := f.Type
				if f.Signed {
					typ += " signed"
				}
				if f.Endian != "" {
					typ += " " + f.Endian
				}
				rng := ""
				if f.Min != nil || f.Max != nil {
					rng = opt(f.Min, func(x float64) string { return fmt.Sprint(x) }) + ".." + opt(f.Max, func(x float64) string { return fmt.Sprint(x) })
				}
				notes := ""
				if f.Invalid != nil {
					notes = fmt.Sprintf("invalid %d", *f.Invalid)
				}
				if s := f.From; s != nil {
					notes = strings.TrimPrefix(notes+"; from "+fieldSource{clock: s.Clock, field: s.Field}.String(), "; ")
				}
				scale := opt(f.Scale, func(x float64) string { return fmt.Sprint(x) })
				off := ""
				if f.Offset != 0 {
					off = fmt.Sprint(f.Offset)
				}
				ft.add(f.Name, at, strings.TrimSpace(typ), scale, off, strings.TrimSpace(f.Unit), f.Enum, rng, notes)
			}
			ft.write(&b)
			b.WriteString(packetDiagram(m))
		}
	}

	if len(rv.Signals) > 0 {
		b.WriteString("## Signals\n\n")
		t := table{head: []string{"Name", "Header", "Cmd", "Kind", "Label", "State", "Status", "Ref"}}
		for _, s := range rv.Signals {
			var cmds []string
			for _, c := range s.Cmd {
				cmds = append(cmds, hex8(c))
			}
			var kind []string
			for _, k := range []struct {
				name string
				set  bool
			}{{"flag", s.Flag != nil}, {"enum", s.Enum != nil}, {"number", s.Number != nil}, {"fan", s.Fan != nil},
				{"bits", s.Bits != nil}, {"burst", s.Burst != nil}, {"values", s.Values != nil}, {"lines", s.Lines != nil}} {
				if k.set {
					kind = append(kind, k.name)
				}
			}
			t.add(s.Name, hdr(s.Hdr), strings.Join(cmds, ", "), strings.Join(kind, " "), s.Label, s.State, badge(s.Status), s.Ref)
		}
		t.write(&b)
	}

	if k := rv.Keys; k != nil && (len(k.Push) > 0 || len(k.Knobs) > 0) {
		b.WriteString("## Keys\n\n")
		t := table{head: []string{"Kind", "Type", "Code", "Label", "Key / up / down"}}
		for _, p := range k.Push {
			t.add("push", hex8(p.Type), hex8(p.Code), p.Label, fmt.Sprint(uint16(p.Key)))
		}
		for _, n := range k.Knobs {
			t.add("knob", hex8(n.Type), hex8(n.Code), n.Label, fmt.Sprintf("%d / %d", uint16(n.Up), uint16(n.Down)))
		}
		t.write(&b)
	}

	if len(rv.Profile) > 0 {
		b.WriteString("## Profile (sent routinely)\n\n")
		t := table{head: []string{"Name", "Every", "Frame", "Template", "Opt-in", "Replaces", "Status", "Ref"}}
		for _, p := range rv.Profile {
			e := ""
			if p.Every != 0 {
				e = p.Every.String()
			}
			t.add(p.Name, e, code(p.Frame), p.Template, p.OptIn, p.Replaces, badge(p.Status), p.Ref)
		}
		t.write(&b)
	}

	if len(rv.Send) > 0 {
		b.WriteString("## Send (the control socket may send)\n\n")
		t := table{head: []string{"Name", "Frame", "Pattern", "Status", "Ref"}}
		for _, s := range rv.Send {
			t.add(s.Name, code(s.Frame), code(s.Pattern), badge(s.Status), s.Ref)
		}
		t.write(&b)
	}
	return withContents(b.String())
}

// withContents puts a list of the ## sections after the intro paragraph.
func withContents(doc string) string {
	head, rest, ok := strings.Cut(doc, "\n\n## ")
	if !ok {
		return doc
	}
	rest = "## " + rest
	var toc strings.Builder
	for _, l := range strings.Split(rest, "\n") {
		if t, ok := strings.CutPrefix(l, "## "); ok {
			anchor := strings.ToLower(strings.NewReplacer(" ", "-", "(", "", ")", "").Replace(t))
			fmt.Fprintf(&toc, "- [%s](#%s)\n", t, anchor)
		}
	}
	return head + "\n\n" + toc.String() + "\n" + rest
}

// badge marks how a thing is known, strongest first. The variant files give
// a status per message, signal or header, not per field.
func badge(status string) string {
	switch status {
	case "captured":
		return "🟢 captured"
	case "inferred":
		return "🟠 inferred"
	case "code":
		return "🟡 code"
	case "":
		return ""
	}
	return "⚪ " + status
}

const legend = "How well each thing is known (per message, signal or header; the files do not grade single fields):\n\n" +
	"- 🟢 **captured**: seen on the wire or on the live unit.\n" +
	"- 🟡 **code**: read from stock code, not yet seen to match the car.\n" +
	"- 🟠 **inferred**: worked out from code or the frame rule.\n\n"

// confidence counts messages by status and lists those no capture backs.
func confidence(rv rawVariant) string {
	count := map[string]int{}
	var open []rawMessage
	for _, m := range rv.Messages {
		s := m.Status
		if s == "" {
			s = "unstated"
		}
		count[s]++
		if s != "captured" {
			open = append(open, m)
		}
	}
	if len(rv.Messages) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Confidence\n\n")
	t := table{head: []string{"Status", "Messages"}}
	for _, s := range []string{"captured", "code", "inferred", "unstated"} {
		if count[s] > 0 {
			t.add(badge(s), itoa(count[s]))
			delete(count, s)
		}
	}
	for _, s := range sortedKeys(count) {
		t.add(badge(s), itoa(count[s]))
	}
	t.write(&b)
	if len(open) > 0 {
		b.WriteString("Not yet backed by a capture:\n\n")
		ot := table{head: []string{"Message", "Header", "Cmd", "Status", "Ref"}}
		for _, m := range open {
			ot.add(m.Name, code(m.Hdr), hex8(m.Cmd), badge(m.Status), m.Ref)
		}
		ot.write(&b)
	}
	return b.String()
}

const frameDiagram = "```mermaid\npacket-beta\n0-7: \"0xff start\"\n8-39: \"header (4 bytes)\"\n40-47: \"cmd\"\n48-63: \"length (payload + checksum)\"\n64-95: \"payload ...\"\n96-103: \"checksum\"\n```\n\n"

// packetDiagram is a message's byte layout, or "" for bit-placed fields and
// layouts that overlap.
func packetDiagram(m rawMessage) string {
	type span struct {
		lo, hi int
		name   string
	}
	var spans []span
	end := 0
	for _, f := range m.Fields {
		if f.Off == nil || f.StartBit != nil || f.Bits != "" {
			return ""
		}
		w := typeWidths[f.Type]
		if f.Type == "" {
			w = 1
		}
		if f.Type == "str" {
			w = f.Size
		}
		if w < 1 {
			return ""
		}
		spans = append(spans, span{*f.Off, *f.Off + w - 1, f.Name})
		end = max(end, *f.Off+w)
	}
	if len(spans) == 0 || end > 48 {
		return ""
	}
	slices.SortFunc(spans, func(a, c span) int { return a.lo - c.lo })
	var b strings.Builder
	b.WriteString("```mermaid\npacket-beta\n")
	next := 0
	for _, s := range spans {
		if s.lo < next {
			return ""
		}
		if s.lo > next {
			fmt.Fprintf(&b, "%d-%d: \"?\"\n", next*8, s.lo*8-1)
		}
		fmt.Fprintf(&b, "%d-%d: \"%s\"\n", s.lo*8, s.hi*8+7, strings.ReplaceAll(s.name, "\"", "'"))
		next = s.hi + 1
	}
	b.WriteString("```\n\n")
	return b.String()
}

// hdrNote says what a header name stands for.
func hdrNote(rv rawVariant, s string) string {
	if h, ok := rv.Headers[s]; ok {
		n := code(h.Hdr)
		if h.Ref != "" {
			n += ": " + noMath(h.Ref)
		}
		return n
	}
	return ""
}
