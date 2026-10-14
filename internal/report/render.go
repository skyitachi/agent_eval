package report

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"strings"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets/app.css assets/app.js
var assetFS embed.FS

// CSS returns the report stylesheet.
func CSS() string { return assetString("assets/app.css") }

// JS returns the report client.
func JS() string { return assetString("assets/app.js") }

func assetString(name string) string {
	raw, err := assetFS.ReadFile(name)
	if err != nil {
		// Both files are embedded at build time, so a miss is a programming
		// error, not a runtime condition.
		panic("report: embedded asset missing: " + name)
	}
	return string(raw)
}

// Mode selects which view the client renders.
type Mode string

const (
	ModeRun     Mode = "run"
	ModeIndex   Mode = "index"
	ModeCompare Mode = "compare"
	// ModeSweep renders an attribution: one axis varied, one control, and the
	// paired differences the levels produced.
	ModeSweep Mode = "sweep"
	// ModeReports renders the listing of reports that were recorded.
	ModeReports Mode = "reports"
)

// Record is what a served page says about the registration it came from.
//
// It lives on the page shell rather than in the payload, and that placement is
// the point. The payload is the document that was hashed, so putting the record
// inside it would mean the page a reader opens is never byte-identical to the
// one that was recorded -- and the JSON endpoint would have to strip a field to
// be the same document twice. The shell sits outside the document, so the hash
// stays a claim about the page's contents alone.
type Record struct {
	// ID, CreatedAt and Hash are the registration's own facts, carried as they
	// were stored.
	ID        string
	CreatedAt string
	Hash      string
	// CurrentHash is what rebuilding the payload produced just now, and Match is
	// whether the two agree.
	//
	// Match is not the same claim as "nothing changed": a payload written in an
	// older format hashes differently with no data having moved at all, which is
	// why the note below says which of the two happened instead of leaving the
	// reader to guess.
	CurrentHash string
	Match       bool
	// Severity picks the banner's colour: "info", "warn" or "err".
	Severity string
	// Head and Note are the sentence the page shows. Written here because
	// deciding which combination deserves a warning is a judgement, and a
	// template that re-derived it would be a second, quieter copy of these
	// rules.
	Head string
	Note string
}

// NavLink is one header navigation entry.
type NavLink struct {
	Label  string
	Href   string
	Active bool
}

// RenderOptions shapes the emitted document.
type RenderOptions struct {
	// Title overrides the payload's own title.
	Title string
	// Standalone inlines the CSS and JS so the output is a single portable
	// file. The web server turns it off and serves the assets separately.
	Standalone bool
	// AssetBase is the URL prefix used when Standalone is false.
	AssetBase string
	// Home is where the brand link points.
	Home string
	// Nav adds header links, used by the server to expose the run list.
	Nav []NavLink
	// MetaRefresh adds a meta refresh, which is how a live index page follows a
	// run that is still going.
	MetaRefresh string
	// Record, when set, renders a banner above the document saying which
	// registration the page came from and whether it still reproduces.
	Record *Record
}

func (o RenderOptions) withDefaults() RenderOptions {
	if o.AssetBase == "" {
		o.AssetBase = "/static/"
	}
	if o.Home == "" {
		o.Home = "#"
	}
	return o
}

// shellHead is the vocabulary of the shared <head>.
//
// It is a type of its own because two pages need the head and only one of them
// has a payload, a nav or a mode. Sharing the template without sharing its
// inputs would mean a struct per page carrying fields the template mentions but
// that page never sets -- the failure page would have a Nav it always leaves
// empty, and nothing would keep that field honest.
type shellHead struct {
	Title       string
	MetaRefresh string
	Standalone  bool
	AssetBase   string
	// CSS uses the context-specific safe type. html/template picks its escaper
	// from the surrounding element, so a template.HTML injected into <style>
	// would be re-escaped as CSS text and silently corrupt the stylesheet --
	// template.CSS is the type it accepts verbatim there.
	CSS template.CSS
}

type pageView struct {
	Head   shellHead
	Mode   string
	Home   string
	Nav    []NavLink
	Record *Record
	// JS is the client, inlined when the document is standalone.
	JS      template.JS
	Payload template.JS
}

// errorView is the failure page. It carries no client script: a page that
// cannot be built has nothing for the client to render, and shipping app.js
// would have it paint "no reports recorded yet" over the reason why.
type errorView struct {
	Head     shellHead
	Message  string
	Home     string
	HomeText string
}

var pageTemplate = template.Must(template.New("page.html").ParseFS(templateFS, "templates/*.html"))

// ---------------------------------------------------------------------------
// Public renderers
// ---------------------------------------------------------------------------

// RenderRun writes a single-run report.
func RenderRun(w io.Writer, data *Data, opts RenderOptions) error {
	if data == nil {
		return fmt.Errorf("report: data is nil")
	}
	data.Version = DataVersion
	return render(w, ModeRun, data, data.Title, opts)
}

// RenderComparison writes a two-run comparison.
func RenderComparison(w io.Writer, data *Data, opts RenderOptions) error {
	if data == nil || data.Compare == nil {
		return fmt.Errorf("report: comparison payload is missing")
	}
	return render(w, ModeCompare, data, data.Title, opts)
}

// RenderSweep writes a factor-sweep attribution.
func RenderSweep(w io.Writer, data *Data, opts RenderOptions) error {
	if data == nil || data.Sweep == nil {
		return fmt.Errorf("report: sweep payload is missing")
	}
	return render(w, ModeSweep, data, data.Title, opts)
}

// RenderIndex writes the run listing.
func RenderIndex(w io.Writer, idx *Index, opts RenderOptions) error {
	if idx == nil {
		return fmt.Errorf("report: index is nil")
	}
	return render(w, ModeIndex, idx, idx.Title, opts)
}

// RenderReports writes the listing of recorded reports.
func RenderReports(w io.Writer, r *Reports, opts RenderOptions) error {
	if r == nil {
		return fmt.Errorf("report: report listing is nil")
	}
	return render(w, ModeReports, r, r.Title, opts)
}

// RenderPayload writes a payload in the mode that goes with it.
//
// It exists for the server, which derives a payload from a registration and so
// knows the mode but not the concrete type. The three renderers above type
// their argument, which is what turns a mismatched payload into a compile error
// everywhere else; this one cannot, so it checks and says what it got.
func RenderPayload(w io.Writer, mode Mode, payload any, opts RenderOptions) error {
	switch mode {
	case ModeRun:
		data, ok := payload.(*Data)
		if !ok {
			return fmt.Errorf("report: mode %s needs a *Data payload, got %T", mode, payload)
		}
		return RenderRun(w, data, opts)
	case ModeCompare:
		data, ok := payload.(*Data)
		if !ok {
			return fmt.Errorf("report: mode %s needs a *Data payload, got %T", mode, payload)
		}
		return RenderComparison(w, data, opts)
	case ModeSweep:
		data, ok := payload.(*Data)
		if !ok {
			return fmt.Errorf("report: mode %s needs a *Data payload, got %T", mode, payload)
		}
		return RenderSweep(w, data, opts)
	case ModeIndex:
		idx, ok := payload.(*Index)
		if !ok {
			return fmt.Errorf("report: mode %s needs an *Index payload, got %T", mode, payload)
		}
		return RenderIndex(w, idx, opts)
	case ModeReports:
		listing, ok := payload.(*Reports)
		if !ok {
			return fmt.Errorf("report: mode %s needs a *Reports payload, got %T", mode, payload)
		}
		return RenderReports(w, listing, opts)
	}
	return fmt.Errorf("report: no renderer for mode %q", mode)
}

// ErrorOptions is one failure page.
type ErrorOptions struct {
	// Title is the headline; Status is not shown, because a reader of an HTML
	// page has no use for a number they cannot act on -- the caller sets it on
	// the response and the page says what happened in words.
	Title string
	// Message explains the failure, and has to name the record: "not found" on
	// its own reads like a bug in the tool, while "no report with id X in
	// aeval.db" reads like the wrong database, which is what it usually is.
	Message string
	// Home and HomeText are the way back. Empty hides the link.
	Home     string
	HomeText string
}

// RenderError writes a failure page in the report's own shell.
//
// It is a page and not a text response because the caller is a browser: a
// reader who mistyped a link should land somewhere that looks like the tool,
// says which link was wrong, and offers the list.
func RenderError(w io.Writer, eo ErrorOptions, opts RenderOptions) error {
	opts = opts.withDefaults()
	if strings.TrimSpace(eo.Title) == "" {
		eo.Title = "Not found"
	}
	if eo.Home != "" && strings.TrimSpace(eo.HomeText) == "" {
		eo.HomeText = "All recorded reports"
	}
	view := errorView{
		Head: shellHead{
			Title:      eo.Title,
			Standalone: opts.Standalone,
			AssetBase:  opts.AssetBase,
		},
		Message:  eo.Message,
		Home:     eo.Home,
		HomeText: eo.HomeText,
	}
	if opts.Standalone {
		view.Head.CSS = template.CSS(CSS()) //nolint:gosec // embedded at build time, not user input
	}
	if err := pageTemplate.ExecuteTemplate(w, "error", view); err != nil {
		return fmt.Errorf("render error page: %w", err)
	}
	return nil
}

// RenderRunFile writes a standalone report to disk.
func RenderRunFile(path string, data *Data, opts RenderOptions) error {
	opts.Standalone = true
	return writeFile(path, func(w io.Writer) error { return RenderRun(w, data, opts) })
}

// RenderComparisonFile writes a standalone comparison to disk.
func RenderComparisonFile(path string, data *Data, opts RenderOptions) error {
	opts.Standalone = true
	return writeFile(path, func(w io.Writer) error { return RenderComparison(w, data, opts) })
}

// RenderSweepFile writes a standalone sweep attribution to disk.
func RenderSweepFile(path string, data *Data, opts RenderOptions) error {
	opts.Standalone = true
	return writeFile(path, func(w io.Writer) error { return RenderSweep(w, data, opts) })
}

// RenderIndexFile writes a standalone index to disk.
func RenderIndexFile(path string, idx *Index, opts RenderOptions) error {
	opts.Standalone = true
	return writeFile(path, func(w io.Writer) error { return RenderIndex(w, idx, opts) })
}

func writeFile(path string, fn func(io.Writer) error) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create report directory: %w", err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create report %s: %w", path, err)
	}
	if err := fn(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close report %s: %w", path, err)
	}
	return nil
}

// render executes the shared shell.
func render(w io.Writer, mode Mode, payload any, title string, opts RenderOptions) error {
	opts = opts.withDefaults()
	js, err := payloadJS(payload)
	if err != nil {
		return err
	}
	if strings.TrimSpace(title) == "" {
		title = "Evaluation report"
	}
	view := pageView{
		Head: shellHead{
			Title:       title,
			MetaRefresh: opts.MetaRefresh,
			Standalone:  opts.Standalone,
			AssetBase:   opts.AssetBase,
		},
		Mode:    string(mode),
		Home:    opts.Home,
		Nav:     opts.Nav,
		Record:  opts.Record,
		Payload: js,
	}
	if opts.Standalone {
		view.Head.CSS = template.CSS(CSS()) //nolint:gosec // embedded at build time, not user input
		view.JS = template.JS(JS())         //nolint:gosec // embedded at build time, not user input
	}
	if err := pageTemplate.ExecuteTemplate(w, "page", view); err != nil {
		return fmt.Errorf("render report: %w", err)
	}
	return nil
}

// FileName is the conventional output path for a run's report.
//
// The run id is sanitised because it can come from a configuration file, and
// turning it straight into a path would let "../../etc/x" write wherever it
// liked.
func FileName(runID string) string {
	var b strings.Builder
	for _, r := range runID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "run"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return "report-" + name + ".html"
}

// DataJSON serialises a payload exactly as it is embedded in the HTML.
//
// The web API returns this so a client sees the same document whether it read
// the file or called the server. Anything that diverges here would make the two
// front ends disagree about the same run.
func DataJSON(payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}
	return []byte(neutralize(raw)), nil
}

// payloadJS encodes a payload as a JavaScript literal safe to embed in a
// <script> element.
//
// json.Marshal escapes <, > and & by default, which is what makes this safe. The
// check below does not trust that: it re-escapes the three characters itself, so
// the guarantee holds even if a future caller reaches for
// Encoder.SetEscapeHTML(false). Two distinct attacks are being prevented --
// "</script>" or "<!--" terminating the element early, and the browser decoding
// an entity such as "&amp;" out of the payload and silently corrupting it.
func payloadJS(payload any) (template.JS, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode payload: %w", err)
	}
	return template.JS(neutralize(raw)), nil //nolint:gosec // neutralize removes every HTML-significant byte
}

// Fingerprint is the identity of a payload: a hash over the exact bytes the
// JSON endpoint returns, and how many of them there are.
//
// It hashes DataJSON rather than a fresh marshal so that the claim and the
// document cannot disagree. The two are the same bytes by construction, which
// turns "hashing the endpoint's body reproduces the recorded hash" from a test
// of the encoder's agreement with itself into a test of the invariant -- and it
// is the invariant that matters, because the hash is only worth storing if a
// reader can check it against something they can fetch.
func Fingerprint(payload any) (string, int, error) {
	raw, err := DataJSON(payload)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), len(raw), nil
}

// neutralize rewrites the three HTML-significant ASCII characters as JSON
// escapes.
//
// They can only occur inside JSON strings -- the JSON grammar uses none of them
// outside one -- so the substitution cannot change the document's structure.
func neutralize(raw []byte) string {
	if bytes.IndexAny(raw, "<>&") < 0 {
		return string(raw)
	}
	out := make([]byte, 0, len(raw)+16)
	for _, b := range raw {
		switch b {
		case '<':
			out = append(out, `\u003c`...)
		case '>':
			out = append(out, `\u003e`...)
		case '&':
			out = append(out, `\u0026`...)
		default:
			out = append(out, b)
		}
	}
	return string(out)
}
