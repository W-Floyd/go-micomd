package micom

// Frames no variant file and no -allow may make sendable: each would reset,
// rewrite or reconfigure the micom. All are SystemService requests on the
// system header (sendPacket(0x8a, 1, func), the framing of the captured
// 0x010F alive reply), read from the stock automotive-service
// (jadx SystemService.java).
var denylist = []struct {
	hdr [4]byte
	cmd byte
	why string
}{
	// From the EV6 micom firmware, not SystemService: it writes EEPROM
	// 0x1ad, and 0 makes ACC off skip straight to bus sleep.
	{hdrAliveReply, 0x02, "func 0x0102, SYS_POWER_C (writes the micom's power configuration)"},
	{hdrAliveReply, 0x05, "func 0x0105, requestRearCameraReset"},
	{hdrAliveReply, 0x07, "func 0x0107, requestLastInfoInitialization (also requestMicomFactoryReset)"},
	{hdrAliveReply, 0x18, "func 0x0118, requestVariantCodeSave (writes the variant coding)"},
	{hdrAliveReply, 0x1e, "func 0x011e, requestSPISendingStop"},
	{hdrAliveReply, 0x28, "func 0x0128, requestVBusOffOn"},
}

// denied is why a frame with this header and command may never be sent, ""
// when it may be.
func denied(hdr [4]byte, cmd byte) string {
	for _, d := range denylist {
		if d.hdr == hdr && d.cmd == cmd {
			return d.why
		}
	}
	return ""
}
