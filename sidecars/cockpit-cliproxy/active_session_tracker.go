package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	nonK12MaxConcurrentSessions = 4
	closedRequestTombstoneTTL   = 5 * time.Minute
)

type activeSessionCapacityError struct {
	limit int
}

func (e *activeSessionCapacityError) Error() string {
	limit := nonK12MaxConcurrentSessions
	if e != nil && e.limit > 0 {
		limit = e.limit
	}
	return fmt.Sprintf("no auth available: all non-K12 candidates reached the %d-session concurrency limit", limit)
}

type activeSessionConflictError struct {
	targetAuthID string
}

func (*activeSessionConflictError) Error() string {
	return "active session is still running on another auth"
}

type activeSessionAssignment struct {
	authID     string
	sessionKey string
}

type closedRequestTombstone struct {
	requestID string
	expiresAt time.Time
}

// activeSessionTracker counts distinct sessions that currently have an HTTP,
// SSE, or WebSocket request in flight. It stores only local digests.
type activeSessionTracker struct {
	selectionMu        sync.Mutex
	mu                 sync.Mutex
	requests           map[string]activeSessionAssignment
	refsByAuthSession  map[string]map[string]int
	lastAuthBySession  map[string]string
	closedRequests     map[string]time.Time
	closedRequestQueue []closedRequestTombstone
	closedRequestHead  int
	changed            chan struct{}
}

func newActiveSessionTracker() *activeSessionTracker {
	return &activeSessionTracker{
		requests:          make(map[string]activeSessionAssignment),
		refsByAuthSession: make(map[string]map[string]int),
		lastAuthBySession: make(map[string]string),
		closedRequests:    make(map[string]time.Time),
		changed:           make(chan struct{}),
	}
}

func activeSessionSelectionKeys(ctx context.Context, opts cliproxyexecutor.Options) (string, string) {
	if ctx == nil {
		return "", ""
	}
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	if requestID == "" {
		return "", ""
	}
	sessionID := strings.TrimSpace(coreauth.ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata))
	if sessionID == "" {
		sessionID = "request:" + requestID
	}
	digest := sha256.Sum256([]byte("cockpit-tools:active-session:v1\n" + sessionID))
	return requestID, hex.EncodeToString(digest[:])
}

func (t *activeSessionTracker) selectAndReserve(
	ctx context.Context,
	opts cliproxyexecutor.Options,
	pick func(activeLoads map[string]int, activeAuthID string) (*coreauth.Auth, error),
) (*coreauth.Auth, error) {
	if pick == nil {
		return nil, nil
	}
	if t == nil {
		return pick(nil, "")
	}
	if ctx == nil || ctx.Err() != nil {
		if ctx != nil {
			return nil, ctx.Err()
		}
		return nil, context.Canceled
	}
	requestID, sessionKey := activeSessionSelectionKeys(ctx, opts)
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	t.cleanupClosedRequestsLocked(now)
	if t.requestClosedLocked(requestID, now) {
		return nil, context.Canceled
	}
	activeAuthID := t.activeAuthForSessionLocked(sessionKey)
	hasSibling := t.hasSiblingRequestLocked(requestID, sessionKey, activeAuthID)
	selected, err := pick(t.activeLoadsLocked(), activeAuthID)
	if err != nil || selected == nil || strings.TrimSpace(selected.ID) == "" || requestID == "" || sessionKey == "" {
		return selected, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if activeAuthID != "" && selected.ID != activeAuthID && hasSibling {
		t.suspendRequestLocked(requestID)
		return nil, &activeSessionConflictError{targetAuthID: strings.TrimSpace(selected.ID)}
	}
	t.reserveLocked(requestID, sessionKey, strings.TrimSpace(selected.ID))
	return selected, nil
}

func (t *activeSessionTracker) reserveSelection(ctx context.Context, opts cliproxyexecutor.Options, authID string) error {
	if t == nil {
		return nil
	}
	if ctx == nil || ctx.Err() != nil {
		if ctx != nil {
			return ctx.Err()
		}
		return context.Canceled
	}
	authID = strings.TrimSpace(authID)
	requestID, sessionKey := activeSessionSelectionKeys(ctx, opts)
	if authID == "" || requestID == "" || sessionKey == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.cleanupClosedRequestsLocked(now)
	if t.requestClosedLocked(requestID, now) {
		return context.Canceled
	}
	activeAuthID := t.activeAuthForSessionLocked(sessionKey)
	if activeAuthID != "" && activeAuthID != authID && t.hasSiblingRequestLocked(requestID, sessionKey, activeAuthID) {
		t.suspendRequestLocked(requestID)
		return &activeSessionConflictError{targetAuthID: authID}
	}
	t.reserveLocked(requestID, sessionKey, authID)
	return nil
}

func (t *activeSessionTracker) waitForSiblingRelease(ctx context.Context, opts cliproxyexecutor.Options, targetAuthID string) error {
	if t == nil {
		return nil
	}
	if ctx == nil {
		return context.Canceled
	}
	requestID, sessionKey := activeSessionSelectionKeys(ctx, opts)
	targetAuthID = strings.TrimSpace(targetAuthID)
	if requestID == "" || sessionKey == "" {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		t.mu.Lock()
		now := time.Now()
		t.cleanupClosedRequestsLocked(now)
		if t.requestClosedLocked(requestID, now) {
			t.mu.Unlock()
			return context.Canceled
		}
		activeAuthID := t.activeAuthForSessionLocked(sessionKey)
		if activeAuthID == targetAuthID || !t.hasSiblingRequestLocked(requestID, sessionKey, activeAuthID) {
			t.mu.Unlock()
			return nil
		}
		changed := t.changed
		if changed == nil {
			changed = make(chan struct{})
			t.changed = changed
		}
		t.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (t *activeSessionTracker) releaseRequest(requestID string) {
	if t == nil {
		return
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	t.mu.Lock()
	now := time.Now()
	t.cleanupClosedRequestsLocked(now)
	if t.closedRequests == nil {
		t.closedRequests = make(map[string]time.Time)
	}
	expiresAt := now.Add(closedRequestTombstoneTTL)
	t.closedRequests[requestID] = expiresAt
	t.closedRequestQueue = append(t.closedRequestQueue, closedRequestTombstone{
		requestID: requestID,
		expiresAt: expiresAt,
	})
	if assignment, ok := t.requests[requestID]; ok {
		delete(t.requests, requestID)
		t.releaseAssignmentLocked(assignment)
	}
	t.notifyChangedLocked()
	t.mu.Unlock()
}

func (t *activeSessionTracker) cleanupClosedRequestsLocked(now time.Time) {
	for t.closedRequestHead < len(t.closedRequestQueue) {
		entry := t.closedRequestQueue[t.closedRequestHead]
		if entry.expiresAt.After(now) {
			break
		}
		if expiresAt, ok := t.closedRequests[entry.requestID]; ok && expiresAt.Equal(entry.expiresAt) {
			delete(t.closedRequests, entry.requestID)
		}
		t.closedRequestHead++
	}
	if t.closedRequestHead == len(t.closedRequestQueue) {
		t.closedRequestQueue = nil
		t.closedRequestHead = 0
		return
	}
	if t.closedRequestHead >= 1024 && t.closedRequestHead*2 >= len(t.closedRequestQueue) {
		remaining := append([]closedRequestTombstone(nil), t.closedRequestQueue[t.closedRequestHead:]...)
		t.closedRequestQueue = remaining
		t.closedRequestHead = 0
	}
}

func (t *activeSessionTracker) requestClosedLocked(requestID string, now time.Time) bool {
	if requestID == "" {
		return false
	}
	expiresAt, ok := t.closedRequests[requestID]
	return ok && expiresAt.After(now)
}

func (t *activeSessionTracker) hasSiblingRequestLocked(requestID, sessionKey, authID string) bool {
	if sessionKey == "" || authID == "" {
		return false
	}
	refs := t.refsByAuthSession[authID][sessionKey]
	if assignment, ok := t.requests[requestID]; ok && assignment.authID == authID && assignment.sessionKey == sessionKey {
		refs--
	}
	return refs > 0
}

func (t *activeSessionTracker) reserveLocked(requestID, sessionKey, authID string) {
	if previous, ok := t.requests[requestID]; ok {
		if previous.authID == authID && previous.sessionKey == sessionKey {
			return
		}
		t.releaseAssignmentLocked(previous)
	}
	refs := t.refsByAuthSession[authID]
	if refs == nil {
		refs = make(map[string]int)
		t.refsByAuthSession[authID] = refs
	}
	refs[sessionKey]++
	t.requests[requestID] = activeSessionAssignment{authID: authID, sessionKey: sessionKey}
	t.lastAuthBySession[sessionKey] = authID
	t.notifyChangedLocked()
}

func (t *activeSessionTracker) suspendRequestLocked(requestID string) {
	assignment, ok := t.requests[requestID]
	if !ok {
		return
	}
	delete(t.requests, requestID)
	t.releaseAssignmentLocked(assignment)
	t.notifyChangedLocked()
}

func (t *activeSessionTracker) notifyChangedLocked() {
	if t.changed != nil {
		close(t.changed)
	}
	t.changed = make(chan struct{})
}

func (t *activeSessionTracker) releaseAssignmentLocked(assignment activeSessionAssignment) {
	refs := t.refsByAuthSession[assignment.authID]
	if refs == nil {
		return
	}
	if refs[assignment.sessionKey] <= 1 {
		delete(refs, assignment.sessionKey)
	} else {
		refs[assignment.sessionKey]--
	}
	if len(refs) == 0 {
		delete(t.refsByAuthSession, assignment.authID)
	}
	if t.lastAuthBySession[assignment.sessionKey] != assignment.authID || refs[assignment.sessionKey] > 0 {
		return
	}
	delete(t.lastAuthBySession, assignment.sessionKey)
	for authID, otherRefs := range t.refsByAuthSession {
		if otherRefs[assignment.sessionKey] > 0 {
			t.lastAuthBySession[assignment.sessionKey] = authID
			break
		}
	}
}

func (t *activeSessionTracker) activeLoadsLocked() map[string]int {
	loads := make(map[string]int, len(t.refsByAuthSession))
	for authID, refs := range t.refsByAuthSession {
		loads[authID] = len(refs)
	}
	return loads
}

func (t *activeSessionTracker) activeAuthForSessionLocked(sessionKey string) string {
	if sessionKey == "" {
		return ""
	}
	authID := t.lastAuthBySession[sessionKey]
	if authID != "" && t.refsByAuthSession[authID][sessionKey] > 0 {
		return authID
	}
	for candidate, refs := range t.refsByAuthSession {
		if refs[sessionKey] > 0 {
			t.lastAuthBySession[sessionKey] = candidate
			return candidate
		}
	}
	delete(t.lastAuthBySession, sessionKey)
	return ""
}

func (t *activeSessionTracker) activeLoad(authID string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.refsByAuthSession[strings.TrimSpace(authID)])
}
