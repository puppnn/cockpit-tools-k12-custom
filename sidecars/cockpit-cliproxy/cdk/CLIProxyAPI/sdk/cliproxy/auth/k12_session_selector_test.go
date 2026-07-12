package auth

import (
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
			QuotaSnapshot: func(auth *Auth) K12QuotaSnapshot {
				value, ok := remaining[auth.ID]
				if !ok {
					return K12QuotaSnapshot{}
				}
				return K12QuotaSnapshot{Fresh: true, HourlyRemainingPercent: value}
			},
		},
	})
	t.Cleanup(selector.Stop)
	return selector
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
	if !strings.Contains(stateText, `"version": 1`) || !strings.Contains(stateText, `"sessionDigest"`) {
		t.Fatalf("state file missing versioned digest: %s", stateText)
	}

	recovered := newTestK12Selector(t, statePath, remaining)
	confirmed, handled, err = recovered.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, []*Auth{auth})
	if err != nil || !handled || confirmed == nil || confirmed.ID != auth.ID {
		t.Fatalf("recovered binding = %v handled=%v err=%v", confirmed, handled, err)
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

func TestK12NewSessionsBalanceByConfirmedCountThenQuota(t *testing.T) {
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
	if err != nil || second == nil || second.ID != "k12-b" {
		t.Fatalf("confirmed-count balancing = %#v, %v", second, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: second.ID, Success: true}, secondOpts)

	remaining["k12-a"] = intPtr(15)
	remaining["k12-b"] = intPtr(40)
	third, err := selector.Pick(context.Background(), "codex", "gpt-5", promptCacheOptions("balance-third"), auths)
	if err != nil || third == nil || third.ID != "k12-b" {
		t.Fatalf("equal-count refreshed quota tie-break = %#v, %v", third, err)
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

func TestK12SingleCandidateCapsActiveSessionsAtTwo(t *testing.T) {
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

func TestK12SingleCandidateConcurrentCapacityLimit(t *testing.T) {
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

	directive = selector.OnSelectionResult(context.Background(), Result{AuthID: retry.ID, Success: false, Error: &Error{HTTPStatus: http.StatusForbidden, Message: "revoked"}}, opts)
	if directive.SuppressAvailabilityUpdate || directive.StopCredentialFallback {
		t.Fatalf("hard failure directive = %#v", directive)
	}
	_, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", opts, auths)
	if handled || err != nil {
		t.Fatalf("hard failure left confirmed binding: handled=%v err=%v", handled, err)
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
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.created"}`)}
	executor := &k12BootstrapExecutor{chunks: chunks}
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

func TestK12StreamOpenTimeoutCauseReleasesBindingForNextAttempt(t *testing.T) {
	t.Parallel()
	remaining := map[string]*int{"k12-a": intPtr(20), "k12-b": intPtr(20)}
	selector := newTestK12Selector(t, filepath.Join(t.TempDir(), "state.json"), remaining)
	manager := NewManager(nil, selector, nil)
	auths := []*Auth{testK12Auth("k12-a"), testK12Auth("k12-b")}
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
	if err != nil || retry == nil || retry.ID == selected.ID {
		t.Fatalf("stream timeout did not move to another K12: first=%s retry=%v err=%v", selected.ID, retry, err)
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

func intPtr(value int) *int {
	return &value
}
