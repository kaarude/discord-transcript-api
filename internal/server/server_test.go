package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/kaarude/discord-transcript-api/internal/auth"
	"github.com/kaarude/discord-transcript-api/internal/config"
	"github.com/kaarude/discord-transcript-api/internal/store"
)

type testAuthenticator struct {
	setupRequired bool
	hasPassword   bool
	password      string
	passkeys      []auth.Passkey
}

func (a *testAuthenticator) SetupRequired() bool { return a.setupRequired }
func (a *testAuthenticator) Passkeys() []auth.Passkey {
	return append([]auth.Passkey(nil), a.passkeys...)
}
func (a *testAuthenticator) Authenticated(req *http.Request) (bool, time.Time) {
	cookie, err := req.Cookie("test_auth")
	if err != nil || cookie.Value != "valid" {
		return false, time.Time{}
	}
	return true, time.Now().Add(time.Hour)
}
func (a *testAuthenticator) BeginLogin() (*protocol.CredentialAssertion, *http.Cookie, error) {
	return &protocol.CredentialAssertion{}, testCookie("test_ceremony", "valid"), nil
}
func (a *testAuthenticator) FinishLogin(*http.Request) (*http.Cookie, time.Time, error) {
	expires := time.Now().Add(time.Hour)
	return testCookie("test_auth", "valid"), expires, nil
}
func (a *testAuthenticator) BeginSetup(_, _ string) (*protocol.CredentialCreation, *http.Cookie, error) {
	return &protocol.CredentialCreation{}, testCookie("test_ceremony", "valid"), nil
}
func (a *testAuthenticator) FinishSetup(*http.Request) (*http.Cookie, time.Time, error) {
	expires := time.Now().Add(time.Hour)
	return testCookie("test_auth", "valid"), expires, nil
}
func (a *testAuthenticator) BeginRegistration(string) (*protocol.CredentialCreation, *http.Cookie, error) {
	return &protocol.CredentialCreation{}, testCookie("test_ceremony", "valid"), nil
}
func (a *testAuthenticator) FinishRegistration(*http.Request) error { return nil }
func (a *testAuthenticator) DeletePasskey(id string) error {
	for index, passkey := range a.passkeys {
		if passkey.ID == id {
			a.passkeys = append(a.passkeys[:index], a.passkeys[index+1:]...)
			return nil
		}
	}
	return auth.ErrPasskeyNotFound
}
func (a *testAuthenticator) Logout(*http.Request) *http.Cookie {
	return &http.Cookie{Name: "test_auth", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode}
}
func (a *testAuthenticator) LoginPassword(candidate string) (*http.Cookie, time.Time, error) {
	if !a.hasPassword || a.password == "" || candidate != a.password {
		return nil, time.Time{}, auth.ErrWrongPassword
	}
	expires := time.Now().Add(time.Hour)
	return testCookie("test_auth", "valid"), expires, nil
}
func (a *testAuthenticator) CompletePasswordSetup(_, password string) error {
	a.hasPassword = true
	a.password = password
	return nil
}
func (a *testAuthenticator) SetPassword(current, next string) error {
	if a.hasPassword && current != a.password {
		return auth.ErrWrongPassword
	}
	a.hasPassword = true
	a.password = next
	return nil
}
func (a *testAuthenticator) ClearPassword(current string) error {
	if !a.hasPassword {
		return auth.ErrNoPassword
	}
	if current != a.password {
		return auth.ErrWrongPassword
	}
	if len(a.passkeys) == 0 {
		return auth.ErrLastCredential
	}
	a.hasPassword = false
	a.password = ""
	return nil
}
func (a *testAuthenticator) HasPassword() bool { return a.hasPassword }

func testCookie(name, value string) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}
}

func testApp(t *testing.T) (http.Handler, *store.Registry, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("API_TOKENS", "api-secret")
	settings, err := config.Open(filepath.Join(dir, "data", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := store.Open(filepath.Join(dir, "data", "transcripts.jsonl"), filepath.Join(dir, "transcripts"))
	if err != nil {
		t.Fatal(err)
	}
	return New(settings, registry, slog.New(slog.NewTextHandler(io.Discard, nil)), &testAuthenticator{passkeys: []auth.Passkey{{ID: "primary", Name: "Primary"}}}), registry, dir
}

func request(t *testing.T, app http.Handler, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	return res
}

func TestBudgetWriterStopsBeforeReservationIsExceeded(t *testing.T) {
	var output strings.Builder
	writer := &budgetWriter{writer: &output, remaining: 5}
	if written, err := writer.Write([]byte("1234")); err != nil || written != 4 {
		t.Fatalf("initial write failed: bytes=%d err=%v", written, err)
	}
	if written, err := writer.Write([]byte("56")); !errors.Is(err, errExportTooLarge) || written != 0 {
		t.Fatalf("oversized write was accepted: bytes=%d err=%v", written, err)
	}
	if output.String() != "1234" || writer.remaining != 1 {
		t.Fatalf("budget writer crossed its boundary: output=%q remaining=%d", output.String(), writer.remaining)
	}
}

func TestHealthAndFallback(t *testing.T) {
	app, _, _ := testApp(t)
	headers := map[string]string{"Cookie": "test_auth=valid"}
	health := request(t, app, http.MethodGet, "/health?format=json", "", headers)
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"name":"Discord Transcript API"`) || !strings.Contains(health.Body.String(), `"eventLoopLagP95Ms"`) || !strings.Contains(health.Body.String(), `"lastUpdate"`) {
		t.Fatalf("bad health response: %d %s", health.Code, health.Body.String())
	}
	page := request(t, app, http.MethodGet, "/health", "", map[string]string{"Accept": "text/html", "Cookie": "test_auth=valid"})
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Connection diagnostics") || !strings.Contains(page.Body.String(), "/health/ping") {
		t.Fatal("health page missing")
	}
	missing := request(t, app, http.MethodGet, "/nope", "", nil)
	if missing.Code != 404 || !strings.Contains(missing.Body.String(), "Not Found") {
		t.Fatal("JSON fallback missing")
	}
}

func TestAdminSettingsAndTokenLifecycle(t *testing.T) {
	app, registry, dir := testApp(t)
	unauthorized := request(t, app, http.MethodGet, "/api/admin/settings", "", nil)
	if unauthorized.Code != 401 {
		t.Fatalf("expected 401, got %d", unauthorized.Code)
	}
	if !strings.Contains(unauthorized.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("unauthorized admin response is cacheable: %v", unauthorized.Header())
	}
	headers := map[string]string{"Cookie": "test_auth=valid", "Content-Type": "application/json"}
	settings := request(t, app, http.MethodGet, "/api/admin/settings", "", headers)
	if settings.Code != 200 || !strings.Contains(settings.Body.String(), "apiTokens") {
		t.Fatalf("bad settings: %d %s", settings.Code, settings.Body.String())
	}
	if !strings.Contains(settings.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("admin settings response is cacheable: %v", settings.Header())
	}
	created := request(t, app, http.MethodPost, "/api/admin/tokens", "", headers)
	if created.Code != 201 {
		t.Fatalf("token create failed: %d %s", created.Code, created.Body.String())
	}
	if !strings.Contains(created.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("new token response is cacheable: %v", created.Header())
	}
	var payload map[string]string
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil || len(payload["token"]) != 64 {
		t.Fatalf("bad token: %#v %v", payload, err)
	}
	// The minted secret must authenticate immediately through its stored hash.
	accepted := request(t, app, http.MethodGet, "/transcript", "", map[string]string{"Authorization": payload["token"]})
	if accepted.Code != http.StatusBadRequest || !strings.Contains(accepted.Body.String(), "Discord-Bot-Token") {
		t.Fatalf("hashed token rejected at runtime: %d %s", accepted.Code, accepted.Body.String())
	}
	rawSettings, err := os.ReadFile(filepath.Join(dir, "data", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawSettings), payload["token"]) {
		t.Fatal("plaintext API token was persisted to disk")
	}
	reloadedSettings, err := config.Open(filepath.Join(dir, "data", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	reloadedApp := New(reloadedSettings, registry, slog.New(slog.NewTextHandler(io.Discard, nil)), &testAuthenticator{passkeys: []auth.Passkey{{ID: "primary", Name: "Primary"}}})
	reloadedAccepted := request(t, reloadedApp, http.MethodGet, "/transcript", "", map[string]string{"Authorization": payload["token"]})
	if reloadedAccepted.Code != http.StatusBadRequest || !strings.Contains(reloadedAccepted.Body.String(), "Discord-Bot-Token") {
		t.Fatalf("persisted token hash rejected after reload: %d %s", reloadedAccepted.Code, reloadedAccepted.Body.String())
	}
	deleted := request(t, app, http.MethodDelete, "/api/admin/tokens/"+payload["id"], "", headers)
	if deleted.Code != 200 {
		t.Fatalf("token delete failed: %d %s", deleted.Code, deleted.Body.String())
	}
}

func TestPrivateTranscriptAdminResponseIsNotCacheable(t *testing.T) {
	app, registry, _ := testApp(t)
	id := strings.Repeat("e", 32)
	if err := registry.Add(store.Record{UUID: id, ChannelID: "1", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), StorageVersion: 2, IsPublic: store.Public(false), AccessKey: "private-access-secret"}); err != nil {
		t.Fatal(err)
	}
	res := request(t, app, http.MethodGet, "/api/admin/transcripts", "", map[string]string{"Cookie": "test_auth=valid"})
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "private-access-secret") {
		t.Fatalf("private transcript missing from admin response: %d %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("private transcript response is cacheable: %v", res.Header())
	}
}

func TestPasswordEndpointLifecycle(t *testing.T) {
	app, _, _ := testApp(t)

	setup := request(t, app, http.MethodPost, "/access/setup/password", `{"setupCode":"code","password":"a long setup password"}`, map[string]string{"Content-Type": "application/json"})
	if setup.Code != http.StatusCreated || !strings.Contains(setup.Body.String(), `"authenticated":true`) {
		t.Fatalf("password setup failed: %d %s", setup.Code, setup.Body.String())
	}

	login := request(t, app, http.MethodPost, "/access/login/password", `{"password":"a long setup password"}`, map[string]string{"Content-Type": "application/json"})
	if login.Code != http.StatusOK {
		t.Fatalf("password login failed: %d %s", login.Code, login.Body.String())
	}
	crossOrigin := request(t, app, http.MethodPost, "/access/login/password", `{"password":"x"}`, map[string]string{"Content-Type": "application/json", "Origin": "https://attacker.test"})
	if crossOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin password login returned %d", crossOrigin.Code)
	}
	badLogin := request(t, app, http.MethodPost, "/access/login/password", `{"password":"wrong one here!"}`, map[string]string{"Content-Type": "application/json"})
	if badLogin.Code != http.StatusUnauthorized || !strings.Contains(badLogin.Body.String(), "Incorrect password") {
		t.Fatalf("wrong password accepted: %d %s", badLogin.Code, badLogin.Body.String())
	}

	headers := map[string]string{"Cookie": "test_auth=valid", "Content-Type": "application/json"}
	rotate := request(t, app, http.MethodPut, "/api/admin/password", `{"currentPassword":"a long setup password","newPassword":"rotated to a new one"}`, headers)
	if rotate.Code != http.StatusOK {
		t.Fatalf("rotation failed: %d %s", rotate.Code, rotate.Body.String())
	}
	status := request(t, app, http.MethodGet, "/access/status", "", nil)
	var payload struct {
		SetupRequired bool   `json:"setupRequired"`
		HasPassword   bool   `json:"hasPassword"`
		HasPasskeys   bool   `json:"hasPasskeys"`
		Authenticated bool   `json:"authenticated"`
		Error         string `json:"error"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.HasPassword || !payload.HasPasskeys || payload.Authenticated || payload.SetupRequired {
		t.Fatalf("unexpected status after rotation: %#v", payload)
	}
	remove := request(t, app, http.MethodDelete, "/api/admin/password", `{"currentPassword":"rotated to a new one"}`, headers)
	if remove.Code != http.StatusOK {
		t.Fatalf("removal failed: %d %s", remove.Code, remove.Body.String())
	}
	last := request(t, app, http.MethodDelete, "/api/admin/password", `{"currentPassword":"anything"}`, headers)
	if last.Code != http.StatusConflict || !strings.Contains(last.Body.String(), "No dashboard") {
		t.Fatalf("double removal was accepted: %d %s", last.Code, last.Body.String())
	}
}

func TestInvalidTokenAttemptsAreThrottled(t *testing.T) {
	app, _, _ := testApp(t)
	status := func(code int) {
		res := request(t, app, http.MethodGet, "/transcript", "", map[string]string{"Authorization": "wrong-secret"})
		if res.Code != code {
			t.Fatalf("expected %d for invalid token attempts, got %d: %s", code, res.Code, res.Body.String())
		}
	}
	for range 60 {
		status(http.StatusUnauthorized)
	}
	// The 61st failed guess from the same address is throttled.
	status(http.StatusTooManyRequests)
	// Successful authentication never consumes the failure budget.
	valid := request(t, app, http.MethodGet, "/transcript", "", map[string]string{"Authorization": "api-secret", "Discord-Bot-Token": "x"})
	if valid.Code != http.StatusBadRequest || !strings.Contains(valid.Body.String(), "Channel-Id") {
		t.Fatalf("valid token rejected after failures: %d %s", valid.Code, valid.Body.String())
	}
}

func TestPasskeyBrowserLoginAndLogout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("API_TOKENS", "api-secret")
	settings, _ := config.Open(filepath.Join(dir, "data", "settings.json"))
	registry, _ := store.Open(filepath.Join(dir, "data", "transcripts.jsonl"), filepath.Join(dir, "transcripts"))
	app := New(settings, registry, slog.New(slog.NewTextHandler(io.Discard, nil)), &testAuthenticator{passkeys: []auth.Passkey{{ID: "primary", Name: "Primary"}}})

	redirect := request(t, app, http.MethodGet, "/health", "", map[string]string{"Accept": "text/html"})
	if redirect.Code != http.StatusFound || redirect.Header().Get("Location") != "/access?returnTo=%2Fhealth" {
		t.Fatalf("browser was not redirected safely: %d %s", redirect.Code, redirect.Header().Get("Location"))
	}
	page := request(t, app, http.MethodGet, "/access", "", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Sign in") || strings.Contains(page.Body.String(), "localStorage") || strings.Contains(page.Body.String(), "api-secret") {
		t.Fatal("unsafe or missing access page")
	}
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || page.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("access page security headers missing: %#v", page.Header())
	}
	crossOrigin := request(t, app, http.MethodPost, "/access/passkeys/login/start", "", map[string]string{"Origin": "https://attacker.test"})
	if crossOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login start returned %d", crossOrigin.Code)
	}
	wrongScheme := request(t, app, http.MethodPost, "/access/passkeys/login/start", "", map[string]string{"Origin": "http://example.com"})
	if wrongScheme.Code != http.StatusForbidden {
		t.Fatalf("cross-scheme login start returned %d", wrongScheme.Code)
	}
	start := request(t, app, http.MethodPost, "/access/passkeys/login/start", "", nil)
	if start.Code != http.StatusOK || !strings.Contains(start.Header().Get("Set-Cookie"), "HttpOnly") || !strings.Contains(start.Header().Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("login start failed: %d %#v %s", start.Code, start.Header(), start.Body.String())
	}
	login := request(t, app, http.MethodPost, "/access/passkeys/login/finish", `{}`, map[string]string{"Content-Type": "application/json", "Cookie": "test_ceremony=valid"})
	if login.Code != http.StatusOK {
		t.Fatalf("login finish failed: %d %#v %s", login.Code, login.Header(), login.Body.String())
	}
	cookie := strings.Split(login.Header().Get("Set-Cookie"), ";")[0]
	authorized := request(t, app, http.MethodGet, "/admin/", "", map[string]string{"Accept": "text/html", "Cookie": cookie})
	if authorized.Code != http.StatusOK || !strings.Contains(authorized.Body.String(), "Administration") {
		t.Fatalf("session rejected: %d %s", authorized.Code, authorized.Body.String())
	}
	logout := request(t, app, http.MethodPost, "/access/logout", "", map[string]string{"Cookie": cookie})
	if logout.Code != http.StatusOK || !strings.Contains(logout.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout did not clear the session: %d %#v", logout.Code, logout.Header())
	}
}

func TestLegacyAndPrivateTranscriptServing(t *testing.T) {
	app, registry, _ := testApp(t)
	id := "abcd1234abcd1234"
	path := filepath.Join(registry.Dir(), id+".html")
	if err := os.WriteFile(path, []byte("<html>legacy</html>"), 0640); err != nil {
		t.Fatal(err)
	}
	record := store.Record{UUID: id, ChannelID: "1", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), FilePath: "transcripts/" + id + ".html", IsPublic: store.Public(true)}
	if err := registry.Add(record); err != nil {
		t.Fatal(err)
	}
	served := request(t, app, http.MethodGet, "/transcripts/"+id, "", nil)
	if served.Code != 200 || served.Body.String() != "<html>legacy</html>" || served.Header().Get("Cache-Control") != "no-store" || served.Header().Get("Content-Disposition") != `inline; filename="transcript-`+id+`.html"` || !strings.Contains(served.Header().Get("Content-Security-Policy"), "default-src 'none'") || served.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("legacy serving failed: %d %#v %s", served.Code, served.Header(), served.Body.String())
	}
	private, ok, err := registry.SetVisibility(id, false)
	if err != nil || !ok {
		t.Fatal(err)
	}
	hidden := request(t, app, http.MethodGet, "/transcripts/"+id, "", nil)
	if hidden.Code != 404 {
		t.Fatalf("private transcript exposed: %d", hidden.Code)
	}
	unlocked := request(t, app, http.MethodGet, "/transcripts/"+id+"?access="+private.AccessKey, "", nil)
	if unlocked.Code != 302 || unlocked.Header().Get("Location") != "/transcripts/"+id || !strings.Contains(unlocked.Header().Get("Set-Cookie"), "HttpOnly") || !strings.Contains(unlocked.Header().Get("Set-Cookie"), "Secure") {
		t.Fatalf("unlock failed: %d %#v", unlocked.Code, unlocked.Header())
	}
}

func TestPrivateTranscriptWithoutAccessKeyFailsClosedEverywhere(t *testing.T) {
	app, registry, _ := testApp(t)
	id := strings.Repeat("b", 32)
	root := filepath.Join(registry.Dir(), id)
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o750); err != nil {
		t.Fatal(err)
	}
	asset := strings.Repeat("c", 64) + ".png"
	for path, contents := range map[string]string{
		filepath.Join(root, "index.html"):      "private",
		filepath.Join(root, "transcript.json"): `{}`,
		filepath.Join(root, "assets", asset):   "image",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	record := store.Record{UUID: id, ChannelID: "1", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), StorageVersion: 2, FilePath: "transcripts/" + id + "/index.html", IsPublic: store.Public(false)}
	if err := registry.Add(record); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/transcripts/" + id, "/transcripts/" + id + "/download/json", "/transcripts/" + id + "/assets/" + asset} {
		res := request(t, app, http.MethodGet, path, "", nil)
		if res.Code != http.StatusNotFound {
			t.Errorf("%s returned %d, want 404", path, res.Code)
		}
	}
}

func TestPrivateDownloadAndAssetContinueAfterQueryUnlock(t *testing.T) {
	app, registry, _ := testApp(t)
	id := strings.Repeat("d", 32)
	root := filepath.Join(registry.Dir(), id)
	asset := strings.Repeat("a", 64) + ".png"
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o750); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(root, "index.html"):      "private",
		filepath.Join(root, "transcript.json"): `{"private":true}`,
		filepath.Join(root, "assets", asset):   "image",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	record := store.Record{UUID: id, ChannelID: "1", ChannelName: "private", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), StorageVersion: 2, FilePath: "transcripts/" + id + "/index.html", IsPublic: store.Public(false), AccessKey: "private-key"}
	if err := registry.Add(record); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/transcripts/" + id + "/download/json?access=private-key", "/transcripts/" + id + "/assets/" + asset + "?access=private-key"} {
		res := request(t, app, http.MethodGet, path, "", nil)
		if res.Code != http.StatusOK || !strings.Contains(res.Header().Get("Set-Cookie"), "transcript_access_"+id) {
			t.Errorf("%s did not continue after unlock: %d %#v", path, res.Code, res.Header())
		}
	}
}

func TestTrustedProxyUsesRightmostUntrustedForwardedAddress(t *testing.T) {
	app := &App{trustProxy: "127.0.0.1"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Header.Set("X-Forwarded-For", "2001:db8::dead, 198.51.100.20")
	if got := app.clientIP(req); got != "198.51.100.20" {
		t.Fatalf("spoofable forwarded address selected: %q", got)
	}
}

func TestTrustAnyDirectProxyStillRejectsPrependedSpoof(t *testing.T) {
	app := &App{trustProxy: "true"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.8:4321"
	req.Header.Set("X-Forwarded-For", "2001:db8::dead, 198.51.100.20")
	if got := app.clientIP(req); got != "198.51.100.20" {
		t.Fatalf("prepended address selected: %q", got)
	}
}

func TestBearerAndAdminHeadersDoNotBypassPasskeySession(t *testing.T) {
	app, _, _ := testApp(t)
	for _, headers := range []map[string]string{
		{"Authorization": "Bearer obsolete-secret"},
		{"X-Admin-Token": "obsolete-secret"},
	} {
		res := request(t, app, http.MethodGet, "/health?format=json", "", headers)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("legacy access header returned %d", res.Code)
		}
	}
}

func TestLimiterRefusesUnboundedCardinality(t *testing.T) {
	limit := newLimiter()
	for index := 0; index < maxLimiterEntries; index++ {
		if allowed, _, _ := limit.allow(strconv.Itoa(index), 1, time.Hour); !allowed {
			t.Fatalf("entry %d unexpectedly rejected", index)
		}
	}
	if allowed, _, _ := limit.allow("overflow", 1, time.Hour); allowed || len(limit.values) != maxLimiterEntries {
		t.Fatalf("unbounded limiter state: allowed=%v entries=%d", allowed, len(limit.values))
	}
}

func TestTranscriptStorageReservationEnforcesGlobalCeiling(t *testing.T) {
	dir := t.TempDir()
	registry, err := store.Open(filepath.Join(dir, "data", "transcripts.jsonl"), filepath.Join(dir, "transcripts"))
	if err != nil {
		t.Fatal(err)
	}
	app := &App{registry: registry, storageLimit: storageReservation - 1}
	if release, err := app.reserveTranscriptStorage(); err != nil || release != nil {
		t.Fatalf("storage ceiling was not enforced: release=%v err=%v", release != nil, err)
	}
	app.storageLimit = 2 * storageReservation
	release, err := app.reserveTranscriptStorage()
	if err != nil || release == nil || app.storageReserved != storageReservation {
		t.Fatalf("valid reservation failed: reserved=%d err=%v", app.storageReserved, err)
	}
	release()
	if app.storageReserved != 0 {
		t.Fatalf("reservation leaked: %d", app.storageReserved)
	}
}

func TestOnlyOneTranscriptExportCanRunAtOnce(t *testing.T) {
	app := &App{exportSlots: make(chan struct{}, 1)}
	if !app.acquireExport() {
		t.Fatal("first export slot was rejected")
	}
	if app.acquireExport() {
		t.Fatal("concurrent export slot was accepted")
	}
	app.releaseExport()
	if !app.acquireExport() {
		t.Fatal("released export slot was not reusable")
	}
	app.releaseExport()
}

func TestAccessStatusReportsFirstTimeSetupWithoutFailingReadiness(t *testing.T) {
	dir := t.TempDir()
	settings, _ := config.Open(filepath.Join(dir, "data", "settings.json"))
	registry, _ := store.Open(filepath.Join(dir, "data", "transcripts.jsonl"), filepath.Join(dir, "transcripts"))
	app := New(settings, registry, slog.New(slog.NewTextHandler(io.Discard, nil)), &testAuthenticator{setupRequired: true})
	res := request(t, app, http.MethodGet, "/access/status", "", nil)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"setupRequired":true`) || !strings.Contains(res.Body.String(), `"authenticated":false`) {
		t.Fatalf("first-time setup made liveness fail: %d %s", res.Code, res.Body.String())
	}
}

func TestExpiredPrivateUnlockStillSetsShortLivedCookie(t *testing.T) {
	app, registry, _ := testApp(t)
	id := strings.Repeat("e", 32)
	if err := os.WriteFile(filepath.Join(registry.Dir(), id+".html"), []byte("old"), 0640); err != nil {
		t.Fatal(err)
	}
	record := store.Record{UUID: id, ChannelID: "1", CreatedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), FilePath: "transcripts/" + id + ".html", IsPublic: store.Public(false), AccessKey: "private-key"}
	if err := registry.Add(record); err != nil {
		t.Fatal(err)
	}
	res := request(t, app, http.MethodGet, "/transcripts/"+id+"?access=private-key", "", nil)
	if res.Code != http.StatusFound || !strings.Contains(res.Header().Get("Set-Cookie"), "Max-Age=60") {
		t.Fatalf("expected short-lived unlock cookie: %d %#v", res.Code, res.Header())
	}
}

func TestValidationAndAuthErrors(t *testing.T) {
	app, _, _ := testApp(t)
	invalid := request(t, app, http.MethodGet, "/transcripts/not-hex", "", nil)
	if invalid.Code != 400 {
		t.Fatalf("expected invalid transcript 400, got %d", invalid.Code)
	}
	missing := request(t, app, http.MethodGet, "/transcript", "", nil)
	if missing.Code != 401 {
		t.Fatalf("expected auth 401, got %d", missing.Code)
	}
	missingBot := request(t, app, http.MethodGet, "/transcript", "", map[string]string{"Authorization": "api-secret"})
	if missingBot.Code != 400 || !strings.Contains(missingBot.Body.String(), "Discord-Bot-Token") {
		t.Fatalf("expected bot token error, got %d %s", missingBot.Code, missingBot.Body.String())
	}
}

func TestRefreshStoredHTMLUpgradesOldBundleAndPreservesColor(t *testing.T) {
	dir := t.TempDir()
	registry, err := store.Open(filepath.Join(dir, "data", "transcripts.jsonl"), filepath.Join(dir, "transcripts"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	root := filepath.Join(registry.Dir(), id)
	if err := os.MkdirAll(root, 0750); err != nil {
		t.Fatal(err)
	}
	exported := map[string]any{"transcript": map[string]any{"channel": map[string]any{"id": "1", "name": "general"}, "roles": []any{}}, "messages": []any{map[string]any{"id": "1", "type": 0, "content": "hello", "author": map[string]any{"id": "u1", "username": "Alice"}, "timestamp": "2024-01-01T12:00:00Z"}}}
	raw, _ := json.Marshal(exported)
	if err := os.WriteFile(filepath.Join(root, "transcript.json"), raw, 0640); err != nil {
		t.Fatal(err)
	}
	old := `<span class="username" style="color:#123456">Alice</span>`
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(old), 0640); err != nil {
		t.Fatal(err)
	}
	record := store.Record{UUID: id, ChannelID: "1", ChannelName: "general", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), StorageVersion: 2, RendererVersion: 1, FilePath: "transcripts/" + id + "/index.html", IsPublic: store.Public(false), AccessKey: "keep-me"}
	if err := registry.Add(record); err != nil {
		t.Fatal(err)
	}
	count, failures := RefreshStoredHTML(registry)
	if count != 1 || len(failures) != 0 {
		t.Fatalf("refresh result %d %#v", count, failures)
	}
	next, _ := os.ReadFile(filepath.Join(root, "index.html"))
	if !strings.Contains(string(next), `style="color:#123456"`) || !strings.Contains(string(next), `id="profile-card"`) {
		t.Fatalf("refresh output missing parity: %s", next)
	}
	updated, _ := registry.Get(id)
	if updated.RendererVersion != RendererVersion || updated.AccessKey != "keep-me" || updated.Public() {
		t.Fatalf("metadata was not safely updated: %#v", updated)
	}
}
