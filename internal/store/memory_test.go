package store

import "testing"

func TestMemory(t *testing.T) {
	t.Parallel()
	runSuite(t, func(*testing.T) Store { return NewMemory() })
}

func TestRepoKey(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://github.com/Showmino/Payments":      "showmino/payments",
		"https://github.com/showmino/payments.git":  "showmino/payments",
		"https://github.com/showmino/payments/":     "showmino/payments",
		"https://github.com/showmino/payments.git/": "showmino/payments",
		"https://github.com/showmino":               "",
		"https://github.com/showmino/a/b":           "",
		"git@github.com:showmino/payments.git":      "",
		"https://gitlab.com/showmino/payments":      "",
		"":                                          "",
	} {
		if got := RepoKey(in); got != want {
			t.Errorf("RepoKey(%q) = %q, want %q", in, got, want)
		}
	}
}
