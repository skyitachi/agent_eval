package report

import (
	"bytes"
	"embed"
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
)

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

type pageView struct {
	Title       string
	Mode        string
	Home        string
	Nav         []NavLink
	MetaRefresh string
	Standalone  bool
	AssetBase   string
	// CSS and JS use the context-specific safe types. html/template picks its
	// escaper from the surrounding element, so a template.HTML injected into
	// <style> would be re-escaped as CSS text and silently corrupt the
	// stylesheet -- template.CSS and template.JS are the types it accepts
	// verbatim there.
	CSS     template.CSS
	JS      template.JS
	Payload template.JS
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

// RenderIndex writes the run listing.
func RenderIndex(w io.Writer, idx *Index, opts RenderOptions) error {
	if idx == nil {
		return fmt.Errorf("report: index is nil")
	}
	return render(w, ModeIndex, idx, idx.Title, opts)
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
		Title:       title,
		Mode:        string(mode),
		Home:        opts.Home,
		Nav:         opts.Nav,
		MetaRefresh: opts.MetaRefresh,
		Standalone:  opts.Standalone,
		AssetBase:   opts.AssetBase,
		Payload:     js,
	}
	if opts.Standalone {
		view.CSS = template.CSS(CSS()) //nolint:gosec // embedded at build time, not user input
		view.JS = template.JS(JS())    //nolint:gosec // embedded at build time, not user input
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
