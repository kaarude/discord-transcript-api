package securefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicReplacesLooseTargetWithoutFollowingSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret.json")
	victim := filepath.Join(dir, "victim")
	legacyTemporary := target + ".tmp"
	if err := os.WriteFile(victim, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyTemporary, []byte("old-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(victim); err != nil || string(raw) != "unchanged" {
		t.Fatalf("symlink target changed: %q, %v", raw, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement mode is %o, want 600", info.Mode().Perm())
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "secret" {
		t.Fatalf("replacement contents are %q, %v", raw, err)
	}
	if _, err := os.Stat(legacyTemporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy temporary file survived rewrite: %v", err)
	}
}
