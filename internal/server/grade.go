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

// judgeGrader is the grader this server reaches verdicts with when the task
// declares none.
//
// A grader declaration is more than a type name: the endpoint, the model and
// the name of the credential variable are what make a call possible at all, and
// a server started with --grader-config-file was told all three. Grading with
// the built-in defaults instead would send the request to api.anthropic.com
// with a key that belongs to someone else's gateway -- which arrives at the page
// as "no credentials", the least helpful description of a solved problem.
func (s *Server) judgeGrader() dataset.GraderSpec {
	if s.opts.Judge != nil && strings.TrimSpace(s.opts.Judge.Type) != "" {
		return *s.opts.Judge
	}
	return builtinJudge()
}

// builtinJudge is what every server can grade with, told nothing.
func builtinJudge() dataset.GraderSpec {
	return dataset.GraderSpec{Type: replay.DefaultGrader, Config: replay.DefaultGraderConfig()}
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

// gradeRefresh is how often a page with grading in flight reloads itself.
//
// Three seconds is the interval the session pages already use for a replay. The
// mechanism is the same on purpose: a reader who has watched one "still
// happening" page in this tool should not have to learn a second one, and a
// meta refresh needs no script, no endpoint and no state of its own.
const gradeRefresh = "3"

// gradeStatus is what the page says about the cell named in the URL.
//
// It has three states and they are not the same sentence: still working, failed
// without writing anything, or done with a verdict to report. It is read from
// the rows -- the job and the cell -- never from the query string, so a crafted
// link produces either nothing or the truth.
type gradeStatus struct {
	Notice  *report.Notice
	Refresh string
	// Job is the request the notice is about, so the page can mark the row it
	// is working on rather than only saying so in a banner.
	Job *store.GradeJob
}

func (s *Server) gradeStatus(r *http.Request, reg store.Report) gradeStatus {
	task := strings.TrimSpace(r.URL.Query().Get("graded"))
	if task == "" {
		return gradeStatus{}
	}
	runID, err := runOf(reg)
	if err != nil {
		return gradeStatus{}
	}
	sample := -1
	if raw := strings.TrimSpace(r.URL.Query().Get("sample")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			sample = n
		}
	}
	rows, err := s.st.ListAttempts(r.Context(), runID)
	if err != nil {
		return gradeStatus{}
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
		return gradeStatus{}
	}

	job, err := s.st.LatestGradeJob(r.Context(), runID, row.TaskID, row.Sample)
	if err != nil {
		job = nil
	}
	switch {
	case job != nil && job.Active():
		return gradeStatus{
			Notice:  runningNotice(*row, *job),
			Refresh: gradeRefresh,
			Job:     job,
		}
	case job != nil && job.Status == store.GradeFailed:
		return gradeStatus{Notice: failedNotice(*row, *job), Job: job}
	default:
		return gradeStatus{Notice: verdictNotice(*row, job), Job: job}
	}
}

// runningNotice is what a page says while the work is in flight.
//
// It names the cell and says the page is following it, because "grading" with
// no end in sight is indistinguishable from a page that forgot.
func runningNotice(a store.Attempt, job store.GradeJob) *report.Notice {
	where := cellName(a)
	state := "queued"
	if job.Status == store.GradeRunning {
		state = "running"
	}
	by := job.Grader
	if by == "" || by == "auto" {
		by = "the graders the dataset declares"
	} else {
		by = "the " + by + " grader"
	}
	note := fmt.Sprintf("%s is being graded by %s (%s). This page reloads every %s seconds and will show "+
		"the verdict when the row has one; nothing else on the page changes in the meantime.",
		where, by, state, gradeRefresh)
	return &report.Notice{Severity: "info", Head: "Grading this cell.", Note: note}
}

// failedNotice reports a request that reached no verdict.
//
// The important half is that the cell is unchanged: a reader who pressed the
// button has to be able to tell "the grade disagreed with me" from "the grade
// never happened", and the row still says what it said before.
func failedNotice(a store.Attempt, job store.GradeJob) *report.Notice {
	reason := strings.TrimSpace(job.Error)
	if reason == "" {
		reason = "no reason was recorded"
	}
	return &report.Notice{
		Severity: "warn",
		Head:     "The grade did not finish.",
		Note: fmt.Sprintf("%s was not re-graded: %s. The row below is unchanged -- it still holds the verdict "+
			"it had before this request.", cellName(a), reason),
	}
}

// verdictNotice phrases one row's verdict for the banner.
func verdictNotice(a store.Attempt, job *store.GradeJob) *report.Notice {
	where := cellName(a)
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
	// Before the graders, not after: a reader looking at a green card over a
	// red row needs the two numbers explained next to each other, and "Reached
	// by llm_judge" is not an explanation.
	if cut := graderCutoffs(a); cut != "" {
		parts = append(parts, cut)
	}
	if by := gradedBy(a); by != "" {
		parts = append(parts, "Reached by "+by+".")
	}
	if failed := failedGraders(a); failed != "" {
		parts = append(parts, "Could not run: "+failed+".")
	}
	if spend := spendNote(job); spend != "" {
		parts = append(parts, spend)
	}
	parts = append(parts, "The table below is rendered from that row; the banner beside it says whether this "+
		"page still matches the registration.")
	return &report.Notice{Severity: sev, Head: head, Note: strings.Join(parts, " ")}
}

// graderCutoffs states the cutoffs the graders applied to their own scores.
//
// A cell and its judge can disagree without either being wrong. The judge
// compares its score to the cutoff in its own config; the cell compares the same
// score to the line the run was given. The page used to show only the cell's
// number while the drawer showed the judge's green tick, so a reader saw two
// verdicts about one row and nothing saying they answer different questions.
//
// This is that statement, and it is written from the recorded results rather
// than recomputed: the cutoff to explain is the one that produced the verdict on
// this row, not whatever today's default would give.
func graderCutoffs(a store.Attempt) string {
	var parts []string
	for _, g := range a.Grades {
		if g.Error != "" {
			continue
		}
		cut, ok := detailNumber(g.Details["threshold"])
		if !ok || cut <= 0 {
			continue
		}
		verdict := "fail"
		if g.Passed {
			verdict = "pass"
		}
		// The card's verdict is not always score >= cutoff: a judge that
		// asserts "passed" overrides its own arithmetic down to half the cutoff.
		// So the sentence names the cutoff and the verdict separately rather
		// than inviting a reader to check a sum that was never the rule.
		parts = append(parts, fmt.Sprintf("%s's own cutoff is %.2f and it reads %s on this work",
			g.Grader, cut, verdict))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + " -- that cutoff is the grader's own standard, the threshold above " +
		"is the line this cell was run against, and the verdict comes from that one."
}

// detailNumber reads a number out of a grader's recorded details.
//
// It is written against the shapes a JSON round trip can produce -- the row is
// decoded from the database, not held in memory -- and refuses everything else
// rather than printing a guess.
func detailNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// spendNote says what the judgement itself cost, when it cost anything.
//
// It is the number that was missing: a page could say which grader reached a
// verdict and nothing about what reaching it spent, so a reader comparing a
// cheap grader with an expensive one had to go to the database. Zero tokens
// means no model was called -- patch and exact_match spend nothing -- and that
// is worth stating rather than hiding, because it is the difference between a
// free verdict and a paid one.
func spendNote(job *store.GradeJob) string {
	if job == nil || job.Status != store.GradeDone {
		return ""
	}
	if job.InputTokens == 0 && job.OutputTokens == 0 {
		return fmt.Sprintf("It cost no model tokens and took %s.", durationText(job.LatencyMs))
	}
	model := job.Model
	if model == "" {
		model = "the judge's model"
	}
	return fmt.Sprintf("It cost %d input and %d output tokens on %s and took %s.",
		job.InputTokens, job.OutputTokens, model, durationText(job.LatencyMs))
}

// durationText renders a duration the way a reader of one grade wants it: a
// fast grader in milliseconds rather than "0.0s", a slow one in seconds.
func durationText(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// cellName is how the page refers to one cell.
func cellName(a store.Attempt) string {
	if a.Sample > 0 {
		return fmt.Sprintf("Task %s, sample %d", a.TaskID, a.Sample)
	}
	return fmt.Sprintf("Task %s", a.TaskID)
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
	specs, err := gradeSelection(r.PostForm.Get("grader"), s.judgeGrader())
	if err != nil {
		s.refuseGrade(w, http.StatusBadRequest, reg, "That grader is not one this page offers.", err.Error())
		return
	}
	opts := regrade.Options{
		Graders: specs,
		// A collected task declares no graders of its own, and "grade this"
		// from a page means "reach a verdict", not "tell me the dataset has
		// none". The judge is the criterion that needs no ground truth, which
		// is why the replay button attaches it too -- and it is the judge this
		// server was started with, not a default that ignores the endpoint it
		// was configured to call.
		Fallback: []dataset.GraderSpec{s.judgeGrader()},
	}

	// Everything that can be refused is refused here, while the reader is
	// still on the page that would have to explain it: a judge with no
	// credentials, a cell that never ran, a dataset that moved. Accepting those
	// as a request would answer "303" and then fail a minute later on a page
	// the reader has left.
	//
	// What is left to fail later is the grading itself -- a model that answers
	// nothing, a call that times out -- and that is a fact about this request,
	// which is what the job row is for.
	resolved, err := regrade.Check(r.Context(), s.st, runID, task, sample, opts)
	if err != nil {
		s.refuseGrade(w, http.StatusConflict, reg, "This cell cannot be graded.", err.Error())
		return
	}
	sample = resolved

	// One live request per cell: a second press is answered with the request
	// that is already running rather than with a second paid comparison of the
	// same work. The check here is the polite half; the partial index on
	// grade_jobs is the half that holds under two simultaneous presses.
	if running, err := s.st.ActiveGradeJob(r.Context(), runID, task, sample); err == nil && running != nil {
		http.Redirect(w, r, gradedURL(reg.ID, running.TaskID, running.Sample), http.StatusSeeOther)
		return
	}

	job, err := regrade.Start(r.Context(), s.st, store.GradeRequest{
		ReportID: reg.ID,
		RunID:    runID,
		TaskID:   task,
		Sample:   sample,
		Grader:   strings.TrimSpace(r.PostForm.Get("grader")),
	})
	if err != nil {
		// Lost a race with another press for the same cell. That is not a
		// failure the reader can act on: the work they asked for is happening.
		if running, aerr := s.st.ActiveGradeJob(r.Context(), runID, task, sample); aerr == nil && running != nil {
			http.Redirect(w, r, gradedURL(reg.ID, running.TaskID, running.Sample), http.StatusSeeOther)
			return
		}
		s.failure(w, http.StatusInternalServerError, "The grade could not be recorded.",
			fmt.Sprintf("Writing the request to %s failed: %v", s.st.Path(), err))
		return
	}

	// Post/redirect/get, for the same reason as before: a verdict may cost an
	// API call, and a reload of the page it lands on must not pay for another.
	// The work is not done when this returns -- the page says "grading" and
	// follows the row until it is.
	s.grades.submit(job, opts)
	http.Redirect(w, r, gradedURL(reg.ID, job.TaskID, job.Sample), http.StatusSeeOther)
}

// gradeSelection turns the picker's value into grader specs.
//
// An empty value means the same as "auto" -- a form that lost the field, or a
// page rendered by an older build, falls back to the documented policy rather
// than to an error nobody can act on.
//
// judge is the grader the server was started with; the picker's llm_judge entry
// means that one when it is a judge at all, because the declaration is what
// carries the endpoint and credential its calls need.
func gradeSelection(choice string, judge dataset.GraderSpec) ([]dataset.GraderSpec, error) {
	switch v := strings.TrimSpace(choice); v {
	case "", "auto":
		// Nil: the dataset's own graders, which is the reproducible choice.
		return nil, nil
	case replay.DefaultGrader:
		if judge.Type != replay.DefaultGrader {
			judge = builtinJudge()
		}
		return []dataset.GraderSpec{judge}, nil
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
