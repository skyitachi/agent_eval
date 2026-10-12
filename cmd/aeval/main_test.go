package main

import "testing"

// TestHintArgKeepsOrdinaryPathsBare covers the common case and the reason the
// helper exists: a hint is meant to be pasted, so a path that does not need
// quoting should not acquire any.
func TestHintArgKeepsOrdinaryPathsBare(t *testing.T) {
	for _, in := range []string{"reports/model-swap.db", "aeval.db", "./x/y.db", "/tmp/a.db"} {
		if got := hintArg(in); got != in {
			t.Errorf("hintArg(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestHintArgQuotesWhatTheShellWouldSplit is the half that matters: a database
// under a directory with a space is exactly where a naive hint stops being
// pasteable, and quoting it is cheaper than explaining that.
func TestHintArgQuotesWhatTheShellWouldSplit(t *testing.T) {
	cases := []struct{ in, want string }{
		{"my runs/a.db", `'my runs/a.db'`},
		{"a b", `'a b'`},
		{"tab\there", "'tab\there'"},
		{"$HOME/x.db", `'$HOME/x.db'`},
		{"it's.db", `'it'\''s.db'`},
	}
	for _, c := range cases {
		if got := hintArg(c.in); got != c.want {
			t.Errorf("hintArg(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
