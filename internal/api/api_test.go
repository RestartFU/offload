package api

import "testing"

func TestGitHubOwner(t *testing.T) {
	for in, want := range map[string]string{
		"acme": "acme", "acme/app": "acme/app", "https://github.com/acme/": "acme", "https://github.com/acme/app": "acme/app",
		" Acme ": "Acme", "https://ghe.example.com/org": "org",
	} {
		got, _, err := GitHubOwner(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "acme/app/x", "http://github.com/acme", "https://u:p@github.com/acme", "https://github.com/acme?x=1",
		"../etc", "acme/..", "-acme", "acme app"} {
		if _, _, err := GitHubOwner(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if GitHubKey("Acme/App") != "acme--app" {
		t.Error(GitHubKey("Acme/App"))
	}
}
