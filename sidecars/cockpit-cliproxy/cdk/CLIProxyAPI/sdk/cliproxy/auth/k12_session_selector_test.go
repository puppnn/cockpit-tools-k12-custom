package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func testK12Auth(id string) *Auth {
	return &Auth{ID: id, Provider: "codex", Attributes: map[string]string{"test_plan": "k12"}}
}

func testPlusAuth(id string) *Auth {
	return &Auth{ID: id, Provider: "codex", Attributes: map[string]string{"test_plan": "plus"}}
}

func newTestK12Selector(t *testing.T, statePath string, remaining map[string]*int) *SessionAffinitySelector {
	t.Helper()
	return newTestK12SelectorWithQuota(t, statePath, func(auth *Auth) K12QuotaSnapshot {
		value, ok := remaining[auth.ID]
		if !ok {
			return K12QuotaSnapshot{}
		}
		return K12QuotaSnapshot{Fresh: true, HourlyRemainingPercent: value}
	})
}

func newTestK12SelectorWithSnapshots(t *testing.T, statePath string, snapshots map[string]K12QuotaSnapshot) *SessionAffinitySelector {
	t.Helper()
	return newTestK12SelectorWithQuota(t, statePath, func(auth *Auth) K12QuotaSnapshot {
		snapshot, ok := snapshots[auth.ID]
		if !ok {
			return K12QuotaSnapshot{}
		}
		return snapshot
	})
}

func newTestK12SelectorWithQuota(t *testing.T, statePath string, quotaSnapshot func(*Auth) K12QuotaSnapshot) *SessionAffinitySelector {
	t.Helper()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      time.Hour,
		K12: &K12SessionPolicyConfig{
			StatePath: statePath,
			HMACKey:   []byte("test-local-api-key"),
			IsK12: func(auth *Auth) bool {
				return auth != nil && strings.EqualFold(auth.Attributes["test_plan"], "k12")
			},
			IsSpillover: func(auth *Auth) bool {
				return auth != nil && strings.EqualFold(auth.Attributes["test_plan"], "plus")
			},
			QuotaSnapshot: quotaSnapshot,
		},
	})
	t.Cleanup(selector.Stop)
	return selector
}

func testK12NewSessionQuarantine(t *testing.T, selector *SessionAffinitySelector, authID string) (k12NewSessionQuarantine, bool) {
	t.Helper()
	if selector == nil || selector.k12 == nil || selector.k12.store == nil {
		t.Fatal("K12 session store is not initialized")
	}
	selector.k12.store.mu.Lock()
	defer selector.k12.store.mu.Unlock()
	quarantine, ok := selector.k12.store.newSessionQuarantines[authID]
	return quarantine, ok
}

func TestK12NewSessionRejectsKnownExhaustedWeeklyQuota(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot K12QuotaSnapshot
		wantAuth string
	}{
		{
			name: "fresh exhausted weekly window",
			snapshot: K12QuotaSnapshot{
				Fresh:                  true,
				HourlyRemainingPercent: intPtr(75),
				WeeklyRemainingPercent: intPtr(0),
			},
			wantAuth: "plus-a",
		},
		{
			name: "stale exhausted weekly window",
			snapshot: K12QuotaSnapshot{
				Fresh:                  false,
				HourlyRemainingPercent: intPtr(75),
				WeeklyRemainingPercent: intPtr(0),
			},
			wantAuth: "plus-a",
		},
		{
			name: "weekly window unknown",
			snapshot: K12QuotaSnapshot{
				Fresh:                  true,
				HourlyRemainingPercent: intPtr(75),
			},
			wantAuth: "k12-a",
		},
		{
			name: "known weekly quota remains",
			snapshot: K12QuotaSnapshot{
				Fresh:                  true,
				HourlyRemainingPercent: intPtr(75),
				WeeklyRemainingPercent: intPtr(1),
			},
			wantAuth: "k12-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selector := newTestK12SelectorWithSnapshots(
				t,
				filepath.Join(t.TempDir(), "k12-sessions.json"),
				map[string]K12QuotaSnapshot{"k12-a": tt.snapshot},
			)
			selected, err := selector.Pick(
				context.Background(),
				"codex",
				"gpt-5",
				promptCacheOptions("weekly-quota-"+tt.name),
				[]*Auth{testK12Auth("k12-a"), testPlusAuth("plus-a")},
			)
			if err != nil || selected == nil || selected.ID != tt.wantAuth {
				t.Fatalf("selected=%v err=%v, want %s", selected, err, tt.wantAuth)
			}
		})
	}
}

func TestK12TentativeSessionStopsUsingNewlyExhaustedWeeklyQuota(t *testing.T) {
	t.Parallel()

	snapshots := map[string]K12QuotaSnapshot{
		"k12-a": {
			Fresh:                  true,
			HourlyRemainingPercent: intPtr(75),
			WeeklyRemainingPercent: intPtr(10),
		},
	}
	selector := newTestK12SelectorWithSnapshots(
		t,
		filepath.Join(t.TempDir(), "k12-sessions.json"),
		snapshots,
	)
	auths := []*Auth{testK12Auth("k12-a"), testPlusAuth("plus-a")}
	opts := promptCacheOptions("weekly-quota-tentative")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || selected == nil || selected.ID != "k12-a" {
		t.Fatalf("initial selection=%v err=%v", selected, err)
	}
	snapshots["k12-a"] = K12QuotaSnapshot{
		Fresh:                  false,
		HourlyRemainingPercent: intPtr(75),
		WeeklyRemainingPercent: intPtr(0),
	}

	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || selected == nil || selected.ID != "plus-a" {
		t.Fatalf("selection after weekly exhaustion=%v err=%v, want plus-a", selected, err)
	}
}

func TestK12ConfirmedSessionBypassesKnownExhaustedWeeklyQuota(t *testing.T) {
	t.Parallel()

	snapshots := map[string]K12QuotaSnapshot{
		"k12-a": {
			Fresh:                  true,
			HourlyRemainingPercent: intPtr(75),
			WeeklyRemainingPercent: intPtr(10),
		},
	}
	selector := newTestK12SelectorWithSnapshots(
		t,
		filepath.Join(t.TempDir(), "k12-sessions.json"),
		snapshots,
	)
	auth := testK12Auth("k12-a")
	opts := promptCacheOptions("weekly-quota-confirmed")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{auth})
	if err != nil || selected == nil {
		t.Fatalf("initial selection=%v err=%v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: auth.ID, Success: true}, opts)
	snapshots["k12-a"] = K12QuotaSnapshot{
		Fresh:                  false,
		HourlyRemainingPercent: intPtr(0),
		WeeklyRemainingPercent: intPtr(0),
	}

	selected, handled, err := selector.PickBeforeAvailability(
		context.Background(),
		"codex",
		"gpt-5",
		opts,
		[]*Auth{auth},
	)
	if err != nil || !handled || selected == nil || selected.ID != auth.ID {
		t.Fatalf("confirmed selection=%v handled=%v err=%v", selected, handled, err)
	}
}

func promptCacheOptions(value string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"` + value + `","input":[{"role":"user","content":"secret prompt"}]}`)}
}

func TestExtractSessionIdentitiesCodexNativePriority(t *testing.T) {
	t.Parallel()
	headers := http.Header{
		"X-Codex-Turn-Metadata": {`{"prompt_cache_key":"turn-cache","window_id":"window-turn"}`},
		"X-Codex-Window-Id":     {"window-header"},
		"Session_id":            {"session-header"},
		"Conversation_id":       {"conversation-header"},
		"X-Session-ID":          {"compat-header"},
	}
	payload := []byte(`{"prompt_cache_key":"body-cache","client_metadata":{"x-codex-window-id":"window-body"}}`)

	identity, _ := extractSessionIdentities(headers, payload, map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "execution-session",
	})
	if identity.ID != "execution:execution-session" || identity.Source != "execution_session_id" {
		t.Fatalf("execution identity = %#v", identity)
	}

	identity, _ = extractSessionIdentities(headers, payload, map[string]any{"prompt_cache_key": "metadata-cache"})
	if identity.ID != "prompt-cache:metadata-cache" || identity.Source != "prompt_cache_key_metadata" {
		t.Fatalf("metadata prompt cache identity = %#v", identity)
	}

	identity, _ = extractSessionIdentities(headers, payload, nil)
	if identity.ID != "prompt-cache:body-cache" || identity.Source != "prompt_cache_key" {
		t.Fatalf("prompt cache identity = %#v", identity)
	}

	identity, _ = extractSessionIdentities(headers, []byte(`{"input":[]}`), nil)
	if identity.ID != "prompt-cache:turn-cache" || identity.Source != "codex_turn_metadata.prompt_cache_key" {
		t.Fatalf("turn metadata identity = %#v", identity)
	}

	delete(headers, "X-Codex-Turn-Metadata")
	identity, _ = extractSessionIdentities(headers, []byte(`{"input":[]}`), nil)
	if identity.ID != "window:window-header" || identity.Source != "codex_window_id_header" {
		t.Fatalf("window identity = %#v", identity)
	}

	delete(headers, "X-Codex-Window-Id")
	identity, _ = extractSessionIdentities(headers, []byte(`{"input":[]}`), nil)
	if identity.ID != "codex:session-header" || identity.Source != "session_id_header" {
		t.Fatalf("session header identity = %#v", identity)
	}

	delete(headers, "Session_id")
	identity, _ = extractSessionIdentities(headers, []byte(`{"input":[]}`), nil)
	if identity.ID != "conv:conversation-header" || identity.Source != "conversation_id_header" {
		t.Fatalf("conversation header identity = %#v", identity)
	}

	delete(headers, "Conversation_id")
	identity, _ = extractSessionIdentities(headers, []byte(`{"input":[]}`), nil)
	if identity.ID != "header:compat-header" || identity.Source != "x_session_id" {
		t.Fatalf("compatibility header identity = %#v", identity)
	}
}

func TestExtractSessionIdentitiesCompatibilityAndMessageHashStability(t *testing.T) {
	t.Parallel()
	identity, _ := extractSessionIdentities(nil, []byte(`{"metadata":{"user_id":{"session_id":"claude-session"}}}`), nil)
	if identity.ID != "claude:claude-session" || identity.Source != "claude_metadata_session_id" {
		t.Fatalf("Claude compatibility identity = %#v", identity)
	}

	first, _ := extractSessionIdentities(nil, []byte(`{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_text","text":"stable prompt"}]}]}`), nil)
	second, _ := extractSessionIdentities(nil, []byte(`{"model":"gpt-5-mini","input":[{"role":"user","content":[{"type":"input_text","text":"stable prompt"}]}]}`), nil)
	if first.ID == "" || first.Source != "message_hash" || first.ID != second.ID {
		t.Fatalf("message hash changed across model aliases: first=%#v second=%#v", first, second)
	}
}

func TestK12BindingConfirmsOnlyAfterSuccessAndPersistsDigest(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "k12-sessions.json")
	remaining := map[string]*int{"k12-a": intPtr(25)}
	selector := newTestK12Selector(t, statePath, remaining)
	auth := testK12Auth("k12-a")
	opts := promptCacheOptions("raw-session-must-not-be-written")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{auth})
	if err != nil || selected.ID != auth.ID {
		t.Fatalf("tentative Pick() = %v, %v", selected, err)
	}
	if _, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, []*Auth{auth}); handled || err != nil {
		t.Fatalf("binding became confirmed before success: handled=%v err=%v", handled, err)
	}

	selector.OnSelectionResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true}, opts)
	confirmed, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5-mini", opts, []*Auth{auth})
	if err != nil || !handled || confirmed == nil || confirmed.ID != auth.ID {
		t.Fatalf("confirmed PickBeforeAvailability() = %v handled=%v err=%v", confirmed, handled, err)
	}

	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	stateText := string(content)
	for _, secret := range []string{"raw-session-must-not-be-written", "secret prompt"} {
		if strings.Contains(stateText, secret) {
			t.Fatalf("state file leaked raw session content %q: %s", secret, stateText)
		}
	}
	if !strings.Contains(stateText, fmt.Sprintf(`"version": %d`, k12SessionStateVersion)) || !strings.Contains(stateText, `"sessionDigest"`) {
		t.Fatalf("state file missing versioned digest: %s", stateText)
	}

	recovered := newTestK12Selector(t, statePath, remaining)
	confirmed, handled, err = recovered.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, []*Auth{auth})
	if err != nil || !handled || confirmed == nil || confirmed.ID != auth.ID {
		t.Fatalf("recovered binding = %v handled=%v err=%v", confirmed, handled, err)
	}
}

func TestPreferredPlusAppliesToNewSessionWithoutMovingConfirmedK12Session(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "k12-sessions.json")
	remaining := map[string]*int{"k12-a": intPtr(100)}
	preferPlus := false
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      time.Hour,
		PreferNewSession: func(auth *Auth) bool {
			return preferPlus && auth != nil && auth.ID == "plus-preferred"
		},
		K12: &K12SessionPolicyConfig{
			StatePath: statePath,
			HMACKey:   []byte("test-local-api-key"),
			IsK12: func(auth *Auth) bool {
				return auth != nil && strings.EqualFold(auth.Attributes["test_plan"], "k12")
			},
			IsSpillover: func(auth *Auth) bool {
				return auth != nil && strings.EqualFold(auth.Attributes["test_plan"], "plus")
			},
			QuotaSnapshot: func(auth *Auth) K12QuotaSnapshot {
				return K12QuotaSnapshot{Fresh: true, HourlyRemainingPercent: remaining[auth.ID]}
			},
		},
	})
	t.Cleanup(selector.Stop)

	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-preferred")
	auths := []*Auth{k12, plus}
	existingOpts := promptCacheOptions("existing-k12-session")
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", existingOpts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("initial K12 Pick() = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, existingOpts)

	preferPlus = true
	sticky, err := selector.Pick(context.Background(), "codex", "gpt-5.4", existingOpts, auths)
	if err != nil || sticky == nil || sticky.ID != k12.ID {
		t.Fatalf("confirmed K12 session moved after preference enabled: %#v, %v", sticky, err)
	}

	preferred, err := selector.Pick(context.Background(), "codex", "gpt-5.4", promptCacheOptions("new-plus-session"), auths)
	if err != nil || preferred == nil || preferred.ID != plus.ID {
		t.Fatalf("new session Pick() = %#v, %v; want %s", preferred, err, plus.ID)
	}
}

func TestChangingPreferredPoolDoesNotMoveExistingGenericSessionAheadOfK12(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "k12-sessions.json")
	remaining := map[string]*int{"k12-a": intPtr(100)}
	preferredID := "plus-a"
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      time.Hour,
		PreferNewSession: func(auth *Auth) bool {
			return auth != nil && auth.ID == preferredID
		},
		K12: &K12SessionPolicyConfig{
			StatePath: statePath,
			HMACKey:   []byte("test-local-api-key"),
			IsK12: func(auth *Auth) bool {
				return auth != nil && strings.EqualFold(auth.Attributes["test_plan"], "k12")
			},
			IsSpillover: func(auth *Auth) bool {
				return auth != nil && strings.EqualFold(auth.Attributes["test_plan"], "plus")
			},
			QuotaSnapshot: func(auth *Auth) K12QuotaSnapshot {
				return K12QuotaSnapshot{Fresh: true, HourlyRemainingPercent: remaining[auth.ID]}
			},
		},
	})
	t.Cleanup(selector.Stop)

	k12 := testK12Auth("k12-a")
	plusA := testPlusAuth("plus-a")
	plusB := testPlusAuth("plus-b")
	auths := []*Auth{k12, plusA, plusB}
	existingOpts := promptCacheOptions("existing-generic-session")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", existingOpts, auths)
	if err != nil || selected == nil || selected.ID != plusA.ID {
		t.Fatalf("initial preferred Pick() = %#v, %v; want %s", selected, err, plusA.ID)
	}

	preferredID = plusB.ID
	sticky, err := selector.Pick(context.Background(), "codex", "gpt-5.4", existingOpts, auths)
	if err != nil || sticky == nil || sticky.ID != plusA.ID {
		t.Fatalf("existing generic session moved after preferred pool changed: %#v, %v", sticky, err)
	}

	newSession, err := selector.Pick(context.Background(), "codex", "gpt-5.4", promptCacheOptions("new-generic-session"), auths)
	if err != nil || newSession == nil || newSession.ID != plusB.ID {
		t.Fatalf("new session Pick() = %#v, %v; want %s", newSession, err, plusB.ID)
	}
}

func TestK12SessionStoreSupportsRepeatedAtomicReplacement(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "k12-sessions.json")
	store, err := newK12SessionStore(statePath, []byte("test-local-api-key"), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	digest := store.digest("repeat-write-session")
	now := time.Now().Truncate(time.Second)
	if err := store.confirm(digest, "prompt_cache_key", "k12-a", now); err != nil {
		t.Fatalf("first state write: %v", err)
	}
	if err := store.setCooldown(digest, "k12-a", now.Add(30*time.Second)); err != nil {
		t.Fatalf("replace state with cooldown: %v", err)
	}
	if err := store.confirm(digest, "prompt_cache_key", "k12-a", now.Add(time.Minute)); err != nil {
		t.Fatalf("replace state with rolling success: %v", err)
	}

	recovered, err := newK12SessionStore(statePath, []byte("test-local-api-key"), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := recovered.binding(digest, now.Add(time.Minute))
	if !ok || binding.LastSuccessAt != now.Add(time.Minute).Unix() || binding.CooldownUntil != 0 {
		t.Fatalf("unexpected recovered binding after replacements: %#v, ok=%v", binding, ok)
	}
}

func TestK12SessionStateKeyChangeDiscardsOldBindings(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "k12-sessions.json")
	first, err := newK12SessionStore(statePath, []byte("local-api-key-a"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	digestA := first.digest("same-session")
	if err := first.confirm(digestA, "prompt_cache_key", "k12-a", time.Now()); err != nil {
		t.Fatal(err)
	}

	second, err := newK12SessionStore(statePath, []byte("local-api-key-b"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := second.binding(second.digest("same-session"), time.Now()); ok {
		t.Fatal("binding survived a local API key change")
	}
	if err := second.confirm(second.digest("same-session"), "prompt_cache_key", "k12-b", time.Now()); err != nil {
		t.Fatalf("replace state after key change: %v", err)
	}
	recovered, err := newK12SessionStore(statePath, []byte("local-api-key-b"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if binding, ok := recovered.binding(recovered.digest("same-session"), time.Now()); !ok || binding.AuthID != "k12-b" {
		t.Fatalf("new-key binding was not persisted: %#v, ok=%v", binding, ok)
	}
}

func TestK12SessionStateRecoversFromCorruptAndExpiredFiles(t *testing.T) {
	t.Parallel()
	t.Run("corrupt", func(t *testing.T) {
		statePath := filepath.Join(t.TempDir(), "k12-sessions.json")
		if err := os.WriteFile(statePath, []byte(`{"version":`), 0o600); err != nil {
			t.Fatal(err)
		}
		remaining := map[string]*int{"k12-a": intPtr(30)}
		selector := newTestK12Selector(t, statePath, remaining)
		auth := testK12Auth("k12-a")
		opts := promptCacheOptions("corrupt-state-session")
		selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{auth})
		if err != nil || selected == nil || selected.ID != auth.ID {
			t.Fatalf("selection after corrupt state = %#v, %v", selected, err)
		}
		selector.OnSelectionResult(context.Background(), Result{AuthID: auth.ID, Success: true}, opts)
		content, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		var state k12SessionStateFile
		if err := json.Unmarshal(content, &state); err != nil || len(state.Bindings) != 1 {
			t.Fatalf("corrupt state was not replaced: state=%#v err=%v", state, err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		statePath := filepath.Join(t.TempDir(), "k12-sessions.json")
		store, err := newK12SessionStore(statePath, []byte("test-local-api-key"), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		digest := store.digest("expired-session")
		if err := store.confirm(digest, "prompt_cache_key", "k12-a", time.Now().Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		recovered, err := newK12SessionStore(statePath, []byte("test-local-api-key"), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := recovered.binding(digest, time.Now()); ok {
			t.Fatal("expired binding was recovered")
		}
	})
}

func TestK12NewSessionsIgnoreIdleBindingsAndPreferQuota(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(30), "k12-b": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	auths := []*Auth{testK12Auth("k12-a"), testK12Auth("k12-b")}

	firstOpts := promptCacheOptions("balance-first")
	first, err := selector.Pick(context.Background(), "codex", "gpt-5", firstOpts, auths)
	if err != nil || first == nil || first.ID != "k12-a" {
		t.Fatalf("equal-count quota tie-break = %#v, %v", first, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: first.ID, Success: true}, firstOpts)

	secondOpts := promptCacheOptions("balance-second")
	second, err := selector.Pick(context.Background(), "codex", "gpt-5", secondOpts, auths)
	if err != nil || second == nil || second.ID != "k12-a" {
		t.Fatalf("idle binding affected quota routing = %#v, %v", second, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: second.ID, Success: true}, secondOpts)

	remaining["k12-a"] = intPtr(15)
	remaining["k12-b"] = intPtr(40)
	third, err := selector.Pick(context.Background(), "codex", "gpt-5", promptCacheOptions("balance-third"), auths)
	if err != nil || third == nil || third.ID != "k12-b" {
		t.Fatalf("equal-count refreshed quota tie-break = %#v, %v", third, err)
	}
}

func TestK12IdleConfirmedBindingsDoNotConsumeNewSessionCapacity(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(80)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	auths := []*Auth{testK12Auth("k12-a"), testPlusAuth("plus-a")}

	for index := 0; index < k12MaxConcurrentSessionStarts; index++ {
		opts := promptCacheOptions(fmt.Sprintf("confirmed-idle-%d", index))
		selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
		if err != nil || selected == nil || selected.ID != "k12-a" {
			t.Fatalf("confirmed selection %d = %#v, %v; want k12-a", index, selected, err)
		}
		selector.OnSelectionResult(context.Background(), Result{AuthID: selected.ID, Success: true}, opts)
	}

	selected, err := selector.Pick(
		context.Background(),
		"codex",
		"gpt-5",
		promptCacheOptions("new-after-idle-bindings"),
		auths,
	)
	if err != nil || selected == nil || selected.ID != "k12-a" {
		t.Fatalf("new session after idle bindings = %#v, %v; want k12-a", selected, err)
	}
}

func TestK12TentativeSessionsReserveLoadBeforeFirstSuccess(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(80), "k12-b": intPtr(60)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	auths := []*Auth{testK12Auth("k12-a"), testK12Auth("k12-b"), testPlusAuth("plus-a")}

	want := []string{"k12-a", "k12-b", "k12-a", "k12-b"}
	for index, wantAuthID := range want {
		selected, err := selector.Pick(
			context.Background(),
			"codex",
			"gpt-5",
			promptCacheOptions(fmt.Sprintf("parallel-session-%d", index)),
			auths,
		)
		if err != nil || selected == nil || selected.ID != wantAuthID {
			t.Fatalf("tentative selection %d = %#v, %v; want %s", index, selected, err, wantAuthID)
		}
	}

	remaining["k12-a"] = intPtr(0)
	remaining["k12-b"] = intPtr(0)
	selected, err := selector.Pick(
		context.Background(),
		"codex",
		"gpt-5",
		promptCacheOptions("parallel-session-plus-fallback"),
		auths,
	)
	if err != nil || selected == nil || selected.ID != "plus-a" {
		t.Fatalf("Plus selected before all K12 were unavailable: %#v, %v", selected, err)
	}
}

func TestK12SingleCandidateCapsTentativeSessionsAtTwo(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(80)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	auths := []*Auth{testK12Auth("k12-a"), testPlusAuth("plus-a")}

	want := []string{"k12-a", "k12-a", "plus-a", "plus-a", "plus-a", "plus-a"}
	var spilledOpts cliproxyexecutor.Options
	for index, wantAuthID := range want {
		opts := promptCacheOptions(fmt.Sprintf("single-k12-parallel-%d", index))
		selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
		if err != nil || selected == nil || selected.ID != wantAuthID {
			t.Fatalf("single-K12 selection %d = %#v, %v; want %s", index, selected, err, wantAuthID)
		}
		if index == 2 {
			spilledOpts = opts
			selector.OnSelectionResult(context.Background(), Result{AuthID: selected.ID, Success: true}, opts)
		}
	}

	reused, err := selector.Pick(context.Background(), "codex", "gpt-5-mini", spilledOpts, auths)
	if err != nil || reused == nil || reused.ID != "plus-a" {
		t.Fatalf("spillover session affinity = %#v, %v; want plus-a", reused, err)
	}
}

func TestK12SingleCandidateConcurrentTentativeCapacityLimit(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(80)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	auths := []*Auth{testK12Auth("k12-a"), testPlusAuth("plus-a")}

	const sessionCount = 30
	start := make(chan struct{})
	results := make(chan string, sessionCount)
	errs := make(chan error, sessionCount)
	var workers sync.WaitGroup
	for index := 0; index < sessionCount; index++ {
		index := index
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			selected, err := selector.Pick(
				context.Background(),
				"codex",
				"gpt-5",
				promptCacheOptions(fmt.Sprintf("concurrent-spillover-%d", index)),
				auths,
			)
			if err != nil {
				errs <- err
				return
			}
			if selected == nil {
				errs <- errors.New("selector returned nil auth")
				return
			}
			results <- selected.ID
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	counts := map[string]int{}
	for authID := range results {
		counts[authID]++
	}
	if counts["k12-a"] != 2 || counts["plus-a"] != 28 {
		t.Fatalf("concurrent capacity distribution = %#v; want k12-a=2 plus-a=28", counts)
	}
}

func TestK12MultipleCandidatesEachCapAtTwoBeforePlus(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(80), "k12-b": intPtr(60)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	auths := []*Auth{testK12Auth("k12-a"), testK12Auth("k12-b"), testPlusAuth("plus-a")}

	want := []string{"k12-a", "k12-b", "k12-a", "k12-b", "plus-a", "plus-a"}
	for index, wantAuthID := range want {
		selected, err := selector.Pick(
			context.Background(),
			"codex",
			"gpt-5",
			promptCacheOptions(fmt.Sprintf("multi-k12-capacity-%d", index)),
			auths,
		)
		if err != nil || selected == nil || selected.ID != wantAuthID {
			t.Fatalf("multi-K12 capacity selection %d = %#v, %v; want %s", index, selected, err, wantAuthID)
		}
	}
}

func TestK12ConfirmedSessionNeverSpillsToPlus(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(80)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	auths := []*Auth{testK12Auth("k12-a"), testPlusAuth("plus-a")}
	opts := promptCacheOptions("confirmed-k12-does-not-spill")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || selected == nil || selected.ID != "k12-a" {
		t.Fatalf("initial K12 selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: selected.ID, Success: true}, opts)

	for attempt := 0; attempt < 4; attempt++ {
		reused, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
		if err != nil || reused == nil || reused.ID != "k12-a" {
			t.Fatalf("confirmed K12 selection %d = %#v, %v", attempt, reused, err)
		}
	}
}

func TestK12SingleCandidateKeepsK12WhenSpilloverUnavailable(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(80)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	k12 := testK12Auth("k12-a")
	disabledPlus := testPlusAuth("plus-a")
	disabledPlus.Disabled = true
	auths := []*Auth{k12, disabledPlus}

	for index := 0; index < 3; index++ {
		selected, err := selector.Pick(
			context.Background(),
			"codex",
			"gpt-5",
			promptCacheOptions(fmt.Sprintf("disabled-spillover-%d", index)),
			auths,
		)
		if err != nil || selected == nil || selected.ID != "k12-a" {
			t.Fatalf("selection with unavailable Plus %d = %#v, %v", index, selected, err)
		}
	}

	enabledPlus := testPlusAuth("plus-a")
	selected, err := selector.Pick(
		context.Background(),
		"codex",
		"gpt-5",
		promptCacheOptions("enabled-spillover"),
		[]*Auth{k12, enabledPlus},
	)
	if err != nil || selected == nil || selected.ID != "plus-a" {
		t.Fatalf("selection after Plus became available = %#v, %v", selected, err)
	}
}

func TestK12SyncAuthsPrunesDeletedOrDisabledBindings(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "k12-sessions.json")
	remaining := map[string]*int{"k12-a": intPtr(30), "k12-b": intPtr(20)}
	selector := newTestK12Selector(t, statePath, remaining)
	authA := testK12Auth("k12-a")
	authB := testK12Auth("k12-b")
	opts := promptCacheOptions("pruned-session")
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{authA, authB})
	if err != nil || selected == nil || selected.ID != authA.ID {
		t.Fatalf("initial selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: authA.ID, Success: true}, opts)

	disabledA := testK12Auth("k12-a")
	disabledA.Disabled = true
	selector.SyncAuths([]*Auth{disabledA, authB})
	if _, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, []*Auth{disabledA, authB}); handled || err != nil {
		t.Fatalf("disabled auth binding survived pruning: handled=%v err=%v", handled, err)
	}
	reselected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{disabledA, authB})
	if err != nil || reselected == nil || reselected.ID != authB.ID {
		t.Fatalf("pruned session did not reselect valid K12: %#v, %v", reselected, err)
	}

	recovered := newTestK12Selector(t, statePath, remaining)
	if _, handled, err := recovered.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, []*Auth{authB}); handled || err != nil {
		t.Fatalf("pruned binding returned after restart: handled=%v err=%v", handled, err)
	}
}

func TestK12ConfirmedSessionSurvivesZeroFiveHourQuotaButNewSessionFallsBack(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	oldOpts := promptCacheOptions("old-session")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", oldOpts, []*Auth{k12, plus})
	if err != nil || selected.ID != k12.ID {
		t.Fatalf("old session tentative selection = %v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, oldOpts)
	remaining["k12-a"] = intPtr(0)

	selected, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", oldOpts, []*Auth{k12, plus})
	if err != nil || !handled || selected.ID != k12.ID {
		t.Fatalf("confirmed zero-quota session = %v handled=%v err=%v", selected, handled, err)
	}

	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", promptCacheOptions("new-session"), []*Auth{k12, plus})
	if err != nil || selected.ID != plus.ID {
		t.Fatalf("new zero-quota session = %v, %v", selected, err)
	}
}

func TestK12SessionScopedFailureDirectives(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(20), "k12-b": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	auths := []*Auth{testK12Auth("k12-a"), testK12Auth("k12-b")}
	opts := promptCacheOptions("failure-session")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	result429 := Result{AuthID: selected.ID, Success: false, Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}}
	directive := selector.OnSelectionResult(context.Background(), result429, opts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("new-session 429 directive = %#v", directive)
	}
	retry, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || retry.ID == selected.ID {
		t.Fatalf("new-session 429 did not move to another K12: first=%s retry=%v err=%v", selected.ID, retry, err)
	}

	selector.OnSelectionResult(context.Background(), Result{AuthID: retry.ID, Success: true}, opts)
	directive = selector.OnSelectionResult(context.Background(), Result{AuthID: retry.ID, Success: false, Error: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream unavailable"}}, opts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || !directive.StopCredentialFallback {
		t.Fatalf("confirmed 5xx directive = %#v", directive)
	}
	bound, handled, err := selector.PickBeforeAvailability(
		context.Background(),
		"codex",
		"gpt-5",
		opts,
		append(auths, testPlusAuth("plus-a")),
	)
	if err != nil || !handled || bound == nil || bound.ID != retry.ID {
		t.Fatalf("confirmed 5xx moved the binding: auth=%v handled=%v err=%v", bound, handled, err)
	}

	directive = selector.OnSelectionResult(context.Background(), Result{AuthID: retry.ID, Success: false, Error: &Error{HTTPStatus: http.StatusForbidden, Message: "revoked"}}, opts)
	if directive.SuppressAvailabilityUpdate || directive.StopCredentialFallback {
		t.Fatalf("hard failure directive = %#v", directive)
	}
	_, handled, err = selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths)
	if handled || err != nil {
		t.Fatalf("hard failure left confirmed binding: handled=%v err=%v", handled, err)
	}
}

func TestK12Tentative402QuarantinesOnlyRejectedAuthAndPrefersPlus(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "state.json")
	snapshots := map[string]K12QuotaSnapshot{
		"k12-a": {Fresh: true, HourlyRemainingPercent: intPtr(80)},
		"k12-b": {Fresh: true, HourlyRemainingPercent: intPtr(70)},
	}
	selector := newTestK12SelectorWithSnapshots(t, statePath, snapshots)
	auths := []*Auth{testK12Auth("k12-a"), testK12Auth("k12-b"), testPlusAuth("plus-a")}
	failedOpts := promptCacheOptions("tentative-402-session")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", failedOpts, auths)
	if err != nil || selected == nil || selected.ID != "k12-a" {
		t.Fatalf("initial selection = %#v, %v; want k12-a", selected, err)
	}
	directive := selector.OnSelectionResult(context.Background(), Result{
		AuthID:  selected.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusPaymentRequired, Message: "usage limit reached"},
	}, failedOpts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("tentative 402 directive = %#v", directive)
	}

	retry, err := selector.Pick(context.Background(), "codex", "gpt-5", failedOpts, auths)
	if err != nil || retry == nil || retry.ID != "plus-a" {
		t.Fatalf("same-request retry = %#v, %v; want plus-a", retry, err)
	}
	other, err := selector.Pick(context.Background(), "codex", "gpt-5", promptCacheOptions("other-new-session"), auths)
	if err != nil || other == nil || other.ID != "k12-b" {
		t.Fatalf("other new session = %#v, %v; want non-quarantined k12-b", other, err)
	}
	if quarantine, ok := testK12NewSessionQuarantine(t, selector, "k12-a"); !ok || quarantine.QuarantineUntil <= time.Now().Unix() {
		t.Fatalf("tentative 402 quarantine = %#v, ok=%v", quarantine, ok)
	}

	recovered := newTestK12SelectorWithSnapshots(t, statePath, snapshots)
	afterRestart, err := recovered.Pick(context.Background(), "codex", "gpt-5", promptCacheOptions("new-session-after-restart"), auths)
	if err != nil || afterRestart == nil || afterRestart.ID != "k12-b" {
		t.Fatalf("selection after restart = %#v, %v; want non-quarantined k12-b", afterRestart, err)
	}
}

func TestK12Generic402PreservesUnrelatedConfirmedBinding(t *testing.T) {
	t.Parallel()
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), map[string]*int{"k12-a": intPtr(80)})
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12, plus}
	confirmedOpts := promptCacheOptions("unrelated-confirmed-session")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", confirmedOpts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("confirmed session selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, confirmedOpts)

	failedOpts := promptCacheOptions("unrelated-tentative-402-session")
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", failedOpts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("tentative session selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{
		AuthID:  k12.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusPaymentRequired, Message: "usage limit reached"},
	}, failedOpts)

	bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", confirmedOpts, auths)
	if err != nil || !handled || bound == nil || bound.ID != k12.ID {
		t.Fatalf("unrelated confirmed binding after tentative 402 = %#v handled=%v err=%v", bound, handled, err)
	}
}

func TestK12Confirmed402RemovesOnlyFailedSessionAndSpillsToPlus(t *testing.T) {
	t.Parallel()
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), map[string]*int{"k12-a": intPtr(80)})
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12, plus}
	failedOpts := promptCacheOptions("confirmed-402-failed-session")
	survivorOpts := promptCacheOptions("confirmed-402-survivor-session")

	for _, opts := range []cliproxyexecutor.Options{failedOpts, survivorOpts} {
		selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
		if err != nil || selected == nil || selected.ID != k12.ID {
			t.Fatalf("confirmed session setup selection = %#v, %v", selected, err)
		}
		selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, opts)
	}

	directive := selector.OnSelectionResult(context.Background(), Result{
		AuthID:  k12.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusPaymentRequired, Message: "usage limit reached"},
	}, failedOpts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("confirmed 402 directive = %#v", directive)
	}
	if bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", failedOpts, auths); err != nil || handled || bound != nil {
		t.Fatalf("failed session binding survived 402: auth=%#v handled=%v err=%v", bound, handled, err)
	}
	bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", survivorOpts, auths)
	if err != nil || !handled || bound == nil || bound.ID != k12.ID {
		t.Fatalf("unrelated confirmed session was removed: auth=%#v handled=%v err=%v", bound, handled, err)
	}
	retry, err := selector.Pick(context.Background(), "codex", "gpt-5", failedOpts, auths)
	if err != nil || retry == nil || retry.ID != plus.ID {
		t.Fatalf("confirmed 402 retry = %#v, %v; want plus-a", retry, err)
	}
}

func TestK12DeactivatedWorkspace402ClearsAuthBindingsAndLongQuarantines(t *testing.T) {
	t.Parallel()
	snapshots := map[string]K12QuotaSnapshot{
		"k12-a": {Fresh: true, UpdatedAt: time.Now().Add(-time.Hour), HourlyRemainingPercent: intPtr(80)},
	}
	selector := newTestK12SelectorWithSnapshots(t, filepath.Join(t.TempDir(), "state.json"), snapshots)
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12, plus}
	firstOpts := promptCacheOptions("deactivated-workspace-first-session")
	secondOpts := promptCacheOptions("deactivated-workspace-second-session")

	for _, opts := range []cliproxyexecutor.Options{firstOpts, secondOpts} {
		selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
		if err != nil || selected == nil || selected.ID != k12.ID {
			t.Fatalf("confirmed session setup selection = %#v, %v", selected, err)
		}
		selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, opts)
	}

	directive := selector.OnSelectionResult(context.Background(), Result{
		AuthID:  k12.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusPaymentRequired, Message: `{"error":{"code":"deactivated_workspace"}}`},
	}, firstOpts)
	if directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("deactivated workspace directive = %#v", directive)
	}
	for name, opts := range map[string]cliproxyexecutor.Options{"failed": firstOpts, "unrelated": secondOpts} {
		if bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths); err != nil || handled || bound != nil {
			t.Fatalf("%s binding survived deactivated workspace: auth=%#v handled=%v err=%v", name, bound, handled, err)
		}
	}
	quarantine, ok := testK12NewSessionQuarantine(t, selector, k12.ID)
	minimumUntil := time.Now().Add(k12DeactivatedWorkspaceQuarantine - 5*time.Second).Unix()
	if !ok || !quarantine.Hard || quarantine.QuarantineUntil < minimumUntil {
		t.Fatalf("deactivated workspace quarantine = %#v, ok=%v; want at least %s", quarantine, ok, k12DeactivatedWorkspaceQuarantine)
	}
	snapshots[k12.ID] = K12QuotaSnapshot{
		Fresh:                  true,
		UpdatedAt:              time.Unix(quarantine.FailedAt+1, 0),
		HourlyRemainingPercent: intPtr(100),
	}
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", promptCacheOptions("deactivated-workspace-new-session"), auths)
	if err != nil || selected == nil || selected.ID != plus.ID {
		t.Fatalf("new session after deactivated workspace = %#v, %v; want plus-a", selected, err)
	}
}

func TestK12LegacyStateMigratesAndPersistsQuarantineAcrossRestart(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "state.json")
	key := []byte("test-local-api-key")
	seedStore, err := newK12SessionStore("", key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	legacyOpts := promptCacheOptions("legacy-confirmed-session")
	identity, _ := extractSessionIdentities(legacyOpts.Headers, legacyOpts.OriginalRequest, legacyOpts.Metadata)
	now := time.Now().Truncate(time.Second)
	legacyState := k12SessionStateFile{
		Version: k12SessionStateVersion,
		KeyID:   seedStore.keyID,
		Bindings: []k12SessionBinding{{
			SessionDigest: seedStore.digest(identity.ID),
			Source:        identity.Source,
			AuthID:        "k12-a",
			LastSuccessAt: now.Unix(),
			ExpiresAt:     now.Add(time.Hour).Unix(),
		}},
	}
	content, err := json.Marshal(legacyState)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	snapshots := map[string]K12QuotaSnapshot{
		"k12-a": {
			Fresh:                  true,
			UpdatedAt:              now.Add(-time.Hour),
			HourlyRemainingPercent: intPtr(80),
		},
	}
	selector := newTestK12SelectorWithSnapshots(t, statePath, snapshots)
	auths := []*Auth{testK12Auth("k12-a"), testPlusAuth("plus-a")}
	bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", legacyOpts, auths)
	if err != nil || !handled || bound == nil || bound.ID != "k12-a" {
		t.Fatalf("legacy binding recovery = %#v handled=%v err=%v", bound, handled, err)
	}

	failedOpts := promptCacheOptions("legacy-migration-402-session")
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", failedOpts, auths)
	if err != nil || selected == nil || selected.ID != "k12-a" {
		t.Fatalf("migration failure selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{
		AuthID:  selected.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusPaymentRequired, Message: "usage limit reached"},
	}, failedOpts)

	content, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var migrated k12SessionStateFile
	if err := json.Unmarshal(content, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.Version != k12SessionStateVersion || len(migrated.Bindings) != 1 || len(migrated.NewSessionQuarantines) != 1 {
		t.Fatalf("migrated state = %#v", migrated)
	}

	recovered := newTestK12SelectorWithSnapshots(t, statePath, snapshots)
	bound, handled, err = recovered.PickBeforeAvailability(context.Background(), "codex", "gpt-5", legacyOpts, auths)
	if err != nil || !handled || bound == nil || bound.ID != "k12-a" {
		t.Fatalf("legacy binding after quarantine restart = %#v handled=%v err=%v", bound, handled, err)
	}
	selected, err = recovered.Pick(context.Background(), "codex", "gpt-5", promptCacheOptions("legacy-migration-new-session"), auths)
	if err != nil || selected == nil || selected.ID != "plus-a" {
		t.Fatalf("persisted quarantine after restart = %#v, %v; want plus-a", selected, err)
	}
}

func TestK12NewSessionQuarantineRequiresNewerFreshUsableQuotaToClear(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "state.json")
	snapshots := map[string]K12QuotaSnapshot{
		"k12-a": {Fresh: true, HourlyRemainingPercent: intPtr(80)},
	}
	selector := newTestK12SelectorWithSnapshots(t, statePath, snapshots)
	auths := []*Auth{testK12Auth("k12-a"), testPlusAuth("plus-a")}
	failedOpts := promptCacheOptions("quota-refresh-402-session")
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", failedOpts, auths)
	if err != nil || selected == nil || selected.ID != "k12-a" {
		t.Fatalf("initial selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{
		AuthID:  selected.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusPaymentRequired, Message: "usage limit reached"},
	}, failedOpts)
	quarantine, ok := testK12NewSessionQuarantine(t, selector, "k12-a")
	if !ok || quarantine.FailedAt == 0 {
		t.Fatalf("missing quarantine failure timestamp: %#v, ok=%v", quarantine, ok)
	}
	newer := time.Unix(quarantine.FailedAt+1, 0)

	tests := []struct {
		name     string
		snapshot K12QuotaSnapshot
	}{
		{
			name: "unknown timestamp",
			snapshot: K12QuotaSnapshot{
				Fresh:                  true,
				HourlyRemainingPercent: intPtr(80),
			},
		},
		{
			name: "old timestamp",
			snapshot: K12QuotaSnapshot{
				Fresh:                  true,
				UpdatedAt:              time.Unix(quarantine.FailedAt, 0),
				HourlyRemainingPercent: intPtr(80),
			},
		},
		{
			name: "stale newer snapshot",
			snapshot: K12QuotaSnapshot{
				Fresh:                  false,
				UpdatedAt:              newer,
				HourlyRemainingPercent: intPtr(80),
			},
		},
		{
			name: "unknown five hour quota",
			snapshot: K12QuotaSnapshot{
				Fresh:     true,
				UpdatedAt: newer,
			},
		},
	}
	for index, tt := range tests {
		snapshots["k12-a"] = tt.snapshot
		selected, err = selector.Pick(
			context.Background(),
			"codex",
			"gpt-5",
			promptCacheOptions(fmt.Sprintf("quota-refresh-blocked-%d", index)),
			auths,
		)
		if err != nil || selected == nil || selected.ID != "plus-a" {
			t.Fatalf("%s selection = %#v, %v; want plus-a", tt.name, selected, err)
		}
		if _, ok := testK12NewSessionQuarantine(t, selector, "k12-a"); !ok {
			t.Fatalf("%s unexpectedly cleared quarantine", tt.name)
		}
	}

	snapshots["k12-a"] = K12QuotaSnapshot{
		Fresh:                  true,
		UpdatedAt:              newer,
		HourlyRemainingPercent: intPtr(1),
	}
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", promptCacheOptions("quota-refresh-cleared"), auths)
	if err != nil || selected == nil || selected.ID != "k12-a" {
		t.Fatalf("newer usable quota selection = %#v, %v; want k12-a", selected, err)
	}
	if quarantine, ok := testK12NewSessionQuarantine(t, selector, "k12-a"); ok {
		t.Fatalf("newer usable quota did not clear quarantine: %#v", quarantine)
	}
}

func TestK12Tentative429PrefersSpillover(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(20), "k12-b": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	auths := []*Auth{testK12Auth("k12-a"), testK12Auth("k12-b"), testPlusAuth("plus-a")}
	opts := promptCacheOptions("tentative-429-spillover")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || selected == nil || selected.ID != "k12-a" {
		t.Fatalf("initial selection = %#v, %v", selected, err)
	}
	directive := selector.OnSelectionResult(context.Background(), Result{
		AuthID:  selected.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	}, opts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("tentative 429 directive = %#v", directive)
	}

	retry, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || retry == nil || retry.ID != "plus-a" {
		t.Fatalf("tentative 429 retry = %#v, %v; want plus-a", retry, err)
	}
}

func TestK12Confirmed429ReleasesBindingForFailover(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(20), "k12-b": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	auths := []*Auth{testK12Auth("k12-a"), testK12Auth("k12-b")}
	opts := promptCacheOptions("confirmed-failover-session")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil {
		t.Fatal(err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: selected.ID, Success: true}, opts)

	directive := selector.OnSelectionResult(context.Background(), Result{
		AuthID:  selected.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	}, opts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("confirmed 429 directive = %#v", directive)
	}
	if bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths); err != nil || handled || bound != nil {
		t.Fatalf("confirmed 429 left old binding: auth=%v handled=%v err=%v", bound, handled, err)
	}

	retry, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || retry == nil || retry.ID == selected.ID {
		t.Fatalf("confirmed 429 did not move to another K12: first=%s retry=%v err=%v", selected.ID, retry, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: retry.ID, Success: true}, opts)
	bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || !handled || bound == nil || bound.ID != retry.ID {
		t.Fatalf("successful failover was not rebound: auth=%v handled=%v err=%v", bound, handled, err)
	}
}

func TestMarkResultSuppressesGlobalK12CooldownState(t *testing.T) {
	t.Parallel()
	manager := NewManager(nil, nil, nil)
	auth := testK12Auth("k12-a")
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:                     auth.ID,
		Provider:                   "codex",
		Model:                      "gpt-5",
		Success:                    false,
		Error:                      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
		SuppressAvailabilityUpdate: true,
	})
	got, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth missing")
	}
	if got.Unavailable || got.Quota.Exceeded || len(got.ModelStates) != 0 {
		t.Fatalf("suppressed result changed availability: %#v", got)
	}
}

type k12BootstrapExecutor struct {
	chunks       chan cliproxyexecutor.StreamChunk
	executeErr   error
	executeCalls int
}

func (e *k12BootstrapExecutor) Identifier() string { return "codex" }
func (e *k12BootstrapExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.executeCalls++
	return cliproxyexecutor.Response{}, e.executeErr
}
func (e *k12BootstrapExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return &cliproxyexecutor.StreamResult{Chunks: e.chunks}, nil
}
func (e *k12BootstrapExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e *k12BootstrapExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}
func (e *k12BootstrapExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

type k12FailoverExecutor struct {
	k12BootstrapExecutor
	failAuthID  string
	streamCalls []string
}

type semanticEmptyFailoverExecutor struct {
	k12BootstrapExecutor
	emptyAuthID string
	streamCalls []string
}

type k12CancelableBootstrapExecutor struct {
	k12BootstrapExecutor
	started chan string
	stream  chan cliproxyexecutor.StreamChunk
}

func (e *k12CancelableBootstrapExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.started <- auth.ID
	return &cliproxyexecutor.StreamResult{Chunks: e.stream}, nil
}

func (e *k12FailoverExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.streamCalls = append(e.streamCalls, auth.ID)
	if auth.ID == e.failAuthID {
		return nil, &retryAfterStatusError{
			status:     http.StatusTooManyRequests,
			message:    "upstream K12 quota reached",
			retryAfter: time.Millisecond,
		}
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.created"}`)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *semanticEmptyFailoverExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.streamCalls = append(e.streamCalls, auth.ID)
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.created"}`)}
	if auth.ID == e.emptyAuthID {
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.completed","response":{"output":[]}}`)}
	} else {
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"ok"}`)}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestK12StreamFirstPayloadConfirmsBinding(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	manager := NewManager(nil, selector, nil)
	auth := testK12Auth("k12-a")
	opts := promptCacheOptions("stream-session")
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{auth})
	if err != nil {
		t.Fatal(err)
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.created"}`)}
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"ok"}`)}
	executor := &k12BootstrapExecutor{chunks: chunks}
	opts.SourceFormat = "openai-response"
	stream, err := manager.executeStreamWithModelPool(context.Background(), executor, selected, "codex", cliproxyexecutor.Request{Model: "gpt-5"}, opts, "gpt-5", []string{"gpt-5"}, false)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5-mini", opts, []*Auth{auth})
	if err != nil || !handled || confirmed == nil || confirmed.ID != auth.ID {
		t.Fatalf("stream bootstrap did not confirm: auth=%v handled=%v err=%v", confirmed, handled, err)
	}
	close(chunks)
	for range stream.Chunks {
	}
}

func TestOpenAIResponsesSemanticStreamBootstrap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{name: "created prelude", payload: `data: {"type":"response.created"}`, want: false},
		{name: "empty completion", payload: `data: {"type":"response.completed","response":{"output":[]}}`, want: false},
		{name: "text delta", payload: `data: {"type":"response.output_text.delta","delta":"hello"}`, want: true},
		{name: "reasoning summary only", payload: `data: {"type":"response.reasoning_summary_text.delta","delta":"checking"}`, want: false},
		{name: "function call", payload: `data: {"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"lookup"}}`, want: true},
		{name: "completed text", payload: `data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}}`, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := openAIResponsesEventHasSemanticOutput([]byte(tt.payload)); got != tt.want {
				t.Fatalf("semantic output = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenAIResponsesPreludeOnlyStreamIsRetryableEmpty(t *testing.T) {
	t.Parallel()
	chunks := make(chan cliproxyexecutor.StreamChunk, 3)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.created"}`)}
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.reasoning_summary_text.delta","delta":"checking"}`)}
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.completed","response":{"output":[]}}`)}
	close(chunks)

	executor := &k12BootstrapExecutor{chunks: chunks}
	opts := promptCacheOptions("semantic-empty-stream")
	opts.SourceFormat = "openai-response"
	manager := NewManager(nil, nil, nil)
	_, err := manager.executeStreamWithModelPool(
		context.Background(), executor, testPlusAuth("plus-empty"), "codex",
		cliproxyexecutor.Request{Model: "gpt-5"}, opts, "gpt-5", []string{"gpt-5"}, false,
	)
	if err == nil || !strings.Contains(err.Error(), "meaningful output") {
		t.Fatalf("prelude-only stream error = %v", err)
	}
}

func TestOpenAIResponsesSemanticEmptyStreamFailsOverBeforeBinding(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-empty": intPtr(80)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	manager := NewManager(nil, selector, nil)
	manager.SetRetryConfig(3, time.Second, 0)
	auths := []*Auth{testK12Auth("k12-empty"), testPlusAuth("plus-output")}
	modelRegistry := registry.GetGlobalRegistry()
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		modelRegistry.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
		authID := auth.ID
		t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	}
	executor := &semanticEmptyFailoverExecutor{emptyAuthID: "k12-empty"}
	manager.RegisterExecutor(executor)
	opts := promptCacheOptions("semantic-failover-session")
	opts.SourceFormat = "openai-response"

	stream, err := manager.ExecuteStream(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5"}, opts,
	)
	if err != nil {
		t.Fatalf("semantic empty stream did not fail over: %v", err)
	}
	var payload []byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("failover stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if !bytes.Contains(payload, []byte(`"delta":"ok"`)) {
		t.Fatalf("failover payload = %q", payload)
	}
	if len(executor.streamCalls) != 2 || executor.streamCalls[0] != "k12-empty" || executor.streamCalls[1] != "plus-output" {
		t.Fatalf("semantic failover calls = %#v", executor.streamCalls)
	}
	emptyAuth, ok := manager.GetByID("k12-empty")
	if !ok || emptyAuth == nil {
		t.Fatal("empty-stream K12 auth missing")
	}
	if emptyAuth.Unavailable || emptyAuth.Quota.Exceeded || len(emptyAuth.ModelStates) != 0 {
		t.Fatalf("semantic empty stream changed K12 availability: %#v", emptyAuth)
	}
	selector.k12.store.mu.Lock()
	bindingCount := len(selector.k12.store.bindings)
	selector.k12.store.mu.Unlock()
	if bindingCount != 0 {
		t.Fatalf("semantically empty K12 stream created %d persistent bindings", bindingCount)
	}
}

func TestK12StreamOpenTimeoutCauseReleasesBindingForNextAttempt(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(20), "k12-b": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	manager := NewManager(nil, selector, nil)
	auths := []*Auth{testK12Auth("k12-a"), testK12Auth("k12-b"), testPlusAuth("plus-a")}
	opts := promptCacheOptions("stream-open-timeout-session")
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || selected == nil {
		t.Fatalf("initial selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: selected.ID, Success: true}, opts)

	executor := &k12CancelableBootstrapExecutor{
		started: make(chan string, 1),
		stream:  make(chan cliproxyexecutor.StreamChunk),
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		_, executeErr := manager.executeStreamWithModelPool(ctx, executor, selected, "codex", cliproxyexecutor.Request{Model: "gpt-5"}, opts, "gpt-5", []string{"gpt-5"}, false)
		done <- executeErr
	}()
	if authID := <-executor.started; authID != selected.ID {
		t.Fatalf("stream started with %s, want %s", authID, selected.ID)
	}
	timeoutCause := &retryAfterStatusError{
		status:  http.StatusGatewayTimeout,
		message: "upstream timed out in stream_open attempt=1/2 after 20ms",
	}
	cancel(timeoutCause)
	executeErr := <-done
	close(executor.stream)
	if executeErr == nil || !strings.Contains(executeErr.Error(), "stream_open") {
		t.Fatalf("stream timeout cause was not returned: %v", executeErr)
	}

	if bound, handled, errPick := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths); errPick != nil || handled || bound != nil {
		t.Fatalf("stream timeout left confirmed binding: auth=%v handled=%v err=%v", bound, handled, errPick)
	}
	retry, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || retry == nil || retry.ID != "plus-a" {
		t.Fatalf("stream timeout did not prefer Plus: first=%s retry=%v err=%v", selected.ID, retry, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: retry.ID, Success: true}, opts)
	identity, _ := extractSessionIdentities(opts.Headers, opts.OriginalRequest, opts.Metadata)
	digest := selector.k12.store.digest(identity.ID)
	if binding, ok := selector.k12.store.binding(digest, time.Now()); ok {
		t.Fatalf("successful spillover kept suspended K12 binding: %#v", binding)
	}
	reused, err := selector.Pick(context.Background(), "codex", "gpt-5-mini", opts, auths)
	if err != nil || reused == nil || reused.ID != retry.ID {
		t.Fatalf("successful spillover affinity = %#v, %v; want %s", reused, err, retry.ID)
	}
}

func TestK12FailedSpilloverRestoresBindingAfterCooldown(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12, plus}
	opts := promptCacheOptions("failed-spillover-restores-binding")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("initial selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: selected.ID, Success: true}, opts)
	selector.OnSelectionResult(context.Background(), Result{
		AuthID:  selected.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	}, opts)

	spillover, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || spillover == nil || spillover.ID != plus.ID {
		t.Fatalf("spillover selection = %#v, %v", spillover, err)
	}
	identity, _ := extractSessionIdentities(opts.Headers, opts.OriginalRequest, opts.Metadata)
	digest := selector.k12.store.digest(identity.ID)
	selector.k12.store.mu.Lock()
	binding := selector.k12.store.bindings[digest]
	binding.CooldownUntil = time.Now().Add(-time.Second).Unix()
	selector.k12.store.bindings[digest] = binding
	selector.k12.store.mu.Unlock()

	stillSpilled, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || !handled || stillSpilled == nil || stillSpilled.ID != plus.ID {
		t.Fatalf("expired K12 cooldown bypassed in-flight spillover: auth=%v handled=%v err=%v", stillSpilled, handled, err)
	}
	selector.OnSelectionResult(context.Background(), Result{
		AuthID:  spillover.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "plus unavailable"},
	}, opts)

	restored, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || !handled || restored == nil || restored.ID != k12.ID {
		t.Fatalf("expired cooldown did not restore original binding: auth=%v handled=%v err=%v", restored, handled, err)
	}
}

func TestK12ConcurrentSuccessCommitsSingleAffinity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		firstAuth string
		lastAuth  string
		wantAuth  string
	}{
		{name: "K12 success arrives first", firstAuth: "k12-a", lastAuth: "plus-a", wantAuth: "k12-a"},
		{name: "Plus success arrives first", firstAuth: "plus-a", lastAuth: "k12-a", wantAuth: "plus-a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remaining := map[string]*int{"k12-a": intPtr(20)}
			selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
			k12 := testK12Auth("k12-a")
			plus := testPlusAuth("plus-a")
			auths := []*Auth{k12, plus}
			opts := promptCacheOptions("concurrent-success-" + test.name)

			selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
			if err != nil || selected == nil || selected.ID != k12.ID {
				t.Fatalf("initial selection = %#v, %v", selected, err)
			}
			selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, opts)
			selector.OnSelectionResult(context.Background(), Result{
				AuthID:  k12.ID,
				Success: false,
				Error:   &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
			}, opts)
			spillover, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
			if err != nil || spillover == nil || spillover.ID != plus.ID {
				t.Fatalf("spillover selection = %#v, %v", spillover, err)
			}

			selector.OnSelectionResult(context.Background(), Result{AuthID: test.firstAuth, Success: true}, opts)
			selector.OnSelectionResult(context.Background(), Result{AuthID: test.lastAuth, Success: true}, opts)
			winner, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths)
			if err != nil || !handled || winner == nil || winner.ID != test.wantAuth {
				t.Fatalf("committed affinity = %v handled=%v err=%v; want %s", winner, handled, err, test.wantAuth)
			}

			identity, _ := extractSessionIdentities(opts.Headers, opts.OriginalRequest, opts.Metadata)
			digest := selector.k12.store.digest(identity.ID)
			binding, bound := selector.k12.store.binding(digest, time.Now())
			spilloverAuthID := selector.k12.spilloverAuth(digest, time.Now())
			if test.wantAuth == k12.ID {
				if !bound || binding.AuthID != k12.ID || spilloverAuthID != "" {
					t.Fatalf("K12 winner left dual state: binding=%#v bound=%v spillover=%q", binding, bound, spilloverAuthID)
				}
			} else if bound || spilloverAuthID != plus.ID {
				t.Fatalf("Plus winner left dual state: binding=%#v bound=%v spillover=%q", binding, bound, spilloverAuthID)
			}
		})
	}
}

func TestK12SpilloverSuccessRequiresPersistedBindingRemoval(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12, plus}
	opts := promptCacheOptions("spillover-persist-failure")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("initial selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, opts)
	selector.OnSelectionResult(context.Background(), Result{
		AuthID:  k12.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	}, opts)
	spillover, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || spillover == nil || spillover.ID != plus.ID {
		t.Fatalf("spillover selection = %#v, %v", spillover, err)
	}

	unwritableStatePath := filepath.Join(t.TempDir(), "state-directory")
	if err := os.Mkdir(unwritableStatePath, 0o700); err != nil {
		t.Fatal(err)
	}
	selector.k12.store.path = unwritableStatePath
	selector.OnSelectionResult(context.Background(), Result{AuthID: plus.ID, Success: true}, opts)

	identity, _ := extractSessionIdentities(opts.Headers, opts.OriginalRequest, opts.Metadata)
	digest := selector.k12.store.digest(identity.ID)
	if binding, ok := selector.k12.store.binding(digest, time.Now()); !ok || binding.AuthID != k12.ID {
		t.Fatalf("failed binding removal did not roll back original K12 binding: binding=%#v ok=%v", binding, ok)
	}
	if remaining := selector.k12.store.cooldownRemaining(digest, k12.ID, time.Now()); remaining <= 0 {
		t.Fatalf("failed binding removal did not restore original cooldown: %s", remaining)
	}
	if spilloverAuthID := selector.k12.spilloverAuth(digest, time.Now()); spilloverAuthID != "" {
		t.Fatalf("spillover affinity survived failed binding persistence: %s", spilloverAuthID)
	}
	if rebound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths); err != nil || handled || rebound != nil {
		t.Fatalf("failed spillover commit remained authoritative: auth=%v handled=%v err=%v", rebound, handled, err)
	}
}

func TestK12ClientCancellationPreservesConfirmedBinding(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	manager := NewManager(nil, selector, nil)
	auth := testK12Auth("k12-a")
	opts := promptCacheOptions("client-canceled-session")
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{auth})
	if err != nil || selected == nil {
		t.Fatalf("initial selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: selected.ID, Success: true}, opts)

	executor := &k12CancelableBootstrapExecutor{
		started: make(chan string, 1),
		stream:  make(chan cliproxyexecutor.StreamChunk),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, executeErr := manager.executeStreamWithModelPool(ctx, executor, selected, "codex", cliproxyexecutor.Request{Model: "gpt-5"}, opts, "gpt-5", []string{"gpt-5"}, false)
		done <- executeErr
	}()
	<-executor.started
	cancel()
	executeErr := <-done
	close(executor.stream)
	if !errors.Is(executeErr, context.Canceled) {
		t.Fatalf("client cancellation error = %v", executeErr)
	}
	bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, []*Auth{auth})
	if err != nil || !handled || bound == nil || bound.ID != auth.ID {
		t.Fatalf("client cancellation should preserve binding: auth=%v handled=%v err=%v", bound, handled, err)
	}
}

func TestK12Confirmed429FailsOverWithinSameStreamRequest(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-failover-a": intPtr(20), "k12-failover-b": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	manager := NewManager(nil, selector, nil)
	manager.SetRetryConfig(3, time.Second, 0)
	auths := []*Auth{testK12Auth("k12-failover-a"), testK12Auth("k12-failover-b")}
	modelRegistry := registry.GetGlobalRegistry()
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		modelRegistry.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5"}})
		authID := auth.ID
		t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	}
	opts := promptCacheOptions("confirmed-429-stream-failover")
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || selected == nil {
		t.Fatalf("initial selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: selected.ID, Success: true}, opts)
	executor := &k12FailoverExecutor{failAuthID: selected.ID}
	manager.RegisterExecutor(executor)

	stream, err := manager.ExecuteStream(
		context.Background(),
		[]string{"codex"},
		cliproxyexecutor.Request{Model: "gpt-5"},
		opts,
	)
	if err != nil {
		t.Fatalf("confirmed 429 did not fail over: %v", err)
	}
	var payload []byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("failover stream error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if len(payload) == 0 {
		t.Fatal("failover stream returned no payload")
	}
	if len(executor.streamCalls) != 2 || executor.streamCalls[0] != selected.ID || executor.streamCalls[1] == selected.ID {
		t.Fatalf("stream failover calls = %#v, first auth = %s", executor.streamCalls, selected.ID)
	}
	bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths)
	if err != nil || !handled || bound == nil || bound.ID != executor.streamCalls[1] {
		t.Fatalf("stream failover binding = %v handled=%v err=%v", bound, handled, err)
	}
}

func TestK12ConcurrentSpilloverAttemptsKeepFirstSuccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		failureFirst bool
	}{
		{name: "success then older failure"},
		{name: "failure then success", failureFirst: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remaining := map[string]*int{"k12-a": intPtr(20)}
			selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
			k12 := testK12Auth("k12-a")
			plus := testPlusAuth("plus-a")
			auths := []*Auth{k12, plus}
			baseOpts := promptCacheOptions("parallel-plus-" + test.name)

			selected, err := selector.Pick(context.Background(), "codex", "gpt-5", baseOpts, auths)
			if err != nil || selected == nil || selected.ID != k12.ID {
				t.Fatalf("initial K12 selection = %#v, %v", selected, err)
			}
			selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, baseOpts)
			selector.OnSelectionResult(context.Background(), Result{
				AuthID:  k12.ID,
				Success: false,
				Error:   &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
			}, baseOpts)

			failureOpts := withSelectionAttempt(baseOpts)
			failureAttempt, err := selector.Pick(context.Background(), "codex", "gpt-5", failureOpts, auths)
			if err != nil || failureAttempt == nil || failureAttempt.ID != plus.ID {
				t.Fatalf("failure attempt selection = %#v, %v", failureAttempt, err)
			}
			successOpts := withSelectionAttempt(baseOpts)
			successAttempt, err := selector.Pick(context.Background(), "codex", "gpt-5", successOpts, auths)
			if err != nil || successAttempt == nil || successAttempt.ID != plus.ID {
				t.Fatalf("success attempt selection = %#v, %v", successAttempt, err)
			}

			failureResult := Result{
				AuthID:  plus.ID,
				Success: false,
				Error:   &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "parallel request failed"},
			}
			if test.failureFirst {
				directive := selector.OnSelectionResult(context.Background(), failureResult, failureOpts)
				if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
					t.Fatalf("parallel failure directive = %#v", directive)
				}
				selector.OnSelectionResult(context.Background(), Result{AuthID: plus.ID, Success: true}, successOpts)
			} else {
				selector.OnSelectionResult(context.Background(), Result{AuthID: plus.ID, Success: true}, successOpts)
				directive := selector.OnSelectionResult(context.Background(), failureResult, failureOpts)
				if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
					t.Fatalf("retired failure directive = %#v", directive)
				}
			}

			bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", baseOpts, auths)
			if err != nil || !handled || bound == nil || bound.ID != plus.ID {
				t.Fatalf("Plus affinity after concurrent results = %v handled=%v err=%v", bound, handled, err)
			}
		})
	}
}

func TestK12ConcurrentSibling402StillQuarantinesWithoutRemovingBinding(t *testing.T) {
	t.Parallel()
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), map[string]*int{"k12-a": intPtr(80)})
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12, plus}
	baseOpts := promptCacheOptions("parallel-k12-402")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("initial K12 selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, baseOpts)

	firstOpts := withSelectionAttempt(baseOpts)
	secondOpts := withSelectionAttempt(baseOpts)
	for _, opts := range []cliproxyexecutor.Options{firstOpts, secondOpts} {
		selected, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths)
		if err != nil || !handled || selected == nil || selected.ID != k12.ID {
			t.Fatalf("parallel K12 selection = %#v handled=%v err=%v", selected, handled, err)
		}
	}

	directive := selector.OnSelectionResult(context.Background(), Result{
		AuthID:  k12.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusPaymentRequired, Message: "usage limit reached"},
	}, firstOpts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("sibling 402 directive = %#v", directive)
	}
	if _, ok := testK12NewSessionQuarantine(t, selector, k12.ID); !ok {
		t.Fatal("sibling 402 did not quarantine K12 for new sessions")
	}
	bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || !handled || bound == nil || bound.ID != k12.ID {
		t.Fatalf("sibling 402 removed confirmed binding: auth=%#v handled=%v err=%v", bound, handled, err)
	}
	newSession, err := selector.Pick(context.Background(), "codex", "gpt-5", promptCacheOptions("parallel-k12-402-new-session"), auths)
	if err != nil || newSession == nil || newSession.ID != plus.ID {
		t.Fatalf("new session after sibling 402 = %#v, %v; want plus-a", newSession, err)
	}
}

func TestK12ConcurrentSibling403StillClearsAllAuthBindings(t *testing.T) {
	t.Parallel()
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), map[string]*int{"k12-a": intPtr(80)})
	k12 := testK12Auth("k12-a")
	auths := []*Auth{k12, testPlusAuth("plus-a")}
	firstSession := promptCacheOptions("parallel-k12-403-first")
	secondSession := promptCacheOptions("parallel-k12-403-second")
	for _, opts := range []cliproxyexecutor.Options{firstSession, secondSession} {
		selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, auths)
		if err != nil || selected == nil || selected.ID != k12.ID {
			t.Fatalf("confirmed session setup = %#v, %v", selected, err)
		}
		selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, opts)
	}

	firstAttempt := withSelectionAttempt(firstSession)
	secondAttempt := withSelectionAttempt(firstSession)
	for _, opts := range []cliproxyexecutor.Options{firstAttempt, secondAttempt} {
		selected, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths)
		if err != nil || !handled || selected == nil || selected.ID != k12.ID {
			t.Fatalf("parallel K12 selection = %#v handled=%v err=%v", selected, handled, err)
		}
	}

	directive := selector.OnSelectionResult(context.Background(), Result{
		AuthID:  k12.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusForbidden, Message: "credential revoked"},
	}, firstAttempt)
	if directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("sibling 403 directive = %#v", directive)
	}
	for name, opts := range map[string]cliproxyexecutor.Options{"failed": firstSession, "unrelated": secondSession} {
		if bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths); err != nil || handled || bound != nil {
			t.Fatalf("%s binding survived sibling 403: auth=%#v handled=%v err=%v", name, bound, handled, err)
		}
	}
}

func TestK12OldFailureAfterSpilloverWinDoesNotCoolK12(t *testing.T) {
	t.Parallel()
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), map[string]*int{"k12-a": intPtr(20)})
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12, plus}
	baseOpts := promptCacheOptions("plus-wins-before-old-k12-failure")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("initial K12 selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, baseOpts)

	k12Opts := withSelectionAttempt(baseOpts)
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", k12Opts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("active K12 selection = %#v, %v", selected, err)
	}
	identity, _ := extractSessionIdentities(baseOpts.Headers, baseOpts.OriginalRequest, baseOpts.Metadata)
	digest := selector.k12.store.digest(identity.ID)
	until := time.Now().Add(selector.k12.cooldown)
	if err := selector.k12.store.setCooldown(digest, k12.ID, until); err != nil {
		t.Fatal(err)
	}
	selector.k12.markSpilloverPreferred(digest, until)

	plusOpts := withSelectionAttempt(baseOpts)
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", plusOpts, auths)
	if err != nil || selected == nil || selected.ID != plus.ID {
		t.Fatalf("Plus spillover selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: plus.ID, Success: true}, plusOpts)

	directive := selector.OnSelectionResult(context.Background(), Result{
		AuthID:  k12.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusTooManyRequests, Message: "late K12 quota failure"},
	}, k12Opts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("old K12 loser directive = %#v", directive)
	}
	bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || !handled || bound == nil || bound.ID != plus.ID {
		t.Fatalf("Plus winner affinity = %v handled=%v err=%v", bound, handled, err)
	}
}

func TestK12Late402AfterSpilloverWinStillQuarantinesK12(t *testing.T) {
	t.Parallel()
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), map[string]*int{"k12-a": intPtr(20)})
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12, plus}
	baseOpts := promptCacheOptions("plus-wins-before-late-k12-402")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("initial K12 selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, baseOpts)

	k12Opts := withSelectionAttempt(baseOpts)
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", k12Opts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("active K12 selection = %#v, %v", selected, err)
	}
	identity, _ := extractSessionIdentities(baseOpts.Headers, baseOpts.OriginalRequest, baseOpts.Metadata)
	digest := selector.k12.store.digest(identity.ID)
	until := time.Now().Add(selector.k12.cooldown)
	if err := selector.k12.store.setCooldown(digest, k12.ID, until); err != nil {
		t.Fatal(err)
	}
	selector.k12.markSpilloverPreferred(digest, until)

	plusOpts := withSelectionAttempt(baseOpts)
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", plusOpts, auths)
	if err != nil || selected == nil || selected.ID != plus.ID {
		t.Fatalf("Plus spillover selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: plus.ID, Success: true}, plusOpts)

	directive := selector.OnSelectionResult(context.Background(), Result{
		AuthID:  k12.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusPaymentRequired, Message: "usage limit reached"},
	}, k12Opts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("late K12 402 directive = %#v", directive)
	}
	if _, ok := testK12NewSessionQuarantine(t, selector, k12.ID); !ok {
		t.Fatal("late K12 402 did not quarantine K12")
	}
	bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || !handled || bound == nil || bound.ID != plus.ID {
		t.Fatalf("late K12 402 changed Plus winner: auth=%#v handled=%v err=%v", bound, handled, err)
	}
}

func TestK12Late403AfterSpilloverWinStillClearsK12Bindings(t *testing.T) {
	t.Parallel()
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), map[string]*int{"k12-a": intPtr(20)})
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12, plus}
	baseOpts := promptCacheOptions("plus-wins-before-late-k12-403")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("initial K12 selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, baseOpts)
	k12Opts := withSelectionAttempt(baseOpts)
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", k12Opts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("active K12 selection = %#v, %v", selected, err)
	}

	identity, _ := extractSessionIdentities(baseOpts.Headers, baseOpts.OriginalRequest, baseOpts.Metadata)
	digest := selector.k12.store.digest(identity.ID)
	until := time.Now().Add(selector.k12.cooldown)
	if err := selector.k12.store.setCooldown(digest, k12.ID, until); err != nil {
		t.Fatal(err)
	}
	selector.k12.markSpilloverPreferred(digest, until)
	plusOpts := withSelectionAttempt(baseOpts)
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", plusOpts, auths)
	if err != nil || selected == nil || selected.ID != plus.ID {
		t.Fatalf("Plus spillover selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: plus.ID, Success: true}, plusOpts)

	directive := selector.OnSelectionResult(context.Background(), Result{
		AuthID:  k12.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusForbidden, Message: "credential revoked"},
	}, k12Opts)
	if directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("late K12 403 directive = %#v", directive)
	}
	bound, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || !handled || bound == nil || bound.ID != plus.ID {
		t.Fatalf("late K12 403 changed Plus winner: auth=%#v handled=%v err=%v", bound, handled, err)
	}
}

func TestSelectedAuthCallbackIncludesSelectionAttemptAndKeepsLegacyCallback(t *testing.T) {
	t.Parallel()

	var selected cliproxyexecutor.AuthSelection
	meta := map[string]any{
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(selection cliproxyexecutor.AuthSelection) {
			selected = selection
		},
	}
	publishSelectedAuthMetadata(meta, "auth-a", 42)
	if selected.AuthID != "auth-a" || selected.AttemptID != 42 {
		t.Fatalf("selection callback = %#v", selected)
	}
	if got := meta[cliproxyexecutor.SelectedAuthMetadataKey]; got != "auth-a" {
		t.Fatalf("selected auth metadata = %#v", got)
	}

	legacyAuthID := ""
	legacyMeta := map[string]any{
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
			legacyAuthID = authID
		},
	}
	publishSelectedAuthMetadata(legacyMeta, "auth-b", 84)
	if legacyAuthID != "auth-b" {
		t.Fatalf("legacy selection callback auth = %q", legacyAuthID)
	}
}

func TestReportSelectionFailureRetiresAttemptBeforeLateResult(t *testing.T) {
	t.Parallel()

	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), map[string]*int{"k12-a": intPtr(20)})
	manager := NewManager(nil, selector, nil)
	k12 := testK12Auth("k12-a")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12, plus}

	var observed cliproxyexecutor.AuthSelection
	baseOpts := promptCacheOptions("synchronous-timeout-report")
	baseOpts.Metadata = map[string]any{
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(selection cliproxyexecutor.AuthSelection) {
			observed = selection
		},
	}
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("initial selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, baseOpts)

	attemptOpts := withSelectionAttempt(baseOpts)
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", attemptOpts, auths)
	if err != nil || selected == nil || selected.ID != k12.ID {
		t.Fatalf("active selection = %#v, %v", selected, err)
	}
	publishSelectedAuthMetadata(baseOpts.Metadata, selected.ID, selectionAttemptIDFromOptions(attemptOpts))
	if observed.AuthID != k12.ID || observed.AttemptID == 0 {
		t.Fatalf("observed selection = %#v", observed)
	}

	timeoutErr := &retryAfterStatusError{
		status:  http.StatusGatewayTimeout,
		message: "upstream timed out in stream_open attempt=1/2 after 30s",
	}
	directive := manager.ReportSelectionFailure(context.Background(), observed, "gpt-5", timeoutErr, baseOpts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("synchronous timeout directive = %#v", directive)
	}

	lateDirective := selector.OnSelectionResult(context.Background(), Result{AuthID: k12.ID, Success: true}, attemptOpts)
	if !lateDirective.SuppressAvailabilityUpdate || lateDirective.StopAuthAttempt {
		t.Fatalf("late success directive = %#v", lateDirective)
	}
	identity, _ := extractSessionIdentities(baseOpts.Headers, baseOpts.OriginalRequest, baseOpts.Metadata)
	digest := selector.k12.store.digest(identity.ID)
	if remaining := selector.k12.store.cooldownRemaining(digest, k12.ID, time.Now()); remaining <= 0 {
		t.Fatal("late success cleared the synchronous timeout cooldown")
	}

	retry, err := selector.Pick(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || retry == nil || retry.ID != plus.ID {
		t.Fatalf("timeout spillover selection = %#v, %v", retry, err)
	}
}

func TestReportSelectionFailureDoesNotCoolOrdinaryAuth(t *testing.T) {
	t.Parallel()

	manager := NewManager(nil, nil, nil)
	auth := testPlusAuth("plus-a")
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	timeoutErr := &retryAfterStatusError{
		status:  http.StatusGatewayTimeout,
		message: "upstream timed out in stream_open attempt=1/2 after 30s",
	}
	directive := manager.ReportSelectionFailure(
		context.Background(),
		cliproxyexecutor.AuthSelection{AuthID: auth.ID, AttemptID: 42},
		"gpt-5",
		timeoutErr,
		promptCacheOptions("ordinary-auth-local-timeout"),
	)
	if !directive.SuppressAvailabilityUpdate {
		t.Fatalf("local timeout directive = %#v", directive)
	}
	got, ok := manager.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatal("ordinary auth missing after local timeout report")
	}
	if got.Unavailable || got.Quota.Exceeded || len(got.ModelStates) != 0 {
		t.Fatalf("local timeout changed ordinary auth availability: %#v", got)
	}
}

func TestK12OnlyConfirmedCooldownReturnsRetryAfter(t *testing.T) {
	t.Parallel()
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), map[string]*int{"k12-only": intPtr(20)})
	auth := testK12Auth("k12-only")
	opts := promptCacheOptions("only-confirmed-cooldown")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{auth})
	if err != nil || selected == nil {
		t.Fatalf("initial selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: auth.ID, Success: true}, opts)
	selector.OnSelectionResult(context.Background(), Result{
		AuthID:  auth.ID,
		Success: false,
		Error:   &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	}, opts)

	_, err = selector.Pick(context.Background(), "codex", "gpt-5", opts, []*Auth{auth})
	var cooldownErr *k12SessionCooldownError
	if !errors.As(err, &cooldownErr) {
		t.Fatalf("cooldown selection error = %T %v; want k12SessionCooldownError", err, err)
	}
	if cooldownErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("cooldown status = %d", cooldownErr.StatusCode())
	}
	if cooldownErr.Headers().Get("Retry-After") == "" {
		t.Fatal("cooldown response is missing Retry-After")
	}
}

func intPtr(value int) *int {
	return &value
}
