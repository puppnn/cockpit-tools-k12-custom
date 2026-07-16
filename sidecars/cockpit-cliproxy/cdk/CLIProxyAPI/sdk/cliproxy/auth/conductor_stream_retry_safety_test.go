package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type streamRetrySafetyExecutor struct {
	mu        sync.Mutex
	mode      string
	firstAuth string
	calls     []string
}

func (e *streamRetrySafetyExecutor) Identifier() string { return "codex" }

func (e *streamRetrySafetyExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.mu.Lock()
	e.calls = append(e.calls, authID)
	e.mu.Unlock()
	if e.mode == "always_quota_rejected" {
		err := cliproxyexecutor.MarkCredentialFallbackSafe(&Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    `{"error":{"type":"usage_limit_reached"}}`,
		})
		return cliproxyexecutor.Response{}, cliproxyexecutor.WrapUpstreamAttemptError(err, 32, true, false)
	}
	if e.mode == "always_post_send" {
		return cliproxyexecutor.Response{}, cliproxyexecutor.WrapUpstreamAttemptError(io.ErrUnexpectedEOF, 32, false, false)
	}

	if authID == e.firstAuth {
		switch e.mode {
		case "pre_send":
			return cliproxyexecutor.Response{}, cliproxyexecutor.WrapUpstreamAttemptError(errors.New("dial failed"), 0, false, false)
		case "quota_rejected":
			err := cliproxyexecutor.MarkCredentialFallbackSafe(&Error{
				HTTPStatus: http.StatusTooManyRequests,
				Message:    `{"error":{"type":"usage_limit_reached"}}`,
			})
			return cliproxyexecutor.Response{}, cliproxyexecutor.WrapUpstreamAttemptError(err, 32, true, false)
		case "post_send":
			return cliproxyexecutor.Response{}, cliproxyexecutor.WrapUpstreamAttemptError(io.ErrUnexpectedEOF, 32, false, false)
		case "status_503":
			return cliproxyexecutor.Response{}, cliproxyexecutor.WrapUpstreamAttemptError(&Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream unavailable"}, 32, true, false)
		}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *streamRetrySafetyExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.mu.Lock()
	e.calls = append(e.calls, authID)
	e.mu.Unlock()
	if e.mode == "always_quota_rejected" {
		err := cliproxyexecutor.MarkCredentialFallbackSafe(&Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    `{"error":{"type":"usage_limit_reached"}}`,
		})
		return nil, cliproxyexecutor.WrapUpstreamAttemptError(err, 32, true, false)
	}
	if e.mode == "always_post_send" {
		return nil, cliproxyexecutor.WrapUpstreamAttemptError(io.ErrUnexpectedEOF, 32, false, false)
	}

	if authID == e.firstAuth {
		switch e.mode {
		case "pre_send":
			return nil, cliproxyexecutor.WrapUpstreamAttemptError(errors.New("dial failed"), 0, false, false)
		case "quota_rejected":
			err := cliproxyexecutor.MarkCredentialFallbackSafe(&Error{
				HTTPStatus: http.StatusTooManyRequests,
				Message:    `{"error":{"type":"usage_limit_reached"}}`,
			})
			return nil, cliproxyexecutor.WrapUpstreamAttemptError(err, 32, true, false)
		case "post_send":
			return nil, cliproxyexecutor.WrapUpstreamAttemptError(io.ErrUnexpectedEOF, 32, false, false)
		case "status_503":
			return nil, cliproxyexecutor.WrapUpstreamAttemptError(&Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream unavailable"}, 32, true, false)
		case "response_eof":
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Err: io.ErrUnexpectedEOF}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Upstream": {"started"}}, Chunks: chunks}, nil
		}
	}

	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"ok"}`)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Upstream": {"ok"}}, Chunks: chunks}, nil
}

func (e *streamRetrySafetyExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *streamRetrySafetyExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *streamRetrySafetyExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *streamRetrySafetyExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

type streamRetrySafetyHomeDispatcher struct {
	mu     sync.Mutex
	counts []int
}

func (d *streamRetrySafetyHomeDispatcher) HeartbeatOK() bool { return true }

func (d *streamRetrySafetyHomeDispatcher) RPopAuth(_ context.Context, requestedModel string, _ string, _ http.Header, count int) ([]byte, error) {
	d.mu.Lock()
	d.counts = append(d.counts, count)
	d.mu.Unlock()
	authID := "aa-first"
	if count > 1 {
		authID = "bb-second"
	}
	return json.Marshal(homeAuthDispatchResponse{
		Model: requestedModel,
		Auth:  Auth{ID: authID, Provider: "codex", Status: StatusActive},
	})
}

func (d *streamRetrySafetyHomeDispatcher) Counts() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.counts...)
}

func newStreamRetrySafetyManager(t *testing.T, mode string) (*Manager, *streamRetrySafetyExecutor) {
	t.Helper()
	const model = "gpt-5.4"
	executor := &streamRetrySafetyExecutor{mode: mode, firstAuth: "aa-first"}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(3, 25*time.Millisecond, 0)
	manager.RegisterExecutor(executor)

	for _, authID := range []string{"aa-first", "bb-second"} {
		auth := &Auth{ID: authID, Provider: "codex", Status: StatusActive}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", authID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
		id := authID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return manager, executor
}

func responsesSafetyOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Stream: true, SourceFormat: "openai-response"}
}

func responsesNonStreamSafetyOptions() cliproxyexecutor.Options {
	opts := responsesSafetyOptions()
	opts.Stream = false
	return opts
}

func TestManagerExecuteStreamAllowsExplicitPreSendFallback(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "pre_send")
	result, err := manager.ExecuteStream(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesSafetyOptions(),
	)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("fallback stream error: %v", chunk.Err)
		}
	}
	got := executor.Calls()
	if len(got) != 2 || got[0] != "aa-first" || got[1] != "bb-second" {
		t.Fatalf("upstream calls = %v, want [aa-first bb-second]", got)
	}
}

func TestManagerExecuteStreamAllowsExplicitQuotaRejectionFallback(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "quota_rejected")
	result, err := manager.ExecuteStream(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesSafetyOptions(),
	)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("fallback stream error: %v", chunk.Err)
		}
	}
	got := executor.Calls()
	if len(got) != 2 || got[0] != "aa-first" || got[1] != "bb-second" {
		t.Fatalf("quota rejection calls = %v, want [aa-first bb-second]", got)
	}
}

func TestManagerExecuteStreamHomeQuotaRejectionAdvancesCredentialCount(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "quota_rejected")
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	dispatcher := &streamRetrySafetyHomeDispatcher{}
	previousDispatcher := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = previousDispatcher })

	result, err := manager.ExecuteStream(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesSafetyOptions(),
	)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("fallback stream error: %v", chunk.Err)
		}
	}
	if got := dispatcher.Counts(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("home credential counts = %v, want [1 2]", got)
	}
	if got := executor.Calls(); len(got) != 2 || got[0] != "aa-first" || got[1] != "bb-second" {
		t.Fatalf("home quota rejection calls = %v, want [aa-first bb-second]", got)
	}
}

func TestManagerExecuteStreamDoesNotRetryQuotaRejectionAfterCredentialsExhausted(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "always_quota_rejected")
	result, err := manager.ExecuteStream(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesSafetyOptions(),
	)
	if err == nil {
		t.Fatalf("expected quota rejection, got result %#v", result)
	}
	if !cliproxyexecutor.IsCredentialFallbackSafe(err) {
		t.Fatalf("terminal error lost credential-rejection marker: %v", err)
	}
	if got := executor.Calls(); len(got) != 2 {
		t.Fatalf("exhausted credentials produced %d calls, want 2: %v", len(got), got)
	}
}

func TestManagerExecuteStreamDoesNotFallbackAfterBodyRead(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "post_send")
	result, err := manager.ExecuteStream(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesSafetyOptions(),
	)
	if err == nil {
		t.Fatalf("expected post-send error, got result %#v", result)
	}
	if !cliproxyexecutor.IsPossibleBillableRequest(err) {
		t.Fatalf("post-send failure not marked possibly billable: %v", err)
	}
	got := executor.Calls()
	if len(got) != 1 || got[0] != "aa-first" {
		t.Fatalf("post-send failure was retried: %v", got)
	}
}

func TestManagerExecuteStreamDoesNotFallbackAfterUpstream5xx(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "status_503")
	result, err := manager.ExecuteStream(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesSafetyOptions(),
	)
	if err == nil {
		t.Fatalf("expected 503 error, got result %#v", result)
	}
	if statusCodeFromError(err) != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; err=%v", statusCodeFromError(err), http.StatusServiceUnavailable, err)
	}
	if !cliproxyexecutor.IsPossibleBillableRequest(err) {
		t.Fatalf("503 failure not marked possibly billable: %v", err)
	}
	got := executor.Calls()
	if len(got) != 1 || got[0] != "aa-first" {
		t.Fatalf("503 failure was retried: %v", got)
	}
}

func TestManagerExecuteStreamDoesNotFallbackAfterResponseEOF(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "response_eof")
	result, err := manager.ExecuteStream(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesSafetyOptions(),
	)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil || !cliproxyexecutor.IsPossibleBillableRequest(streamErr) {
		t.Fatalf("response EOF classification = %v", streamErr)
	}
	got := executor.Calls()
	if len(got) != 1 || got[0] != "aa-first" {
		t.Fatalf("response EOF was retried: %v", got)
	}
}

func TestManagerExecuteAllowsExplicitPreSendFallback(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "pre_send")
	if _, err := manager.Execute(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesNonStreamSafetyOptions(),
	); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := executor.Calls()
	if len(got) != 2 || got[0] != "aa-first" || got[1] != "bb-second" {
		t.Fatalf("upstream calls = %v, want [aa-first bb-second]", got)
	}
}

func TestManagerExecuteAllowsExplicitQuotaRejectionFallback(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "quota_rejected")
	if _, err := manager.Execute(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesNonStreamSafetyOptions(),
	); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := executor.Calls()
	if len(got) != 2 || got[0] != "aa-first" || got[1] != "bb-second" {
		t.Fatalf("quota rejection calls = %v, want [aa-first bb-second]", got)
	}
}

func TestManagerExecuteDoesNotRetryQuotaRejectionAfterCredentialsExhausted(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "always_quota_rejected")
	_, err := manager.Execute(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesNonStreamSafetyOptions(),
	)
	if err == nil || !cliproxyexecutor.IsCredentialFallbackSafe(err) {
		t.Fatalf("terminal quota rejection classification = %v", err)
	}
	if got := executor.Calls(); len(got) != 2 {
		t.Fatalf("exhausted credentials produced %d calls, want 2: %v", len(got), got)
	}
}

func TestManagerExecuteDoesNotFallbackAfterBodyRead(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "post_send")
	_, err := manager.Execute(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesNonStreamSafetyOptions(),
	)
	if err == nil || !cliproxyexecutor.IsPossibleBillableRequest(err) {
		t.Fatalf("post-send failure classification = %v", err)
	}
	got := executor.Calls()
	if len(got) != 1 || got[0] != "aa-first" {
		t.Fatalf("post-send failure was retried: %v", got)
	}
}

func TestManagerExecuteDoesNotFallbackAfterUpstream5xx(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "status_503")
	_, err := manager.Execute(
		context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesNonStreamSafetyOptions(),
	)
	if err == nil || statusCodeFromError(err) != http.StatusServiceUnavailable {
		t.Fatalf("503 failure classification = %v", err)
	}
	got := executor.Calls()
	if len(got) != 1 || got[0] != "aa-first" {
		t.Fatalf("503 failure was retried: %v", got)
	}
}

func TestManagerConcurrentResponsesRequestsDoNotAmplifyPostSendFailures(t *testing.T) {
	manager, executor := newStreamRetrySafetyManager(t, "always_post_send")
	const logicalRequests = 12
	start := make(chan struct{})
	errorsCh := make(chan error, logicalRequests)
	var wg sync.WaitGroup
	for index := 0; index < logicalRequests; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := manager.ExecuteStream(
				context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, responsesSafetyOptions(),
			)
			errorsCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err == nil || !cliproxyexecutor.IsPossibleBillableRequest(err) {
			t.Fatalf("concurrent failure classification = %v", err)
		}
	}
	if got := len(executor.Calls()); got != logicalRequests {
		t.Fatalf("%d logical requests produced %d upstream POST attempts", logicalRequests, got)
	}
}
