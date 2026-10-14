package report

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/skyitachi/agenteval/internal/store"
)

// defaultLinkPrefix is where a served listing points at one report.
//
// It is a parameter of LoadReports rather than a constant of the client so that
// the payload carries a link instead of the page deriving one: the same listing
// inlined into a file has no server to point at, and a client that builds the
// route itself cannot be told that.
const defaultLinkPrefix = "/r/"

// hashChars is how much of a payload hash the listing shows. Twelve hex
// characters tell two registrations of the same view apart at a glance, which
// is what the listing is for; proving anything is the page's job, and it shows
// the whole hash.
const hashChars = 12

// Request is what a registration records about the report that was produced.
//
// It holds the request and not the result. The result is the payload, and the
// payload is a function of the database -- storing it beside the database would
// be storing a second answer to the same question, one that stops being updated
// the moment the rows move. What cannot be re-derived is which view was asked
// for, in what words, and with what budget.
type Request struct {
	// Kind is one of the store.ReportKind constants.
	Kind string
	// Title is the document title as rendered.
	Title string
	// Subject is the human label of what the report was about. It is stored
	// because the thing it names can be deleted: a registration whose run is
	// gone still has to say which run it was, and no lookup can answer that
	// afterwards.
	Subject string
	// Params are the ids and switches the view was asked for. They are the
	// request; a key the view needs and does not find is an error when the
	// report is rebuilt, not a default.
	Params map[string]string
	// Options is the configuration that shaped the payload. It is resolved
	// before being stored, so replaying it cannot pick up a default that
	// differs from the one that applied.
	Options Options
}

// Save records that a report was produced, and returns the row it wrote.
//
// The id is generated here rather than derived from the request. Two
// registrations of the same view are two events -- the report was produced
// twice -- and a derived id would collapse them into one row whose hash was
// whichever ran last, erasing the pair of hashes that is the evidence of drift.
func Save(ctx context.Context, st *store.Store, req Request, payload any) (store.Report, error) {
	if strings.TrimSpace(req.Kind) == "" {
		return store.Report{}, fmt.Errorf("report: a registration needs a kind")
	}
	opts := req.Options.Resolve()

	hash, size, err := Fingerprint(payload)
	if err != nil {
		return store.Report{}, fmt.Errorf("report: fingerprint payload: %w", err)
	}
	// Marshalled without omitempty on the map itself, so a view with no
	// parameters stores {} rather than null. null and {} would both decode to
	// an empty map, but only one of them says the request was recorded.
	params, err := json.Marshal(nonNilParams(req.Params))
	if err != nil {
		return store.Report{}, fmt.Errorf("report: encode params: %w", err)
	}
	options, err := json.Marshal(opts)
	if err != nil {
		return store.Report{}, fmt.Errorf("report: encode options: %w", err)
	}
	id, err := newReportID()
	if err != nil {
		return store.Report{}, err
	}

	reg := store.Report{
		ID:           id,
		Kind:         req.Kind,
		Title:        req.Title,
		Subject:      req.Subject,
		ParamsJSON:   string(params),
		OptionsJSON:  string(options),
		CreatedAt:    opts.GeneratedAt,
		PayloadHash:  hash,
		PayloadBytes: int64(size),
		DataVersion:  DataVersion,
	}
	if err := st.SaveReport(ctx, reg); err != nil {
		return store.Report{}, err
	}
	return reg, nil
}

// newReportID returns a 64-bit random token, hex encoded.
//
// Random rather than sequential because one row per registration makes this a
// log, and a log that numbers its entries invites reading the number as a
// version. Random rather than content-addressed because the content is exactly
// what two registrations can share while still being two events.
func newReportID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("report: generate report id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ParseParams decodes a registration's request.
func ParseParams(reg store.Report) (map[string]string, error) {
	var params map[string]string
	if err := json.Unmarshal([]byte(reg.ParamsJSON), &params); err != nil {
		return nil, fmt.Errorf("report: registration %s has unreadable params: %w", reg.ID, err)
	}
	if params == nil {
		params = map[string]string{}
	}
	return params, nil
}

// ParseOptions decodes the options a registration was built with.
//
// It refuses rather than falling back to the defaults. The options are part of
// what identifies the payload: replayed with a different field budget, the same
// view rebuilds a different document, and reporting that difference as the data
// having moved would be a claim the reader could not see through. The
// generation stamp is required for the same reason and one more -- it is what
// makes the rebuilt document carry the time it was recorded, so a zero would
// restamp it with the time of reading and move the hash for nothing.
func ParseOptions(reg store.Report) (Options, error) {
	var opts Options
	if err := json.Unmarshal([]byte(reg.OptionsJSON), &opts); err != nil {
		return Options{}, fmt.Errorf("report: registration %s has unreadable options: %w", reg.ID, err)
	}
	if opts.GeneratedAt.IsZero() {
		return Options{}, fmt.Errorf(
			"report: registration %s records no generation time, so the document it describes cannot be rebuilt", reg.ID)
	}
	return opts, nil
}

// Derive rebuilds the payload a registration describes, and says which mode
// renders it.
//
// The options are the caller's, and the caller is expected to have taken them
// from ParseOptions: rebuilding with today's defaults would produce a different
// document and then present the difference as the database having moved. A
// parameter the view needs and does not find is an error, because an empty
// projection and a projection of nothing look the same on the page.
func Derive(ctx context.Context, st *store.Store, reg store.Report, opts Options) (any, Mode, error) {
	params, err := ParseParams(reg)
	if err != nil {
		return nil, "", err
	}
	switch reg.Kind {
	case store.ReportKindRun:
		id, err := requiredParam(reg, params, "run")
		if err != nil {
			return nil, "", err
		}
		data, err := Load(ctx, st, id, opts)
		return data, ModeRun, err

	case store.ReportKindCompare:
		left, err := requiredParam(reg, params, "left")
		if err != nil {
			return nil, "", err
		}
		right, err := requiredParam(reg, params, "right")
		if err != nil {
			return nil, "", err
		}
		data, err := LoadComparison(ctx, st, left, right, opts)
		return data, ModeCompare, err

	case store.ReportKindSweep:
		id, err := requiredParam(reg, params, "sweep")
		if err != nil {
			return nil, "", err
		}
		data, err := LoadSweep(ctx, st, id, opts)
		return data, ModeSweep, err

	case store.ReportKindIndex:
		// A listing with no limit is a listing of everything, which is what
		// LoadIndex does with 0. It is not a missing parameter: the report a
		// registration made with no --limit is exactly that document.
		limit := 0
		if raw := strings.TrimSpace(params["limit"]); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return nil, "", fmt.Errorf(
					"report: registration %s records a limit that is not a number (%q)", reg.ID, raw)
			}
			limit = n
		}
		idx, err := LoadIndex(ctx, st, limit, store.RunFilter{SweepID: params["sweep_id"]}, opts)
		return idx, ModeIndex, err
	}
	return nil, "", fmt.Errorf("report: registration %s names a report kind this build cannot rebuild (%q)",
		reg.ID, reg.Kind)
}

func requiredParam(reg store.Report, params map[string]string, key string) (string, error) {
	v := strings.TrimSpace(params[key])
	if v == "" {
		return "", fmt.Errorf("report: registration %s does not record which %s it was about, "+
			"so there is nothing to rebuild", reg.ID, key)
	}
	return v, nil
}

// LoadReports projects the registry as a listing.
//
// It reads only what the rows stored. Rebuilding each payload to find out
// whether it still reproduces would cost a full projection per row, and a
// listing whose cost grows with the number of reports is a listing that stops
// being openable -- so the drift verdict is on the page a reader gets by
// opening one.
func LoadReports(ctx context.Context, st *store.Store, opts Options, limit int, linkPrefix string) (*Reports, error) {
	opts = opts.Resolve()
	if linkPrefix == "" {
		linkPrefix = defaultLinkPrefix
	}
	rows, err := st.ListReports(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := &Reports{
		Version:   DataVersion,
		Generated: opts.GeneratedAt.UTC().Format(time.RFC3339),
		Title:     "Recorded reports",
		// An empty slice, not nil: the page distinguishes "nothing has been
		// recorded" from a listing that failed to load, and null would make the
		// two look alike in the payload.
		Entries: []ReportEntry{},
	}
	if opts.Title != "" {
		out.Title = opts.Title
	}
	for _, r := range rows {
		out.Entries = append(out.Entries, ReportEntry{
			ID:          r.ID,
			Kind:        r.Kind,
			Title:       r.Title,
			Subject:     r.Subject,
			CreatedAt:   r.CreatedAt.UTC().Format(time.RFC3339),
			Hash:        shortenHash(r.PayloadHash),
			Href:        linkPrefix + r.ID,
			Bytes:       r.PayloadBytes,
			DataVersion: r.DataVersion,
			// A registration from a build with a different payload format can
			// never reproduce, and saying so before it is opened is what keeps
			// that from reading as the data having changed.
			OlderFormat: r.DataVersion != DataVersion,
		})
	}
	return out, nil
}

func shortenHash(h string) string {
	if len(h) <= hashChars {
		return h
	}
	return h[:hashChars]
}

func nonNilParams(p map[string]string) map[string]string {
	if p == nil {
		return map[string]string{}
	}
	return p
}
