package grader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/skyitachi/agenteval/internal/dataset"
	"github.com/skyitachi/agenteval/internal/sandbox"
)

// Artifact names written into HarnessDir for every graded attempt.
//
// This is a stable, documented contract: custom `command` graders and humans
// debugging a run read the same files, and the names do not change within a
// major release.
const (
	ArtifactDiff    = "diff.patch"
	ArtifactAnswer  = "answer.txt"
	ArtifactTrace   = "trace.json"
	ArtifactAttempt = "attempt.json"
	ArtifactTask    = "task.json"
	ArtifactGrade   = "grade.json"
)

// WriteArtifacts materialises the standard bundle of run outputs inside the
// sandbox at HarnessDir, and returns the workspace-relative directory.
//
// It is idempotent, so calling it from several graders simply rewrites the same
// files. An individual artifact failing to serialise is not fatal: the bundle is
// a convenience for graders and for debugging, not the source of truth.
func WriteArtifacts(gc *Context) (string, error) {
	dir := HarnessDir
	if gc.Sandbox == nil {
		return dir, nil
	}
	if diff, err := gc.Diff(); err == nil {
		if err := gc.Sandbox.WriteFile(filepath.Join(dir, ArtifactDiff), []byte(diff), 0o644); err != nil {
			return dir, fmt.Errorf("write %s: %w", ArtifactDiff, err)
		}
	}
	if err := gc.Sandbox.WriteFile(filepath.Join(dir, ArtifactAnswer),
		[]byte(gc.FinalAnswer()), 0o644); err != nil {
		return dir, fmt.Errorf("write %s: %w", ArtifactAnswer, err)
	}
	if tr := gc.Trace(); tr != nil {
		if raw, err := json.MarshalIndent(tr, "", "  "); err == nil {
			_ = gc.Sandbox.WriteFile(filepath.Join(dir, ArtifactTrace), raw, 0o644)
		}
	}
	if gc.Attempt != nil {
		if raw, err := json.MarshalIndent(gc.Attempt, "", "  "); err == nil {
			_ = gc.Sandbox.WriteFile(filepath.Join(dir, ArtifactAttempt), raw, 0o644)
		}
	}
	if raw, err := json.MarshalIndent(gc.Task, "", "  "); err == nil {
		_ = gc.Sandbox.WriteFile(filepath.Join(dir, ArtifactTask), raw, 0o644)
	}
	return dir, nil
}

// CopyInto copies a dataset-relative file or directory into the sandbox.
//
// Used by graders that need extra inputs -- hidden test suites, reference data --
// which must not be visible to the agent while it works.
func CopyInto(gc *Context, datasetRel, target string) error {
	if datasetRel == "" {
		return nil
	}
	src := datasetRel
	if !filepath.IsAbs(src) {
		src = filepath.Join(gc.Task.DatasetDir, src)
	}
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("artifact source %s: %w", datasetRel, err)
	}
	if !info.IsDir() {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		rel := target
		switch {
		case rel == "":
			rel = filepath.Base(src)
		case filepath.Ext(rel) == "":
			// A directory-ish target gets the file appended.
			rel = filepath.Join(rel, filepath.Base(src))
		}
		return gc.Sandbox.WriteFile(rel, data, 0o644)
	}
	if err := sandbox.CopyDirInto(src, gc.Sandbox.Root(), target); err != nil {
		return fmt.Errorf("copy %s into sandbox: %w", datasetRel, err)
	}
	return nil
}

// describeTask renders a compact one-line task label for grader reasons.
func describeTask(t dataset.TaskSpec) string {
	if t.Category == "" {
		return t.ID
	}
	return t.ID + " (" + t.Category + ")"
}
