package cli

import (
	"strings"
	"testing"

	"github.com/herdanis/his-mouse-friday/internal/daemon"
)

func attachRows() []daemon.SessionListItem {
	return []daemon.SessionListItem{
		{Name: "penny-1", Project: "penny", AgentSessionID: "ses_penny001", Dir: "/repo/penny"},
		{Name: "mouse-1", Project: "mouse", AgentSessionID: "ses_mouse002", Dir: "/repo/mouse"},
		{Name: "mouse-2", Project: "mouse", AgentSessionID: "ses_mouse003", Dir: "/repo/mouse"},
		{Name: "ghost-1", Project: "ghost", Dir: "/repo/ghost"},
		// Resumed task: one row per run, one conversation.
		{Name: "rerun-1", Project: "rerun", AgentSessionID: "ses_rerun004", Dir: "/repo/rerun"},
		{Name: "rerun-1", Project: "rerun", AgentSessionID: "ses_rerun004", Dir: "/repo/rerun"},
	}
}

func TestResolveSessionForAttach(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		want    string // expected opencode session id
		wantErr string // substring; empty = expect success
	}{
		{"exact name", "penny-1", "ses_penny001", ""},
		{"exact session id", "ses_mouse002", "ses_mouse002", ""},
		{"unique name prefix", "penny", "ses_penny001", ""},
		{"unique session id prefix", "ses_penny", "ses_penny001", ""},
		{"ambiguous prefix", "mouse", "", "ambiguous"},
		{"no match", "nope", "", "no session matches"},
		{"empty agent session id", "ghost-1", "", "never captured"},
		{"resumed rows collapse", "rerun", "ses_rerun004", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSessionForAttach(attachRows(), tc.id)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.AgentSessionID != tc.want {
				t.Errorf("session id = %q, want %q", got.AgentSessionID, tc.want)
			}
		})
	}
}

// The ambiguous error must name the candidates so the user can retype a longer id.
func TestResolveSessionForAttach_AmbiguousListsCandidates(t *testing.T) {
	_, err := resolveSessionForAttach(attachRows(), "mouse")
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"mouse-1", "mouse-2", "ses_mouse002", "ses_mouse003"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing candidate %q", err, want)
		}
	}
}
