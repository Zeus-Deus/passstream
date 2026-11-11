// Package cli is the `passstream` command for local agents and people
// (CONTRACT §12, HOST-CONTROL §8.1). Each command maps to one daemon op.
// Secrets never come from argv: PIN values come from a no-echo prompt,
// --pin-stdin or --pin-fd.
package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/passstream/passstream/internal/host/hostcmd"
	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/host/serverapi"
)

// Exit codes (CONTRACT §12).
const (
	exitOK          = 0
	exitError       = 1
	exitUsage       = 2
	exitUnreachable = 3
	exitPort        = 4
)

// Env is the process environment, replaceable in tests.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// IsTTY reports whether stdin is a terminal.
	IsTTY func() bool
	// ReadSecret reads a no-echo line from the terminal.
	ReadSecret func(prompt string) (string, error)
	// OpenFD opens an inherited file descriptor for --pin-fd.
	OpenFD func(fd int) (io.ReadCloser, error)
	// Spawn allows starting a self-bound daemon when none answers.
	Spawn bool
	Sleep func(time.Duration)
	// Clipboard is wl-copy/wl-paste and the delayed clear; nil is the real one.
	Clipboard Clipboard
	// ClearAfter is the clipboard clear delay (Appendix A #20); 0 is 30 s.
	ClearAfter time.Duration
}

func defaultEnv() *Env {
	return &Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		IsTTY: func() bool { return isTTY(0) }, ReadSecret: readSecretTTY,
		OpenFD: func(fd int) (io.ReadCloser, error) { return os.NewFile(uintptr(fd), "pin-fd"), nil },
		Spawn:  true, Sleep: time.Sleep,
	}
}

func isTTY(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

// readSecretTTY prompts on /dev/tty with echo off.
func readSecretTTY(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer tty.Close()
	fd := int(tty.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	nw := *old
	nw.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &nw); err != nil {
		return "", err
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, old)
	fmt.Fprint(tty, prompt)
	line, err := bufio.NewReader(tty).ReadString('\n')
	fmt.Fprintln(tty)
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// usageError is exit code 2. Its message never contains the offending
// value when that value might be a secret.
type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func usage(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// opts are the parsed flags common to every command.
type opts struct {
	json      bool
	wait      bool
	noWait    bool
	timeout   time.Duration
	pinPolicy *proto.PinPolicy
	pinStdin  bool
	pinFD     int
	knock     bool
	label     *string
	mode      string
	noSafe    bool
	lock      bool
	rotatePin bool
	keepPin   bool
	reveal    bool
	code      string
	codeSet   bool
	force     bool
	actionID  string
	pos       []string
}

// parse reads flags anywhere among the arguments. A value-looking --pin is
// refused without echoing it (CONTRACT §12).
func parse(args []string) (*opts, error) {
	o := &opts{pinFD: -1, timeout: 120 * time.Second}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := a, "", false
		if strings.HasPrefix(a, "--") {
			if k, v, ok := strings.Cut(a, "="); ok {
				name, val, hasVal = k, v, true
			}
		} else if strings.HasPrefix(a, "-") && a != "-" {
			return nil, usage("unknown flag %s", clip(a))
		}
		next := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", usage("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		boolFlag := func(p *bool) error {
			if hasVal {
				return usage("%s takes no value", name)
			}
			*p = true
			return nil
		}
		var err error
		switch name {
		case "--json":
			err = boolFlag(&o.json)
		case "--wait":
			err = boolFlag(&o.wait)
		case "--no-wait":
			err = boolFlag(&o.noWait)
		case "--pin-stdin":
			err = boolFlag(&o.pinStdin)
		case "--knock":
			err = boolFlag(&o.knock)
		case "--lock":
			err = boolFlag(&o.lock)
		case "--rotate-pin":
			err = boolFlag(&o.rotatePin)
		case "--keep-pin":
			err = boolFlag(&o.keepPin)
		case "--reveal":
			err = boolFlag(&o.reveal)
		case "--force":
			err = boolFlag(&o.force)
		case "--action-id":
			v, e := next()
			if e != nil {
				return nil, e
			}
			if !serverapi.ValidActionID(v) {
				return nil, usage("--action-id is 1-64 characters of A-Z a-z 0-9 _ -")
			}
			o.actionID = v
		case "--motion":
			o.mode = "motion"
		case "--no-safe":
			err = boolFlag(&o.noSafe)
		case "--pin":
			v, e := next()
			if e != nil {
				return nil, e
			}
			p, ok := hostcmd.ParsePolicyName(v)
			if !ok {
				// Never echo it: it may be a PIN or a passphrase.
				return nil, usage("--pin names a policy (4, 6, none or passphrase), never a value; pass a value with --pin-stdin or --pin-fd")
			}
			o.pinPolicy = &p
		case "--pin-fd":
			v, e := next()
			if e != nil {
				return nil, e
			}
			n, e := strconv.Atoi(v)
			if e != nil || n < 0 {
				return nil, usage("--pin-fd needs a file descriptor number")
			}
			o.pinFD = n
		case "--timeout":
			v, e := next()
			if e != nil {
				return nil, e
			}
			d, e := time.ParseDuration(v)
			if e != nil {
				if n, e2 := strconv.Atoi(v); e2 == nil {
					d, e = time.Duration(n)*time.Second, nil
				}
			}
			if e != nil || d <= 0 {
				return nil, usage("--timeout needs a duration such as 120s")
			}
			o.timeout = d
		case "--label":
			v, e := next()
			if e != nil {
				return nil, e
			}
			o.label = &v
		case "--code":
			v, e := next()
			if e != nil {
				return nil, e
			}
			o.code, o.codeSet = v, true
		default:
			if strings.HasPrefix(a, "--") {
				return nil, usage("unknown flag %s", clip(name))
			}
			o.pos = append(o.pos, a)
		}
		if err != nil {
			return nil, err
		}
	}
	if o.pinStdin && o.pinFD >= 0 {
		return nil, usage("use --pin-stdin or --pin-fd, not both")
	}
	return o, nil
}

func clip(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}

// NormalizeCode trims and upper-cases ASCII letters, then requires exactly
// three row-code characters (CONTRACT §12).
func NormalizeCode(s string) (string, bool) {
	s = strings.TrimSpace(s)
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b), proto.IsRowCode(string(b))
}

const help = `passstream: private live screen sharing (local commands talk to the host daemon)

  passstream share [--pin 4|6|none|passphrase] [--pin-stdin|--pin-fd N] [--knock]
                   [--label L] [--motion] [--no-safe] [--no-wait] [--timeout 120s] [--json]
  passstream stop | pause | resume | lock | unlock
  passstream safe on|off              (redact sensitive data; on by default)
  passstream curtain on|off           (privacy curtain: the pause latch)
  passstream annotations on|off|clear (viewers may draw; host-only by default)
  passstream unfreeze [--keep-pin] [--pin-stdin|--pin-fd N] [--wait]
  passstream status | viewers [--json]
  passstream link [--json]
  passstream kick <admission_id> [--lock] [--rotate-pin [--pin-stdin|--pin-fd N]] [--wait]
  passstream ban <connection_id> [--action-id ID]
  passstream knock approve <knock_id> --code K7P | knock deny <knock_id>
  passstream pin [--reveal] [--json] | pin rotate | pin none | pin set 4|6|passphrase
             (PIN changes take [--pin-stdin|--pin-fd N] [--wait] [--timeout])
  passstream copy link | copy pin
  passstream frame [--after ID] [--wait 30] [--max-width N] [--out file.png|-] [--json]
  passstream record start [--duration 120] [--max-bytes 33554432] | record stop|status [--json]
  passstream publish snapshot passfile|passpage [--max-width N] [--json]
  passstream publish recording passfile [--json]
  passstream capture-auth [--json]
  passstream shutdown [--force]
  passstream host [--server URL] [--detach] | host install-units [--no-enable]

PINs and passphrases never go in argv. pin --reveal erases its line after 10 s
on a terminal, but scrollback and terminal logs may keep it. copy marks the
value sensitive for clipboard managers and clears it after 30 s unless
something newer replaced it; a clipboard history may already have stored it.
Ban, kick, unfreeze, PIN changes and knock approval use one action id per action
(pass --action-id to reuse one across runs). Ban retries transient failures.
Exit codes: 0 ok, 1 error, 2 usage, 3 daemon not reachable, 4 port_not_owned.
`

// Main runs the CLI and returns the exit code.
func Main(args []string) int { return Run(args, defaultEnv()) }

// Run is Main with an explicit environment.
func Run(args []string, env *Env) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(env.Stdout, help)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	cmd := args[0]
	if cmd == "frame" || cmd == "record" || cmd == "publish" || cmd == "capture-auth" {
		err, jsonOut := runFeature(cmd, args[1:], env)
		return finish(err, jsonOut, env)
	}
	if cmd == serveCommand {
		return clipboardServe(args[1:], env)
	}
	o, err := parse(args[1:])
	if err == nil {
		err = run(cmd, o, env)
	}
	jsonOut := o != nil && o.json
	if o == nil {
		// The flags didn't parse: --json still asks for a JSON error.
		for _, a := range args[1:] {
			jsonOut = jsonOut || a == "--json"
		}
	}
	return finish(err, jsonOut, env)
}

func finish(err error, jsonOut bool, env *Env) int {
	if err == nil {
		return exitOK
	}
	var u usageError
	if errors.As(err, &u) {
		if jsonOut {
			printErr(env, true, proto.E("invalid_argument", "%s", u.msg))
		} else {
			fmt.Fprintln(env.Stderr, "passstream:", u.msg)
		}
		return exitUsage
	}
	if errors.Is(err, errUnreachable) {
		printErr(env, jsonOut, proto.E("daemon_unreachable", "the passstream daemon isn't reachable (passstream host)"))
		return exitUnreachable
	}
	e := proto.AsError(err)
	if _, ok := err.(*proto.Error); !ok {
		e = proto.E("internal", "%s", err.Error())
	}
	printErr(env, jsonOut, e)
	if e.Code == "port_not_owned" {
		return exitPort
	}
	return exitError
}

func printErr(env *Env, jsonOut bool, e *proto.Error) {
	if jsonOut {
		b, _ := json.Marshal(map[string]any{"error": e})
		fmt.Fprintln(env.Stderr, string(b))
		return
	}
	fmt.Fprintf(env.Stderr, "passstream: %s (%s)\n", e.Message, e.Code)
}

func out(env *Env, o *opts, data any, human string) {
	if o.json {
		b, _ := json.Marshal(data)
		fmt.Fprintln(env.Stdout, string(b))
		return
	}
	if human != "" {
		fmt.Fprintln(env.Stdout, human)
	}
}

func wantPos(o *opts, n int, what string) error {
	if len(o.pos) != n {
		return usage("expected %s", what)
	}
	return nil
}

// readPin reads a value from --pin-stdin, --pin-fd, or a no-echo prompt
// (only when required and stdin is a terminal). It returns nil when none
// was asked for.
func readPin(o *opts, env *Env, required bool, prompt string) (*string, error) {
	var s string
	switch {
	case o.pinStdin:
		line, err := bufio.NewReader(io.LimitReader(env.Stdin, 4096)).ReadString('\n')
		if err != nil && line == "" {
			return nil, usage("--pin-stdin: no line on stdin")
		}
		s = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	case o.pinFD >= 0:
		f, err := env.OpenFD(o.pinFD)
		if err != nil {
			return nil, usage("--pin-fd: can't read that descriptor")
		}
		line, err := bufio.NewReader(io.LimitReader(f, 4096)).ReadString('\n')
		f.Close()
		if err != nil && line == "" {
			return nil, usage("--pin-fd: nothing to read")
		}
		s = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	case required && env.IsTTY():
		v, err := env.ReadSecret(prompt)
		if err != nil {
			return nil, usage("can't read the value from the terminal")
		}
		s = v
	case required:
		return nil, usage("a value is needed: use --pin-stdin or --pin-fd")
	default:
		return nil, nil
	}
	if s == "" {
		return nil, usage("empty value")
	}
	return &s, nil
}

func run(cmd string, o *opts, env *Env) error {
	switch cmd {
	case "host":
		return usage("passstream host is handled by main")
	case "share", "start":
		return share(o, env)
	}
	// Validate everything that needs no daemon before connecting.
	type plan struct {
		op   string
		args map[string]any
		pin  bool // a PIN change: --wait and storage{pending} handling
	}
	var p plan
	switch cmd {
	case "stop", "pause", "resume", "lock", "unlock", "status", "viewers", "link", "shutdown":
		if err := wantPos(o, 0, "no arguments"); err != nil {
			return err
		}
		p.op, p.args = cmd, map[string]any{}
		switch cmd {
		case "lock":
			p.args["locked"] = true
		case "unlock":
			p.op, p.args["locked"] = "lock", false
		case "viewers":
			p.op = "status"
		case "shutdown":
			if o.force {
				p.args["force"] = true
			}
		}
	case "curtain":
		// The privacy curtain is the pause latch: on = pause (allowed from anywhere), off = resume (local only).
		if err := wantPos(o, 1, "on or off"); err != nil {
			return err
		}
		switch o.pos[0] {
		case "on":
			p.op = "pause"
		case "off":
			p.op = "resume"
		default:
			return usage("curtain is on or off")
		}
		p.args = map[string]any{}
	case "annotations":
		if err := wantPos(o, 1, "on, off or clear"); err != nil {
			return err
		}
		switch o.pos[0] {
		case "on", "off":
			p.op, p.args = "annotations", map[string]any{"viewers": o.pos[0] == "on"}
		case "clear":
			p.op, p.args = "annotations.clear", map[string]any{}
		default:
			return usage("annotations is on, off or clear")
		}
	case "mode":
		if err := wantPos(o, 1, "text or motion"); err != nil {
			return err
		}
		if o.pos[0] != "text" && o.pos[0] != "motion" {
			return usage("mode is text or motion")
		}
		p.op, p.args = "mode", map[string]any{"mode": o.pos[0]}
	case "safe":
		// Safe mode on is restrictive (allowed from anywhere); off is local only.
		if err := wantPos(o, 1, "on or off"); err != nil {
			return err
		}
		if o.pos[0] != "on" && o.pos[0] != "off" {
			return usage("safe is on or off")
		}
		p.op, p.args = "safe", map[string]any{"on": o.pos[0] == "on"}
	case "unfreeze":
		if err := wantPos(o, 0, "no arguments"); err != nil {
			return err
		}
		p.op, p.args, p.pin = "unfreeze", map[string]any{}, !o.keepPin
		if o.keepPin {
			p.args["keep_pin"] = true
			if o.pinStdin || o.pinFD >= 0 {
				return usage("--keep-pin takes no PIN value")
			}
		}
	case "kick":
		if err := wantPos(o, 1, "an admission id"); err != nil {
			return err
		}
		if !proto.IsId22(o.pos[0]) {
			return usage("an admission id is 22 characters (use admission_ids[0] from viewers)")
		}
		p.op, p.args, p.pin = "kick", map[string]any{"admission_id": o.pos[0]}, o.rotatePin
		if o.lock {
			p.args["lock"] = true
		}
		if o.rotatePin {
			p.args["rotate_pin"] = true
		} else if o.pinStdin || o.pinFD >= 0 {
			return usage("a PIN value needs --rotate-pin")
		}
	case "ban":
		if err := wantPos(o, 1, "a connection id"); err != nil {
			return err
		}
		if !proto.IsConnID(o.pos[0]) {
			return usage("a connection id looks like c_…")
		}
		// One action id per ban, reused on retries: the server deduplicates.
		aid := o.actionID
		if aid == "" {
			aid = "ban-" + proto.NewId22()
		}
		p.op, p.args = "ban", map[string]any{"connection_id": o.pos[0], "action_id": aid}
	case "knock":
		if len(o.pos) != 2 || (o.pos[0] != "approve" && o.pos[0] != "deny") {
			return usage("knock approve <knock_id> --code K7P | knock deny <knock_id>")
		}
		if !proto.IsId22(o.pos[1]) {
			return usage("a knock id is 22 characters")
		}
		if o.pos[0] == "deny" {
			p.op, p.args = "knock.deny", map[string]any{"knock_id": o.pos[1]}
			break
		}
		if !o.codeSet {
			return usage("knock approve needs --code: ask the person which code their screen shows")
		}
		code, ok := NormalizeCode(o.code)
		if !ok {
			return usage("--code is the three-character code the viewer sees, e.g. K7P")
		}
		p.op, p.args = "knock.approve", map[string]any{"knock_id": o.pos[1], "row_code": code}
	case "pin":
		return pinCmd(o, env)
	case "copy":
		return copyCmd(o, env)
	default:
		return usage("unknown command %q (passstream help)", clip(cmd))
	}
	if p.pin {
		v, err := readPin(o, env, false, "")
		if err != nil {
			return err
		}
		if v != nil {
			p.args["pin"] = *v
		}
	}
	if p.op == "kick" || p.op == "unfreeze" || p.op == "knock.approve" {
		setActionID(o, p.args)
	}
	cn, err := dial(env.Spawn)
	if err != nil {
		return err
	}
	defer cn.close()
	if p.pin {
		return pinChange(cn, o, env, p.op, p.args)
	}
	data, err := cn.call(p.op, p.args)
	for try := 1; p.op == "ban" && try < 3 && retryable(err); try++ {
		env.Sleep(time.Second)
		data, err = cn.call(p.op, p.args)
	}
	if err != nil {
		return err
	}
	switch cmd {
	case "status":
		out(env, o, data, humanStatus(data))
	case "viewers":
		var list any = map[string]any{"viewers": []any{}}
		if s, ok := field(data, "stream").(map[string]any); ok {
			list = map[string]any{"watching": s["watching"], "knocking": s["knocking"], "viewers": s["viewers"]}
		}
		out(env, o, list, humanViewers(list))
	case "link":
		out(env, o, data, str(field(data, "link")))
	case "kick":
		out(env, o, data, "kicked "+strings.Join(strs(field(data, "admission_ids")), ", "))
	case "ban":
		out(env, o, data, "banned; kicked "+strings.Join(strs(field(data, "admission_ids")), ", "))
	case "knock":
		if p.op == "knock.approve" {
			out(env, o, data, "admitted "+str(field(data, "admission_id")))
		} else {
			out(env, o, data, "denied")
		}
	default:
		out(env, o, data, "ok")
	}
	return nil
}

// retryable errors may be transient, and the command may or may not have
// applied: only commands with an action id are retried.
func retryable(err error) bool {
	if err == nil || errors.Is(err, errUnreachable) {
		return false
	}
	c := proto.AsError(err).Code
	return c == "server_unreachable" || c == "sender_timeout"
}

func field(v any, k string) any {
	if m, ok := v.(map[string]any); ok {
		return m[k]
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	a, _ := v.([]any)
	var out []string
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// pinChange sends a PIN change with the retry rules of CONTRACT §5.4/§12.
func setActionID(o *opts, args map[string]any) {
	if _, ok := args["action_id"]; !ok {
		id := o.actionID
		if id == "" {
			id = "cli-" + proto.NewId22()
		}
		args["action_id"] = id
	}
}

func pinChange(cn *conn, o *opts, env *Env, op string, args map[string]any) error {
	setActionID(o, args)
	deadline := time.Now().Add(o.timeout)
	for {
		data, err := cn.call(op, args)
		if err == nil {
			out(env, o, data, "PIN changed")
			return nil
		}
		e := proto.AsError(err)
		if e.Code == "storage" && e.Details["pending"] == true {
			out(env, o, map[string]any{"pending": true}, "PIN change accepted, still being applied")
			return nil
		}
		if e.Code != "rotation_pending" || !o.wait || time.Now().After(deadline) {
			return err
		}
		// Wait until access.rotation_pending is false, then send again.
		for time.Now().Before(deadline) {
			env.Sleep(500 * time.Millisecond)
			st, err := cn.call("status", map[string]any{})
			if err != nil {
				return err
			}
			acc, _ := field(field(st, "stream"), "access").(map[string]any)
			if acc == nil || acc["rotation_pending"] != true {
				break
			}
		}
	}
}

func pinCmd(o *opts, env *Env) error {
	sub := ""
	if len(o.pos) > 0 {
		sub = o.pos[0]
	}
	var op string
	args := map[string]any{}
	switch sub {
	case "":
		cn, err := dial(env.Spawn)
		if err != nil {
			return err
		}
		defer cn.close()
		if !o.reveal {
			st, err := cn.call("status", map[string]any{})
			if err != nil {
				return err
			}
			pin, _ := field(field(field(st, "stream"), "access"), "pin").(map[string]any)
			out(env, o, map[string]any{"pin": pin}, humanPin(pin))
			return nil
		}
		data, err := cn.call("pin.reveal", map[string]any{})
		if err != nil {
			return err
		}
		if o.json {
			out(env, o, data, "")
			return nil
		}
		line := "PIN " + revealText(data)
		fmt.Fprint(env.Stdout, line)
		if f, ok := env.Stdout.(*os.File); ok && isTTY(int(f.Fd())) {
			env.Sleep(10 * time.Second)
			fmt.Fprint(env.Stdout, "\r\033[2K")
			return nil
		}
		fmt.Fprintln(env.Stdout)
		return nil
	case "rotate":
		if err := wantPos(o, 1, "pin rotate"); err != nil {
			return err
		}
		op = "pin.rotate"
		v, err := readPin(o, env, false, "")
		if err != nil {
			return err
		}
		if v != nil {
			args["pin"] = *v
		}
	case "none":
		if err := wantPos(o, 1, "pin none"); err != nil {
			return err
		}
		if o.pinStdin || o.pinFD >= 0 {
			return usage("pin none takes no value")
		}
		op, args["policy"] = "pin.set", map[string]any{"kind": "none"}
	case "set":
		if len(o.pos) != 2 {
			return usage("pin set 4|6|passphrase")
		}
		p, ok := hostcmd.ParsePolicyName(o.pos[1])
		if !ok {
			return usage("pin set names a policy (4, 6 or passphrase), never a value; pass a value with --pin-stdin or --pin-fd")
		}
		pol := map[string]any{"kind": p.Kind}
		if p.Kind == "digits" {
			pol["length"] = p.Length
		}
		op, args["policy"] = "pin.set", pol
		v, err := readPin(o, env, p.Kind == "passphrase", "New passphrase: ")
		if err != nil {
			return err
		}
		if v != nil {
			if p.Kind == "none" {
				return usage("pin set none takes no value")
			}
			args["pin"] = *v
		}
	default:
		return usage("pin [--reveal] | pin rotate | pin none | pin set 4|6|passphrase")
	}
	cn, err := dial(env.Spawn)
	if err != nil {
		return err
	}
	defer cn.close()
	return pinChange(cn, o, env, op, args)
}

func revealText(data any) string {
	pin, _ := field(data, "pin").(string)
	if pin == "" {
		pin = "(none)"
	}
	if p, ok := field(data, "pending_pin").(string); ok && p != "" {
		pin += " (pending: " + p + ")"
	}
	return pin
}

func humanPin(pin map[string]any) string {
	if pin == nil {
		return "PIN · unknown (no share, or the sender hasn't reported yet)"
	}
	switch pin["kind"] {
	case "none":
		return "PIN · none (anyone with the link can watch)"
	case "passphrase":
		return "PIN · passphrase ••••••••"
	}
	n, _ := pin["length"].(float64)
	return fmt.Sprintf("PIN · %d digits %s", int(n), strings.Repeat("•", int(n)))
}

func copyCmd(o *opts, env *Env) error {
	if len(o.pos) != 1 || (o.pos[0] != "link" && o.pos[0] != "pin") {
		return usage("copy link | copy pin")
	}
	cn, err := dial(env.Spawn)
	if err != nil {
		return err
	}
	defer cn.close()
	var value string
	if o.pos[0] == "link" {
		data, err := cn.call("link", map[string]any{})
		if err != nil {
			return err
		}
		value = str(field(data, "link"))
	} else {
		data, err := cn.call("pin.reveal", map[string]any{})
		if err != nil {
			return err
		}
		value = str(field(data, "pin"))
		if value == "" {
			return proto.E("invalid_argument", "this share has no PIN")
		}
	}
	return copyValue(o, env, o.pos[0], value)
}

// copyValue is Appendix A #20: wl-copy --sensitive over stdin, never argv,
// served for 30 s unless something newer replaces it first (see
// serveCommand). A wl-copy without --sensitive still copies, and the CLI
// says so; it never falls back silently.
func copyValue(o *opts, env *Env, what, value string) error {
	cb := env.clipboard()
	sensitive := cb.SensitiveSupported()
	after := env.clearAfter()
	cleared, err := cb.Copy(value, sensitive, after)
	if err != nil {
		return proto.E("internal", "wl-copy failed")
	}
	res := map[string]any{"copied": what, "sensitive": sensitive, "clears_in_s": nil}
	if cleared {
		res["clears_in_s"] = int(after / time.Second)
	}
	if !sensitive {
		fmt.Fprintln(env.Stderr, "passstream: warning: this wl-copy can't mark the copy sensitive (--sensitive); a clipboard manager may keep it")
	}
	if !cleared {
		fmt.Fprintln(env.Stderr, "passstream: warning: can't schedule the clipboard clear; clear it yourself")
	}
	human := "copied the " + what
	if cleared {
		human += fmt.Sprintf(" (cleared in %d s unless replaced)", int(after/time.Second))
	}
	out(env, o, res, human)
	return nil
}

// share is `start` plus waiting for live (or link_ready with --no-wait).
func share(o *opts, env *Env) error {
	if err := wantPos(o, 0, "no arguments"); err != nil {
		return err
	}
	args := map[string]any{"knock": o.knock}
	if o.label != nil {
		args["label"] = *o.label
	}
	if o.mode != "" {
		args["mode"] = o.mode
	}
	if o.noSafe {
		args["safe"] = false
	}
	if o.pinPolicy != nil {
		pol := map[string]any{"kind": o.pinPolicy.Kind}
		if o.pinPolicy.Kind == "digits" {
			pol["length"] = o.pinPolicy.Length
		}
		args["pin_policy"] = pol
		v, err := readPin(o, env, o.pinPolicy.Kind == "passphrase", "Passphrase: ")
		if err != nil {
			return err
		}
		if v != nil {
			if o.pinPolicy.Kind == "none" {
				return usage("--pin none takes no value")
			}
			args["pin"] = *v
		}
	} else if o.pinStdin || o.pinFD >= 0 {
		return usage("a PIN value needs --pin 4|6|passphrase")
	}
	cn, err := dial(env.Spawn)
	if err != nil {
		return err
	}
	defer cn.close()
	if _, err := cn.call("subscribe", map[string]any{}); err != nil {
		return err
	}
	data, err := cn.call("start", args)
	if err != nil {
		return err
	}
	id := str(field(data, "stream_id"))
	want := "live"
	if o.noWait {
		want = "link_ready"
	}
	timer := time.NewTimer(o.timeout)
	defer timer.Stop()
	for {
		select {
		case ev := <-cn.events:
			if str(field(ev.Data, "stream_id")) != id {
				continue
			}
			if ev.Event == "ended" {
				return proto.E("stream_ended", "the share ended (%s)", str(field(ev.Data, "reason")))
			}
			if ev.Event == want || (want == "link_ready" && ev.Event == "live") {
				ld, err := cn.call("link", map[string]any{})
				if err != nil {
					return err
				}
				link := str(field(ld, "link"))
				out(env, o, map[string]any{"stream_id": id, "link": link, "live": ev.Event == "live"}, link)
				return nil
			}
		case <-cn.closed:
			return errUnreachable
		case <-timer.C:
			cn.call("stop", map[string]any{})
			return proto.E("picker_timeout", "nothing was shared within %s; the share was stopped", o.timeout)
		}
	}
}

func humanStatus(data any) string {
	d, _ := field(data, "daemon").(map[string]any)
	lines := []string{fmt.Sprintf("daemon %v · server %v (%v) · key %v · port %v (%v)", d["version"], d["server_url"], d["server"], d["key"], d["port_owner"], d["port_mode"])}
	s, ok := field(data, "stream").(map[string]any)
	if !ok {
		return strings.Join(append(lines, "no share"), "\n")
	}
	lines = append(lines, fmt.Sprintf("stream %v · %v · %v · watching %v, knocking %v", s["id"], s["phase"], s["attachment"], s["watching"], s["knocking"]))
	if acc, ok := s["access"].(map[string]any); ok {
		pin, _ := acc["pin"].(map[string]any)
		l, _ := acc["latches"].(map[string]any)
		lines = append(lines, humanPin(pin)+fmt.Sprintf(" · locked %v paused %v frozen %v · knock %v", l["locked"], l["paused"], l["frozen"], acc["knock"]))
	}
	return strings.Join(lines, "\n")
}

func humanViewers(list any) string {
	vs, _ := field(list, "viewers").([]any)
	if len(vs) == 0 {
		return "no viewers"
	}
	var lines []string
	for _, v := range vs {
		m, _ := v.(map[string]any)
		code, _ := m["row_code"].(string)
		ids := strs(m["admission_ids"])
		first := ""
		if len(ids) > 0 {
			first = ids[0]
		}
		if k, ok := m["knock_id"].(string); ok {
			first = "knock " + k
		}
		lines = append(lines, fmt.Sprintf("%-12v %-4s %s", m["status"], code, first))
	}
	return strings.Join(lines, "\n")
}
