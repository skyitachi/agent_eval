package server

// The body templates of the session and replay pages.
//
// Everything interpolated here goes through html/template's contextual
// escaping. The class names are the report pages' own vocabulary (app.css):
// .kv lists, .ev/.bubble turns, .toolcard tool calls. The one place raw values
// would reach a script context is Links, which is a pre-built
// template-escaped-by-construction fragment -- see the Links field.

const navSessions = "sessions"
const navReplays = "replays"

const sessionsBody = `<div class="section">
  <h2>Sessions</h2>
  <p class="muted">{{.Total}} session(s) on this machine, newest first. Claude Code and Cursor histories are merged.</p>
  {{if not .Entries}}
  <div class="empty">No sessions found. The histories are read from this machine's Claude Code and Cursor data.</div>
  {{else}}
  <div class="tablewrap"><table>
    <thead><tr><th>name</th><th>source</th><th>id</th><th>when</th></tr></thead>
    <tbody>
    {{range .Entries}}
      <tr>
        <td><a href="{{.Href}}">{{.Name}}</a><div class="muted small">{{.Muted}}</div></td>
        <td>{{.Source}}</td>
        <td class="mono-sm">{{.ID}}</td>
        <td class="nowrap">{{.WhenShort}}</td>
      </tr>
    {{end}}
    </tbody>
  </table></div>
  <div class="filters">
    {{if .HasPrev}}<a class="btn ghost" href="{{.PrevHref}}">&larr; newer</a>{{end}}
    <span class="muted">page {{.Page}}</span>
    {{if .HasNext}}<a class="btn ghost" href="{{.NextHref}}">older &rarr;</a>{{end}}
  </div>
  {{end}}
</div>
`

const sessionBody = `<div class="section">
  <h2>{{.Title}}</h2>
  <dl class="kv">
    <dt>source</dt><dd>{{.Source}}</dd>
    <dt>cwd</dt><dd>{{.Cwd}}</dd>
    <dt>git</dt><dd>{{.Git}}</dd>
    <dt>when</dt><dd>{{.When}}</dd>
    <dt>models</dt><dd>{{.Models}}</dd>
    {{if .Tokens}}<dt>tokens</dt><dd>{{.Tokens}}</dd>{{end}}
    {{if .Cost}}<dt>cost</dt><dd>{{.Cost}}</dd>{{end}}
    {{if .Busy}}<dt>busy</dt><dd>{{.Busy}}</dd>{{end}}
  </dl>
</div>

<div class="section">
  <h2>Replay this session</h2>
  {{if .CanReplay}}
  <form method="post" action="{{.ReplayAction}}" class="card" style="padding:14px 16px">
    <input type="hidden" name="csrf" value="{{.CSRF}}">
    <div class="grid2" style="margin-bottom:14px">
      <div>
        <label for="harness" class="small muted">Harness</label><br>
        <select id="harness" name="harness" class="search" style="min-width:100%">
          {{range $i, $h := .Harnesses}}
          <option value="{{$h}}"{{if eq $i 0}} selected{{end}}>{{$h}}</option>
          {{end}}
        </select>
      </div>
      <div>
        <label for="skill" class="small muted">Skill</label><br>
        <select id="skill" name="skill" class="search" style="min-width:100%">
          {{range $i, $k := .Skills}}
          <option value="{{$k}}"{{if eq $i 0}} selected{{end}}>{{$k}}</option>
          {{end}}
        </select>
      </div>
      <div>
        <label for="model" class="small muted">Model</label><br>
        <input id="model" name="model" list="model-suggestions" placeholder="e.g. sonnet" required
               class="search" style="min-width:100%">
        <datalist id="model-suggestions">
          {{range .ModelSuggestions}}<option value="{{.}}">{{end}}
        </datalist>
      </div>
      <div>
        <label for="split" class="small muted">Split</label><br>
        <select id="split" name="split" class="search" style="min-width:100%">
          <option value="session">session (one task from the whole session)</option>
          <option value="turn">turn (one task per human prompt)</option>
        </select>
      </div>
    </div>
    <label class="small" style="display:block;margin-bottom:14px">
      <input type="checkbox" name="workspace_cwd" value="1"> bind each task to the directory its recording ran in (--workspace-cwd)
    </label>
    <div class="muted small" style="margin-bottom:14px">A replay collects this session into a dataset beside the database, runs the agent on this machine, and records a report.</div>
    <button type="submit" class="btn primary">Replay</button>
  </form>
  {{else}}
  <div class="notice warn">This session cannot be replayed: {{.WhyNot}}.</div>
  {{end}}
</div>

<div class="section">
  <h2>Prompts</h2>
  {{range .Prompts}}
  <div class="card" style="padding:10px 14px;margin-bottom:10px">
    <div class="ev-head"><span class="who user">human</span><span>#{{.Index}}</span>{{if .When}}<span>{{.When}}</span>{{end}}</div>
    <div class="bubble">{{.Text}}</div>
  </div>
  {{end}}
  {{if not .Prompts}}<div class="empty">This session has no human prompt.</div>{{end}}
</div>

<div class="section">
  <h2>Turns</h2>
  {{range .Turns}}
  <div class="ev {{if eq .Who "human"}}ev-tool{{else}}ev-text{{end}}">
    <div class="ev-head"><span class="who {{if eq .Who "human"}}user{{else}}assistant{{end}}">{{.Who}}</span>{{if .When}}<span>{{.When}}</span>{{end}}{{if .Meta}}<span class="tmeta">{{.Meta}}</span>{{end}}</div>
    {{if .Thinking}}
    <details><summary class="muted small">thinking</summary><div class="inner"><div class="bubble think">{{.Thinking}}</div></div></details>
    {{end}}
    {{if .Text}}<div class="bubble">{{.Text}}</div>{{end}}
    {{range .Tools}}
    <div class="toolcard">
      <div class="th"><span class="tname">{{.Name}}</span></div>
      <div class="tsec"><pre class="code">{{.Input}}</pre></div>
    </div>
    {{end}}
  </div>
  {{end}}
</div>
`

// replaysBody is the replay listing. The Links column is a fragment built by
// buildReplaysView: one report anchor, escaped at construction the way the
// report templates escape a hash -- the id inside it is the row's own stored
// value, not anything user-typed, and the surrounding markup is a literal.
const replaysBody = `<div class="section">
  <h2>Replays</h2>
  {{if not .Entries}}
  <div class="empty">No replays yet. Open a session and use its replay form.</div>
  {{else}}
  <div class="tablewrap"><table>
    <thead><tr><th>session</th><th>harness</th><th>skill</th><th>model</th><th>status</th><th>when</th><th></th></tr></thead>
    <tbody>
    {{range .Entries}}
      <tr>
        <td class="mono-sm"><a href="/replays/{{.ID}}">{{.SessionShort}}</a></td>
        <td>{{.Harness}}</td>
        <td>{{.Skill}}</td>
        <td class="mono-sm">{{.Model}}</td>
        <td><span class="pill {{.StatusClass}}">{{.Status}}</span>{{if .Error}}<div class="muted small">{{.Error}}</div>{{end}}</td>
        <td class="nowrap">{{.When}}</td>
        <td>{{.Links}}</td>
      </tr>
    {{end}}
    </tbody>
  </table></div>
  {{end}}
</div>
`

// sessionShellBody maps the page name to its body template. Short and flat:
// every page in sessionTitles has exactly one entry here.
var sessionShellBody = map[string]string{
	"sessions": sessionsBody,
	"session":  sessionBody,
	"replays":  replaysBody,
	"replay":   replayBody,
}

const replayBody = `<div class="section">
  <h2>Replay {{.ID}}</h2>
  <dl class="kv">
    <dt>status</dt><dd><span class="pill {{.StatusClass}}">{{.Status}}</span>{{if .Error}} <span class="errmessage">{{.Error}}</span>{{end}}</dd>
    <dt>session</dt><dd><a href="/sessions/{{.Source}}/{{.SessionID}}">{{.Source}}/{{.SessionID}}</a></dd>
    <dt>harness</dt><dd>{{.Harness}}</dd>
    <dt>skill</dt><dd>{{.Skill}}</dd>
    <dt>model</dt><dd>{{.Model}}</dd>
    <dt>split</dt><dd>{{.Split}}</dd>
    <dt>workspace-cwd</dt><dd>{{.WorkspaceCwd}}</dd>
    <dt>started</dt><dd>{{.When}}</dd>
  </dl>
  {{if .Error}}
  <div class="notice err"><b>Failed.</b> {{.Error}}</div>
  {{end}}
  {{if .ReportHref}}
  <p class="mt">
    <a class="btn primary" href="{{.ReportHref}}">Open the report</a>
    {{if .RunHref}}<a class="btn ghost" href="{{.RunHref}}">The run</a>{{end}}
  </p>
  {{end}}
</div>
`
