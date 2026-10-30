package bind

import (
	"testing"

	"github.com/metalmatze/kelpie/internal/core"
	"github.com/metalmatze/kelpie/internal/host"
	"github.com/metalmatze/kelpie/internal/state"
)

func TestBranchBinding(t *testing.T) {
	st := state.NewMemory()
	b := New(st, true)
	ws := host.Workspace{Host: "herdr", ID: "w1:p1", Repos: []string{"metalmatze/api", "Acme/API"}, Branch: "fix-auth"}
	b.SetWorkspaces("herdr", []host.Workspace{ws, {Host: "herdr", ID: "w2:p1", Repos: []string{"acme/api"}, Branch: "main"}})

	pr := core.Event{Subject: "github:acme/api#421", Bind: map[string]string{"repo": "acme/api", "head_repo": "metalmatze/api", "branch": "fix-auth"}}
	got, _, isNew, ok := b.Resolve(pr)
	if !ok || got.ID != "w1:p1" || !isNew {
		t.Fatalf("first resolve: %v %v %v", got.ID, isNew, ok)
	}
	if _, _, isNew, ok := b.Resolve(pr); !ok || isNew {
		t.Fatalf("second resolve should reuse the binding: new=%v ok=%v", isNew, ok)
	}

	// Checking out another branch moves the PR away from the workspace.
	ws.Branch = "other"
	b.SetWorkspaces("herdr", []host.Workspace{ws})
	if _, _, _, ok := b.Resolve(pr); ok {
		t.Fatal("bound after branch switch")
	}

	// Two agents on the same branch: don't guess.
	b.SetWorkspaces("herdr", []host.Workspace{
		{Host: "herdr", ID: "w1:p1", Repos: []string{"acme/api"}, Branch: "fix-auth"},
		{Host: "herdr", ID: "w3:p1", Repos: []string{"acme/api"}, Branch: "fix-auth"},
	})
	if _, _, _, ok := b.Resolve(pr); ok {
		t.Fatal("ambiguous match bound")
	}

	// Closing the workspace ends the binding.
	b.SetWorkspaces("herdr", []host.Workspace{{Host: "herdr", ID: "w1:p1", Repos: []string{"acme/api"}, Branch: "fix-auth"}})
	b.Resolve(pr)
	b.SetWorkspaces("herdr", nil)
	if _, ok := st.Binding(pr.Subject); ok {
		t.Fatal("binding survived its workspace")
	}
}

func TestGitHubRepo(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:metalmatze/kelpie.git":     "metalmatze/kelpie",
		"https://github.com/dash0hq/dash0":         "dash0hq/dash0",
		"https://github.com/dash0hq/dash0.git/":    "dash0hq/dash0",
		"ssh://git@github.com/parca-dev/parca.git": "parca-dev/parca",
		"git@gitlab.com:foo/bar.git":               "",
	} {
		got, _ := host.GitHubRepo(url)
		if got != want {
			t.Errorf("GitHubRepo(%q) = %q, want %q", url, got, want)
		}
	}
}
