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
	if os.Getenv(helper) == "1" {
		os.Exit(Main([]string{"share", "--pin", "passphrase"}))
	}
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
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
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

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSecretPromptRestoresEchoAfterInterrupt$")
	cmd.Env = append(os.Environ(), helper+"=1")
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
	const prompt = "Passphrase: "
	buf := make([]byte, len(prompt))
	if _, err := io.ReadFull(master, buf); err != nil || string(buf) != prompt {
		t.Fatalf("waiting for prompt: read %q, error %v", buf, err)
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
