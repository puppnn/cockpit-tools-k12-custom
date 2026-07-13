package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	k12SessionStateVersion = 1
	k12SessionKeyContext   = "cockpit-tools:k12-session-affinity:v1"
	k12SessionKeyIDContext = "cockpit-tools:k12-session-affinity:key-id:v1"
)

type k12SessionBinding struct {
	SessionDigest string `json:"sessionDigest"`
	Source        string `json:"source"`
	AuthID        string `json:"authId"`
	LastSuccessAt int64  `json:"lastSuccessAt"`
	ExpiresAt     int64  `json:"expiresAt"`
	CooldownUntil int64  `json:"cooldownUntil,omitempty"`
}

type k12SessionCooldown struct {
	SessionDigest string `json:"sessionDigest"`
	AuthID        string `json:"authId"`
	CooldownUntil int64  `json:"cooldownUntil"`
}

type k12SessionStateFile struct {
	Version   int                  `json:"version"`
	KeyID     string               `json:"keyId"`
	Bindings  []k12SessionBinding  `json:"bindings"`
	Cooldowns []k12SessionCooldown `json:"cooldowns,omitempty"`
}

type k12SessionStore struct {
	mu        sync.Mutex
	path      string
	key       []byte
	keyID     string
	ttl       time.Duration
	bindings  map[string]k12SessionBinding
	cooldowns map[string]k12SessionCooldown
}

func newK12SessionStore(path string, keyMaterial []byte, ttl time.Duration) (*k12SessionStore, error) {
	if len(keyMaterial) == 0 {
		return nil, fmt.Errorf("K12 session HMAC key material is empty")
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	keyMAC := hmac.New(sha256.New, keyMaterial)
	_, _ = keyMAC.Write([]byte(k12SessionKeyContext))
	key := keyMAC.Sum(nil)
	keyIDMAC := hmac.New(sha256.New, key)
	_, _ = keyIDMAC.Write([]byte(k12SessionKeyIDContext))

	store := &k12SessionStore{
		path:      strings.TrimSpace(path),
		key:       key,
		keyID:     hex.EncodeToString(keyIDMAC.Sum(nil)[:12]),
		ttl:       ttl,
		bindings:  make(map[string]k12SessionBinding),
		cooldowns: make(map[string]k12SessionCooldown),
	}
	if err := store.load(); err != nil {
		return store, err
	}
	return store, nil
}

func (s *k12SessionStore) digest(sessionID string) string {
	if s == nil || len(s.key) == 0 || sessionID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *k12SessionStore) load() error {
	if s == nil || s.path == "" {
		return nil
	}
	content, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read K12 session state: %w", err)
	}
	var state k12SessionStateFile
	if err := json.Unmarshal(content, &state); err != nil {
		return fmt.Errorf("parse K12 session state: %w", err)
	}
	if state.Version != k12SessionStateVersion || !hmac.Equal([]byte(state.KeyID), []byte(s.keyID)) {
		return nil
	}

	now := time.Now().Unix()
	for _, binding := range state.Bindings {
		binding.SessionDigest = strings.TrimSpace(binding.SessionDigest)
		binding.AuthID = strings.TrimSpace(binding.AuthID)
		if binding.SessionDigest == "" || binding.AuthID == "" || binding.ExpiresAt <= now {
			continue
		}
		s.bindings[binding.SessionDigest] = binding
	}
	for _, cooldown := range state.Cooldowns {
		cooldown.SessionDigest = strings.TrimSpace(cooldown.SessionDigest)
		cooldown.AuthID = strings.TrimSpace(cooldown.AuthID)
		if cooldown.SessionDigest == "" || cooldown.AuthID == "" || cooldown.CooldownUntil <= now {
			continue
		}
		s.cooldowns[k12CooldownKey(cooldown.SessionDigest, cooldown.AuthID)] = cooldown
	}
	return nil
}

func (s *k12SessionStore) binding(sessionDigest string, now time.Time) (k12SessionBinding, bool) {
	if s == nil || sessionDigest == "" {
		return k12SessionBinding{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.cleanupLocked(now)
	binding, ok := s.bindings[sessionDigest]
	if changed {
		_ = s.persistLocked()
	}
	return binding, ok
}

func (s *k12SessionStore) confirm(sessionDigest, source, authID string, now time.Time) error {
	if s == nil || sessionDigest == "" || authID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	s.bindings[sessionDigest] = k12SessionBinding{
		SessionDigest: sessionDigest,
		Source:        strings.TrimSpace(source),
		AuthID:        strings.TrimSpace(authID),
		LastSuccessAt: now.Unix(),
		ExpiresAt:     now.Add(s.ttl).Unix(),
	}
	delete(s.cooldowns, k12CooldownKey(sessionDigest, authID))
	return s.persistLocked()
}

func (s *k12SessionStore) setCooldown(sessionDigest, authID string, until time.Time) error {
	if s == nil || sessionDigest == "" || authID == "" || until.IsZero() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.cleanupLocked(now)
	if binding, ok := s.bindings[sessionDigest]; ok && binding.AuthID == authID {
		binding.CooldownUntil = until.Unix()
		s.bindings[sessionDigest] = binding
	} else {
		cooldown := k12SessionCooldown{
			SessionDigest: sessionDigest,
			AuthID:        authID,
			CooldownUntil: until.Unix(),
		}
		s.cooldowns[k12CooldownKey(sessionDigest, authID)] = cooldown
	}
	return s.persistLocked()
}

func (s *k12SessionStore) releaseSessionToCooldown(sessionDigest, authID string, until time.Time) error {
	if s == nil || sessionDigest == "" || authID == "" || until.IsZero() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(time.Now())
	if binding, ok := s.bindings[sessionDigest]; ok && binding.AuthID == authID {
		delete(s.bindings, sessionDigest)
	}
	key := k12CooldownKey(sessionDigest, authID)
	cooldownUntil := until.Unix()
	if existing, ok := s.cooldowns[key]; ok && existing.CooldownUntil > cooldownUntil {
		cooldownUntil = existing.CooldownUntil
	}
	s.cooldowns[key] = k12SessionCooldown{
		SessionDigest: sessionDigest,
		AuthID:        authID,
		CooldownUntil: cooldownUntil,
	}
	return s.persistLocked()
}

func (s *k12SessionStore) cooldownRemaining(sessionDigest, authID string, now time.Time) time.Duration {
	if s == nil || sessionDigest == "" || authID == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.cleanupLocked(now)
	until := int64(0)
	if binding, ok := s.bindings[sessionDigest]; ok && binding.AuthID == authID {
		until = binding.CooldownUntil
	}
	if cooldown, ok := s.cooldowns[k12CooldownKey(sessionDigest, authID)]; ok && cooldown.CooldownUntil > until {
		until = cooldown.CooldownUntil
	}
	if changed {
		_ = s.persistLocked()
	}
	if until <= now.Unix() {
		return 0
	}
	return time.Unix(until, 0).Sub(now)
}

func (s *k12SessionStore) removeSession(sessionDigest string) error {
	if s == nil || sessionDigest == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	removedBinding, hadBinding := s.bindings[sessionDigest]
	if hadBinding {
		delete(s.bindings, sessionDigest)
		changed = true
	}
	removedCooldowns := make(map[string]k12SessionCooldown)
	for key, cooldown := range s.cooldowns {
		if cooldown.SessionDigest == sessionDigest {
			removedCooldowns[key] = cooldown
			delete(s.cooldowns, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := s.persistLocked(); err != nil {
		if hadBinding {
			s.bindings[sessionDigest] = removedBinding
		}
		for key, cooldown := range removedCooldowns {
			s.cooldowns[key] = cooldown
		}
		return err
	}
	return nil
}

func (s *k12SessionStore) removeAuth(authID string) error {
	if s == nil || authID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for digest, binding := range s.bindings {
		if binding.AuthID == authID {
			delete(s.bindings, digest)
			changed = true
		}
	}
	for key, cooldown := range s.cooldowns {
		if cooldown.AuthID == authID {
			delete(s.cooldowns, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.persistLocked()
}

func (s *k12SessionStore) pruneAuths(valid map[string]struct{}, now time.Time) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.cleanupLocked(now)
	for digest, binding := range s.bindings {
		if _, ok := valid[binding.AuthID]; !ok {
			delete(s.bindings, digest)
			changed = true
		}
	}
	for key, cooldown := range s.cooldowns {
		if _, ok := valid[cooldown.AuthID]; !ok {
			delete(s.cooldowns, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.persistLocked()
}

func (s *k12SessionStore) confirmedAndRecentCounts(since, now time.Time) (map[string]int, map[string]int) {
	counts := make(map[string]int)
	recent := make(map[string]int)
	if s == nil {
		return counts, recent
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.cleanupLocked(now)
	sinceUnix := since.Unix()
	for _, binding := range s.bindings {
		counts[binding.AuthID]++
		if binding.LastSuccessAt >= sinceUnix {
			recent[binding.AuthID]++
		}
	}
	if changed {
		_ = s.persistLocked()
	}
	return counts, recent
}

func (s *k12SessionStore) cleanupLocked(now time.Time) bool {
	changed := false
	nowUnix := now.Unix()
	for digest, binding := range s.bindings {
		if binding.ExpiresAt <= nowUnix {
			delete(s.bindings, digest)
			changed = true
			continue
		}
		if binding.CooldownUntil > 0 && binding.CooldownUntil <= nowUnix {
			binding.CooldownUntil = 0
			s.bindings[digest] = binding
			changed = true
		}
	}
	for key, cooldown := range s.cooldowns {
		if cooldown.CooldownUntil <= nowUnix {
			delete(s.cooldowns, key)
			changed = true
		}
	}
	return changed
}

func (s *k12SessionStore) persistLocked() error {
	if s == nil || s.path == "" {
		return nil
	}
	state := k12SessionStateFile{
		Version:   k12SessionStateVersion,
		KeyID:     s.keyID,
		Bindings:  make([]k12SessionBinding, 0, len(s.bindings)),
		Cooldowns: make([]k12SessionCooldown, 0, len(s.cooldowns)),
	}
	for _, binding := range s.bindings {
		state.Bindings = append(state.Bindings, binding)
	}
	for _, cooldown := range s.cooldowns {
		state.Cooldowns = append(state.Cooldowns, cooldown)
	}
	sort.Slice(state.Bindings, func(i, j int) bool {
		return state.Bindings[i].SessionDigest < state.Bindings[j].SessionDigest
	})
	sort.Slice(state.Cooldowns, func(i, j int) bool {
		left := k12CooldownKey(state.Cooldowns[i].SessionDigest, state.Cooldowns[i].AuthID)
		right := k12CooldownKey(state.Cooldowns[j].SessionDigest, state.Cooldowns[j].AuthID)
		return left < right
	})
	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode K12 session state: %w", err)
	}
	content = append(content, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create K12 session state directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".k12-sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("create K12 session state temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("protect K12 session state temp file: %w", err)
	}
	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write K12 session state temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("flush K12 session state temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close K12 session state temp file: %w", err)
	}
	if err := replaceK12SessionStateFile(tempPath, s.path); err != nil {
		return fmt.Errorf("replace K12 session state: %w", err)
	}
	return nil
}

func k12CooldownKey(sessionDigest, authID string) string {
	return sessionDigest + "::" + authID
}
