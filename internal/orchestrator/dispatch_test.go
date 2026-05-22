package orchestrator

import "testing"

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"simple":              "'simple'",
		"with space":          "'with space'",
		"https://x.example/y": "'https://x.example/y'",
		"a'b":                 `'a'\''b'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// Verify SSHDispatcher satisfies the Dispatcher interface.
var _ Dispatcher = (*SSHDispatcher)(nil)
