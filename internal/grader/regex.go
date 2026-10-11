package grader

import (
	"regexp"
	"sync"
)

// Regex compilation is repeated constantly during grading (per task, per grader,
// per answer), and patterns come from dataset files that are re-read on every
// run. The cache keeps that off the hot path.
var (
	regexMu    sync.Mutex
	regexCache = map[string]*regexp.Regexp{}
	regexErrs  = map[string]error{}
)

// compileCache compiles pattern, memoising both successes and failures.
func compileCache(pattern string) (*regexp.Regexp, error) {
	regexMu.Lock()
	defer regexMu.Unlock()
	if re, ok := regexCache[pattern]; ok {
		return re, nil
	}
	if err, ok := regexErrs[pattern]; ok {
		return nil, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		regexErrs[pattern] = err
		return nil, err
	}
	regexCache[pattern] = re
	return re, nil
}
