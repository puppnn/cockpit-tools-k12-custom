package auth

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	defaultK12SessionTTL      = 7 * 24 * time.Hour
	defaultK12SessionCooldown = 30 * time.Second
	k12TentativeTTL           = 2 * time.Minute
	defaultK12SpilloverTTL    = time.Hour
	k12MaxActiveSessions      = 2
)

// K12QuotaSnapshot describes the latest local quota observation used only to
// decide whether a K12 account may start a new session.
type K12QuotaSnapshot struct {
	Fresh                  bool
	HourlyRemainingPercent *int
}

// K12SessionPolicyConfig enables confirmed, persistent K12 session affinity.
type K12SessionPolicyConfig struct {
	StatePath     string
	HMACKey       []byte
	TTL           time.Duration
	Cooldown      time.Duration
	SpilloverTTL  time.Duration
	IsK12         func(*Auth) bool
	IsSpillover   func(*Auth) bool
	QuotaSnapshot func(*Auth) K12QuotaSnapshot
}

type k12TentativeSelection struct {
	authID    string
	source    string
	expiresAt time.Time
}

type k12SpilloverSelection struct {
	authID    string
	source    string
	expiresAt time.Time
}

type k12CandidateReservation struct {
	auth           *Auth
	confirmedCount int
	sessionLoad    int
	spilled        bool
	reused         bool
	spilloverErr   error
}

type k12SessionPolicy struct {
	store         *k12SessionStore
	isK12         func(*Auth) bool
	isSpillover   func(*Auth) bool
	quotaSnapshot func(*Auth) K12QuotaSnapshot
	cooldown      time.Duration
	spilloverTTL  time.Duration

	mu         sync.Mutex
	tentative  map[string]k12TentativeSelection
	spillovers map[string]k12SpilloverSelection
}

func newK12SessionPolicy(cfg *K12SessionPolicyConfig) (*k12SessionPolicy, error) {
	if cfg == nil || cfg.IsK12 == nil {
		return nil, nil
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = defaultK12SessionTTL
	}
	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = defaultK12SessionCooldown
	}
	spilloverTTL := cfg.SpilloverTTL
	if spilloverTTL <= 0 {
		spilloverTTL = defaultK12SpilloverTTL
	}
	store, err := newK12SessionStore(cfg.StatePath, cfg.HMACKey, ttl)
	if store == nil {
		return nil, err
	}
	return &k12SessionPolicy{
		store:         store,
		isK12:         cfg.IsK12,
		isSpillover:   cfg.IsSpillover,
		quotaSnapshot: cfg.QuotaSnapshot,
		cooldown:      cooldown,
		spilloverTTL:  spilloverTTL,
		tentative:     make(map[string]k12TentativeSelection),
		spillovers:    make(map[string]k12SpilloverSelection),
	}, err
}

func (p *k12SessionPolicy) quota(auth *Auth) K12QuotaSnapshot {
	if p == nil || p.quotaSnapshot == nil {
		return K12QuotaSnapshot{}
	}
	return p.quotaSnapshot(auth)
}

func (p *k12SessionPolicy) canStart(auth *Auth) bool {
	if p == nil || auth == nil || !p.isK12(auth) || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	snapshot := p.quota(auth)
	return !snapshot.Fresh || snapshot.HourlyRemainingPercent == nil || *snapshot.HourlyRemainingPercent > 0
}

func (p *k12SessionPolicy) canSpillover(auth *Auth) bool {
	if p == nil || auth == nil || p.isK12(auth) || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	return p.isSpillover == nil || p.isSpillover(auth)
}

func (p *k12SessionPolicy) tentativeAuth(sessionDigest string, now time.Time) string {
	if p == nil || sessionDigest == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupTentativeLocked(now)
	return p.tentative[sessionDigest].authID
}

func (p *k12SessionPolicy) clearTentative(sessionDigest, authID string) {
	if p == nil || sessionDigest == "" {
		return
	}
	p.mu.Lock()
	if selection, ok := p.tentative[sessionDigest]; ok && (authID == "" || selection.authID == authID) {
		delete(p.tentative, sessionDigest)
	}
	p.mu.Unlock()
}

func (p *k12SessionPolicy) clearTentativeAuth(authID string) {
	if p == nil || authID == "" {
		return
	}
	p.mu.Lock()
	for digest, selection := range p.tentative {
		if selection.authID == authID {
			delete(p.tentative, digest)
		}
	}
	p.mu.Unlock()
}

func (p *k12SessionPolicy) spilloverAuth(sessionDigest string, now time.Time) string {
	if p == nil || sessionDigest == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupSpilloversLocked(now)
	selection, ok := p.spillovers[sessionDigest]
	if !ok {
		return ""
	}
	selection.expiresAt = now.Add(p.spilloverTTL)
	p.spillovers[sessionDigest] = selection
	return selection.authID
}

func (p *k12SessionPolicy) clearSpillover(sessionDigest, authID string) {
	if p == nil || sessionDigest == "" {
		return
	}
	p.mu.Lock()
	if selection, ok := p.spillovers[sessionDigest]; ok && (authID == "" || selection.authID == authID) {
		delete(p.spillovers, sessionDigest)
	}
	p.mu.Unlock()
}

func (p *k12SessionPolicy) clearSpilloverAuth(authID string) {
	if p == nil || authID == "" {
		return
	}
	p.mu.Lock()
	for digest, selection := range p.spillovers {
		if selection.authID == authID {
			delete(p.spillovers, digest)
		}
	}
	p.mu.Unlock()
}

func (p *k12SessionPolicy) cleanupTentativeLocked(now time.Time) {
	for digest, selection := range p.tentative {
		if !selection.expiresAt.After(now) {
			delete(p.tentative, digest)
		}
	}
}

func (p *k12SessionPolicy) cleanupSpilloversLocked(now time.Time) {
	for digest, selection := range p.spillovers {
		if !selection.expiresAt.After(now) {
			delete(p.spillovers, digest)
		}
	}
}

func (p *k12SessionPolicy) reserveCandidate(
	sessionDigest string,
	source string,
	k12Candidates []*Auth,
	spilloverCandidates []*Auth,
	now time.Time,
	pickK12 func([]*Auth) (*Auth, error),
	pickSpillover func([]*Auth) (*Auth, error),
) (k12CandidateReservation, error) {
	if p == nil || p.store == nil || sessionDigest == "" || len(k12Candidates) == 0 {
		return k12CandidateReservation{}, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupTentativeLocked(now)
	p.cleanupSpilloversLocked(now)

	if selection, ok := p.tentative[sessionDigest]; ok {
		for _, auth := range k12Candidates {
			if auth != nil && auth.ID == selection.authID {
				return k12CandidateReservation{auth: auth, reused: true}, nil
			}
		}
		delete(p.tentative, sessionDigest)
	}
	if selection, ok := p.spillovers[sessionDigest]; ok {
		for _, auth := range spilloverCandidates {
			if auth != nil && auth.ID == selection.authID {
				selection.expiresAt = now.Add(p.spilloverTTL)
				p.spillovers[sessionDigest] = selection
				return k12CandidateReservation{auth: auth, spilled: true, reused: true}, nil
			}
		}
		delete(p.spillovers, sessionDigest)
	}

	confirmedCounts, recentCounts := p.store.confirmedAndRecentCounts(now.Add(-p.spilloverTTL), now)
	activeLoads := make(map[string]int, len(recentCounts)+len(p.tentative))
	for authID, count := range recentCounts {
		activeLoads[authID] = count
	}
	for _, selection := range p.tentative {
		activeLoads[selection.authID]++
	}

	reservation := k12CandidateReservation{}
	underCapacity := make([]*Auth, 0, len(k12Candidates))
	for _, auth := range k12Candidates {
		if activeLoads[auth.ID] < k12MaxActiveSessions {
			underCapacity = append(underCapacity, auth)
		}
	}
	if len(underCapacity) > 0 {
		k12Candidates = underCapacity
	} else if len(spilloverCandidates) > 0 && pickSpillover != nil {
		selected, spilloverErr := pickSpillover(spilloverCandidates)
		if spilloverErr == nil && selected != nil && selected.ID != "" {
			p.spillovers[sessionDigest] = k12SpilloverSelection{
				authID:    selected.ID,
				source:    source,
				expiresAt: now.Add(p.spilloverTTL),
			}
			reservation.auth = selected
			reservation.spilled = true
			return reservation, nil
		}
		reservation.spilloverErr = spilloverErr
		if spilloverErr == nil {
			reservation.spilloverErr = fmt.Errorf("spillover selector returned an empty auth")
		}
	}

	minLoad := -1
	for _, auth := range k12Candidates {
		load := activeLoads[auth.ID]
		if minLoad < 0 || load < minLoad {
			minLoad = load
		}
	}
	balanced := make([]*Auth, 0, len(k12Candidates))
	for _, auth := range k12Candidates {
		if activeLoads[auth.ID] == minLoad {
			balanced = append(balanced, auth)
		}
	}

	bestRemaining := -1
	for _, auth := range balanced {
		snapshot := p.quota(auth)
		if snapshot.Fresh && snapshot.HourlyRemainingPercent != nil && *snapshot.HourlyRemainingPercent > bestRemaining {
			bestRemaining = *snapshot.HourlyRemainingPercent
		}
	}
	if bestRemaining >= 0 {
		withBestQuota := make([]*Auth, 0, len(balanced))
		for _, auth := range balanced {
			snapshot := p.quota(auth)
			if snapshot.Fresh && snapshot.HourlyRemainingPercent != nil && *snapshot.HourlyRemainingPercent == bestRemaining {
				withBestQuota = append(withBestQuota, auth)
			}
		}
		balanced = withBestQuota
	}

	selected, err := pickK12(balanced)
	if err != nil {
		return k12CandidateReservation{}, err
	}
	if selected == nil || selected.ID == "" {
		return k12CandidateReservation{}, fmt.Errorf("K12 selector returned an empty auth")
	}
	p.tentative[sessionDigest] = k12TentativeSelection{
		authID:    selected.ID,
		source:    source,
		expiresAt: now.Add(k12TentativeTTL),
	}
	reservation.auth = selected
	reservation.confirmedCount = confirmedCounts[selected.ID]
	reservation.sessionLoad = activeLoads[selected.ID] + 1
	return reservation, nil
}

func (s *SessionAffinitySelector) PickBeforeAvailability(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, bool, error) {
	if s == nil || s.k12 == nil || s.k12.store == nil {
		return nil, false, nil
	}
	identity, _ := extractSessionIdentities(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if identity.ID == "" {
		return nil, false, nil
	}
	digest := s.k12.store.digest(identity.ID)
	binding, ok := s.k12.store.binding(digest, time.Now())
	if !ok {
		return nil, false, nil
	}
	for _, auth := range auths {
		if auth == nil || auth.ID != binding.AuthID || !s.k12.isK12(auth) {
			continue
		}
		if auth.Disabled || auth.Status == StatusDisabled {
			_ = s.k12.store.removeAuth(auth.ID)
			s.k12.clearTentativeAuth(auth.ID)
			return nil, false, nil
		}
		if remaining := s.k12.store.cooldownRemaining(digest, auth.ID, time.Now()); remaining > 0 {
			return nil, true, newK12SessionCooldownError(remaining)
		}
		selectorLogEntry(ctx).Infof(
			"k12-session-affinity: confirmed binding hit | source=%s session=%s auth=%s provider=%s model=%s",
			identity.Source, shortSessionDigest(digest), auth.ID, provider, model,
		)
		return auth, true, nil
	}
	// Request-scoped filtering can hide an otherwise valid binding. Preserve it
	// and do not silently move the upstream conversation to another account.
	return nil, true, &Error{Code: "k12_session_auth_unavailable", Message: "confirmed K12 session account is unavailable"}
}

func (s *SessionAffinitySelector) pickK12(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, identity sessionIdentity, auths []*Auth) (*Auth, []*Auth, bool, error) {
	if s == nil || s.k12 == nil || s.k12.store == nil || identity.ID == "" {
		return nil, auths, false, nil
	}
	now := time.Now()
	digest := s.k12.store.digest(identity.ID)
	if binding, ok := s.k12.store.binding(digest, now); ok {
		for _, auth := range auths {
			if auth != nil && auth.ID == binding.AuthID && s.k12.isK12(auth) {
				if remaining := s.k12.store.cooldownRemaining(digest, auth.ID, now); remaining > 0 {
					return nil, nil, true, newK12SessionCooldownError(remaining)
				}
				return auth, nil, true, nil
			}
		}
		return nil, nil, true, &Error{Code: "k12_session_auth_unavailable", Message: "confirmed K12 session account is unavailable"}
	}
	if spilloverAuthID := s.k12.spilloverAuth(digest, now); spilloverAuthID != "" {
		available, _ := getAvailableAuths(auths, provider, model, now)
		for _, auth := range available {
			if auth != nil && auth.ID == spilloverAuthID && !s.k12.isK12(auth) {
				selectorLogEntry(ctx).Infof(
					"k12-session-affinity: spillover binding hit | source=%s session=%s auth=%s provider=%s model=%s",
					identity.Source, shortSessionDigest(digest), auth.ID, provider, model,
				)
				return auth, nil, true, nil
			}
		}
		s.k12.clearSpillover(digest, spilloverAuthID)
	}

	nonK12 := make([]*Auth, 0, len(auths))
	spilloverCandidates := make([]*Auth, 0, 1)
	k12Candidates := make([]*Auth, 0, len(auths))
	var earliestCooldown time.Duration
	for _, auth := range auths {
		if auth == nil || !s.k12.isK12(auth) {
			nonK12 = append(nonK12, auth)
			if s.k12.canSpillover(auth) {
				spilloverCandidates = append(spilloverCandidates, auth)
			}
			continue
		}
		if !s.k12.canStart(auth) {
			continue
		}
		if remaining := s.k12.store.cooldownRemaining(digest, auth.ID, now); remaining > 0 {
			if earliestCooldown == 0 || remaining < earliestCooldown {
				earliestCooldown = remaining
			}
			continue
		}
		k12Candidates = append(k12Candidates, auth)
	}

	if len(k12Candidates) > 0 {
		reservation, err := s.k12.reserveCandidate(
			digest,
			identity.Source,
			k12Candidates,
			spilloverCandidates,
			now,
			func(balanced []*Auth) (*Auth, error) {
				return s.fallback.Pick(ctx, provider, model, opts, balanced)
			},
			func(spillover []*Auth) (*Auth, error) {
				return s.fallback.Pick(ctx, provider, model, opts, spillover)
			},
		)
		if err != nil {
			return nil, nil, true, err
		}
		if reservation.auth == nil {
			return nil, nil, true, &Error{Code: "auth_unavailable", Message: "no K12 auth available for a new session"}
		}
		if reservation.reused {
			return reservation.auth, nil, true, nil
		}
		if reservation.spilloverErr != nil {
			selectorLogEntry(ctx).Debugf(
				"k12-session-affinity: spillover unavailable, keeping K12 | source=%s session=%s error=%v",
				identity.Source, shortSessionDigest(digest), reservation.spilloverErr,
			)
		}
		if reservation.spilled {
			selectorLogEntry(ctx).Infof(
				"k12-session-affinity: K12 capacity spillover | source=%s session=%s auth=%s max_active_per_k12=%d",
				identity.Source,
				shortSessionDigest(digest),
				reservation.auth.ID,
				k12MaxActiveSessions,
			)
			return reservation.auth, nil, true, nil
		}
		selectorLogEntry(ctx).Infof(
			"k12-session-affinity: tentative selection | source=%s session=%s auth=%s confirmed=%d session_load=%d remaining5h=%s",
			identity.Source,
			shortSessionDigest(digest),
			reservation.auth.ID,
			reservation.confirmedCount,
			reservation.sessionLoad,
			formatK12Remaining(s.k12.quota(reservation.auth)),
		)
		return reservation.auth, nil, true, nil
	}

	if len(nonK12) == 0 {
		if earliestCooldown > 0 {
			return nil, nil, true, newK12SessionCooldownError(earliestCooldown)
		}
		return nil, nil, true, &Error{Code: "auth_unavailable", Message: "no auth available for a new K12 session"}
	}
	return nil, nonK12, false, nil
}

func (s *SessionAffinitySelector) OnSelectionResult(ctx context.Context, result Result, opts cliproxyexecutor.Options) SelectionResultDirective {
	if s == nil || s.k12 == nil || s.k12.store == nil || result.AuthID == "" {
		return SelectionResultDirective{}
	}
	identity, _ := extractSessionIdentities(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if identity.ID == "" {
		return SelectionResultDirective{}
	}
	now := time.Now()
	digest := s.k12.store.digest(identity.ID)
	binding, confirmed := s.k12.store.binding(digest, now)
	if confirmed && binding.AuthID != result.AuthID {
		return SelectionResultDirective{}
	}
	if !confirmed {
		spilloverAuthID := s.k12.spilloverAuth(digest, now)
		if spilloverAuthID != "" {
			if spilloverAuthID != result.AuthID {
				return SelectionResultDirective{}
			}
			entry := selectorLogEntry(ctx)
			if result.Success {
				entry.Infof("k12-session-affinity: spillover binding confirmed | source=%s session=%s auth=%s", identity.Source, shortSessionDigest(digest), result.AuthID)
				return SelectionResultDirective{}
			}
			status := statusCodeFromResult(result.Error)
			if status == http.StatusUnauthorized || status == http.StatusPaymentRequired || status == http.StatusForbidden {
				s.k12.clearSpilloverAuth(result.AuthID)
			} else {
				s.k12.clearSpillover(digest, result.AuthID)
			}
			entry.Warnf("k12-session-affinity: spillover binding released after failure | source=%s session=%s auth=%s status=%d", identity.Source, shortSessionDigest(digest), result.AuthID, status)
			return SelectionResultDirective{}
		}
	}
	tentativeAuthID := s.k12.tentativeAuth(digest, now)
	if !confirmed && tentativeAuthID != result.AuthID {
		return SelectionResultDirective{}
	}

	entry := selectorLogEntry(ctx)
	if result.Success {
		if err := s.k12.store.confirm(digest, identity.Source, result.AuthID, now); err != nil {
			entry.Warnf("k12-session-affinity: persist success failed | source=%s session=%s auth=%s error=%v", identity.Source, shortSessionDigest(digest), result.AuthID, err)
		}
		s.k12.clearTentative(digest, result.AuthID)
		entry.Infof("k12-session-affinity: binding confirmed | source=%s session=%s auth=%s", identity.Source, shortSessionDigest(digest), result.AuthID)
		return SelectionResultDirective{}
	}

	status := statusCodeFromResult(result.Error)
	if status == http.StatusUnauthorized || status == http.StatusPaymentRequired || status == http.StatusForbidden {
		_ = s.k12.store.removeAuth(result.AuthID)
		s.k12.clearTentativeAuth(result.AuthID)
		entry.Warnf("k12-session-affinity: hard auth failure cleared bindings | source=%s session=%s auth=%s status=%d", identity.Source, shortSessionDigest(digest), result.AuthID, status)
		return SelectionResultDirective{StopAuthAttempt: true}
	}

	if isModelSupportResultError(result.Error) {
		s.k12.clearTentative(digest, result.AuthID)
		return SelectionResultDirective{StopAuthAttempt: true, StopCredentialFallback: confirmed}
	}

	transient := status == 0 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
	if !transient {
		s.k12.clearTentative(digest, result.AuthID)
		return SelectionResultDirective{StopCredentialFallback: confirmed && isRequestInvalidResultError(result.Error)}
	}

	streamOpenTimeout := isStreamOpenTimeoutResultError(result.Error)
	if status == http.StatusTooManyRequests || streamOpenTimeout {
		reason := "429"
		if streamOpenTimeout {
			reason = "stream_open_timeout"
		}
		until := now.Add(s.k12.cooldown)
		if confirmed {
			if err := s.k12.store.releaseSessionToCooldown(digest, result.AuthID, until); err != nil {
				entry.Warnf("k12-session-affinity: release binding for failover failed | source=%s session=%s auth=%s reason=%s error=%v", identity.Source, shortSessionDigest(digest), result.AuthID, reason, err)
			}
			s.k12.clearTentative(digest, result.AuthID)
			entry.Warnf("k12-session-affinity: confirmed binding released for failover | source=%s session=%s auth=%s reason=%s", identity.Source, shortSessionDigest(digest), result.AuthID, reason)
			return SelectionResultDirective{
				SuppressAvailabilityUpdate: true,
				StopAuthAttempt:            true,
			}
		}
		if err := s.k12.store.setCooldown(digest, result.AuthID, until); err != nil {
			entry.Warnf("k12-session-affinity: persist cooldown failed | source=%s session=%s auth=%s error=%v", identity.Source, shortSessionDigest(digest), result.AuthID, err)
		}
	}
	s.k12.clearTentative(digest, result.AuthID)
	return SelectionResultDirective{
		SuppressAvailabilityUpdate: true,
		StopAuthAttempt:            true,
		StopCredentialFallback:     confirmed,
	}
}

func isStreamOpenTimeoutResultError(err *Error) bool {
	if err == nil || err.HTTPStatus != http.StatusGatewayTimeout {
		return false
	}
	return strings.Contains(strings.ToLower(err.Message), "stream_open")
}

func (s *SessionAffinitySelector) SyncAuths(auths []*Auth) {
	if s == nil || s.k12 == nil || s.k12.store == nil {
		return
	}
	valid := make(map[string]struct{})
	validAll := make(map[string]struct{})
	for _, auth := range auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		validAll[auth.ID] = struct{}{}
		if s.k12.isK12(auth) {
			valid[auth.ID] = struct{}{}
		}
	}
	if err := s.k12.store.pruneAuths(valid, time.Now()); err != nil {
		selectorLogEntry(context.Background()).Warnf("k12-session-affinity: prune state failed: %v", err)
	}
	s.k12.mu.Lock()
	for digest, selection := range s.k12.tentative {
		if _, ok := valid[selection.authID]; !ok {
			delete(s.k12.tentative, digest)
		}
	}
	for digest, selection := range s.k12.spillovers {
		if _, ok := validAll[selection.authID]; !ok {
			delete(s.k12.spillovers, digest)
		}
	}
	s.k12.mu.Unlock()
}

func formatK12Remaining(snapshot K12QuotaSnapshot) string {
	if !snapshot.Fresh {
		return "stale"
	}
	if snapshot.HourlyRemainingPercent == nil {
		return "unknown"
	}
	return strconv.Itoa(*snapshot.HourlyRemainingPercent) + "%"
}

func shortSessionDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

type k12SessionCooldownError struct {
	retryAfter time.Duration
}

func newK12SessionCooldownError(retryAfter time.Duration) *k12SessionCooldownError {
	if retryAfter < 0 {
		retryAfter = 0
	}
	return &k12SessionCooldownError{retryAfter: retryAfter}
}

func (e *k12SessionCooldownError) Error() string {
	seconds := int(e.retryAfter.Round(time.Second).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf(`{"error":{"code":"k12_session_cooldown","message":"Confirmed K12 session is cooling down","reset_seconds":%d}}`, seconds)
}

func (e *k12SessionCooldownError) StatusCode() int {
	return http.StatusTooManyRequests
}

func (e *k12SessionCooldownError) Headers() http.Header {
	seconds := int(e.retryAfter.Round(time.Second).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Retry-After", strconv.Itoa(seconds))
	return headers
}

func isRequestInvalidResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := err.StatusCode()
	return status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}
