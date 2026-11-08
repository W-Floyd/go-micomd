package micom

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestAgainstEmulator runs the real Daemon against the Icicle micom-run
// emulator (tools/micom-harness in the nura-G5W-Android repo) over its
// --uart-sock, instead of a mock io.ReadWriteCloser: the closest thing to a
// golden-trace test without the real unit.
//
// Needs, from the environment (unset: skipped, nothing else in this package
// depends on them):
//
//	MICOM_RUNNER  path to the built micom-run binary
//	MICOM_IMAGE   path to the decrypted micom_sw.bin (proprietary; never in
//	              this repo)
//	MICOM_STATE   a --save-state snapshot taken after CpuUart_Init but
//	              before the handshake below (the harness's boot25m.txt)
//
// GHIDRA_SRC and ICICLE_LANG, which micom-run itself needs to build its
// Icicle VM, are inherited from the test's own environment, same as running
// micom-run by hand.
//
// The emulator is run at --realtime 8 (its own clock tracks the wall clock,
// at 8x: see tools/micom-harness/docs/firmware-runner.md for why --realtime,
// not --time-scale, is the one that gives a real multiple of real time). The
// daemon's own profile is scaled the same way with ScaledProfile, so its
// send cadence matches the micom's clock rather than the host's wall clock.
//
// 8 is not an arbitrary default: a sweep (MICOM_TEST_SCALE, below) found the
// relationship between scale and real wall-clock time to reach the alive
// exchange is NOT monotonic. 1 is too close to honest pacing to finish in a
// reasonable time; 8-12 is the fastest region found, but 12 failed one run
// in three; 16 and 32 pass but take longer in real time than 8 does, not
// less; 64 and up fail outright. The likely mechanism: a higher scale means
// more PIT ticks, hence more interrupt-handling, per real second, and past
// some point that overhead costs more real time than the faster clock saves.
// MICOM_TEST_SCALE overrides the default for deliberately exploring this;
// it is not a dial to turn up for routine speed.
//
// The daemon does not implement the SoC-side handshake yet (a known gap, not
// this test's concern), so the test sends it itself, straight into the live
// connection before handing that same connection to a Daemon: cmd 0x01
// brings the link up, cmd 0x11 lets the micom's own alive request through
// (see tools/micom-harness/docs/micom-findings.md). This, not a snapshot
// taken after the handshake, is deliberate: a --save-state/--load-state
// round trip through an in-progress exchange has not been confirmed
// faithful for everything the sequencer depends on, where sending the
// handshake into one continuously-running process has.
func TestAgainstEmulator(t *testing.T) {
	runner := os.Getenv("MICOM_RUNNER")
	image := os.Getenv("MICOM_IMAGE")
	state := os.Getenv("MICOM_STATE")
	if runner == "" || image == "" || state == "" {
		t.Skip("set MICOM_RUNNER, MICOM_IMAGE and MICOM_STATE to run against the Icicle emulator (see tools/micom-harness)")
	}

	scale := 8
	if v := os.Getenv("MICOM_TEST_SCALE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("MICOM_TEST_SCALE: %v", err)
		}
		scale = n
	}
	const soc = "0xffe44000" // LINFlex_1, the SoC link

	sock := fmt.Sprintf("/tmp/micomd-it-%d.sock", os.Getpid())
	os.Remove(sock)
	defer os.Remove(sock)

	ctx, cancelProc := context.WithCancel(context.Background())
	defer cancelProc()
	cmd := exec.CommandContext(ctx, runner,
		"--image", image,
		"--load-state", state,
		"--realtime", strconv.Itoa(scale),
		"--uart-sock", soc+"="+sock+",99",
		"--max-insns", "2000000000",
	)
	cmd.Stderr = nil // the emulator's own diagnostics are not this test's concern
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting micom-run: %v", err)
	}
	defer cmd.Process.Kill()

	var conn net.Conn
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if conn, err = net.Dial("unix", sock); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("dialing the emulator's uart-sock: %v", err)
	}
	defer conn.Close()

	// The handshake: see the doc comment above.
	hello := Frame{Hdr: hdrAliveReply, Cmd: 0x01, Payload: []byte{0}}.Encode()
	unsuppress := Frame{Hdr: hdrAliveReply, Cmd: 0x11, Payload: []byte{0}}.Encode()
	if _, err := conn.Write(append(hello, unsuppress...)); err != nil {
		t.Fatalf("sending the handshake: %v", err)
	}

	seen := make(chan Frame, 32)
	d := New(Config{
		Port:    conn,
		Profile: ScaledProfile(time.Duration(scale), DefaultProfile()),
		OnFrame: func(f Frame) { seen <- f },
	})
	ctx2, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx2) }()

	// The micom's own alive request, answered inline, and a reply to one of
	// our poll frames: both must happen within the scaled-down window a real
	// micom allows (see ScaledProfile's doc).
	var gotAlive, gotPollReply bool
	deadline := time.After(25 * time.Second)
	for !gotAlive || !gotPollReply {
		select {
		case f := <-seen:
			if f.Hdr == fromMicom && f.Cmd == 0x5c {
				gotPollReply = true
			}
		case <-time.After(50 * time.Millisecond):
			if s := d.Stats(); s.AliveRx > 0 && s.AliveTx == s.AliveRx {
				gotAlive = true
			}
		case <-deadline:
			s := d.Stats()
			t.Fatalf("timed out: alive %d/%d, poll reply seen=%v", s.AliveRx, s.AliveTx, gotPollReply)
		}
	}

	cancel()
	<-done
}
