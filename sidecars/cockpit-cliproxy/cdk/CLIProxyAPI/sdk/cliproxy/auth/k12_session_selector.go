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
	defaultK12SessionTTL              = 7 * 24 * time.Hour
	defaultK12SessionCooldown         = 30 * time.Second
	defaultK12NewSessionQuarantine    = 6 * time.Hour
	k12DeactivatedWorkspaceQuarantine = 7 * 24 * time.Hour
	k12TentativeTTL                   = 2 * time.Minute
	defaultK12SpilloverTTL            = time.Hour
	k12MaxConcurrentSessionStarts     = 2
)

// K12QuotaSnapshot describes the latest local quota observation used only to
// decide whether a K12 account may start a new session.
type K12QuotaSnapshot struct {
	Fresh                  bool
	UpdatedAt              time.Time
	HourlyRemainingPercent *int
	WeeklyRemainingPercent *int
}

// K12SessionPolicyConfig enables confirmed, persistent K12 session affinity.
type K12SessionPolicyConfig struct {
	StatePath            string
	HMACKey              []byte
	TTL                  time.Duration
	Cooldown             time.Duration
	NewSessionQuarantine time.Duration
	SpilloverTTL         time.Duration
	IsK12                func(*Auth) bool
	IsSpillover          func(*Auth) bool
	QuotaSnapshot        func(*Auth) K12QuotaSnapshot
	ActiveSessionLoad    func(*Auth) int
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
	recovery  bool
}

type k12CredentialAttemptKey struct {
	sessionDigest string
	authID        string
}

type k12CredentialAttemptState struct {
	active         map[uint64]struct{}
	resolved       map[uint64]struct{}
	cohortMax      uint64
	retiredThrough uint64
	expiresAt      time.Time
	k12            bool
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
	store                *k12SessionStore
	isK12                func(*Auth) bool
	isSpillover          func(*Auth) bool
	quotaSnapshot        func(*Auth) K12QuotaSnapshot
	activeSessionLoad    func(*Auth) int
	cooldown             time.Duration
	newSessionQuarantine time.Duration
	spilloverTTL         time.Duration

	mu         sync.Mutex
	tentative  map[string]k12TentativeSelection
	spillovers map[string]k12SpilloverSelection
	attempts   map[k12CredentialAttemptKey]k12CredentialAttemptState
	// preferSpilloverUntil is a one-shot recovery hint after a K12 429 or
	// stream-open timeout. It is intentionally in-memory: the persisted binding
	// cooldown remains the source of truth across restarts.
	preferSpilloverUntil map[string]time.Time
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
	newSessionQuarantine := cfg.NewSessionQuarantine
	if newSessionQuarantine <= 0 {
		newSessionQuarantine = defaultK12NewSessionQuarantine
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
		store:                store,
		isK12:                cfg.IsK12,
		isSpillover:          cfg.IsSpillover,
		quotaSnapshot:        cfg.QuotaSnapshot,
		activeSessionLoad:    cfg.ActiveSessionLoad,
		cooldown:             cooldown,
		newSessionQuarantine: newSessionQuarantine,
		spilloverTTL:         spilloverTTL,
		tentative:            make(map[string]k12TentativeSelection),
		spillovers:           make(map[string]k12SpilloverSelection),
		attempts:             make(map[k12CredentialAttemptKey]k12CredentialAttemptState),
		preferSpilloverUntil: make(map[string]time.Time),
	}, err
}

func (p *k12SessionPolicy) quota(auth *Auth) K12QuotaSnapshot {
	if p == nil || p.quotaSnapshot == nil {
		return K12QuotaSnapshot{}
	}
	return p.quotaSnapshot(auth)
}

func (p *k12SessionPolicy) canStart(auth *Auth, now time.Time) bool {
	if p == nil || auth == nil || !p.isK12(auth) || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	snapshot := p.quota(auth)
	// A known exhausted weekly window cannot start or continue a tentative
	// session, even when the snapshot is stale. Confirmed bindings bypass
	// canStart and remain eligible for upstream-controlled continuation.
	if snapshot.WeeklyRemainingPercent != nil && *snapshot.WeeklyRemainingPercent <= 0 {
		return false
	}
	refreshedQuotaUsable := snapshot.Fresh && snapshot.HourlyRemainingPercent != nil && *snapshot.HourlyRemainingPercent > 0
	if p.store != nil && p.store.newSessionQuarantineRemaining(auth.ID, snapshot.UpdatedAt, refreshedQuotaUsable, now) > 0 {
		return false
	}
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

func (p *k12SessionPolicy) spilloverSelection(sessionDigest string, now time.Time) (k12SpilloverSelection, bool) {
	if p == nil || sessionDigest == "" {
		return k12SpilloverSelection{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupSpilloversLocked(now)
	selection, ok := p.spillovers[sessionDigest]
	return selection, ok
}

func (p *k12SessionPolicy) spilloverAuth(sessionDigest string, now time.Time) string {
	selection, ok := p.spilloverSelection(sessionDigest, now)
	if !ok {
		return ""
	}
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

func (p *k12SessionPolicy) markSpilloverPreferred(sessionDigest string, until time.Time) {
	if p == nil || sessionDigest == "" || until.IsZero() {
		return
	}
	p.mu.Lock()
	if current := p.preferSpilloverUntil[sessionDigest]; until.After(current) {
		p.preferSpilloverUntil[sessionDigest] = until
	}
	p.mu.Unlock()
}

func (p *k12SessionPolicy) recoveryUntil(now time.Time, retryAfter *time.Duration) time.Time {
	wait := p.cooldown
	if retryAfter != nil && *retryAfter > wait {
		wait = *retryAfter
	}
	if wait > defaultK12SessionTTL {
		wait = defaultK12SessionTTL
	}
	return now.Add(wait)
}

func (p *k12SessionPolicy) clearSpilloverPreference(sessionDigest string) {
	if p == nil || sessionDigest == "" {
		return
	}
	p.mu.Lock()
	delete(p.preferSpilloverUntil, sessionDigest)
	p.mu.Unlock()
}

func (p *k12SessionPolicy) quarantineAfterPaymentRequired(
	authID string,
	sessionDigest string,
	releaseSession bool,
	clearTentative bool,
	hard bool,
	until time.Time,
	now time.Time,
) error {
	if p == nil || p.store == nil || authID == "" || sessionDigest == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupTentativeLocked(now)
	p.cleanupSpilloverPreferencesLocked(now)
	cleanupDigest := ""
	if releaseSession {
		cleanupDigest = sessionDigest
	}
	err := p.store.quarantineNewSessions(authID, cleanupDigest, hard, until, now, hard)
	if hard {
		for digest, selection := range p.tentative {
			if selection.authID == authID {
				delete(p.tentative, digest)
			}
		}
	} else if clearTentative {
		if selection, ok := p.tentative[sessionDigest]; ok && selection.authID == authID {
			delete(p.tentative, sessionDigest)
		}
	}
	preferenceUntil := now.Add(p.cooldown)
	if current := p.preferSpilloverUntil[sessionDigest]; preferenceUntil.After(current) {
		p.preferSpilloverUntil[sessionDigest] = preferenceUntil
	}
	return err
}

func (p *k12SessionPolicy) selectionAttemptTTL() time.Duration {
	if p != nil && p.store != nil && p.store.ttl > 0 {
		return p.store.ttl
	}
	return defaultK12SessionTTL
}

func (p *k12SessionPolicy) registerSelectionAttempt(sessionDigest, authID string, opts cliproxyexecutor.Options, now time.Time, isK12 bool) {
	attemptID := selectionAttemptIDFromOptions(opts)
	if p == nil || sessionDigest == "" || authID == "" || attemptID == 0 {
		return
	}
	key := k12CredentialAttemptKey{sessionDigest: sessionDigest, authID: authID}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupSelectionAttemptsLocked(now)
	state := p.attempts[key]
	if attemptID <= state.retiredThrough {
		return
	}
	if _, resolved := state.resolved[attemptID]; resolved {
		return
	}
	if state.active == nil {
		state.active = make(map[uint64]struct{})
	}
	state.active[attemptID] = struct{}{}
	if attemptID > state.cohortMax {
		state.cohortMax = attemptID
	}
	state.k12 = state.k12 || isK12
	state.expiresAt = now.Add(p.selectionAttemptTTL())
	p.attempts[key] = state
}

// resolveSelectionAttempt distinguishes policy-managed selections from ordinary
// auths and accepts only results that may still change affinity. The first
// active success retires the entire cohort.
func (p *k12SessionPolicy) resolveSelectionAttempt(sessionDigest, authID string, opts cliproxyexecutor.Options, success bool, now time.Time) (known, accepted, isK12 bool) {
	attemptID := selectionAttemptIDFromOptions(opts)
	if p == nil || sessionDigest == "" || authID == "" || attemptID == 0 {
		return false, true, false
	}
	key := k12CredentialAttemptKey{sessionDigest: sessionDigest, authID: authID}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupSelectionAttemptsLocked(now)
	state, ok := p.attempts[key]
	if !ok {
		return false, true, false
	}
	if attemptID <= state.retiredThrough {
		return true, false, state.k12
	}
	if _, resolved := state.resolved[attemptID]; resolved {
		return true, false, state.k12
	}
	if _, active := state.active[attemptID]; !active {
		// A newer ID that was never registered for this credential may belong to
		// an ordinary non-K12 selection after an older spillover expired.
		return false, true, false
	}
	if success {
		if state.cohortMax > state.retiredThrough {
			state.retiredThrough = state.cohortMax
		}
		state.active = nil
		state.resolved = nil
		state.cohortMax = 0
		state.expiresAt = now.Add(p.selectionAttemptTTL())
		p.attempts[key] = state
		return true, true, state.k12
	}
	delete(state.active, attemptID)
	if state.resolved == nil {
		state.resolved = make(map[uint64]struct{})
	}
	state.resolved[attemptID] = struct{}{}
	if len(state.active) > 0 {
		state.expiresAt = now.Add(p.selectionAttemptTTL())
		p.attempts[key] = state
		return true, false, state.k12
	}
	if state.cohortMax > state.retiredThrough {
		state.retiredThrough = state.cohortMax
	}
	state.active = nil
	state.resolved = nil
	state.cohortMax = 0
	state.expiresAt = now.Add(p.selectionAttemptTTL())
	p.attempts[key] = state
	return true, true, state.k12
}

func (p *k12SessionPolicy) commitK12Success(sessionDigest, source, authID string, now time.Time) (bool, error) {
	if p == nil || p.store == nil || sessionDigest == "" || authID == "" {
		return false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupTentativeLocked(now)
	binding, confirmed := p.store.binding(sessionDigest, now)
	tentative, tentativeOK := p.tentative[sessionDigest]
	if (!confirmed || binding.AuthID != authID) && (!tentativeOK || tentative.authID != authID) {
		return false, nil
	}
	err := p.store.confirm(sessionDigest, source, authID, now)
	// Keep the runtime state single-valued even if persistence failed after the
	// store updated its in-memory binding.
	delete(p.tentative, sessionDigest)
	delete(p.spillovers, sessionDigest)
	delete(p.preferSpilloverUntil, sessionDigest)
	return true, err
}

func (p *k12SessionPolicy) commitSpilloverSuccess(sessionDigest, authID string, now time.Time) (bool, error) {
	if p == nil || p.store == nil || sessionDigest == "" || authID == "" {
		return false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupSpilloversLocked(now)
	spillover, ok := p.spillovers[sessionDigest]
	if !ok || spillover.authID != authID {
		return false, nil
	}
	// Spillover keeps the request alive while a confirmed K12 binding is
	// temporarily rejected. The original binding remains authoritative after
	// the recovery window instead of being permanently replaced by Plus.
	delete(p.tentative, sessionDigest)
	delete(p.preferSpilloverUntil, sessionDigest)
	return true, nil
}

func (p *k12SessionPolicy) reservePreferredSpillover(
	sessionDigest string,
	source string,
	candidates []*Auth,
	now time.Time,
	pick func([]*Auth) (*Auth, error),
) (*Auth, bool, error) {
	if p == nil || sessionDigest == "" || len(candidates) == 0 || pick == nil {
		return nil, false, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupSpilloversLocked(now)
	p.cleanupSpilloverPreferencesLocked(now)
	preferenceUntil, ok := p.preferSpilloverUntil[sessionDigest]
	if !ok {
		return nil, false, nil
	}
	selected, err := pick(candidates)
	if err != nil {
		return nil, true, err
	}
	if selected == nil || selected.ID == "" {
		return nil, true, fmt.Errorf("preferred spillover selector returned an empty auth")
	}
	p.spillovers[sessionDigest] = k12SpilloverSelection{
		authID:    selected.ID,
		source:    source,
		expiresAt: preferenceUntil,
		recovery:  true,
	}
	delete(p.preferSpilloverUntil, sessionDigest)
	return selected, true, nil
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

func (p *k12SessionPolicy) cleanupSpilloverPreferencesLocked(now time.Time) {
	for digest, until := range p.preferSpilloverUntil {
		if !until.After(now) {
			delete(p.preferSpilloverUntil, digest)
		}
	}
}

func (p *k12SessionPolicy) cleanupSelectionAttemptsLocked(now time.Time) {
	for key, state := range p.attempts {
		if !state.expiresAt.After(now) {
			delete(p.attempts, key)
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
	eligibleK12 := make([]*Auth, 0, len(k12Candidates))
	for _, auth := range k12Candidates {
		if p.canStart(auth, now) {
			eligibleK12 = append(eligibleK12, auth)
		}
	}
	k12Candidates = eligibleK12
	if len(k12Candidates) == 0 {
		return k12CandidateReservation{}, nil
	}

	if selection, ok := p.tentative[sessionDigest]; ok {
		for _, auth := range k12Candidates {
			if auth != nil && auth.ID == selection.authID {
				return k12CandidateReservation{auth: auth, reused: true}, nil
			}
		}
		delete(p.tentative, sessionDigest)
	}
	spilloverSelection, hasSpillover := p.spillovers[sessionDigest]
	var reusableSpillover *Auth
	if hasSpillover {
		for _, auth := range spilloverCandidates {
			if auth != nil && auth.ID == spilloverSelection.authID {
				reusableSpillover = auth
				break
			}
		}
		if reusableSpillover == nil {
			delete(p.spillovers, sessionDigest)
			hasSpillover = false
		}
	}

	confirmedCounts := p.store.confirmedCounts(now)
	p.cleanupSelectionAttemptsLocked(now)
	activeSessions := make(map[string]map[string]struct{}, len(p.attempts)+len(p.tentative))
	addActiveSession := func(authID, digest string) {
		if authID == "" || digest == "" {
			return
		}
		if activeSessions[authID] == nil {
			activeSessions[authID] = make(map[string]struct{})
		}
		activeSessions[authID][digest] = struct{}{}
	}
	for key, state := range p.attempts {
		if state.k12 && len(state.active) > 0 {
			addActiveSession(key.authID, key.sessionDigest)
		}
	}
	for digest, selection := range p.tentative {
		addActiveSession(selection.authID, digest)
	}
	activeLoads := make(map[string]int, len(activeSessions))
	for authID, sessions := range activeSessions {
		activeLoads[authID] = len(sessions)
	}
	if p.activeSessionLoad != nil {
		for _, auth := range k12Candidates {
			load := p.activeSessionLoad(auth)
			if load > activeLoads[auth.ID] {
				activeLoads[auth.ID] = load
			}
		}
	}

	reservation := k12CandidateReservation{}
	underCapacity := make([]*Auth, 0, len(k12Candidates))
	for _, auth := range k12Candidates {
		if activeLoads[auth.ID] < k12MaxConcurrentSessionStarts {
			underCapacity = append(underCapacity, auth)
		}
	}
	if len(underCapacity) > 0 {
		k12Candidates = underCapacity
		if hasSpillover {
			delete(p.spillovers, sessionDigest)
		}
	} else if len(spilloverCandidates) > 0 && pickSpillover != nil {
		if reusableSpillover != nil {
			return k12CandidateReservation{auth: reusableSpillover, spilled: true, reused: true}, nil
		}
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
	} else {
		// A stale snapshot must not make a K12 ineligible, but its last known
		// positive value is still a useful ordering hint. Prefer it over accounts
		// last seen at zero, then let the upstream request verify availability.
		bestKnownPositive := 0
		for _, auth := range balanced {
			snapshot := p.quota(auth)
			if snapshot.HourlyRemainingPercent != nil &&
				*snapshot.HourlyRemainingPercent > bestKnownPositive &&
				*snapshot.HourlyRemainingPercent <= 100 {
				bestKnownPositive = *snapshot.HourlyRemainingPercent
			}
		}
		if bestKnownPositive > 0 {
			withBestKnownQuota := make([]*Auth, 0, len(balanced))
			for _, auth := range balanced {
				snapshot := p.quota(auth)
				if snapshot.HourlyRemainingPercent != nil && *snapshot.HourlyRemainingPercent == bestKnownPositive {
					withBestKnownQuota = append(withBestKnownQuota, auth)
				}
			}
			balanced = withBestKnownQuota
		}
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
	now := time.Now()
	binding, ok := s.k12.store.binding(digest, now)
	if !ok {
		spilloverSelection, exists := s.k12.spilloverSelection(digest, now)
		if !exists || !spilloverSelection.recovery {
			return nil, false, nil
		}
		available, err := getAvailableAuths(auths, provider, model, now)
		if err == nil {
			for _, auth := range available {
				if auth != nil && auth.ID == spilloverSelection.authID && s.k12.canSpillover(auth) {
					selectorLogEntry(ctx).Infof(
						"k12-session-affinity: recovery spillover retained after K12 hard release | source=%s session=%s auth=%s provider=%s model=%s",
						identity.Source, shortSessionDigest(digest), auth.ID, provider, model,
					)
					s.k12.registerSelectionAttempt(digest, auth.ID, opts, now, false)
					return auth, true, nil
				}
			}
		}
		s.k12.clearSpillover(digest, spilloverSelection.authID)
		return nil, false, nil
	}
	if _, excluded := excludedAuthIDsFromOptions(opts)[binding.AuthID]; excluded {
		selectorLogEntry(ctx).Infof(
			"k12-session-affinity: confirmed binding excluded for request failover | source=%s session=%s auth=%s",
			identity.Source, shortSessionDigest(digest), binding.AuthID,
		)
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
		if remaining := s.k12.store.cooldownRemaining(digest, auth.ID, now); remaining > 0 {
			if spilloverAuthID := s.k12.spilloverAuth(digest, now); spilloverAuthID != "" {
				available, err := getAvailableAuths(auths, provider, model, now)
				if err == nil {
					for _, spillover := range available {
						if spillover != nil && spillover.ID == spilloverAuthID && s.k12.canSpillover(spillover) {
							selectorLogEntry(ctx).Infof(
								"k12-session-affinity: temporary spillover binding hit during K12 cooldown | source=%s session=%s auth=%s k12_auth=%s retry_after=%s provider=%s model=%s",
								identity.Source, shortSessionDigest(digest), spillover.ID, auth.ID, remaining.Round(time.Second), provider, model,
							)
							s.k12.registerSelectionAttempt(digest, spillover.ID, opts, now, false)
							return spillover, true, nil
						}
					}
				}
				s.k12.clearSpillover(digest, spilloverAuthID)
			}
			selectorLogEntry(ctx).Infof(
				"k12-session-affinity: confirmed binding suspended for failover | source=%s session=%s auth=%s retry_after=%s",
				identity.Source, shortSessionDigest(digest), auth.ID, remaining.Round(time.Second),
			)
			return nil, false, nil
		}
		if spilloverAuthID := s.k12.spilloverAuth(digest, now); spilloverAuthID != "" {
			s.k12.clearSpillover(digest, spilloverAuthID)
			s.k12.clearSpilloverPreference(digest)
			selectorLogEntry(ctx).Infof(
				"k12-session-affinity: K12 cooldown expired, released temporary spillover | source=%s session=%s auth=%s spillover=%s",
				identity.Source, shortSessionDigest(digest), auth.ID, spilloverAuthID,
			)
		}
		selectorLogEntry(ctx).Infof(
			"k12-session-affinity: confirmed binding hit | source=%s session=%s auth=%s provider=%s model=%s",
			identity.Source, shortSessionDigest(digest), auth.ID, provider, model,
		)
		s.k12.registerSelectionAttempt(digest, auth.ID, opts, now, true)
		return auth, true, nil
	}
	if remaining := s.k12.store.cooldownRemaining(digest, binding.AuthID, now); remaining > 0 {
		return nil, false, nil
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
	excludedAuthIDs := excludedAuthIDsFromOptions(opts)
	suspendedAuthID := ""
	var earliestCooldown time.Duration
	if binding, ok := s.k12.store.binding(digest, now); ok {
		if _, excluded := excludedAuthIDs[binding.AuthID]; excluded {
			suspendedAuthID = binding.AuthID
			earliestCooldown = s.k12.cooldown
		} else if remaining := s.k12.store.cooldownRemaining(digest, binding.AuthID, now); remaining > 0 {
			suspendedAuthID = binding.AuthID
			earliestCooldown = remaining
		} else {
			for _, auth := range auths {
				if auth != nil && auth.ID == binding.AuthID && s.k12.isK12(auth) {
					return auth, nil, true, nil
				}
			}
			return nil, nil, true, &Error{Code: "k12_session_auth_unavailable", Message: "confirmed K12 session account is unavailable"}
		}
	}
	nonK12 := make([]*Auth, 0, len(auths))
	spilloverCandidates := make([]*Auth, 0, 1)
	k12Candidates := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || !s.k12.isK12(auth) {
			nonK12 = append(nonK12, auth)
			if s.k12.canSpillover(auth) {
				spilloverCandidates = append(spilloverCandidates, auth)
			}
			continue
		}
		if auth.ID == suspendedAuthID {
			continue
		}
		if !s.k12.canStart(auth, now) {
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

	// A stream-open timeout explicitly prefers paid spillover to avoid repeating
	// a slow bootstrap. A 429 instead tries another quota-eligible K12 first;
	// only when no K12 candidate remains do we create a temporary spillover.
	if len(k12Candidates) == 0 && earliestCooldown > 0 {
		s.k12.markSpilloverPreferred(digest, now.Add(earliestCooldown))
	}
	if selected, preferred, err := s.k12.reservePreferredSpillover(
		digest,
		identity.Source,
		spilloverCandidates,
		now,
		func(spillover []*Auth) (*Auth, error) {
			return s.fallback.Pick(ctx, provider, model, opts, spillover)
		},
	); preferred {
		if err == nil && selected != nil {
			selectorLogEntry(ctx).Infof(
				"k12-session-affinity: failure recovery spillover | source=%s session=%s auth=%s provider=%s model=%s",
				identity.Source, shortSessionDigest(digest), selected.ID, provider, model,
			)
			return selected, nil, true, nil
		}
		selectorLogEntry(ctx).WithError(err).Debugf(
			"k12-session-affinity: preferred spillover unavailable, continuing with K12 | source=%s session=%s",
			identity.Source, shortSessionDigest(digest),
		)
	}
	if suspendedAuthID != "" && len(k12Candidates) == 0 {
		if len(nonK12) > 0 {
			selectorLogEntry(ctx).Infof(
				"k12-session-affinity: confirmed binding unavailable, delegating to non-K12 fallback | source=%s session=%s auth=%s candidates=%d",
				identity.Source, shortSessionDigest(digest), suspendedAuthID, len(nonK12),
			)
			return nil, nonK12, false, nil
		}
		if earliestCooldown > 0 {
			return nil, nil, true, newK12SessionCooldownError(earliestCooldown)
		}
		return nil, nil, true, &Error{Code: "k12_session_auth_unavailable", Message: "confirmed K12 session account is temporarily unavailable"}
	}

	if s.k12.tentativeAuth(digest, now) == "" {
		preferredK12 := s.preferredNewSessionAuths(k12Candidates)
		availableNonK12, _ := getAvailableAuths(nonK12, provider, model, now)
		preferredNonK12 := s.preferredNewSessionAuths(availableNonK12)
		if len(preferredNonK12) > 0 {
			spilloverCandidates = preferredNonK12
		}
		if len(preferredK12) > 0 {
			k12Candidates = preferredK12
		}
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
			if len(nonK12) > 0 {
				return nil, nonK12, false, nil
			}
			return nil, nil, true, &Error{Code: "auth_unavailable", Message: "no eligible auth available for a new session"}
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
				"k12-session-affinity: K12 capacity spillover | source=%s session=%s auth=%s max_concurrent_starts_per_k12=%d",
				identity.Source,
				shortSessionDigest(digest),
				reservation.auth.ID,
				k12MaxConcurrentSessionStarts,
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
		return nil, nil, true, &Error{Code: "auth_unavailable", Message: "no eligible auth available for a new session"}
	}
	return nil, nonK12, false, nil
}

func (s *SessionAffinitySelector) handleK12PaymentRequired(
	ctx context.Context,
	result Result,
	identity sessionIdentity,
	digest string,
	now time.Time,
	releaseSession bool,
	clearTentative bool,
) SelectionResultDirective {
	entry := selectorLogEntry(ctx)
	deactivatedWorkspace := isDeactivatedWorkspaceResultError(result.Error)
	quarantineDuration := s.k12.newSessionQuarantine
	if deactivatedWorkspace {
		quarantineDuration = k12DeactivatedWorkspaceQuarantine
	}
	if err := s.k12.quarantineAfterPaymentRequired(
		result.AuthID,
		digest,
		releaseSession,
		clearTentative,
		deactivatedWorkspace,
		now.Add(quarantineDuration),
		now,
	); err != nil {
		entry.Warnf("k12-session-affinity: persist new-session quarantine failed | source=%s session=%s auth=%s error=%v", identity.Source, shortSessionDigest(digest), result.AuthID, err)
		store := s.k12.store
		source := identity.Source
		authID := result.AuthID
		go retryPersistK12SessionState(store, source, digest, authID)
	}
	entry.Warnf(
		"k12-session-affinity: 402 quarantined auth for new sessions | source=%s session=%s auth=%s deactivated=%t quarantine=%s",
		identity.Source, shortSessionDigest(digest), result.AuthID, deactivatedWorkspace, quarantineDuration,
	)
	return SelectionResultDirective{SuppressAvailabilityUpdate: !deactivatedWorkspace, StopAuthAttempt: true}
}

func retryPersistK12SessionState(store *k12SessionStore, source, sessionDigest, authID string) {
	if store == nil {
		return
	}
	delays := [...]time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}
	var err error
	for _, delay := range delays {
		time.Sleep(delay)
		if err = store.persist(); err == nil {
			return
		}
	}
	selectorLogEntry(context.Background()).Warnf(
		"k12-session-affinity: persist state retry exhausted | source=%s session=%s auth=%s error=%v",
		source, shortSessionDigest(sessionDigest), authID, err,
	)
}

func (s *SessionAffinitySelector) handleK12CredentialHardFailure(
	ctx context.Context,
	result Result,
	identity sessionIdentity,
	digest string,
	status int,
) SelectionResultDirective {
	entry := selectorLogEntry(ctx)
	persistRetryNeeded := false
	if err := s.k12.store.removeAuth(result.AuthID); err != nil {
		entry.Warnf("k12-session-affinity: hard auth binding cleanup failed | source=%s session=%s auth=%s status=%d error=%v", identity.Source, shortSessionDigest(digest), result.AuthID, status, err)
		persistRetryNeeded = true
	}
	now := time.Now()
	quarantineUntil := now.Add(s.k12.newSessionQuarantine)
	if err := s.k12.store.quarantineNewSessions(result.AuthID, "", false, quarantineUntil, now, false); err != nil {
		entry.Warnf("k12-session-affinity: hard auth new-session quarantine failed | source=%s session=%s auth=%s status=%d error=%v", identity.Source, shortSessionDigest(digest), result.AuthID, status, err)
		persistRetryNeeded = true
	}
	if persistRetryNeeded {
		go retryPersistK12SessionState(s.k12.store, identity.Source, digest, result.AuthID)
	}
	s.k12.clearTentativeAuth(result.AuthID)
	s.k12.clearSpilloverPreference(digest)
	entry.Warnf(
		"k12-session-affinity: hard auth failure cleared bindings and quarantined new sessions | source=%s session=%s auth=%s status=%d quarantine=%s",
		identity.Source, shortSessionDigest(digest), result.AuthID, status, s.k12.newSessionQuarantine,
	)
	return SelectionResultDirective{StopAuthAttempt: true}
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
	status := statusCodeFromResult(result.Error)
	streamOpenTimeout := isStreamOpenTimeoutResultError(result.Error)
	attemptKnown, attemptAccepted, attemptIsK12 := s.k12.resolveSelectionAttempt(digest, result.AuthID, opts, result.Success, now)
	binding, confirmed := s.k12.store.binding(digest, now)
	spilloverAuthID := s.k12.spilloverAuth(digest, now)
	tentativeAuthID := s.k12.tentativeAuth(digest, now)
	resultIsBinding := confirmed && binding.AuthID == result.AuthID
	resultIsTentative := tentativeAuthID == result.AuthID
	if !attemptAccepted {
		if attemptIsK12 {
			switch status {
			case http.StatusPaymentRequired:
				return s.handleK12PaymentRequired(ctx, result, identity, digest, now, false, false)
			case http.StatusUnauthorized, http.StatusForbidden:
				return s.handleK12CredentialHardFailure(ctx, result, identity, digest, status)
			}
		}
		if streamOpenTimeout {
			// A sibling request may still own another active attempt for this
			// credential. The failed attempt cannot retire that cohort, but its
			// outer retry must still prefer a paid spillover over another K12.
			s.k12.markSpilloverPreferred(digest, now.Add(s.k12.cooldown))
		}
		return SelectionResultDirective{SuppressAvailabilityUpdate: true, StopAuthAttempt: !result.Success}
	}
	if spilloverAuthID == result.AuthID {
		entry := selectorLogEntry(ctx)
		if result.Success {
			committed, err := s.k12.commitSpilloverSuccess(digest, result.AuthID, now)
			if err != nil {
				entry.Errorf("k12-session-affinity: spillover success state update failed | source=%s session=%s auth=%s error=%v", identity.Source, shortSessionDigest(digest), result.AuthID, err)
				return SelectionResultDirective{}
			}
			if !committed {
				entry.Debugf("k12-session-affinity: stale spillover success ignored | source=%s session=%s auth=%s", identity.Source, shortSessionDigest(digest), result.AuthID)
				return SelectionResultDirective{}
			}
			entry.Infof("k12-session-affinity: spillover binding confirmed | source=%s session=%s auth=%s", identity.Source, shortSessionDigest(digest), result.AuthID)
			return SelectionResultDirective{}
		}
		status := statusCodeFromResult(result.Error)
		if status == http.StatusUnauthorized || status == http.StatusPaymentRequired || status == http.StatusForbidden {
			s.k12.clearSpilloverAuth(result.AuthID)
		} else {
			s.k12.clearSpillover(digest, result.AuthID)
		}
		if binding, ok := s.k12.store.binding(digest, now); ok {
			if remaining := s.k12.store.cooldownRemaining(digest, binding.AuthID, now); remaining > 0 {
				s.k12.markSpilloverPreferred(digest, now.Add(remaining))
			}
		}
		entry.Warnf("k12-session-affinity: spillover binding released after failure | source=%s session=%s auth=%s status=%d", identity.Source, shortSessionDigest(digest), result.AuthID, status)
		transient := status == 0 || status == http.StatusRequestTimeout || status >= 500
		return SelectionResultDirective{
			SuppressAvailabilityUpdate: transient,
			StopAuthAttempt:            transient,
		}
	}
	if !resultIsBinding && !resultIsTentative {
		if attemptKnown && attemptIsK12 {
			switch status {
			case http.StatusPaymentRequired:
				return s.handleK12PaymentRequired(ctx, result, identity, digest, now, false, false)
			case http.StatusUnauthorized, http.StatusForbidden:
				return s.handleK12CredentialHardFailure(ctx, result, identity, digest, status)
			}
		}
		if attemptKnown {
			return SelectionResultDirective{SuppressAvailabilityUpdate: true, StopAuthAttempt: !result.Success}
		}
		return SelectionResultDirective{}
	}

	entry := selectorLogEntry(ctx)
	if result.Success {
		committed, err := s.k12.commitK12Success(digest, identity.Source, result.AuthID, now)
		if err != nil {
			entry.Warnf("k12-session-affinity: persist success failed | source=%s session=%s auth=%s error=%v", identity.Source, shortSessionDigest(digest), result.AuthID, err)
		}
		if !committed {
			entry.Debugf("k12-session-affinity: stale K12 success ignored | source=%s session=%s auth=%s", identity.Source, shortSessionDigest(digest), result.AuthID)
			return SelectionResultDirective{}
		}
		entry.Infof("k12-session-affinity: binding confirmed | source=%s session=%s auth=%s", identity.Source, shortSessionDigest(digest), result.AuthID)
		return SelectionResultDirective{}
	}

	if status == http.StatusPaymentRequired {
		return s.handleK12PaymentRequired(ctx, result, identity, digest, now, resultIsBinding, true)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return s.handleK12CredentialHardFailure(ctx, result, identity, digest, status)
	}

	if isModelSupportResultError(result.Error) {
		s.k12.clearTentative(digest, result.AuthID)
		return SelectionResultDirective{StopAuthAttempt: true, StopCredentialFallback: resultIsBinding}
	}

	transient := status == 0 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
	if !transient {
		s.k12.clearTentative(digest, result.AuthID)
		return SelectionResultDirective{StopCredentialFallback: resultIsBinding && isRequestInvalidResultError(result.Error)}
	}

	allowFailover := status == http.StatusTooManyRequests || streamOpenTimeout
	if allowFailover {
		reason := "429"
		if streamOpenTimeout {
			reason = "stream_open_timeout"
		}
		until := s.k12.recoveryUntil(now, result.RetryAfter)
		if streamOpenTimeout {
			s.k12.store.setCooldownRuntime(digest, result.AuthID, until)
			store := s.k12.store
			source := identity.Source
			authID := result.AuthID
			go func() {
				if err := store.persist(); err != nil {
					selectorLogEntry(context.Background()).Warnf("k12-session-affinity: persist cooldown failed | source=%s session=%s auth=%s error=%v", source, shortSessionDigest(digest), authID, err)
				}
			}()
		} else if err := s.k12.store.setCooldown(digest, result.AuthID, until); err != nil {
			entry.Warnf("k12-session-affinity: persist cooldown failed | source=%s session=%s auth=%s error=%v", identity.Source, shortSessionDigest(digest), result.AuthID, err)
		}
		if streamOpenTimeout {
			s.k12.markSpilloverPreferred(digest, until)
		}
		if resultIsBinding {
			entry.Warnf("k12-session-affinity: confirmed binding temporarily suspended for failover | source=%s session=%s auth=%s reason=%s retry_after=%s", identity.Source, shortSessionDigest(digest), result.AuthID, reason, until.Sub(now).Round(time.Second))
		}
	}
	s.k12.clearTentative(digest, result.AuthID)
	return SelectionResultDirective{
		SuppressAvailabilityUpdate: true,
		StopAuthAttempt:            true,
		StopCredentialFallback:     resultIsBinding && !allowFailover,
	}
}

func isStreamOpenTimeoutResultError(err *Error) bool {
	if err == nil || err.HTTPStatus != http.StatusGatewayTimeout {
		return false
	}
	return strings.Contains(strings.ToLower(err.Message), "stream_open")
}

func isDeactivatedWorkspaceResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Message), "deactivated_workspace")
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
	for key := range s.k12.attempts {
		if _, ok := validAll[key.authID]; !ok {
			delete(s.k12.attempts, key)
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
