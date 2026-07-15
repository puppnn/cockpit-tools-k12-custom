package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type handlerStreamRetrySafetyExecutor struct {
	mu    sync.Mutex
	calls []string
}

func (e *handlerStreamRetrySafetyExecutor) Identifier() string { return "codex" }

func (e *handlerStreamRetrySafetyExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *handlerStreamRetrySafetyExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.mu.Lock()
	e.calls = append(e.calls, authID)
	e.mu.Unlock()

	chunks := make(chan coreexecutor.StreamChunk, 1)
	if authID == "aa-first" {
		chunks <- coreexecutor.StreamChunk{Err: io.ErrUnexpectedEOF}
	} else {
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"ok"}`)}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Headers: http.Header{"X-Upstream": {authID}}, Chunks: chunks}, nil
}

func (e *handlerStreamRetrySafetyExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *handlerStreamRetrySafetyExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *handlerStreamRetrySafetyExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *handlerStreamRetrySafetyExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func TestExecuteStreamWithAuthManagerDoesNotBootstrapRetryPossiblySentResponse(t *testing.T) {
	const model = "gpt-5.4"
	executor := &handlerStreamRetrySafetyExecutor{}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)

	for _, authID := range []string{"aa-first", "bb-second"} {
		auth := &coreauth.Auth{ID: authID, Provider: "codex", Status: coreauth.StatusActive}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", authID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
		id := authID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 2},
	}, manager)
	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(
		context.Background(), "openai-response", model, []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`), "",
	)
	for range dataChan {
	}
	var streamErr error
	for msg := range errChan {
		if msg != nil {
			streamErr = msg.Error
		}
	}
	if streamErr == nil {
		t.Fatal("missing stream error")
	}
	if !coreexecutor.IsPossibleBillableRequest(streamErr) {
		t.Fatalf("stream error not marked possibly billable: %v", streamErr)
	}
	got := executor.Calls()
	if len(got) != 1 || got[0] != "aa-first" {
		t.Fatalf("bootstrap retried possibly sent request: %v", got)
	}
}
