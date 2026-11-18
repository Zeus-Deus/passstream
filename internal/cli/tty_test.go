package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSecretPromptRestoresEchoAfterInterrupt(t *testing.T) {
	const helper = "PASSSTREAM_TEST_SECRET_TTY"
	switch os.Getenv(helper) {
	case "human":
		os.Exit(Main([]string{"share", "--pin", "passphrase"}))
	case "json":
		os.Exit(Main([]string{"share", "--pin", "passphrase", "--json"}))
	}
	for _, tc := range []struct {
		name       string
		stopBefore bool
		stopAfter  bool
		json       bool
	}{
		{name: "normal output"},
		{name: "output stopped before prompt", stopBefore: true},
		{name: "output stopped after prompt", stopAfter: true},
		{name: "JSON with output stopped after prompt", stopAfter: true, json: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode := "human"
			if tc.json {
				mode = "json"
			}
			testSecretPromptInterrupt(t, helper+"="+mode, tc.stopBefore, tc.stopAfter)
		})
	}
}

func testSecretPromptInterrupt(t *testing.T, helper string, stopBefore, stopAfter bool) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("no pseudoterminal device")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	termios := func() unix.Termios {
		t.Helper()
		m, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		return *m
	}
	before := termios()
	if before.Lflag&unix.ECHO == 0 {
		t.Fatal("expected echo enabled on a new pseudoterminal")
	}
	slaveFD := int(slave.Fd())
	if stopBefore || stopAfter {
		// Keep Ctrl+C from flushing the queues or resuming stopped output.
		before.Lflag |= unix.NOFLSH
		before.Iflag &^= unix.IXON
		if err := unix.IoctlSetTermios(slaveFD, unix.TCSETS, &before); err != nil {
			t.Fatal(err)
		}
	}
	stopOutput := func() {
		t.Helper()
		if err := unix.IoctlSetInt(slaveFD, unix.TCXONC, unix.TCOOFF); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.Write(slaveFD, []byte("output probe")); !errors.Is(err, unix.EAGAIN) {
			t.Fatalf("expected output to be blocked, got %v", err)
		}
	}
	if stopBefore {
		stopOutput()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSecretPromptRestoresEchoAfterInterrupt$")
	cmd.Env = append(os.Environ(), helper)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	deadline, _ := ctx.Deadline()
	if err := master.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if stopBefore {
		// The prompt cannot arrive. Disabling echo tells us the helper reached it.
		for termios().Lflag&unix.ECHO != 0 {
			if ctx.Err() != nil {
				t.Fatal("timed out waiting for the prompt to disable echo")
			}
			time.Sleep(5 * time.Millisecond)
		}
	} else {
		const prompt = "Passphrase: "
		buf := make([]byte, len(prompt))
		if _, err := io.ReadFull(master, buf); err != nil || string(buf) != prompt {
			t.Fatalf("waiting for prompt: read %q, error %v", buf, err)
		}
	}
	if stopAfter {
		stopOutput()
	}
	if termios().Lflag&unix.ECHO != 0 {
		t.Fatal("echo was enabled while prompting")
	}
	if _, err := master.Write([]byte{before.Cc[unix.VINTR]}); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Errorf("Ctrl+C: expected exit 130, got %v", err)
	}
	if got := termios(); got != before {
		t.Errorf("terminal settings were altered after Ctrl+C: got %+v, want %+v", got, before)
	}
}
