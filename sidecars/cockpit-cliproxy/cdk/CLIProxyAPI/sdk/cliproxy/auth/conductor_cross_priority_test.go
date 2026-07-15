package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerCrossPriorityCandidatesKeepK12AheadOfHigherPriorityFallback(t *testing.T) {
	remaining := 100
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		K12: &K12SessionPolicyConfig{
			StatePath: filepath.Join(t.TempDir(), "k12-sessions.json"),
			HMACKey:   []byte("cross-priority-test-key"),
			IsK12: func(auth *Auth) bool {
				return auth != nil && auth.Attributes["plan_type"] == "k12"
			},
			QuotaSnapshot: func(*Auth) K12QuotaSnapshot {
				value := remaining
				return K12QuotaSnapshot{Fresh: true, HourlyRemainingPercent: &value}
			},
		},
	})
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)

	plus := &Auth{ID: "plus-high", Provider: "codex", Attributes: map[string]string{"priority": "10", "plan_type": "plus"}}
	k12 := &Auth{ID: "k12-low", Provider: "codex", Attributes: map[string]string{"priority": "1", "plan_type": "k12"}}
	auths := []*Auth{plus, k12}
	available, err := manager.availableAuthsForRouteModel(auths, "codex", "gpt-5.4", time.Now())
	if err != nil {
		t.Fatalf("available auths: %v", err)
	}
	if len(available) != 1 || available[0].ID != plus.ID {
		t.Fatalf("highest priority bucket = %#v, want only %s", available, plus.ID)
	}
	candidates := manager.authsForSelector(auths, available, "gpt-5.4", time.Now())
	if len(candidates) != 2 {
		t.Fatalf("cross-priority candidates = %#v, want both auths", candidates)
	}

	selected, err := selector.Pick(
		context.Background(),
		"codex",
		"gpt-5.4",
		cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"cross-priority-k12"}`)},
		candidates,
	)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("eligible K12 selection = %#v, err=%v; want %s", selected, err, k12.ID)
	}

	remaining = 0
	selected, err = selector.Pick(
		context.Background(),
		"codex",
		"gpt-5.4",
		cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"cross-priority-plus"}`)},
		candidates,
	)
	if err != nil || selected == nil || selected.ID != plus.ID {
		t.Fatalf("depleted K12 fallback = %#v, err=%v; want %s", selected, err, plus.ID)
	}
}
