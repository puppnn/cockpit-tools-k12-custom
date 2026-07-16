package executor

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

type upstreamAttemptStatusError struct {
	status int
}

func (e upstreamAttemptStatusError) Error() string   { return http.StatusText(e.status) }
func (e upstreamAttemptStatusError) StatusCode() int { return e.status }

type credentialFallbackStatusError struct {
	status     int
	retryAfter time.Duration
}

func (e credentialFallbackStatusError) Error() string   { return http.StatusText(e.status) }
func (e credentialFallbackStatusError) StatusCode() int { return e.status }
func (e credentialFallbackStatusError) Headers() http.Header {
	return http.Header{"X-Test": {"preserved"}}
}
func (e credentialFallbackStatusError) RetryAfter() *time.Duration { return &e.retryAfter }

func TestUpstreamAttemptErrorRetrySafety(t *testing.T) {
	tests := []struct {
		name             string
		err              error
		wantSafe         bool
		wantBillable     bool
		wantCancellation bool
	}{
		{
			name:     "pre-send transport failure",
			err:      WrapUpstreamAttemptError(errors.New("dial failed"), 0, false, false),
			wantSafe: true,
		},
		{
			name:         "request body consumed",
			err:          WrapUpstreamAttemptError(errors.New("unexpected EOF"), 12, false, false),
			wantBillable: true,
		},
		{
			name:         "response received",
			err:          WrapUpstreamAttemptError(errors.New("empty stream"), 0, true, false),
			wantBillable: true,
		},
		{
			name:             "cancellation after send",
			err:              WrapUpstreamAttemptError(context.Canceled, 12, false, true),
			wantBillable:     true,
			wantCancellation: true,
		},
		{
			name:             "nested annotation preserves cancellation",
			err:              WrapUpstreamAttemptError(WrapUpstreamAttemptError(context.Canceled, 12, false, true), 0, true, false),
			wantBillable:     true,
			wantCancellation: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			known, safe := UpstreamAttemptRetrySafety(tt.err)
			if !known {
				t.Fatal("attempt safety was not recognized")
			}
			if safe != tt.wantSafe {
				t.Fatalf("safe = %v, want %v", safe, tt.wantSafe)
			}
			if got := IsPossibleBillableRequest(tt.err); got != tt.wantBillable {
				t.Fatalf("possible billable = %v, want %v", got, tt.wantBillable)
			}
			if got := IsUpstreamCancellationUnconfirmed(tt.err); got != tt.wantCancellation {
				t.Fatalf("cancellation unconfirmed = %v, want %v", got, tt.wantCancellation)
			}
		})
	}
}

func TestUpstreamAttemptErrorPreservesStatusCode(t *testing.T) {
	err := WrapUpstreamAttemptError(upstreamAttemptStatusError{status: http.StatusBadGateway}, 1, true, false)
	statusErr, ok := err.(StatusError)
	if !ok {
		t.Fatalf("wrapped error %T does not implement StatusError", err)
	}
	if got := statusErr.StatusCode(); got != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", got, http.StatusBadGateway)
	}
}

func TestUpstreamAttemptRetrySafetyUnknownError(t *testing.T) {
	known, safe := UpstreamAttemptRetrySafety(errors.New("unclassified"))
	if known || safe {
		t.Fatalf("unknown error classified as known=%v safe=%v", known, safe)
	}
}

func TestCredentialFallbackSafeDoesNotRelaxTransportSafetyOrMarkBillable(t *testing.T) {
	rejected := MarkCredentialFallbackSafe(credentialFallbackStatusError{
		status:     http.StatusTooManyRequests,
		retryAfter: 7 * time.Second,
	})
	err := WrapUpstreamAttemptError(rejected, 64, true, false)

	if !IsCredentialFallbackSafe(err) {
		t.Fatal("explicit rejection marker was not preserved through attempt annotation")
	}
	known, safe := UpstreamAttemptRetrySafety(err)
	if !known || safe {
		t.Fatalf("transport safety was widened: known=%v safe=%v", known, safe)
	}
	if IsPossibleBillableRequest(err) {
		t.Fatal("explicit credential rejection was marked possibly billable")
	}
	statusErr, ok := rejected.(StatusError)
	if !ok || statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status was not preserved: %#v", rejected)
	}
	headerErr, ok := rejected.(interface{ Headers() http.Header })
	if !ok || headerErr.Headers().Get("X-Test") != "preserved" {
		t.Fatalf("headers were not preserved: %#v", rejected)
	}
	retryErr, ok := rejected.(interface{ RetryAfter() *time.Duration })
	if !ok || retryErr.RetryAfter() == nil || *retryErr.RetryAfter() != 7*time.Second {
		t.Fatalf("retry metadata was not preserved: %#v", rejected)
	}
}
