package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type codexSafetyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexSafetyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type codexDataThenErrorReader struct {
	data []byte
	err  error
}

type codexDelayedReader struct {
	delay  time.Duration
	reader io.Reader
	once   sync.Once
}

func (r *codexDelayedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { time.Sleep(r.delay) })
	return r.reader.Read(p)
}

func (r *codexDataThenErrorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func codexSafetyAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: "auth-test", Attributes: map[string]string{
		"base_url": "https://upstream.invalid",
		"api_key":  "test",
	}}
}

func codexSafetyRequest() cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`),
	}
}

func codexSafetyOptions() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
	}
}

func codexNonStreamSafetyOptions() cliproxyexecutor.Options {
	opts := codexSafetyOptions()
	opts.Stream = false
	return opts
}

func codexSafetyContext(rt http.RoundTripper) context.Context {
	return context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
}

func TestCodexExecutorExecuteStreamClassifiesPreSendFailureAsRetrySafe(t *testing.T) {
	var calls atomic.Int32
	rt := codexSafetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("dial tcp: connection refused")
	})

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.ExecuteStream(codexSafetyContext(rt), codexSafetyAuth(), codexSafetyRequest(), codexSafetyOptions())
	if err == nil {
		t.Fatal("expected pre-send transport error")
	}
	known, safe := cliproxyexecutor.UpstreamAttemptRetrySafety(err)
	if !known || !safe {
		t.Fatalf("pre-send safety = known:%v safe:%v; err=%v", known, safe, err)
	}
	if cliproxyexecutor.IsPossibleBillableRequest(err) {
		t.Fatalf("pre-send error marked billable: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestCodexExecutorExecuteStreamClassifiesPostSendEOFAsPossiblyBillable(t *testing.T) {
	var calls atomic.Int32
	rt := codexSafetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		return nil, io.ErrUnexpectedEOF
	})

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.ExecuteStream(codexSafetyContext(rt), codexSafetyAuth(), codexSafetyRequest(), codexSafetyOptions())
	if err == nil {
		t.Fatal("expected post-send EOF")
	}
	known, safe := cliproxyexecutor.UpstreamAttemptRetrySafety(err)
	if !known || safe {
		t.Fatalf("post-send safety = known:%v safe:%v; err=%v", known, safe, err)
	}
	if !cliproxyexecutor.IsPossibleBillableRequest(err) {
		t.Fatalf("post-send error not marked billable: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestCodexExecutorExecuteStreamMarksOnlyExplicitUsageLimit429ForCredentialFallback(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantFallback bool
		wantBillable bool
	}{
		{
			name:         "usage limit rejection",
			status:       http.StatusTooManyRequests,
			body:         `{"error":{"type":"usage_limit_reached","message":"quota exhausted","resets_in_seconds":7}}`,
			wantFallback: true,
		},
		{
			name:         "ordinary 429",
			status:       http.StatusTooManyRequests,
			body:         `{"error":{"type":"rate_limit_error","message":"slow down"}}`,
			wantBillable: true,
		},
		{
			name:         "usage shaped 503",
			status:       http.StatusServiceUnavailable,
			body:         `{"error":{"type":"usage_limit_reached","message":"unavailable"}}`,
			wantBillable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := codexSafetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if _, err := io.Copy(io.Discard, req.Body); err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: tt.status,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(tt.body)),
					Request:    req,
				}, nil
			})

			executor := NewCodexExecutor(&config.Config{})
			_, err := executor.ExecuteStream(codexSafetyContext(rt), codexSafetyAuth(), codexSafetyRequest(), codexSafetyOptions())
			if err == nil {
				t.Fatal("expected upstream rejection")
			}
			if got := cliproxyexecutor.IsCredentialFallbackSafe(err); got != tt.wantFallback {
				t.Fatalf("credential fallback safe = %v, want %v; err=%v", got, tt.wantFallback, err)
			}
			known, safe := cliproxyexecutor.UpstreamAttemptRetrySafety(err)
			if !known || safe {
				t.Fatalf("transport safety = known:%v safe:%v; err=%v", known, safe, err)
			}
			if got := cliproxyexecutor.IsPossibleBillableRequest(err); got != tt.wantBillable {
				t.Fatalf("possible billable = %v, want %v; err=%v", got, tt.wantBillable, err)
			}
		})
	}
}

func TestCodexExecutorExecuteNonStreamClassifiesPostSendEOFAsPossiblyBillable(t *testing.T) {
	var calls atomic.Int32
	rt := codexSafetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		return nil, io.ErrUnexpectedEOF
	})

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(codexSafetyContext(rt), codexSafetyAuth(), codexSafetyRequest(), codexNonStreamSafetyOptions())
	if err == nil {
		t.Fatal("expected post-send EOF")
	}
	known, safe := cliproxyexecutor.UpstreamAttemptRetrySafety(err)
	if !known || safe || !cliproxyexecutor.IsPossibleBillableRequest(err) {
		t.Fatalf("post-send safety = known:%v safe:%v billable:%v err:%v", known, safe, cliproxyexecutor.IsPossibleBillableRequest(err), err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestCodexExecutorExecuteStreamPropagatesCancellationWithoutReplay(t *testing.T) {
	var calls atomic.Int32
	requestRead := make(chan struct{})
	rt := codexSafetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		close(requestRead)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})

	ctx, cancel := context.WithCancel(codexSafetyContext(rt))
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		executor := NewCodexExecutor(&config.Config{})
		_, err := executor.ExecuteStream(ctx, codexSafetyAuth(), codexSafetyRequest(), codexSafetyOptions())
		errCh <- err
	}()

	select {
	case <-requestRead:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("upstream did not consume request body")
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
		if !cliproxyexecutor.IsUpstreamCancellationUnconfirmed(err) {
			t.Fatalf("cancellation was not marked unconfirmed: %v", err)
		}
		if !cliproxyexecutor.IsPossibleBillableRequest(err) {
			t.Fatalf("canceled request was not marked possibly billable: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ExecuteStream did not return after cancellation")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestCodexExecutorExecuteStreamSlowFirstTokenUsesSinglePOST(t *testing.T) {
	for _, delay := range []time.Duration{90 * time.Millisecond, 150 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			var calls atomic.Int32
			var gotIdempotencyKey string
			var getBodyWasSet bool
			var observationsMu sync.Mutex
			var observations []cliproxyexecutor.UpstreamAttemptObservation
			rt := codexSafetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				gotIdempotencyKey = req.Header.Get("Idempotency-Key")
				getBodyWasSet = req.GetBody != nil
				if _, err := io.Copy(io.Discard, req.Body); err != nil {
					return nil, err
				}
				body := `data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(&codexDelayedReader{
						delay:  delay,
						reader: strings.NewReader(body),
					}),
					Request: req,
				}, nil
			})

			opts := codexSafetyOptions()
			opts.Metadata = map[string]any{
				cliproxyexecutor.IdempotencyKeyMetadataKey: "logical-request-1",
				cliproxyexecutor.UpstreamAttemptObserverMetadataKey: func(observation cliproxyexecutor.UpstreamAttemptObservation) {
					observationsMu.Lock()
					observations = append(observations, observation)
					observationsMu.Unlock()
				},
			}
			executor := NewCodexExecutor(&config.Config{})
			result, err := executor.ExecuteStream(codexSafetyContext(rt), codexSafetyAuth(), codexSafetyRequest(), opts)
			if err != nil {
				t.Fatalf("ExecuteStream: %v", err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream error: %v", chunk.Err)
				}
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1", got)
			}
			if gotIdempotencyKey != "logical-request-1" {
				t.Fatalf("Idempotency-Key = %q", gotIdempotencyKey)
			}
			if getBodyWasSet {
				t.Fatal("request remained replayable through GetBody")
			}
			observationsMu.Lock()
			defer observationsMu.Unlock()
			if len(observations) != 3 ||
				observations[0].Phase != cliproxyexecutor.UpstreamAttemptStarted ||
				observations[1].Phase != cliproxyexecutor.UpstreamAttemptResponded ||
				observations[2].Phase != cliproxyexecutor.UpstreamAttemptFinished {
				t.Fatalf("attempt observations = %#v", observations)
			}
			if observations[0].AuthID != "auth-test" || observations[1].StatusCode != http.StatusOK || observations[2].RequestBodyBytesRead == 0 {
				t.Fatalf("attempt observations lost transport metadata: %#v", observations)
			}
		})
	}
}

func TestCodexExecutorExecuteNonStreamSlowResponseUsesSinglePOST(t *testing.T) {
	var calls atomic.Int32
	var gotIdempotencyKey string
	var getBodyWasSet bool
	var observationsMu sync.Mutex
	var observations []cliproxyexecutor.UpstreamAttemptObservation
	rt := codexSafetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		gotIdempotencyKey = req.Header.Get("Idempotency-Key")
		getBodyWasSet = req.GetBody != nil
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		body := `data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(&codexDelayedReader{
				delay:  90 * time.Millisecond,
				reader: strings.NewReader(body),
			}),
			Request: req,
		}, nil
	})

	opts := codexNonStreamSafetyOptions()
	opts.Metadata = map[string]any{
		cliproxyexecutor.IdempotencyKeyMetadataKey: "logical-request-non-stream",
		cliproxyexecutor.UpstreamAttemptObserverMetadataKey: func(observation cliproxyexecutor.UpstreamAttemptObservation) {
			observationsMu.Lock()
			observations = append(observations, observation)
			observationsMu.Unlock()
		},
	}
	executor := NewCodexExecutor(&config.Config{})
	if _, err := executor.Execute(codexSafetyContext(rt), codexSafetyAuth(), codexSafetyRequest(), opts); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if gotIdempotencyKey != "logical-request-non-stream" {
		t.Fatalf("Idempotency-Key = %q", gotIdempotencyKey)
	}
	if getBodyWasSet {
		t.Fatal("request remained replayable through GetBody")
	}
	observationsMu.Lock()
	defer observationsMu.Unlock()
	if len(observations) != 3 ||
		observations[0].Phase != cliproxyexecutor.UpstreamAttemptStarted ||
		observations[1].Phase != cliproxyexecutor.UpstreamAttemptResponded ||
		observations[2].Phase != cliproxyexecutor.UpstreamAttemptFinished {
		t.Fatalf("attempt observations = %#v", observations)
	}
}

func TestCodexExecutorExecuteNonStreamResponseEOFIsPossiblyBillable(t *testing.T) {
	var calls atomic.Int32
	rt := codexSafetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(&codexDataThenErrorReader{
				data: []byte(`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n"),
				err:  io.ErrUnexpectedEOF,
			}),
			Request: req,
		}, nil
	})

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(codexSafetyContext(rt), codexSafetyAuth(), codexSafetyRequest(), codexNonStreamSafetyOptions())
	if err == nil {
		t.Fatal("missing response EOF error")
	}
	known, safe := cliproxyexecutor.UpstreamAttemptRetrySafety(err)
	if !known || safe || !cliproxyexecutor.IsPossibleBillableRequest(err) {
		t.Fatalf("response EOF classification = known:%v safe:%v billable:%v err:%v", known, safe, cliproxyexecutor.IsPossibleBillableRequest(err), err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestCodexExecutorExecuteStreamResponseEOFIsPossiblyBillable(t *testing.T) {
	var calls atomic.Int32
	rt := codexSafetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		reader := &codexDataThenErrorReader{
			data: []byte(`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n"),
			err:  io.ErrUnexpectedEOF,
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(reader),
			Request:    req,
		}, nil
	})

	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(codexSafetyContext(rt), codexSafetyAuth(), codexSafetyRequest(), codexSafetyOptions())
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("missing response EOF error")
	}
	known, safe := cliproxyexecutor.UpstreamAttemptRetrySafety(streamErr)
	if !known || safe || !cliproxyexecutor.IsPossibleBillableRequest(streamErr) {
		t.Fatalf("response EOF classification = known:%v safe:%v billable:%v err:%v", known, safe, cliproxyexecutor.IsPossibleBillableRequest(streamErr), streamErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestCodexExecutorExecuteStreamCleanEOFBeforeCompletedIsPossiblyBillable(t *testing.T) {
	var calls atomic.Int32
	rt := codexSafetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			return nil, err
		}
		body := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			`data: {"type":"response.output_text.delta","delta":"partial"}`,
			"",
		}, "\n")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})

	executor := NewCodexExecutor(&config.Config{})
	result, err := executor.ExecuteStream(codexSafetyContext(rt), codexSafetyAuth(), codexSafetyRequest(), codexSafetyOptions())
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("clean EOF before response.completed was treated as success")
	}
	known, safe := cliproxyexecutor.UpstreamAttemptRetrySafety(streamErr)
	if !known || safe || !cliproxyexecutor.IsPossibleBillableRequest(streamErr) {
		t.Fatalf("clean EOF classification = known:%v safe:%v billable:%v err:%v", known, safe, cliproxyexecutor.IsPossibleBillableRequest(streamErr), streamErr)
	}
	if !strings.Contains(streamErr.Error(), "before response.completed") {
		t.Fatalf("clean EOF error lost terminal context: %v", streamErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}
