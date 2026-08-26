package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"golang.org/x/crypto/argon2"
)

const (
	testRPID   = "master.test"
	testOrigin = "https://master.test"
)

type virtualPasskey struct {
	credentialID []byte
	userHandle   []byte
	privateKey   *ecdsa.PrivateKey
}

func TestFirstRunRegistrationAndPasskeyLoginRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "auth.json")
	store, setupCode, err := Open(path, testRPID)
	if err != nil {
		t.Fatal(err)
	}
	if setupCode == "" || !store.SetupRequired() || len(store.Passkeys()) != 0 {
		t.Fatalf("unexpected initial state: setup=%q required=%v passkeys=%d", setupCode, store.SetupRequired(), len(store.Passkeys()))
	}
	manager, err := NewManager(store, Options{RelyingPartyID: testRPID, Origins: []string{testOrigin}, SecureCookies: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.BeginSetup("wrong-code", "Laptop"); err != ErrInvalidSetupCode {
		t.Fatalf("invalid setup code returned %v", err)
	}

	creation, ceremonyCookie, err := manager.BeginSetup(setupCode, "Laptop")
	if err != nil {
		t.Fatal(err)
	}
	authenticator := newVirtualPasskey(t, store.User().WebAuthnID())
	registration := authenticator.registrationRequest(t, creation)
	registration.AddCookie(ceremonyCookie)
	sessionCookie, _, err := manager.FinishSetup(registration)
	if err != nil {
		t.Fatalf("finish setup: %v", err)
	}
	if store.SetupRequired() || len(store.Passkeys()) != 1 {
		t.Fatalf("setup did not persist one passkey: required=%v passkeys=%d", store.SetupRequired(), len(store.Passkeys()))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), setupCodeFileName)); !os.IsNotExist(err) {
		t.Fatalf("setup code file remains after enrollment: %v", err)
	}
	authorized := httptest.NewRequest(http.MethodGet, "/admin", nil)
	authorized.AddCookie(sessionCookie)
	if ok, _ := manager.Authenticated(authorized); !ok {
		t.Fatal("first registration did not create an authenticated session")
	}

	assertion, loginCookie, err := manager.BeginLogin()
	if err != nil {
		t.Fatal(err)
	}
	login := authenticator.loginRequest(t, assertion)
	login.AddCookie(loginCookie)
	loginSession, _, err := manager.FinishLogin(login)
	if err != nil {
		t.Fatalf("finish login: %v", err)
	}
	loggedIn := httptest.NewRequest(http.MethodGet, "/health", nil)
	loggedIn.AddCookie(loginSession)
	if ok, _ := manager.Authenticated(loggedIn); !ok {
		t.Fatal("valid passkey login did not create a session")
	}
	passkeys := store.Passkeys()
	if len(passkeys) != 1 || passkeys[0].LastUsedAt == nil {
		t.Fatalf("successful login metadata was not persisted: %#v", passkeys)
	}

	reloaded, nextSetupCode, err := Open(path, testRPID)
	if err != nil || nextSetupCode != "" || reloaded.SetupRequired() || len(reloaded.Passkeys()) != 1 {
		t.Fatalf("persisted auth state did not reload: code=%q required=%v passkeys=%d err=%v", nextSetupCode, reloaded.SetupRequired(), len(reloaded.Passkeys()), err)
	}
}

func TestStoreKeepsAtLeastOneSignInMethod(t *testing.T) {
	store, _, err := Open(filepath.Join(t.TempDir(), "auth.json"), testRPID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AddCredential("First", dummyCredential(1), RegistrationSetup)
	if err != nil {
		t.Fatal(err)
	}
	for index := 2; index <= 5; index++ {
		if _, err := store.AddCredential(fmt.Sprintf("Backup %d", index), dummyCredential(byte(index)), RegistrationAdditional); err != nil {
			t.Fatalf("passkey %d was rejected: %v", index, err)
		}
	}
	if len(store.Passkeys()) != 5 {
		t.Fatalf("expected five stored passkeys, got %d", len(store.Passkeys()))
	}
	if _, err := store.DeletePasskey(first.ID); err != nil {
		t.Fatal(err)
	}
	remaining := store.Passkeys()
	if len(remaining) != 4 {
		t.Fatalf("expected four passkeys, got %d", len(remaining))
	}
	for _, passkey := range remaining[1:] {
		if _, err := store.DeletePasskey(passkey.ID); err != nil {
			t.Fatal(err)
		}
	}
	last := store.Passkeys()
	if len(last) != 1 {
		t.Fatalf("expected one passkey, got %d", len(last))
	}
	if _, err := store.DeletePasskey(last[0].ID); err != ErrLastCredential {
		t.Fatalf("last credential deletion returned %v", err)
	}

	if err := store.SetPassword("$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA"); err != nil {
		t.Fatal(err)
	}
	if !store.HasPassword() {
		t.Fatal("password enrollment did not persist")
	}
	if err := store.VerifyPassword("whatever"); err == nil {
		t.Fatal("arbitrary password verified against an unknown hash")
	}
	if _, err := store.DeletePasskey(last[0].ID); err != nil {
		t.Fatalf("passkey removal alongside a password failed: %v", err)
	}
	if len(store.Passkeys()) != 0 || !store.HasPassword() {
		t.Fatalf("password-only state is wrong: passkeys=%d hasPassword=%v", len(store.Passkeys()), store.HasPassword())
	}
	if err := store.DeletePassword(); err != ErrLastCredential {
		t.Fatalf("password-only removal returned %v", err)
	}
}

func TestRelyingPartyCannotChangeAfterEnrollment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store, _, err := Open(path, testRPID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddCredential("Primary", dummyCredential(1), RegistrationSetup); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(path, "other.test"); err == nil || !strings.Contains(err.Error(), "existing passkeys") {
		t.Fatalf("relying party change was accepted: %v", err)
	}
}

func TestPasswordHashRoundTripAndRejection(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$"+passwordAlgorithm+"$v=") || !strings.Contains(hash, "$m="+fmt.Sprint(passwordMemoryKiB)) {
		t.Fatalf("unexpected PHC format: %q", hash)
	}
	if verifyPasswordHash(hash, "correct horse battery staple") != true {
		t.Fatal("the correct password failed verification")
	}
	if verifyPasswordHash(hash, "wrong password entirely") {
		t.Fatal("a wrong password verified")
	}
	if verifyPasswordHash(hash+"x", "correct horse battery staple") {
		t.Fatal("a tampered hash verified")
	}
	if verifyPasswordHash("$argon2id$v=999$m=1,t=1,p=1$abc$def", "anything") {
		t.Fatal("an unknown version verified")
	}
	if _, err := HashPassword("short"); err != ErrWeakPassword {
		t.Fatalf("short password returned %v", err)
	}
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("candidate"), salt, 1, 8, 1, 16)
	stored := fmt.Sprintf("$%s$v=%d$m=8,t=1,p=1$%s$%s", passwordAlgorithm, argon2.Version, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
	if !verifyPasswordHash(stored, "candidate") {
		t.Fatal("a hand-built valid hash failed to verify")
	}
}

func TestCompletePasswordSetupClaimsAndLogsIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store, setupCode, err := Open(path, testRPID)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store, Options{RelyingPartyID: testRPID, Origins: []string{testOrigin}, SecureCookies: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CompletePasswordSetup("wrong", "long enough password"); err == nil {
		t.Fatal("setup accepted an invalid code")
	}
	if err := manager.CompletePasswordSetup(setupCode, "short"); err != ErrWeakPassword {
		t.Fatalf("weak password returned %v", err)
	}
	if err := manager.CompletePasswordSetup(setupCode, "long enough password"); err != nil {
		t.Fatalf("valid setup rejected: %v", err)
	}
	if store.SetupRequired() {
		t.Fatal("setup did not claim the installation")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(path), setupCodeFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("setup code file remains: %v", statErr)
	}
	if _, _, err := manager.BeginLogin(); err != ErrNotConfigured {
		t.Fatalf("passkey login offered without passkeys: %v", err)
	}
	session, expires, err := manager.LoginPassword("long enough password")
	if err != nil || session == nil || !expires.After(time.Now()) {
		t.Fatalf("password login failed: %v %v", err, expires)
	}
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	request.AddCookie(session)
	if ok, _ := manager.Authenticated(request); !ok {
		t.Fatal("password login did not create a session")
	}
	if _, _, err := manager.LoginPassword("not the password"); err == nil {
		t.Fatal("a wrong password signed in")
	}
	// Rotation invalidates old password sessions.
	if err := manager.SetPassword("long enough password", "brand new password!"); err != nil {
		t.Fatalf("rotation rejected: %v", err)
	}
	if ok, _ := manager.Authenticated(request); ok {
		t.Fatal("old password session survived rotation")
	}
	newSession, _, err := manager.LoginPassword("brand new password!")
	if err != nil {
		t.Fatalf("new password did not sign in: %v", err)
	}
	request = httptest.NewRequest(http.MethodGet, "/admin", nil)
	request.AddCookie(newSession)
	if ok, _ := manager.Authenticated(request); !ok {
		t.Fatal("rotated password did not create a session")
	}
	if err := manager.SetPassword("wrong current", "another long password"); err == nil {
		t.Fatal("rotation accepted a wrong current password")
	}
	// Removing the last sign-in method must be refused.
	if err := manager.ClearPassword("wrong current"); err == nil {
		t.Fatal("removal accepted a wrong password")
	}
	if err := manager.ClearPassword("brand new password!"); err != ErrLastCredential {
		t.Fatalf("last-credential removal returned %v", err)
	}
	if _, err := store.AddCredential("Second method", dummyCredential(2), RegistrationAdditional); err != nil {
		t.Fatalf("additional passkey registration after password onboarding: %v", err)
	}
	if err := manager.ClearPassword("brand new password!"); err != nil {
		t.Fatalf("password removal with a passkey present failed: %v", err)
	}
	if manager.HasPassword() {
		t.Fatal("password removal did not persist")
	}
}

func TestAuthInitializationLeavesExistingApplicationDataUntouched(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	transcriptDir := filepath.Join(root, "transcripts", "existing")
	existing := map[string][]byte{
		filepath.Join(dataDir, "settings.json"):         []byte(`{"tokens":[{"id":"api-key","value":"keep-me"}],"unknown":{"keep":true}}`),
		filepath.Join(dataDir, "transcripts.jsonl"):     []byte("{\"uuid\":\"existing\"}\n"),
		filepath.Join(transcriptDir, "transcript.json"): []byte(`{"messages":["keep me"]}`),
	}
	for path, content := range existing {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	authPath := filepath.Join(dataDir, "auth.json")
	stalePaths := []string{authPath + ".tmp", filepath.Join(dataDir, setupCodeFileName) + ".tmp"}
	for _, stale := range stalePaths {
		if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(stale, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := Open(authPath, testRPID); err != nil {
		t.Fatal(err)
	}
	for _, stale := range stalePaths {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("legacy auth temporary file survived initialization: %s: %v", stale, err)
		}
	}
	for path, expected := range existing {
		actual, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != string(expected) {
			t.Fatalf("authentication initialization changed %s: got %q want %q", path, actual, expected)
		}
	}
	for _, path := range []string{authPath, filepath.Join(dataDir, setupCodeFileName)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s has permissions %o, want 600", path, info.Mode().Perm())
		}
	}
}

func TestPasskeyEnvironmentOptionsRejectUnsafeOrMismatchedOrigins(t *testing.T) {
	valid, err := OptionsFromEnvironment("https://admin.example.com", "example.com", "https://admin.example.com,https://health.example.com")
	if err != nil || valid.RelyingPartyID != "example.com" || len(valid.Origins) != 2 || !valid.SecureCookies {
		t.Fatalf("valid passkey options failed: %#v %v", valid, err)
	}
	for _, test := range []struct {
		publicBase string
		rpID       string
		origins    string
	}{
		{"https://admin.example.com/path", "", ""},
		{"https://admin.example.com", "example.com", "https://attacker.test"},
		{"http://admin.example.com", "example.com", ""},
		{"https://admin.example.com", "example.com", "://bad"},
		{"ftp://admin.example.com", "example.com", "https://admin.example.com"},
		{"https://user@admin.example.com", "example.com", ""},
		{"https://admin.example.com", "example.com", "https://health.example.com"},
	} {
		if _, err := OptionsFromEnvironment(test.publicBase, test.rpID, test.origins); err == nil {
			t.Fatalf("unsafe options were accepted: %#v", test)
		}
	}
	loopback, err := OptionsFromEnvironment("http://127.0.0.1:3010", "", "")
	if err != nil || loopback.SecureCookies || loopback.RelyingPartyID != "127.0.0.1" {
		t.Fatalf("loopback development origin failed: %#v %v", loopback, err)
	}
	normalized, err := OptionsFromEnvironment("HTTPS://ADMIN.EXAMPLE.COM:443", "example.com", "https://admin.example.com")
	if err != nil || len(normalized.Origins) != 1 || normalized.Origins[0] != "https://admin.example.com" {
		t.Fatalf("equivalent origin was not normalized: %#v %v", normalized, err)
	}
}

func newVirtualPasskey(t *testing.T, userHandle []byte) virtualPasskey {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return virtualPasskey{credentialID: []byte("transcript-test-credential"), userHandle: append([]byte(nil), userHandle...), privateKey: privateKey}
}

func (v virtualPasskey) registrationRequest(t *testing.T, creation *protocol.CredentialCreation) *http.Request {
	t.Helper()
	x := v.privateKey.PublicKey.X.FillBytes(make([]byte, 32))
	y := v.privateKey.PublicKey.Y.FillBytes(make([]byte, 32))
	publicKey, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(testRPID))
	authenticatorData := append([]byte(nil), rpHash[:]...)
	authenticatorData = append(authenticatorData, byte(0x45))
	authenticatorData = binary.BigEndian.AppendUint32(authenticatorData, 0)
	authenticatorData = append(authenticatorData, make([]byte, 16)...)
	authenticatorData = binary.BigEndian.AppendUint16(authenticatorData, uint16(len(v.credentialID)))
	authenticatorData = append(authenticatorData, v.credentialID...)
	authenticatorData = append(authenticatorData, publicKey...)
	attestationObject, err := cbor.Marshal(map[string]any{"authData": authenticatorData, "fmt": "none", "attStmt": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	clientData := clientDataJSON(t, "webauthn.create", creation.Response.Challenge.String())
	body := map[string]any{
		"id":                      encode(v.credentialID),
		"rawId":                   encode(v.credentialID),
		"type":                    "public-key",
		"authenticatorAttachment": "platform",
		"clientExtensionResults":  map[string]any{},
		"response": map[string]any{
			"attestationObject": encode(attestationObject),
			"clientDataJSON":    encode(clientData),
			"transports":        []string{"internal"},
		},
	}
	return jsonRequest(t, body)
}

func (v virtualPasskey) loginRequest(t *testing.T, assertion *protocol.CredentialAssertion) *http.Request {
	t.Helper()
	rpHash := sha256.Sum256([]byte(testRPID))
	authenticatorData := append([]byte(nil), rpHash[:]...)
	authenticatorData = append(authenticatorData, byte(0x05))
	authenticatorData = binary.BigEndian.AppendUint32(authenticatorData, 1)
	clientData := clientDataJSON(t, "webauthn.get", assertion.Response.Challenge.String())
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte(nil), authenticatorData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, v.privateKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"id":                      encode(v.credentialID),
		"rawId":                   encode(v.credentialID),
		"type":                    "public-key",
		"authenticatorAttachment": "platform",
		"clientExtensionResults":  map[string]any{},
		"response": map[string]any{
			"authenticatorData": encode(authenticatorData),
			"clientDataJSON":    encode(clientData),
			"signature":         encode(signature),
			"userHandle":        encode(v.userHandle),
		},
	}
	return jsonRequest(t, body)
}

func clientDataJSON(t *testing.T, ceremony, challenge string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": ceremony, "challenge": challenge, "origin": testOrigin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func jsonRequest(t *testing.T, body map[string]any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, testOrigin, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func encode(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func dummyCredential(id byte) webauthn.Credential {
	return webauthn.Credential{ID: []byte{id}, PublicKey: []byte{1, id}}
}
