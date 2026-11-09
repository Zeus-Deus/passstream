package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/passstream/passstream/internal/host/proto"
)

func runFeature(cmd string, argv []string, env *Env) (error, bool) {
	jsonOut := false
	for _, a := range argv {
		if a == "--json" {
			jsonOut = true
		}
	}
	args := map[string]any{}
	pos := []string{}
	output := ""
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--json" {
			continue
		}
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		k, v, has := strings.Cut(a, "=")
		allowed := cmd == "frame" && (k == "--after" || k == "--wait" || k == "--out" || k == "--max-width") || cmd == "record" && (k == "--duration" || k == "--max-bytes") || cmd == "publish" && (k == "--max-width" || k == "--wait")
		if !allowed {
			return usage("unknown capture flag"), jsonOut
		}
		if !has {
			i++
			if i >= len(argv) {
				return usage("%s needs a value", k), jsonOut
			}
			v = argv[i]
		}
		switch k {
		case "--after":
			if len(v) > 100 {
				return usage("invalid change ID"), jsonOut
			}
			args["after"] = v
		case "--out":
			output = v
		default:
			n, e := strconv.Atoi(v)
			if e != nil || n < 0 {
				return usage("%s needs a nonnegative integer", k), jsonOut
			}
			key := strings.ReplaceAll(strings.TrimPrefix(k, "--"), "-", "_")
			bound := 3840
			if key == "wait" {
				bound = 30
			}
			if key == "duration" {
				bound = 120
			}
			if key == "max_bytes" {
				bound = 32 << 20
			}
			if n > bound {
				return usage("%s exceeds its bound", k), jsonOut
			}
			args[key] = n
		}
	}
	op := cmd
	switch cmd {
	case "frame":
		if len(pos) != 0 {
			return usage("frame takes no positional arguments"), jsonOut
		}
		if output == "-" && jsonOut {
			return usage("--out - cannot be combined with --json"), jsonOut
		}
	case "capture-auth":
		if len(pos) != 0 {
			return usage("capture-auth takes no arguments"), jsonOut
		}
		op = "capture.auth"
	case "record":
		if len(pos) != 1 || (pos[0] != "start" && pos[0] != "stop" && pos[0] != "status") {
			return usage("record start|stop|status"), jsonOut
		}
		if pos[0] != "start" && len(args) != 0 {
			return usage("recording bounds apply only to start"), jsonOut
		}
		op = "record." + pos[0]
	case "publish":
		if len(pos) == 2 && pos[0] == "status" {
			op = "publish.status"
			args["request_id"] = pos[1]
			break
		}
		if len(pos) != 2 || (pos[0] != "snapshot" && pos[0] != "recording") || (pos[1] != "passfile" && pos[1] != "passpage") || pos[0] == "recording" && pos[1] != "passfile" {
			return usage("publish snapshot passfile|passpage or recording passfile"), jsonOut
		}
		if _, ok := args["wait"]; ok {
			return usage("--wait applies to publish status"), jsonOut
		}
		args["kind"], args["service"] = pos[0], pos[1]
	}
	cn, e := dial(env.Spawn)
	if e != nil {
		return e, jsonOut
	}
	defer cn.close()
	data, e := cn.callTimeout(op, args, 55*time.Second)
	if e != nil {
		return e, jsonOut
	}
	m, ok := data.(map[string]any)
	if !ok {
		return proto.E("internal", "malformed capture result"), jsonOut
	}
	if cmd == "frame" && output != "" && m["status"] == "frame" {
		s, _ := m["data"].(string)
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return proto.E("internal", "malformed PNG"), jsonOut
		}
		if output == "-" {
			_, err = env.Stdout.Write(b)
			return err, jsonOut
		}
		// Exclusive create preserves existing files and refuses symlinks.
		f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return proto.E("storage", "cannot create capture output"), jsonOut
		}
		_, err = f.Write(b)
		ce := f.Close()
		if err != nil || ce != nil {
			return proto.E("storage", "cannot write capture output"), jsonOut
		}
		delete(m, "data")
		m["out"] = output
	}
	human := ""
	if u, ok := m["url"].(string); ok {
		human = u
	} else if cmd == "capture-auth" {
		human = fmt.Sprintf("%s\n%s", m["origin"], m["token"])
	} else {
		human = fmt.Sprint(m["status"])
		if id, ok := m["request_id"].(string); ok {
			human += " " + id
			if cmd == "publish" && m["status"] == "pending" {
				human += " · owner confirmation required in the sender window"
			}
		}
		if failure, ok := m["error"].(map[string]any); ok {
			human += " · " + fmt.Sprint(failure["code"])
		}
		if id, ok := m["id"].(string); ok {
			human += " " + id
		}
		if cmd == "record" {
			human = fmt.Sprint(m)
		}
	}
	out(env, &opts{json: jsonOut}, m, human)
	return nil, jsonOut
}
