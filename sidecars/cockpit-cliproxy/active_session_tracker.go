package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const nonK12MaxConcurrentSessions = 4

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

type activeSessionAssignment struct {
	authID     string
	sessionKey string
}

// activeSessionTracker counts distinct sessions that currently have an HTTP,
// SSE, or WebSocket request in flight. It stores only local digests.
type activeSessionTracker struct {
	selectionMu       sync.Mutex
	mu                sync.Mutex
	requests          map[string]activeSessionAssignment
	refsByAuthSession map[string]map[string]int
	lastAuthBySession map[string]string
}

func newActiveSessionTracker() *activeSessionTracker {
	return &activeSessionTracker{
		requests:          make(map[string]activeSessionAssignment),
		refsByAuthSession: make(map[string]map[string]int),
		lastAuthBySession: make(map[string]string),
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
	requestID, sessionKey := activeSessionSelectionKeys(ctx, opts)
	t.mu.Lock()
	defer t.mu.Unlock()

	selected, err := pick(t.activeLoadsLocked(), t.activeAuthForSessionLocked(sessionKey))
	if err != nil || selected == nil || strings.TrimSpace(selected.ID) == "" || requestID == "" || sessionKey == "" {
		return selected, err
	}
	t.reserveLocked(requestID, sessionKey, strings.TrimSpace(selected.ID))
	return selected, nil
}

func (t *activeSessionTracker) reserveSelection(ctx context.Context, opts cliproxyexecutor.Options, authID string) {
	if t == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	requestID, sessionKey := activeSessionSelectionKeys(ctx, opts)
	if authID == "" || requestID == "" || sessionKey == "" {
		return
	}
	t.mu.Lock()
	t.reserveLocked(requestID, sessionKey, authID)
	t.mu.Unlock()
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
	if assignment, ok := t.requests[requestID]; ok {
		delete(t.requests, requestID)
		t.releaseAssignmentLocked(assignment)
	}
	t.mu.Unlock()
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
