package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsPersistAndMigrateLegacyTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	t.Setenv("API_TOKENS", "legacy-one,legacy-two")
	settings, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := settings.Snapshot()
	if len(got.Tokens) != 2 || got.Tokens[0].Hash != HashToken("legacy-one") {
		t.Fatalf("legacy migration failed: %#v", got.Tokens)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "legacy-one") {
		t.Fatalf("plaintext token persisted to disk: %s", raw)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("settings not persisted: %v", err)
	}
	if _, err := settings.Update(func(values *Values) error { values.TranscriptLimit = 42; return nil }); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(path)
	if err != nil || reloaded.Snapshot().TranscriptLimit != 42 {
		t.Fatalf("updated settings did not persist: %v", err)
	}
}

func TestSettingsPreservesLegacyFieldsWithoutUsingThemForActiveValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("API_TOKENS", "environment-token")
	if err := os.WriteFile(path, []byte(`{"adminToken":"keep-but-inert","apiTokens":["legacy-token"],"tokens":[{"id":"disk","value":"disk-token"}],"futureConfig":{"nested":["keep",1]},"rateLimit":{"max":10,"windowMs":2000},"transcriptLimit":50}`), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.Snapshot(); len(got.Tokens) != 3 || got.Tokens[0].Hash != HashToken("disk-token") || got.Tokens[1].Hash != HashToken("legacy-token") || got.Tokens[2].Hash != HashToken("environment-token") {
		t.Fatalf("legacy token not migrated: %#v", got)
	}
	if _, err := settings.Update(func(values *Values) error {
		values.RateLimit.Max = 11
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, plaintext := range []string{"disk-token", "legacy-token", "environment-token"} {
		if strings.Contains(string(raw), plaintext) {
			t.Fatalf("plaintext token %q survived migration: %s", plaintext, raw)
		}
	}
	if strings.Contains(string(raw), `"apiTokens"`) {
		t.Fatalf("legacy plaintext field survived migration: %s", raw)
	}
	if !strings.Contains(string(raw), `"adminToken": "keep-but-inert"`) || !strings.Contains(string(raw), `"futureConfig"`) || !strings.Contains(string(raw), `"keep"`) {
		t.Fatalf("legacy data was erased: %s", raw)
	}
	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Snapshot().Tokens; len(got) != 3 {
		t.Fatalf("token migration was not idempotent: %#v", got)
	}
}

func TestSettingsRejectsMalformedHashedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"tokens":[{"id":"broken","hash":"not-a-sha256-hash"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "invalid SHA-256 hash") {
		t.Fatalf("malformed token hash was accepted: %v", err)
	}
}

func TestSettingsRewriteDoesNotReuseLooseTemporaryMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path+".tmp", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+".tmp", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode is %o, want 600", info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("legacy settings temporary file survived rewrite: %v", err)
	}
}

func TestRandomHex(t *testing.T) {
	if got := RandomHex(16); len(got) != 32 {
		t.Fatalf("expected 32 hex characters, got %q", got)
	}
}
