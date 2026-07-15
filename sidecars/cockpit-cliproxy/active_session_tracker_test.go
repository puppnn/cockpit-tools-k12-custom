package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func activeRoutingFixture(priorities map[string]int) (*manifest, []*coreauth.Auth) {
	m := &manifest{
		RoutingStrategy:   "custom",
		accountByAuthID:   make(map[string]*accountSpec),
		accountByID:       make(map[string]*accountSpec),
		accountByAPIKey:   make(map[string]*accountSpec),
		originalIndexByID: make(map[string]int),
	}
	auths := make([]*coreauth.Auth, 0, len(priorities))
	index := 0
	for id, priority := range priorities {
		authID := id + ".json"
		account := &accountSpec{ID: id, AuthID: authID, PlanType: "Plus"}
		m.Accounts = append(m.Accounts, *account)
		m.accountByAuthID[authID] = account
		m.accountByID[id] = account
		m.CustomRoutingRules = append(m.CustomRoutingRules, customRoutingRule{AccountID: id, Priority: priority, Weight: 1})
		m.originalIndexByID[id] = index
		index++
		auths = append(auths, &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive})
	}
	return m, auths
}

func activeRoutingPick(t *testing.T, selector coreauth.Selector, requestID, sessionID string, auths []*coreauth.Auth) *coreauth.Auth {
	t.Helper()
	headers := make(http.Header)
	headers.Set("X-Session-ID", sessionID)
	ctx := internallogging.WithRequestID(context.Background(), requestID)
	selected, err := selector.Pick(ctx, "codex", "gpt-5.4", cliproxyexecutor.Options{Headers: headers}, auths)
	if err != nil {
		t.Fatalf("Pick(%s): %v", requestID, err)
	}
	if selected == nil {
		t.Fatalf("Pick(%s) returned nil", requestID)
	}
	return selected
}

func TestActiveSessionTrackerCountsDistinctSessionsAndReferenceReleases(t *testing.T) {
	m, auths := activeRoutingFixture(map[string]int{"high": 10, "low": 0})
	tracker := newActiveSessionTracker()
	selector := &cockpitSelector{manifest: m, sessions: tracker}

	first := activeRoutingPick(t, selector, "request-1", "shared-session", auths)
	second := activeRoutingPick(t, selector, "request-2", "shared-session", auths)
	if first.ID != "high.json" || second.ID != first.ID {
		t.Fatalf("parallel requests for one session split across auths: first=%s second=%s", first.ID, second.ID)
	}
	if got := tracker.activeLoad(first.ID); got != 1 {
		t.Fatalf("one distinct session with two requests has load %d, want 1", got)
	}

	tracker.releaseRequest("request-1")
	if got := tracker.activeLoad(first.ID); got != 1 {
		t.Fatalf("releasing one of two requests changed distinct-session load to %d", got)
	}
	tracker.releaseRequest("request-2")
	if got := tracker.activeLoad(first.ID); got != 0 {
		t.Fatalf("load after final request release = %d, want 0", got)
	}
}

func TestCustomRoutingUsesHighestPriorityUntilFourThenFallsThrough(t *testing.T) {
	m, auths := activeRoutingFixture(map[string]int{"high": 10, "low": 0})
	tracker := newActiveSessionTracker()
	selector := &cockpitSelector{manifest: m, sessions: tracker}

	for i := 0; i < nonK12MaxConcurrentSessions; i++ {
		selected := activeRoutingPick(t, selector, "high-request-"+string(rune('a'+i)), "high-session-"+string(rune('a'+i)), auths)
		if selected.ID != "high.json" {
			t.Fatalf("selection %d = %s, want high.json", i, selected.ID)
		}
	}
	spill := activeRoutingPick(t, selector, "spill-request", "spill-session", auths)
	if spill.ID != "low.json" {
		t.Fatalf("selection after high tier reached capacity = %s, want low.json", spill.ID)
	}

	tracker.releaseRequest("high-request-a")
	reused := activeRoutingPick(t, selector, "reused-request", "reused-session", auths)
	if reused.ID != "high.json" {
		t.Fatalf("selection after high-priority capacity released = %s, want high.json", reused.ID)
	}
}

func TestCustomRoutingKeepsK12AheadOfHigherPriorityPlus(t *testing.T) {
	m, auths := activeRoutingFixture(map[string]int{"plus-high": 100})
	k12 := &accountSpec{ID: "k12-low", AuthID: "k12-low.json", PlanType: "K12"}
	m.Accounts = append(m.Accounts, *k12)
	m.accountByAuthID[k12.AuthID] = k12
	m.accountByID[k12.ID] = k12
	m.CustomRoutingRules = append(m.CustomRoutingRules, customRoutingRule{AccountID: k12.ID, Priority: -100, Weight: 1})
	auths = append(auths, &coreauth.Auth{ID: k12.AuthID, Provider: "codex", Status: coreauth.StatusActive})
	tracker := newActiveSessionTracker()
	selector := &cockpitSelector{manifest: m, sessions: tracker}

	selected := activeRoutingPick(t, selector, "k12-first-request", "k12-first-session", auths)
	if selected.ID != k12.AuthID {
		t.Fatalf("mixed custom-priority selection = %s, want K12 %s", selected.ID, k12.AuthID)
	}
}

func TestCustomRoutingWithSessionTrackerHandlesMissingManifest(t *testing.T) {
	tracker := newActiveSessionTracker()
	selector := &cockpitSelector{sessions: tracker}
	auth := &coreauth.Auth{ID: "auth.json", Provider: "codex", Status: coreauth.StatusActive}
	selected := activeRoutingPick(t, selector, "nil-manifest-request", "nil-manifest-session", []*coreauth.Auth{auth})
	if selected.ID != auth.ID {
		t.Fatalf("nil-manifest selection = %s, want %s", selected.ID, auth.ID)
	}
}

func TestCustomRoutingBalancesEqualPriorityByActiveSessionLoad(t *testing.T) {
	m, auths := activeRoutingFixture(map[string]int{"account-a": 5, "account-b": 5})
	for index := range m.CustomRoutingRules {
		if m.CustomRoutingRules[index].AccountID == "account-a" {
			m.CustomRoutingRules[index].Weight = 100
		}
	}
	tracker := newActiveSessionTracker()
	selector := &cockpitSelector{manifest: m, sessions: tracker}

	selectedCounts := map[string]int{}
	for i := 0; i < 6; i++ {
		selected := activeRoutingPick(t, selector, "request-"+string(rune('a'+i)), "session-"+string(rune('a'+i)), auths)
		selectedCounts[selected.ID]++
	}
	if selectedCounts["account-a.json"] != 3 || selectedCounts["account-b.json"] != 3 {
		t.Fatalf("equal-priority distribution = %#v, want three sessions per account", selectedCounts)
	}
}

func TestActiveSessionTrackerMovesRetryReservationWithoutDoubleCounting(t *testing.T) {
	tracker := newActiveSessionTracker()
	headers := make(http.Header)
	headers.Set("X-Session-ID", "retry-session")
	opts := cliproxyexecutor.Options{Headers: headers}
	ctx := internallogging.WithRequestID(context.Background(), "retry-request")

	tracker.reserveSelection(ctx, opts, "auth-a")
	tracker.reserveSelection(ctx, opts, "auth-b")
	tracker.reserveSelection(ctx, opts, "auth-b")
	if got := tracker.activeLoad("auth-a"); got != 0 {
		t.Fatalf("old retry auth load = %d, want 0", got)
	}
	if got := tracker.activeLoad("auth-b"); got != 1 {
		t.Fatalf("replacement retry auth load = %d, want 1", got)
	}
	tracker.releaseRequest("retry-request")
	if got := tracker.activeLoad("auth-b"); got != 0 {
		t.Fatalf("replacement load after release = %d, want 0", got)
	}
}

func TestCustomRoutingConcurrentAdmissionDoesNotExceedFourPerAccount(t *testing.T) {
	m, auths := activeRoutingFixture(map[string]int{"account-a": 5, "account-b": 5})
	tracker := newActiveSessionTracker()
	selector := &cockpitSelector{manifest: m, sessions: tracker}

	type result struct {
		authID string
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, nonK12MaxConcurrentSessions*2)
	for i := 0; i < nonK12MaxConcurrentSessions*2; i++ {
		i := i
		go func() {
			<-start
			headers := make(http.Header)
			headers.Set("X-Session-ID", "concurrent-session-"+string(rune('a'+i)))
			ctx := internallogging.WithRequestID(context.Background(), "concurrent-request-"+string(rune('a'+i)))
			selected, err := selector.Pick(ctx, "codex", "gpt-5.4", cliproxyexecutor.Options{Headers: headers}, auths)
			if selected == nil {
				results <- result{err: err}
				return
			}
			results <- result{authID: selected.ID, err: err}
		}()
	}
	close(start)
	counts := map[string]int{}
	for i := 0; i < nonK12MaxConcurrentSessions*2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent Pick: %v", result.err)
		}
		counts[result.authID]++
	}
	if counts["account-a.json"] != 4 || counts["account-b.json"] != 4 {
		t.Fatalf("concurrent distribution = %#v, want four per account", counts)
	}

	headers := make(http.Header)
	headers.Set("X-Session-ID", "over-capacity-session")
	ctx := internallogging.WithRequestID(context.Background(), "over-capacity-request")
	selected, err := selector.Pick(ctx, "codex", "gpt-5.4", cliproxyexecutor.Options{Headers: headers}, auths)
	if err == nil || selected != nil {
		t.Fatalf("all-full Pick() = %#v, %v; want a capacity error", selected, err)
	}
}

func TestCustomRoutingFallsToLowerTierOnlyAfterEveryTopAccountIsFull(t *testing.T) {
	m, auths := activeRoutingFixture(map[string]int{"top-a": 10, "top-b": 10, "lower": 0})
	tracker := newActiveSessionTracker()
	selector := &cockpitSelector{manifest: m, sessions: tracker}

	counts := map[string]int{}
	for i := 0; i < nonK12MaxConcurrentSessions*2; i++ {
		selected := activeRoutingPick(t, selector, "top-request-"+string(rune('a'+i)), "top-session-"+string(rune('a'+i)), auths)
		counts[selected.ID]++
	}
	if counts["top-a.json"] != 4 || counts["top-b.json"] != 4 || counts["lower.json"] != 0 {
		t.Fatalf("top-tier distribution before capacity = %#v", counts)
	}
	selected := activeRoutingPick(t, selector, "lower-request", "lower-session", auths)
	if selected.ID != "lower.json" {
		t.Fatalf("selection after top tier filled = %s, want lower.json", selected.ID)
	}
}

func TestBackupAccountReceivesNewSessionAfterRegularCapacityIsFull(t *testing.T) {
	m, auths := activeRoutingFixture(map[string]int{"regular": 10})
	backup := &accountSpec{ID: "backup", AuthID: "backup.json", PlanType: "API_KEY"}
	m.Accounts = append(m.Accounts, *backup)
	m.accountByAuthID[backup.AuthID] = backup
	m.accountByID[backup.ID] = backup
	m.CustomRoutingRules = append(m.CustomRoutingRules, customRoutingRule{
		AccountID: backup.ID,
		Priority:  -100,
		Weight:    1,
		IsBackup:  true,
	})
	auths = append(auths, &coreauth.Auth{ID: backup.AuthID, Provider: "codex", Status: coreauth.StatusActive})
	tracker := newActiveSessionTracker()
	base := &cockpitSelector{manifest: m, sessions: tracker}
	selector := &backupAccountSelector{manifest: m, fallback: base}

	for i := 0; i < nonK12MaxConcurrentSessions; i++ {
		selected := activeRoutingPick(t, selector, "regular-request-"+string(rune('a'+i)), "regular-session-"+string(rune('a'+i)), auths)
		if selected.ID != "regular.json" {
			t.Fatalf("regular selection %d = %s, want regular.json", i, selected.ID)
		}
	}
	selected := activeRoutingPick(t, selector, "backup-request", "backup-session", auths)
	if selected.ID != backup.AuthID {
		t.Fatalf("selection after regular capacity = %s, want backup %s", selected.ID, backup.AuthID)
	}
}

func TestExistingAffinityRemainsOnBoundAuthWhenNewSessionCapacityIsFull(t *testing.T) {
	m, auths := activeRoutingFixture(map[string]int{"high": 10, "low": 0})
	tracker := newActiveSessionTracker()
	base := &cockpitSelector{manifest: m, sessions: tracker}
	affinity := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: base,
		TTL:      time.Hour,
	})
	t.Cleanup(affinity.Stop)
	selector := &recordingSelector{inner: affinity, manifest: m, sessions: tracker}

	old := activeRoutingPick(t, selector, "old-initial", "old-session", auths)
	if old.ID != "high.json" {
		t.Fatalf("initial old-session auth = %s, want high.json", old.ID)
	}
	tracker.releaseRequest("old-initial")
	for i := 0; i < nonK12MaxConcurrentSessions; i++ {
		selected := activeRoutingPick(t, selector, "fill-request-"+string(rune('a'+i)), "fill-session-"+string(rune('a'+i)), auths)
		if selected.ID != "high.json" {
			t.Fatalf("fill selection %d = %s, want high.json", i, selected.ID)
		}
	}

	sticky := activeRoutingPick(t, selector, "old-resumed", "old-session", auths)
	if sticky.ID != "high.json" {
		t.Fatalf("existing affinity moved at capacity: got %s", sticky.ID)
	}
	if got := tracker.activeLoad("high.json"); got != nonK12MaxConcurrentSessions+1 {
		t.Fatalf("active load after preserved affinity = %d, want %d", got, nonK12MaxConcurrentSessions+1)
	}
	newSession := activeRoutingPick(t, selector, "new-request", "new-session", auths)
	if newSession.ID != "low.json" {
		t.Fatalf("new session at high-account capacity = %s, want low.json", newSession.ID)
	}
}

func TestRequestPolicyReleasesActiveSessionAfterHandlerCompletes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tracker := newActiveSessionTracker()
	policy := &requestPolicy{sessions: tracker}
	router := gin.New()
	router.Use(policy.middleware())
	router.POST("/v1/responses", func(c *gin.Context) {
		tracker.reserveSelection(c.Request.Context(), cliproxyexecutor.Options{Headers: c.Request.Header}, "auth.json")
		if got := tracker.activeLoad("auth.json"); got != 1 {
			t.Fatalf("load inside handler = %d, want 1", got)
		}
		c.Status(http.StatusOK)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("X-Session-ID", "middleware-session")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d, want 200", response.Code)
	}
	if got := tracker.activeLoad("auth.json"); got != 0 {
		t.Fatalf("load after middleware completion = %d, want 0", got)
	}
}
