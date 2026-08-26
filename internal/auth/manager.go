package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	SessionCookieName  = "transcript_session"
	CeremonyCookieName = "transcript_ceremony"
	maxSessionTTL      = 24 * time.Hour
	ceremonyTTL        = 5 * time.Minute
	maxSessions        = 4096
	maxCeremonies      = 256
)

var (
	ErrCeremony      = errors.New("passkey ceremony is missing or expired")
	ErrNotConfigured = errors.New("no passkeys are configured")
)

type Options struct {
	RelyingPartyID string
	Origins        []string
	SecureCookies  bool
}

func OptionsFromEnvironment(publicBaseURL, relyingPartyID, origins string) (Options, error) {
	parsed, err := url.Parse(publicBaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return Options{}, errors.New("PUBLIC_BASE_URL must be an absolute URL")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Options{}, errors.New("PUBLIC_BASE_URL must contain only an origin")
	}
	if relyingPartyID = strings.TrimSpace(relyingPartyID); relyingPartyID == "" {
		relyingPartyID = parsed.Hostname()
	}
	relyingPartyID = strings.ToLower(strings.TrimSuffix(relyingPartyID, "."))
	publicOrigin := normalizeOrigin(parsed)
	if err := validatePasskeyOrigin(publicOrigin, relyingPartyID); err != nil {
		return Options{}, err
	}
	allowedOrigins, err := splitOrigins(origins)
	if err != nil {
		return Options{}, err
	}
	if len(allowedOrigins) == 0 {
		allowedOrigins = []string{publicOrigin}
	} else if !containsOrigin(allowedOrigins, publicOrigin) {
		return Options{}, errors.New("PASSKEY_ORIGINS must include the PUBLIC_BASE_URL origin")
	}
	for _, origin := range allowedOrigins {
		if err := validatePasskeyOrigin(origin, relyingPartyID); err != nil {
			return Options{}, err
		}
	}
	return Options{RelyingPartyID: relyingPartyID, Origins: allowedOrigins, SecureCookies: strings.EqualFold(parsed.Scheme, "https")}, nil
}

func splitOrigins(value string) ([]string, error) {
	result := make([]string, 0)
	seen := make(map[string]struct{})
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, errors.New("PASSKEY_ORIGINS must contain comma-separated absolute origins")
		}
		origin := normalizeOrigin(parsed)
		if _, exists := seen[origin]; !exists {
			seen[origin] = struct{}{}
			result = append(result, origin)
		}
	}
	return result, nil
}

func normalizeOrigin(parsed *url.URL) string {
	scheme := strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	port := parsed.Port()
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return scheme + "://" + host
}

func containsOrigin(origins []string, target string) bool {
	for _, origin := range origins {
		if strings.EqualFold(origin, target) {
			return true
		}
	}
	return false
}

func validatePasskeyOrigin(origin, relyingPartyID string) error {
	parsed, err := url.Parse(origin)
	if err != nil {
		return errors.New("invalid passkey origin")
	}
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if hostname != relyingPartyID && !strings.HasSuffix(hostname, "."+relyingPartyID) {
		return errors.New("every passkey origin must use the relying party ID or one of its subdomains")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(hostname)) {
		return errors.New("passkey origins require HTTPS except on localhost or a loopback address")
	}
	return nil
}

func isLoopbackHost(hostname string) bool {
	ip := net.ParseIP(hostname)
	return hostname == "localhost" || ip != nil && ip.IsLoopback()
}

type session struct {
	CredentialID  []byte
	CreatedAt     time.Time
	ExpiresAt     time.Time
	PasswordEpoch uint64
}

type registrationCeremony struct {
	Kind      RegistrationKind
	Name      string
	Session   webauthn.SessionData
	CreatedAt time.Time
	ExpiresAt time.Time
}

type loginCeremony struct {
	Session   webauthn.SessionData
	CreatedAt time.Time
	ExpiresAt time.Time
}

type Manager struct {
	store         *Store
	webauthn      *webauthn.WebAuthn
	secure        bool
	now           func() time.Time
	mu            sync.Mutex
	sessions      map[[32]byte]session
	registrations map[[32]byte]registrationCeremony
	logins        map[[32]byte]loginCeremony
	passwordEpoch uint64
}

func NewManager(store *Store, options Options) (*Manager, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPDisplayName: "Discord Transcript API",
		RPID:          options.RelyingPartyID,
		RPOrigins:     options.Origins,
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: ceremonyTTL},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Manager{
		store:         store,
		webauthn:      wa,
		secure:        options.SecureCookies,
		now:           time.Now,
		sessions:      make(map[[32]byte]session),
		registrations: make(map[[32]byte]registrationCeremony),
		logins:        make(map[[32]byte]loginCeremony),
	}, nil
}

func (m *Manager) SetupRequired() bool { return m.store.SetupRequired() }

func (m *Manager) Passkeys() []Passkey { return m.store.Passkeys() }

func (m *Manager) Authenticated(req *http.Request) (bool, time.Time) {
	token := cookieValue(req, SessionCookieName)
	if token == "" {
		return false, time.Time{}
	}
	hash := tokenHash(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.sweepLocked(now)
	stored, ok := m.sessions[hash]
	if !ok || !stored.ExpiresAt.After(now) {
		delete(m.sessions, hash)
		return false, time.Time{}
	}
	if len(stored.CredentialID) == 0 {
		if m.passwordEpoch != stored.PasswordEpoch || !m.store.HasPassword() {
			delete(m.sessions, hash)
			return false, time.Time{}
		}
		return true, stored.ExpiresAt
	}
	if !m.store.HasCredential(stored.CredentialID) {
		delete(m.sessions, hash)
		return false, time.Time{}
	}
	return true, stored.ExpiresAt
}

func (m *Manager) BeginLogin() (*protocol.CredentialAssertion, *http.Cookie, error) {
	if len(m.store.Passkeys()) == 0 {
		return nil, nil, ErrNotConfigured
	}
	options, sessionData, err := m.webauthn.BeginDiscoverableMediatedLogin(
		protocol.MediationDefault,
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return nil, nil, err
	}
	token, err := newToken()
	if err != nil {
		return nil, nil, err
	}
	now := m.now()
	m.mu.Lock()
	m.sweepLocked(now)
	m.evictOldestLoginLocked()
	m.logins[tokenHash(token)] = loginCeremony{Session: *sessionData, CreatedAt: now, ExpiresAt: now.Add(ceremonyTTL)}
	m.mu.Unlock()
	return options, m.ceremonyCookie(token, now.Add(ceremonyTTL)), nil
}

func (m *Manager) FinishLogin(req *http.Request) (*http.Cookie, time.Time, error) {
	ceremony, err := m.takeLogin(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	_, credential, err := m.webauthn.FinishPasskeyLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
		user, lookupErr := m.store.UserByCredential(rawID, userHandle)
		if lookupErr != nil {
			return nil, lookupErr
		}
		return user, nil
	}, ceremony.Session, req)
	if err != nil {
		return nil, time.Time{}, err
	}
	if err := m.store.UpdateCredential(*credential); err != nil {
		return nil, time.Time{}, err
	}
	return m.createSession(credential.ID)
}

func (m *Manager) BeginSetup(code, name string) (*protocol.CredentialCreation, *http.Cookie, error) {
	if !m.store.VerifySetupCode(code) {
		return nil, nil, ErrInvalidSetupCode
	}
	return m.beginRegistration(name, RegistrationSetup)
}

func (m *Manager) FinishSetup(req *http.Request) (*http.Cookie, time.Time, error) {
	credential, err := m.finishRegistration(req, RegistrationSetup)
	if err != nil {
		return nil, time.Time{}, err
	}
	return m.createSession(credential.ID)
}

func (m *Manager) BeginRegistration(name string) (*protocol.CredentialCreation, *http.Cookie, error) {
	return m.beginRegistration(name, RegistrationAdditional)
}

func (m *Manager) FinishRegistration(req *http.Request) error {
	_, err := m.finishRegistration(req, RegistrationAdditional)
	return err
}

func (m *Manager) beginRegistration(name string, kind RegistrationKind) (*protocol.CredentialCreation, *http.Cookie, error) {
	name, err := validateName(name)
	if err != nil {
		return nil, nil, err
	}
	if kind == RegistrationSetup && !m.store.SetupRequired() {
		return nil, nil, ErrSetupComplete
	}
	if kind == RegistrationAdditional {
		if m.store.SetupRequired() {
			return nil, nil, ErrSetupComplete
		}
	}
	user := m.store.User()
	options, sessionData, err := m.webauthn.BeginMediatedRegistration(
		user,
		protocol.MediationDefault,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			RequireResidentKey: protocol.ResidentKeyRequired(),
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			UserVerification:   protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(webauthn.Credentials(user.WebAuthnCredentials()).CredentialDescriptors()),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		return nil, nil, err
	}
	token, err := newToken()
	if err != nil {
		return nil, nil, err
	}
	now := m.now()
	m.mu.Lock()
	m.sweepLocked(now)
	m.evictOldestRegistrationLocked()
	m.registrations[tokenHash(token)] = registrationCeremony{Kind: kind, Name: name, Session: *sessionData, CreatedAt: now, ExpiresAt: now.Add(ceremonyTTL)}
	m.mu.Unlock()
	return options, m.ceremonyCookie(token, now.Add(ceremonyTTL)), nil
}

func (m *Manager) finishRegistration(req *http.Request, expected RegistrationKind) (*webauthn.Credential, error) {
	ceremony, err := m.takeRegistration(req)
	if err != nil {
		return nil, err
	}
	if ceremony.Kind != expected {
		return nil, ErrCeremony
	}
	credential, err := m.webauthn.FinishRegistration(m.store.User(), ceremony.Session, req)
	if err != nil {
		return nil, err
	}
	if _, err := m.store.AddCredential(ceremony.Name, *credential, ceremony.Kind); err != nil {
		return nil, err
	}
	return credential, nil
}

func (m *Manager) DeletePasskey(id string) error {
	credentialID, err := m.store.DeletePasskey(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	for hash, stored := range m.sessions {
		if bytes.Equal(stored.CredentialID, credentialID) {
			delete(m.sessions, hash)
		}
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) Logout(req *http.Request) *http.Cookie {
	if token := cookieValue(req, SessionCookieName); token != "" {
		m.mu.Lock()
		delete(m.sessions, tokenHash(token))
		m.mu.Unlock()
	}
	return m.sessionCookie("", time.Unix(1, 0))
}

func (m *Manager) createSession(credentialID []byte) (*http.Cookie, time.Time, error) {
	token, err := newToken()
	if err != nil {
		return nil, time.Time{}, err
	}
	now := m.now()
	expires := now.Add(maxSessionTTL)
	m.mu.Lock()
	m.sweepLocked(now)
	m.evictOldestSessionLocked()
	m.sessions[tokenHash(token)] = session{CredentialID: append([]byte(nil), credentialID...), CreatedAt: now, ExpiresAt: expires, PasswordEpoch: m.passwordEpoch}
	m.mu.Unlock()
	return m.sessionCookie(token, expires), expires, nil
}

// LoginPassword verifies a dashboard password and issues the same 24-hour
// browser session a passkey sign-in produces.
func (m *Manager) LoginPassword(password string) (*http.Cookie, time.Time, error) {
	if err := m.store.VerifyPassword(password); err != nil {
		return nil, time.Time{}, err
	}
	return m.createSession(nil)
}

// CompletePasswordSetup consumes the one-time setup code to enroll the first
// dashboard password.
func (m *Manager) CompletePasswordSetup(code, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return m.store.CompletePasswordSetup(code, hash)
}

// SetPassword enrolls or replaces the dashboard password. Replacing requires
// the current password and invalidates existing password sessions.
func (m *Manager) SetPassword(current, next string) error {
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if m.store.HasPassword() && m.store.VerifyPassword(current) != nil {
		return ErrWrongPassword
	}
	if err := m.store.SetPassword(hash); err != nil {
		return err
	}
	m.bumpPasswordEpoch()
	return nil
}

// ClearPassword removes the dashboard password after verifying it. The store
// refuses when passkeys would not remain as another sign-in method.
func (m *Manager) ClearPassword(current string) error {
	if err := m.store.VerifyPassword(current); err != nil {
		return err
	}
	if err := m.store.DeletePassword(); err != nil {
		return err
	}
	m.bumpPasswordEpoch()
	return nil
}

func (m *Manager) bumpPasswordEpoch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.passwordEpoch++
	for hash, stored := range m.sessions {
		if len(stored.CredentialID) == 0 {
			delete(m.sessions, hash)
		}
	}
}

func (m *Manager) HasPassword() bool { return m.store.HasPassword() }

func (m *Manager) takeLogin(req *http.Request) (loginCeremony, error) {
	token := cookieValue(req, CeremonyCookieName)
	if token == "" {
		return loginCeremony{}, ErrCeremony
	}
	hash := tokenHash(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.sweepLocked(now)
	ceremony, ok := m.logins[hash]
	delete(m.logins, hash)
	if !ok || !ceremony.ExpiresAt.After(now) {
		return loginCeremony{}, ErrCeremony
	}
	return ceremony, nil
}

func (m *Manager) takeRegistration(req *http.Request) (registrationCeremony, error) {
	token := cookieValue(req, CeremonyCookieName)
	if token == "" {
		return registrationCeremony{}, ErrCeremony
	}
	hash := tokenHash(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.sweepLocked(now)
	ceremony, ok := m.registrations[hash]
	delete(m.registrations, hash)
	if !ok || !ceremony.ExpiresAt.After(now) {
		return registrationCeremony{}, ErrCeremony
	}
	return ceremony, nil
}

func (m *Manager) sweepLocked(now time.Time) {
	for hash, stored := range m.sessions {
		if !stored.ExpiresAt.After(now) {
			delete(m.sessions, hash)
		}
	}
	for hash, stored := range m.registrations {
		if !stored.ExpiresAt.After(now) {
			delete(m.registrations, hash)
		}
	}
	for hash, stored := range m.logins {
		if !stored.ExpiresAt.After(now) {
			delete(m.logins, hash)
		}
	}
}

func (m *Manager) evictOldestSessionLocked() {
	if len(m.sessions) < maxSessions {
		return
	}
	var oldestHash [32]byte
	var oldest session
	found := false
	for hash, candidate := range m.sessions {
		if !found || candidate.CreatedAt.Before(oldest.CreatedAt) {
			oldestHash, oldest, found = hash, candidate, true
		}
	}
	if found {
		delete(m.sessions, oldestHash)
	}
}

func (m *Manager) evictOldestRegistrationLocked() {
	if len(m.registrations) < maxCeremonies {
		return
	}
	var oldestHash [32]byte
	var oldest registrationCeremony
	found := false
	for hash, candidate := range m.registrations {
		if !found || candidate.CreatedAt.Before(oldest.CreatedAt) {
			oldestHash, oldest, found = hash, candidate, true
		}
	}
	if found {
		delete(m.registrations, oldestHash)
	}
}

func (m *Manager) evictOldestLoginLocked() {
	if len(m.logins) < maxCeremonies {
		return
	}
	var oldestHash [32]byte
	var oldest loginCeremony
	found := false
	for hash, candidate := range m.logins {
		if !found || candidate.CreatedAt.Before(oldest.CreatedAt) {
			oldestHash, oldest, found = hash, candidate, true
		}
	}
	if found {
		delete(m.logins, oldestHash)
	}
}

func (m *Manager) sessionCookie(value string, expires time.Time) *http.Cookie {
	maxAge := 0
	if value != "" {
		maxAge = max(0, int(expires.Sub(m.now()).Seconds()))
	}
	return &http.Cookie{Name: SessionCookieName, Value: value, Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteStrictMode, MaxAge: maxAge, Expires: expires}
}

func (m *Manager) ceremonyCookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{Name: CeremonyCookieName, Value: value, Path: "/", HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteStrictMode, MaxAge: max(0, int(expires.Sub(m.now()).Seconds())), Expires: expires}
}

func cookieValue(req *http.Request, name string) string {
	cookie, err := req.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func tokenHash(value string) [32]byte { return sha256.Sum256([]byte(value)) }

func newToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
