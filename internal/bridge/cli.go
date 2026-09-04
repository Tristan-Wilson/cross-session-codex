package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"time"
)

const usage = `cross-session-codex — local Claude/Codex messaging

  install                         Install the binary and optional Codex skill
  launch [--resume UUID]           Launch Codex on the shared app-server
  shutdown [--check]               Stop the shared server after closing all sessions
  start [--name NAME]              Enable the current exact thread (alias: enable)
  status | disable | identity      Inspect or stop this thread's worker
  list                            Discover local peers
  send PEER --body-file PATH       Send to an authorized peer
  reply INBOX_ID --body-file PATH  Reply to a received message
  read [--state held] [--after ID]  Read a repeatable, bounded inbox page
  ack ID...                       Acknowledge messages after reading
  release ID... | decline ID...    Decide held messages
  wait [--timeout 20] | sent       Wait for messages or inspect sent status
  version                         Print runtime/build versions
  mcp | hook                      Optional host integration

Stateful commands accept --thread UUID (defaults to CODEX_THREAD_ID).
Send/reply also accept --body TEXT; --body-file - reads stdin.
Use COMMAND --help for options. No Python or tmux is required.
`

func Main(args []string, in io.Reader, out, errOut io.Writer) int {
	result, err := runCLI(args, in, out, errOut)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		_ = json.NewEncoder(errOut).Encode(Object{"error": err.Error()})
		return 1
	}
	if result != nil {
		e := json.NewEncoder(out)
		e.SetEscapeHTML(false)
		e.SetIndent("", "  ")
		if err = e.Encode(result); err != nil {
			_, _ = fmt.Fprintln(errOut, err)
			return 1
		}
	}
	return 0
}

// Go's flag package stops at the first positional argument. Preserve the old
// CLI's options-after-target syntax, including literal values beginning with '-'.
func parseFlags(fs *flag.FlagSet, args []string) error {
	options, positionals := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		options = append(options, arg)
		name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		f := fs.Lookup(name)
		if strings.Contains(arg, "=") || f == nil {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	return fs.Parse(append(append(options, "--"), positionals...))
}
func runCLI(args []string, in io.Reader, out, errOut io.Writer) (Object, error) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, usage)
		return nil, err
	}
	command := args[0]
	args = args[1:]
	if command == "version" {
		return Object{"name": "cross-session-codex", "version": Version, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH}, nil
	}
	if command == "worker" {
		if len(args) != 1 {
			return nil, errors.New("worker requires its state directory")
		}
		return nil, RunWorker(args[0])
	}
	if command == "mcp" {
		return nil, serveMCP(in, out)
	}
	if command == "hook" {
		var event Object
		b, err := io.ReadAll(io.LimitReader(in, MaxBuffer+1))
		if err != nil {
			return nil, err
		}
		if len(b) > MaxBuffer {
			return nil, errors.New("hook event too large")
		}
		if err = json.Unmarshal(b, &event); err != nil {
			return nil, err
		}
		return handleHook(event)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	if command == "shutdown" {
		socket := fs.String("app-server-socket", defaultAppSocket(), "Shared app-server Unix socket")
		check := fs.Bool("check", false, "Report whether shutdown is possible without stopping anything")
		if err := parseFlags(fs, args); err != nil {
			return nil, err
		}
		if fs.NArg() != 0 {
			return nil, errors.New("unexpected shutdown arguments")
		}
		if !*check && os.Getenv("CODEX_THREAD_ID") != "" {
			return nil, errors.New("run shutdown in your terminal after exiting all Codex sessions; use shutdown --check to inspect blockers")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return Shutdown(ctx, *socket, *check)
	}
	if command == "install" {
		opts := defaultInstallOptions()
		fs.StringVar(&opts.Prefix, "prefix", opts.Prefix, "Release directory")
		fs.StringVar(&opts.BinDir, "bin-dir", opts.BinDir, "Installed command directory")
		fs.StringVar(&opts.SkillDir, "skill-dir", opts.SkillDir, "Codex skill directory")
		fs.BoolVar(&opts.NoSkill, "no-skill", false, "Skip skill installation")
		if err := parseFlags(fs, args); err != nil {
			return nil, err
		}
		if fs.NArg() != 0 {
			return nil, errors.New("unexpected install arguments")
		}
		return Install(opts)
	}
	if command == "launch" {
		opts := LaunchOptions{Codex: "codex"}
		for i, arg := range args {
			if arg == "--" {
				opts.ClientArgs = append([]string{}, args[i+1:]...)
				args = args[:i]
				break
			}
		}
		fs.StringVar(&opts.Resume, "resume", "", "Exact thread UUID to resume after exiting its old UI")
		fs.StringVar(&opts.Name, "name", "", "Peer name")
		fs.StringVar(&opts.Inbound, "inbound", "", "accept (default), parity, hold or refuse")
		fs.StringVar(&opts.Permission, "permission-class", "", "Advertised messaging class: bypass (default) or prompting; independent of Codex approvals")
		fs.StringVar(&opts.Socket, "app-server-socket", "", "Explicit existing app-server Unix socket")
		fs.StringVar(&opts.CWD, "cwd", "", "Project directory")
		fs.StringVar(&opts.Codex, "codex", "codex", "Codex executable")
		if err := parseFlags(fs, args); err != nil {
			return nil, err
		}
		if fs.NArg() != 0 {
			return nil, errors.New("unexpected launch arguments; Codex client options go after --")
		}
		return nil, Launch(opts)
	}
	if command == "identity" {
		return Object{"thread_id": envOr("CODEX_THREAD_ID", os.Getenv("CODEX_SESSION_ID"))}, nil
	}
	if command == "list" {
		if len(args) != 0 {
			return nil, errors.New("list takes no arguments")
		}
		peers, err := Discover("", 0)
		return Object{"sessions": peers}, err
	}
	thread := fs.String("thread", os.Getenv("CODEX_THREAD_ID"), "Current Codex thread UUID")
	var name, inbound, permission, delivery, socket, cwd, body, bodyFile, state, after, messageID string
	var limit int
	var timeout float64
	switch command {
	case "start", "enable":
		fs.StringVar(&name, "name", "", "Peer name")
		fs.StringVar(&inbound, "inbound", "", "accept (default), parity, hold or refuse")
		fs.StringVar(&permission, "permission-class", "", "Advertised messaging class: bypass (default) or prompting; independent of Codex approvals")
		fs.StringVar(&delivery, "delivery", "", "app-server (default) or manual")
		fs.StringVar(&socket, "app-server-socket", "", "Codex app-server Unix socket")
		fs.StringVar(&cwd, "cwd", "", "Project directory")
	case "send", "reply":
		fs.StringVar(&body, "body", "", "Exact message text")
		fs.StringVar(&bodyFile, "body-file", "", "UTF-8 file, or - for stdin")
	case "read":
		fs.StringVar(&state, "state", "unread", "unread, held, read, declined or expired")
		fs.StringVar(&after, "after", "", "Previous page's next_after inbox ID")
		fs.IntVar(&limit, "limit", 20, "Maximum message count (1–50)")
	case "wait":
		fs.Float64Var(&timeout, "timeout", 20, "Wait up to 25 seconds")
	case "sent":
		fs.StringVar(&messageID, "message-id", "", "Outgoing message ID")
	case "status", "disable", "ack", "release", "decline":
	default:
		return nil, fmt.Errorf("unknown command %q; use --help", command)
	}
	if err := parseFlags(fs, args); err != nil {
		return nil, err
	}
	id, err := canonicalThread(*thread)
	if err != nil {
		return nil, err
	}
	params := Object{}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	if command == "send" || command == "reply" {
		if fs.NArg() != 1 {
			return nil, errors.New("send/reply requires exactly one target or inbox ID")
		}
		if visited["body"] == visited["body-file"] {
			return nil, errors.New("supply exactly one of --body or --body-file")
		}
		if visited["body-file"] {
			reader := in
			if bodyFile != "-" {
				f, e := os.Open(bodyFile)
				if e != nil {
					return nil, e
				}
				defer closeQuietly(f)
				reader = f
			}
			b, e := io.ReadAll(io.LimitReader(reader, MaxBuffer+1))
			if e != nil {
				return nil, e
			}
			if len(b) > MaxBuffer {
				return nil, errors.New("message body exceeds size limit")
			}
			body = string(b)
		}
		params["body"] = body
		key := "target"
		if command == "reply" {
			key = "message_id"
		}
		params[key] = fs.Arg(0)
	} else if command == "ack" || command == "release" || command == "decline" {
		if fs.NArg() == 0 || fs.NArg() > 100 {
			return nil, errors.New("provide 1–100 inbox IDs")
		}
		params["ids"] = fs.Args()
	} else if fs.NArg() != 0 {
		return nil, errors.New("unexpected positional arguments")
	}
	switch command {
	case "start", "enable":
		for k, v := range map[string]string{"name": name, "inbound": inbound, "permission_class": permission, "delivery": delivery, "app_server_socket": socket, "cwd": cwd} {
			flagName := strings.ReplaceAll(k, "_", "-")
			if visited[flagName] {
				params[k] = v
			}
		}
		return Enable(id, params)
	case "status":
		command = "info"
	case "disable":
		return Object{"stopped": true}, Stop(id)
	case "read":
		if limit < 1 || limit > 50 {
			return nil, errors.New("limit must be between 1 and 50")
		}
		params["state"] = state
		params["limit"] = limit
		if after != "" {
			params["after"] = after
		}
	case "wait":
		if math.IsNaN(timeout) || math.IsInf(timeout, 0) || timeout < 0 || timeout > 25 {
			return nil, errors.New("timeout must be between 0 and 25 seconds")
		}
		params["timeout"] = timeout
	case "sent":
		if messageID != "" {
			params["message_id"] = messageID
		}
	}
	return RPC(id, command, params)
}
