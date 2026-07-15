package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"

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
	if e.mode == "always_post_send" {
		return cliproxyexecutor.Response{}, cliproxyexecutor.WrapUpstreamAttemptError(io.ErrUnexpectedEOF, 32, false, false)
	}

	if authID == e.firstAuth {
		switch e.mode {
		case "pre_send":
			return cliproxyexecutor.Response{}, cliproxyexecutor.WrapUpstreamAttemptError(errors.New("dial failed"), 0, false, false)
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
	if e.mode == "always_post_send" {
		return nil, cliproxyexecutor.WrapUpstreamAttemptError(io.ErrUnexpectedEOF, 32, false, false)
	}

	if authID == e.firstAuth {
		switch e.mode {
		case "pre_send":
			return nil, cliproxyexecutor.WrapUpstreamAttemptError(errors.New("dial failed"), 0, false, false)
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

func newStreamRetrySafetyManager(t *testing.T, mode string) (*Manager, *streamRetrySafetyExecutor) {
	t.Helper()
	const model = "gpt-5.4"
	executor := &streamRetrySafetyExecutor{mode: mode, firstAuth: "aa-first"}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(3, 0, 0)
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
