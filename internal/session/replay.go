package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// IsReplayWorkspace reports whether cwd is a workspace a replay created.
//
// A replay runs its agent in
//
//	<replay work root>/<replay id>/work/<run id>/<session>/s<sample>
//
// (see internal/runner's plan). The work root is configurable and may have any
// name, so the test is on the shape rather than on a prefix: an id of exactly
// 16 hex digits -- what newID makes a replay -- directly above a "work"
// directory, then the run's two directories, ending in the sample directory.
// A recording cannot have been made in such a directory unless a replay of
// some session put it there.
func IsReplayWorkspace(cwd string) bool {
	if cwd == "" {
		return false
	}
	parts := strings.Split(filepath.Clean(cwd), string(filepath.Separator))
	// ... <replay id> work <run id> <session> s<sample>
	n := len(parts)
	if n < 5 {
		return false
	}
	if !isReplayID(parts[n-5]) || parts[n-4] != "work" {
		return false
	}
	sample := parts[n-1]
	if len(sample) < 2 || sample[0] != 's' {
		return false
	}
	_, err := strconv.Atoi(sample[1:])
	return err == nil
}

// isReplayID reports whether s is the 16 lowercase hex digits a replay id is.
func isReplayID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// IsReplayTranscript reports whether the transcript at path was written by a
// replay this tool ran. Such a transcript is a result of the replay already,
// so it is not a recording to list or collect again.
//
// It reads only until the first record that names the working directory, which
// is the first or second line of a transcript. A listing runs this over every
// transcript on the machine, so it must not pay for the rest of scanHead.
func IsReplayTranscript(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)
	for n := 0; sc.Scan() && n < headLines; n++ {
		var rec rawRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.IsSidechain {
			continue
		}
		if rec.Cwd != "" {
			return IsReplayWorkspace(rec.Cwd), nil
		}
	}
	return false, sc.Err()
}
