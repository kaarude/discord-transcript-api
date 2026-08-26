package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/kaarude/discord-transcript-api/internal/securefile"
)

const (
	stateVersion      = 2
	minStateVersion   = 1
	setupCodeFileName = "setup-code"
)

var (
	ErrInvalidSetupCode = errors.New("invalid setup code")
	ErrSetupComplete    = errors.New("first-time setup is already complete")
	ErrLastCredential   = errors.New("the last sign-in method cannot be removed")
	ErrLastPasskey      = ErrLastCredential
	ErrPasskeyNotFound  = errors.New("passkey not found")
	ErrInvalidName      = errors.New("passkey name must be between 1 and 64 characters")
	ErrNoPassword       = errors.New("no dashboard password is configured")
	ErrWrongPassword    = errors.New("incorrect password")
)

type RegistrationKind uint8

const (
	RegistrationSetup RegistrationKind = iota + 1
	RegistrationAdditional
)

type passkeyRecord struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	CreatedAt  string              `json:"createdAt"`
	LastUsedAt *string             `json:"lastUsedAt,omitempty"`
	Credential webauthn.Credential `json:"credential"`
}

type state struct {
	Version           int             `json:"version"`
	RelyingParty      string          `json:"relyingPartyId"`
	UserHandle        []byte          `json:"userHandle"`
	SetupCodeHash     []byte          `json:"setupCodeHash,omitempty"`
	PasswordHash      string          `json:"passwordHash,omitempty"`
	PasswordAlgo      string          `json:"passwordAlgo,omitempty"`
	PasswordCreatedAt *string         `json:"passwordCreatedAt,omitempty"`
	Passkeys          []passkeyRecord `json:"passkeys"`
}

type Passkey struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt"`
}

type User struct {
	handle      []byte
	credentials []webauthn.Credential
}

func (u User) WebAuthnID() []byte                         { return append([]byte(nil), u.handle...) }
func (u User) WebAuthnName() string                       { return "admin" }
func (u User) WebAuthnDisplayName() string                { return "Discord Transcript API administrator" }
func (u User) WebAuthnCredentials() []webauthn.Credential { return cloneCredentials(u.credentials) }

type Store struct {
	mu        sync.RWMutex
	path      string
	setupPath string
	state     state
}

func Open(path, relyingPartyID string) (*Store, string, error) {
	if strings.TrimSpace(relyingPartyID) == "" {
		return nil, "", errors.New("passkey relying party ID is required")
	}
	store := &Store{path: path, setupPath: filepath.Join(filepath.Dir(path), setupCodeFileName)}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		userHandle, randomErr := randomBytes(64)
		if randomErr != nil {
			return nil, "", fmt.Errorf("generate WebAuthn user handle: %w", randomErr)
		}
		store.state = state{
			Version:      stateVersion,
			RelyingParty: relyingPartyID,
			UserHandle:   userHandle,
			Passkeys:     []passkeyRecord{},
		}
		code, err := store.resetSetupCodeLocked()
		return store, code, err
	}
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(raw, &store.state); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validateState(store.state); err != nil {
		return nil, "", fmt.Errorf("validate %s: %w", path, err)
	}
	if store.anyCredentialLocked() && store.state.RelyingParty != relyingPartyID {
		if len(store.state.Passkeys) > 0 {
			return nil, "", fmt.Errorf("PASSKEY_RP_ID changed from %q to %q; existing passkeys only work with the original relying party ID", store.state.RelyingParty, relyingPartyID)
		}
		store.state.RelyingParty = relyingPartyID
	}
	if len(store.state.Passkeys) == 0 && store.state.RelyingParty != relyingPartyID {
		store.state.RelyingParty = relyingPartyID
		if err := store.saveLocked(store.state); err != nil {
			return nil, "", err
		}
	}
	if store.anyCredentialLocked() {
		if len(store.state.SetupCodeHash) > 0 {
			next := cloneState(store.state)
			next.SetupCodeHash = nil
			if err := store.saveLocked(next); err != nil {
				return nil, "", err
			}
			store.state = next
		}
		_ = os.Remove(store.setupPath)
		return store, "", nil
	}
	if code, readErr := os.ReadFile(store.setupPath); readErr == nil && store.verifySetupCodeLocked(strings.TrimSpace(string(code))) {
		return store, strings.TrimSpace(string(code)), nil
	}
	code, err := store.resetSetupCodeLocked()
	return store, code, err
}

func validateState(value state) error {
	if value.Version < minStateVersion || value.Version > stateVersion {
		return fmt.Errorf("unsupported auth state version %d", value.Version)
	}
	if value.RelyingParty == "" {
		return errors.New("missing relying party ID")
	}
	if len(value.UserHandle) == 0 || len(value.UserHandle) > 64 {
		return errors.New("invalid WebAuthn user handle")
	}
	if (value.PasswordHash == "") != (value.PasswordAlgo == "") {
		return errors.New("password hash and algorithm must be configured together")
	}
	if value.PasswordHash != "" && value.PasswordAlgo != passwordAlgorithm {
		return fmt.Errorf("unsupported password algorithm %q", value.PasswordAlgo)
	}
	recordIDs := make(map[string]struct{}, len(value.Passkeys))
	credentialIDs := make(map[string]struct{}, len(value.Passkeys))
	for _, record := range value.Passkeys {
		if record.ID == "" || record.Name == "" || len(record.Credential.ID) == 0 || len(record.Credential.PublicKey) == 0 {
			return errors.New("invalid passkey record")
		}
		if _, exists := recordIDs[record.ID]; exists {
			return errors.New("duplicate passkey record ID")
		}
		recordIDs[record.ID] = struct{}{}
		key := base64.RawURLEncoding.EncodeToString(record.Credential.ID)
		if _, exists := credentialIDs[key]; exists {
			return errors.New("duplicate credential ID")
		}
		credentialIDs[key] = struct{}{}
	}
	return nil
}

func (s *Store) resetSetupCodeLocked() (string, error) {
	value, err := randomBytes(24)
	if err != nil {
		return "", fmt.Errorf("generate setup code: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(value)
	hash := sha256.Sum256([]byte(code))
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return "", err
	}
	if err := securefile.WriteFileAtomic(s.setupPath, append([]byte(code), '\n'), 0o600); err != nil {
		return "", err
	}
	next := cloneState(s.state)
	next.SetupCodeHash = hash[:]
	if err := s.saveLocked(next); err != nil {
		return "", err
	}
	s.state = next
	return code, nil
}

func (s *Store) SetupRequired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.anyCredentialLocked()
}

func (s *Store) HasPassword() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.PasswordHash != ""
}

// SetPassword stores an Argon2id hash. Enrolling a password only adds a
// sign-in capability, so it can never strand the installation.
func (s *Store) SetPassword(hash string) error {
	if hash == "" {
		return errors.New("password hash is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.anyCredentialLocked() {
		return ErrSetupComplete
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	next := cloneState(s.state)
	if next.Version < stateVersion {
		next.Version = stateVersion
	}
	next.PasswordHash = hash
	next.PasswordAlgo = passwordAlgorithm
	nowStr := now
	if next.PasswordCreatedAt == nil {
		next.PasswordCreatedAt = &nowStr
	}
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *Store) VerifyPassword(password string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.PasswordHash == "" || password == "" {
		return ErrNoPassword
	}
	if !verifyPasswordHash(s.state.PasswordHash, password) {
		return ErrWrongPassword
	}
	return nil
}

// DeletePassword removes the password unless it is the only remaining
// sign-in method.
func (s *Store) DeletePassword() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.PasswordHash == "" {
		return ErrNoPassword
	}
	if len(s.state.Passkeys) == 0 {
		return ErrLastCredential
	}
	next := cloneState(s.state)
	next.PasswordHash, next.PasswordAlgo, next.PasswordCreatedAt = "", "", nil
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *Store) anyCredentialLocked() bool { return s.state.PasswordHash != "" || len(s.state.Passkeys) > 0 }

func (s *Store) VerifySetupCode(code string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.anyCredentialLocked() && s.verifySetupCodeLocked(strings.TrimSpace(code))
}

func (s *Store) verifySetupCodeLocked(code string) bool {
	if code == "" || len(s.state.SetupCodeHash) != sha256.Size {
		return false
	}
	hash := sha256.Sum256([]byte(code))
	return subtle.ConstantTimeCompare(hash[:], s.state.SetupCodeHash) == 1
}

func (s *Store) User() User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	credentials := make([]webauthn.Credential, 0, len(s.state.Passkeys))
	for _, record := range s.state.Passkeys {
		credentials = append(credentials, record.Credential)
	}
	return User{handle: append([]byte(nil), s.state.UserHandle...), credentials: cloneCredentials(credentials)}
}

func (s *Store) UserByCredential(rawID, userHandle []byte) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !bytes.Equal(userHandle, s.state.UserHandle) {
		return User{}, ErrPasskeyNotFound
	}
	credentials := make([]webauthn.Credential, 0, len(s.state.Passkeys))
	found := false
	for _, record := range s.state.Passkeys {
		credentials = append(credentials, record.Credential)
		found = found || bytes.Equal(rawID, record.Credential.ID)
	}
	if !found {
		return User{}, ErrPasskeyNotFound
	}
	return User{handle: append([]byte(nil), s.state.UserHandle...), credentials: cloneCredentials(credentials)}, nil
}

func (s *Store) AddCredential(name string, credential webauthn.Credential, kind RegistrationKind) (Passkey, error) {
	name, err := validateName(name)
	if err != nil {
		return Passkey{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == RegistrationSetup && s.anyCredentialLocked() {
		return Passkey{}, ErrSetupComplete
	}
	if kind == RegistrationAdditional && len(s.state.Passkeys) == 0 && s.state.SetupCodeHash != nil {
		return Passkey{}, ErrSetupComplete
	}
	for _, record := range s.state.Passkeys {
		if bytes.Equal(record.Credential.ID, credential.ID) {
			return Passkey{}, errors.New("passkey is already registered")
		}
	}
	recordID, err := randomHex(16)
	if err != nil {
		return Passkey{}, fmt.Errorf("generate passkey record ID: %w", err)
	}
	record := passkeyRecord{ID: recordID, Name: name, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Credential: credential}
	next := cloneState(s.state)
	next.Passkeys = append(next.Passkeys, record)
	if kind == RegistrationSetup {
		next.SetupCodeHash = nil
	}
	if err := s.saveLocked(next); err != nil {
		return Passkey{}, err
	}
	s.state = next
	if kind == RegistrationSetup {
		_ = os.Remove(s.setupPath)
	}
	return summarize(record), nil
}

func (s *Store) UpdateCredential(credential webauthn.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	for index := range next.Passkeys {
		if bytes.Equal(next.Passkeys[index].Credential.ID, credential.ID) {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			next.Passkeys[index].Credential = credential
			next.Passkeys[index].LastUsedAt = &now
			if err := s.saveLocked(next); err != nil {
				return err
			}
			s.state = next
			return nil
		}
	}
	return ErrPasskeyNotFound
}

func (s *Store) Passkeys() []Passkey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Passkey, 0, len(s.state.Passkeys))
	for _, record := range s.state.Passkeys {
		result = append(result, summarize(record))
	}
	return result
}

func (s *Store) DeletePasskey(id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.Passkeys) <= 1 && s.state.PasswordHash == "" {
		return nil, ErrLastCredential
	}
	next := cloneState(s.state)
	for index, record := range next.Passkeys {
		if record.ID == id {
			credentialID := append([]byte(nil), record.Credential.ID...)
			next.Passkeys = append(next.Passkeys[:index], next.Passkeys[index+1:]...)
			if err := s.saveLocked(next); err != nil {
				return nil, err
			}
			s.state = next
			return credentialID, nil
		}
	}
	return nil, ErrPasskeyNotFound
}

// CompletePasswordSetup claims an unclaimed installation with a dashboard
// password and consumes the one-time setup code.
func (s *Store) CompletePasswordSetup(code, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.anyCredentialLocked() {
		return ErrSetupComplete
	}
	if hash == "" || s.state.SetupCodeHash == nil || len(s.state.SetupCodeHash) != sha256.Size {
		return ErrInvalidSetupCode
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(code)))
	if subtle.ConstantTimeCompare(sum[:], s.state.SetupCodeHash) != 1 {
		return ErrInvalidSetupCode
	}
	next := cloneState(s.state)
	if next.Version < stateVersion {
		next.Version = stateVersion
	}
	next.PasswordHash = hash
	next.PasswordAlgo = passwordAlgorithm
	created := time.Now().UTC().Format(time.RFC3339Nano)
	next.PasswordCreatedAt = &created
	next.SetupCodeHash = nil
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.state = next
	os.Remove(s.setupPath)
	return nil
}

func (s *Store) HasCredential(id []byte) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, record := range s.state.Passkeys {
		if bytes.Equal(record.Credential.ID, id) {
			return true
		}
	}
	return false
}

func (s *Store) saveLocked(value state) error {
	if err := validateState(value); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return securefile.WriteFileAtomic(s.path, append(raw, '\n'), 0o600)
}

func summarize(record passkeyRecord) Passkey {
	return Passkey{ID: record.ID, Name: record.Name, CreatedAt: record.CreatedAt, LastUsedAt: record.LastUsedAt}
}

func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len([]rune(name)) < 1 || len([]rune(name)) > 64 {
		return "", ErrInvalidName
	}
	return name, nil
}

func randomBytes(size int) ([]byte, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return nil, err
	}
	return value, nil
}

func randomHex(size int) (string, error) {
	value, err := randomBytes(size)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func cloneCredentials(credentials []webauthn.Credential) []webauthn.Credential {
	raw, err := json.Marshal(credentials)
	if err != nil {
		return nil
	}
	var cloned []webauthn.Credential
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil
	}
	return cloned
}

func cloneState(value state) state {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var cloned state
	if err := json.Unmarshal(raw, &cloned); err != nil {
		panic(err)
	}
	return cloned
}
