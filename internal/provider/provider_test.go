package provider

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metalmatze/kelpie/internal/core"
)

func sh(script string) []string { return []string{"sh", "-c", script, "provider"} }

func TestSubject(t *testing.T) {
	// $1 is the subcommand kelpie appends, then the target.
	got, err := Subject(t.Context(), sh(`[ "$1" = subject ] && printf 'acme:%s\n\nacme:%s-b\n' "$2" "$2"`), []string{"A-1"})
	if err != nil || !slices.Equal(got, []core.Subject{"acme:A-1", "acme:A-1-b"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	for script, want := range map[string]string{
		`echo "want an ID like A-1" >&2; exit 1`: "sh subject: want an ID like A-1",
		`echo https://acme.example/A-1`:          "not a subject",
		`true`:                                   "printed no subject",
		`read -r line; echo "acme:$line"`:        `printed "acme:"`, // stdin is closed: the read gets nothing
	} {
		if _, err := Subject(t.Context(), sh(script), nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", script, err, want)
		}
	}
	defer func(d time.Duration) { SubjectTimeout = d }(SubjectTimeout)
	SubjectTimeout = 100 * time.Millisecond
	if _, err := Subject(t.Context(), sh(`sleep 5`), nil); err == nil || !strings.Contains(err.Error(), "no answer within 100ms") {
		t.Errorf("hanging: %v", err)
	}
}

// The token reaches verify on stdin only, and never comes back in an error.
func TestWho(t *testing.T) {
	who, err := Who(t.Context(), sh(`[ "$1" = check ] && read -r tok && [ "$tok" = s3cret ] && echo alice`), []string{"check"}, "s3cret")
	if err != nil || who != "alice" {
		t.Fatalf("got %q, %v", who, err)
	}
	_, err = Who(t.Context(), sh(`read -r tok; echo "rejected header: $tok" >&2; exit 1`), []string{"verify"}, "s3cret")
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("got %v", err)
	}
	// Without a token, as for me, stdin is closed rather than an empty line.
	if who, err := Who(t.Context(), sh(`read -r x && echo "read $x" || echo octocat`), []string{"me"}, ""); err != nil || who != "octocat" {
		t.Errorf("me: %q, %v", who, err)
	}
}
