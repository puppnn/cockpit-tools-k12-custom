package auth

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManagerExecuteStreamExcludesAuthIDsFromOptions(t *testing.T) {
	t.Parallel()

	const model = "excluded-auth-retry-model"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	auths := []*Auth{
		{ID: "excluded-auth-retry-a", Provider: "codex"},
		{ID: "excluded-auth-retry-b", Provider: "codex"},
	}
	modelRegistry := registry.GetGlobalRegistry()
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
		modelRegistry.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		authID := auth.ID
		t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	}

	executor := &k12FailoverExecutor{}
	manager.RegisterExecutor(executor)
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.ExcludedAuthIDsMetadataKey: []string{auths[0].ID},
	}}
	stream, err := manager.ExecuteStream(
		context.Background(),
		[]string{"codex"},
		cliproxyexecutor.Request{Model: model},
		opts,
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
	if len(executor.streamCalls) != 1 || executor.streamCalls[0] != auths[1].ID {
		t.Fatalf("stream auth calls = %#v, want only %q", executor.streamCalls, auths[1].ID)
	}
}

func TestK12SynchronousStreamOpenTimeoutAllowsExcludedRetryAndIgnoresLateResult(t *testing.T) {
	t.Parallel()

	selector := newTestK12Selector(
		t,
		filepath.Join(t.TempDir(), "state.json"),
		map[string]*int{"k12-a": intPtr(20), "k12-b": intPtr(20)},
	)
	k12A := testK12Auth("k12-a")
	k12B := testK12Auth("k12-b")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12A, k12B, plus}
	baseOpts := promptCacheOptions("synchronous-stream-open-timeout")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || selected == nil || selected.ID != k12A.ID {
		t.Fatalf("initial K12 selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: selected.ID, Success: true}, baseOpts)

	timeoutOpts := withSelectionAttempt(baseOpts)
	selected, err = selector.Pick(context.Background(), "codex", "gpt-5", timeoutOpts, auths)
	if err != nil || selected == nil || selected.ID != k12A.ID {
		t.Fatalf("active K12 selection = %#v, %v", selected, err)
	}
	timeoutResult := Result{
		AuthID:  selected.ID,
		Success: false,
		Error: &Error{
			HTTPStatus: http.StatusGatewayTimeout,
			Message:    "upstream timed out in stream_open attempt=1/2",
		},
	}
	directive := selector.OnSelectionResult(context.Background(), timeoutResult, timeoutOpts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("synchronous timeout directive = %#v", directive)
	}

	identity, _ := extractSessionIdentities(baseOpts.Headers, baseOpts.OriginalRequest, baseOpts.Metadata)
	digest := selector.k12.store.digest(identity.ID)
	if binding, ok := selector.k12.store.binding(digest, time.Now()); !ok || binding.AuthID != k12A.ID {
		t.Fatalf("synchronous timeout deleted confirmed binding: binding=%#v ok=%v", binding, ok)
	}
	if remaining := selector.k12.store.cooldownRemaining(digest, k12A.ID, time.Now()); remaining <= 0 {
		t.Fatal("synchronous timeout did not establish session-scoped cooldown")
	}

	retryOpts := withSelectionAttempt(baseOpts)
	retryOpts.Metadata[cliproxyexecutor.ExcludedAuthIDsMetadataKey] = []string{k12A.ID}
	retryCandidates := []*Auth{k12B, plus}
	if bound, handled, errPick := selector.PickBeforeAvailability(
		context.Background(), "codex", "gpt-5", retryOpts, retryCandidates,
	); errPick != nil || handled || bound != nil {
		t.Fatalf("filtered retry pre-selection = %v handled=%v err=%v", bound, handled, errPick)
	}

	retry, err := selector.Pick(context.Background(), "codex", "gpt-5", retryOpts, retryCandidates)
	if err != nil || retry == nil || retry.ID == k12A.ID {
		t.Fatalf("filtered retry selection = %#v, %v", retry, err)
	}
	if retry.ID != plus.ID {
		t.Fatalf("stream-open timeout retry selected %q, want preferred spillover %q", retry.ID, plus.ID)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: retry.ID, Success: true}, retryOpts)

	lateDirective := selector.OnSelectionResult(context.Background(), timeoutResult, timeoutOpts)
	if !lateDirective.SuppressAvailabilityUpdate || !lateDirective.StopAuthAttempt || lateDirective.StopCredentialFallback {
		t.Fatalf("late duplicate timeout directive = %#v", lateDirective)
	}
	winner, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || !handled || winner == nil || winner.ID != plus.ID {
		t.Fatalf("late timeout displaced successful spillover: auth=%v handled=%v err=%v", winner, handled, err)
	}
}

func TestK12ExcludedBindingSpillsOverWithAnotherAttemptStillActive(t *testing.T) {
	t.Parallel()

	selector := newTestK12Selector(
		t,
		filepath.Join(t.TempDir(), "state.json"),
		map[string]*int{"k12-a": intPtr(20), "k12-b": intPtr(20)},
	)
	k12A := testK12Auth("k12-a")
	k12B := testK12Auth("k12-b")
	plus := testPlusAuth("plus-a")
	auths := []*Auth{k12A, k12B, plus}
	baseOpts := promptCacheOptions("excluded-binding-with-active-attempt")

	selected, err := selector.Pick(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || selected == nil || selected.ID != k12A.ID {
		t.Fatalf("initial K12 selection = %#v, %v", selected, err)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: selected.ID, Success: true}, baseOpts)

	otherOpts := withSelectionAttempt(baseOpts)
	other, err := selector.Pick(context.Background(), "codex", "gpt-5", otherOpts, auths)
	if err != nil || other == nil || other.ID != k12A.ID {
		t.Fatalf("other active selection = %#v, %v", other, err)
	}
	timedOpts := withSelectionAttempt(baseOpts)
	timed, err := selector.Pick(context.Background(), "codex", "gpt-5", timedOpts, auths)
	if err != nil || timed == nil || timed.ID != k12A.ID {
		t.Fatalf("timed selection = %#v, %v", timed, err)
	}
	timeoutResult := Result{
		AuthID:  timed.ID,
		Success: false,
		Error: &Error{
			HTTPStatus: http.StatusGatewayTimeout,
			Message:    "upstream timed out in stream_open attempt=1/2",
		},
	}
	directive := selector.OnSelectionResult(context.Background(), timeoutResult, timedOpts)
	if !directive.SuppressAvailabilityUpdate || !directive.StopAuthAttempt || directive.StopCredentialFallback {
		t.Fatalf("parallel timeout directive = %#v", directive)
	}

	identity, _ := extractSessionIdentities(baseOpts.Headers, baseOpts.OriginalRequest, baseOpts.Metadata)
	digest := selector.k12.store.digest(identity.ID)
	if remaining := selector.k12.store.cooldownRemaining(digest, k12A.ID, time.Now()); remaining > 0 {
		t.Fatalf("parallel timeout unexpectedly established persisted cooldown: %s", remaining)
	}

	retryOpts := withSelectionAttempt(baseOpts)
	retryOpts.Metadata[cliproxyexecutor.ExcludedAuthIDsMetadataKey] = []string{k12A.ID}
	retryCandidates := []*Auth{k12B, plus}
	if bound, handled, errPick := selector.PickBeforeAvailability(
		context.Background(), "codex", "gpt-5", retryOpts, retryCandidates,
	); errPick != nil || handled || bound != nil {
		t.Fatalf("request-excluded pre-selection = %v handled=%v err=%v", bound, handled, errPick)
	}
	retry, err := selector.Pick(context.Background(), "codex", "gpt-5", retryOpts, retryCandidates)
	if err != nil || retry == nil || retry.ID != plus.ID {
		t.Fatalf("request-excluded spillover = %#v, %v", retry, err)
	}

	lateTimed := selector.OnSelectionResult(context.Background(), Result{AuthID: k12A.ID, Success: true}, timedOpts)
	if !lateTimed.SuppressAvailabilityUpdate || lateTimed.StopAuthAttempt {
		t.Fatalf("late timed success directive = %#v", lateTimed)
	}
	selector.OnSelectionResult(context.Background(), Result{AuthID: plus.ID, Success: true}, retryOpts)
	lateOther := selector.OnSelectionResult(context.Background(), Result{AuthID: k12A.ID, Success: true}, otherOpts)
	if !lateOther.SuppressAvailabilityUpdate || lateOther.StopAuthAttempt {
		t.Fatalf("late other success directive = %#v", lateOther)
	}
	winner, handled, err := selector.PickBeforeAvailability(context.Background(), "codex", "gpt-5", baseOpts, auths)
	if err != nil || !handled || winner == nil || winner.ID != plus.ID {
		t.Fatalf("parallel attempt displaced Plus spillover: auth=%v handled=%v err=%v", winner, handled, err)
	}
}
