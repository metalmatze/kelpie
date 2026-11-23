package host

import "testing"

func TestHostOfAndRegister(t *testing.T) {
	// Built-in hosts keep their existing prefix and agent-reporting rules.
	for id, want := range map[string]string{
		"w3:p1":            "herdr",
		"orca:repo::/work": "orca",
		"hc:t7":            "hc",
		"unknown:1":        "herdr",
	} {
		if got := HostOf(id); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", id, got, want)
		}
	}
	if !ReportsAgent("herdr") || !ReportsAgent("orca") || ReportsAgent("hc") {
		t.Fatal("built-in ReportsAgent mismatch")
	}

	// Registering an external host enables prefix resolution and updates
	// ReportsAgent idempotently without disturbing herdr pane IDs.
	t.Cleanup(func() {
		regMu.Lock()
		prefixed = []string{"orca", "hc"}
		agentHosts = []string{"herdr", "orca"}
		regMu.Unlock()
	})

	Register("custom", false)
	if got := HostOf("custom:sess-1"); got != "custom" {
		t.Fatalf("HostOf(custom:sess-1) = %q, want custom", got)
	}
	if got := HostOf("w1:p2"); got != "herdr" {
		t.Fatalf("HostOf(w1:p2) = %q, want herdr", got)
	}
	if ReportsAgent("custom") {
		t.Fatal("ReportsAgent(custom) = true, want false")
	}

	Register("custom", true)
	if !ReportsAgent("custom") {
		t.Fatal("ReportsAgent(custom) = false after re-register, want true")
	}
	Register("custom", false)
	if ReportsAgent("custom") {
		t.Fatal("ReportsAgent(custom) = true after disabling, want false")
	}
}
