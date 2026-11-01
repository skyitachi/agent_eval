package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/regrade"
	"github.com/skyitachi/agenteval/internal/replay"
	"github.com/skyitachi/agenteval/internal/report"
	"github.com/skyitachi/agenteval/internal/store"
)

// # Grading one cell from its own page
//
// The page already knows everything the verdict needs, because it is rendered
// from the row that holds it: the diff, the answer, the normalized trajectory.
// What it could not do was write. POST /r/{id}/grade is that write, and it is
// deliberately the narrowest one possible -- one cell, one verdict, the run
// untouched, the agent not re-run.
//
// Re-running would answer a different question. A second attempt is different
// work, differently paid for, and the verdict would belong to it rather than to
// the attempt the reader is looking at.
//
// # What is refused
//
// A grader that executes inside the workspace (pytest, command) needs a sandbox
// a finished run no longer has; regrade refuses those by name rather than
// running them against nothing. A judge with no credentials is refused before
// anything is written, for the same reason: an error produced by the harness
// must not be recorded as a failure produced by the agent.
//
// # Guarded like the session pages
//
// A POST here writes to the database and may spend an API call, and it works
// against a server bound to loopback -- which is reachable from a page on any
// site the reader has open. So the same three checks apply: a Host this server
// can be reached by, an Origin that is this server when the browser sends one,
// and a form token only a page this server rendered knows.

// gradeChoices lists the graders the picker offers, in order.
//
// The values are the contract: the server accepts exactly these and refuses
// anything else by name. "auto" is a policy rather than a grader -- the graders
// the dataset declares, and the judge when it declares none -- which is what a
// reader pressing the button on a collected task means.
func gradeChoices() []string {
	return []string{"auto", replay.DefaultGrader, "patch"}
}

// gradeAction is what the page needs to offer the control, or nil when the page
// cannot be graded.
//
// Only a single-run report can: a comparison spans two runs and a sweep spans
// several, so "which cell" is a question the page does not answer by itself.
//
// The mode is the only check needed, and that is a property of the caller
// rather than of this function: Derive refuses a run registration that records
// no run, so a page that reaches here with ModeRun has one. A second check on
// the params read like a guard and was unreachable -- a mutation that deleted
// it left every test green, which is how it was found.
func (s *Server) gradeAction(reg store.Report, mode report.Mode) *report.GradeAction {
	if mode != report.ModeRun {
		return nil
	}
	return &report.GradeAction{
		URL:     "/r/" + reg.ID + "/grade",
		Token:   s.csrf,
		Graders: gradeChoices(),
	}
}

// runOf is the run a registration is about, or why it has none.
func runOf(reg store.Report) (string, error) {
	params, err := report.ParseParams(reg)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(params["run"])
	if id == "" {
		return "", fmt.Errorf("this report records no run, so there is no cell to grade")
	}
	return id, nil
}

// gradeNotice is the banner a page shows after a verdict was reached.
//
// It is composed from the row, never from the URL. A link can carry
// ?graded=<id>; it cannot make a row say something the row does not hold, so a
// crafted link produces either nothing or the truth.
func (s *Server) gradeNotice(r *http.Request, reg store.Report) *report.Notice {
	task := strings.TrimSpace(r.URL.Query().Get("graded"))
	if task == "" {
		return nil
	}
	runID, err := runOf(reg)
	if err != nil {
		return nil
	}
	sample := -1
	if raw := strings.TrimSpace(r.URL.Query().Get("sample")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			sample = n
		}
	}
	rows, err := s.st.ListAttempts(r.Context(), runID)
	if err != nil {
		return nil
	}
	var row *store.Attempt
	for i := range rows {
		a := &rows[i]
		if a.TaskID != task || (sample >= 0 && a.Sample != sample) {
			continue
		}
		if row == nil || a.Sample < row.Sample {
			row = a
		}
	}
	if row == nil {
		return nil
	}
	return verdictNotice(*row)
}

// verdictNotice phrases one row's verdict for the banner.
func verdictNotice(a store.Attempt) *report.Notice {
	where := fmt.Sprintf("Task %s", a.TaskID)
	if a.Sample > 0 {
		where = fmt.Sprintf("Task %s, sample %d", a.TaskID, a.Sample)
	}
	switch {
	case a.Status == store.CellError:
		return &report.Notice{
			Severity: "warn",
			Head:     "This cell never ran.",
			Note: fmt.Sprintf("%s stopped before the agent produced anything (%s), so there is nothing whose "+
				"quality could be judged. Grading it would be a statement about the harness wearing the agent's name.",
				where, firstNonEmpty(a.Error, "no reason recorded")),
		}
	case !a.Graded:
		return &report.Notice{
			Severity: "warn",
			Head:     "The cell still holds no verdict.",
			Note: fmt.Sprintf("%s was reached but not scored. The row is what this page renders, so the table "+
				"below is what the database holds.", where),
		}
	}

	sev, head := "warn", "The verdict on record is FAIL."
	if a.Passed {
		sev, head = "info", "The verdict on record is PASS."
	}
	parts := []string{fmt.Sprintf("%s scores %.2f against a threshold of %.2f.", where, a.Score, a.Threshold)}
	if by := gradedBy(a); by != "" {
		parts = append(parts, "Reached by "+by+".")
	}
	if failed := failedGraders(a); failed != "" {
		parts = append(parts, "Could not run: "+failed+".")
	}
	parts = append(parts, "The table below is rendered from that row; the banner beside it says whether this "+
		"page still matches the registration.")
	return &report.Notice{Severity: sev, Head: head, Note: strings.Join(parts, " ")}
}

// gradedBy names the graders that produced the verdict on a row, in the order
// they ran. Names rather than types, because two graders of the same type are
// distinguishable only by name.
func gradedBy(a store.Attempt) string {
	names := make([]string, 0, len(a.Grades))
	for _, g := range a.Grades {
		if g.Error != "" {
			continue
		}
		names = append(names, g.Grader)
	}
	return strings.Join(names, ", ")
}

func failedGraders(a store.Attempt) string {
	var names []string
	for _, g := range a.Grades {
		if g.Error != "" {
			names = append(names, g.Grader+" ("+g.Error+")")
		}
	}
	return strings.Join(names, ", ")
}

func firstNonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// gradeGuard applies the checks every write here needs.
//
// The report pages themselves are read without any of them, as they were: a
// reader who reached this server still reads every report. What only a write
// needs is the Host check -- a page whose DNS was pointed here would otherwise
// be same-origin and could read the token out of the page and then use it.
func (s *Server) gradeGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host) {
			s.failure(w, http.StatusForbidden, "This host name is not served.",
				fmt.Sprintf("Grading answers only on localhost or an IP address, and this request named %q. "+
					"A page that reaches this server under another name is one whose DNS was pointed here.", r.Host))
			return
		}
		guardHeaders(w)
		if !s.checkPost(w, r) {
			return
		}
		next(w, r)
	}
}

func (s *Server) handleGrade(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reg, err := s.st.GetReport(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.missingReport(w, r, id)
			return
		}
		s.failure(w, http.StatusInternalServerError, "The report could not be read.",
			fmt.Sprintf("Reading the registry from %s failed: %v", s.st.Path(), err))
		return
	}

	// The kind comes first: a comparison report also carries ids, and answering
	// "no run recorded" for one would send the reader looking for a missing
	// parameter instead of at the reason.
	if reg.Kind != store.ReportKindRun {
		s.refuseGrade(w, http.StatusBadRequest, reg, "Only a single-run report can be graded from its page.",
			fmt.Sprintf("This page is a %s report: it aggregates several runs, so \"which cell\" is a question "+
				"it does not answer. Open the run's own report and grade there.", reg.Kind))
		return
	}
	runID, err := runOf(reg)
	if err != nil {
		s.refuseGrade(w, http.StatusBadRequest, reg, "This report has no run to grade.", err.Error())
		return
	}

	task := strings.TrimSpace(r.PostForm.Get("task"))
	if task == "" {
		s.refuseGrade(w, http.StatusBadRequest, reg, "No task was named.",
			"The form posts the task id of the row whose button was pressed; this request carried none, "+
				"so there is no cell to grade.")
		return
	}
	sample := -1
	if raw := strings.TrimSpace(r.PostForm.Get("sample")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			s.refuseGrade(w, http.StatusBadRequest, reg, "That sample number is not a number.",
				fmt.Sprintf("This form sent sample %q; a sample is a non-negative integer.", raw))
			return
		}
		sample = n
	}
	specs, err := gradeSelection(r.PostForm.Get("grader"))
	if err != nil {
		s.refuseGrade(w, http.StatusBadRequest, reg, "That grader is not one this page offers.", err.Error())
		return
	}

	outcome, err := regrade.One(r.Context(), s.st, runID, task, sample, regrade.Options{
		Graders: specs,
		// A collected task declares no graders of its own, and "grade this"
		// from a page means "reach a verdict", not "tell me the dataset has
		// none". The judge is the criterion that needs no ground truth, which
		// is why the replay button attaches it too.
		Fallback: []dataset.GraderSpec{{
			Type:   replay.DefaultGrader,
			Config: replay.DefaultGraderConfig(),
		}},
	})
	if err != nil {
		// Nothing was written when this returns an error, so the page can say
		// why in the same breath as "the row is unchanged".
		s.refuseGrade(w, http.StatusConflict, reg, "This cell cannot be graded.", err.Error())
		return
	}
	// Post/redirect/get, because a verdict may have cost an API call: a reload
	// of the resulting page must not grade a second time.
	http.Redirect(w, r, gradedURL(reg.ID, outcome.TaskID, outcome.Sample), http.StatusSeeOther)
}

// gradeSelection turns the picker's value into grader specs.
//
// An empty value means the same as "auto" -- a form that lost the field, or a
// page rendered by an older build, falls back to the documented policy rather
// than to an error nobody can act on.
func gradeSelection(choice string) ([]dataset.GraderSpec, error) {
	switch v := strings.TrimSpace(choice); v {
	case "", "auto":
		// Nil: the dataset's own graders, which is the reproducible choice.
		return nil, nil
	case replay.DefaultGrader:
		return []dataset.GraderSpec{{Type: replay.DefaultGrader, Config: replay.DefaultGraderConfig()}}, nil
	case "patch":
		// No config: the defaults ask only that something changed and that it
		// is not a repository-wide rewrite, which is the question this grader
		// is on the list to answer.
		return []dataset.GraderSpec{{Type: "patch"}}, nil
	default:
		return nil, fmt.Errorf("%q is not a grader this page offers (%s). A grader that needs a live workspace "+
			"cannot run here at all: a finished run has none left.",
			v, strings.Join(gradeChoices(), ", "))
	}
}

// gradedURL is where the page comes back to. The fragment puts the reader at
// the table rather than at the top of a document they have already read.
func gradedURL(reportID, task string, sample int) string {
	q := url.Values{}
	q.Set("graded", task)
	q.Set("sample", strconv.Itoa(sample))
	return "/r/" + reportID + "?" + q.Encode() + "#tasks"
}

// refuseGrade answers a grade that did not happen, with the way back.
//
// It is a page rather than a status code for the same reason every other
// failure here is, and it keeps the report reachable: the reader arrived from
// one and should not have to reconstruct its URL from the address bar.
func (s *Server) refuseGrade(w http.ResponseWriter, status int, reg store.Report, title, message string) {
	s.failureWith(w, status, title, message, "/r/"+reg.ID, "Back to the report")
}
