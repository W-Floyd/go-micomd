// Command mkcorpus writes testdata/corpus.txt: the distinct frames emulated
// micoms answered (the send-entry matrix and the probe, both under
// scratch/.cache/fw), at most -cap payloads per header and command, spread
// over the sorted set so the sample is stable. Run from the repository root.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var reply = regexp.MustCompile(`^hdr=([0-9a-f]{8}) cmd=([0-9a-f]{2}) payload=([0-9a-f]*)`)

func main() {
	dir := flag.String("cache", "scratch/.cache/fw", "directory with matrix.jsonl and probe.jsonl")
	out := flag.String("o", "testdata/corpus.txt", "the corpus to write")
	capN := flag.Int("cap", 6, "payloads kept per header and command")
	flag.Parse()

	seen := map[[3]string]bool{}
	lines(*dir+"/matrix.jsonl", func(raw []byte) {
		var d struct {
			Entries map[string]struct{ Replies []string }
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			log.Fatal(err)
		}
		for _, e := range d.Entries {
			for _, r := range e.Replies {
				if m := reply.FindStringSubmatch(r); m != nil {
					seen[[3]string{m[1], m[2], m[3]}] = true
				}
			}
		}
	})
	lines(*dir+"/probe.jsonl", func(raw []byte) {
		var d struct{ Frames map[string]string }
		if err := json.Unmarshal(raw, &d); err != nil {
			log.Fatal(err)
		}
		for c, p := range d.Frames {
			n, err := strconv.ParseUint(c, 16, 8)
			if err != nil {
				log.Fatal(err)
			}
			seen[[3]string{"018a0181", fmt.Sprintf("%02x", n), p}] = true
		}
	})

	all := make([][3]string, 0, len(seen))
	for k := range seen {
		all = append(all, k)
	}
	sort.Slice(all, func(i, j int) bool {
		for x := range all[i] {
			if all[i][x] != all[j][x] {
				return all[i][x] < all[j][x]
			}
		}
		return false
	})
	var b strings.Builder
	b.WriteString("# hdr cmd payload: frames from emulated micoms, read by TestGolden\n")
	kept := 0
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j][0] == all[i][0] && all[j][1] == all[i][1] {
			j++
		}
		group := all[i:j]
		if len(group) > *capN {
			pick := make([][3]string, *capN)
			for k := range pick {
				pick[k] = group[k*(len(group)-1)/(*capN-1)]
			}
			group = pick
		}
		for _, f := range group {
			fmt.Fprintf(&b, "%s %s %s\n", f[0], f[1], f[2])
			kept++
		}
		i = j
	}
	if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(all), "distinct,", kept, "kept")
}

func lines(path string, f func([]byte)) {
	fh, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			f(sc.Bytes())
		}
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
}
