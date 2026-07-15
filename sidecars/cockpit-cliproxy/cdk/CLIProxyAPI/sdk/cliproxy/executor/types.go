package executor

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// RequestedModelMetadataKey stores the client-requested model name in Options.Metadata.
const RequestedModelMetadataKey = "requested_model"

// RequestPathMetadataKey stores the inbound HTTP request path (e.g. "/v1/images/generations") in Options.Metadata.
// It is optional and may be absent for non-HTTP executions.
const RequestPathMetadataKey = "request_path"

// DisallowFreeAuthMetadataKey instructs auth selection to skip known free-tier credentials.
const DisallowFreeAuthMetadataKey = "disallow_free_auth"

// ReasoningEffortMetadataKey stores the client-requested reasoning effort for usage logs.
const ReasoningEffortMetadataKey = "reasoning_effort"

// ServiceTierMetadataKey stores the client-requested service tier for usage logs.
const ServiceTierMetadataKey = "service_tier"

// SessionAffinityNamespaceMetadataKey separates session-affinity cache entries
// for callers that share the same downstream session identifier.
const SessionAffinityNamespaceMetadataKey = "session_affinity_namespace"

const (
	// PinnedAuthMetadataKey locks execution to a specific auth ID.
	PinnedAuthMetadataKey = "pinned_auth_id"
	// SelectedAuthMetadataKey stores the auth ID selected by the scheduler.
	SelectedAuthMetadataKey = "selected_auth_id"
	// SelectedAuthCallbackMetadataKey carries an optional callback invoked with
	// AuthSelection. A func(string) callback remains supported for compatibility.
	SelectedAuthCallbackMetadataKey = "selected_auth_callback"
	// ExcludedAuthIDsMetadataKey carries auth IDs that must not be reused by a
	// higher-level retry which already timed out those credentials.
	ExcludedAuthIDsMetadataKey = "excluded_auth_ids"
	// ExecutionSessionMetadataKey identifies a long-lived downstream execution session.
	ExecutionSessionMetadataKey = "execution_session_id"
	// LogicalRequestIDMetadataKey identifies one downstream logical request across
	// any explicitly safe upstream retries.
	LogicalRequestIDMetadataKey = "logical_request_id"
	// IdempotencyKeyMetadataKey carries the stable key forwarded to an upstream
	// that supports idempotent request submission.
	IdempotencyKeyMetadataKey = "idempotency_key"
	// UpstreamAttemptObserverMetadataKey carries an optional callback that is
	// notified at the boundary of each real provider HTTP attempt.
	UpstreamAttemptObserverMetadataKey = "upstream_attempt_observer"
)

// UpstreamAttemptPhase identifies a lifecycle transition for one real
// provider HTTP request.
type UpstreamAttemptPhase string

const (
	UpstreamAttemptStarted   UpstreamAttemptPhase = "started"
	UpstreamAttemptResponded UpstreamAttemptPhase = "responded"
	UpstreamAttemptFinished  UpstreamAttemptPhase = "finished"
)

// UpstreamAttemptObservation is emitted by an executor at the actual HTTP
// transport boundary. It is deliberately separate from auth selection: an
// auth may fail preparation without sending anything, while one selection may
// make more than one explicitly retry-safe transport attempt.
type UpstreamAttemptObservation struct {
	Phase                UpstreamAttemptPhase
	AuthID               string
	At                   time.Time
	RequestBodyBytesRead int64
	ResponseReceived     bool
	StatusCode           int
	Err                  error
}

// NotifyUpstreamAttempt invokes the optional observer attached to opts.
func NotifyUpstreamAttempt(opts Options, observation UpstreamAttemptObservation) {
	if len(opts.Metadata) == 0 {
		return
	}
	observer, _ := opts.Metadata[UpstreamAttemptObserverMetadataKey].(func(UpstreamAttemptObservation))
	if observer != nil {
		observer(observation)
	}
}

// AuthSelection identifies one scheduler selection and the internal attempt
// that must be used when synchronously reporting its result.
type AuthSelection struct {
	AuthID    string
	AttemptID uint64
}

// Request encapsulates the translated payload that will be sent to a provider executor.
type Request struct {
	// Model is the upstream model identifier after translation.
	Model string
	// Payload is the provider specific JSON payload.
	Payload []byte
	// Format represents the provider payload schema.
	Format sdktranslator.Format
	// Metadata carries optional provider specific execution hints.
	Metadata map[string]any
}

// Options controls execution behavior for both streaming and non-streaming calls.
type Options struct {
	// Stream toggles streaming mode.
	Stream bool
	// Alt carries optional alternate format hint (e.g. SSE JSON key).
	Alt string
	// Headers are forwarded to the provider request builder.
	Headers http.Header
	// Query contains optional query string parameters.
	Query url.Values
	// OriginalRequest preserves the inbound request bytes prior to translation.
	OriginalRequest []byte
	// SourceFormat identifies the inbound schema.
	SourceFormat sdktranslator.Format
	// Metadata carries extra execution hints shared across selection and executors.
	Metadata map[string]any
}

// Response wraps either a full provider response or metadata for streaming flows.
type Response struct {
	// Payload is the provider response in the executor format.
	Payload []byte
	// Metadata exposes optional structured data for translators.
	Metadata map[string]any
	// Headers carries upstream HTTP response headers for passthrough to clients.
	Headers http.Header
}

// StreamChunk represents a single streaming payload unit emitted by provider executors.
type StreamChunk struct {
	// Payload is the raw provider chunk payload.
	Payload []byte
	// Err reports any terminal error encountered while producing chunks.
	Err error
}

// StreamResult wraps the streaming response, providing both the chunk channel
// and the upstream HTTP response headers captured before streaming begins.
type StreamResult struct {
	// Headers carries upstream HTTP response headers from the initial connection.
	Headers http.Header
	// Chunks is the channel of streaming payload units.
	Chunks <-chan StreamChunk
}

// StatusError represents an error that carries an HTTP-like status code.
// Provider executors should implement this when possible to enable
// better auth state updates on failures (e.g., 401/402/429).
type StatusError interface {
	error
	StatusCode() int
}

// UpstreamAttemptError records how far an upstream HTTP attempt progressed.
// A request is retry-safe only when the transport did not consume any request
// body bytes and no response was observed. Body consumption is deliberately a
// conservative boundary: bytes read by a transport may already be billable even
// when the caller never receives response headers or a first stream event.
type UpstreamAttemptError struct {
	Cause                   error
	RequestBodyBytesRead    int64
	ResponseReceived        bool
	CancellationUnconfirmed bool
}

func (e *UpstreamAttemptError) Error() string {
	if e == nil || e.Cause == nil {
		return "upstream attempt failed"
	}
	return e.Cause.Error()
}

func (e *UpstreamAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// RetrySafe reports whether the failed attempt is confirmed to have stopped
// before any request body bytes were consumed by the HTTP transport.
func (e *UpstreamAttemptError) RetrySafe() bool {
	return e != nil && e.RequestBodyBytesRead == 0 && !e.ResponseReceived
}

// PossibleBillableRequest reports whether the upstream may have accepted work.
func (e *UpstreamAttemptError) PossibleBillableRequest() bool {
	return e != nil && (e.RequestBodyBytesRead > 0 || e.ResponseReceived)
}

// StatusCode preserves HTTP status information for callers that use a direct
// interface assertion instead of errors.As.
func (e *UpstreamAttemptError) StatusCode() int {
	if e == nil || e.Cause == nil {
		return 0
	}
	var statusErr StatusError
	if errors.As(e.Cause, &statusErr) && statusErr != nil {
		return statusErr.StatusCode()
	}
	return 0
}

// Headers preserves optional upstream response headers through the safety
// wrapper without coupling this package to a concrete provider error type.
func (e *UpstreamAttemptError) Headers() http.Header {
	if e == nil || e.Cause == nil {
		return nil
	}
	var headerErr interface{ Headers() http.Header }
	if errors.As(e.Cause, &headerErr) && headerErr != nil {
		return headerErr.Headers()
	}
	return nil
}

// RetryAfter preserves provider cooldown hints for auth state updates.
func (e *UpstreamAttemptError) RetryAfter() *time.Duration {
	if e == nil || e.Cause == nil {
		return nil
	}
	var retryErr interface{ RetryAfter() *time.Duration }
	if errors.As(e.Cause, &retryErr) && retryErr != nil {
		return retryErr.RetryAfter()
	}
	return nil
}

// WrapUpstreamAttemptError annotates an error with the observable send state.
func WrapUpstreamAttemptError(err error, requestBodyBytesRead int64, responseReceived, cancellationUnconfirmed bool) error {
	if err == nil {
		return nil
	}
	if requestBodyBytesRead < 0 {
		requestBodyBytesRead = 0
	}
	var existing *UpstreamAttemptError
	if errors.As(err, &existing) && existing != nil {
		if existing.RequestBodyBytesRead > requestBodyBytesRead {
			requestBodyBytesRead = existing.RequestBodyBytesRead
		}
		responseReceived = responseReceived || existing.ResponseReceived
		cancellationUnconfirmed = cancellationUnconfirmed || existing.CancellationUnconfirmed
	}
	return &UpstreamAttemptError{
		Cause:                   err,
		RequestBodyBytesRead:    requestBodyBytesRead,
		ResponseReceived:        responseReceived,
		CancellationUnconfirmed: cancellationUnconfirmed,
	}
}

// UpstreamAttemptRetrySafety reports whether err carries an explicit send
// boundary and, when it does, whether retrying is safe.
func UpstreamAttemptRetrySafety(err error) (known, safe bool) {
	var attemptErr *UpstreamAttemptError
	if !errors.As(err, &attemptErr) || attemptErr == nil {
		return false, false
	}
	return true, attemptErr.RetrySafe()
}

// IsPossibleBillableRequest reports whether a failed attempt may still have
// executed upstream even though no usage response was available.
func IsPossibleBillableRequest(err error) bool {
	var attemptErr *UpstreamAttemptError
	return errors.As(err, &attemptErr) && attemptErr != nil && attemptErr.PossibleBillableRequest()
}

// IsUpstreamCancellationUnconfirmed reports cancellation after an attempt may
// already have reached the upstream.
func IsUpstreamCancellationUnconfirmed(err error) bool {
	var attemptErr *UpstreamAttemptError
	return errors.As(err, &attemptErr) && attemptErr != nil && attemptErr.CancellationUnconfirmed
}
