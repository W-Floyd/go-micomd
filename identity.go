package micom

import (
	"strconv"
	"strings"
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

func isIdentityKey(k string) bool {
	for _, x := range identityKeys {
		if x == k {
			return true
		}
	}
	return false
}
