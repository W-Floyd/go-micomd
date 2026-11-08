package micom

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Identity is what the kernel command line says about the car. The stock
// bootloader reads a record from the micom before the kernel starts and
// passes its vehicle and country codes as vehicle_info=0x.. and
// country_info=0x.. (0xff when the read failed); the other keys are the head
// unit's own.
type Identity map[string]int

// identityKeys are the command-line keys a variant may match on.
var identityKeys = []string{"vehicle_info", "country_info", "daudio_hw_ver", "cabin_camera_type"}

// ParseIdentity reads the identity keys out of a kernel command line, numbers
// in decimal or 0x hex. A key that is absent, or 0xff for the two the
// bootloader could not read, is left out.
func ParseIdentity(cmdline string) Identity {
	id := Identity{}
	for _, w := range strings.Fields(cmdline) {
		k, v, ok := strings.Cut(w, "=")
		if !ok || !isIdentityKey(k) {
			continue
		}
		n, err := strconv.ParseInt(v, 0, 64)
		if err != nil {
			continue
		}
		if (k == "vehicle_info" || k == "country_info") && n == 0xff {
			continue
		}
		id[k] = int(n)
	}
	return id
}

// recordVerdict is what the micom's variant record, read as id, says of the
// variant running: the variant id matches, and a line for the log and the
// snapshot. The variant is never switched live: the profile's senders, the
// allow list, the keyboard's keys and the input's messages were all made
// from it at start, so a mismatch asks for a restart instead.
func recordVerdict(running string, id Identity) (matched, msg string) {
	matched, err := MatchVariant(id)
	says := "the micom reports " + identityString(id)
	switch {
	case err != nil:
		return "", fmt.Sprintf("%s: %v", says, err)
	case matched == "":
		return "", fmt.Sprintf("%s, which no variant matches; running %s", says, running)
	case matched == running:
		return matched, fmt.Sprintf("%s, variant %s, as running", says, matched)
	}
	return matched, fmt.Sprintf("%s, variant %s; running %s: restart with -variant %s", says, matched, running, matched)
}

// Stock Android retries its variant-code request 5 times
// (EVENT_VARIANTCODING_RETRY); 3 s each is ours.
const (
	identifyTries = 5
	identifyWait  = 3 * time.Second
)

// askVariant sends req, the identify request, once a frame has arrived (the
// micom is up), and again every wait until its variant record arrives,
// tries times at most.
func (d *Daemon) askVariant(ctx context.Context, req Frame, tries int, wait time.Duration) {
	for !d.linkUp() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	for i := 1; i <= tries && !d.hasVariantRecord(); i++ {
		if err := d.Send(req); err != nil {
			d.logf("-identify-request: %v", err)
			return
		}
		d.logf("asked the micom for its variant coding (%d/%d): %v", i, tries, req)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
	if !d.hasVariantRecord() {
		d.logf("-identify-request: no variant record after %d requests", tries)
	}
}

func (d *Daemon) linkUp() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.lastRx.IsZero()
}

func (d *Daemon) hasVariantRecord() bool {
	d.telem.mu.Lock()
	defer d.telem.mu.Unlock()
	return d.telem.reply != nil
}

func isIdentityKey(k string) bool {
	for _, x := range identityKeys {
		if x == k {
			return true
		}
	}
	return false
}
