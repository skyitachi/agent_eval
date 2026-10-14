// Package session turns recorded Claude Code sessions into evaluation data.
//
// A transcript is the JSONL file Claude Code appends to under
//
//	~/.claude/projects/<slugged-cwd>/<session-uuid>.jsonl
//
// It holds more than a benchmark instance does: the human's opening request,
// every tool call the agent made, the token spend per message, the wall-clock
// and API time of each turn, the session cost, and the files the session edited.
// `aeval collect` reads it and emits a task plus a *baseline* -- the recorded
// attempt a new run is measured against.
//
// # Why the parser dedupes by message id
//
// Claude Code writes one record per content *block*, not per message. A single
// assistant message that thinks, says something and calls a tool lands as three
// records, each repeating the same `usage` object. Summing usage per record
// therefore multiplies the token count by the number of blocks -- measured at
// 176 assistant records for 63 messages in a real session. The parser
// accumulates blocks per message id and counts usage exactly once.
//
// # What a transcript gives back about the repository
//
// Recent builds write a session-context attachment whose gitStatus block is a
// snapshot of the repository as the session found it. Its "Recent commits" list
// starts at HEAD, so the commit the session started from is recoverable, and
// GitStart carries it along with whether the worktree had uncommitted changes.
// Older captures have no such record and leave both empty.
//
// What it still cannot give back is the *tree*: a commit is reproducible, and
// uncommitted changes are not. A recording that started dirty says so, because
// a replay from the commit is not a replay from where the session started.
//
// # What it deliberately drops
//
// Tool results. They are large, they are environment state rather than agent
// behaviour, and a baseline is a record of what the agent did, not of what the
// filesystem said back. Results are counted (so the record is complete) but not
// stored.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxLine bounds one transcript line. Tool results and file snapshots can be
// large; a real session was observed with multi-megabyte shell output.
const maxLine = 64 << 20

// maxGap bounds a single gap counted as machine time by busyFromGaps. It sits
// far above any plausible tool call and far below a session left suspended.
const maxGap = 30 * time.Minute

// Roles used in Turn.Role.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// SourceName is the producer recorded in dataset.Baseline.Source.
const SourceName = "claude-code-session"

// GitStart is what a capture says about its repository when the session began.
//
// It comes from a rendered block rather than a field, so every part of it is
// best-effort: Known says whether the block was there at all, and the rest is
// only meaningful when it was.
type GitStart struct {
	// Head is the commit HEAD pointed at, as the block wrote it. Recent builds
	// abbreviate it to ten hex characters, so it is not a full object name and
	// has to be resolved against the repository before it can be checked out.
	Head string
	// Dirty is whether the worktree had uncommitted changes. It is the half of
	// the snapshot a commit cannot reproduce.
	Dirty bool
	// DirtyPaths is how many paths the status block listed. It counts paths
	// rather than changes, because that is what the block lists.
	DirtyPaths int
	// Known reports that a status block was found, which is what makes a clean
	// reading a measurement rather than an absence.
	Known bool
}

// Session is a parsed transcript.
type Session struct {
	// ID is the session uuid.
	ID string
	// Path is the file it was read from, when known.
	Path string
	// Source names the producer, recorded in the baseline. Empty means the
	// Claude Code transcript this package was originally written for; a session
	// that came from somewhere else says so, because "which agent produced this
	// baseline" is the first thing a reader of a comparison needs.
	Source string
	// BaselineNotes are caveats the capture itself knows about -- a field it
	// never recorded, an estimate standing in for a measurement. They travel
	// with the baseline so that a reader who only has the dataset still sees
	// them, rather than having to re-derive them from the parser.
	BaselineNotes []string

	// Provenance.
	Cwd       string
	GitBranch string
	// GitHead is the commit the session started from, as the transcript's
	// session-context block recorded it. Empty when the capture has no such
	// block, or when it started outside a repository.
	GitHead string
	// GitStart is what that block said about the worktree. It travels with the
	// session because a commit cannot reproduce uncommitted changes, and a
	// reader who is told only the commit would take the replay for a replay of
	// where the session actually started.
	GitStart GitStart
	Version  string
	Models   []string

	StartedAt time.Time
	EndedAt   time.Time

	// Prompts are the human turns, in order. The first is the task.
	Prompts []Prompt
	// Turns is the conversation: human turns and assistant turns, in order,
	// with tool calls attached to the assistant turn that issued them.
	Turns []Turn

	// ToolCalls is every tool invocation, flattened in order.
	ToolCalls []ToolCall
	// ToolResults counts results observed. They are not stored; the count is
	// what makes "nothing was dropped by accident" checkable.
	ToolResults int

	// Usage is the session's token spend. It prefers the per-model totals from
	// the cost-state record and falls back to the deduped per-message sum.
	Usage Usage
	// CostUSD is the session total as recorded by Claude Code.
	CostUSD float64

	// haveTotals records whether a cost-state record was seen. The fallback is
	// derived at finish() from the per-turn usage, which is already deduped.
	haveTotals bool
	// staleTotals records that messages arrived after the last cost-state
	// record, which makes that record a checkpoint of a prefix rather than a
	// total. See absorbAssistant.
	staleTotals bool
	// messagesAfterTotals counts the messages the last checkpoint does not
	// cover, so the note beside a partial cost can say how partial.
	messagesAfterTotals int
	// uncoveredUsage is the spend of those messages. It is added to the
	// checkpoint's totals, which is what keeps a session total a total.
	uncoveredUsage Usage

	// DurationMs is wall-clock for the whole session. It includes the time the
	// human spent reading and typing, so it is not the number to compare an
	// unattended run against; BusyMs is.
	DurationMs int64
	// BusyMs is API time plus tool time: the machine working. This is the
	// baseline number a new run's wall clock is compared to, because a session
	// with a human in the loop is idle most of the time and comparing against
	// its wall clock would flatter every automated attempt.
	BusyMs int64
	// APIDurationMs and ToolDurationMs are the two halves of BusyMs.
	APIDurationMs  int64
	ToolDurationMs int64
	// ThinkingMs is summed per-message thinking time.
	ThinkingMs int64

	// EditedFiles are the paths the session modified, sorted.
	EditedFiles []string

	// ParseErrors are lines that could not be decoded. A corrupt capture stays
	// usable; only I/O failures abort.
	ParseErrors []string
}

// Prompt is one human turn.
type Prompt struct {
	// Index is the position among human turns, starting at 0.
	Index int
	Text  string
	At    time.Time
	// TurnIndex is the position of this prompt in Session.Turns. It is what
	// lets a per-turn collection slice the trajectory from this point on
	// without matching on text.
	TurnIndex int
}

// Turn is one entry in the conversation.
type Turn struct {
	Role string
	// Text is the assistant's prose, or the human's message.
	Text string
	// Thinking is the assistant's reasoning, when the record carries it.
	Thinking string
	// Tools are the calls this turn issued.
	Tools []ToolCall
	At    time.Time
	// MessageID is the assistant message id the blocks belong to.
	MessageID string
	// DurationMs is the turn's own wall clock, recorded on the turn's last
	// assistant entry. Zero when the build did not record one.
	DurationMs int64
	// Usage is this turn's token spend, counted once per message. Nil on human
	// turns and on turns whose record carried no usage.
	Usage *Usage
}

// ToolCall is one tool invocation.
type ToolCall struct {
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Usage is token spend.
type Usage struct {
	InputTokens       int `json:"input_tokens,omitempty"`
	OutputTokens      int `json:"output_tokens,omitempty"`
	CacheReadTokens   int `json:"cache_read_tokens,omitempty"`
	CacheCreateTokens int `json:"cache_creation_tokens,omitempty"`
	ThinkingTokens    int `json:"thinking_tokens,omitempty"`
}

// Total is the token count used for cost comparison, billed and cached alike.
//
// Cached reads are included rather than hidden: a baseline that re-read a large
// cached context 60 times is not cheaper than one that read a small context
// once, and a comparison that ignored the cache would say it was.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreateTokens
}

// add is how a cost-state checkpoint is completed by the messages that followed
// it. Thinking tokens are carried too: the checkpoint reports them and the
// per-message sum does not, so dropping them would lose the one figure only the
// checkpoint has.
func (u Usage) add(o Usage) Usage {
	return Usage{
		InputTokens:       u.InputTokens + o.InputTokens,
		OutputTokens:      u.OutputTokens + o.OutputTokens,
		CacheReadTokens:   u.CacheReadTokens + o.CacheReadTokens,
		CacheCreateTokens: u.CacheCreateTokens + o.CacheCreateTokens,
		ThinkingTokens:    u.ThinkingTokens + o.ThinkingTokens,
	}
}

// ---------------------------------------------------------------------------
// Raw records
// ---------------------------------------------------------------------------

// rawRecord is the envelope shared by every line. Claude Code writes a
// heterogeneous stream under one `type` discriminator, so decoding it into a
// single wide struct is simpler than a per-type unmarshal -- and it means an
// unrecognised record type is skipped rather than failing the parse.
type rawRecord struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`
	SessionID  string `json:"sessionId"`
	Timestamp  string `json:"timestamp"`

	Cwd         string `json:"cwd"`
	GitBranch   string `json:"gitBranch"`
	Version     string `json:"version"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`

	Origin *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	PromptSource string `json:"promptSource"`

	Message *rawMessage `json:"message"`

	DurationMs         int64 `json:"durationMs"`
	ThinkingDurationMs int64 `json:"thinkingDurationMs"`
	MessageCount       int   `json:"messageCount"`

	TotalCostUSD      float64                  `json:"totalCostUSD"`
	TotalDuration     int64                    `json:"totalDuration"`
	TotalAPIDuration  int64                    `json:"totalAPIDuration"`
	TotalToolDuration int64                    `json:"totalToolDuration"`
	ModelUsage        map[string]rawModelUsage `json:"modelUsage"`

	Snapshot *rawSnapshot `json:"snapshot"`

	Attachment *rawAttachment `json:"attachment"`
}

// rawAttachment is a record about the environment rather than about the
// conversation -- hooks, tool listings, and the session context that carries the
// repository snapshot.
//
// Context stays raw because the shape under it varies with the attachment type
// and with the build that wrote it. Decoding it into one wide struct would turn
// a shape this build does not know into a failed line, and the record is worth
// nothing next to the conversation beside it.
type rawAttachment struct {
	Type    string          `json:"type"`
	Context json.RawMessage `json:"context"`
}

type rawMessage struct {
	ID      string    `json:"id"`
	Role    string    `json:"role"`
	Model   string    `json:"model"`
	Content content   `json:"content"`
	Usage   *rawUsage `json:"usage"`
}

// rawUsage is the per-message spend. It is repeated verbatim on every block of
// the same message, which is why the parser keys on message id.
type rawUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type rawModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	ThinkingTokens           int     `json:"thinkingTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

type rawSnapshot struct {
	// TrackedFileBackups is keyed by absolute path; the values name the backup
	// Claude Code keeps of the pre-edit content. Only the keys are used here.
	TrackedFileBackups map[string]json.RawMessage `json:"trackedFileBackups"`
}

// content is either a bare string (a human turn) or a list of blocks.
type content struct {
	text   string
	blocks []contentBlock
}

func (c *content) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		return json.Unmarshal(b, &c.text)
	}
	if !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(b, &c.blocks); err != nil {
			// A shape we do not know is not worth failing the line over.
			return nil
		}
	}
	return nil
}

type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	ID       string          `json:"id"`
	Input    json.RawMessage `json:"input"`
	// ToolUseID marks a tool_result block.
	ToolUseID string `json:"tool_use_id"`
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// Parse reads a transcript. A malformed line is recorded in Session.ParseErrors
// and skipped; a non-nil error means the stream itself failed.
func Parse(r io.Reader) (*Session, error) {
	s := &Session{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)

	// Assistant blocks accumulate per message id; the map is what keeps a
	// message's thinking, text and tool call in one turn.
	assistantAt := map[string]int{}
	modelSet := map[string]bool{}
	edited := map[string]bool{}
	line := 0

	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var rec rawRecord
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			s.ParseErrors = append(s.ParseErrors, fmt.Sprintf("line %d: %v", line, err))
			continue
		}
		if rec.IsSidechain {
			// A subagent's messages are not the main trajectory. Keeping them
			// would attribute a subagent's tool calls to the task itself.
			continue
		}
		s.absorb(rec, assistantAt, modelSet, edited)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}

	s.finish(modelSet, edited)
	return s, nil
}

// ParseFile reads a transcript from disk.
func ParseFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	s.Path = path
	return s, nil
}

func (s *Session) absorb(rec rawRecord, assistantAt map[string]int, modelSet map[string]bool, edited map[string]bool) {
	if rec.SessionID != "" && s.ID == "" {
		s.ID = rec.SessionID
	}
	if rec.Cwd != "" && s.Cwd == "" {
		s.Cwd = rec.Cwd
	}
	if rec.GitBranch != "" && s.GitBranch == "" {
		s.GitBranch = rec.GitBranch
	}
	if rec.Version != "" && s.Version == "" {
		s.Version = rec.Version
	}
	if at, ok := parseTime(rec.Timestamp); ok {
		if s.StartedAt.IsZero() || at.Before(s.StartedAt) {
			s.StartedAt = at
		}
		if at.After(s.EndedAt) {
			s.EndedAt = at
		}
	}

	switch rec.Type {
	case "assistant":
		s.absorbAssistant(rec, assistantAt, modelSet)
	case "user":
		s.absorbUser(rec)
	case "system":
		if rec.Subtype == "turn_duration" {
			s.closeTurn(rec.DurationMs)
		}
	case "cost-state":
		// A checkpoint of everything so far. The last one wins, and it is a
		// total only if nothing follows it.
		s.CostUSD = rec.TotalCostUSD
		s.DurationMs = rec.TotalDuration
		s.APIDurationMs = rec.TotalAPIDuration
		s.ToolDurationMs = rec.TotalToolDuration
		s.Usage = usageFromModelTotals(rec.ModelUsage)
		s.haveTotals = true
		s.staleTotals = false
		s.messagesAfterTotals = 0
		s.uncoveredUsage = Usage{}
	case "file-history-snapshot":
		if rec.Snapshot != nil {
			for p := range rec.Snapshot.TrackedFileBackups {
				edited[p] = true
			}
		}
	case "attachment":
		s.absorbAttachment(rec)
	}
}

func (s *Session) absorbAssistant(rec rawRecord, assistantAt map[string]int, modelSet map[string]bool) {
	m := rec.Message
	if m == nil {
		return
	}
	if m.Model != "" {
		modelSet[m.Model] = true
	}
	at, _ := parseTime(rec.Timestamp)

	idx, seen := assistantAt[m.ID]
	if !seen {
		s.Turns = append(s.Turns, Turn{
			Role:      RoleAssistant,
			At:        at,
			MessageID: m.ID,
		})
		idx = len(s.Turns) - 1
		assistantAt[m.ID] = idx
		// A message that arrives after the last cost-state record is one that
		// record does not cover. Claude Code writes the record as a checkpoint
		// and normally closes a session with one, so this is rare -- but a
		// capture that outlives its last checkpoint would otherwise publish a
		// prefix as a session total. One real session put its last checkpoint at
		// line 178 of 905 and under-reported its tokens fourteenfold.
		if s.haveTotals {
			s.staleTotals = true
			s.messagesAfterTotals++
		}
		if m.Usage != nil {
			u := Usage{
				InputTokens:       m.Usage.InputTokens,
				OutputTokens:      m.Usage.OutputTokens,
				CacheReadTokens:   m.Usage.CacheReadInputTokens,
				CacheCreateTokens: m.Usage.CacheCreationInputTokens,
			}
			s.Turns[idx].Usage = &u
			if s.haveTotals {
				// The checkpoint's totals stop where it was written, so what
				// this message spent has to be added to them. This is the same
				// arithmetic as summing every message; it just keeps the billed
				// figure for the part the record does cover.
				s.uncoveredUsage = s.uncoveredUsage.add(u)
			}
		}
		s.ThinkingMs += rec.ThinkingDurationMs
	}
	t := &s.Turns[idx]
	if t.At.IsZero() {
		t.At = at
	}

	for _, b := range m.Content.blocks {
		switch b.Type {
		case "text":
			t.Text = joinText(t.Text, b.Text)
		case "thinking":
			t.Thinking = joinText(t.Thinking, b.Thinking)
		case "tool_use":
			tc := ToolCall{ID: b.ID, Name: b.Name, Input: b.Input}
			t.Tools = append(t.Tools, tc)
			s.ToolCalls = append(s.ToolCalls, tc)
		}
	}
}

// closeTurn records the wall clock of the turn that just ended.
//
// The clock goes on the last assistant entry, not on the prompt at the same
// ordinal. Position among prompts is what a session with a queued prompt breaks:
// a human who types while the agent is still working writes their next prompt
// before the running turn's duration record lands, so every later turn would
// take the clock of the one before it. Walking back to the assistant output is
// what makes the attachment survive that.
//
// A turn that produced no output of its own cannot be told from that case, so
// the record is dropped rather than charged to whichever turn happens to be
// nearby.
func (s *Session) closeTurn(ms int64) {
	for i := len(s.Turns) - 1; i >= 0; i-- {
		if s.Turns[i].Role != RoleAssistant {
			continue
		}
		if s.Turns[i].DurationMs != 0 {
			return
		}
		s.Turns[i].DurationMs = ms
		return
	}
}

func (s *Session) absorbUser(rec rawRecord) {
	m := rec.Message
	if m == nil {
		return
	}
	at, _ := parseTime(rec.Timestamp)

	// A tool result is not a human turn. The count is kept so that "the
	// trajectory is complete" is checkable even though the payloads are not
	// stored.
	text := m.Content.text
	if len(m.Content.blocks) > 0 {
		toolResult := false
		var parts []string
		for _, b := range m.Content.blocks {
			switch b.Type {
			case "tool_result":
				s.ToolResults++
				toolResult = true
			case "text":
				parts = append(parts, b.Text)
			}
		}
		if toolResult {
			return
		}
		// Prompts sent through the SDK (the IDE extensions) arrive as text
		// blocks: what the human typed, then the attached file references and
		// selections. All of it is what the model was asked, so all of it is
		// the prompt.
		text = strings.Join(parts, "\n")
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// Claude Code injects its own user-role records (context, caveats). Only a
	// record the human typed is a prompt; otherwise the "first prompt" of the
	// session would be a system paragraph.
	if !isHumanPrompt(rec) {
		return
	}
	s.Prompts = append(s.Prompts, Prompt{Index: len(s.Prompts), Text: text, At: at, TurnIndex: len(s.Turns)})
	s.Turns = append(s.Turns, Turn{Role: RoleUser, Text: text, At: at})
}

// isHumanPrompt decides whether a user-role record is something the human typed.
func isHumanPrompt(rec rawRecord) bool {
	if rec.IsMeta {
		return false
	}
	if rec.Origin != nil {
		return rec.Origin.Kind == "human"
	}
	// Older builds carry no origin. A typed prompt is the only user record that
	// says where it came from, so the flag is the fallback signal.
	return rec.PromptSource == "typed"
}

// absorbAttachment reads the session-context block, which is where a capture
// says what repository it started in.
//
// Only the first one counts. A resumed session appends a second block, and that
// one describes the resume rather than the state the recorded work started
// from, which is the state a replay has to be measured against.
func (s *Session) absorbAttachment(rec rawRecord) {
	if rec.Attachment == nil || rec.Attachment.Type != "session_context" {
		return
	}
	if s.GitStart.Known {
		return
	}
	// A context this build cannot read is not a corrupt capture. The record is
	// one line about the environment, and failing it would report the whole
	// transcript as damaged over a block nothing else depends on.
	var ctx struct {
		GitStatus string `json:"gitStatus"`
	}
	if err := json.Unmarshal(rec.Attachment.Context, &ctx); err != nil {
		return
	}
	start := parseGitStatus(ctx.GitStatus)
	if !start.Known {
		return
	}
	s.GitStart = start
	if s.GitHead == "" {
		s.GitHead = start.Head
	}
}

// parseGitStatus reads the rendered git-status block.
//
// It is prose, not a record, so this reads as little of it as it can: the first
// line under "Recent commits:" is HEAD, and the status section is either
// "(clean)" or a list of paths. Everything else in the block is ignored, and a
// block whose shape has changed yields Known with nothing else set rather than
// a guess.
func parseGitStatus(status string) GitStart {
	if strings.TrimSpace(status) == "" {
		return GitStart{}
	}
	out := GitStart{Known: true}

	if _, rest, ok := strings.Cut(status, "Recent commits:\n"); ok {
		line, _, _ := strings.Cut(rest, "\n")
		// The list is "<abbreviated object name> <subject>". A repository with
		// no commits has no list, so the section is simply absent.
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if isObjectName(name) {
			out.Head = name
		}
	}

	if _, rest, ok := strings.Cut(status, "\nStatus:\n"); ok {
		// The section ends at the blank line before the commit list. It is cut
		// again on the heading because a build that omits the blank line would
		// otherwise fold the heading into the last path.
		block, _, _ := strings.Cut(rest, "\n\n")
		block, _, _ = strings.Cut(block, "Recent commits:")
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line == "(clean)" {
				continue
			}
			out.DirtyPaths++
		}
		out.Dirty = out.DirtyPaths > 0
	}
	return out
}

// isObjectName reports whether s could be a git object name. Recent builds write
// ten hex characters; the range is wide because that is a rendering choice and
// not a promise.
func isObjectName(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func (s *Session) finish(modelSet map[string]bool, edited map[string]bool) {
	for m := range modelSet {
		s.Models = append(s.Models, m)
	}
	sort.Strings(s.Models)

	for p := range edited {
		s.EditedFiles = append(s.EditedFiles, p)
	}
	sort.Strings(s.EditedFiles)

	// Busy time is the machine working. Wall clock is reported separately
	// because a session with a human in the loop spends most of it idle.
	//
	// A checkpoint the session outlived is not a total, so its API and tool
	// timers describe a prefix too and cannot stand in for the session's.
	if s.haveTotals && !s.staleTotals {
		s.BusyMs = s.APIDurationMs + s.ToolDurationMs
	}
	if s.BusyMs == 0 {
		s.BusyMs = busyFromGaps(s.Turns)
		if s.BusyMs > 0 {
			s.BaselineNotes = append(s.BaselineNotes,
				"busy time is estimated from the gaps that end at an assistant turn: "+
					"this capture recorded no API or tool durations")
		}
	}
	// The wall clock comes from the record when the record is a total, and from
	// the session's own span when it is not -- a checkpoint's duration stops
	// where the checkpoint does, just as its token count does.
	if s.staleTotals || s.DurationMs == 0 {
		if !s.StartedAt.IsZero() && s.EndedAt.After(s.StartedAt) {
			s.DurationMs = s.EndedAt.Sub(s.StartedAt).Milliseconds()
		}
	}

	// A capture with no cost-state record still has the deduped per-message
	// sum, which is complete even though it cannot be split by model. A capture
	// whose record stopped being written before it ended needs the messages
	// that followed the record added to it; without them the session total is
	// whatever the checkpoint happened to have reached.
	if s.haveTotals {
		s.Usage = s.Usage.add(s.uncoveredUsage)
	} else {
		s.Usage = s.summedUsage()
	}
	if s.staleTotals {
		s.BaselineNotes = append(s.BaselineNotes, fmt.Sprintf(
			"the recorded cost covers only the start of this session: %d messages follow the last cost checkpoint, "+
				"so their tokens are added to it from the messages themselves and the cost is a partial figure",
			s.messagesAfterTotals))
	}
}

// busyFromGaps estimates machine time from the timestamps of a conversation.
//
// It sums every gap that ends at an assistant turn. The gap that starts at an
// assistant turn and ends at a human one is the human reading and typing, and
// is exactly what must not be counted; the gap before an assistant turn is the
// model and its tools working. On the captures that do record their own API and
// tool time it reproduces them to within a few percent -- measured at +1% to
// +7% across eleven sessions.
//
// It is an estimate, and every caller that publishes it labels it as one. What
// it is for is the budget: a task whose capture recorded no timings still needs
// a timeout sized for the work, and a default would report a timeout the agent
// did not earn.
func busyFromGaps(turns []Turn) int64 {
	var total int64
	for i := 1; i < len(turns); i++ {
		prev, cur := turns[i-1], turns[i]
		if cur.Role != RoleAssistant || prev.At.IsZero() || cur.At.IsZero() {
			continue
		}
		gap := cur.At.Sub(prev.At)
		if gap <= 0 {
			continue
		}
		// One gap can dwarf the rest: a session suspended mid-turn leaves the
		// hours it was closed behind as a single assistant-to-assistant gap. A
		// real capture had three of them in one turn. Clamping counts a long
		// tool run as busy without inheriting a closed laptop.
		if gap > maxGap {
			gap = maxGap
		}
		total += gap.Milliseconds()
	}
	return total
}

// summedUsage totals the per-turn usage. Because usage is attached to a turn
// only when its message id is first seen, this sum counts each message once.
func (s *Session) summedUsage() Usage {
	var u Usage
	for i := range s.Turns {
		if t := s.Turns[i].Usage; t != nil {
			u.InputTokens += t.InputTokens
			u.OutputTokens += t.OutputTokens
			u.CacheReadTokens += t.CacheReadTokens
			u.CacheCreateTokens += t.CacheCreateTokens
		}
	}
	return u
}

func usageFromModelTotals(mu map[string]rawModelUsage) Usage {
	var u Usage
	for _, m := range mu {
		u.InputTokens += m.InputTokens
		u.OutputTokens += m.OutputTokens
		u.CacheReadTokens += m.CacheReadInputTokens
		u.CacheCreateTokens += m.CacheCreationInputTokens
		u.ThinkingTokens += m.ThinkingTokens
	}
	return u
}

func joinText(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n" + b
	}
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// FinalOutput is the last non-empty assistant prose: the answer, for the
// purposes of "did the new attempt conclude the same thing?".
func (s *Session) FinalOutput() string {
	for i := len(s.Turns) - 1; i >= 0; i-- {
		if s.Turns[i].Role == RoleAssistant && strings.TrimSpace(s.Turns[i].Text) != "" {
			return strings.TrimSpace(s.Turns[i].Text)
		}
	}
	return ""
}
