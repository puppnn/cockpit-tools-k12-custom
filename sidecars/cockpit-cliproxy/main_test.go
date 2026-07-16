package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexClientModelsResponseShape(t *testing.T) {
	response := buildCodexClientModelsResponse([]string{"gpt-5.4", "gpt-image-2", codexAutoReviewModel})
	models, ok := response["models"].([]map[string]any)
	if !ok {
		t.Fatalf("models response should contain a models array: %#v", response["models"])
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	textModel := findCodexClientModelForTest(models, "gpt-5.4")
	imageModel := findCodexClientModelForTest(models, "gpt-image-2")
	reviewModel := findCodexClientModelForTest(models, codexAutoReviewModel)
	if textModel == nil || imageModel == nil || reviewModel == nil {
		t.Fatalf("expected all requested models, got %#v", models)
	}
	if _, ok := textModel["prefer_websockets"].(bool); !ok {
		t.Fatalf("text model should keep websocket preference: %#v", textModel)
	}
	if textModel["visibility"] != "list" {
		t.Fatalf("text model should be listed in Codex client catalog: %#v", textModel)
	}
	if textModel["shell_type"] != "shell_command" || textModel["supported_in_api"] != true {
		t.Fatalf("text model should keep required Codex catalog fields: %#v", textModel)
	}
	if _, ok := textModel["input_modalities"].([]any); !ok {
		t.Fatalf("text model should keep input modalities: %#v", textModel)
	}
	if imageModel["visibility"] != "hide" {
		t.Fatalf("image model should be hidden in Codex client catalog: %#v", imageModel)
	}
	if reviewModel["visibility"] != "hide" {
		t.Fatalf("auto review model should be hidden in Codex client catalog: %#v", reviewModel)
	}
}

func TestCodexSparkUsesCompleteCodexClientCatalogTemplate(t *testing.T) {
	response := buildCodexClientModelsResponse([]string{codexSparkCatalogTemplateModel, codexSparkModel})
	models, ok := response["models"].([]map[string]any)
	if !ok {
		t.Fatalf("models response should contain a models array: %#v", response["models"])
	}
	template := findCodexClientModelForTest(models, codexSparkCatalogTemplateModel)
	spark := findCodexClientModelForTest(models, codexSparkModel)
	if template == nil || spark == nil {
		t.Fatalf("expected template and Spark models, got %#v", models)
	}
	if spark["display_name"] != "GPT-5.3 Codex Spark" || spark["visibility"] != "list" || spark["supported_in_api"] != true {
		t.Fatalf("Spark should be listed as an API model: %#v", spark)
	}
	for _, field := range []string{"available_in_plans", "base_instructions", "minimal_client_version", "model_messages", "prefer_websockets"} {
		if spark[field] == nil || !reflect.DeepEqual(spark[field], template[field]) {
			t.Fatalf("Spark should inherit %s from the Codex client template: %#v", field, spark[field])
		}
	}
}

func findCodexClientModelForTest(models []map[string]any, slug string) map[string]any {
	for _, model := range models {
		if model["slug"] == slug {
			return model
		}
	}
	return nil
}

func TestVisibleModelsForAPIKeyUsesPrefixAndFilters(t *testing.T) {
	spec := &apiKeySpec{
		ModelPrefix:    "team",
		AllowedModels:  []string{"gpt-*"},
		ExcludedModels: []string{"gpt-image-*"},
	}
	m := &manifest{
		ModelIDs: []string{"gpt-5.4", "gpt-image-2", "custom-model"},
	}

	models := visibleModelsForAPIKey(m, spec)

	if len(models) != 1 || models[0] != "team/gpt-5.4" {
		t.Fatalf("unexpected visible models: %#v", models)
	}
}

func TestCockpitSelectorMovesBoundOAuthAccountToFinalFallback(t *testing.T) {
	bound := &accountSpec{ID: "bound", AuthID: "bound.json"}
	preferred := &accountSpec{ID: "preferred", AuthID: "preferred.json"}
	other := &accountSpec{ID: "other", AuthID: "other.json"}
	m := &manifest{
		Accounts:            []accountSpec{*bound, *preferred, *other},
		RoutingStrategy:     "custom",
		BoundOAuthAccountID: "bound",
		CustomRoutingRules: []customRoutingRule{
			{AccountID: "bound", Priority: 100, Weight: 1},
			{AccountID: "preferred", Priority: 10, Weight: 1},
			{AccountID: "other", Priority: 0, Weight: 1},
		},
		accountByAuthID: map[string]*accountSpec{
			"bound.json":     bound,
			"preferred.json": preferred,
			"other.json":     other,
		},
		originalIndexByID: map[string]int{"bound": 0, "preferred": 1, "other": 2},
	}
	selector := &cockpitSelector{manifest: m}
	auths := []*coreauth.Auth{{ID: "bound.json"}, {ID: "preferred.json"}, {ID: "other.json"}}

	ordered := selector.orderAuths(auths, 0)
	got := []string{ordered[0].ID, ordered[1].ID, ordered[2].ID}
	want := []string{"preferred.json", "other.json", "bound.json"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ordered auths = %v, want %v", got, want)
		}
	}
}

func TestK12SpilloverAccountSpecAllowsPaidOAuthOnly(t *testing.T) {
	paidRank := 300
	goRank := 200
	freeRank := 100
	tests := []struct {
		name    string
		account *accountSpec
		want    bool
	}{
		{name: "plus", account: &accountSpec{PlanType: "Plus", AuthID: "plus.json"}, want: true},
		{name: "team", account: &accountSpec{PlanType: "TEAM", AuthID: "team.json"}, want: true},
		{name: "paid dynamic plan", account: &accountSpec{PlanType: "future-paid", AuthID: "paid.json", PlanRank: &paidRank}, want: true},
		{name: "go by name", account: &accountSpec{PlanType: "Go", AuthID: "go.json"}, want: false},
		{name: "go by rank", account: &accountSpec{PlanType: "future-go", AuthID: "go-rank.json", PlanRank: &goRank}, want: false},
		{name: "free by name", account: &accountSpec{PlanType: "Free", AuthID: "free.json"}, want: false},
		{name: "free by rank", account: &accountSpec{PlanType: "future-free", AuthID: "free-rank.json", PlanRank: &freeRank}, want: false},
		{name: "k12", account: &accountSpec{PlanType: "K12", AuthID: "k12.json"}, want: false},
		{name: "api key plan", account: &accountSpec{PlanType: "API_KEY", AuthID: "api-key.json"}, want: false},
		{name: "api key credential", account: &accountSpec{PlanType: "Plus", AuthID: "api-key.json", UpstreamAPIKey: "secret"}, want: false},
		{name: "missing oauth auth id", account: &accountSpec{PlanType: "Plus"}, want: false},
		{name: "unknown plan", account: &accountSpec{PlanType: "unknown", AuthID: "unknown.json"}, want: false},
		{name: "nil account", account: nil, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isK12SpilloverAccountSpec(test.account); got != test.want {
				t.Fatalf("isK12SpilloverAccountSpec() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestK12QuotaSnapshotForAuthPreservesKnownWeeklyQuotaWhenStale(t *testing.T) {
	k12 := &accountSpec{ID: "k12", AuthID: "k12.json", PlanType: "K12"}
	m := &manifest{
		Accounts: []accountSpec{*k12},
		accountByAuthID: map[string]*accountSpec{
			k12.AuthID: k12,
		},
	}
	now := time.Now()
	hourlyRemaining := 99
	weeklyRemaining := 0
	windowPresent := true
	windowAbsent := false
	staleUpdatedAt := now.Add(-4 * time.Minute).Unix()
	freshUpdatedAt := now.Unix()
	auth := &coreauth.Auth{ID: k12.AuthID, Provider: "codex"}

	t.Run("stale weekly zero remains known", func(t *testing.T) {
		quota := newQuotaReserveStateStore("", m)
		quota.snapshot.Store(quotaReserveRuntimeState{accounts: map[string]quotaReserveSnapshot{
			k12.ID: {
				SnapshotUpdatedAtUnixSeconds: &staleUpdatedAt,
				HourlyRemainingPercent:       &hourlyRemaining,
				WeeklyRemainingPercent:       &weeklyRemaining,
				HourlyWindowPresent:          &windowPresent,
				WeeklyWindowPresent:          &windowPresent,
			},
		}})

		got := k12QuotaSnapshotForAuth(m, quota, auth, now)
		if got.Fresh {
			t.Fatal("stale quota snapshot must not be marked fresh")
		}
		if got.HourlyRemainingPercent == nil || *got.HourlyRemainingPercent != hourlyRemaining {
			t.Fatalf("stale hourly remaining = %v, want %d", got.HourlyRemainingPercent, hourlyRemaining)
		}
		if got.WeeklyRemainingPercent == nil || *got.WeeklyRemainingPercent != weeklyRemaining {
			t.Fatalf("stale weekly remaining = %v, want %d", got.WeeklyRemainingPercent, weeklyRemaining)
		}
	})

	t.Run("absent weekly window omits weekly remaining", func(t *testing.T) {
		quota := newQuotaReserveStateStore("", m)
		quota.snapshot.Store(quotaReserveRuntimeState{accounts: map[string]quotaReserveSnapshot{
			k12.ID: {
				SnapshotUpdatedAtUnixSeconds: &freshUpdatedAt,
				HourlyRemainingPercent:       &hourlyRemaining,
				WeeklyRemainingPercent:       &weeklyRemaining,
				HourlyWindowPresent:          &windowPresent,
				WeeklyWindowPresent:          &windowAbsent,
			},
		}})

		got := k12QuotaSnapshotForAuth(m, quota, auth, now)
		if !got.Fresh {
			t.Fatal("current quota snapshot should be marked fresh")
		}
		if got.WeeklyRemainingPercent != nil {
			t.Fatalf("absent weekly window returned remaining %v", got.WeeklyRemainingPercent)
		}
	})

	t.Run("unknown weekly window omits weekly remaining", func(t *testing.T) {
		quota := newQuotaReserveStateStore("", m)
		quota.snapshot.Store(quotaReserveRuntimeState{accounts: map[string]quotaReserveSnapshot{
			k12.ID: {
				SnapshotUpdatedAtUnixSeconds: &freshUpdatedAt,
				HourlyRemainingPercent:       &hourlyRemaining,
				WeeklyRemainingPercent:       &weeklyRemaining,
				HourlyWindowPresent:          &windowPresent,
				WeeklyWindowPresent:          nil,
			},
		}})

		got := k12QuotaSnapshotForAuth(m, quota, auth, now)
		if !got.Fresh {
			t.Fatal("current quota snapshot should be marked fresh")
		}
		if got.WeeklyRemainingPercent != nil {
			t.Fatalf("unknown weekly window returned remaining %v", got.WeeklyRemainingPercent)
		}
	})

	t.Run("auth plan type recovers missing manifest plan type", func(t *testing.T) {
		k12.PlanType = ""
		auth.Attributes = map[string]string{"plan_type": "K12"}
		quota := newQuotaReserveStateStore("", m)
		quota.snapshot.Store(quotaReserveRuntimeState{accounts: map[string]quotaReserveSnapshot{
			k12.ID: {
				SnapshotUpdatedAtUnixSeconds: &freshUpdatedAt,
				HourlyRemainingPercent:       &hourlyRemaining,
				HourlyWindowPresent:          &windowPresent,
			},
		}})

		got := k12QuotaSnapshotForAuth(m, quota, auth, now)
		if !got.Fresh || got.HourlyRemainingPercent == nil || *got.HourlyRemainingPercent != hourlyRemaining {
			t.Fatalf("auth-derived K12 quota snapshot = %#v", got)
		}
	})
}

func TestBuildCoreAuthSelectorRecoversK12PlanFromAuthAttributes(t *testing.T) {
	t.Parallel()
	paidRank := 300
	k12 := &accountSpec{ID: "k12", AuthID: "k12.json"}
	plus := &accountSpec{ID: "plus", AuthID: "plus.json", PlanRank: &paidRank}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "local", Key: "test-local-key", Enabled: true}},
		Accounts: []accountSpec{*k12, *plus},
		accountByAuthID: map[string]*accountSpec{
			k12.AuthID:  k12,
			plus.AuthID: plus,
		},
		originalIndexByID: map[string]int{k12.ID: 0, plus.ID: 1},
	}
	selector := buildCoreAuthSelector(&config.Config{}, &cockpitSelector{manifest: m}, m, nil)
	if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
		t.Cleanup(stoppable.Stop)
	}
	auths := []*coreauth.Auth{
		{ID: k12.AuthID, Provider: "codex", Attributes: map[string]string{"plan_type": "k12"}},
		{ID: plus.AuthID, Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
	}

	for index, wantAuthID := range []string{k12.AuthID, k12.AuthID, plus.AuthID} {
		opts := cliproxyexecutor.Options{OriginalRequest: []byte(fmt.Sprintf(`{"prompt_cache_key":"manifest-plan-fallback-%d","input":[]}`, index))}
		selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
		if err != nil || selected == nil || selected.ID != wantAuthID {
			t.Fatalf("selection %d = %#v, %v; want %s", index, selected, err, wantAuthID)
		}
	}
}

func TestBuildCoreAuthSelectorUsesPaidOAuthMembersForK12CapacitySpillover(t *testing.T) {
	k12 := &accountSpec{ID: "k12", AuthID: "k12.json", PlanType: "K12"}
	plus := &accountSpec{ID: "plus", AuthID: "plus.json", PlanType: "Plus"}
	team := &accountSpec{ID: "team", AuthID: "team.json", PlanType: "Team"}
	apiKey := &accountSpec{ID: "api-key", AuthID: "api-key.json", PlanType: "API_KEY", UpstreamAPIKey: "upstream-key"}
	m := &manifest{
		APIKeys:             []apiKeySpec{{ID: "local", Key: "test-local-key", Enabled: true}},
		Accounts:            []accountSpec{*k12, *plus, *team, *apiKey},
		RoutingStrategy:     "custom",
		BoundOAuthAccountID: "bound-account-outside-member-pool",
		CustomRoutingRules: []customRoutingRule{
			{AccountID: apiKey.ID, Priority: 100, Weight: 1},
			{AccountID: plus.ID, Priority: 20, Weight: 1},
			{AccountID: team.ID, Priority: 10, Weight: 1},
			{AccountID: k12.ID, Priority: 1, Weight: 1},
		},
		accountByAuthID: map[string]*accountSpec{
			k12.AuthID:    k12,
			plus.AuthID:   plus,
			team.AuthID:   team,
			apiKey.AuthID: apiKey,
		},
		originalIndexByID: map[string]int{k12.ID: 0, plus.ID: 1, team.ID: 2, apiKey.ID: 3},
	}
	selector := buildCoreAuthSelector(&config.Config{}, &cockpitSelector{manifest: m}, m, nil)
	if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
		t.Cleanup(stoppable.Stop)
	}
	auths := []*coreauth.Auth{
		{ID: k12.AuthID, Provider: "codex"},
		{ID: apiKey.AuthID, Provider: "codex"},
		{ID: team.AuthID, Provider: "codex"},
		{ID: plus.AuthID, Provider: "codex"},
	}
	want := []string{k12.AuthID, k12.AuthID, plus.AuthID}
	for index, wantAuthID := range want {
		opts := cliproxyexecutor.Options{OriginalRequest: []byte(fmt.Sprintf(`{"prompt_cache_key":"member-spillover-%d","input":[]}`, index))}
		selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
		if err != nil || selected == nil || selected.ID != wantAuthID {
			t.Fatalf("capacity selection %d = %#v, %v; want %s", index, selected, err, wantAuthID)
		}
	}

	threshold := 10
	remaining := 10
	updatedAt := time.Now().Unix()
	windowPresent := true
	plus.QuotaReserve = &quotaReserveSpec{
		HourlyThresholdPercent:       &threshold,
		SnapshotUpdatedAtUnixSeconds: &updatedAt,
		HourlyRemainingPercent:       &remaining,
		HourlyWindowPresent:          &windowPresent,
	}
	blockedOpts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"member-plus-reserved","input":[]}`)}
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", blockedOpts, auths)
	if err != nil || selected == nil || selected.ID != team.AuthID {
		t.Fatalf("capacity selection with reserved Plus = %#v, %v; want %s", selected, err, team.AuthID)
	}

	plus.QuotaReserve = nil
	auths[3].Disabled = true
	disabledOpts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"member-plus-disabled","input":[]}`)}
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5.4", disabledOpts, auths)
	if err != nil || selected == nil || selected.ID != team.AuthID {
		t.Fatalf("capacity selection with disabled Plus = %#v, %v; want %s", selected, err, team.AuthID)
	}

	auths[3].Disabled = false
	auths[3].ModelStates = map[string]*coreauth.ModelState{
		"gpt-5.4": {Status: coreauth.StatusDisabled},
	}
	modelBlockedOpts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"member-plus-model-blocked","input":[]}`)}
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5.4", modelBlockedOpts, auths)
	if err != nil || selected == nil || selected.ID != team.AuthID {
		t.Fatalf("capacity selection with model-blocked Plus = %#v, %v; want %s", selected, err, team.AuthID)
	}
}

func TestQuotaReserveSelectorReroutesEstablishedK12Spillover(t *testing.T) {
	k12 := &accountSpec{ID: "k12", AuthID: "k12.json", PlanType: "K12"}
	hourlyThreshold := 10
	hourlyRemaining := 80
	updatedAt := time.Now().Unix()
	windowPresent := true
	plus := &accountSpec{
		ID:       "plus",
		AuthID:   "plus.json",
		PlanType: "Plus",
		QuotaReserve: &quotaReserveSpec{
			HourlyThresholdPercent:       &hourlyThreshold,
			SnapshotUpdatedAtUnixSeconds: &updatedAt,
			HourlyRemainingPercent:       &hourlyRemaining,
			HourlyWindowPresent:          &windowPresent,
		},
	}
	team := &accountSpec{ID: "team", AuthID: "team.json", PlanType: "Team"}
	m := &manifest{
		APIKeys:         []apiKeySpec{{ID: "local", Key: "test-local-key", Enabled: true}},
		Accounts:        []accountSpec{*k12, *plus, *team},
		RoutingStrategy: "custom",
		CustomRoutingRules: []customRoutingRule{
			{AccountID: plus.ID, Priority: 20, Weight: 1},
			{AccountID: team.ID, Priority: 10, Weight: 1},
			{AccountID: k12.ID, Priority: 1, Weight: 1},
		},
		accountByAuthID: map[string]*accountSpec{
			k12.AuthID:  k12,
			plus.AuthID: plus,
			team.AuthID: team,
		},
		originalIndexByID: map[string]int{k12.ID: 0, plus.ID: 1, team.ID: 2},
	}
	selector := buildCoreAuthSelector(&config.Config{}, &cockpitSelector{manifest: m}, m, nil)
	if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
		t.Cleanup(stoppable.Stop)
	}
	auths := []*coreauth.Auth{
		{ID: k12.AuthID, Provider: "codex"},
		{ID: team.AuthID, Provider: "codex"},
		{ID: plus.AuthID, Provider: "codex"},
	}
	for index := 0; index < 2; index++ {
		opts := cliproxyexecutor.Options{OriginalRequest: []byte(fmt.Sprintf(`{"prompt_cache_key":"reserve-reroute-capacity-%d","input":[]}`, index))}
		selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
		if err != nil || selected == nil || selected.ID != k12.AuthID {
			t.Fatalf("capacity selection %d = %#v, %v; want %s", index, selected, err, k12.AuthID)
		}
	}

	spilloverOpts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"reserve-reroute-spillover","input":[]}`)}
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", spilloverOpts, auths)
	if err != nil || selected == nil || selected.ID != plus.AuthID {
		t.Fatalf("initial spillover = %#v, %v; want %s", selected, err, plus.AuthID)
	}

	hourlyRemaining = hourlyThreshold
	preSelector, ok := selector.(coreauth.PreAvailabilitySelector)
	if !ok {
		t.Fatal("quota reserve selector should expose pre-availability selection")
	}
	preselected, handled, err := preSelector.PickBeforeAvailability(
		context.Background(),
		"codex",
		"gpt-5.4",
		spilloverOpts,
		auths,
	)
	if err != nil || handled || preselected != nil {
		t.Fatalf("reserve-blocked spillover preselection = %#v, handled=%t, err=%v; want ordinary reroute", preselected, handled, err)
	}

	selected, err = selector.Pick(context.Background(), "codex", "gpt-5.4", spilloverOpts, auths)
	if err != nil || selected == nil || selected.ID != team.AuthID {
		t.Fatalf("reserve-blocked spillover reroute = %#v, %v; want %s", selected, err, team.AuthID)
	}
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5.4", spilloverOpts, auths)
	if err != nil || selected == nil || selected.ID != team.AuthID {
		t.Fatalf("replacement spillover after capacity recheck = %#v, %v; want %s", selected, err, team.AuthID)
	}
}

func TestBuildCoreAuthSelectorReadsPreferredNewSessionPoolFromHotState(t *testing.T) {
	first := &accountSpec{ID: "first", AuthID: "first.json", PlanType: "Plus"}
	preferred := &accountSpec{ID: "preferred", AuthID: "preferred.json", PlanType: "Plus"}
	m := &manifest{
		Accounts: []accountSpec{*first, *preferred},
		accountByID: map[string]*accountSpec{
			first.ID:     first,
			preferred.ID: preferred,
		},
		accountByAuthID: map[string]*accountSpec{
			first.AuthID:     first,
			preferred.AuthID: preferred,
		},
	}
	statePath := filepath.Join(t.TempDir(), "quota-reserve-state.json")
	writeState := func(enabled bool, accountIDs []string) {
		t.Helper()
		content, err := json.Marshal(quotaReserveStateFile{
			Version:                      quotaReserveStateVersion,
			Accounts:                     map[string]quotaReserveSnapshot{},
			NewSessionPriorityEnabled:    enabled,
			NewSessionPriorityAccountIDs: accountIDs,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeState(true, []string{preferred.ID})
	quota := newQuotaReserveStateStore(statePath, m)
	if err := quota.load(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Routing.SessionAffinity = true
	selector := buildCoreAuthSelector(cfg, &coreauth.FillFirstSelector{}, m, quota)
	if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
		t.Cleanup(stoppable.Stop)
	}
	auths := []*coreauth.Auth{{ID: first.AuthID, Provider: "codex"}, {ID: preferred.AuthID, Provider: "codex"}}

	selected, err := selector.Pick(
		context.Background(),
		"codex",
		"gpt-5.4",
		cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"preferred-hot-state"}`)},
		auths,
	)
	if err != nil || selected == nil || selected.ID != preferred.AuthID {
		t.Fatalf("preferred hot-state Pick() = %#v, %v; want %s", selected, err, preferred.AuthID)
	}

	writeState(false, nil)
	if err := quota.load(); err != nil {
		t.Fatal(err)
	}
	selected, err = selector.Pick(
		context.Background(),
		"codex",
		"gpt-5.4",
		cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"normal-hot-state"}`)},
		auths,
	)
	if err != nil || selected == nil || selected.ID != first.AuthID {
		t.Fatalf("disabled hot-state Pick() = %#v, %v; want %s", selected, err, first.AuthID)
	}
}

func TestClientCatalogModelsIncludesAutoReviewWithoutPrefix(t *testing.T) {
	spec := &apiKeySpec{
		ModelPrefix:    "team",
		AllowedModels:  []string{"gpt-*"},
		ExcludedModels: []string{"gpt-image-*"},
	}
	m := &manifest{
		ModelIDs: []string{"gpt-5.4", "gpt-image-2", "custom-model"},
	}

	models := clientCatalogModelsForAPIKey(m, spec)

	if len(models) != 2 || models[0] != "team/gpt-5.4" || models[1] != codexAutoReviewModel {
		t.Fatalf("unexpected client catalog models: %#v", models)
	}
}

func TestCockpitSelectorRestrictsAuthsToClientAPIKeyAccountScope(t *testing.T) {
	highQuotaAccount := &accountSpec{
		ID:       "account-high",
		AuthID:   "account-high.json",
		PlanRank: intPtrForTest(500),
	}
	scopedAccount := &accountSpec{
		ID:       "account-scoped",
		AuthID:   "account-scoped.json",
		PlanRank: intPtrForTest(300),
	}
	selector := &cockpitSelector{
		manifest: &manifest{
			RoutingStrategy: "auto",
			accountByAuthID: map[string]*accountSpec{
				"account-high.json":   highQuotaAccount,
				"account-scoped.json": scopedAccount,
			},
			accountByID: map[string]*accountSpec{
				"account-high":   highQuotaAccount,
				"account-scoped": scopedAccount,
			},
		},
	}
	apiKey := &apiKeySpec{
		ID:         "key-scoped",
		Label:      "Scoped client",
		AccountIDs: []string{"account-scoped"},
	}
	ctx := context.WithValue(context.Background(), clientAPIKeyContextKey, apiKey)
	auths := []*coreauth.Auth{
		{ID: "account-high.json", Provider: "codex", Status: coreauth.StatusActive},
		{ID: "account-scoped.json", Provider: "codex", Status: coreauth.StatusActive},
	}

	selected, err := selector.Pick(ctx, "codex", "gpt-5.6-sol", cliproxyexecutor.Options{}, auths)

	if err != nil {
		t.Fatalf("pick scoped auth: %v", err)
	}
	if selected.ID != "account-scoped.json" {
		t.Fatalf("expected only scoped account to be selected, got %q", selected.ID)
	}
}

func TestAPIKeyPriorityStateOrdersFallbackAccountsWithoutRestart(t *testing.T) {
	tempDir := t.TempDir()
	priorityPath := filepath.Join(tempDir, "api-key-priorities.json")
	if err := os.WriteFile(priorityPath, []byte(`{"priorityAccountIds":{"key-team":["account-a","account-b"]}}`), 0o600); err != nil {
		t.Fatalf("write priority state: %v", err)
	}
	store := newAPIKeyPriorityStateStore(filepath.Join(tempDir, "manifest.json"))

	accountA := &accountSpec{ID: "account-a"}
	accountB := &accountSpec{ID: "account-b"}
	accountC := &accountSpec{ID: "account-c"}
	selector := &cockpitSelector{
		manifest: &manifest{
			accountByAuthID: map[string]*accountSpec{
				"auth-a": accountA,
				"auth-b": accountB,
				"auth-c": accountC,
			},
		},
		priorities: store,
	}
	ctx := context.WithValue(context.Background(), clientAPIKeyContextKey, &apiKeySpec{ID: "key-team"})
	auths := []*coreauth.Auth{{ID: "auth-c"}, {ID: "auth-b"}, {ID: "auth-a"}}
	ordered := selector.prioritizeAuthsForAPIKey(ctx, auths)
	if ordered[0].ID != "auth-a" || ordered[1].ID != "auth-b" || ordered[2].ID != "auth-c" {
		t.Fatalf("priority accounts should lead in order, got %#v", ordered)
	}
	fallbackAuths := []*coreauth.Auth{{ID: "auth-c"}, {ID: "auth-b"}}
	ordered = selector.prioritizeAuthsForAPIKey(ctx, fallbackAuths)
	if ordered[0].ID != "auth-b" {
		t.Fatalf("next priority account should lead when the first is unavailable, got %#v", ordered)
	}

	if err := os.WriteFile(priorityPath, []byte(`{"priorityAccountIds":{"key-team":["account-b","account-a"]}}`), 0o600); err != nil {
		t.Fatalf("update priority state: %v", err)
	}
	updatedAt := time.Now().Add(time.Second)
	if err := os.Chtimes(priorityPath, updatedAt, updatedAt); err != nil {
		t.Fatalf("advance priority state timestamp: %v", err)
	}
	ordered = selector.prioritizeAuthsForAPIKey(ctx, auths)
	if ordered[0].ID != "auth-b" || ordered[1].ID != "auth-a" {
		t.Fatalf("updated priority should apply without a sidecar restart, got %#v", ordered)
	}
}

func TestCockpitSessionAffinitySeparatesClientAPIKeyScopes(t *testing.T) {
	highQuotaAccount := &accountSpec{
		ID:       "account-high",
		AuthID:   "account-high.json",
		PlanRank: intPtrForTest(500),
	}
	scopedAccount := &accountSpec{
		ID:       "account-scoped",
		AuthID:   "account-scoped.json",
		PlanRank: intPtrForTest(300),
	}
	fallback := &cockpitSelector{
		manifest: &manifest{
			RoutingStrategy: "auto",
			accountByAuthID: map[string]*accountSpec{
				"account-high.json":   highQuotaAccount,
				"account-scoped.json": scopedAccount,
			},
			accountByID: map[string]*accountSpec{
				"account-high":   highQuotaAccount,
				"account-scoped": scopedAccount,
			},
		},
	}
	selector := &cockpitSessionAffinitySelector{
		inner: coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
			Fallback: fallback,
			TTL:      time.Hour,
		}),
	}
	auths := []*coreauth.Auth{
		{ID: "account-high.json", Provider: "codex", Status: coreauth.StatusActive},
		{ID: "account-scoped.json", Provider: "codex", Status: coreauth.StatusActive},
	}
	opts := cliproxyexecutor.Options{
		Headers: http.Header{"X-Session-ID": []string{"shared-session"}},
	}
	defaultKey := &apiKeySpec{
		ID:         "default-key",
		AccountIDs: []string{"account-high", "account-scoped"},
	}
	scopedKey := &apiKeySpec{
		ID:         "scoped-key",
		AccountIDs: []string{"account-scoped"},
	}

	first, err := selector.Pick(
		context.WithValue(context.Background(), clientAPIKeyContextKey, defaultKey),
		"codex",
		"gpt-5.4",
		opts,
		auths,
	)
	if err != nil {
		t.Fatalf("pick default key auth: %v", err)
	}
	if first.ID != "account-high.json" {
		t.Fatalf("expected default key to select high quota auth, got %q", first.ID)
	}

	second, err := selector.Pick(
		context.WithValue(context.Background(), clientAPIKeyContextKey, scopedKey),
		"codex",
		"gpt-5.4",
		opts,
		auths,
	)
	if err != nil {
		t.Fatalf("pick scoped key auth: %v", err)
	}
	if second.ID != "account-scoped.json" {
		t.Fatalf("expected scoped key not to reuse default key affinity auth, got %q", second.ID)
	}
}

func intPtrForTest(value int) *int {
	return &value
}

func TestCanonicalModelForClientModelHandlesPrefixAliasAndSnapshot(t *testing.T) {
	spec := &apiKeySpec{ModelPrefix: "team"}
	m := &manifest{
		ModelIDs:      []string{"gpt-5.4", "gpt-5.4-mini"},
		aliasToSource: map[string]string{"fast": "gpt-5.4-mini"},
	}

	if got := canonicalModelForClientModel(m, spec, "team/fast"); got != "gpt-5.4-mini" {
		t.Fatalf("alias should resolve to source model, got %q", got)
	}
	if got := canonicalModelForClientModel(m, spec, "team/gpt-5.4-2026-03-05"); got != "gpt-5.4" {
		t.Fatalf("snapshot should resolve to supported model, got %q", got)
	}
	if got := canonicalModelForClientModel(m, spec, codexAutoReviewModel); got != codexAutoReviewModel {
		t.Fatalf("auto review model should stay canonical, got %q", got)
	}
}

func TestLoadManifestIndexesAPIKeyAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{
		"apiKeys": [{"id":"client","label":"Client","key":"client-key","enabled":true}],
		"accounts": [{"id":"api-account","email":"api@example.com","upstreamApiKey":"  sk-upstream  "}]
	}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	m, err := loadManifest(path)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}

	account := m.accountByAPIKey["sk-upstream"]
	if account == nil {
		t.Fatalf("API Key account should be indexed by upstream key: %#v", m.accountByAPIKey)
	}
	if account.ID != "api-account" || account.UpstreamAPIKey != "sk-upstream" {
		t.Fatalf("unexpected indexed account: %#v", account)
	}
}

func TestHydrateManifestPlanTypesFromAuthDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k12.json"), []byte(`{"plan_type":" k12 ","access_token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plus.json"), []byte(`{"plan_type":"plus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &manifest{Accounts: []accountSpec{
		{ID: "k12", AuthID: "k12.json"},
		{ID: "plus", AuthID: "plus.json", PlanType: "Team"},
		{ID: "missing", AuthID: "missing.json"},
	}}

	if got := hydrateManifestPlanTypesFromAuthDir(m, dir); got != 1 {
		t.Fatalf("hydrated count = %d, want 1", got)
	}
	if m.Accounts[0].PlanType != "k12" {
		t.Fatalf("K12 plan type = %q", m.Accounts[0].PlanType)
	}
	if m.Accounts[1].PlanType != "Team" {
		t.Fatalf("existing manifest plan type was overwritten: %q", m.Accounts[1].PlanType)
	}
	if m.Accounts[2].PlanType != "" {
		t.Fatalf("missing auth file produced plan type %q", m.Accounts[2].PlanType)
	}
}

func TestLoadManifestIndexesTokenAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{
		"accounts": [{
			"id":"token-account",
			"email":" token@example.com ",
			"authId":"nested/token-account.json",
			"authKind":"access_token",
			"accessTokenOnly":true,
			"chatgptAccountId":" acct-token "
		}]
	}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	m, err := loadManifest(path)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}

	if got := m.accountByAuthID["nested/token-account.json"]; got == nil || got.ID != "token-account" {
		t.Fatalf("auth id should index token account, got %#v", got)
	}
	if got := m.accountByAuthID["token-account.json"]; got == nil || got.ID != "token-account" {
		t.Fatalf("auth file basename should index token account, got %#v", got)
	}
	if got := m.accountByChatGPT["acct-token"]; got == nil || got.ID != "token-account" {
		t.Fatalf("chatgpt account id should index token account, got %#v", got)
	}
	if got := m.accountByEmail["token@example.com"]; got == nil || got.ID != "token-account" {
		t.Fatalf("email should index token account, got %#v", got)
	}
}

func TestLoadManifestParsesBoundOAuthQuotaReserve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{
		"accounts": [{
			"id": "oauth-account",
			"email": "oauth@example.com",
			"authId": "oauth-account.json",
			"quotaReserve": {
				"hourlyThresholdPercent": 10,
				"weeklyThresholdPercent": 20,
				"snapshotUpdatedAtUnixSeconds": 1234567890,
				"hourlyRemainingPercent": 55,
				"weeklyRemainingPercent": 66,
				"hourlyWindowPresent": true,
				"weeklyWindowPresent": false
			}
		}]
	}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	m, err := loadManifest(path)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	account := m.accountByID["oauth-account"]
	if account == nil || account.QuotaReserve == nil {
		t.Fatalf("quota reserve should be parsed: %#v", account)
	}
	reserve := account.QuotaReserve
	if reserve.HourlyThresholdPercent == nil || *reserve.HourlyThresholdPercent != 10 ||
		reserve.WeeklyThresholdPercent == nil || *reserve.WeeklyThresholdPercent != 20 ||
		reserve.SnapshotUpdatedAtUnixSeconds == nil || *reserve.SnapshotUpdatedAtUnixSeconds != 1234567890 ||
		reserve.HourlyRemainingPercent == nil || *reserve.HourlyRemainingPercent != 55 ||
		reserve.WeeklyRemainingPercent == nil || *reserve.WeeklyRemainingPercent != 66 ||
		reserve.HourlyWindowPresent == nil || !*reserve.HourlyWindowPresent ||
		reserve.WeeklyWindowPresent == nil || *reserve.WeeklyWindowPresent {
		t.Fatalf("unexpected parsed quota reserve: %#v", reserve)
	}
}

func TestCockpitSelectorPickSkipsBoundOAuthAtEitherQuotaReserve(t *testing.T) {
	tests := []struct {
		name            string
		hourlyRemaining int
		weeklyRemaining int
	}{
		{name: "hourly", hourlyRemaining: 10, weeklyRemaining: 90},
		{name: "weekly", hourlyRemaining: 90, weeklyRemaining: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hourlyThreshold := 10
			weeklyThreshold := 20
			snapshotUpdatedAt := time.Now().Unix()
			windowPresent := true
			protectedAccount := &accountSpec{
				ID:     "protected",
				Email:  "protected@example.com",
				AuthID: "protected.json",
				QuotaReserve: &quotaReserveSpec{
					HourlyThresholdPercent:       &hourlyThreshold,
					WeeklyThresholdPercent:       &weeklyThreshold,
					SnapshotUpdatedAtUnixSeconds: &snapshotUpdatedAt,
					HourlyRemainingPercent:       &tt.hourlyRemaining,
					WeeklyRemainingPercent:       &tt.weeklyRemaining,
					HourlyWindowPresent:          &windowPresent,
					WeeklyWindowPresent:          &windowPresent,
				},
			}
			normalAccount := &accountSpec{ID: "normal", AuthID: "normal.json"}
			selector := &cockpitSelector{manifest: &manifest{
				accountByAuthID: map[string]*accountSpec{
					"protected.json": protectedAccount,
					"normal.json":    normalAccount,
				},
			}}

			selected, err := selector.Pick(
				context.Background(),
				"codex",
				"gpt-5.4",
				cliproxyexecutor.Options{},
				[]*coreauth.Auth{{ID: "protected.json"}, {ID: "normal.json"}},
			)
			if err != nil {
				t.Fatalf("Pick: %v", err)
			}
			if selected == nil || selected.ID != "normal.json" {
				t.Fatalf("expected normal auth after reserve filtering, got %#v", selected)
			}
		})
	}
}

func TestCockpitSelectorPrefersAccountWithFewerImageJobs(t *testing.T) {
	busyAccount := &accountSpec{ID: "busy", AuthID: "busy.json"}
	idleAccount := &accountSpec{ID: "idle", AuthID: "idle.json"}
	tracker := newRequestUsageTracker()
	if !tracker.tryReserveImageJob("existing-image", "busy.json", 1) {
		t.Fatal("expected initial busy image reservation")
	}
	if tracker.tryReserveImageJob("competing-image", "busy.json", 1) {
		t.Fatal("expected busy image auth to reject a second concurrent reservation")
	}
	selector := &cockpitSelector{
		manifest: &manifest{accountByAuthID: map[string]*accountSpec{
			"busy.json": busyAccount,
			"idle.json": idleAccount,
		}},
		tracker: tracker,
	}
	ctx := internallogging.WithRequestID(context.Background(), "new-image")
	ctx = context.WithValue(ctx, requestKindContextKey, "image_generation")

	selected, err := selector.Pick(
		ctx,
		"codex",
		"gpt-5.4-mini",
		cliproxyexecutor.Options{},
		[]*coreauth.Auth{{ID: "busy.json"}, {ID: "idle.json"}},
	)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if selected == nil || selected.ID != "idle.json" {
		t.Fatalf("expected idle auth, got %#v", selected)
	}
	if got := tracker.imageInFlightCount("idle.json"); got != 1 {
		t.Fatalf("idle auth in-flight count = %d, want 1", got)
	}

	changed := tracker.imageJobChangeSignal()
	tracker.releaseImageJobs("new-image")
	select {
	case <-changed:
	default:
		t.Fatal("expected image slot release notification")
	}
	if got := tracker.imageInFlightCount("idle.json"); got != 0 {
		t.Fatalf("idle auth in-flight count after release = %d, want 0", got)
	}
}

func TestRequestUsageTrackerHonorsConfiguredImageJobLimit(t *testing.T) {
	tracker := newRequestUsageTracker()
	if !tracker.tryReserveImageJob("first-image", "shared.json", 2) {
		t.Fatal("expected first image reservation")
	}
	if !tracker.tryReserveImageJob("second-image", "shared.json", 2) {
		t.Fatal("expected second image reservation within configured limit")
	}
	if tracker.tryReserveImageJob("third-image", "shared.json", 2) {
		t.Fatal("expected image reservation above configured limit to be rejected")
	}
	if got := tracker.imageInFlightCount("shared.json"); got != 2 {
		t.Fatalf("shared auth in-flight count = %d, want 2", got)
	}
}

func TestImageRequestSelectorBypassesSessionAffinityFallback(t *testing.T) {
	imageAuth := &coreauth.Auth{ID: "image.json"}
	affinityAuth := &coreauth.Auth{ID: "affinity.json"}
	imageFallback := &countingSelector{auth: imageAuth}
	affinityFallback := &countingSelector{auth: affinityAuth}
	selector := &imageRequestSelector{
		imageFallback: imageFallback,
		fallback:      affinityFallback,
	}
	ctx := context.WithValue(context.Background(), requestKindContextKey, "image_generation")

	selected, err := selector.Pick(ctx, "codex", "gpt-5.4-mini", cliproxyexecutor.Options{}, []*coreauth.Auth{imageAuth, affinityAuth})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if selected != imageAuth || imageFallback.count != 1 || affinityFallback.count != 0 {
		t.Fatalf("image request should use image fallback, selected=%#v image=%d affinity=%d", selected, imageFallback.count, affinityFallback.count)
	}
}

func TestCockpitSelectorPickIgnoresExplicitlyMissingQuotaWindow(t *testing.T) {
	hourlyThreshold := 10
	weeklyThreshold := 20
	snapshotUpdatedAt := time.Now().Unix()
	weeklyRemaining := 80
	hourlyWindowPresent := false
	weeklyWindowPresent := true
	account := &accountSpec{
		ID:     "protected",
		AuthID: "protected.json",
		QuotaReserve: &quotaReserveSpec{
			HourlyThresholdPercent:       &hourlyThreshold,
			WeeklyThresholdPercent:       &weeklyThreshold,
			SnapshotUpdatedAtUnixSeconds: &snapshotUpdatedAt,
			HourlyRemainingPercent:       nil,
			WeeklyRemainingPercent:       &weeklyRemaining,
			HourlyWindowPresent:          &hourlyWindowPresent,
			WeeklyWindowPresent:          &weeklyWindowPresent,
		},
	}
	selector := &cockpitSelector{manifest: &manifest{
		accountByAuthID: map[string]*accountSpec{"protected.json": account},
	}}

	selected, err := selector.Pick(
		context.Background(),
		"codex",
		"gpt-5.4",
		cliproxyexecutor.Options{},
		[]*coreauth.Auth{{ID: "protected.json"}},
	)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if selected == nil || selected.ID != "protected.json" {
		t.Fatalf("expected auth with explicitly absent hourly window, got %#v", selected)
	}
}

func TestCockpitSelectorNullableWeeklyReserveUsesOnlyFiveHourBoundary(t *testing.T) {
	hourlyThreshold := 10
	snapshotUpdatedAt := time.Now().Unix()
	weeklyRemaining := 0
	windowPresent := true
	account := &accountSpec{
		ID:     "protected",
		AuthID: "protected.json",
		QuotaReserve: &quotaReserveSpec{
			HourlyThresholdPercent:       &hourlyThreshold,
			WeeklyThresholdPercent:       nil,
			SnapshotUpdatedAtUnixSeconds: &snapshotUpdatedAt,
			WeeklyRemainingPercent:       &weeklyRemaining,
			HourlyWindowPresent:          &windowPresent,
			WeeklyWindowPresent:          &windowPresent,
		},
	}
	selector := &cockpitSelector{manifest: &manifest{
		accountByAuthID: map[string]*accountSpec{"protected.json": account},
	}}
	auths := []*coreauth.Auth{{ID: "protected.json"}}

	hourlyRemaining := 11
	account.QuotaReserve.HourlyRemainingPercent = &hourlyRemaining
	selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, auths)
	if err != nil || selected == nil || selected.ID != "protected.json" {
		t.Fatalf("11%% hourly with unlimited weekly should be available: auth=%#v err=%v", selected, err)
	}

	hourlyRemaining = 10
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5.4", cliproxyexecutor.Options{}, auths)
	if selected != nil || err == nil || !strings.Contains(err.Error(), "5h remaining 10% <= reserve 10%") {
		t.Fatalf("10%% hourly should be blocked at the boundary: auth=%#v err=%v", selected, err)
	}
}

func TestCockpitSelectorPickFailsClosedForUnknownBoundOAuthQuota(t *testing.T) {
	hourlyThreshold := 10
	weeklyThreshold := 20
	snapshotUpdatedAt := time.Now().Unix()
	weeklyWindowPresent := false
	account := &accountSpec{
		ID:     "protected",
		Email:  "protected@example.com",
		AuthID: "protected.json",
		QuotaReserve: &quotaReserveSpec{
			HourlyThresholdPercent:       &hourlyThreshold,
			WeeklyThresholdPercent:       &weeklyThreshold,
			SnapshotUpdatedAtUnixSeconds: &snapshotUpdatedAt,
			HourlyRemainingPercent:       nil,
			WeeklyRemainingPercent:       nil,
			HourlyWindowPresent:          nil,
			WeeklyWindowPresent:          &weeklyWindowPresent,
		},
	}
	selector := &cockpitSelector{manifest: &manifest{
		accountByAuthID: map[string]*accountSpec{"protected.json": account},
	}}

	selected, err := selector.Pick(
		context.Background(),
		"codex",
		"gpt-5.4",
		cliproxyexecutor.Options{},
		[]*coreauth.Auth{{ID: "protected.json"}},
	)
	if selected != nil {
		t.Fatalf("expected no selected auth, got %#v", selected)
	}
	if err == nil {
		t.Fatal("expected quota reserve error")
	}
	message := err.Error()
	for _, fragment := range []string{
		"no auth available",
		"bound OAuth quota reserve blocked 1 auth(s)",
		"protected@example.com",
		"5h remaining quota unknown",
	} {
		if !strings.Contains(message, fragment) {
			t.Fatalf("expected %q in error %q", fragment, message)
		}
	}
}

func TestCockpitSelectorPickFailsClosedForInvalidQuotaSnapshotTimestamp(t *testing.T) {
	now := time.Now().Unix()
	tests := []struct {
		name      string
		timestamp *int64
		reason    string
	}{
		{name: "missing", timestamp: nil, reason: "quota snapshot timestamp unknown"},
		{name: "non-positive", timestamp: int64PointerForTest(0), reason: "quota snapshot timestamp invalid"},
		{name: "future", timestamp: int64PointerForTest(now + 60), reason: "quota snapshot timestamp invalid"},
		{name: "stale", timestamp: int64PointerForTest(now - int64(quotaReserveMaxSnapshotAge/time.Second) - 1), reason: "quota snapshot stale"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hourlyThreshold := 10
			weeklyThreshold := 20
			hourlyRemaining := 80
			weeklyRemaining := 80
			windowPresent := true
			account := &accountSpec{
				ID:     "protected",
				Email:  "protected@example.com",
				AuthID: "protected.json",
				QuotaReserve: &quotaReserveSpec{
					HourlyThresholdPercent:       &hourlyThreshold,
					WeeklyThresholdPercent:       &weeklyThreshold,
					SnapshotUpdatedAtUnixSeconds: tt.timestamp,
					HourlyRemainingPercent:       &hourlyRemaining,
					WeeklyRemainingPercent:       &weeklyRemaining,
					HourlyWindowPresent:          &windowPresent,
					WeeklyWindowPresent:          &windowPresent,
				},
			}
			selector := &cockpitSelector{manifest: &manifest{
				accountByAuthID: map[string]*accountSpec{"protected.json": account},
			}}

			selected, err := selector.Pick(
				context.Background(),
				"codex",
				"gpt-5.4",
				cliproxyexecutor.Options{},
				[]*coreauth.Auth{{ID: "protected.json"}},
			)
			if selected != nil {
				t.Fatalf("expected no selected auth, got %#v", selected)
			}
			if err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("expected %q in quota reserve error, got %v", tt.reason, err)
			}
		})
	}
}

func TestQuotaReserveStateStoreHotReloadsSnapshot(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "quota-reserve.json")
	hourlyThreshold := 20
	weeklyThreshold := 10
	account := &accountSpec{
		ID:    "protected",
		Email: "protected@example.com",
		QuotaReserve: &quotaReserveSpec{
			HourlyThresholdPercent: &hourlyThreshold,
			WeeklyThresholdPercent: &weeklyThreshold,
		},
	}
	writeState := func(hourly, weekly int) {
		t.Helper()
		content, err := json.Marshal(quotaReserveStateFile{Version: quotaReserveStateVersion, Accounts: map[string]quotaReserveSnapshot{
			"protected": {
				SnapshotUpdatedAtUnixSeconds: int64PointerForTest(time.Now().Unix()),
				HourlyRemainingPercent:       intPointerForTest(hourly),
				WeeklyRemainingPercent:       intPointerForTest(weekly),
				HourlyWindowPresent:          boolPointerForTest(true),
				WeeklyWindowPresent:          boolPointerForTest(true),
			},
		}})
		if err != nil {
			t.Fatalf("marshal quota reserve state: %v", err)
		}
		if err := os.WriteFile(statePath, content, 0o600); err != nil {
			t.Fatalf("write quota reserve state: %v", err)
		}
	}

	writeState(80, 80)
	store := newQuotaReserveStateStore(statePath, nil)
	if err := store.load(); err != nil {
		t.Fatalf("load available state: %v", err)
	}
	if reason := quotaReserveBlockReasonWithState(account, store, time.Now()); reason != "" {
		t.Fatalf("expected available snapshot, got %q", reason)
	}

	writeState(20, 80)
	if err := store.load(); err != nil {
		t.Fatalf("load blocked state: %v", err)
	}
	if reason := quotaReserveBlockReasonWithState(account, store, time.Now()); !strings.Contains(reason, "5h remaining 20% <= reserve 20%") {
		t.Fatalf("expected hot-reloaded reserve block, got %q", reason)
	}
}

func TestQuotaRoutingBlocksFreshDepletedNonK12Only(t *testing.T) {
	t.Parallel()
	statePath := filepath.Join(t.TempDir(), "quota-reserve.json")
	now := time.Now()
	content, err := json.Marshal(quotaReserveStateFile{
		Version: quotaReserveStateVersion,
		Accounts: map[string]quotaReserveSnapshot{
			"plus-depleted": {
				SnapshotUpdatedAtUnixSeconds: int64PointerForTest(now.Unix()),
				HourlyRemainingPercent:       intPointerForTest(0),
				WeeklyRemainingPercent:       intPointerForTest(80),
				HourlyWindowPresent:          boolPointerForTest(true),
				WeeklyWindowPresent:          boolPointerForTest(true),
			},
			"plus-weekly-only": {
				SnapshotUpdatedAtUnixSeconds: int64PointerForTest(now.Unix()),
				HourlyRemainingPercent:       intPointerForTest(0),
				WeeklyRemainingPercent:       intPointerForTest(80),
				HourlyWindowPresent:          boolPointerForTest(false),
				WeeklyWindowPresent:          boolPointerForTest(true),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	store := newQuotaReserveStateStore(statePath, nil)
	if err := store.load(); err != nil {
		t.Fatal(err)
	}

	depleted := &accountSpec{ID: "plus-depleted", Email: "depleted@example.com", PlanType: "plus"}
	if reason := quotaRoutingBlockReasonWithState(depleted, store, now); !strings.Contains(reason, "5h quota depleted") {
		t.Fatalf("fresh depleted Plus was not blocked: %q", reason)
	}
	weeklyOnly := &accountSpec{ID: "plus-weekly-only", Email: "weekly@example.com", PlanType: "plus"}
	if reason := quotaRoutingBlockReasonWithState(weeklyOnly, store, now); reason != "" {
		t.Fatalf("Plus without a 5h window was blocked: %q", reason)
	}
	k12 := &accountSpec{ID: "plus-depleted", Email: "k12@example.com", PlanType: "K12"}
	if reason := quotaRoutingBlockReasonWithState(k12, store, now); reason != "" {
		t.Fatalf("K12 quota was blocked outside session policy: %q", reason)
	}
	if reason := quotaRoutingBlockReasonWithState(depleted, store, now.Add(quotaReserveMaxSnapshotAge+time.Second)); reason != "" {
		t.Fatalf("stale depleted Plus should be verified upstream: %q", reason)
	}
}

func TestQuotaReserveStateStoreRejectsUnknownVersion(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "quota-reserve.json")
	content, err := json.Marshal(quotaReserveStateFile{
		Version:  quotaReserveStateVersion + 1,
		Accounts: map[string]quotaReserveSnapshot{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	store := newQuotaReserveStateStore(statePath, nil)
	if err := store.load(); err == nil || !strings.Contains(err.Error(), "unsupported quota reserve state version") {
		t.Fatalf("unknown state version was accepted: %v", err)
	}
}

func TestQuotaReserveSelectorFiltersCachedSessionAffinityAuth(t *testing.T) {
	tests := []struct {
		name          string
		includeNormal bool
		mutateReserve func(*quotaReserveSpec)
		wantAuthID    string
		wantError     string
	}{
		{
			name:          "blocked reselects normal",
			includeNormal: true,
			mutateReserve: func(reserve *quotaReserveSpec) {
				*reserve.HourlyRemainingPercent = *reserve.HourlyThresholdPercent
			},
			wantAuthID: "normal.json",
		},
		{
			name:          "stale reselects normal",
			includeNormal: true,
			mutateReserve: func(reserve *quotaReserveSpec) {
				*reserve.SnapshotUpdatedAtUnixSeconds = time.Now().Add(-quotaReserveMaxSnapshotAge - time.Second).Unix()
			},
			wantAuthID: "normal.json",
		},
		{
			name: "blocked without fallback returns quota error",
			mutateReserve: func(reserve *quotaReserveSpec) {
				*reserve.WeeklyRemainingPercent = *reserve.WeeklyThresholdPercent
			},
			wantError: "bound OAuth quota reserve blocked 1 auth(s)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hourlyThreshold := 10
			weeklyThreshold := 20
			snapshotUpdatedAt := time.Now().Unix()
			hourlyRemaining := 80
			weeklyRemaining := 80
			windowPresent := true
			protectedPlanRank := 2
			normalPlanRank := 1
			reserve := &quotaReserveSpec{
				HourlyThresholdPercent:       &hourlyThreshold,
				WeeklyThresholdPercent:       &weeklyThreshold,
				SnapshotUpdatedAtUnixSeconds: &snapshotUpdatedAt,
				HourlyRemainingPercent:       &hourlyRemaining,
				WeeklyRemainingPercent:       &weeklyRemaining,
				HourlyWindowPresent:          &windowPresent,
				WeeklyWindowPresent:          &windowPresent,
			}
			protectedAccount := &accountSpec{
				ID:           "protected",
				Email:        "protected@example.com",
				AuthID:       "protected.json",
				PlanRank:     &protectedPlanRank,
				QuotaReserve: reserve,
			}
			normalAccount := &accountSpec{
				ID:       "normal",
				Email:    "normal@example.com",
				AuthID:   "normal.json",
				PlanRank: &normalPlanRank,
			}
			m := &manifest{
				Accounts:          []accountSpec{*protectedAccount, *normalAccount},
				RoutingStrategy:   "plan_high_first",
				accountByID:       map[string]*accountSpec{"protected": protectedAccount, "normal": normalAccount},
				accountByAuthID:   map[string]*accountSpec{"protected.json": protectedAccount, "normal.json": normalAccount},
				originalIndexByID: map[string]int{"protected": 0, "normal": 1},
			}
			cfg := &config.Config{}
			cfg.Routing.SessionAffinity = true
			cfg.Routing.SessionAffinityTTL = time.Minute.String()
			selector := buildCoreAuthSelector(cfg, &cockpitSelector{manifest: m}, m, nil)
			if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
				defer stoppable.Stop()
			}

			auths := []*coreauth.Auth{{ID: "protected.json"}}
			if tt.includeNormal {
				auths = append(auths, &coreauth.Auth{ID: "normal.json"})
			}
			opts := cliproxyexecutor.Options{
				OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_ac980658-63bd-4fb3-97ba-8da64cb1e344"}}`),
			}

			first, err := selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
			if err != nil {
				t.Fatalf("initial Pick: %v", err)
			}
			if first == nil || first.ID != "protected.json" {
				t.Fatalf("expected protected auth to establish affinity, got %#v", first)
			}
			cached, err := selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
			if err != nil || cached == nil || cached.ID != "protected.json" {
				t.Fatalf("expected protected affinity cache hit, got auth=%#v err=%v", cached, err)
			}

			tt.mutateReserve(reserve)
			selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
			if tt.wantError != "" {
				if selected != nil {
					t.Fatalf("expected no auth after reserve block, got %#v", selected)
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected quota error containing %q, got %v", tt.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Pick after reserve change: %v", err)
			}
			if selected == nil || selected.ID != tt.wantAuthID {
				t.Fatalf("expected %s after cached auth was filtered, got %#v", tt.wantAuthID, selected)
			}
		})
	}
}

func TestBackupAccountSelectorOnlyAffectsNewSessions(t *testing.T) {
	regularAccount := &accountSpec{ID: "regular", AuthID: "regular.json"}
	backupAccount := &accountSpec{ID: "backup", AuthID: "backup.json"}
	m := &manifest{
		Accounts:        []accountSpec{*regularAccount, *backupAccount},
		RoutingStrategy: "custom",
		CustomRoutingRules: []customRoutingRule{
			{AccountID: "regular", Priority: 0, Weight: 1},
			{AccountID: "backup", Priority: 100, Weight: 1, IsBackup: true},
		},
		accountByID: map[string]*accountSpec{
			"regular": regularAccount,
			"backup":  backupAccount,
		},
		accountByAuthID: map[string]*accountSpec{
			"regular.json": regularAccount,
			"backup.json":  backupAccount,
		},
		originalIndexByID: map[string]int{"regular": 0, "backup": 1},
	}
	cfg := &config.Config{}
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionAffinityTTL = time.Minute.String()
	selector := buildCoreAuthSelector(cfg, &cockpitSelector{manifest: m}, m, nil)
	if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
		defer stoppable.Stop()
	}

	regularAuth := &coreauth.Auth{
		ID:             "regular.json",
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(time.Minute),
	}
	backupAuth := &coreauth.Auth{ID: "backup.json"}
	auths := []*coreauth.Auth{regularAuth, backupAuth}
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_43d54db9-d7ba-4b2f-b09a-47f238dc78ac"}}`),
	}

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
	if err != nil || selected == nil || selected.ID != "backup.json" {
		t.Fatalf("expected backup while regular is unavailable, got auth=%#v err=%v", selected, err)
	}

	regularAuth.Unavailable = false
	regularAuth.NextRetryAfter = time.Time{}
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
	if err != nil || selected == nil || selected.ID != "backup.json" {
		t.Fatalf("expected existing session to keep backup affinity, got auth=%#v err=%v", selected, err)
	}

	newSessionOpts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_7d26642b-b430-455b-bf57-28d4d0d785be"}}`),
	}
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5.4", newSessionOpts, auths)
	if err != nil || selected == nil || selected.ID != "regular.json" {
		t.Fatalf("expected new session to use recovered regular auth, got auth=%#v err=%v", selected, err)
	}
}

func TestBackupAPIKeyDoesNotKeepGenericSessionAffinity(t *testing.T) {
	regularAccount := &accountSpec{ID: "regular", AuthID: "regular.json"}
	apiKeyAccount := &accountSpec{
		ID:             "api-key",
		PlanType:       "API_KEY",
		AuthKind:       "api_key",
		UpstreamAPIKey: "upstream-key",
	}
	m := &manifest{
		Accounts:        []accountSpec{*regularAccount, *apiKeyAccount},
		RoutingStrategy: "custom",
		CustomRoutingRules: []customRoutingRule{
			{AccountID: "regular", Priority: 0, Weight: 1},
			{AccountID: "api-key", Priority: 100, Weight: 1, IsBackup: true},
		},
		accountByID: map[string]*accountSpec{
			"regular": regularAccount,
			"api-key": apiKeyAccount,
		},
		accountByAuthID: map[string]*accountSpec{
			"regular.json": regularAccount,
		},
		accountByAPIKey: map[string]*accountSpec{
			"upstream-key": apiKeyAccount,
		},
		originalIndexByID: map[string]int{"regular": 0, "api-key": 1},
	}
	cfg := &config.Config{}
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionAffinityTTL = time.Minute.String()
	selector := buildCoreAuthSelector(cfg, &cockpitSelector{manifest: m}, m, nil)
	if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
		defer stoppable.Stop()
	}

	regularAuth := &coreauth.Auth{
		ID:             "regular.json",
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(time.Minute),
	}
	apiKeyAuth := &coreauth.Auth{
		ID:         "codex:apikey:test",
		Attributes: map[string]string{"api_key": "upstream-key"},
	}
	auths := []*coreauth.Auth{regularAuth, apiKeyAuth}
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"prompt_cache_key":"api-key-fallback-affinity"}`),
	}

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
	if err != nil || selected == nil || selected.ID != apiKeyAuth.ID {
		t.Fatalf("expected API key while regular auth is unavailable, got auth=%#v err=%v", selected, err)
	}

	regularAuth.Unavailable = false
	regularAuth.NextRetryAfter = time.Time{}
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
	if err != nil || selected == nil || selected.ID != regularAuth.ID {
		t.Fatalf("API key fallback kept generic affinity after regular recovery: auth=%#v err=%v", selected, err)
	}
}

func TestBackupAccountSelectorPreservesConfirmedK12Binding(t *testing.T) {
	regularAccount := &accountSpec{ID: "regular-k12", AuthID: "regular-k12.json", PlanType: "K12"}
	backupAccount := &accountSpec{ID: "backup-k12", AuthID: "backup-k12.json", PlanType: "K12"}
	m := &manifest{
		APIKeys:         []apiKeySpec{{ID: "local", Key: "local-api-key", Enabled: true}},
		Accounts:        []accountSpec{*regularAccount, *backupAccount},
		RoutingStrategy: "custom",
		CustomRoutingRules: []customRoutingRule{
			{AccountID: regularAccount.ID, Priority: 0, Weight: 1},
			{AccountID: backupAccount.ID, Priority: 100, Weight: 1, IsBackup: true},
		},
		accountByID: map[string]*accountSpec{
			regularAccount.ID: regularAccount,
			backupAccount.ID:  backupAccount,
		},
		accountByAuthID: map[string]*accountSpec{
			regularAccount.AuthID: regularAccount,
			backupAccount.AuthID:  backupAccount,
		},
		originalIndexByID: map[string]int{regularAccount.ID: 0, backupAccount.ID: 1},
	}
	selector := buildCoreAuthSelector(&config.Config{}, &cockpitSelector{manifest: m}, m, nil)
	if stoppable, ok := selector.(coreauth.StoppableSelector); ok {
		defer stoppable.Stop()
	}

	regularAuth := &coreauth.Auth{
		ID:             regularAccount.AuthID,
		Provider:       "codex",
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(time.Minute),
	}
	backupAuth := &coreauth.Auth{ID: backupAccount.AuthID, Provider: "codex"}
	auths := []*coreauth.Auth{regularAuth, backupAuth}
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"backup-k12-existing","input":[]}`)}

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
	if err != nil || selected == nil || selected.ID != backupAuth.ID {
		t.Fatalf("expected backup K12 while regular is unavailable, got auth=%#v err=%v", selected, err)
	}
	preSelector, ok := selector.(coreauth.PreAvailabilitySelector)
	if !ok {
		t.Fatal("selector should expose K12 pre-availability selection")
	}
	if _, handled, errPre := preSelector.PickBeforeAvailability(context.Background(), "codex", "gpt-5.4", opts, auths); handled || errPre != nil {
		t.Fatalf("tentative K12 selection was treated as confirmed: handled=%t err=%v", handled, errPre)
	}
	observer, ok := selector.(coreauth.SelectionResultSelector)
	if !ok {
		t.Fatal("selector should expose K12 result notifications")
	}
	observer.OnSelectionResult(context.Background(), coreauth.Result{
		AuthID:   backupAuth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  true,
	}, opts)
	confirmed, handled, err := preSelector.PickBeforeAvailability(context.Background(), "codex", "gpt-5.4", opts, auths)
	if err != nil || !handled || confirmed == nil || confirmed.ID != backupAuth.ID {
		t.Fatalf("successful K12 selection was not confirmed: auth=%#v handled=%t err=%v", confirmed, handled, err)
	}

	regularAuth.Unavailable = false
	regularAuth.NextRetryAfter = time.Time{}
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5.4", opts, auths)
	if err != nil || selected == nil || selected.ID != backupAuth.ID {
		t.Fatalf("expected confirmed K12 session to keep backup binding, got auth=%#v err=%v", selected, err)
	}

	newSessionOpts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"prompt_cache_key":"backup-k12-new","input":[]}`)}
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5.4", newSessionOpts, auths)
	if err != nil || selected == nil || selected.ID != regularAuth.ID {
		t.Fatalf("expected new K12 session to use recovered regular account, got auth=%#v err=%v", selected, err)
	}
}

func int64PointerForTest(value int64) *int64 {
	return &value
}

func intPointerForTest(value int) *int {
	return &value
}

func boolPointerForTest(value bool) *bool {
	return &value
}

func TestSidecarRuntimeRegistersConfigCodexAPIKeyAuths(t *testing.T) {
	tempDir := t.TempDir()
	authDir := filepath.Join(tempDir, "auths")
	configPath := filepath.Join(tempDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write config path: %v", err)
	}

	cfg := &config.Config{
		AuthDir: authDir,
		CodexKey: []config.CodexKey{{
			APIKey:  "sk-upstream",
			BaseURL: "http://127.0.0.1:1",
		}},
	}
	account := &accountSpec{ID: "api-account", Email: "api@example.com", UpstreamAPIKey: "sk-upstream"}
	m := &manifest{
		Accounts:        []accountSpec{*account},
		accountByID:     map[string]*accountSpec{"api-account": account},
		accountByAuthID: map[string]*accountSpec{},
		accountByAPIKey: map[string]*accountSpec{"sk-upstream": account},
		ModelIDs:        []string{"gpt-5.4"},
	}
	manager := buildCoreAuthManager(cfg, &cockpitSelector{manifest: m}, &authHook{manifest: m}, m, nil, newRequestUsageTracker(), nil)

	runtime, err := newSidecarRuntime(context.Background(), configPath, cfg, m, manager)
	if err != nil {
		t.Fatalf("newSidecarRuntime: %v", err)
	}
	defer runtime.Stop()

	var codexAPIKeyAuth *coreauth.Auth
	for _, auth := range manager.List() {
		if auth == nil || !strings.EqualFold(auth.Provider, "codex") {
			continue
		}
		if auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) == "sk-upstream" {
			codexAPIKeyAuth = auth
			break
		}
	}
	if codexAPIKeyAuth == nil {
		t.Fatalf("expected codex API Key auth to be registered, got %#v", manager.List())
	}
	if got := m.accountByAuthID[strings.ToLower(codexAPIKeyAuth.ID)]; got == nil || got.ID != "api-account" {
		t.Fatalf("expected auth to be linked to manifest account, got %#v", got)
	}
}

func TestSidecarRuntimeRegistersManifestCodexAccessTokenAuths(t *testing.T) {
	tempDir := t.TempDir()
	authDir := filepath.Join(tempDir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	configPath := filepath.Join(tempDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write config path: %v", err)
	}
	authFile := filepath.Join(authDir, "token-account.json")
	if err := os.WriteFile(authFile, []byte(`{
		"type":"codex",
		"email":"token@example.com",
		"access_token":"session-runtime-token",
		"personal_access_token":"at-runtime-token",
		"at_token":"at-runtime-token",
		"account_id":"acct-token",
		"openai_auth_mode":"personal_access_token",
		"proxy_url":"http://127.0.0.1:9"
	}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	cfg := &config.Config{AuthDir: authDir}
	account := &accountSpec{
		ID:               "token-account",
		Email:            "token@example.com",
		AuthID:           "token-account.json",
		AuthKind:         "access_token",
		AccessTokenOnly:  true,
		ChatGPTAccountID: "acct-token",
	}
	m := &manifest{
		Accounts:         []accountSpec{*account},
		accountByID:      map[string]*accountSpec{"token-account": account},
		accountByAuthID:  map[string]*accountSpec{"token-account.json": account},
		accountByAPIKey:  map[string]*accountSpec{},
		accountByChatGPT: map[string]*accountSpec{"acct-token": account},
		accountByEmail:   map[string]*accountSpec{"token@example.com": account},
		ModelIDs:         []string{"gpt-5.4"},
	}
	manager := buildCoreAuthManager(cfg, &cockpitSelector{manifest: m}, &authHook{manifest: m}, m, nil, newRequestUsageTracker(), nil)

	runtime, err := newSidecarRuntime(context.Background(), configPath, cfg, m, manager)
	if err != nil {
		t.Fatalf("newSidecarRuntime: %v", err)
	}
	defer runtime.Stop()

	var tokenAuth *coreauth.Auth
	for _, auth := range manager.List() {
		if auth == nil || !strings.EqualFold(auth.Provider, "codex") {
			continue
		}
		if auth.Metadata != nil && auth.Metadata["access_token"] == "at-runtime-token" {
			tokenAuth = auth
			break
		}
	}
	if tokenAuth == nil {
		t.Fatalf("expected codex access token auth to be registered, got %#v", manager.List())
	}
	if tokenAuth.ProxyURL != "http://127.0.0.1:9" {
		t.Fatalf("expected proxy url from auth metadata, got %q", tokenAuth.ProxyURL)
	}
	if got := m.accountByAuthID[strings.ToLower(tokenAuth.ID)]; got == nil || got.ID != "token-account" {
		t.Fatalf("expected token auth to be linked to manifest account, got %#v", got)
	}
	if info := findModelInfoForTest(
		registry.GetGlobalRegistry().GetModelsForClient(tokenAuth.ID),
		"gpt-5.4",
	); info == nil {
		t.Fatalf("expected manifest models to be registered for token auth")
	}
}

func TestManifestRegistryModelsPreservesStaticThinkingSupport(t *testing.T) {
	models := manifestRegistryModels(&manifest{
		ModelIDs: []string{"gpt-5.2"},
	})

	info := findModelInfoForTest(models, "gpt-5.2")
	if info == nil {
		t.Fatalf("expected gpt-5.2 in manifest registry models: %#v", models)
	}
	if info.Thinking == nil {
		t.Fatalf("expected gpt-5.2 to preserve static thinking support: %#v", info)
	}
	if !stringSliceContains(info.Thinking.Levels, "high") {
		t.Fatalf("expected gpt-5.2 thinking levels to include high: %#v", info.Thinking.Levels)
	}
	if info.UserDefined {
		t.Fatalf("static model should not be marked user-defined: %#v", info)
	}
}

func TestManifestRegistryModelsCopiesSourceThinkingToAliases(t *testing.T) {
	models := manifestRegistryModels(&manifest{
		ModelAliases: []modelAliasSpec{{
			SourceModel: "gpt-5.2",
			Alias:       "gpt-5.2-codex",
			Fork:        true,
		}},
	})

	alias := findModelInfoForTest(models, "gpt-5.2-codex")
	if alias == nil {
		t.Fatalf("expected alias in manifest registry models: %#v", models)
	}
	if alias.Thinking == nil {
		t.Fatalf("expected alias to inherit source thinking support: %#v", alias)
	}
	if !stringSliceContains(alias.Thinking.Levels, "high") {
		t.Fatalf("expected alias thinking levels to include high: %#v", alias.Thinking.Levels)
	}
	if alias.UserDefined {
		t.Fatalf("alias backed by static source should not be marked user-defined: %#v", alias)
	}
}

func TestManifestRegistryModelsTreatsUnknownModelsAsUserDefined(t *testing.T) {
	models := manifestRegistryModels(&manifest{
		ModelIDs: []string{"custom-codex-model"},
	})

	info := findModelInfoForTest(models, "custom-codex-model")
	if info == nil {
		t.Fatalf("expected custom model in manifest registry models: %#v", models)
	}
	if !info.UserDefined {
		t.Fatalf("unknown manifest model should be user-defined so thinking passes upstream: %#v", info)
	}
	if info.Thinking != nil {
		t.Fatalf("unknown manifest model should not invent thinking support: %#v", info)
	}
}

func TestManifestRegisteredModelsPreserveReasoningEffortThroughThinkingPipeline(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "test-codex-auth",
		Provider: "codex",
		Status:   coreauth.StatusActive,
	}
	manager := buildCoreAuthManager(&config.Config{}, &cockpitSelector{}, nil, nil, nil, nil, nil)
	registered, err := manager.Register(context.Background(), auth)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	auth = registered
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	registerManifestModelsForAuth(manager, &manifest{
		ModelIDs: []string{"gpt-5.2"},
		ModelAliases: []modelAliasSpec{{
			SourceModel: "gpt-5.2",
			Alias:       "gpt-5.2-codex",
		}},
	}, auth)

	for _, model := range []string{"gpt-5.2", "gpt-5.2-codex"} {
		out, err := thinking.ApplyThinking(
			[]byte(`{"model":"`+model+`","reasoning":{"effort":"high"}}`),
			model,
			"openai-response",
			"codex",
			"codex",
		)
		if err != nil {
			t.Fatalf("ApplyThinking(%s): %v", model, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("translated payload for %s should be JSON: %v", model, err)
		}
		reasoning, _ := payload["reasoning"].(map[string]any)
		if reasoning["effort"] != "high" {
			t.Fatalf("reasoning effort should survive manifest registry for %s: %s", model, out)
		}
	}
}

func findModelInfoForTest(models []*cliproxy.ModelInfo, id string) *cliproxy.ModelInfo {
	for _, model := range models {
		if model != nil && strings.EqualFold(model.ID, id) {
			return model
		}
	}
	return nil
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func TestBuiltinTranslatorNormalizesOpenAIResponsesForCodex(t *testing.T) {
	in := []byte(`{"model":"gpt-5.4-mini","input":"pong","stream":false,"temperature":0.1}`)
	out := sdktranslator.TranslateRequest(
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatCodex,
		"gpt-5.4-mini",
		in,
		true,
	)

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("translated payload should be JSON: %v", err)
	}
	if payload["stream"] != true {
		t.Fatalf("stream should be forced true, got %#v", payload["stream"])
	}
	if _, exists := payload["temperature"]; exists {
		t.Fatalf("unsupported temperature leaked into Codex payload: %s", out)
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input should be normalized to a message list, got %#v", payload["input"])
	}
	first, ok := input[0].(map[string]any)
	if !ok || first["type"] != "message" || first["role"] != "user" {
		t.Fatalf("unexpected normalized input item: %#v", input[0])
	}
}

func TestRequestPolicyMiddlewareSetsCPAUsageAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := &manifest{
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "key_1", Label: "Test key", Key: "client-key", Enabled: true},
		},
	}
	policy := &requestPolicy{manifest: m}
	router := gin.New()
	router.Use(policy.middleware())
	router.GET("/v1/responses", func(c *gin.Context) {
		value, exists := c.Get(ginUserAPIKeyKey)
		if !exists {
			t.Fatalf("%s should be set for CPA usage reporter", ginUserAPIKeyKey)
		}
		if value != "client-key" {
			t.Fatalf("unexpected %s: %#v", ginUserAPIKeyKey, value)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", w.Code)
	}
}

type testExecutorStatusError struct {
	status int
}

func (e testExecutorStatusError) Error() string {
	return http.StatusText(e.status)
}

func (e testExecutorStatusError) StatusCode() int {
	return e.status
}

func TestWriteExecutorErrorThrottlesRetryableDownstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	server := &relayServer{
		cfg: &config.Config{
			SDKConfig: config.SDKConfig{
				Streaming: config.StreamingConfig{
					BootstrapRetryBaseDelayMS: 50,
					BootstrapRetryMaxDelayMS:  50,
				},
			},
		},
	}

	started := time.Now()
	server.writeExecutorError(c, testExecutorStatusError{status: http.StatusServiceUnavailable})
	elapsed := time.Since(started)

	if elapsed < 50*time.Millisecond {
		t.Fatalf("expected downstream error delay >= 50ms, got %v", elapsed)
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
}

func TestRequestUsageTrackerFinalizesWithLastSuccessfulAttempt(t *testing.T) {
	tracker := newRequestUsageTracker()
	tracker.start("req-1")
	tracker.recordSelectedAccount("req-1", &accountSpec{
		ID:    "account-ok",
		Email: "ok@example.com",
	}, "auth-ok")
	tracker.record(usagePayload{
		Type:          "usage",
		RequestID:     "req-1",
		AccountID:     "account-failed",
		AccountEmail:  "failed@example.com",
		Model:         "gpt-5.5",
		RequestKind:   "text",
		Success:       false,
		Status:        http.StatusInternalServerError,
		ErrorCategory: "upstream_error",
		ErrorMessage:  "unexpected EOF",
	})
	tracker.record(usagePayload{
		Type:         "usage",
		RequestID:    "req-1",
		AccountID:    "account-ok",
		AccountEmail: "ok@example.com",
		Model:        "gpt-5.5",
		RequestKind:  "text",
		ServiceTier:  "priority",
		Success:      true,
		Status:       http.StatusOK,
		Usage: usageDetails{
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  15,
		},
	})

	payload, ok := tracker.finalize("req-1", usageFinalizeInput{
		spec:          &apiKeySpec{ID: "key_1", Label: "Default"},
		requestKind:   "text",
		model:         "gpt-5.5",
		status:        http.StatusOK,
		latencyMS:     446_000,
		completedAtMS: 123,
	})

	if !ok {
		t.Fatal("expected finalized usage payload")
	}
	if !payload.Success || payload.AccountID != "account-ok" {
		t.Fatalf("expected successful account payload, got %#v", payload)
	}
	if payload.ErrorCategory != "" || payload.ErrorMessage != "" {
		t.Fatalf("successful final request should not keep attempt error: %#v", payload)
	}
	if payload.LatencyMS != 446_000 || payload.APIKeyID != "key_1" {
		t.Fatalf("final request metadata was not applied: %#v", payload)
	}
	if payload.ServiceTier != "priority" {
		t.Fatalf("expected service tier to be preserved, got %#v", payload)
	}
}

func TestRequestUsageTrackerFinalizesWithSelectedAccount(t *testing.T) {
	tracker := newRequestUsageTracker()
	tracker.recordSelectedAccount("req-selected", &accountSpec{
		ID:    "account-selected",
		Email: "selected@example.com",
	}, "auth-selected")

	payload, ok := tracker.finalize("req-selected", usageFinalizeInput{
		spec:          &apiKeySpec{ID: "key_1", Label: "Default"},
		requestKind:   "text",
		model:         "gpt-5.5",
		status:        http.StatusOK,
		latencyMS:     100,
		completedAtMS: 123,
	})

	if !ok {
		t.Fatal("expected finalized usage payload")
	}
	if payload.AccountID != "account-selected" || payload.AccountEmail != "selected@example.com" || payload.AuthID != "auth-selected" {
		t.Fatalf("expected selected account metadata, got %#v", payload)
	}
}

func TestRequestUsageTrackerSelectedAccountOverridesUsageAccount(t *testing.T) {
	tracker := newRequestUsageTracker()
	tracker.start("req-usage")
	tracker.recordSelectedAccount("req-usage", &accountSpec{
		ID:    "account-selected",
		Email: "selected@example.com",
	}, "auth-selected")
	tracker.record(usagePayload{
		Type:         "usage",
		RequestID:    "req-usage",
		AccountID:    "account-usage",
		AccountEmail: "usage@example.com",
		AuthID:       "auth-usage",
		Success:      true,
	})

	payload, ok := tracker.finalize("req-usage", usageFinalizeInput{
		status:        http.StatusOK,
		latencyMS:     100,
		completedAtMS: 123,
	})

	if !ok {
		t.Fatal("expected finalized usage payload")
	}
	if payload.AccountID != "account-selected" || payload.AccountEmail != "selected@example.com" || payload.AuthID != "auth-selected" {
		t.Fatalf("selected account metadata should win, got %#v", payload)
	}
}

func TestRequestUsageTrackerEmitsLateUsageCorrectionAfterFinalize(t *testing.T) {
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	tracker := newRequestUsageTracker()
	tracker.setEmitter(emitter)
	tracker.recordSelectedAccount("req-late", &accountSpec{
		ID:    "account-selected",
		Email: "selected@example.com",
	}, "auth-selected")

	initial, ok := tracker.finalize("req-late", usageFinalizeInput{
		spec:          &apiKeySpec{ID: "key_1", Label: "Default", Key: "never-log-this-secret"},
		requestKind:   "text",
		model:         "gpt-5.5",
		status:        499,
		latencyMS:     30_000,
		completedAtMS: 123,
		errorMessage:  "context canceled",
	})
	if !ok || initial.AccountID != "account-selected" || initial.LogicalRequestID != "req-late" {
		t.Fatalf("unexpected initial finalized usage: %#v", initial)
	}
	tracker.record(usagePayload{
		RequestID:     "req-late",
		Provider:      "codex",
		AccountID:     "account-selected",
		AccountEmail:  "selected@example.com",
		AuthID:        "auth-selected",
		Success:       true,
		RequestedAtMS: 100,
		Usage: usageDetails{
			InputTokens:  100,
			OutputTokens: 25,
			TotalTokens:  125,
		},
	})
	if emitter.flush(20*time.Millisecond) && output.Len() != 0 {
		t.Fatalf("late correction must wait until the initial usage event is queued: %s", output.String())
	}
	tracker.markFinalizedEmitted("req-late")
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing late usage correction")
	}
	if strings.Contains(output.String(), "never-log-this-secret") {
		t.Fatalf("raw API key leaked into usage event: %s", output.String())
	}
	var correction usagePayload
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &correction); err != nil {
		t.Fatalf("decode correction: %v output=%s", err, output.String())
	}
	if !correction.Correction || correction.LogicalRequestID != "req-late" || correction.APIKeyID != "key_1" {
		t.Fatalf("late usage should be an attributed correction: %#v", correction)
	}
	if correction.Usage.TotalTokens != 125 || correction.AccountID != "account-selected" {
		t.Fatalf("late usage tokens or account were lost: %#v", correction)
	}
	if correction.Success || correction.Status != 499 || correction.ErrorCategory != "client_canceled" {
		t.Fatalf("late usage must not erase the logical cancellation result: %#v", correction)
	}

	tracker.mu.Lock()
	tombstone := tracker.finalized["req-late"]
	_, activeRecord := tracker.records["req-late"]
	tracker.mu.Unlock()
	if activeRecord || tombstone.payload.Usage.TotalTokens != 125 {
		t.Fatalf("late usage was not merged into the bounded tombstone: active=%t tombstone=%#v", activeRecord, tombstone)
	}
}

func TestRequestUsageTrackerEmitsLateUsageAfterTombstoneEviction(t *testing.T) {
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	tracker := newRequestUsageTracker()
	tracker.setEmitter(emitter)

	if _, ok := tracker.finalize("req-evicted", usageFinalizeInput{
		status:        http.StatusBadGateway,
		completedAtMS: 123,
		errorMessage:  "upstream response ended early",
	}); !ok {
		t.Fatal("expected initial finalized usage")
	}
	tracker.markFinalizedEmitted("req-evicted")
	tracker.mu.Lock()
	tombstone := tracker.finalized["req-evicted"]
	tombstone.expiresAt = time.Now().Add(-time.Second)
	tracker.finalized["req-evicted"] = tombstone
	tracker.pruneFinalizedLocked(time.Now())
	tracker.mu.Unlock()

	tracker.record(usagePayload{
		RequestID: "req-evicted",
		Success:   true,
		Status:    http.StatusOK,
		Usage:     usageDetails{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing evicted late usage correction")
	}

	var correction usagePayload
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &correction); err != nil {
		t.Fatalf("decode correction: %v\n%s", err, output.String())
	}
	if !correction.Correction || correction.RequestID != "req-evicted" || correction.Usage.TotalTokens != 15 {
		t.Fatalf("late usage after tombstone eviction was not emitted as a correction: %#v", correction)
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if _, exists := tracker.records["req-evicted"]; exists {
		t.Fatal("late usage after tombstone eviction leaked into pending records")
	}
}

func TestRequestUsageTrackerLateFailureCorrectsSyntheticSuccess(t *testing.T) {
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	tracker := newRequestUsageTracker()
	tracker.setEmitter(emitter)
	initial, ok := tracker.finalize("req-late-failure", usageFinalizeInput{
		status:        http.StatusOK,
		completedAtMS: 123,
	})
	if !ok || !initial.Success {
		t.Fatalf("expected initial synthetic success: %#v", initial)
	}
	tracker.markFinalizedEmitted("req-late-failure")
	tracker.record(usagePayload{
		RequestID:     "req-late-failure",
		Success:       false,
		Status:        http.StatusBadGateway,
		ErrorCategory: "upstream_error",
		ErrorMessage:  "unexpected EOF",
	})
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing late failure correction")
	}
	var correction usagePayload
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &correction); err != nil {
		t.Fatalf("decode correction: %v", err)
	}
	if !correction.Correction || correction.Success || correction.Status != http.StatusBadGateway || correction.ErrorCategory != "upstream_error" {
		t.Fatalf("late executor failure did not correct synthetic success: %#v", correction)
	}
}

func TestRequestUsageTrackerBoundsFinalizedTombstones(t *testing.T) {
	tracker := newRequestUsageTracker()
	now := time.Now()
	tracker.mu.Lock()
	for index := 0; index < usageTombstoneLimit+10; index++ {
		requestID := fmt.Sprintf("request-%d", index)
		tracker.rememberFinalizedLocked(requestID, usagePayload{RequestID: requestID}, now, false)
	}
	count := len(tracker.finalized)
	_, oldestStillPresent := tracker.finalized["request-0"]
	_, newestPresent := tracker.finalized[fmt.Sprintf("request-%d", usageTombstoneLimit+9)]
	tracker.mu.Unlock()
	if count != usageTombstoneLimit || oldestStillPresent || !newestPresent {
		t.Fatalf("unexpected bounded tombstone state: count=%d oldest=%t newest=%t", count, oldestStillPresent, newestPresent)
	}

	tracker.record(usagePayload{RequestID: "request-10", Usage: usageDetails{TotalTokens: 1}})
	tracker.mu.Lock()
	tracker.rememberFinalizedLocked("request-extra", usagePayload{RequestID: "request-extra"}, now, false)
	_, touchedPresent := tracker.finalized["request-10"]
	_, nextOldestPresent := tracker.finalized["request-11"]
	tracker.mu.Unlock()
	if !touchedPresent || nextOldestPresent {
		t.Fatalf("renewed tombstone should move behind older entries: touched=%t next_oldest=%t", touchedPresent, nextOldestPresent)
	}
}

func TestRecordingSelectorRecordsProvisionalAccountBeforeFailure(t *testing.T) {
	account := &accountSpec{ID: "account-selected", Email: "selected@example.com"}
	auth := &coreauth.Auth{ID: "auth-selected", Provider: "codex", Status: coreauth.StatusActive}
	tracker := newRequestUsageTracker()
	selector := &recordingSelector{
		inner:   &countingSelector{auth: auth},
		tracker: tracker,
		manifest: &manifest{
			accountByAuthID: map[string]*accountSpec{"auth-selected": account},
		},
	}
	ctx := internallogging.WithRequestID(context.Background(), "req-provisional")
	if _, err := selector.Pick(ctx, "codex", "gpt-5.5", cliproxyexecutor.Options{}, []*coreauth.Auth{auth}); err != nil {
		t.Fatalf("pick: %v", err)
	}
	payload, ok := tracker.finalize("req-provisional", usageFinalizeInput{
		status:        http.StatusBadGateway,
		completedAtMS: 123,
		errorMessage:  "unexpected EOF",
	})
	if !ok || payload.AccountID != account.ID || payload.AccountEmail != account.Email || payload.AuthID != auth.ID {
		t.Fatalf("failed request lost provisional account ownership: %#v", payload)
	}
}

type countingSelector struct {
	auth  *coreauth.Auth
	count int
}

func (s *countingSelector) Pick(context.Context, string, string, cliproxyexecutor.Options, []*coreauth.Auth) (*coreauth.Auth, error) {
	s.count++
	return s.auth, nil
}

type selectorLifecycleProbe struct {
	auth            *coreauth.Auth
	result          coreauth.Result
	resultCalls     int
	syncedAuths     int
	stopped         bool
	pickNamespace   string
	preNamespace    string
	resultNamespace string
}

func sessionAffinityNamespaceForTest(opts cliproxyexecutor.Options) string {
	value, _ := opts.Metadata[cliproxyexecutor.SessionAffinityNamespaceMetadataKey].(string)
	return value
}

func (s *selectorLifecycleProbe) Pick(_ context.Context, _ string, _ string, opts cliproxyexecutor.Options, _ []*coreauth.Auth) (*coreauth.Auth, error) {
	s.pickNamespace = sessionAffinityNamespaceForTest(opts)
	return s.auth, nil
}

func (s *selectorLifecycleProbe) PickBeforeAvailability(_ context.Context, _ string, _ string, opts cliproxyexecutor.Options, _ []*coreauth.Auth) (*coreauth.Auth, bool, error) {
	s.preNamespace = sessionAffinityNamespaceForTest(opts)
	return s.auth, true, nil
}

func (s *selectorLifecycleProbe) OnSelectionResult(_ context.Context, result coreauth.Result, opts cliproxyexecutor.Options) coreauth.SelectionResultDirective {
	s.result = result
	s.resultCalls++
	s.resultNamespace = sessionAffinityNamespaceForTest(opts)
	return coreauth.SelectionResultDirective{SuppressAvailabilityUpdate: true, StopAuthAttempt: true}
}

func (s *selectorLifecycleProbe) SyncAuths(auths []*coreauth.Auth) {
	s.syncedAuths = len(auths)
}

func (s *selectorLifecycleProbe) Stop() {
	s.stopped = true
}

func (s *selectorLifecycleProbe) NeedsCrossPriorityCandidates() bool {
	return true
}

func TestSelectorWrappersForwardExtendedLifecycle(t *testing.T) {
	auth := &coreauth.Auth{ID: "auth-1", Provider: "codex"}
	probe := &selectorLifecycleProbe{auth: auth}
	selector := coreauth.Selector(&recordingSelector{
		inner: &quotaReserveSelector{
			fallback: &cockpitSessionAffinitySelector{
				inner: &backupAccountSelector{fallback: probe},
			},
		},
	})
	ctx := context.WithValue(context.Background(), clientAPIKeyContextKey, &apiKeySpec{ID: "client-key-1"})
	if selected, err := selector.Pick(ctx, "codex", "gpt-5.5", cliproxyexecutor.Options{}, []*coreauth.Auth{auth}); err != nil || selected != auth {
		t.Fatalf("pick forwarding = auth=%#v err=%v", selected, err)
	}

	preSelector, ok := selector.(coreauth.PreAvailabilitySelector)
	if !ok {
		t.Fatal("recording selector should expose pre-availability selection")
	}
	selected, handled, err := preSelector.PickBeforeAvailability(ctx, "codex", "gpt-5.5", cliproxyexecutor.Options{}, []*coreauth.Auth{auth})
	if err != nil || !handled || selected != auth {
		t.Fatalf("pre-availability forwarding = auth=%#v handled=%t err=%v", selected, handled, err)
	}

	result := coreauth.Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5.5", Success: true}
	observer, ok := selector.(coreauth.SelectionResultSelector)
	if !ok {
		t.Fatal("recording selector should expose result notifications")
	}
	directive := observer.OnSelectionResult(ctx, result, cliproxyexecutor.Options{})
	if probe.resultCalls != 1 || probe.result.AuthID != auth.ID || !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt {
		t.Fatalf("result forwarding failed: probe=%#v directive=%#v", probe, directive)
	}
	if probe.pickNamespace != "client-key-1" || probe.preNamespace != "client-key-1" || probe.resultNamespace != "client-key-1" {
		t.Fatalf("session namespace forwarding failed: probe=%#v", probe)
	}
	crossPriority, ok := selector.(coreauth.CrossPrioritySelector)
	if !ok || !crossPriority.NeedsCrossPriorityCandidates() {
		t.Fatal("cross-priority candidate capability was not forwarded through selector wrappers")
	}

	snapshot, ok := selector.(coreauth.AuthSnapshotSelector)
	if !ok {
		t.Fatal("recording selector should expose auth snapshots")
	}
	snapshot.SyncAuths([]*coreauth.Auth{auth})
	if probe.syncedAuths != 1 {
		t.Fatalf("auth snapshot forwarding count = %d", probe.syncedAuths)
	}

	stoppable, ok := selector.(coreauth.StoppableSelector)
	if !ok {
		t.Fatal("recording selector should be stoppable")
	}
	stoppable.Stop()
	if !probe.stopped {
		t.Fatal("stop was not forwarded through selector wrappers")
	}
}

func TestRecordingSelectorRecordsSuccessfulSessionAffinityCacheHit(t *testing.T) {
	account := &accountSpec{ID: "account-selected", Email: "selected@example.com"}
	otherAccount := &accountSpec{ID: "account-other", Email: "other@example.com"}
	m := &manifest{
		accountByAuthID: map[string]*accountSpec{"auth-selected": account, "auth-other": otherAccount},
		accountByID:     map[string]*accountSpec{"account-selected": account, "account-other": otherAccount},
		accountByAPIKey: map[string]*accountSpec{},
	}
	auth := &coreauth.Auth{ID: "auth-selected", Provider: "codex", Status: coreauth.StatusActive}
	fallback := &countingSelector{auth: auth}
	affinity := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: fallback,
		TTL:      time.Hour,
	})
	tracker := newRequestUsageTracker()
	selector := &recordingSelector{inner: affinity, manifest: m, tracker: tracker}
	headers := make(http.Header)
	headers.Set("X-Session-ID", "session-selected")
	opts := cliproxyexecutor.Options{Headers: headers}

	ctx1 := internallogging.WithRequestID(context.Background(), "req-first")
	if _, err := selector.Pick(ctx1, "codex", "gpt-5.5", opts, []*coreauth.Auth{auth}); err != nil {
		t.Fatalf("first pick: %v", err)
	}
	ctx2 := internallogging.WithRequestID(context.Background(), "req-cache")
	if _, err := selector.Pick(ctx2, "codex", "gpt-5.5", opts, []*coreauth.Auth{auth}); err != nil {
		t.Fatalf("cache pick: %v", err)
	}
	selector.OnSelectionResult(ctx2, coreauth.Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.5",
		Success:  true,
	}, opts)
	selector.OnSelectionResult(ctx2, coreauth.Result{
		AuthID:   "auth-other",
		Provider: "codex",
		Model:    "gpt-5.5",
		Success:  false,
	}, opts)
	canceledCtx, cancel := context.WithCancel(ctx2)
	cancel()
	selector.OnSelectionResult(canceledCtx, coreauth.Result{
		AuthID:   "auth-other",
		Provider: "codex",
		Model:    "gpt-5.5",
		Success:  true,
	}, opts)
	if fallback.count != 1 {
		t.Fatalf("expected second pick to use affinity cache, fallback count=%d", fallback.count)
	}

	payload, ok := tracker.finalize("req-cache", usageFinalizeInput{
		status:        http.StatusOK,
		latencyMS:     100,
		completedAtMS: 123,
	})
	if !ok {
		t.Fatal("expected finalized usage payload")
	}
	if payload.AccountID != "account-selected" || payload.AccountEmail != "selected@example.com" || payload.AuthID != "auth-selected" {
		t.Fatalf("expected cache hit selected account metadata, got %#v", payload)
	}
}

func TestRequestUsageTrackerKeepsStreamFailureAfterHTTPHeaders(t *testing.T) {
	tracker := newRequestUsageTracker()
	tracker.start("req-2")
	tracker.record(usagePayload{
		Type:          "usage",
		RequestID:     "req-2",
		AccountID:     "account-failed",
		Model:         "gpt-5.5",
		RequestKind:   "text",
		Success:       false,
		ErrorCategory: "request_failed",
		ErrorMessage:  "stream closed",
	})

	payload, ok := tracker.finalize("req-2", usageFinalizeInput{
		requestKind:   "text",
		model:         "gpt-5.5",
		status:        http.StatusOK,
		latencyMS:     100,
		completedAtMS: 123,
	})

	if !ok {
		t.Fatal("expected finalized usage payload")
	}
	if payload.Success || payload.ErrorCategory != "request_failed" {
		t.Fatalf("stream failure should remain failed even when HTTP status is 200: %#v", payload)
	}
}

func TestRequestPolicyEmitsRequestDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	m := &manifest{
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "key_1", Label: "Test key", Key: "client-key", Enabled: true},
		},
	}
	policy := &requestPolicy{manifest: m, emitter: emitter}
	router := gin.New()
	router.Use(policy.middleware())
	router.GET("/v1/responses", func(c *gin.Context) {
		if internallogging.GetRequestID(c.Request.Context()) == "" {
			t.Fatalf("request id should be attached to request context")
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	router.ServeHTTP(httptest.NewRecorder(), req)
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing request diagnostics")
	}
	out := output.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected start and complete diagnostics, got %d lines:\n%s", len(lines), out)
	}
	var start requestDiagnosticPayload
	if err := json.Unmarshal([]byte(lines[0]), &start); err != nil {
		t.Fatalf("start diagnostic should be JSON: %v\n%s", err, lines[0])
	}
	var complete requestDiagnosticPayload
	if err := json.Unmarshal([]byte(lines[1]), &complete); err != nil {
		t.Fatalf("complete diagnostic should be JSON: %v\n%s", err, lines[1])
	}
	if start.Type != "request_started" || complete.Type != "request_completed" {
		t.Fatalf("unexpected diagnostic types: %#v %#v", start.Type, complete.Type)
	}
	if start.RequestID == "" || complete.RequestID != start.RequestID {
		t.Fatalf("request id should be stable across diagnostics: %#v %#v", start, complete)
	}
	if start.LogicalRequestID != start.RequestID || complete.LogicalRequestID != start.RequestID {
		t.Fatalf("logical request id should be stable across diagnostics: %#v %#v", start, complete)
	}
	if complete.Status != http.StatusNoContent || complete.RequestKind != "text" || complete.APIKeyID != "key_1" {
		t.Fatalf("unexpected completion diagnostic: %#v", complete)
	}
}

func TestBuildExecutorRequestUsesStableLogicalIDForIdempotency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request = request.WithContext(internallogging.WithRequestID(request.Context(), "logical-1"))
	c.Request = request

	_, opts := buildExecutorRequest(c, []byte(`{"model":"gpt-5.5"}`), "gpt-5.5", sdktranslator.FromString("openai-response"), "", true)
	if got := opts.Metadata[cliproxyexecutor.LogicalRequestIDMetadataKey]; got != "logical-1" {
		t.Fatalf("logical request ID = %v", got)
	}
	if got := opts.Metadata[cliproxyexecutor.IdempotencyKeyMetadataKey]; got != "logical-1" {
		t.Fatalf("default idempotency key = %v", got)
	}

	c.Request.Header.Set("Idempotency-Key", "client-key-1")
	_, opts = buildExecutorRequest(c, []byte(`{"model":"gpt-5.5"}`), "gpt-5.5", sdktranslator.FromString("openai-response"), "", true)
	if got := opts.Metadata[cliproxyexecutor.IdempotencyKeyMetadataKey]; got != "client-key-1" {
		t.Fatalf("explicit idempotency key = %v", got)
	}
}

func TestRequestPolicyRecordsCanceledContextAsCanceledUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	tracker := newRequestUsageTracker()
	tracker.setEmitter(emitter)
	m := &manifest{apiKeyByValue: map[string]*apiKeySpec{
		"client-key": {ID: "key_1", Label: "Test key", Key: "client-key", Enabled: true},
	}}
	policy := &requestPolicy{manifest: m, emitter: emitter, tracker: tracker}
	router := gin.New()
	router.Use(policy.middleware())
	router.POST("/v1/responses", func(c *gin.Context) { c.Status(http.StatusOK) })

	requestContext, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
	req.Header.Set("Authorization", "Bearer client-key")
	router.ServeHTTP(httptest.NewRecorder(), req)
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing canceled usage")
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected request start, completion, and usage events; got %d: %s", len(lines), output.String())
	}
	var completed requestDiagnosticPayload
	if err := json.Unmarshal([]byte(lines[1]), &completed); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	var usage usagePayload
	if err := json.Unmarshal([]byte(lines[2]), &usage); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if !completed.Aborted || completed.ErrorMessage != context.Canceled.Error() {
		t.Fatalf("completion should retain canceled state: %#v", completed)
	}
	if usage.Success || usage.Status != clientClosedRequestStatus || usage.ErrorCategory != "client_canceled" {
		t.Fatalf("canceled request was not classified correctly: %#v", usage)
	}
}

func TestRequestPolicyClassifiesStreamTerminalErrorsAfterHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name          string
		err           error
		wantStatus    int
		wantCategory  string
		wantErrorText string
	}{
		{
			name:          "total timeout",
			err:           relayTimeoutError{phase: "stream_total", timeout: time.Second},
			wantStatus:    http.StatusGatewayTimeout,
			wantCategory:  "upstream_stream_timeout",
			wantErrorText: "stream_total",
		},
		{
			name:          "idle timeout",
			err:           relayTimeoutError{phase: "stream_idle", timeout: time.Second},
			wantStatus:    http.StatusGatewayTimeout,
			wantCategory:  "upstream_stream_timeout",
			wantErrorText: "stream_idle",
		},
		{
			name:          "chunk error",
			err:           errors.New("chunk read failed"),
			wantStatus:    http.StatusBadGateway,
			wantCategory:  "upstream_error",
			wantErrorText: "chunk read failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			emitter := newEventEmitter(&output)
			tracker := newRequestUsageTracker()
			tracker.setEmitter(emitter)
			m := &manifest{
				ModelIDs: []string{"gpt-5.5"},
				apiKeyByValue: map[string]*apiKeySpec{
					"client-key": {ID: "key_1", Label: "Test key", Key: "client-key", Enabled: true},
				},
			}
			policy := &requestPolicy{manifest: m, emitter: emitter, tracker: tracker}
			router := gin.New()
			router.Use(policy.middleware())
			router.POST("/v1/responses", func(c *gin.Context) {
				setEventStreamHeaders(c.Writer.Header())
				c.Status(http.StatusOK)
				writeStreamTerminalError(c, tt.err)
			})

			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
			req.Header.Set("Authorization", "Bearer client-key")
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("stream diagnostic HTTP status = %d, want 200", recorder.Code)
			}
			if !emitter.flush(time.Second) {
				t.Fatal("timed out flushing stream terminal diagnostics")
			}

			var completed requestDiagnosticPayload
			var usage usagePayload
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				var envelope struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal([]byte(line), &envelope); err != nil {
					t.Fatalf("decode event envelope: %v\n%s", err, line)
				}
				switch envelope.Type {
				case "request_completed":
					if err := json.Unmarshal([]byte(line), &completed); err != nil {
						t.Fatalf("decode completion: %v", err)
					}
				case "usage":
					if err := json.Unmarshal([]byte(line), &usage); err != nil {
						t.Fatalf("decode usage: %v", err)
					}
				}
			}

			if completed.Status != http.StatusOK {
				t.Fatalf("completion diagnostic status = %d, want original HTTP 200: %#v", completed.Status, completed)
			}
			if usage.Success || usage.Status != tt.wantStatus || usage.ErrorCategory != tt.wantCategory {
				t.Fatalf("stream terminal usage was not classified correctly: %#v", usage)
			}
			if !strings.Contains(strings.ToLower(usage.ErrorMessage), strings.ToLower(tt.wantErrorText)) {
				t.Fatalf("usage error %q does not contain %q", usage.ErrorMessage, tt.wantErrorText)
			}
		})
	}
}

func TestUpstreamAttemptLifecycleEmitsAttributedCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	tracker := newRequestUsageTracker()
	account := &accountSpec{ID: "account-1", Email: "account@example.com", AuthID: "auth-1"}
	server := &relayServer{
		emitter: emitter,
		manifest: &manifest{
			accountByAuthID: map[string]*accountSpec{"auth-1": account},
		},
		policy: &requestPolicy{tracker: tracker},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	requestContext := internallogging.WithRequestID(request.Context(), "logical-1")
	requestContext = context.WithValue(requestContext, clientAPIKeyContextKey, &apiKeySpec{
		ID:    "key_1",
		Label: "Default",
		Key:   "never-log-this-secret",
	})
	requestContext = context.WithValue(requestContext, requestKindContextKey, "text")
	requestContext = withClientInstanceID(requestContext, "codex-client-1")
	c.Request = request.WithContext(requestContext)

	lifecycle := newUpstreamAttemptLifecycle(server, c, "gpt-5.5")
	lifecycle.selected(cliproxyexecutor.AuthSelection{AuthID: "auth-1", AttemptID: 42})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:  cliproxyexecutor.UpstreamAttemptStarted,
		AuthID: "auth-1",
		At:     time.Now(),
	})
	lifecycle.complete(relayTimeoutError{phase: "stream_open attempt=1/1", timeout: 180 * time.Second}, true, true)
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing attempt events")
	}
	if strings.Contains(output.String(), "never-log-this-secret") {
		t.Fatalf("raw API key leaked into attempt event: %s", output.String())
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected start and terminal attempt events, got %d: %s", len(lines), output.String())
	}
	var terminal upstreamAttemptPayload
	if err := json.Unmarshal([]byte(lines[1]), &terminal); err != nil {
		t.Fatalf("decode terminal attempt: %v", err)
	}
	if terminal.Type != "upstream_attempt" || terminal.LogicalRequestID != "logical-1" || terminal.AttemptNumber != 1 {
		t.Fatalf("unexpected attempt identity: %#v", terminal)
	}
	if terminal.AccountID != account.ID || terminal.Email != account.Email || terminal.APIKeyID != "key_1" {
		t.Fatalf("attempt ownership was not retained: %#v", terminal)
	}
	if terminal.SentAt == 0 || terminal.CanceledAt == 0 || terminal.CompletedAt == 0 || terminal.FirstByteAt != 0 {
		t.Fatalf("unexpected attempt timestamps: %#v", terminal)
	}
	if terminal.RetryReason != "stream_open_timeout" || !terminal.PossibleBillableRequest || !terminal.UpstreamCancellationUnconfirmed {
		t.Fatalf("timeout should be marked possibly billable and cancellation-unconfirmed: %#v", terminal)
	}

	payload, ok := tracker.finalize("logical-1", usageFinalizeInput{status: http.StatusGatewayTimeout, completedAtMS: 123})
	if !ok || payload.AccountID != account.ID || payload.AccountEmail != account.Email || payload.AuthID != "auth-1" {
		t.Fatalf("attempt selection was not retained for logical usage: %#v", payload)
	}
}

func TestUpstreamAttemptLifecyclePreservesFirstTerminalOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	server := &relayServer{
		emitter: emitter,
		manifest: &manifest{accountByAuthID: map[string]*accountSpec{
			"auth-1": {ID: "account-1", Email: "account@example.com", AuthID: "auth-1"},
		}},
		policy: &requestPolicy{tracker: newRequestUsageTracker()},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = request.WithContext(internallogging.WithRequestID(request.Context(), "logical-timeout"))

	lifecycle := newUpstreamAttemptLifecycle(server, c, "gpt-5.5")
	lifecycle.selected(cliproxyexecutor.AuthSelection{AuthID: "auth-1", AttemptID: 1})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:  cliproxyexecutor.UpstreamAttemptStarted,
		AuthID: "auth-1",
		At:     time.Now(),
	})
	timeoutErr := relayTimeoutError{phase: "stream_open attempt=1/1", timeout: 180 * time.Second}
	lifecycle.complete(timeoutErr, true, true)
	// The executor commonly reports context cancellation after the sidecar has
	// already recorded its more specific first-byte timeout.
	lifecycle.complete(cliproxyexecutor.WrapUpstreamAttemptError(context.Canceled, 128, false, true), true, true)
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing attempt events")
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected lifecycle events, got %s", output.String())
	}
	var terminal upstreamAttemptPayload
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &terminal); err != nil {
		t.Fatalf("decode terminal attempt: %v", err)
	}
	if terminal.Status != http.StatusGatewayTimeout || terminal.RetryReason != "stream_open_timeout" {
		t.Fatalf("late cancellation overwrote first terminal outcome: %#v", terminal)
	}
	if !terminal.PossibleBillableRequest || !terminal.UpstreamCancellationUnconfirmed {
		t.Fatalf("late cancellation risk flags were not retained: %#v", terminal)
	}

	output.Reset()
	success := newUpstreamAttemptLifecycle(server, c, "gpt-5.5")
	success.logicalRequestID = "logical-complete"
	success.selected(cliproxyexecutor.AuthSelection{AuthID: "auth-1", AttemptID: 2})
	success.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:  cliproxyexecutor.UpstreamAttemptStarted,
		AuthID: "auth-1",
		At:     time.Now(),
	})
	success.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:      cliproxyexecutor.UpstreamAttemptResponded,
		AuthID:     "auth-1",
		At:         time.Now(),
		StatusCode: http.StatusOK,
	})
	success.complete(nil, false, false)
	success.complete(cliproxyexecutor.WrapUpstreamAttemptError(context.Canceled, 128, true, true), true, true)
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing successful attempt events")
	}
	lines = strings.Split(strings.TrimSpace(output.String()), "\n")
	terminal = upstreamAttemptPayload{}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &terminal); err != nil {
		t.Fatalf("decode successful terminal attempt: %v", err)
	}
	if terminal.Status != http.StatusOK || terminal.RetryReason != "" || terminal.CanceledAt != 0 {
		t.Fatalf("late cancellation replaced successful completion: %#v", terminal)
	}
}

func TestUpstreamAttemptLifecycleCountsTransportStartsNotAuthSelections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	server := &relayServer{
		emitter: emitter,
		manifest: &manifest{accountByAuthID: map[string]*accountSpec{
			"auth-1": {ID: "account-1", Email: "one@example.com", AuthID: "auth-1"},
			"auth-2": {ID: "account-2", Email: "two@example.com", AuthID: "auth-2"},
		}},
		policy: &requestPolicy{tracker: newRequestUsageTracker()},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = request.WithContext(internallogging.WithRequestID(request.Context(), "logical-transport"))
	lifecycle := newUpstreamAttemptLifecycle(server, c, "gpt-5.5")

	// Auth selection and preparation are not physical upstream attempts.
	lifecycle.selected(cliproxyexecutor.AuthSelection{AuthID: "auth-1", AttemptID: 1})
	if !emitter.flush(time.Second) || output.Len() != 0 {
		t.Fatalf("selection alone emitted an upstream attempt: %s", output.String())
	}

	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:  cliproxyexecutor.UpstreamAttemptStarted,
		AuthID: "auth-1",
		At:     time.Now(),
	})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase: cliproxyexecutor.UpstreamAttemptFinished,
		Err:   cliproxyexecutor.WrapUpstreamAttemptError(errors.New("dial failed"), 0, false, false),
	})
	lifecycle.selected(cliproxyexecutor.AuthSelection{AuthID: "auth-2", AttemptID: 2})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:  cliproxyexecutor.UpstreamAttemptStarted,
		AuthID: "auth-2",
		At:     time.Now(),
	})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:            cliproxyexecutor.UpstreamAttemptResponded,
		AuthID:           "auth-2",
		At:               time.Now(),
		StatusCode:       http.StatusOK,
		ResponseReceived: true,
	})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:            cliproxyexecutor.UpstreamAttemptFinished,
		AuthID:           "auth-2",
		ResponseReceived: true,
	})
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing physical attempt events")
	}
	unique := make(map[string]upstreamAttemptPayload)
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var event upstreamAttemptPayload
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode attempt event: %v", err)
		}
		unique[event.UpstreamAttemptID] = event
	}
	if len(unique) != 2 {
		t.Fatalf("unique physical attempts = %d, want 2; events=%s", len(unique), output.String())
	}
	if unique["logical-transport:1"].RetryReason != "pre_send_failure" {
		t.Fatalf("first physical attempt missing pre-send outcome: %#v", unique["logical-transport:1"])
	}
	second := unique["logical-transport:2"]
	if second.AccountID != "account-2" || second.Email != "two@example.com" || second.FirstByteAt == 0 || second.CompletedAt == 0 {
		t.Fatalf("second physical attempt was not completed and attributed: %#v", second)
	}
}

func TestUpstreamAttemptLifecycleTracksCredentialRejectionFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	server := &relayServer{
		emitter: emitter,
		manifest: &manifest{accountByAuthID: map[string]*accountSpec{
			"auth-1": {ID: "account-1", Email: "one@example.com", AuthID: "auth-1"},
			"auth-2": {ID: "account-2", Email: "two@example.com", AuthID: "auth-2"},
		}},
		policy: &requestPolicy{tracker: newRequestUsageTracker()},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request = request.WithContext(internallogging.WithRequestID(request.Context(), "logical-quota-failover"))
	lifecycle := newUpstreamAttemptLifecycle(server, c, "gpt-5.5")

	lifecycle.selected(cliproxyexecutor.AuthSelection{AuthID: "auth-1", AttemptID: 1})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:  cliproxyexecutor.UpstreamAttemptStarted,
		AuthID: "auth-1",
		At:     time.Now(),
	})
	rejected := cliproxyexecutor.MarkCredentialFallbackSafe(testExecutorStatusError{status: http.StatusTooManyRequests})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:                cliproxyexecutor.UpstreamAttemptFinished,
		AuthID:               "auth-1",
		At:                   time.Now(),
		RequestBodyBytesRead: 64,
		ResponseReceived:     true,
		StatusCode:           http.StatusTooManyRequests,
		Err:                  cliproxyexecutor.WrapUpstreamAttemptError(rejected, 64, true, false),
	})

	lifecycle.selected(cliproxyexecutor.AuthSelection{AuthID: "auth-2", AttemptID: 2})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:  cliproxyexecutor.UpstreamAttemptStarted,
		AuthID: "auth-2",
		At:     time.Now(),
	})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:            cliproxyexecutor.UpstreamAttemptResponded,
		AuthID:           "auth-2",
		At:               time.Now(),
		ResponseReceived: true,
		StatusCode:       http.StatusOK,
	})
	lifecycle.observe(cliproxyexecutor.UpstreamAttemptObservation{
		Phase:            cliproxyexecutor.UpstreamAttemptFinished,
		AuthID:           "auth-2",
		At:               time.Now(),
		ResponseReceived: true,
		StatusCode:       http.StatusOK,
	})
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing credential-failover attempts")
	}

	unique := make(map[string]upstreamAttemptPayload)
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var event upstreamAttemptPayload
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode attempt event: %v", err)
		}
		unique[event.UpstreamAttemptID] = event
	}
	if len(unique) != 2 {
		t.Fatalf("unique physical attempts = %d, want 2; events=%s", len(unique), output.String())
	}
	first := unique["logical-quota-failover:1"]
	if first.Status != http.StatusTooManyRequests || first.RetryReason != "upstream_429" || first.PossibleBillableRequest {
		t.Fatalf("quota rejection attempt classification = %#v", first)
	}
	second := unique["logical-quota-failover:2"]
	if second.AccountID != "account-2" || second.Status != http.StatusOK || second.CompletedAt == 0 {
		t.Fatalf("fallback attempt classification = %#v", second)
	}
}

func TestUsagePluginResolvesAPIKeyAndRequestKindFromCPARecord(t *testing.T) {
	m := &manifest{
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "key_1", Label: "Test key", Key: "client-key", Enabled: true},
		},
	}
	tracker := newRequestUsageTracker()
	tracker.start("req-1")
	plugin := &usagePlugin{manifest: m, tracker: tracker}
	ctx := internallogging.WithRequestID(context.Background(), "req-1")
	ctx = internallogging.WithEndpoint(ctx, "POST /v1/responses")

	plugin.HandleUsage(ctx, coreusage.Record{
		Provider:    "codex",
		Model:       "gpt-5.4-mini",
		APIKey:      "client-key",
		RequestedAt: time.UnixMilli(123),
		Latency:     50 * time.Millisecond,
	})

	payload, ok := tracker.finalize("req-1", usageFinalizeInput{
		status:        http.StatusOK,
		latencyMS:     50,
		completedAtMS: 123,
	})
	if !ok {
		t.Fatal("expected usage payload")
	}
	if payload.APIKeyID != "key_1" || payload.APIKeyLabel != "Test key" {
		t.Fatalf("API key metadata was not resolved: %#v", payload)
	}
	if payload.RequestID != "req-1" {
		t.Fatalf("request id should be forwarded, got %q", payload.RequestID)
	}
	if payload.RequestKind != "text" {
		t.Fatalf("request kind should be inferred from endpoint, got %q", payload.RequestKind)
	}
}

func TestErrorCategoryClassifiesClientCanceled(t *testing.T) {
	if got := errorCategory(0, "context canceled", false); got != "client_canceled" {
		t.Fatalf("expected client_canceled, got %q", got)
	}
	if got := errorCategory(http.StatusGatewayTimeout, `Post "https://chatgpt.com/backend-api/codex/responses": context canceled`, false); got != "gateway_context_canceled" {
		t.Fatalf("expected gateway_context_canceled for upstream context cancellation, got %q", got)
	}
	if got := errorCategory(http.StatusBadGateway, "write tcp: broken pipe", false); got != "client_canceled" {
		t.Fatalf("expected client_canceled for broken pipe, got %q", got)
	}
	if got := errorCategory(http.StatusGatewayTimeout, "upstream timed out in stream_open attempt=1/1 after 60s", false); got != "upstream_first_byte_timeout" {
		t.Fatalf("expected upstream_first_byte_timeout, got %q", got)
	}
	if got := errorCategory(http.StatusBadGateway, "unexpected EOF before response.completed", false); got != "upstream_error" {
		t.Fatalf("expected upstream_error for an upstream EOF, got %q", got)
	}
}

func TestAuthHookEmitsRequestScopedResultDiagnostics(t *testing.T) {
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	apiKey := &apiKeySpec{ID: "key_1", Label: "Test key", Key: "client-key", Enabled: true}
	account := &accountSpec{ID: "account_1", Email: "user@example.com", AuthID: "auth.json"}
	m := &manifest{
		accountByAuthID: map[string]*accountSpec{"auth.json": account},
		accountByID:     map[string]*accountSpec{"auth": account},
	}
	hook := &authHook{manifest: m, emitter: emitter}
	ctx := internallogging.WithRequestID(context.Background(), "req-2")
	ctx = context.WithValue(ctx, clientAPIKeyContextKey, apiKey)
	ctx = context.WithValue(ctx, requestKindContextKey, "text")
	ctx = context.WithValue(ctx, requestModelContextKey, "gpt-5.5")

	hook.OnResult(ctx, coreauth.Result{
		AuthID:   "auth.json",
		Provider: "codex",
		Model:    "upstream-model",
		Success:  false,
		Error: &coreauth.Error{
			Code:       "upstream_timeout",
			Message:    "upstream timed out",
			Retryable:  true,
			HTTPStatus: http.StatusGatewayTimeout,
		},
	})
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing auth result diagnostic")
	}
	out := output.String()

	var payload requestDiagnosticPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("auth result diagnostic should be JSON: %v\n%s", err, out)
	}
	if payload.Type != "auth_result" || payload.RequestID != "req-2" {
		t.Fatalf("unexpected auth result diagnostic identity: %#v", payload)
	}
	if payload.Model != "gpt-5.5" || payload.AccountID != "account_1" || payload.APIKeyID != "key_1" {
		t.Fatalf("unexpected auth result metadata: %#v", payload)
	}
	if payload.Success == nil || *payload.Success || payload.Retryable == nil || !*payload.Retryable {
		t.Fatalf("failure details should be preserved: %#v", payload)
	}
	if payload.HTTPStatus != http.StatusGatewayTimeout || payload.ErrorCode != "upstream_timeout" {
		t.Fatalf("unexpected failure details: %#v", payload)
	}
}

func TestRelayServerExecutesNonStreamingRequestThroughRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{
		response: cliproxyexecutor.Response{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Payload: []byte(`{"ok":true}`),
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
	if runtime.executeCalls != 1 || runtime.streamCalls != 0 {
		t.Fatalf("unexpected runtime calls: execute=%d stream=%d", runtime.executeCalls, runtime.streamCalls)
	}
	if runtime.lastReq.Model != "gpt-5.5" || runtime.lastOpts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("unexpected executor request: %#v %#v", runtime.lastReq, runtime.lastOpts)
	}
	if runtime.lastOpts.Headers.Get("Authorization") != "Bearer client-key" {
		t.Fatalf("request headers should be forwarded to CPA executor")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS header should match CPA server behavior")
	}
}

func TestRelayServerProviderGatewayRoutesResponsesToChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamPath string
	var upstreamAuth string
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	runtime := &fakeRuntime{}
	m := &manifest{
		APIKeys: []apiKeySpec{{
			ID:      "provider_gateway_account_1",
			Label:   "Provider Gateway",
			Key:     "client-key",
			Enabled: true,
			ProviderGateway: &providerGatewaySpec{
				BaseURL:        upstream.URL,
				APIKey:         "deepseek-key",
				UpstreamModel:  "deepseek-v4-flash",
				UpstreamModels: []string{"deepseek-v4-flash", "deepseek-v4-pro"},
				WireAPI:        "chat_completions",
			},
		}},
		ModelIDs: []string{"deepseek-chat"},
		ModelAliases: []modelAliasSpec{
			{SourceModel: "deepseek-v4-flash", Alias: "gpt-5.5"},
			{SourceModel: "deepseek-v4-pro", Alias: "gpt-5.4"},
		},
		aliasToSource: map[string]string{
			"gpt-5.5": "deepseek-v4-flash",
			"gpt-5.4": "deepseek-v4-pro",
		},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {
				ID:      "provider_gateway_account_1",
				Label:   "Provider Gateway",
				Key:     "client-key",
				Enabled: true,
				ProviderGateway: &providerGatewaySpec{
					BaseURL:        upstream.URL,
					APIKey:         "deepseek-key",
					UpstreamModel:  "deepseek-v4-flash",
					UpstreamModels: []string{"deepseek-v4-flash", "deepseek-v4-pro"},
					WireAPI:        "chat_completions",
				},
			},
		},
	}
	var eventOutput bytes.Buffer
	emitter := newEventEmitter(&eventOutput)
	tracker := newRequestUsageTracker()
	tracker.setEmitter(emitter)
	policy := &requestPolicy{manifest: m, emitter: emitter, tracker: tracker}
	router := (&relayServer{
		runtime:  runtime,
		cfg:      &config.Config{},
		manifest: m,
		emitter:  emitter,
		policy:   policy,
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 0 || runtime.streamCalls != 0 {
		t.Fatalf("provider gateway should bypass runtime auth pool: execute=%d stream=%d", runtime.executeCalls, runtime.streamCalls)
	}
	if upstreamPath != "/v1/chat/completions" {
		t.Fatalf("unexpected upstream path: %s", upstreamPath)
	}
	if upstreamAuth != "Bearer deepseek-key" {
		t.Fatalf("unexpected upstream auth: %s", upstreamAuth)
	}
	if !strings.Contains(upstreamBody, `"messages"`) || !strings.Contains(upstreamBody, `"stream":false`) {
		t.Fatalf("request should be converted to chat completions: %s", upstreamBody)
	}
	if !strings.Contains(upstreamBody, `"model":"deepseek-v4-pro"`) || strings.Contains(upstreamBody, `"model":"gpt-5.4"`) {
		t.Fatalf("request should use provider upstream model: %s", upstreamBody)
	}
	if !strings.Contains(w.Body.String(), `"object":"response"`) || !strings.Contains(w.Body.String(), `"output_text"`) {
		t.Fatalf("response should be converted back to responses shape: %s", w.Body.String())
	}
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing provider gateway usage")
	}
	var recordedUsage usagePayload
	for _, line := range strings.Split(strings.TrimSpace(eventOutput.String()), "\n") {
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &envelope) != nil || envelope.Type != "usage" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &recordedUsage); err != nil {
			t.Fatalf("decode provider gateway usage: %v", err)
		}
	}
	if recordedUsage.Usage.InputTokens != 1 || recordedUsage.Usage.OutputTokens != 1 || recordedUsage.Usage.TotalTokens != 2 {
		t.Fatalf("provider gateway usage was not preserved: %#v", recordedUsage)
	}
	if recordedUsage.AccountID == "" || recordedUsage.AccountEmail == "" || recordedUsage.APIKeyID != "provider_gateway_account_1" {
		t.Fatalf("provider gateway usage lost ownership: %#v", recordedUsage)
	}

	modelReq := httptest.NewRequest(http.MethodGet, "/v1/models?codex_client=1", nil)
	modelReq.Header.Set("Authorization", "Bearer client-key")
	modelW := httptest.NewRecorder()
	router.ServeHTTP(modelW, modelReq)
	if modelW.Code != http.StatusOK {
		t.Fatalf("unexpected models status: %d body=%s", modelW.Code, modelW.Body.String())
	}
	if !strings.Contains(modelW.Body.String(), "gpt-5.5") || !strings.Contains(modelW.Body.String(), "gpt-5.4") || strings.Contains(modelW.Body.String(), "deepseek-v4-pro") {
		t.Fatalf("provider gateway should expose client model slots only: %s", modelW.Body.String())
	}
}

func TestProviderGatewayUsageParsingNormalizesResponsesAndChat(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    usageDetails
	}{
		{
			name: "responses",
			payload: `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,` +
				`"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":3}}}}`,
			want: usageDetails{InputTokens: 11, OutputTokens: 7, TotalTokens: 18, CachedTokens: 5, ReasoningTokens: 3},
		},
		{
			name: "chat",
			payload: `{"usage":{"prompt_tokens":13,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":4},` +
				`"completion_tokens_details":{"reasoning_tokens":2}}}`,
			want: usageDetails{InputTokens: 13, OutputTokens: 9, TotalTokens: 22, CachedTokens: 4, ReasoningTokens: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := providerGatewayUsageFromPayload([]byte(tt.payload))
			if !ok || got != tt.want {
				t.Fatalf("usage = %#v, %v; want %#v, true", got, ok, tt.want)
			}
		})
	}
}

func TestProviderGatewaySSEUsageObserverHandlesSplitFrames(t *testing.T) {
	var recorded []usageDetails
	observer := &providerGatewaySSEUsageObserver{record: func(details usageDetails) {
		recorded = append(recorded, details)
	}}
	payload := []byte("event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":21,\"output_tokens\":8,\"total_tokens\":29}}}\r\n\r\n")
	for _, chunk := range [][]byte{payload[:17], payload[17:63], payload[63:]} {
		observer.feed(chunk)
	}
	observer.flush()
	if len(recorded) != 1 || recorded[0].TotalTokens != 29 || recorded[0].InputTokens != 21 || recorded[0].OutputTokens != 8 {
		t.Fatalf("split SSE usage was not recorded once: %#v", recorded)
	}
}

func TestProviderGatewayResponsesObserverRequiresCompletedEvent(t *testing.T) {
	responsesObserver := &providerGatewaySSEUsageObserver{requireResponseCompleted: true}
	responsesObserver.feed([]byte("data: [DONE]\n\n"))
	responsesObserver.flush()
	if err := responsesObserver.terminalError(); err == nil {
		t.Fatal("native Responses stream accepted [DONE] without response.completed")
	}

	failedObserver := &providerGatewaySSEUsageObserver{requireResponseCompleted: true}
	failedObserver.feed([]byte("data: {\"type\":\"response.failed\"}\n\ndata: [DONE]\n\n"))
	failedObserver.flush()
	if err := failedObserver.terminalError(); err == nil || !strings.Contains(err.Error(), "response.failed") {
		t.Fatalf("response.failed terminal error = %v", err)
	}

	chatObserver := &providerGatewaySSEUsageObserver{}
	chatObserver.feed([]byte("data: [DONE]\n\n"))
	chatObserver.flush()
	if err := chatObserver.terminalError(); err != nil {
		t.Fatalf("Chat Completions [DONE] terminal error = %v", err)
	}
}

func TestProviderGatewayObservedBodyRejectsCleanEOFWithoutTerminalEvent(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{
			name:    "missing terminal event",
			payload: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
			wantErr: true,
		},
		{
			name:    "completed response",
			payload: "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observer := &providerGatewaySSEUsageObserver{}
			var completedErr error
			body := &providerGatewayObservedBody{
				body:     io.NopCloser(strings.NewReader(tt.payload)),
				observe:  observer.feed,
				flush:    observer.flush,
				eofError: observer.terminalError,
				finish:   func(err error) { completedErr = err },
			}
			_, readErr := io.ReadAll(body)
			if tt.wantErr {
				if !errors.Is(readErr, io.ErrUnexpectedEOF) || !errors.Is(completedErr, io.ErrUnexpectedEOF) {
					t.Fatalf("clean EOF errors = read:%v completed:%v", readErr, completedErr)
				}
				return
			}
			if readErr != nil || completedErr != nil {
				t.Fatalf("completed stream errors = read:%v completed:%v", readErr, completedErr)
			}
		})
	}
}

func TestEnsureProviderGatewayChatStreamUsage(t *testing.T) {
	body := ensureProviderGatewayChatStreamUsage([]byte(`{"model":"upstream","stream":true,"stream_options":{"custom":1}}`))
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	options, _ := payload["stream_options"].(map[string]any)
	if options["include_usage"] != true || options["custom"] != float64(1) {
		t.Fatalf("stream options were not merged: %#v", options)
	}
}

func TestRelayServerProviderGatewayDoesNotReplayRedirectedResponsesPOST(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamCalls atomic.Int32
	var idempotencyKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		idempotencyKey = r.Header.Get("Idempotency-Key")
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/v1/responses" {
			w.Header().Set("Location", "/v1/replayed")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL:       upstream.URL,
		APIKey:        "provider-key",
		UpstreamModel: "upstream-model",
		WireAPI:       "responses",
	}
	providerAccount := accountSpec{ID: "provider-account-id", Email: "provider@example.com", AuthID: "provider-auth-id", UpstreamAPIKey: "provider-key"}
	apiKey := apiKeySpec{ID: "provider-key-id", Label: "Provider", Key: "client-key", Enabled: true, ProviderGateway: gateway, AccountIDs: []string{providerAccount.ID}}
	m := &manifest{
		APIKeys:       []apiKeySpec{apiKey},
		Accounts:      []accountSpec{providerAccount},
		ModelIDs:      []string{"gpt-5.4"},
		apiKeyByValue: map[string]*apiKeySpec{"client-key": &apiKey},
		accountByID:   map[string]*accountSpec{providerAccount.ID: &providerAccount},
		accountByAPIKey: map[string]*accountSpec{
			providerAccount.UpstreamAPIKey: &providerAccount,
		},
	}
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	tracker := newRequestUsageTracker()
	tracker.setEmitter(emitter)
	policy := &requestPolicy{manifest: m, emitter: emitter, tracker: tracker}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		emitter:  emitter,
		policy:   policy,
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if location := w.Header().Get("Location"); location != "" {
		t.Fatalf("redirect Location leaked to downstream client: %q", location)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("redirect replayed provider gateway POST: calls=%d", got)
	}
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing provider gateway attempt events")
	}

	var terminal upstreamAttemptPayload
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &envelope) != nil || envelope.Type != "upstream_attempt" {
			continue
		}
		var event upstreamAttemptPayload
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode upstream attempt: %v", err)
		}
		terminal = event
	}
	if terminal.CompletedAt == 0 || terminal.Status != http.StatusFound || !terminal.PossibleBillableRequest {
		t.Fatalf("provider gateway terminal attempt = %#v", terminal)
	}
	if terminal.AccountID != providerAccount.ID || terminal.Email != providerAccount.Email || terminal.APIKeyID != "provider-key-id" {
		t.Fatalf("provider gateway attempt lost ownership: %#v", terminal)
	}
	if terminal.LogicalRequestID == "" || idempotencyKey != terminal.LogicalRequestID {
		t.Fatalf("Idempotency-Key = %q, logical_request_id = %q", idempotencyKey, terminal.LogicalRequestID)
	}
}

func TestRelayServerProviderGatewayStreamTimeoutsCancelOneUpstreamPOST(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name          string
		openTimeout   int
		idleTimeout   int
		totalTimeout  int
		initialOutput string
		heartbeat     time.Duration
		wantPhase     string
	}{
		{name: "open", openTimeout: 40, idleTimeout: 500, initialOutput: "", wantPhase: "stream_open"},
		{name: "idle", openTimeout: 500, idleTimeout: 40, initialOutput: "data: {\"type\":\"response.created\"}\n\n", wantPhase: "stream_idle"},
		{name: "total", openTimeout: 500, idleTimeout: 500, totalTimeout: 60, initialOutput: "data: {\"type\":\"response.created\"}\n\n", heartbeat: 15 * time.Millisecond, wantPhase: "stream_total"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstreamCanceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				defer close(upstreamCanceled)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				if tt.initialOutput != "" {
					_, _ = io.WriteString(w, tt.initialOutput)
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				if tt.heartbeat <= 0 {
					<-r.Context().Done()
					return
				}
				ticker := time.NewTicker(tt.heartbeat)
				defer ticker.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
						_, _ = io.WriteString(w, ": heartbeat\n\n")
						if flusher, ok := w.(http.Flusher); ok {
							flusher.Flush()
						}
					}
				}
			}))
			defer upstream.Close()

			gateway := &providerGatewaySpec{BaseURL: upstream.URL, APIKey: "provider-key", UpstreamModel: "upstream-model", WireAPI: "responses"}
			apiKey := apiKeySpec{ID: "provider-key-id", Label: "Provider", Key: "client-key", Enabled: true, ProviderGateway: gateway}
			m := &manifest{
				APIKeys:       []apiKeySpec{apiKey},
				ModelIDs:      []string{"gpt-5.4"},
				apiKeyByValue: map[string]*apiKeySpec{"client-key": &apiKey},
			}
			cfg := &config.Config{}
			cfg.Streaming.StreamOpenTimeoutMS = tt.openTimeout
			cfg.Streaming.StreamIdleTimeoutMS = tt.idleTimeout
			cfg.Streaming.StreamTotalTimeoutMS = tt.totalTimeout
			router := (&relayServer{runtime: &fakeRuntime{}, cfg: cfg, manifest: m, policy: &requestPolicy{manifest: m}}).router()
			relay := httptest.NewServer(router)
			defer relay.Close()

			req, err := http.NewRequest(http.MethodPost, relay.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer client-key")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("provider gateway request failed: %v", err)
			}
			responseBody, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read provider gateway response: %v", readErr)
			}

			if got := upstreamCalls.Load(); got != 1 {
				t.Fatalf("upstream POST count = %d, want 1", got)
			}
			if !strings.Contains(string(responseBody), tt.wantPhase) {
				t.Fatalf("response body does not contain %q: %s", tt.wantPhase, responseBody)
			}
			select {
			case <-upstreamCanceled:
			case <-time.After(time.Second):
				t.Fatal("provider gateway upstream request was not canceled")
			}
		})
	}
}

func TestRelayServerProviderGatewayOpenTimeoutBeforeHeadersReturns504(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamCalls atomic.Int32
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{BaseURL: upstream.URL, APIKey: "provider-key", UpstreamModel: "upstream-model", WireAPI: "responses"}
	apiKey := apiKeySpec{ID: "provider-key-id", Label: "Provider", Key: "client-key", Enabled: true, ProviderGateway: gateway}
	m := &manifest{APIKeys: []apiKeySpec{apiKey}, ModelIDs: []string{"gpt-5.4"}, apiKeyByValue: map[string]*apiKeySpec{"client-key": &apiKey}}
	cfg := &config.Config{}
	cfg.Streaming.StreamOpenTimeoutMS = 40
	cfg.Streaming.StreamIdleTimeoutMS = 500
	router := (&relayServer{runtime: &fakeRuntime{}, cfg: cfg, manifest: m, policy: &requestPolicy{manifest: m}}).router()
	relay := httptest.NewServer(router)
	defer relay.Close()

	req, err := http.NewRequest(http.MethodPost, relay.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("provider gateway request failed: %v", err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", resp.StatusCode, responseBody)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream POST count = %d, want 1", got)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("upstream did not observe open-timeout cancellation")
	}
}

func TestRelayServerProviderGatewayClientCancellationKeepsAttemptOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamCalls atomic.Int32
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(upstreamStarted)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{BaseURL: upstream.URL, APIKey: "provider-key", UpstreamModel: "upstream-model", WireAPI: "responses"}
	providerAccount := accountSpec{ID: "provider-account-id", Email: "provider@example.com", AuthID: "provider-auth-id", UpstreamAPIKey: "provider-key"}
	apiKey := apiKeySpec{ID: "provider-key-id", Label: "Provider", Key: "client-key", Enabled: true, ProviderGateway: gateway, AccountIDs: []string{providerAccount.ID}}
	m := &manifest{
		APIKeys:       []apiKeySpec{apiKey},
		Accounts:      []accountSpec{providerAccount},
		ModelIDs:      []string{"gpt-5.4"},
		apiKeyByValue: map[string]*apiKeySpec{"client-key": &apiKey},
		accountByID:   map[string]*accountSpec{providerAccount.ID: &providerAccount},
		accountByAPIKey: map[string]*accountSpec{
			providerAccount.UpstreamAPIKey: &providerAccount,
		},
	}
	var output bytes.Buffer
	emitter := newEventEmitter(&output)
	tracker := newRequestUsageTracker()
	tracker.setEmitter(emitter)
	policy := &requestPolicy{manifest: m, emitter: emitter, tracker: tracker}
	cfg := &config.Config{}
	cfg.Streaming.StreamOpenTimeoutMS = 5_000
	cfg.Streaming.StreamIdleTimeoutMS = 5_000
	router := (&relayServer{runtime: &fakeRuntime{}, cfg: cfg, manifest: m, emitter: emitter, policy: policy}).router()
	localHandlerDone := make(chan struct{})
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(localHandlerDone)
		router.ServeHTTP(w, r)
	}))
	defer relay.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, relay.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		resp, requestErr := http.DefaultClient.Do(req)
		if resp != nil {
			_, readErr := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if requestErr == nil {
				requestErr = readErr
			}
		}
		done <- requestErr
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("provider gateway upstream request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("downstream handler did not stop after client cancellation")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("upstream did not observe client cancellation")
	}
	select {
	case <-localHandlerDone:
	case <-time.After(time.Second):
		t.Fatal("local provider gateway handler did not finish cancellation accounting")
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream POST count = %d, want 1", got)
	}
	if !emitter.flush(time.Second) {
		t.Fatal("timed out flushing provider gateway cancellation event")
	}

	var terminal upstreamAttemptPayload
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var event upstreamAttemptPayload
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "upstream_attempt" && event.CompletedAt > 0 {
			terminal = event
		}
	}
	if terminal.AccountID != providerAccount.ID || terminal.Email != providerAccount.Email || terminal.APIKeyID != apiKey.ID {
		t.Fatalf("provider gateway cancellation lost ownership: %#v", terminal)
	}
	if terminal.CanceledAt == 0 || !terminal.PossibleBillableRequest || !terminal.UpstreamCancellationUnconfirmed {
		t.Fatalf("provider gateway cancellation flags = %#v", terminal)
	}
}

func TestRelayServerProviderGatewayPreservesVersionedBaseURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"glm-5.1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL:        upstream.URL + "/api/coding/paas/v4",
		APIKey:         "zhipu-key",
		UpstreamModel:  "glm-5.1",
		UpstreamModels: []string{"glm-5.1"},
		WireAPI:        "chat_completions",
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway}},
		ModelIDs: []string{"glm-5.1"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway},
		},
	}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.1","input":"hello","stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if upstreamPath != "/api/coding/paas/v4/chat/completions" {
		t.Fatalf("unexpected upstream path: %s", upstreamPath)
	}
}

func TestProviderGatewayURLPreservesVersionedBasePaths(t *testing.T) {
	tests := []struct {
		name string
		base string
		path string
		want string
	}{
		{
			name: "bare host appends openai v1 path",
			base: "https://api.example.com",
			path: "/v1/chat/completions",
			want: "https://api.example.com/v1/chat/completions",
		},
		{
			name: "existing v1 base keeps single v1",
			base: "https://api.example.com/v1/",
			path: "/v1/chat/completions",
			want: "https://api.example.com/v1/chat/completions",
		},
		{
			name: "complete endpoint is left unchanged",
			base: "https://api.example.com/v1/chat/completions",
			path: "/v1/chat/completions",
			want: "https://api.example.com/v1/chat/completions",
		},
		{
			name: "zhipu coding paas v4 base keeps v4 root",
			base: "https://open.bigmodel.cn/api/coding/paas/v4",
			path: "/v1/chat/completions",
			want: "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
		},
		{
			name: "zai coding paas v4 base keeps v4 root",
			base: "https://api.z.ai/api/coding/paas/v4",
			path: "/v1/chat/completions",
			want: "https://api.z.ai/api/coding/paas/v4/chat/completions",
		},
		{
			name: "volcengine coding v3 base keeps v3 root",
			base: "https://ark.cn-beijing.volces.com/api/coding/v3",
			path: "/v1/chat/completions",
			want: "https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions",
		},
		{
			name: "doubao api v3 base keeps v3 root",
			base: "https://ark.cn-beijing.volces.com/api/v3",
			path: "/v1/chat/completions",
			want: "https://ark.cn-beijing.volces.com/api/v3/chat/completions",
		},
		{
			name: "qianfan v2 coding base keeps v2 root",
			base: "https://qianfan.baidubce.com/v2/coding",
			path: "/v1/chat/completions",
			want: "https://qianfan.baidubce.com/v2/coding/chat/completions",
		},
		{
			name: "versioned responses path drops openai v1 prefix",
			base: "https://open.bigmodel.cn/api/coding/paas/v4",
			path: "/v1/responses",
			want: "https://open.bigmodel.cn/api/coding/paas/v4/responses",
		},
		{
			name: "base query is stripped",
			base: "https://api.example.com/v1?ignored=1",
			path: "/v1/chat/completions",
			want: "https://api.example.com/v1/chat/completions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := providerGatewayURL(tt.base, tt.path)
			if err != nil {
				t.Fatalf("providerGatewayURL returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("providerGatewayURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRelayServerProviderGatewayChatStreamTerminatesResponsesSSEFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL:        upstream.URL,
		APIKey:         "deepseek-key",
		UpstreamModel:  "deepseek-v4-flash",
		UpstreamModels: []string{"deepseek-v4-flash"},
		WireAPI:        "chat_completions",
		SupportsVision: true,
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway}},
		ModelIDs: []string{"deepseek-v4-flash"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway},
		},
	}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"deepseek-v4-flash","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("stream should include response.completed: %s", body)
	}
	if !strings.Contains(body, "event: response.completed\n") || !strings.Contains(body, "\n\n") {
		t.Fatalf("stream should emit complete SSE frames separated by a blank line: %q", body)
	}
}

func TestRelayServerProviderGatewayFallsBackToDefaultUpstreamModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL:        upstream.URL,
		APIKey:         "deepseek-key",
		UpstreamModel:  "deepseek-v4-flash",
		UpstreamModels: []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		WireAPI:        "chat_completions",
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway}},
		ModelIDs: []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway},
		},
	}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(upstreamBody, `"model":"deepseek-v4-flash"`) || strings.Contains(upstreamBody, `"model":"gpt-5.4"`) {
		t.Fatalf("request should fall back to provider default upstream model: %s", upstreamBody)
	}
}

func TestRelayServerProviderGatewayPassesThroughModelWhenCatalogEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL: upstream.URL,
		APIKey:  "provider-key",
		WireAPI: "chat_completions",
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway}},
		ModelIDs: []string{"gpt-5"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway},
		},
	}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hello","stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(upstreamBody, `"model":"gpt-5"`) || strings.Contains(upstreamBody, "gpt-5.5") {
		t.Fatalf("request should pass through the client model when provider catalog is empty: %s", upstreamBody)
	}
}

func TestRelayServerProviderGatewayUsesSelectedUpstreamModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"deepseek-v4-pro","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL:        upstream.URL,
		APIKey:         "deepseek-key",
		UpstreamModel:  "deepseek-v4-flash",
		UpstreamModels: []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		WireAPI:        "chat_completions",
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway}},
		ModelIDs: []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway},
		},
	}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"deepseek-v4-pro","input":"hello","stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(upstreamBody, `"model":"deepseek-v4-pro"`) || strings.Contains(upstreamBody, `"model":"deepseek-v4-flash"`) {
		t.Fatalf("request should use selected upstream model: %s", upstreamBody)
	}
}

func TestRelayServerProviderGatewayRejectsVisionInputWhenUnsupported(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL:        upstream.URL,
		APIKey:         "deepseek-key",
		UpstreamModel:  "deepseek-v4-flash",
		UpstreamModels: []string{"deepseek-v4-flash"},
		WireAPI:        "chat_completions",
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway}},
		ModelIDs: []string{"deepseek-v4-flash"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway},
		},
	}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"deepseek-v4-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,abc"}]}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if upstreamCalled {
		t.Fatal("unsupported image input without routing model should not call upstream")
	}
	if !strings.Contains(w.Body.String(), "unsupported_image_input") {
		t.Fatalf("unsupported image input should return explicit error: %s", w.Body.String())
	}
}

func TestRelayServerProviderGatewayRoutesVisionInputToConfiguredModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamPath string
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"mimo-v2.5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL:            upstream.URL,
		APIKey:             "mimo-key",
		UpstreamModel:      "mimo-v2.5-pro",
		UpstreamModels:     []string{"mimo-v2.5-pro", "mimo-v2.5"},
		WireAPI:            "chat_completions",
		VisionRoutingModel: "mimo-v2.5",
		ModelCapabilities: map[string]providerGatewayModelCapability{
			"mimo-v2.5": {SupportsVision: true},
		},
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway}},
		ModelIDs: []string{"mimo-v2.5-pro", "mimo-v2.5"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway},
		},
	}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"mimo-v2.5-pro","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,abc"}]}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if upstreamPath != "/v1/chat/completions" {
		t.Fatalf("unexpected upstream path: %s", upstreamPath)
	}
	if !strings.Contains(upstreamBody, `"model":"mimo-v2.5"`) || strings.Contains(upstreamBody, `"model":"mimo-v2.5-pro"`) {
		t.Fatalf("vision request should be routed to configured model: %s", upstreamBody)
	}
	if !strings.Contains(upstreamBody, "image_url") {
		t.Fatalf("vision request should keep image input: %s", upstreamBody)
	}
}

func TestRelayServerProviderGatewayRoutesVisionInputToOnlyVisionModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"mimo-v2.5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL:        upstream.URL,
		APIKey:         "mimo-key",
		UpstreamModel:  "mimo-v2.5-pro",
		UpstreamModels: []string{"mimo-v2.5-pro", "mimo-v2.5"},
		WireAPI:        "chat_completions",
		ModelCapabilities: map[string]providerGatewayModelCapability{
			"mimo-v2.5": {SupportsVision: true},
		},
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway}},
		ModelIDs: []string{"mimo-v2.5-pro", "mimo-v2.5"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway},
		},
	}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"mimo-v2.5-pro","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,abc"}]}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(upstreamBody, `"model":"mimo-v2.5"`) || strings.Contains(upstreamBody, `"model":"mimo-v2.5-pro"`) {
		t.Fatalf("single vision model should be used automatically: %s", upstreamBody)
	}
}

func TestRelayServerProviderGatewayAllowsVisionInputForModelCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"qwen-vl-plus","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL:        upstream.URL,
		APIKey:         "qwen-key",
		UpstreamModel:  "qwen-plus",
		UpstreamModels: []string{"qwen-plus", "qwen-vl-plus"},
		WireAPI:        "chat_completions",
		ModelCapabilities: map[string]providerGatewayModelCapability{
			"qwen-vl-plus": {SupportsVision: true},
		},
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway}},
		ModelIDs: []string{"qwen-plus", "qwen-vl-plus"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway},
		},
	}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qwen-vl-plus","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,abc"}]}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !upstreamCalled {
		t.Fatal("vision-capable model should call upstream")
	}
}

func TestRelayServerProviderGatewayAllowsVisionInputForProviderDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"qwen-vl-plus","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	gateway := &providerGatewaySpec{
		BaseURL:        upstream.URL,
		APIKey:         "qwen-key",
		UpstreamModel:  "qwen-vl-plus",
		UpstreamModels: []string{"qwen-vl-plus"},
		WireAPI:        "chat_completions",
		SupportsVision: true,
	}
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway}},
		ModelIDs: []string{"qwen-vl-plus"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "provider_gateway_account_1", Label: "Provider Gateway", Key: "client-key", Enabled: true, ProviderGateway: gateway},
		},
	}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"qwen-vl-plus","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,abc"}]}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(upstreamBody, "Image omitted") || !strings.Contains(upstreamBody, "image_url") {
		t.Fatalf("provider default vision support should keep image input: %s", upstreamBody)
	}
}

func TestRelayServerAcceptsCodexAutoReviewModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{
		response: cliproxyexecutor.Response{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Payload: []byte(`{"ok":true}`),
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"codex-auto-review","input":"allow?","stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 1 || runtime.lastReq.Model != codexAutoReviewModel {
		t.Fatalf("auto review request should be forwarded unchanged: calls=%d req=%#v", runtime.executeCalls, runtime.lastReq)
	}
}

func TestRelayServerModelsExposeCodexAutoReview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := testRelayRouter(&fakeRuntime{})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), codexAutoReviewModel) {
		t.Fatalf("models response should expose auto review model: %s", w.Body.String())
	}
}

func TestRelayServerFramesStreamingChatCompletionThroughRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := make(chan cliproxyexecutor.StreamChunk, 2)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"choices":[]}`)}
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`[DONE]`)}
	close(stream)
	runtime := &fakeRuntime{
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{
				"Content-Type":       []string{"application/json"},
				"Connection":         []string{"X-Remove-Me"},
				"X-Remove-Me":        []string{"secret"},
				"X-Litellm-Trace":    []string{"gateway"},
				"Content-Encoding":   []string{"gzip"},
				"X-Upstream":         []string{"ok"},
				"Access-Control-Foo": []string{"bar"},
			},
			Chunks: stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","messages":[],"stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 0 || runtime.streamCalls != 1 {
		t.Fatalf("unexpected runtime calls: execute=%d stream=%d", runtime.executeCalls, runtime.streamCalls)
	}
	if runtime.lastOpts.SourceFormat != sdktranslator.FormatOpenAI || !runtime.lastOpts.Stream {
		t.Fatalf("unexpected stream options: %#v", runtime.lastOpts)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("unexpected content type: %q", got)
	}
	if values := w.Header().Values("Content-Type"); len(values) != 1 {
		t.Fatalf("Content-Type should not be duplicated: %#v", values)
	}
	if w.Header().Get("X-Upstream") != "ok" {
		t.Fatalf("upstream headers should be preserved")
	}
	if w.Header().Get("X-Remove-Me") != "" ||
		w.Header().Get("X-Litellm-Trace") != "" ||
		w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("filtered upstream headers leaked: %#v", w.Header())
	}
	if got := w.Body.String(); got != "data: {\"choices\":[]}\n\ndata: [DONE]\n\n" {
		t.Fatalf("unexpected framed stream:\n%s", got)
	}
}

func TestRelayServerTimesOutWhenStreamDoesNotOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := streamOpenTimeout
	oldAttempts := streamOpenMaxAttempts
	oldFinalMinimum := streamOpenFinalAttemptMinimum
	streamOpenTimeout = 20 * time.Millisecond
	streamOpenMaxAttempts = 2
	streamOpenFinalAttemptMinimum = 40 * time.Millisecond
	defer func() {
		streamOpenTimeout = oldTimeout
		streamOpenMaxAttempts = oldAttempts
		streamOpenFinalAttemptMinimum = oldFinalMinimum
	}()
	runtime := &fakeRuntime{streamWaitForContext: true}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stream_open") {
		t.Fatalf("timeout response should name stream_open phase: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream_first_byte_timeout") {
		t.Fatalf("timeout response should expose first-byte timeout code: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "after 20ms") {
		t.Fatalf("timeout response should expose the single-attempt timeout: %s", w.Body.String())
	}
	if runtime.streamCalls != 1 {
		t.Fatalf("a first-byte timeout must not resend a possibly accepted request, got %d calls", runtime.streamCalls)
	}
}

func TestRelayServerImmediateSSEResponsesTerminalErrorUsesTypedChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{
		"/v1/responses",
		"/v1/chat/completions/v1/responses",
	} {
		t.Run(path, func(t *testing.T) {
			server := testRelayServer(&fakeRuntime{
				err: relayStatusError{status: http.StatusTooManyRequests, message: "upstream busy"},
			})
			server.manifest.ImmediateSSEResponse = true
			router := server.router()

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
			req.Header.Set("Authorization", "Bearer client-key")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("immediate SSE status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.HasPrefix(body, ": accepted\n\n") || !strings.Contains(body, "event: error\n") {
				t.Fatalf("unexpected immediate SSE body: %s", body)
			}
			dataIndex := strings.LastIndex(body, "data: ")
			if dataIndex < 0 {
				t.Fatalf("missing SSE error data: %s", body)
			}
			data := strings.TrimSpace(body[dataIndex+len("data: "):])
			var payload map[string]any
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				t.Fatalf("unmarshal SSE error: %v; data=%s", err, data)
			}
			if payload["type"] != "error" || payload["code"] != "rate_limit_exceeded" || payload["message"] != "upstream busy" {
				t.Fatalf("unexpected typed SSE error: %#v", payload)
			}
		})
	}
}

func TestRelayServerRefreshesOpenTimeoutAfterAuthFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := streamOpenTimeout
	oldAttempts := streamOpenMaxAttempts
	streamOpenTimeout = 20 * time.Millisecond
	streamOpenMaxAttempts = 1
	defer func() {
		streamOpenTimeout = oldTimeout
		streamOpenMaxAttempts = oldAttempts
	}()

	stream := make(chan cliproxyexecutor.StreamChunk, 1)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`[DONE]`)}
	close(stream)
	runtime := &fakeRuntime{
		streamAuthSelections:   []string{"k12-a", "k12-b"},
		streamAuthSelectionGap: 15 * time.Millisecond,
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Chunks:  stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("auth failover should receive a fresh open timeout, got status=%d body=%s", w.Code, w.Body.String())
	}
	if got := runtime.observedAuthSelections; len(got) != 2 || got[0] != "k12-a" || got[1] != "k12-b" {
		t.Fatalf("unexpected auth selection callbacks: %#v", got)
	}
}

func TestStreamOpenFailoverBudgetCapsExtraWait(t *testing.T) {
	if got := streamOpenFailoverBudget(20 * time.Millisecond); got != 40*time.Millisecond {
		t.Fatalf("short timeout budget = %s", got)
	}
	if got := streamOpenFailoverBudget(2 * time.Minute); got != 3*time.Minute {
		t.Fatalf("long timeout budget = %s", got)
	}
}

func TestStreamOpenAttemptTimeoutUsesFullFirstAndLongFinalTimeouts(t *testing.T) {
	tests := []struct {
		name        string
		openTimeout time.Duration
		attempt     int
		attempts    int
		extendFinal bool
		minimum     time.Duration
		want        time.Duration
	}{
		{name: "healthy text first attempt", openTimeout: 120 * time.Second, attempt: 1, attempts: 2, extendFinal: true, minimum: 5 * time.Minute, want: 120 * time.Second},
		{name: "text final attempt has five minute floor", openTimeout: 120 * time.Second, attempt: 2, attempts: 2, extendFinal: true, minimum: 5 * time.Minute, want: 5 * time.Minute},
		{name: "long text final attempt doubles", openTimeout: 4 * time.Minute, attempt: 2, attempts: 2, extendFinal: true, minimum: 5 * time.Minute, want: 8 * time.Minute},
		{name: "single text attempt stays configured", openTimeout: 120 * time.Second, attempt: 1, attempts: 1, extendFinal: true, minimum: 5 * time.Minute, want: 120 * time.Second},
		{name: "short test final floor is controllable", openTimeout: 20 * time.Millisecond, attempt: 2, attempts: 2, extendFinal: true, minimum: 50 * time.Millisecond, want: 50 * time.Millisecond},
		{name: "image final attempt stays configured", openTimeout: 120 * time.Second, attempt: 2, attempts: 2, extendFinal: false, minimum: 5 * time.Minute, want: 120 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := streamOpenAttemptTimeoutWithMinimum(test.openTimeout, test.attempt, test.attempts, test.extendFinal, test.minimum); got != test.want {
				t.Fatalf("streamOpenAttemptTimeoutWithMinimum() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestStreamOpenTimeoutForSelectedAuthNeverShortensInFlightRequest(t *testing.T) {
	now := time.Now()
	hourlyWindowPresent := true
	weeklyWindowPresent := true
	server := &relayServer{
		manifest: &manifest{
			accountByAuthID: map[string]*accountSpec{
				"healthy-k12.json": {ID: "healthy-k12", AuthID: "healthy-k12.json", PlanType: "K12"},
				"hourly-zero.json": {ID: "hourly-zero", AuthID: "hourly-zero.json", PlanType: "k12"},
				"weekly-zero.json": {ID: "weekly-zero", AuthID: "weekly-zero.json", PlanType: "K12"},
				"plus.json":        {ID: "plus", AuthID: "plus.json", PlanType: "Plus"},
			},
		},
		quota: &quotaReserveStateStore{},
	}
	server.quota.snapshot.Store(quotaReserveRuntimeState{accounts: map[string]quotaReserveSnapshot{
		"healthy-k12": {
			SnapshotUpdatedAtUnixSeconds: int64PointerForTest(now.Unix()),
			HourlyRemainingPercent:       intPointerForTest(100),
			WeeklyRemainingPercent:       intPointerForTest(50),
			HourlyWindowPresent:          &hourlyWindowPresent,
			WeeklyWindowPresent:          &weeklyWindowPresent,
		},
		"hourly-zero": {
			SnapshotUpdatedAtUnixSeconds: int64PointerForTest(now.Unix()),
			HourlyRemainingPercent:       intPointerForTest(0),
			WeeklyRemainingPercent:       intPointerForTest(50),
			HourlyWindowPresent:          &hourlyWindowPresent,
			WeeklyWindowPresent:          &weeklyWindowPresent,
		},
		"weekly-zero": {
			SnapshotUpdatedAtUnixSeconds: int64PointerForTest(now.Unix()),
			HourlyRemainingPercent:       intPointerForTest(100),
			WeeklyRemainingPercent:       intPointerForTest(0),
			HourlyWindowPresent:          &hourlyWindowPresent,
			WeeklyWindowPresent:          &weeklyWindowPresent,
		},
	}})

	tests := []struct {
		name          string
		authID        string
		quotaAdaptive bool
		want          time.Duration
	}{
		{name: "healthy K12 keeps full timeout", authID: "healthy-k12.json", quotaAdaptive: true, want: 120 * time.Second},
		{name: "Plus keeps full timeout", authID: "plus.json", quotaAdaptive: true, want: 120 * time.Second},
		{name: "hourly depleted K12 keeps full timeout", authID: "hourly-zero.json", quotaAdaptive: true, want: 120 * time.Second},
		{name: "weekly depleted K12 keeps full timeout", authID: "weekly-zero.json", quotaAdaptive: true, want: 120 * time.Second},
		{name: "image request ignores depleted K12", authID: "hourly-zero.json", quotaAdaptive: false, want: 120 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := server.streamOpenTimeoutForSelectedAuth(120*time.Second, test.authID, test.quotaAdaptive, now); got != test.want {
				t.Fatalf("streamOpenTimeoutForSelectedAuth() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestStreamTimeoutProfileKeepsImagesUnchangedAndExtendsFinalTextAttempt(t *testing.T) {
	server := &relayServer{cfg: &config.Config{}}
	server.cfg.Streaming.ImageStreamOpenTimeoutMS = 120

	textRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	server.cfg.Streaming.StreamOpenTimeoutMS = 120_000
	textProfile := server.streamTimeoutsForRequest(textRequest, []byte(`{"input":"hello"}`), "gpt-5.5")
	if textProfile.open != 120*time.Second || !textProfile.quotaAdaptiveOpen {
		t.Fatalf("unexpected configured text timeout profile: %#v", textProfile)
	}
	if textProfile.total != 0 {
		t.Fatalf("default stream total timeout = %s, want disabled", textProfile.total)
	}
	if got := streamOpenAttemptTimeout(textProfile.open, 1, 2, textProfile.quotaAdaptiveOpen); got != 120*time.Second {
		t.Fatalf("configured text first-attempt timeout = %s, want 120s", got)
	}
	if got := streamOpenAttemptTimeout(textProfile.open, 2, 2, textProfile.quotaAdaptiveOpen); got != 5*time.Minute {
		t.Fatalf("configured text final-attempt timeout = %s, want 5m", got)
	}

	server.cfg.Streaming.StreamOpenTimeoutMS = 80
	shortProfile := server.streamTimeoutsForRequest(textRequest, []byte(`{"input":"hello"}`), "gpt-5.5")
	if got := streamOpenAttemptTimeout(shortProfile.open, 1, 2, shortProfile.quotaAdaptiveOpen); got != 80*time.Millisecond {
		t.Fatalf("short configured text first-attempt timeout = %s, want 80ms", got)
	}

	imageRequest := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	imageProfile := server.streamTimeoutsForRequest(imageRequest, []byte(`{"prompt":"draw"}`), "gpt-image-2")
	if imageProfile.open != 120*time.Millisecond || imageProfile.quotaAdaptiveOpen {
		t.Fatalf("unexpected configured image timeout profile: %#v", imageProfile)
	}
	if got := streamOpenAttemptTimeout(imageProfile.open, 1, 2, imageProfile.quotaAdaptiveOpen); got != 120*time.Millisecond {
		t.Fatalf("configured image first-attempt timeout = %s, want 120ms", got)
	}
	if got := streamOpenAttemptTimeout(imageProfile.open, 2, 2, imageProfile.quotaAdaptiveOpen); got != 120*time.Millisecond {
		t.Fatalf("configured image final-attempt timeout = %s, want 120ms", got)
	}
}

func TestStreamTotalTimeoutIsIndependentAndOptional(t *testing.T) {
	server := &relayServer{cfg: &config.Config{}}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	profile := server.streamTimeoutsForRequest(request, []byte(`{"input":"hello"}`), "gpt-5.5")
	if profile.total != 0 {
		t.Fatalf("default total timeout = %s, want disabled", profile.total)
	}

	server.cfg.Streaming.StreamOpenTimeoutMS = 180_000
	server.cfg.Streaming.StreamIdleTimeoutMS = 120_000
	server.cfg.Streaming.StreamTotalTimeoutMS = 45
	profile = server.streamTimeoutsForRequest(request, []byte(`{"input":"hello"}`), "gpt-5.5")
	if profile.open != 180*time.Second || profile.idle != 120*time.Second || profile.total != 45*time.Millisecond {
		t.Fatalf("timeouts are not independent: %#v", profile)
	}

	ctx, cancel := streamContextWithTotalTimeout(context.Background(), profile.total)
	defer cancel()
	select {
	case <-ctx.Done():
		var timeoutErr relayTimeoutError
		if !errors.As(context.Cause(ctx), &timeoutErr) || timeoutErr.phase != "stream_total" {
			t.Fatalf("total timeout cause = %v, want stream_total", context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("configured stream total timeout did not fire")
	}
}

func TestRelayServerStreamTotalTimeoutCancelsWithoutRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := make(chan cliproxyexecutor.StreamChunk)
	runtime := &fakeRuntime{
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": {"text/event-stream"}},
			Chunks:  stream,
		},
	}
	server := testRelayServer(runtime)
	server.cfg.Streaming.StreamOpenTimeoutMS = 1_000
	server.cfg.Streaming.StreamIdleTimeoutMS = 1_000
	server.cfg.Streaming.StreamTotalTimeoutMS = 40
	router := server.router()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	startedAt := time.Now()
	router.ServeHTTP(w, req)

	if elapsed := time.Since(startedAt); elapsed < 30*time.Millisecond || elapsed > time.Second {
		t.Fatalf("stream total timeout elapsed = %s", elapsed)
	}
	if runtime.streamCalls != 1 {
		t.Fatalf("stream total timeout sent %d attempts, want 1", runtime.streamCalls)
	}
	if !strings.Contains(w.Body.String(), "stream_total") {
		t.Fatalf("missing stream total timeout response: %s", w.Body.String())
	}
}

func TestRelayServerHealthyK12LongFirstByteUsesFullConfiguredTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := streamOpenTimeout
	oldAttempts := streamOpenMaxAttempts
	streamOpenTimeout = 80 * time.Millisecond
	streamOpenMaxAttempts = 2
	defer func() {
		streamOpenTimeout = oldTimeout
		streamOpenMaxAttempts = oldAttempts
	}()

	stream := make(chan cliproxyexecutor.StreamChunk, 1)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`[DONE]`)}
	close(stream)
	runtime := &fakeRuntime{
		streamAuthSelections: []string{"healthy-k12.json"},
		streamOpenDelay:      50 * time.Millisecond,
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Chunks:  stream,
		},
	}
	server := testRelayServer(runtime)
	account := &accountSpec{ID: "healthy-k12", AuthID: "healthy-k12.json", PlanType: "K12"}
	server.manifest.Accounts = []accountSpec{*account}
	server.manifest.accountByAuthID = map[string]*accountSpec{"healthy-k12.json": account}
	server.manifest.accountByID = map[string]*accountSpec{"healthy-k12": account}
	hourlyWindowPresent := true
	weeklyWindowPresent := true
	server.quota = &quotaReserveStateStore{}
	server.quota.snapshot.Store(quotaReserveRuntimeState{accounts: map[string]quotaReserveSnapshot{
		"healthy-k12": {
			SnapshotUpdatedAtUnixSeconds: int64PointerForTest(time.Now().Unix()),
			HourlyRemainingPercent:       intPointerForTest(100),
			WeeklyRemainingPercent:       intPointerForTest(50),
			HourlyWindowPresent:          &hourlyWindowPresent,
			WeeklyWindowPresent:          &weeklyWindowPresent,
		},
	}})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("healthy K12 long first byte should stay within the full timeout, got status=%d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 {
		t.Fatalf("healthy K12 should not be killed by a short first probe, got %d attempts", runtime.streamCalls)
	}
}

func TestRelayServerUsesLongOpenTimeoutForImageGenerationTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldOpenTimeout := streamOpenTimeout
	oldImageOpenTimeout := imageStreamOpenTimeout
	oldAttempts := streamOpenMaxAttempts
	streamOpenTimeout = 20 * time.Millisecond
	imageStreamOpenTimeout = 120 * time.Millisecond
	streamOpenMaxAttempts = 2
	defer func() {
		streamOpenTimeout = oldOpenTimeout
		imageStreamOpenTimeout = oldImageOpenTimeout
		streamOpenMaxAttempts = oldAttempts
	}()
	stream := make(chan cliproxyexecutor.StreamChunk, 1)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`event: response.completed
data: {"type":"response.completed"}

`)}
	close(stream)
	runtime := &fakeRuntime{
		streamOpenDelay: 60 * time.Millisecond,
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Chunks:  stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"draw","stream":true,"tools":[{"type":"image_generation","model":"gpt-image-2"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("image stream should use longer open timeout, got status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 {
		t.Fatalf("expected one stream runtime call, got %d", runtime.streamCalls)
	}
	if !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("image stream response was not forwarded: %s", w.Body.String())
	}
}

func TestRelayServerHandlesImagesGenerationsEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := make(chan cliproxyexecutor.StreamChunk, 1)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`event: response.completed
data: {"type":"response.completed","response":{"created_at":1710000000,"output":[{"type":"image_generation_call","result":"ZmFrZS1wbmc=","output_format":"png","size":"1024x1024"}]}}

`)}
	close(stream)
	runtime := &fakeRuntime{
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Chunks:  stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"draw","response_format":"b64_json"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 || runtime.executeCalls != 0 {
		t.Fatalf("unexpected runtime calls: execute=%d stream=%d", runtime.executeCalls, runtime.streamCalls)
	}
	if runtime.lastReq.Model != defaultImagesMainModel {
		t.Fatalf("image endpoint should execute via main model, got %q", runtime.lastReq.Model)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response should be json: %v body=%s", err, w.Body.String())
	}
	data, _ := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected one image result: %#v", body)
	}
	first, _ := data[0].(map[string]any)
	if first["b64_json"] != "ZmFrZS1wbmc=" {
		t.Fatalf("unexpected image payload: %#v", body)
	}
}

func TestRelayServerDoesNotRetryWhenStreamDoesNotOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := streamOpenTimeout
	oldAttempts := streamOpenMaxAttempts
	streamOpenTimeout = 20 * time.Millisecond
	streamOpenMaxAttempts = 2
	defer func() {
		streamOpenTimeout = oldTimeout
		streamOpenMaxAttempts = oldAttempts
	}()
	runtime := &fakeRuntime{
		streamWaitForContext: true,
		streamAuthSelections: []string{"auth-a"},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 {
		t.Fatalf("first-byte timeout must not retry a sent request, got %d calls", runtime.streamCalls)
	}
	if len(runtime.streamCancelCauses) != 1 {
		t.Fatalf("expected one canceled attempt cause, got %#v", runtime.streamCancelCauses)
	}
	if len(runtime.reportedFailures) != 0 {
		t.Fatalf("timeout must not globally penalize or blacklist the selected auth: %#v", runtime.reportedFailures)
	}
	if len(runtime.streamOpts) != 1 {
		t.Fatalf("expected options for one outer attempt, got %d", len(runtime.streamOpts))
	}
	statusCause, ok := runtime.streamCancelCauses[0].(interface{ StatusCode() int })
	if !ok || statusCause.StatusCode() != http.StatusGatewayTimeout ||
		!strings.Contains(runtime.streamCancelCauses[0].Error(), "stream_open attempt=1/2") ||
		!strings.Contains(runtime.streamCancelCauses[0].Error(), "after 20ms") {
		t.Fatalf("timeout cancel cause should be a typed stream-open 504: %#v", runtime.streamCancelCauses[0])
	}
}

func TestRelayServerRetriesOnlyExplicitPreSendFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldAttempts := streamOpenMaxAttempts
	streamOpenMaxAttempts = 2
	defer func() { streamOpenMaxAttempts = oldAttempts }()

	stream := make(chan cliproxyexecutor.StreamChunk, 1)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`[DONE]`)}
	close(stream)
	runtime := &fakeRuntime{
		streamAuthsByAttempt: [][]string{{"auth-a"}, {"auth-b"}},
		streamErrors: []error{
			cliproxyexecutor.WrapUpstreamAttemptError(errors.New("dial failed"), 0, false, false),
		},
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Chunks:  stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK || runtime.streamCalls != 2 {
		t.Fatalf("explicit pre-send failure should retry once: status=%d calls=%d body=%s", w.Code, runtime.streamCalls, w.Body.String())
	}
	excluded, ok := runtime.streamOpts[1].Metadata[cliproxyexecutor.ExcludedAuthIDsMetadataKey].([]string)
	if !ok || len(excluded) != 1 || excluded[0] != "auth-a" {
		t.Fatalf("safe retry should exclude the failed auth: %#v", runtime.streamOpts[1].Metadata[cliproxyexecutor.ExcludedAuthIDsMetadataKey])
	}
}

func TestRelayServerDoesNotRetryPostSendEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldAttempts := streamOpenMaxAttempts
	streamOpenMaxAttempts = 2
	defer func() { streamOpenMaxAttempts = oldAttempts }()

	runtime := &fakeRuntime{
		streamAuthsByAttempt: [][]string{{"auth-a"}, {"auth-b"}},
		streamErrors: []error{
			cliproxyexecutor.WrapUpstreamAttemptError(io.ErrUnexpectedEOF, 128, false, true),
		},
	}
	router := testRelayRouter(runtime)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway || runtime.streamCalls != 1 {
		t.Fatalf("post-send EOF must be returned without replay: status=%d calls=%d body=%s", w.Code, runtime.streamCalls, w.Body.String())
	}
}

func TestRelayServerTimeoutDoesNotInvokeSelectionFailureReporter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := streamOpenTimeout
	oldAttempts := streamOpenMaxAttempts
	streamOpenTimeout = 20 * time.Millisecond
	streamOpenMaxAttempts = 2
	defer func() {
		streamOpenTimeout = oldTimeout
		streamOpenMaxAttempts = oldAttempts
	}()

	reportBlock := make(chan struct{})
	runtime := &fakeRuntime{
		streamWaitForContext: true,
		streamAuthSelections: []string{"auth-a"},
		reportFailureBlock:   reportBlock,
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	startedAt := time.Now()
	router.ServeHTTP(w, req)
	elapsed := time.Since(startedAt)

	if w.Code != http.StatusGatewayTimeout || runtime.streamCalls != 1 {
		t.Fatalf("timeout should stop after one attempt: status=%d calls=%d body=%s", w.Code, runtime.streamCalls, w.Body.String())
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("blocked reporter delayed failover for %s", elapsed)
	}
	if len(runtime.reportedFailures) != 0 {
		t.Fatalf("timeout unexpectedly called selection failure reporter: %#v", runtime.reportedFailures)
	}
	close(reportBlock)
}

func TestRelayServerKeepsStreamContextOpenAfterOpen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldOpenTimeout := streamOpenTimeout
	oldIdleTimeout := streamIdleTimeout
	streamOpenTimeout = 100 * time.Millisecond
	streamIdleTimeout = time.Second
	defer func() {
		streamOpenTimeout = oldOpenTimeout
		streamIdleTimeout = oldIdleTimeout
	}()
	runtime := &fakeRuntime{
		streamResultFromContext: true,
		streamResultDelay:       20 * time.Millisecond,
		streamResultPayload:     []byte(`[DONE]`),
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 {
		t.Fatalf("expected one stream runtime call, got %d", runtime.streamCalls)
	}
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("stream context should stay alive after opening: %s", w.Body.String())
	}
}

func TestRelayServerTimesOutIdleOpenedStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := streamIdleTimeout
	streamIdleTimeout = 20 * time.Millisecond
	defer func() {
		streamIdleTimeout = oldTimeout
	}()
	streamContextDone := make(chan error, 1)
	runtime := &fakeRuntime{
		streamResultFromContext: true,
		streamResultDelay:       time.Second,
		streamContextDone:       streamContextDone,
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("stream should be opened before idle timeout, got status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stream_idle") {
		t.Fatalf("idle timeout should be sent as terminal SSE error: %s", w.Body.String())
	}
	if runtime.streamCalls != 1 {
		t.Fatalf("stream idle timeout sent %d attempts, want 1", runtime.streamCalls)
	}
	select {
	case cause := <-streamContextDone:
		var timeoutErr relayTimeoutError
		if !errors.As(cause, &timeoutErr) || timeoutErr.phase != "stream_idle" {
			t.Fatalf("upstream cancel cause = %v, want stream_idle timeout", cause)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream stream context did not receive idle timeout cancellation")
	}
}

func TestRelayServerStreamChunkErrorDoesNotRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := make(chan cliproxyexecutor.StreamChunk, 1)
	stream <- cliproxyexecutor.StreamChunk{Err: errors.New("chunk read failed")}
	close(stream)
	runtime := &fakeRuntime{
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Chunks:  stream,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("stream should retain opened HTTP status, got %d: %s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 {
		t.Fatalf("stream chunk error sent %d attempts, want 1", runtime.streamCalls)
	}
	if !strings.Contains(w.Body.String(), "chunk read failed") {
		t.Fatalf("chunk error should be sent as terminal SSE error: %s", w.Body.String())
	}
}

func TestRelayServerAnthropicMessagesUsesClaudeFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{
		response: cliproxyexecutor.Response{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Payload: []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`),
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 1 || runtime.lastOpts.SourceFormat != sdktranslator.FormatClaude || runtime.lastReq.Format != sdktranslator.FormatClaude {
		t.Fatalf("expected Claude executor request, got calls=%d req=%#v opts=%#v", runtime.executeCalls, runtime.lastReq, runtime.lastOpts)
	}
}

func TestRelayServerAnthropicCountTokensUsesClaudeShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := testRelayRouter(&fakeRuntime{})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello world"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"input_tokens"`) {
		t.Fatalf("Anthropic token count response should use input_tokens: %s", w.Body.String())
	}
}

func TestRelayServerGeminiGenerateInjectsPathModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{
		response: cliproxyexecutor.Response{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Payload: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}`),
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gpt-5.5:generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 1 || runtime.lastOpts.SourceFormat != sdktranslator.FormatGemini || runtime.lastReq.Model != "gpt-5.5" {
		t.Fatalf("expected Gemini executor request, got calls=%d req=%#v opts=%#v", runtime.executeCalls, runtime.lastReq, runtime.lastOpts)
	}
	if !strings.Contains(string(runtime.lastReq.Payload), `"model":"gpt-5.5"`) {
		t.Fatalf("Gemini path model should be injected into executor payload: %s", runtime.lastReq.Payload)
	}
}

func TestRelayServerGeminiModelsResponseShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := testRelayRouter(&fakeRuntime{})

	req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"name":"models/gpt-5.5"`) ||
		!strings.Contains(w.Body.String(), `"streamGenerateContent"`) ||
		!strings.Contains(w.Body.String(), `"countTokens"`) {
		t.Fatalf("Gemini models response has unexpected shape: %s", w.Body.String())
	}
}

func TestRelayServerOllamaChatConvertsNonStreamingResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtime := &fakeRuntime{
		response: cliproxyexecutor.Response{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Payload: []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"gpt-5.5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`),
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.executeCalls != 1 || runtime.lastOpts.SourceFormat != sdktranslator.FormatOpenAI || runtime.lastReq.Model != "gpt-5.5" {
		t.Fatalf("expected OpenAI chat executor request, got calls=%d req=%#v opts=%#v", runtime.executeCalls, runtime.lastReq, runtime.lastOpts)
	}
	if !strings.Contains(w.Body.String(), `"done":true`) || !strings.Contains(w.Body.String(), `"content":"ok"`) || !strings.Contains(w.Body.String(), `"eval_count":3`) {
		t.Fatalf("Ollama response has unexpected shape: %s", w.Body.String())
	}
}

func TestRelayServerOllamaChatConvertsStreamingChunks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.5","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`)}
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)}
	close(chunks)
	runtime := &fakeRuntime{
		streamResult: &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
			Chunks:  chunks,
		},
	}
	router := testRelayRouter(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	if runtime.streamCalls != 1 || runtime.lastOpts.SourceFormat != sdktranslator.FormatOpenAI {
		t.Fatalf("expected OpenAI chat stream executor request, got calls=%d opts=%#v", runtime.streamCalls, runtime.lastOpts)
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected content and final Ollama chunks, got %d lines: %s", len(lines), w.Body.String())
	}
	if !strings.Contains(lines[0], `"content":"ok"`) || !strings.Contains(lines[1], `"done":true`) || !strings.Contains(lines[1], `"eval_count":3`) {
		t.Fatalf("unexpected Ollama stream body: %s", w.Body.String())
	}
}

func TestRelayServerHandlesCORSPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := testRelayRouter(&fakeRuntime{})

	req := httptest.NewRequest(http.MethodOptions, "/v1/responses", nil)
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" ||
		w.Header().Get("Access-Control-Allow-Headers") != "*" {
		t.Fatalf("unexpected CORS headers: %#v", w.Header())
	}
}

func testRelayServer(runtime executorRuntime) *relayServer {
	m := &manifest{
		APIKeys:  []apiKeySpec{{ID: "key_1", Label: "Test key", Key: "client-key", Enabled: true}},
		ModelIDs: []string{"gpt-5.5", "gpt-image-2"},
		apiKeyByValue: map[string]*apiKeySpec{
			"client-key": {ID: "key_1", Label: "Test key", Key: "client-key", Enabled: true},
		},
	}
	policy := &requestPolicy{manifest: m}
	return &relayServer{
		runtime:  runtime,
		cfg:      &config.Config{},
		manifest: m,
		policy:   policy,
	}
}

func testRelayRouter(runtime executorRuntime) *gin.Engine {
	return testRelayServer(runtime).router()
}

type fakeRuntime struct {
	response                cliproxyexecutor.Response
	streamResult            *cliproxyexecutor.StreamResult
	err                     error
	streamWaitForContext    bool
	streamWaitAttempts      int
	streamResultFromContext bool
	streamOpenDelay         time.Duration
	streamResultDelay       time.Duration
	streamResultPayload     []byte
	streamContextDone       chan<- error
	streamCancelCauses      []error
	streamAuthSelections    []string
	streamAuthsByAttempt    [][]string
	streamAuthSelectionGap  time.Duration
	streamErrors            []error
	observedAuthSelections  []string
	streamOpts              []cliproxyexecutor.Options
	reportMu                sync.Mutex
	reportFailureBlock      <-chan struct{}
	reportFailureReturned   chan<- struct{}
	reportedFailures        []reportedSelectionFailure

	executeCalls int
	streamCalls  int
	lastReq      cliproxyexecutor.Request
	lastOpts     cliproxyexecutor.Options

	alphaSearchStatus  int
	alphaSearchHeaders http.Header
	alphaSearchPayload []byte
	alphaSearchErr     error
	alphaSearchCalls   int
	lastAlphaModel     string
	lastAlphaBody      []byte
	lastAlphaHeaders   http.Header
}

func (r *fakeRuntime) Execute(_ context.Context, _ []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	r.executeCalls++
	r.lastReq = req
	r.lastOpts = opts
	return r.response, r.err
}

func (r *fakeRuntime) CodexAlphaSearch(_ context.Context, model string, body []byte, headers http.Header) (int, http.Header, []byte, error) {
	r.alphaSearchCalls++
	r.lastAlphaModel = model
	r.lastAlphaBody = append([]byte(nil), body...)
	if headers != nil {
		r.lastAlphaHeaders = headers.Clone()
	}
	status := r.alphaSearchStatus
	if status == 0 {
		status = http.StatusOK
	}
	payload := r.alphaSearchPayload
	if payload == nil {
		payload = []byte(`{"ok":true}`)
	}
	return status, r.alphaSearchHeaders, payload, r.alphaSearchErr
}

func (r *fakeRuntime) ExecuteStream(ctx context.Context, _ []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	r.streamCalls++
	call := r.streamCalls
	r.lastReq = req
	r.lastOpts = opts
	r.streamOpts = append(r.streamOpts, opts)
	selections := r.streamAuthSelections
	if call <= len(r.streamAuthsByAttempt) {
		selections = r.streamAuthsByAttempt[call-1]
	}
	for index, authID := range selections {
		selection := cliproxyexecutor.AuthSelection{AuthID: authID, AttemptID: uint64(call*100 + index + 1)}
		switch callback := opts.Metadata[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(type) {
		case func(cliproxyexecutor.AuthSelection):
			if callback != nil {
				callback(selection)
			}
		case func(string):
			if callback != nil {
				callback(authID)
			}
		}
		if strings.TrimSpace(authID) != "" {
			r.observedAuthSelections = append(r.observedAuthSelections, authID)
		}
		if r.streamAuthSelectionGap <= 0 {
			continue
		}
		timer := time.NewTimer(r.streamAuthSelectionGap)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if call <= len(r.streamErrors) && r.streamErrors[call-1] != nil {
		return nil, r.streamErrors[call-1]
	}
	if r.streamWaitForContext || call <= r.streamWaitAttempts {
		<-ctx.Done()
		r.streamCancelCauses = append(r.streamCancelCauses, context.Cause(ctx))
		return nil, ctx.Err()
	}
	if r.streamOpenDelay > 0 {
		timer := time.NewTimer(r.streamOpenDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if r.streamResultFromContext {
		stream := make(chan cliproxyexecutor.StreamChunk, 1)
		delay := r.streamResultDelay
		if delay <= 0 {
			delay = 10 * time.Millisecond
		}
		payload := r.streamResultPayload
		if len(payload) == 0 {
			payload = []byte(`[DONE]`)
		}
		go func() {
			defer close(stream)
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				if r.streamContextDone != nil {
					r.streamContextDone <- context.Cause(ctx)
				}
				return
			case <-timer.C:
				stream <- cliproxyexecutor.StreamChunk{Payload: payload}
			}
		}()
		return &cliproxyexecutor.StreamResult{
			Headers: http.Header{"Content-Type": []string{"application/json"}},
			Chunks:  stream,
		}, nil
	}
	return r.streamResult, r.err
}

type reportedSelectionFailure struct {
	selection cliproxyexecutor.AuthSelection
	model     string
	err       error
	opts      cliproxyexecutor.Options
}

func (r *fakeRuntime) ReportSelectionFailure(_ context.Context, selection cliproxyexecutor.AuthSelection, model string, executionErr error, opts cliproxyexecutor.Options) coreauth.SelectionResultDirective {
	r.reportMu.Lock()
	r.reportedFailures = append(r.reportedFailures, reportedSelectionFailure{
		selection: selection,
		model:     model,
		err:       executionErr,
		opts:      opts,
	})
	r.reportMu.Unlock()
	if r.reportFailureBlock != nil {
		<-r.reportFailureBlock
	}
	if r.reportFailureReturned != nil {
		r.reportFailureReturned <- struct{}{}
	}
	return coreauth.SelectionResultDirective{SuppressAvailabilityUpdate: true, StopAuthAttempt: true}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = old
		_ = reader.Close()
	}()

	fn()
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	return string(data)
}

func TestRelayAcceptsResponsesPathAppendedToChatCompletionsBase(t *testing.T) {
	t.Parallel()
	// Route registration only: ensure compatibility paths are not NoRoute 404.
	m := &manifest{}
	policy := &requestPolicy{manifest: m}
	router := (&relayServer{
		manifest: m,
		policy:   policy,
	}).router()
	for _, path := range []string{
		"/v1/chat/completions/v1/responses",
		"/v1/chat/completions/v1/responses/compact",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer unused")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code == http.StatusNotFound {
			t.Fatalf("path %s should not be NoRoute 404 (got %d body=%s)", path, w.Code, w.Body.String())
		}
	}
}

func TestSanitizeCodexAlphaSearchBodyRemovesLocalRoutingFields(t *testing.T) {
	t.Parallel()
	body := []byte(`{"query":"hello","prompt_cache_key":"drop-me","prompt_cache_retention":"24h","id":"sess-1"}`)
	out := sanitizeCodexAlphaSearchBody(body)
	if strings.Contains(string(out), "prompt_cache_key") || strings.Contains(string(out), "prompt_cache_retention") {
		t.Fatalf("local routing fields survived: %s", out)
	}
	if !strings.Contains(string(out), `"query":"hello"`) || !strings.Contains(string(out), `"id":"sess-1"`) {
		t.Fatalf("expected search fields preserved: %s", out)
	}
}

func TestResolveCodexAlphaSearchURL(t *testing.T) {
	t.Parallel()
	if got := resolveCodexAlphaSearchURL(nil); got != defaultCodexAlphaSearchURL {
		t.Fatalf("nil auth = %q, want default", got)
	}
	auth := &coreauth.Auth{Attributes: map[string]string{"base_url": "https://example.test/backend-api/codex/"}}
	if got := resolveCodexAlphaSearchURL(auth); got != "https://example.test/backend-api/codex/alpha/search" {
		t.Fatalf("codex base = %q", got)
	}
	auth.Attributes["base_url"] = "https://example.test/backend-api"
	if got := resolveCodexAlphaSearchURL(auth); got != "https://example.test/backend-api/codex/alpha/search" {
		t.Fatalf("backend-api base = %q", got)
	}
}

func TestRequestKindFromPathTreatsAlphaSearchAsText(t *testing.T) {
	t.Parallel()
	if got := requestKindFromPath("/v1/alpha/search"); got != "text" {
		t.Fatalf("requestKindFromPath(/v1/alpha/search) = %q, want text", got)
	}
	if got := requestKindFromPath("/backend-api/codex/alpha/search"); got != "text" {
		t.Fatalf("requestKindFromPath(direct) = %q, want text", got)
	}
}

func TestCodexAlphaSearchRouteForwardsToRuntime(t *testing.T) {
	t.Parallel()
	runtime := &fakeRuntime{
		alphaSearchStatus:  http.StatusOK,
		alphaSearchPayload: []byte(`{"results":[{"title":"ok"}]}`),
		alphaSearchHeaders: http.Header{"Content-Type": []string{"application/json"}},
	}
	m := &manifest{
		APIKeys: []apiKeySpec{{
			ID:      "key_1",
			Label:   "Test key",
			Key:     "client-key",
			Enabled: true,
		}},
		ModelIDs: []string{"gpt-5.6-sol"},
	}
	m.apiKeyByValue = map[string]*apiKeySpec{
		"client-key": &m.APIKeys[0],
	}
	router := (&relayServer{
		runtime:  runtime,
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	body := `{"query":"OpenAI Codex authentication documentation","model":"gpt-5.6-sol","id":"sess-42"}`
	req := httptest.NewRequest(http.MethodPost, codexAlphaSearchPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Openai-Actor-Authorization", "actor-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if runtime.alphaSearchCalls != 1 {
		t.Fatalf("alphaSearchCalls = %d, want 1", runtime.alphaSearchCalls)
	}
	if runtime.lastAlphaModel != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want gpt-5.6-sol", runtime.lastAlphaModel)
	}
	if !strings.Contains(string(runtime.lastAlphaBody), `"query":"OpenAI Codex authentication documentation"`) {
		t.Fatalf("body not forwarded: %s", runtime.lastAlphaBody)
	}
	if got := runtime.lastAlphaHeaders.Get("X-Session-ID"); got != "sess-42" {
		t.Fatalf("X-Session-ID = %q, want sess-42", got)
	}
	if got := runtime.lastAlphaHeaders.Get("X-Openai-Actor-Authorization"); got != "actor-token" {
		t.Fatalf("actor header = %q", got)
	}
	if !strings.Contains(w.Body.String(), `"results"`) {
		t.Fatalf("response body missing results: %s", w.Body.String())
	}
}

func TestCodexAlphaSearchDirectPathIsRegistered(t *testing.T) {
	t.Parallel()
	runtime := &fakeRuntime{alphaSearchPayload: []byte(`{"ok":true}`)}
	m := &manifest{
		APIKeys: []apiKeySpec{{ID: "key_1", Key: "client-key", Enabled: true}},
	}
	m.apiKeyByValue = map[string]*apiKeySpec{"client-key": &m.APIKeys[0]}
	router := (&relayServer{
		runtime:  runtime,
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodPost, codexDirectAlphaSearchPath, strings.NewReader(`{"query":"ping"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Fatalf("direct path should not be NoRoute 404: %s", w.Body.String())
	}
	if runtime.alphaSearchCalls != 1 {
		t.Fatalf("alphaSearchCalls = %d, want 1", runtime.alphaSearchCalls)
	}
}

func TestCodexAlphaSearchRequiresAPIKey(t *testing.T) {
	t.Parallel()
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		manifest: &manifest{},
		policy:   &requestPolicy{manifest: &manifest{}},
	}).router()
	req := httptest.NewRequest(http.MethodPost, codexAlphaSearchPath, strings.NewReader(`{"query":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 body=%s", w.Code, w.Body.String())
	}
}

func TestResponsesWebsocketRouteRequiresAPIKey(t *testing.T) {
	t.Parallel()
	called := false
	router := (&relayServer{
		runtime: &fakeRuntime{},
		manifest: &manifest{
			APIKeys: []apiKeySpec{{ID: "key_1", Key: "client-key", Enabled: true}},
			apiKeyByValue: map[string]*apiKeySpec{
				"client-key": {ID: "key_1", Key: "client-key", Enabled: true},
			},
		},
		policy: &requestPolicy{
			manifest: &manifest{
				APIKeys: []apiKeySpec{{ID: "key_1", Key: "client-key", Enabled: true}},
				apiKeyByValue: map[string]*apiKeySpec{
					"client-key": {ID: "key_1", Key: "client-key", Enabled: true},
				},
			},
		},
		responsesWebsocket: func(c *gin.Context) {
			called = true
			c.Status(http.StatusSwitchingProtocols)
		},
	}).router()

	// Missing key → 401, handler not invoked.
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want 401 body=%s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("websocket handler should not run without API key")
	}

	// Valid key → handler runs.
	req = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if !called {
		t.Fatal("websocket handler should run with valid API key")
	}
	if w.Code != http.StatusSwitchingProtocols {
		t.Fatalf("valid key status = %d, want 101 body=%s", w.Code, w.Body.String())
	}
}

func TestResponsesWebsocketRouteUnavailableWithoutHandler(t *testing.T) {
	t.Parallel()
	m := &manifest{
		APIKeys: []apiKeySpec{{ID: "key_1", Key: "client-key", Enabled: true}},
	}
	m.apiKeyByValue = map[string]*apiKeySpec{"client-key": &m.APIKeys[0]}
	router := (&relayServer{
		runtime:  &fakeRuntime{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "responses websocket unavailable") {
		t.Fatalf("body = %s", w.Body.String())
	}
}
