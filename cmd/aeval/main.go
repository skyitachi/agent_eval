// Command aeval runs evaluations and inspects their results.
//
// # What it is for
//
// The tool answers three questions and refuses to answer a fourth:
//
//   - how well did an agent do on a dataset?  (aeval run)
//   - which of harness, model and skill is responsible for a difference?
//     (aeval matrix)
//   - what did a real session cost, so a re-run can be measured against it?
//     (aeval collect)
//
// It does not answer "is this agent good". That question needs a dataset whose
// tasks have ground truth and a reader who knows what the numbers are for, and
// no command can supply either.
//
// # Exit codes
//
//	0   success
//	1   the operation failed (bad dataset, a run that could not be carried out)
//	2   the command line itself was wrong
//	130 interrupted
//
// A run where tasks failed is *not* an error: a resolve rate below 1 is the
// normal outcome of an evaluation, not a malfunction, and conflating the two
// would make the tool unusable in CI. Only a run that could not be carried out
// reports failure.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/skyitachi/agenteval/internal/agent"
	"github.com/skyitachi/agenteval/internal/attrib"
	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/machine"
	"github.com/skyitachi/agenteval/internal/replay"
	"github.com/skyitachi/agenteval/internal/report"
	"github.com/skyitachi/agenteval/internal/runner"
	"github.com/skyitachi/agenteval/internal/server"
	"github.com/skyitachi/agenteval/internal/session"
	"github.com/skyitachi/agenteval/internal/store"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
	exitCtlC  = 130

	// defaultDB is the database a command uses when --db is not given. One file
	// in the working directory rather than a directory under $HOME, because a
	// run's results belong next to the dataset they came from.
	defaultDB = "aeval.db"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "collect":
		return cmdCollect(rest, stdout, stderr)
	case "validate":
		return cmdValidate(rest, stdout, stderr)
	case "run":
		return cmdRun(rest, stdout, stderr)
	case "matrix":
		return cmdMatrix(rest, stdout, stderr)
	case "report":
		return cmdReport(rest, stdout, stderr)
	case "serve":
		return cmdServe(rest, stdout, stderr)
	case "list", "ls":
		return cmdList(rest, stdout, stderr)
	case "version", "--version", "-v":
		return cmdVersion(rest, stdout, stderr)
	case "help", "--help", "-h":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "aeval: unknown command %q\n\n", cmd)
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `aeval evaluates an agent harness, a model and a skill, one axis at a time.

usage: aeval <command> [flags]

  collect   turn recorded agent sessions into a dataset with baselines
  validate  check a dataset without running anything
  run       run a dataset against one agent
  matrix    sweep one axis, or attribute a sweep that already ran
  report    render an HTML report, comparison, attribution or run list
  serve     serve recorded reports, and this machine's sessions with a replay button, over HTTP
  list      list recorded runs
  version   print version and schema information

Every command accepts --db <path> for the SQLite file (default `+defaultDB+`).
Run "aeval <command> -h" for the flags of one command.

Exit codes: 0 success, 1 operation failed, 2 bad command line, 130 interrupted.
A run in which tasks failed exits 0: that is a measurement, not a malfunction.
`)
}

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

// commonFlags are accepted by every command that opens the database.
type commonFlags struct {
	db string
}

func addCommon(fs *flag.FlagSet, c *commonFlags) {
	fs.StringVar(&c.db, "db", defaultDB, "SQLite database holding runs and results")
}

func openStore(c *commonFlags) (*store.Store, error) {
	if strings.TrimSpace(c.db) == "" {
		return nil, errors.New("aeval: --db may not be empty")
	}
	return store.Open(c.db)
}

// signalContext cancels on SIGINT/SIGTERM. A cancelled run records what it
// managed to measure and marks itself interrupted; it does not lose the file.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func fail(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "aeval: "+format+"\n", args...)
	return exitError
}

// usageFail reports a bad command line. It exists so that "you typed it wrong"
// and "it did not work" stay distinguishable in a script.
func usageFail(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "aeval: "+format+"\n", args...)
	return exitUsage
}

// parse runs a FlagSet and reports whether it succeeded, keeping flag's own
// error output on stderr and mapping it to the usage exit code.
func parse(fs *flag.FlagSet, args []string, stderr io.Writer) (bool, int) {
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, exitOK
		}
		return false, exitUsage
	}
	return true, exitOK
}

// absFlagPath resolves a path given on the command line against the caller's
// working directory.
//
// A value with no separator is left alone: `--bin claude` means "find claude on
// PATH", and turning it into /cwd/claude would break every invocation that does
// not spell out an absolute path. The rule itself lives in agent.ResolveCommand,
// because a matrix document resolves paths the same way against its own
// directory and the two must not drift.
func absFlagPath(p string) string {
	if p == "" {
		return p
	}
	wd, err := os.Getwd()
	if err != nil {
		return p
	}
	return agent.ResolveCommand(wd, p)
}

// absDirList resolves repeatable directory flags against the caller's working
// directory.
//
// A directory gets the resolver that always joins: there is no PATH to fall back
// on, so `--plugin-dir skills` means the skills directory here, and leaving it
// bare would point inside the sandbox workspace instead.
func absDirList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	wd, err := os.Getwd()
	if err != nil {
		// Without a base the values are returned as written; every other
		// resolver in this program degrades the same way rather than failing a
		// run over a missing environment.
		return in
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, agent.ResolveDir(wd, v))
	}
	return out
}

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// ---------------------------------------------------------------------------
// Agent flags
// ---------------------------------------------------------------------------

// agentFlags is the harness configuration: everything that selects or shapes
// the agent process without changing the model or the skills.
//
// It is a separate type because `aeval matrix --axis harness` varies exactly
// these, and a matrix document reuses the same vocabulary in its levels.
type agentFlags struct {
	kind     string
	bin      string
	model    string
	fallback string
	effort   string

	permissionMode string
	tools          stringList
	allowedTools   stringList
	deniedTools    stringList
	addDirs        stringList

	systemPrompt       string
	appendSystemPrompt string
	settings           string
	mcpServers         stringList
	pluginDirs         stringList
	extraArgs          stringList

	bare         bool
	strictMCP    bool
	disableSlash bool
	maxBudgetUSD float64
}

func addAgentFlags(fs *flag.FlagSet, a *agentFlags) {
	fs.StringVar(&a.kind, "agent", "claude_code", "agent implementation to drive")
	fs.StringVar(&a.bin, "bin", "", "agent executable; a bare name is looked up on PATH (default claude)")
	fs.StringVar(&a.model, "model", "", "model to pass to the agent; forwarded verbatim")
	fs.StringVar(&a.fallback, "fallback-model", "", "model to fall back to when the first is unavailable")
	fs.StringVar(&a.effort, "effort", "", "reasoning effort, when the agent supports it")

	fs.StringVar(&a.permissionMode, "permission-mode", "bypassPermissions", "how the agent asks for permission; evaluation runs are unattended")
	fs.Var(&a.tools, "tools", "available tool set (repeatable); bare mode collapses the default set")
	fs.Var(&a.allowedTools, "allow-tool", "auto-approved tool pattern (repeatable)")
	fs.Var(&a.deniedTools, "deny-tool", "forbidden tool pattern (repeatable)")
	fs.Var(&a.addDirs, "add-dir", "extra directory the agent may read (repeatable)")

	fs.StringVar(&a.systemPrompt, "system-prompt", "", "replace the system prompt")
	fs.StringVar(&a.appendSystemPrompt, "append-system-prompt", "", "append to the system prompt")
	fs.StringVar(&a.settings, "settings", "", "settings file or inline JSON")
	fs.Var(&a.mcpServers, "mcp-config", "MCP configuration path or JSON (repeatable)")
	fs.Var(&a.pluginDirs, "plugin-dir", "skill directory to make available (repeatable)")
	fs.Var(&a.extraArgs, "extra-arg", "argument appended verbatim; not validated (repeatable)")

	fs.BoolVar(&a.bare, "bare", true, "run the agent in a clean room: no user hooks, no auto-memory, no CLAUDE.md")
	fs.BoolVar(&a.strictMCP, "strict-mcp", true, "load only the MCP servers named on the command line")
	fs.BoolVar(&a.disableSlash, "disable-slash-commands", true, "remove user skills from the agent's context")
	fs.Float64Var(&a.maxBudgetUSD, "max-budget-usd", 0, "abort an attempt once it has spent this much")
}

// spec turns the flags into an agent.Spec. Every path-shaped value is resolved
// against the caller's working directory.
func (a agentFlags) spec() agent.Spec {
	spec := agent.Spec{
		Kind:               a.kind,
		Bin:                absFlagPath(a.bin),
		Model:              a.model,
		FallbackModel:      a.fallback,
		Effort:             a.effort,
		PermissionMode:     a.permissionMode,
		Tools:              a.tools,
		AllowedTools:       a.allowedTools,
		DisallowedTools:    a.deniedTools,
		AddDirs:            absDirList(a.addDirs),
		SystemPrompt:       a.systemPrompt,
		AppendSystemPrompt: a.appendSystemPrompt,
		Settings:           a.settings,
		// Not put through a resolver: an MCP entry may be a JSON blob, and a blob
		// containing a "/" would be mangled into a path.
		MCPServers:   a.mcpServers,
		PluginDirs:   absDirList(a.pluginDirs),
		ExtraArgs:    a.extraArgs,
		MaxBudgetUSD: a.maxBudgetUSD,
		Isolation: &agent.Isolation{
			Bare:                 a.bare,
			StrictMCPConfig:      a.strictMCP,
			DisableSlashCommands: a.disableSlash,
		},
	}
	return spec
}

// warnUnboundReplays names the directories the collected tasks were recorded in
// but will not replay in.
//
// A collected task starts from an empty workspace unless the caller bound one,
// and that default is deliberate: a transcript records the branch a session ran
// on and not the commit it started from, so the tree cannot be reconstructed
// from the capture and inventing one would be a guess. What is not acceptable is
// the silence -- the recording had a working directory, the replay has none, and
// every task in that state goes on to be compared against a recording made
// somewhere else.
//
// Each such baseline already carries the fact, so a report states it per task.
// This is the same fact moved to the one moment it can still be acted on, where
// the fix is a flag rather than a re-read.
// warnDroppedPrompts reports sessions whose prompts did not all become tasks.
//
// Under --split session that is every session the human kept asking in: the task
// is the opening request and the prompts after it are not collected at all.
// Under --split turn it is the prompts --min-prompt-runes dropped as too short.
// Both are quiet by construction -- nothing in the tasks says how many prompts
// the recording held -- and the reading that must not survive is that the
// dataset covers the whole session.
//
// The count is taken over everything this collection produced rather than over
// what an append actually wrote. A task skipped as already collected is still
// in the dataset, so counting only the appended ones would report the same
// session as partly collected on every re-run.
func warnDroppedPrompts(w io.Writer, items []session.Item, split session.Split) {
	type tally struct {
		id          string
		total, kept int
	}
	bySession := map[string]*tally{}
	var order []string
	for _, it := range items {
		id, _ := it.Task.Metadata["session_id"].(string)
		if id == "" {
			continue
		}
		t := bySession[id]
		if t == nil {
			t = &tally{id: id, total: metaInt(it.Task.Metadata["session_prompts"])}
			bySession[id] = t
			order = append(order, id)
		}
		t.kept++
	}

	var short []string
	for _, id := range order {
		t := bySession[id]
		if t.total > t.kept {
			short = append(short, fmt.Sprintf("%s (%d of %d)", shortID(t.id), t.kept, t.total))
		}
	}
	if len(short) == 0 {
		return
	}
	sort.Strings(short)
	advice := "Re-run with --split turn to collect each prompt as its own task"
	if split == session.SplitTurn {
		advice = "Lower --min-prompt-runes so the dropped prompts are collected too"
	}
	fmt.Fprintf(w, "warning: %d session(s) hold more prompts than this dataset collected: %s.\n"+
		"         %s.\n", len(short), strings.Join(short, ", "), advice)
}

// metaInt reads a count out of a task's metadata block, which is JSON-decoded
// on the way in and an int in memory.
func metaInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func warnUnboundReplays(w io.Writer, items []session.Item) {
	var dirs []string
	n := 0
	for _, it := range items {
		if it.Task.Workspace.Type != dataset.WorkspaceEmpty {
			continue
		}
		cwd, _ := it.Task.Metadata["cwd"].(string)
		if cwd == "" {
			continue
		}
		n++
		if !slices.Contains(dirs, cwd) {
			dirs = append(dirs, cwd)
		}
	}
	if n == 0 {
		return
	}
	sort.Strings(dirs)
	shown, more := dirs, ""
	if len(shown) > 2 {
		more = fmt.Sprintf(" (and %d more)", len(shown)-2)
		shown = shown[:2]
	}
	fmt.Fprintf(w, "warning: %d task(s) replay in an empty workspace, but their recordings ran in %s%s.\n"+
		"         A transcript does not carry the files a session worked on, so those replays start\n"+
		"         without them. Re-collect with --workspace-cwd to bind each recording's repository at\n"+
		"         the commit its session started from, or pass --workspace-repo <repo> --ref <commit>\n"+
		"         to bind one repository to all of them.\n",
		n, strings.Join(shown, ", "), more)
}

// warnUnresolvableCommands reports tasks whose prompt opens with a slash command
// while the harness has hidden the user's skills.
//
// A prompt recorded on one agent carries that agent's invocation syntax, so a
// task that needs a command can only be replayed by an agent that exposes it.
// The clean-room defaults hide exactly those: --disable-slash-commands leaves the
// CLI advertising no commands at all, and --bare on its own drops the user's
// skills while keeping the built-ins. Either way the CLI answers "Unknown
// command" locally, without calling the model, and the cell measures nothing.
//
// The run does catch that -- it is a harness error, not a measurement -- but only
// after the attempt has been paid for, and for a replayed session that attempt is
// the whole cost. This is the same warning moved to the only moment it can still
// change anything.
//
// The wording claims only what the flags guarantee. Where --bare is on but
// --disable-slash-commands is not, a built-in command would still resolve, so the
// warning names the flags rather than asserting the command is unusable.
func warnUnresolvableCommands(w io.Writer, ds *dataset.Dataset, spec agent.Spec) {
	iso := spec.EffectiveIsolation()
	if !iso.Bare && !iso.DisableSlashCommands {
		return
	}
	tasks := 0
	var names []string
	for _, t := range ds.Tasks {
		cmd, ok := agent.LeadingSlashCommand(t.ResolvedPrompt)
		if !ok {
			continue
		}
		tasks++
		if !slices.Contains(names, cmd) {
			names = append(names, cmd)
		}
	}
	if tasks == 0 {
		return
	}
	sort.Strings(names)
	if len(names) > 3 {
		names = append(names[:3:3], "...")
	}
	fmt.Fprintf(w, "warning: %d task(s) open with a slash command (%s), but this run does not load the user's\n"+
		"         skills (bare=%t, disable-slash-commands=%t): a recorded command cannot resolve, and the\n"+
		"         cell will measure nothing. Re-run with --bare=false --disable-slash-commands=false to load them.\n",
		tasks, "/"+strings.Join(names, ", /"), iso.Bare, iso.DisableSlashCommands)
}

// summarizeAgent renders the harness configuration the way it will be recorded,
// so a reader of the run list can tell two harnesses apart without opening a
// report.
//
// skills is about the user's own skills, and both --bare and
// --disable-slash-commands remove them, so reading it off one flag reports a
// clean-room run as having them. Declaring plugin directories is reported
// separately from that: a directory that the flags make inert is still a
// declared one, and the count alone would read as "loaded".
func summarizeAgent(s agent.Spec) string {
	iso := s.EffectiveIsolation()
	var parts []string
	parts = append(parts, "kind="+orDefault(s.Kind, "claude_code"))
	if s.Bin != "" {
		parts = append(parts, "bin="+s.Bin)
	}
	if s.Model != "" {
		parts = append(parts, "model="+s.Model)
	}
	parts = append(parts, fmt.Sprintf("bare=%t", iso.Bare))
	parts = append(parts, "skills="+onOff(!iso.Bare && !iso.DisableSlashCommands))
	if n := len(s.PluginDirs); n > 0 {
		state := "loaded"
		if !s.SkillsActive() {
			state = "inert"
		}
		parts = append(parts, fmt.Sprintf("plugin_dirs=%d (%s)", n, state))
	}
	if s.AppendSystemPrompt != "" {
		parts = append(parts, "system_prompt=appended")
	}
	return strings.Join(parts, " ")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// ---------------------------------------------------------------------------
// collect
// ---------------------------------------------------------------------------

func cmdCollect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	var (
		source   = fs.String("source", "claude", "whose history to read: claude (Claude Code transcripts) or cursor")
		cursorDB = fs.String("cursor-db", "", "Cursor state database (default: Cursor's own location for this platform)")
		list     = fs.Bool("list", false, "list the recorded sessions and exit; needs no --out")

		root     = fs.String("root", "", "directory holding Claude Code transcripts (default ~/.claude/projects)")
		selector = fs.String("session", "", "a .jsonl path, a session id prefix, or the working directory to search")
		cwd      = fs.String("cwd", "", "collect every session recorded for this working directory")
		latest   = fs.Bool("latest", false, "collect the most recently modified session")
		out      = fs.String("out", "", "directory to write the dataset into (required)")
		name     = fs.String("name", "", "dataset name (default: the output directory's name)")
		desc     = fs.String("description", "", "dataset description")

		split      = fs.String("split", string(session.SplitSession), "one task per session, or per human turn")
		minPrompt  = fs.Int("min-prompt-runes", 0, "under --split turn, drop prompts shorter than this")
		graderType = fs.String("grader", "", "attach a grader of this type; without one the task is measurable but not scorable")
		graderName = fs.String("grader-name", "", "name for the attached grader")
		graderCfg  = fs.String("grader-config", "", "inline JSON configuration for the grader")

		wsRepo   = fs.String("workspace-repo", "", "bind every task to this repository")
		wsRef    = fs.String("ref", "", "commit or ref to check out")
		wsSubdir = fs.String("subdir", "", "subdirectory of the repository to work in")
		wsAssets = fs.String("assets", "", "directory of files to copy into every workspace")
		wsCwd    = fs.Bool("workspace-cwd", false,
			"bind every task to the directory its own recording ran in, instead of one directory named up front")
		outputT = fs.String("output-type", "", "deliverable contract: patch, files, stdout or none")

		timeoutS  = fs.Int("timeout", 0, "per-task budget in seconds; 0 derives it from the baseline")
		maxTurns  = fs.Int("max-turns", 0, "per-task turn budget; 0 derives it from the baseline")
		idPrefix  = fs.String("id-prefix", "", "prefix for generated task ids")
		category  = fs.String("category", "", "category applied to every task")
		tags      = stringList{}
		asJSON    = fs.Bool("json", false, "print the summary as JSON")
		dryRun    = fs.Bool("dry-run", false, "parse the sessions and report what would be written, without writing")
		overwrite = fs.Bool("overwrite", false, "replace the dataset in --out instead of appending to it")
	)
	fs.Var(&tags, "tag", "tag applied to every task (repeatable)")
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}

	srcKind := strings.ToLower(strings.TrimSpace(*source))
	switch srcKind {
	case "claude", "cursor":
	default:
		return usageFail(stderr, "collect: --source must be \"claude\" or \"cursor\", got %q", *source)
	}
	if srcKind == "claude" && *cursorDB != "" {
		return usageFail(stderr, "collect: --cursor-db only means something with --source cursor")
	}
	if srcKind == "cursor" && *root != "" {
		return usageFail(stderr, "collect: --root only means something with --source claude")
	}

	if *list {
		return cmdCollectList(srcKind, *cursorDB, *root, stdout, stderr)
	}

	if *out == "" && !*dryRun {
		return usageFail(stderr, "collect: --out is required (or --dry-run to look without writing)")
	}
	if *wsCwd && (*wsRepo != "" || *wsAssets != "") {
		return usageFail(stderr, "collect: --workspace-cwd binds each task to its own recording's directory, "+
			"so it cannot be combined with --workspace-repo or --assets, which bind one directory to all of them")
	}
	splitMode := session.Split(*split)
	if splitMode != session.SplitSession && splitMode != session.SplitTurn {
		return usageFail(stderr, "collect: --split must be %q or %q, got %q",
			session.SplitSession, session.SplitTurn, *split)
	}

	var graderConfig map[string]any
	if *graderCfg != "" {
		if err := json.Unmarshal([]byte(*graderCfg), &graderConfig); err != nil {
			return usageFail(stderr, "collect: --grader-config is not valid JSON: %v", err)
		}
	}

	var (
		sessions []*session.Session
		names    []string
	)
	switch srcKind {
	case "cursor":
		var code int
		sessions, names, code = collectCursorSessions(*cursorDB, *selector, *cwd, *latest, stderr)
		if code != exitOK {
			return code
		}
	default:
		sources, code := collectSources(*root, *selector, *cwd, *latest, stderr)
		if code != exitOK {
			return code
		}
		for _, src := range sources {
			s, err := session.ParseFile(src.Path)
			if err != nil {
				return fail(stderr, "collect: parse %s: %v", src.Path, err)
			}
			sessions = append(sessions, s)
			names = append(names, src.SessionID)
		}
	}

	opts := session.Options{
		Split:           splitMode,
		Grader:          *graderType,
		GraderName:      *graderName,
		GraderConfig:    graderConfig,
		WorkspaceRepo:   absFlagPath(*wsRepo),
		WorkspaceRef:    *wsRef,
		WorkspaceSubdir: *wsSubdir,
		WorkspaceAssets: absFlagPath(*wsAssets),
		WorkspaceCwd:    *wsCwd,
		OutputType:      dataset.OutputType(*outputT),
		TimeoutS:        *timeoutS,
		MaxTurns:        *maxTurns,
		MinPromptRunes:  *minPrompt,
		IDPrefix:        *idPrefix,
		Category:        *category,
		Tags:            tags,
	}
	// The machine collect runs on is stamped on every baseline, so that a replay
	// elsewhere is refused rather than compared against an environment it is not
	// running in. A machine that cannot be identified still collects -- refusing
	// would leave its sessions uncollectable -- but the dataset then carries no
	// identity and its replays are not checked, which is said out loud.
	if id, err := machine.Current(); err != nil {
		fmt.Fprintf(stderr, "warning: %v.\n"+
			"         The baselines are written without a machine identity, so a replay on another\n"+
			"         machine will not be refused.\n", err)
	} else {
		opts.Machine = &id
	}
	items, err := session.Collect(sessions, opts)
	if err != nil {
		return fail(stderr, "collect: %v", err)
	}

	// An existing dataset in --out is appended to unless --overwrite says to
	// replace it. Re-collecting a working directory after a few more sessions
	// is the common case, and replacing would drop every task collected before.
	dir := ""
	if *out != "" {
		dir = absFlagPath(*out)
	}
	var plan session.AppendResult
	if *overwrite || dir == "" {
		plan = session.AppendResult{Fresh: true, Added: items}
	} else {
		plan, err = session.PlanAppend(dir, items)
		if err != nil {
			return fail(stderr, "collect: %v", err)
		}
	}

	if *dryRun {
		if plan.Fresh {
			fmt.Fprintf(stdout, "would write %d task(s) from %d session(s)\n\n%s\n",
				len(items), len(sessions), session.Summarize(items))
		} else {
			fmt.Fprintf(stdout, "would append %d task(s) to %s from %d session(s)%s\n",
				len(plan.Added), dir, len(sessions), appendDetail(plan))
			if len(plan.Added) > 0 {
				fmt.Fprintf(stdout, "\n%s\n", session.Summarize(plan.Added))
			}
		}
		warnDroppedPrompts(stderr, items, splitMode)
		return exitOK
	}

	dsName := *name
	if dsName == "" {
		dsName = filepath.Base(dir)
	}
	if plan.Fresh {
		if err := session.WriteDataset(dir, dsName, *desc, items); err != nil {
			return fail(stderr, "collect: write dataset: %v", err)
		}
	} else {
		// --name and --description describe a new manifest. The one in --out
		// is kept as it is, because it may have been edited since it was made.
		if *name != "" || *desc != "" {
			fmt.Fprintln(stderr, "collect: --name and --description are ignored when appending; the existing manifest is kept")
		}
		if plan, err = session.AppendDataset(dir, dsName, *desc, items); err != nil {
			return fail(stderr, "collect: append to dataset: %v", err)
		}
	}

	if *asJSON {
		out := map[string]any{
			"dir": dir, "name": dsName, "tasks": len(plan.Added),
			"sources": names, "appended": !plan.Fresh,
			"skipped": orEmpty(plan.Skipped), "renamed": plan.Renamed,
		}
		return emitJSON(stdout, stderr, out)
	}
	if plan.Fresh {
		fmt.Fprintf(stdout, "wrote %d task(s) to %s\n\n%s\n", len(items), dir, session.Summarize(items))
	} else {
		fmt.Fprintf(stdout, "appended %d task(s) to %s%s\n", len(plan.Added), dir, appendDetail(plan))
		if len(plan.Added) > 0 {
			fmt.Fprintf(stdout, "\n%s\n", session.Summarize(plan.Added))
		}
	}
	warnDroppedPrompts(stderr, items, splitMode)
	if !*wsCwd {
		warnUnboundReplays(stderr, plan.Added)
	} else {
		fmt.Fprintln(stderr, "note: --workspace-cwd binds each recording's repository at the commit its "+
			"session started from; where the capture named no commit, or the repository no longer holds "+
			"it, the task falls back to the recorded branch as it stands now, and says so in its notes")
	}
	fmt.Fprintf(stdout, "\nNext: aeval validate --dataset %s\n", filepath.Join(dir, "dataset.yaml"))
	return exitOK
}

// appendDetail says what an append left out and what it renamed, so that
// "appended 0 task(s)" reads as "already there" rather than as a failure.
func appendDetail(plan session.AppendResult) string {
	var b strings.Builder
	if n := len(plan.Skipped); n > 0 {
		fmt.Fprintf(&b, "\nskipped %d already collected: %s", n, strings.Join(plan.Skipped, ", "))
	}
	if len(plan.Renamed) > 0 {
		var pairs []string
		for from, to := range plan.Renamed {
			pairs = append(pairs, from+" -> "+to)
		}
		sort.Strings(pairs)
		fmt.Fprintf(&b, "\nrenamed %d whose id was taken: %s", len(pairs), strings.Join(pairs, ", "))
	}
	return b.String()
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func collectSources(root, selector, cwd string, latest bool, stderr io.Writer) ([]session.Source, int) {
	if root == "" {
		root = session.DefaultRoot()
	}
	switch {
	case selector != "":
		// Resolved against the caller's directory before anything is read, so
		// the transcript path the collected dataset records means the same
		// thing from anywhere. A bare session-id prefix has no separator and is
		// left alone. Without this the written dataset names a file that only
		// exists relative to wherever collect happened to be run.
		src, err := session.Resolve(root, absFlagPath(selector))
		if err != nil {
			fail(stderr, "collect: %v", err)
			return nil, exitError
		}
		return []session.Source{src}, exitOK
	case cwd != "":
		srcs, err := session.SourcesForCwd(root, absFlagPath(cwd))
		if err != nil {
			fail(stderr, "collect: %v", err)
			return nil, exitError
		}
		if len(srcs) == 0 {
			fail(stderr, "collect: no sessions recorded for %s", cwd)
			return nil, exitError
		}
		return srcs, exitOK
	case latest:
		src, err := session.Latest(root)
		if err != nil {
			fail(stderr, "collect: %v", err)
			return nil, exitError
		}
		return []session.Source{src}, exitOK
	default:
		fail(stderr,
			"collect: name what to collect: --session <path|id|cwd>, --cwd <dir>, or --latest\n"+
				"       transcripts are read from %s", root)
		return nil, exitUsage
	}
}

// collectCursorSessions reads the conversations the flags name out of Cursor's
// state database.
//
// It returns the ids alongside the sessions because the caller prints them, and
// re-deriving them from a parsed session would be a second, subtly different
// answer to the same question.
func collectCursorSessions(dbPath, selector, cwd string, latest bool, stderr io.Writer) ([]*session.Session, []string, int) {
	if strings.TrimSpace(dbPath) == "" {
		dbPath = session.CursorDefaultDB()
	}
	if strings.TrimSpace(dbPath) == "" {
		fail(stderr, "collect: cannot locate the Cursor database on this platform; pass --cursor-db")
		return nil, nil, exitUsage
	}
	dbPath = absFlagPath(dbPath)

	var ids []string
	switch {
	case selector != "":
		ch, err := session.ResolveCursor(dbPath, selector)
		if err != nil {
			fail(stderr, "collect: %v", err)
			return nil, nil, exitError
		}
		ids = append(ids, ch.ID)
	case cwd != "":
		chats, err := session.CursorChatsForCwd(dbPath, absFlagPath(cwd))
		if err != nil {
			fail(stderr, "collect: %v", err)
			return nil, nil, exitError
		}
		if len(chats) == 0 {
			fail(stderr, "collect: no cursor conversations recorded for %s", cwd)
			return nil, nil, exitError
		}
		for _, ch := range chats {
			ids = append(ids, ch.ID)
		}
	case latest:
		ch, err := session.LatestCursorChat(dbPath)
		if err != nil {
			fail(stderr, "collect: %v", err)
			return nil, nil, exitError
		}
		ids = append(ids, ch.ID)
	default:
		fail(stderr,
			"collect: name what to collect: --session <id>, --cwd <dir>, or --latest\n"+
				"       cursor conversations are read from %s\n"+
				"       --list shows what is in there", dbPath)
		return nil, nil, exitUsage
	}

	sessions, err := session.ParseCursorChats(dbPath, ids)
	if err != nil {
		fail(stderr, "collect: %v", err)
		return nil, nil, exitError
	}

	// Collect refuses a session with no human prompt, and it is right to: a
	// capture with no request in it is not a task. A Cursor history is full of
	// them -- drafts, aborted chats, sessions whose only message was attached
	// context -- and letting them through would abort a whole collection on the
	// first one. So they are dropped here, with a count.
	kept := make([]*session.Session, 0, len(sessions))
	keptIDs := make([]string, 0, len(ids))
	var dropped int
	for i, s := range sessions {
		if len(s.Prompts) == 0 {
			dropped++
			continue
		}
		kept = append(kept, s)
		keptIDs = append(keptIDs, ids[i])
	}
	if dropped > 0 {
		fmt.Fprintf(stderr, "collect: skipped %d conversation(s) with no human prompt\n", dropped)
	}
	if len(kept) == 0 {
		fail(stderr, "collect: none of the %d selected conversation(s) contains a human prompt", len(sessions))
		return nil, nil, exitError
	}
	return kept, keptIDs, exitOK
}

// cmdCollectList prints what is available to collect, without collecting it.
//
// It exists because naming a session is otherwise guesswork: a Cursor
// conversation is a uuid in a database, and a Claude Code transcript is a uuid
// in a directory whose name is lossy.
func cmdCollectList(srcKind, cursorDB, root string, stdout, stderr io.Writer) int {
	switch srcKind {
	case "cursor":
		dbPath := cursorDB
		if strings.TrimSpace(dbPath) == "" {
			dbPath = session.CursorDefaultDB()
		}
		if strings.TrimSpace(dbPath) == "" {
			return usageFail(stderr, "collect: cannot locate the Cursor database on this platform; pass --cursor-db")
		}
		dbPath = absFlagPath(dbPath)

		chats, err := session.CursorChats(dbPath)
		if err != nil {
			return fail(stderr, "collect: %v", err)
		}
		fmt.Fprintf(stdout, "%d cursor conversation(s) in %s\n\n", len(chats), dbPath)
		if len(chats) == 0 {
			return exitOK
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "id\ttouched\tmode\tmsgs\tprompts\tname")
		for _, ch := range chats {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n",
				ch.ID, shortWhen(ch.UpdatedAt), orDefault(ch.Mode, "-"),
				ch.Bubbles, ch.Prompts, oneLine(ch.Name, 48))
		}
		tw.Flush()
		fmt.Fprintf(stdout, "\ncollect one with: aeval collect --source cursor --session <id prefix> --out <dir>\n")
		return exitOK

	default:
		if root == "" {
			root = session.DefaultRoot()
		}
		sources, err := session.Sources(root)
		if err != nil {
			return fail(stderr, "collect: %v", err)
		}
		fmt.Fprintf(stdout, "%d claude code transcript(s) in %s\n\n", len(sources), root)
		if len(sources) == 0 {
			return exitOK
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "session\tmodified\tsize\tproject")
		for _, src := range sources {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				src.SessionID, shortWhen(src.ModTime), humanBytes(src.Size), filepath.Base(src.ProjectDir))
		}
		tw.Flush()
		fmt.Fprintf(stdout, "\ncollect one with: aeval collect --session <id prefix> --out <dir>\n")
		return exitOK
	}
}

// shortWhen renders a timestamp for a listing.
//
// The date and the time are joined with "T" rather than a space so that a row
// is one token per column: these listings are read by people, but they are read
// with `awk` and `cut` too, and a space inside a column value silently shifts
// every field to its right.
func shortWhen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02T15:04")
}

// oneLine flattens a title for a listing. A name carrying a newline would
// otherwise break the column it is printed into.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max < 2 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	var (
		path   = fs.String("dataset", "", "dataset manifest to check (required)")
		asJSON = fs.Bool("json", false, "print the result as JSON")
	)
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	if *path == "" {
		return usageFail(stderr, "validate: --dataset is required")
	}

	ds, err := dataset.ValidatePath(absFlagPath(*path))
	if err != nil {
		// Validation issues are the expected failure and get one line each, so
		// that a manifest with eight typos does not have to be fixed eight
		// times. Anything else is an environment problem and stays a terse
		// single-line error.
		var vErr *dataset.ValidationError
		if errors.As(err, &vErr) {
			if *asJSON {
				return emitJSON(stdout, stderr, map[string]any{
					"valid": false, "issues": issueStrings(vErr.Issues),
				})
			}
			fmt.Fprintf(stderr, "%s is not valid: %d issue(s)\n", *path, len(vErr.Issues))
			for _, issue := range vErr.Issues {
				fmt.Fprintf(stderr, "  - %s\n", issue.String())
			}
			return exitError
		}
		return fail(stderr, "validate: %v", err)
	}

	if *asJSON {
		return emitJSON(stdout, stderr, map[string]any{
			"valid": true, "name": ds.Manifest.Name, "tasks": len(ds.Tasks),
			"hash": ds.Hash, "dir": ds.Dir,
		})
	}
	fmt.Fprintf(stdout, "%s is valid\n", *path)
	fmt.Fprintf(stdout, "  name     %s\n", ds.Manifest.Name)
	fmt.Fprintf(stdout, "  tasks    %d\n", len(ds.Tasks))
	fmt.Fprintf(stdout, "  hash     %s\n", ds.Hash)
	fmt.Fprintf(stdout, "\nNext: aeval run --dataset %s\n", *path)
	return exitOK
}

func issueStrings(issues []dataset.ValidationIssue) []string {
	out := make([]string, 0, len(issues))
	for _, issue := range issues {
		out = append(out, issue.String())
	}
	return out
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var (
		common  commonFlags
		agents  agentFlags
		dsPath  = fs.String("dataset", "", "dataset manifest to run (required)")
		name    = fs.String("name", "", "run name (default: the dataset's name)")
		notes   = fs.String("notes", "", "free-form note recorded on the run")
		samples = fs.Int("samples", 1, "how many times to run each task (pass@k)")
		conc    = fs.Int("concurrency", 1, "cells to keep in flight")
		chain   = fs.Bool("chain-sessions", false,
			"give each recorded session's tasks one shared workspace and run them in order")
		timeout = fs.Duration("timeout", 10*time.Minute, "per-task budget when the task declares none")
		turns   = fs.Int("max-turns", 0, "per-task turn budget when the task declares none")
		thresh  = fs.Float64("threshold", runner.DefaultThreshold, "composite score a task must reach to pass")
		work    = fs.String("work-root", "", "where per-cell workspaces are created (default: beside the database)")
		keep    = fs.Bool("keep", false, "leave each cell's workspace on disk for inspection")
		inPlace = fs.Bool("in-place", false,
			"work in the directories the tasks record, checked out at the commits they name, and put each back afterwards")
		failFast = fs.Bool("fail-fast", false, "stop scheduling cells after the first harness failure")
		resume   = fs.Bool("resume", false, "skip cells that already have a row for this run")
		runID    = fs.String("run-id", "", "reuse a run id, which is what --resume needs")
		quiet    = fs.Bool("quiet", false, "do not print per-cell progress")
		asJSON   = fs.Bool("json", false, "print the summary as JSON")
		reportTo = fs.String("report", "", "also write an HTML report to this path")
	)
	addCommon(fs, &common)
	addAgentFlags(fs, &agents)
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	if *dsPath == "" {
		return usageFail(stderr, "run: --dataset is required")
	}

	ds, err := dataset.Load(absFlagPath(*dsPath))
	if err != nil {
		return fail(stderr, "run: %v", err)
	}
	st, err := openStore(&common)
	if err != nil {
		return fail(stderr, "run: %v", err)
	}
	defer st.Close()

	ctx, stop := signalContext()
	defer stop()

	cfg := runner.Config{
		Store:          st,
		Agent:          agents.spec(),
		RunID:          *runID,
		RunName:        *name,
		Notes:          *notes,
		Samples:        *samples,
		Concurrency:    *conc,
		WorkRoot:       absFlagPath(*work),
		KeepWorkspaces: *keep,
		Timeout:        *timeout,
		MaxTurns:       *turns,
		Threshold:      *thresh,
		ToolVersion:    version,
		FailFast:       *failFast,
		Resume:         *resume,
		ChainSessions:  *chain,
		InPlace:        *inPlace,
	}
	// Asking the configuration rather than restating its rules: a flag mistake
	// is a usage error, which is a different exit code from a run that started
	// and failed.
	if err := cfg.Validate(); err != nil {
		return usageFail(stderr, "run: %v", err)
	}
	// The dataset's half of the same question, asked before the run row exists
	// and before anything has been touched. It is reported as a usage error for
	// the same reason as above: every one of these is a fact the caller can act
	// on, and the run it would otherwise produce is not a measurement of
	// anything.
	if err := runner.CheckMachine(ds); err != nil {
		return usageFail(stderr, "run: %v", err)
	}
	if err := runner.Preflight(ctx, ds, cfg); err != nil {
		return usageFail(stderr, "run: %v", err)
	}
	if !*quiet && !*asJSON {
		cfg.Progress = progressLogger(stdout)
	}

	warnUnresolvableCommands(stderr, ds, cfg.Agent)

	fmt.Fprintf(stdout, "running %d task(s) × %d sample(s) from %s\n", len(ds.Tasks), cfg.Samples, ds.Dir)
	fmt.Fprintf(stdout, "harness %s\n\n", summarizeAgent(cfg.Agent))
	if cfg.ChainSessions {
		fmt.Fprintln(stdout, "chained: each session's tasks share one workspace and run in order, "+
			"so the cells of a chain are a sequence rather than independent samples")
		fmt.Fprintln(stdout)
	}
	if cfg.InPlace {
		warnInPlace(stderr, ds, cfg)
	}

	res, err := runner.Run(ctx, ds, cfg)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stdout, "\ninterrupted; what was measured is recorded")
			return exitCtlC
		}
		return fail(stderr, "run: %v", err)
	}

	if *asJSON {
		if code := emitJSON(stdout, stderr, map[string]any{
			"run": res.Run, "summary": res.Summary,
		}); code != exitOK {
			return code
		}
	} else {
		printSummary(stdout, res.Summary, res.Run, common.db)
	}

	if *reportTo != "" {
		data, err := report.Load(ctx, st, res.Run.ID, report.Options{})
		if err != nil {
			return fail(stderr, "run: build report: %v", err)
		}
		if err := report.RenderRunFile(absFlagPath(*reportTo), data, report.RenderOptions{}); err != nil {
			return fail(stderr, "run: write report: %v", err)
		}
		fmt.Fprintf(stdout, "\nreport  %s\n", absFlagPath(*reportTo))
	}
	return exitOK
}

// warnInPlace says what an in-place run is about to do to which directories.
//
// It is a warning and not a confirmation, because the run refuses everything it
// cannot undo and puts back everything it does. What it cannot promise is
// stated here rather than left to be discovered: a repository the agent pushed
// to, a branch it created, and a file the .gitignore hides are all outside what
// a restore can reach, and each of them is something a person would reasonably
// have assumed was covered.
//
// The recovery instruction is not printed here because the values it needs are
// read under the lock, a moment later. It is written into the lock instead, and
// printed in full by any later run that finds the lock still there -- which is
// exactly the run a person makes after finding their repository in the wrong
// state.
func warnInPlace(w io.Writer, ds *dataset.Dataset, cfg runner.Config) {
	repos := runner.InPlaceRepos(ds, cfg)
	if len(repos) == 0 {
		return
	}
	counts := map[string]int{}
	for _, t := range ds.Tasks {
		counts[t.Workspace.RepoPath(ds.Dir)] += cfg.Samples
	}
	fmt.Fprintln(w, "in place: this run works in the directories below, each checked out at the commit "+
		"its tasks name, and puts each one back when its chain finishes.")
	fmt.Fprintln(w, "          A repository with uncommitted changes is refused before anything runs.")
	fmt.Fprintln(w, "          Not put back: branches, tags and stashes the agent creates or moves "+
		"(reported, not removed);")
	fmt.Fprintln(w, "          files your .gitignore hides (left in place, reported); anything pushed to "+
		"a remote.")
	fmt.Fprintln(w, "          The repository's whole history is readable by the agent for the whole run.")
	fmt.Fprintln(w)
	for _, repo := range repos {
		fmt.Fprintf(w, "  %s   (%d cell(s))\n", repo, counts[repo])
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Each directory holds a lock naming the commit and branch to return it to. If this "+
		"process is killed outright it puts nothing back, and the next run that finds the lock will "+
		"refuse and print the commands that repair it.")
	fmt.Fprintln(w)
}

// progressLogger prints one line per finished cell.
//
// The columns are padded by hand rather than with tabwriter: a progress line
// has to stand on its own as it arrives, and tabwriter buffers a whole block
// before emitting anything, which is exactly wrong here.
func progressLogger(w io.Writer) func(runner.Progress) {
	return func(p runner.Progress) {
		cell := p.Cell
		fmt.Fprintf(w, "[%*d/%d] %-28s %-9s %8s  %7s\n",
			len(fmt.Sprint(p.Total)), p.Done, p.Total,
			truncate(cell.TaskID, 28), cellState(cell),
			compactInt(cell.TokenTotal()), shortDuration(cell.DurationMs))
	}
}

// cellState is the word shown for a finished cell.
//
// A timeout keeps its own label: calling it "failed" would claim the agent got
// the task wrong, when all that is known is that it did not finish inside the
// budget. Likewise a cell with no grader is "measured", not "failed".
func cellState(cell store.Attempt) string {
	switch {
	case cell.Status == store.CellError:
		return "error"
	case cell.Status == store.CellTimeout:
		return "timeout"
	case cell.Graded && cell.Passed:
		return "passed"
	case cell.Graded:
		return "failed"
	default:
		return "measured"
	}
}

func printSummary(w io.Writer, s runner.Summary, run store.Run, db string) {
	fmt.Fprintf(w, "run     %s\n", run.ID)
	fmt.Fprintf(w, "status  %s\n", run.Status)
	if run.Error != "" {
		fmt.Fprintf(w, "error   %s\n", run.Error)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "cells\t"+fmt.Sprint(s.Cells))
	fmt.Fprintln(tw, "graded\t"+fmt.Sprint(s.Graded))
	if rate, ok := s.PassRate(); ok {
		fmt.Fprintf(tw, "passed\t%d (%.1f%%)\n", s.Passed, rate*100)
	} else {
		fmt.Fprintln(tw, "passed\tn/a (nothing in this run was graded)")
	}
	if s.Ungraded > 0 {
		fmt.Fprintf(tw, "ungraded\t%d (measured, nothing to score)\n", s.Ungraded)
	}
	if s.Timeouts > 0 {
		fmt.Fprintf(tw, "timed out\t%d (ran out of budget: data about the agent)\n", s.Timeouts)
	}
	if s.HarnessErrors > 0 {
		fmt.Fprintf(tw, "harness errors\t%d (could not be measured at all)\n", s.HarnessErrors)
	}
	fmt.Fprintln(tw, "tokens\t"+compactInt(s.Tokens))
	fmt.Fprintln(tw, "cost\t"+fmt.Sprintf("$%.4f", s.CostUSD))
	fmt.Fprintln(tw, "wall clock\t"+shortDuration(s.DurationMs))
	// Each half is printed only when some cell actually carried the matching
	// measurement. A recording that never captured busy time leaves the delta at
	// zero, and printing that as "0" would read as "took the same time" rather
	// than as "was never timed".
	if s.BaselineCells > 0 {
		var parts []string
		if s.TokenCells > 0 {
			parts = append(parts, signed(s.TokenDelta)+" tokens")
		}
		if s.TimedCells > 0 {
			parts = append(parts, signedDuration(s.TimeDeltaMs))
		}
		if len(parts) > 0 {
			fmt.Fprintf(tw, "vs baseline\t%s across %d cell(s)\n",
				strings.Join(parts, ", "), s.BaselineCells)
		} else {
			fmt.Fprintln(tw, "vs baseline\trecorded, but it carries neither tokens nor time to subtract")
		}
	}
	tw.Flush()

	// The hint has to be the command that actually works. Repeating the --db
	// only when it is not the default keeps the common case short, and omitting
	// it when it was given is worse than noise: `report` would look in the
	// default file and fail with "not found" for a run the user just watched
	// finish. The value is echoed as typed rather than absolutised, because the
	// reader is in the same directory that this command was run from.
	next := fmt.Sprintf("aeval report --run %s", run.ID)
	if db != "" && db != defaultDB {
		next += " --db " + hintArg(db)
	}
	fmt.Fprintf(w, "\nNext: %s\n", next)
}

// hintArg renders a flag value for a copy-pasteable command line. Only values
// that would otherwise be split or expanded get quoted; a plain path stays bare,
// because that is what the user typed and what the hint should look like.
func hintArg(v string) string {
	if v == "" || !strings.ContainsAny(v, " \t\n'\"\\$`*?[]{}()|&;<>#!~") {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// ---------------------------------------------------------------------------
// matrix
// ---------------------------------------------------------------------------

func cmdMatrix(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("matrix", flag.ContinueOnError)
	var (
		common   commonFlags
		file     = fs.String("file", "", "matrix document describing the sweep")
		sweep    = fs.String("sweep", "", "attribute a sweep that already ran, by id")
		only     = fs.String("only", "", "run just this level")
		sweepID  = fs.String("sweep-id", "", "use this sweep id instead of generating one")
		dryRun   = fs.Bool("dry-run", false, "print the plan and exit without running anything")
		quiet    = fs.Bool("quiet", false, "do not print per-cell progress")
		asJSON   = fs.Bool("json", false, "print the attribution as JSON")
		reportTo = fs.String("report", "", "write an attribution report to this path")
		base     = fs.String("base-model", "", "override the model every level starts from")
	)
	addCommon(fs, &common)
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	if *file == "" && *sweep == "" {
		return usageFail(stderr, "matrix: give --file to run a sweep, or --sweep to attribute one that already ran")
	}

	st, err := openStore(&common)
	if err != nil {
		return fail(stderr, "matrix: %v", err)
	}
	defer st.Close()

	ctx, stop := signalContext()
	defer stop()

	id := *sweep
	if *file != "" {
		plan, err := attrib.Load(absFlagPath(*file))
		if err != nil {
			return fail(stderr, "matrix: %v", err)
		}
		// Resolve the threshold here rather than leaving it 0 in the plan: the
		// runner would substitute its own default anyway, and a plan that prints
		// "threshold 0" while the run uses 1.0 is a plan that lies about itself.
		if plan.Threshold <= 0 {
			plan.Threshold = runner.DefaultThreshold
		}
		if *base != "" {
			plan.Base.Model = *base
			for i := range plan.Levels {
				if plan.Levels[i].Agent.Model == "" {
					plan.Levels[i].Agent.Model = *base
				}
			}
		}
		if *dryRun {
			printPlan(stdout, plan)
			return exitOK
		}
		if id == "" {
			id = *sweepID
		}
		if id == "" {
			id = newSweepID(plan.Name)
		}
		if code := runSweep(ctx, st, plan, id, *only, !*quiet, stdout, stderr); code != exitOK {
			return code
		}
	}

	att, err := attrib.Attribute(ctx, st, id)
	if err != nil {
		return fail(stderr, "matrix: %v", err)
	}
	if *asJSON {
		if code := emitJSON(stdout, stderr, att); code != exitOK {
			return code
		}
	} else {
		printAttribution(stdout, att)
	}
	if *reportTo != "" {
		data, err := report.LoadSweep(ctx, st, id, report.Options{})
		if err != nil {
			return fail(stderr, "matrix: build report: %v", err)
		}
		if err := report.RenderSweepFile(absFlagPath(*reportTo), data, report.RenderOptions{}); err != nil {
			return fail(stderr, "matrix: write report: %v", err)
		}
		fmt.Fprintf(stdout, "\nreport  %s\n", absFlagPath(*reportTo))
	}
	return exitOK
}

// runSweep executes every level of a sweep, control first.
//
// Each level is a separate run with the same sweep id and its own factor label,
// which is what lets an attribution be recomputed later from the database
// alone. Running levels concurrently would be faster and would also let them
// contend for the same machine, so the default is one at a time.
func runSweep(ctx context.Context, st *store.Store, plan *attrib.Plan, sweepID, only string, progress bool,
	stdout, stderr io.Writer) int {

	ds, err := dataset.Load(plan.DatasetPath)
	if err != nil {
		return fail(stderr, "matrix: %v", err)
	}
	// Once for the sweep, before the first level: every level replays the same
	// dataset, and a refusal from inside the control level would arrive after
	// the sweep header had already said it was starting.
	if err := runner.CheckMachine(ds); err != nil {
		return usageFail(stderr, "matrix: %v", err)
	}

	steps := plan.Steps()
	fmt.Fprintf(stdout, "sweep %s over %s: %d level(s), control %s\n\n",
		sweepID, plan.Axis, len(steps), plan.Control)

	for _, step := range steps {
		if only != "" && step.Level.Name != only {
			continue
		}
		label := fmt.Sprintf("%s=%s", plan.Axis, step.Level.Name)
		if step.IsControl {
			label += " (control)"
		}
		fmt.Fprintf(stdout, "── %s\n", label)
		if len(step.Level.Changed) > 0 {
			fmt.Fprintf(stdout, "   varies %s\n", strings.Join(step.Level.Changed, ", "))
		}
		fmt.Fprintf(stdout, "   harness %s\n\n", summarizeAgent(step.Level.Agent))

		// The blob is built per level rather than once for the sweep: `changed`
		// is the level's own evidence of what it varied, and the levels do not
		// vary the same keys.
		meta := attrib.Meta{
			Name:    plan.Name,
			Axis:    plan.Axis,
			Control: plan.Control,
			Notes:   plan.Notes,
			Changed: step.Level.Changed,
		}
		blob, err := meta.Encode()
		if err != nil {
			return fail(stderr, "matrix: %v", err)
		}

		cfg := runner.Config{
			Store:          st,
			Agent:          step.Level.Agent,
			RunName:        plan.Name,
			Notes:          plan.Notes,
			Samples:        plan.Samples,
			Concurrency:    plan.Concurrency,
			WorkRoot:       plan.WorkRoot,
			KeepWorkspaces: plan.KeepWork,
			Timeout:        plan.Timeout,
			Threshold:      plan.Threshold,
			FactorAxis:     string(plan.Axis),
			FactorLevel:    step.Level.Name,
			SweepID:        sweepID,
			ToolVersion:    version,
			ConfigJSON:     blob,
		}
		if progress {
			cfg.Progress = progressLogger(stdout)
		}
		res, err := runner.Run(ctx, ds, cfg)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintln(stdout, "\ninterrupted; the levels already run are recorded and can be attributed")
				return exitCtlC
			}
			return fail(stderr, "matrix: level %s: %v", step.Level.Name, err)
		}
		rate := "n/a"
		if r, ok := res.Summary.PassRate(); ok {
			rate = fmt.Sprintf("%.1f%%", r*100)
		}
		fmt.Fprintf(stdout, "   → run %s  pass %s  tokens %s  %s\n\n",
			res.Run.ID, rate, compactInt(res.Summary.Tokens), shortDuration(res.Summary.DurationMs))
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	return exitOK
}

// headlineFindings drops the findings that only restate the table above.
//
// Every metric gets a verdict, so the JSON and the report can show a complete
// matrix. The terminal summary is a different job: a level that moved on
// nothing produces one identical sentence per metric, and a findings list that
// does that is one readers learn to skip -- which costs exactly the line that
// mattered. So correctness is always reported, and everything else only when it
// actually moved.
func headlineFindings(all []attrib.Finding) []attrib.Finding {
	out := make([]attrib.Finding, 0, len(all))
	for _, f := range all {
		if f.Metric == attrib.MetricPassRate || f.MeanDelta != 0 {
			out = append(out, f)
		}
	}
	return out
}

func printPlan(w io.Writer, plan *attrib.Plan) {
	fmt.Fprintf(w, "sweep  %s\naxis   %s\ncontrol %s\ndataset %s\n",
		plan.Name, plan.Axis, plan.Control, plan.DatasetPath)
	fmt.Fprintf(w, "samples %d · concurrency %d · threshold %g\n\n", plan.Samples, plan.Concurrency, plan.Threshold)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "level\tvaries\tnotes")
	for _, step := range plan.Steps() {
		name := step.Level.Name
		if step.IsControl {
			name += " *"
		}
		varies := strings.Join(step.Level.Changed, ", ")
		if varies == "" {
			varies = "(nothing)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, varies, step.Level.Notes)
	}
	tw.Flush()
	fmt.Fprintln(w, "\n* control level; every other level is measured against it")
	for _, warn := range plan.Warnings {
		fmt.Fprintf(w, "\nwarning: %s\n", warn)
	}
}

func printAttribution(w io.Writer, att *attrib.Attribution) {
	fmt.Fprintf(w, "%s: %d level(s) of %s, control %s\n\n",
		orDefault(att.Name, att.SweepID), len(att.Levels), att.Axis, att.Control)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "level\tstatus\tpass rate\ttokens\twall clock")
	for _, l := range att.Levels {
		rate := "n/a"
		if l.PassRate != nil {
			rate = fmt.Sprintf("%.1f%%", *l.PassRate*100)
		}
		name := l.Name
		if l.IsControl {
			name += " *"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, l.Status, rate,
			compactInt(l.Summary.Tokens), shortDuration(l.Summary.DurationMs))
	}
	tw.Flush()

	for _, cmp := range att.Comparisons {
		fmt.Fprintf(w, "\n%s against %s\n\n", cmp.Level, cmp.Control)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  metric\t"+cmp.Control+"\t"+cmp.Level+"\tmean delta\twin/loss/tie\tp")
		for _, s := range cmp.Stats {
			p := "n/a"
			if s.Testable {
				p = fmt.Sprintf("%.3f", s.P)
			}
			mark := " "
			if s.Primary {
				mark = "*"
			}
			fmt.Fprintf(tw, "%s %s\t%.2f\t%.2f\t%+.2f %s\t%d/%d/%d\t%s\n",
				mark, s.Label, s.ControlMean, s.LevelMean, s.MeanDelta, s.Unit,
				s.Wins, s.Losses, s.Ties, p)
		}
		tw.Flush()
		fmt.Fprintln(w, "\n  * correctness: the metric a change is judged on")
	}

	if lines := headlineFindings(att.Findings); len(lines) > 0 {
		fmt.Fprintln(w, "\nfindings")
		for _, f := range lines {
			fmt.Fprintf(w, "  %-21s %s\n", "["+f.Direction+"]", f.Text)
		}
	}
	if len(att.Caveats) > 0 {
		fmt.Fprintln(w, "\ncaveats")
		for _, c := range att.Caveats {
			fmt.Fprintf(w, "  - %s\n", c)
		}
	}
}

// newSweepID names a sweep so it is identifiable in a list weeks later: the
// plan's name plus the moment it started, with the timestamp in the format that
// sorts.
func newSweepID(name string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	safe = strings.Trim(safe, "-")
	if safe == "" {
		safe = "sweep"
	}
	if len(safe) > 40 {
		safe = safe[:40]
	}
	return safe + "-" + time.Now().UTC().Format("20060102-150405")
}

// ---------------------------------------------------------------------------
// report
// ---------------------------------------------------------------------------

func cmdReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	var (
		common  commonFlags
		runID   = fs.String("run", "", "run to render")
		cmp     = fs.Bool("compare", false, "render a comparison of the two positional run ids")
		sweep   = fs.String("sweep", "", "render the attribution of this sweep")
		listAll = fs.Bool("index", false, "render the run list, plus one report per run")
		outDir  = fs.String("out-dir", "", "directory for --index output (default .)")
		limit   = fs.Int("limit", 100, "with --index, how many runs to list")
		sweepF  = fs.String("sweep-filter", "", "with --index, only list this sweep's runs")
		title   = fs.String("title", "", "document title")
		summary = fs.Bool("summary", false, "omit per-task detail: a small report of the numbers only")
	)
	addCommon(fs, &common)
	// -o is the conventional spelling for where the artifact goes.
	var out string
	fs.StringVar(&out, "o", "", "output path (default: report-<id>.html beside the database)")
	fs.StringVar(&out, "out", "", "alias for -o")
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}

	// Recover the flags written after a positional argument *before* opening the
	// store, not inside the branch that uses them. Go's flag package stops at
	// the first non-flag word, so the natural spelling
	// `report --compare a b --db my.db` leaves --db unparsed; recovering it
	// after openStore would set it too late, and the store would already be open
	// at the default path -- every run then reported as missing from aeval.db.
	trailing, code := positionals(fs, stderr)
	if code != exitOK {
		return code
	}

	ctx := context.Background()
	st, err := openStore(&common)
	if err != nil {
		return fail(stderr, "report: %v", err)
	}
	defer st.Close()

	// Resolved up front because the same options both build the payload and are
	// stored beside its hash. Left unresolved, the second call inside Load would
	// stamp a fresh time and the registration would be of a document that no
	// longer exists by the time it is written.
	opts := report.Options{Title: *title, NoTaskDetail: *summary}.Resolve()
	ropts := report.RenderOptions{}

	switch {
	case *listAll:
		dir := orDefault(*outDir, ".")
		dir = absFlagPath(dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fail(stderr, "report: %v", err)
		}
		idx, err := report.LoadIndex(ctx, st, *limit, store.RunFilter{SweepID: *sweepF}, opts)
		if err != nil {
			return fail(stderr, "report: %v", err)
		}
		for _, entry := range idx.Runs {
			data, err := report.Load(ctx, st, entry.ID, opts)
			if err != nil {
				return fail(stderr, "report: %v", err)
			}
			path := filepath.Join(dir, report.FileName(entry.ID))
			if err := report.RenderRunFile(path, data, ropts); err != nil {
				return fail(stderr, "report: %v", err)
			}
		}
		indexPath := filepath.Join(dir, "index.html")
		if err := report.RenderIndexFile(indexPath, idx, ropts); err != nil {
			return fail(stderr, "report: %v", err)
		}
		// One row for the invocation, describing the index. The per-run files
		// written above are a batch convenience rather than the artifact: they
		// are the same documents `report --run` produces, and recording each of
		// them here would turn one command into a hundred registry rows.
		if err := recordReport(ctx, st, stdout, report.Request{
			Kind:    store.ReportKindIndex,
			Title:   idx.Title,
			Subject: indexSubject(len(idx.Runs), *sweepF),
			Params:  indexParams(*limit, *sweepF),
			Options: opts,
		}, idx); err != nil {
			return fail(stderr, "report: %v", err)
		}
		fmt.Fprintf(stdout, "%d run report(s) and %s\n", len(idx.Runs), indexPath)
		return exitOK

	case *sweep != "":
		data, err := report.LoadSweep(ctx, st, *sweep, opts)
		if err != nil {
			return fail(stderr, "report: %v", err)
		}
		path := absFlagPath(orDefault(out, "attribution-"+safeName(*sweep)+".html"))
		if err := report.RenderSweepFile(path, data, ropts); err != nil {
			return fail(stderr, "report: %v", err)
		}
		if err := recordReport(ctx, st, stdout, report.Request{
			Kind:    store.ReportKindSweep,
			Title:   data.Title,
			Subject: *sweep,
			Params:  map[string]string{"sweep": *sweep},
			Options: opts,
		}, data); err != nil {
			return fail(stderr, "report: %v", err)
		}
		fmt.Fprintln(stdout, path)
		return exitOK

	case *cmp:
		ids := trailing
		if len(ids) != 2 {
			return usageFail(stderr, "report: --compare needs exactly two run ids, got %d", len(ids))
		}
		data, err := report.LoadComparison(ctx, st, ids[0], ids[1], opts)
		if err != nil {
			return fail(stderr, "report: %v", err)
		}
		path := absFlagPath(orDefault(out, "compare-"+safeName(ids[0])+"-"+safeName(ids[1])+".html"))
		if err := report.RenderComparisonFile(path, data, ropts); err != nil {
			return fail(stderr, "report: %v", err)
		}
		if err := recordReport(ctx, st, stdout, report.Request{
			Kind:    store.ReportKindCompare,
			Title:   data.Title,
			Subject: data.Title,
			Params:  map[string]string{"left": ids[0], "right": ids[1]},
			Options: opts,
		}, data); err != nil {
			return fail(stderr, "report: %v", err)
		}
		fmt.Fprintln(stdout, path)
		return exitOK
	}

	id := *runID
	if id == "" && len(trailing) == 1 {
		id = trailing[0]
	}
	if id == "" {
		return usageFail(stderr, "report: give --run <id>, --compare <a> <b>, --sweep <id> or --index")
	}
	data, err := report.Load(ctx, st, id, opts)
	if err != nil {
		return fail(stderr, "report: %v", err)
	}
	path := absFlagPath(orDefault(out, report.FileName(id)))
	if err := report.RenderRunFile(path, data, ropts); err != nil {
		return fail(stderr, "report: %v", err)
	}
	if err := recordReport(ctx, st, stdout, report.Request{
		Kind:    store.ReportKindRun,
		Title:   data.Title,
		Subject: data.Title,
		Params:  map[string]string{"run": id},
		Options: opts,
	}, data); err != nil {
		return fail(stderr, "report: %v", err)
	}
	fmt.Fprintln(stdout, path)
	return exitOK
}

// indexParams is the request a run listing records.
func indexParams(limit int, sweepID string) map[string]string {
	p := map[string]string{"limit": strconv.Itoa(limit)}
	if sweepID != "" {
		p["sweep_id"] = sweepID
	}
	return p
}

// indexSubject labels a listing by what it showed, not by its limit: a limit of
// 100 over three runs is a document with three runs in it, and that is the
// thing a reader scanning the registry wants to know.
func indexSubject(runs int, sweepID string) string {
	what := fmt.Sprintf("the %d most recent run(s)", runs)
	if sweepID != "" {
		return what + " of sweep " + sweepID
	}
	return what
}

// recordReport records a report that has been rendered.
//
// A failure here is fatal rather than a warning, which is the opposite of how
// this program treats a task that failed to pass. The difference is what the
// two are: a failing task is a measurement, and a registry that could not be
// written is a bookkeeping failure -- the command would exit 0 having written a
// file that the server will deny exists. The exit code is the only channel a
// caller cannot miss.
func recordReport(ctx context.Context, st *store.Store, stdout io.Writer, req report.Request, payload any) error {
	reg, err := report.Save(ctx, st, req, payload)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "recorded  %s\n", reg.ID)
	return nil
}

// safeName makes an id usable inside a filename, matching the sanitiser the
// report package uses so that a link and the file it points at agree.
func safeName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "run"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

// cmdServe serves the recorded reports over HTTP.
//
// It does not render anything to disk and it does not snapshot the database:
// every page is rebuilt from the store on request, so a report recorded after
// the server started is openable without a restart, and a page always shows
// what the rows say now. What it cannot show is the past -- a report whose data
// has moved says so in a banner rather than pretending to be the document that
// was registered.
func cmdServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		common commonFlags
		addr   = fs.String("addr", "127.0.0.1:8080", "address to listen on")
		limit  = fs.Int("limit", 200, "how many recorded reports the list shows")
		title  = fs.String("title", "", "heading of the report list")

		noSessions  = fs.Bool("no-sessions", false, "serve reports only: no session pages and no replay button")
		presetsPath = fs.String("replay-config", "", "YAML file of the harnesses, skill sets and models a replay can pick")
		claudeRoot  = fs.String("claude-root", "", "where Claude Code transcripts are read from (default ~/.claude/projects)")
		cursorDB    = fs.String("cursor-db", "", "Cursor's state database (default: the platform's location)")
		replayRoot  = fs.String("replay-root", "", "where replays keep their datasets and workspaces (default: beside the database)")
	)
	addCommon(fs, &common)
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	if strings.TrimSpace(*addr) == "" {
		return usageFail(stderr, "serve: --addr may not be empty")
	}
	if *noSessions && (*presetsPath != "" || *claudeRoot != "" || *cursorDB != "" || *replayRoot != "") {
		return usageFail(stderr, "serve: --replay-config, --claude-root, --cursor-db and --replay-root "+
			"only mean something with the session pages, which --no-sessions turns off")
	}
	if !*noSessions {
		// Checked before listening: the session pages serve every transcript on
		// this machine and launch agents, and the defences against a hostile
		// page in the user's own browser (see internal/server/sessions.go) do
		// not cover a request from another machine.
		if host, _, err := net.SplitHostPort(*addr); err == nil && !isLoopbackHost(host) {
			return usageFail(stderr, "serve: the session pages read every transcript on this machine and start "+
				"agents with permission prompts bypassed, so they are served on loopback only; %s is not. "+
				"Bind 127.0.0.1, or pass --no-sessions to serve only the reports there", *addr)
		}
	}

	st, err := openStore(&common)
	if err != nil {
		return fail(stderr, "serve: %v", err)
	}
	defer st.Close()

	// Listen before printing anything. The address that comes back is the one
	// that is actually open, which is what makes `--addr 127.0.0.1:0` usable for
	// taking a free port; and a port already in use has to fail before a line
	// claiming the server is up, not after it.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fail(stderr, "serve: listen on %s: %v", *addr, err)
	}

	web := server.New(st, server.Options{Title: *title, Limit: *limit})
	if !*noSessions {
		presets := replay.DefaultPresets()
		if *presetsPath != "" {
			if presets, err = replay.LoadPresets(absFlagPath(*presetsPath)); err != nil {
				ln.Close()
				return usageFail(stderr, "serve: %v", err)
			}
		}
		catalog := &session.Catalog{ClaudeRoot: session.DefaultRoot(), CursorDB: session.CursorDefaultDB()}
		if *claudeRoot != "" {
			catalog.ClaudeRoot = absFlagPath(*claudeRoot)
		}
		if *cursorDB != "" {
			catalog.CursorDB = absFlagPath(*cursorDB)
		}
		mgr, err := replay.NewManager(replay.Config{
			Store: st, Catalog: catalog, Presets: presets,
			WorkRoot: absFlagPath(*replayRoot), ToolVersion: version,
		})
		if err != nil {
			ln.Close()
			return fail(stderr, "serve: %v", err)
		}
		defer mgr.Close()
		if err := web.EnableSessions(server.SessionOptions{Catalog: catalog, Replays: mgr}); err != nil {
			ln.Close()
			return fail(stderr, "serve: %v", err)
		}
	}
	srv := &http.Server{
		Handler:           web.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signalContext()
	defer stop()
	go func() {
		<-ctx.Done()
		// A fresh context rather than the cancelled one: Shutdown's job is to
		// let in-flight requests finish, and a context that is already done
		// would abort them instead.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Fprintf(stdout, "serving   %s\n", listenURL(ln.Addr().String()))
	fmt.Fprintf(stdout, "database  %s\n", st.Path())
	if !isLoopback(ln.Addr().String()) {
		// Worth a line: the database holds every recorded transcript, diff and
		// answer, so the difference between this and the default is the
		// difference between a page on this machine and one on the network.
		fmt.Fprintln(stdout, "warning   bound to a non-loopback address: "+
			"anything that can reach this port can read every recorded report")
	}
	if !*noSessions {
		fmt.Fprintf(stdout, "sessions  %ssessions   (replays launch agents on this machine; --no-sessions turns this off)\n",
			listenURL(ln.Addr().String()))
	}
	fmt.Fprintln(stdout, "press ctrl-c to stop")

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fail(stderr, "serve: %v", err)
	}
	fmt.Fprintln(stdout, "stopped")
	// Stopping is how a server ends, not an interruption of work: the signal is
	// the ordinary way to stop one, and the work -- serving -- finished. 130
	// would make every script that kills its own server look like a failure.
	return exitOK
}

// listenURL renders a bound address as something a person can paste.
//
// The wildcard forms become 127.0.0.1 because that is where a reader on this
// machine reaches the same port, and printing "http://0.0.0.0:8080/" would be
// technically what was bound and useless to click.
func listenURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/"
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// isLoopback reports whether a bound address is reachable only from this
// machine. An unparseable address counts as loopback so that a warning is never
// printed about a bind this program cannot describe.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackHost is the stricter question the session pages ask of an address
// before anything is bound: only an explicit loopback name or address passes.
// The wildcard ("", "0.0.0.0", "::") fails, because it listens on every
// interface.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var (
		common commonFlags
		sweep  = fs.String("sweep", "", "only runs of this sweep")
		axis   = fs.String("axis", "", "only runs that varied this axis")
		limit  = fs.Int("limit", 50, "how many runs to show")
		asJSON = fs.Bool("json", false, "print the runs as JSON")
	)
	addCommon(fs, &common)
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}

	ctx := context.Background()
	st, err := openStore(&common)
	if err != nil {
		return fail(stderr, "list: %v", err)
	}
	defer st.Close()

	runs, err := st.ListRuns(ctx, store.RunFilter{SweepID: *sweep, FactorAxis: *axis, Limit: *limit})
	if err != nil {
		return fail(stderr, "list: %v", err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, runs)
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "no runs recorded")
		return exitOK
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "id\trun\tstatus\tfactor\tpass\tcells\ttokens\tstarted")
	for _, r := range runs {
		attempts, err := st.ListAttempts(ctx, r.ID)
		if err != nil {
			return fail(stderr, "list: %v", err)
		}
		s := runner.Summarize(attempts, len(distinct(attempts)))
		rate := "n/a"
		if v, ok := s.PassRate(); ok {
			rate = fmt.Sprintf("%.0f%%", v*100)
		}
		factor := "-"
		if r.FactorAxis != "" {
			factor = r.FactorAxis + "=" + r.FactorLevel
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			r.ID, truncate(r.Name, 24), r.Status, factor, rate, s.Cells,
			compactInt(s.Tokens), r.StartedAt.Local().Format("2006-01-02 15:04"))
	}
	tw.Flush()
	return exitOK
}

func distinct(attempts []store.Attempt) map[string]bool {
	seen := map[string]bool{}
	for _, a := range attempts {
		seen[a.TaskID] = true
	}
	return seen
}

// ---------------------------------------------------------------------------
// version
// ---------------------------------------------------------------------------

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the version information as JSON")
	if ok, code := parse(fs, args, stderr); !ok {
		return code
	}
	info := map[string]any{
		"version":        version,
		"schema_version": store.SchemaVersion(),
		"data_version":   report.DataVersion,
		"go":             runtime.Version(),
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"agents":         agent.Registered(),
	}
	if *asJSON {
		return emitJSON(stdout, stderr, info)
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "version\t%s\n", version)
	fmt.Fprintf(tw, "schema\t%d (SQLite)\n", store.SchemaVersion())
	fmt.Fprintf(tw, "payload\t%d (report JSON)\n", report.DataVersion)
	fmt.Fprintf(tw, "go\t%s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(tw, "agents\t%s\n", strings.Join(agent.Registered(), ", "))
	tw.Flush()
	return exitOK
}

// ---------------------------------------------------------------------------
// Output helpers
// ---------------------------------------------------------------------------

// positionals returns the positional words a command needs, tolerating flags
// written after them.
//
// Go's flag package stops parsing at the first non-flag word, so the natural
// spelling `aeval report --compare a b -o out.html` leaves "-o out.html" looking
// positional and fails with a count of four. Re-parsing from that word recovers
// the flags, which is plainly what the caller meant. Positional words keep the
// order they were written in.
func positionals(fs *flag.FlagSet, stderr io.Writer) ([]string, int) {
	rest := append([]string(nil), fs.Args()...)
	for i, w := range rest {
		if !isFlagWord(w) {
			continue
		}
		head := rest[:i]
		if ok, code := parse(fs, rest[i:], stderr); !ok {
			return nil, code
		}
		return append(head, fs.Args()...), exitOK
	}
	return rest, exitOK
}

// isFlagWord reports whether w looks like a flag rather than a value. A lone "-"
// is a value (it means stdin), so it does not count.
func isFlagWord(w string) bool {
	return len(w) > 1 && w[0] == '-'
}

func emitJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(stderr, "encode output: %v", err)
	}
	return exitOK
}

func compactInt(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprint(n)
	}
}

func shortDuration(ms int64) string {
	switch {
	case ms <= 0:
		return "-"
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm%02ds", ms/60_000, (ms/1000)%60)
	default:
		return fmt.Sprintf("%dh%02dm", ms/3_600_000, (ms/60_000)%60)
	}
}

func signed(n int) string {
	if n == 0 {
		return "0"
	}
	if n > 0 {
		return "+" + compactInt(n)
	}
	return "-" + compactInt(-n)
}

func signedDuration(ms int64) string {
	if ms == 0 {
		return "0"
	}
	if ms > 0 {
		return "+" + shortDuration(ms)
	}
	return "-" + shortDuration(-ms)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// sortedKeys is here for callers that want a stable order over a set; keeping
// it in one place is cheaper than re-deriving it at each call site.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
