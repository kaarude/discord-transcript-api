package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistryLifecycleAndLegacyReload(t *testing.T) {
	dir := t.TempDir()
	transcripts := filepath.Join(dir, "transcripts")
	registryPath := filepath.Join(dir, "data", "transcripts.jsonl")
	r, err := Open(registryPath, transcripts)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	record := Record{UUID: id, ChannelID: "123", ChannelName: "general", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), StorageVersion: 2, FilePath: "transcripts/" + id + "/index.html", Participants: []Participant{{ID: "42", Username: "alice", DisplayName: "Alice"}}, IsPublic: Public(true)}
	if err := os.MkdirAll(filepath.Join(transcripts, id), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcripts, id, "index.html"), []byte("hello"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(record); err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Get(id); !ok || got.ChannelName != "general" {
		t.Fatalf("record missing: %#v %v", got, ok)
	}
	if listed := r.List(ListOptions{UserQuery: "ALICE", Page: 1, Limit: 50}); listed.Total != 1 || listed.Transcripts[0].FileSize != 5 {
		t.Fatalf("unexpected list: %#v", listed)
	}
	private, ok, err := r.SetVisibility(id, false)
	if err != nil || !ok || private.Public() || private.AccessKey == "" {
		t.Fatalf("visibility failed: %#v %v %v", private, ok, err)
	}
	if _, ok, err = r.Renew(id); err != nil || !ok {
		t.Fatalf("renew failed: %v %v", ok, err)
	}
	reloaded, err := Open(registryPath, transcripts)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.Get(id); !ok || got.AccessKey == "" {
		t.Fatalf("reload lost update: %#v", got)
	}
	if found, err := reloaded.Remove(id); err != nil || !found {
		t.Fatalf("delete failed: %v %v", found, err)
	}
	if _, ok := reloaded.Get(id); ok {
		t.Fatal("deleted record remains active")
	}
}

func TestLegacyRecordAndInterruptedCleanup(t *testing.T) {
	dir := t.TempDir()
	transcripts := filepath.Join(dir, "transcripts")
	if err := os.MkdirAll(transcripts, 0750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "transcripts.jsonl")
	id := "abcd1234abcd1234"
	line := `{"uuid":"` + id + `","channelId":"1","createdAt":"2026-01-01T00:00:00Z","expiresAt":"2099-01-01T00:00:00Z","filePath":"transcripts/` + id + `.html"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcripts, id+".html"), []byte("legacy"), 0640); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(transcripts, "."+strings.Repeat("b", 32)+".tmp")
	if err := os.MkdirAll(temp, 0750); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, transcripts)
	if err != nil {
		t.Fatal(err)
	}
	record, ok := r.Get(id)
	if !ok || r.Resolve(record, "html") != filepath.Join(transcripts, id+".html") {
		t.Fatal("legacy record not resolved")
	}
	if err := r.CleanupInterrupted(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("interrupted directory not removed")
	}
}

func TestMalformedExpiryFailsClosed(t *testing.T) {
	malformed := Record{UUID: strings.Repeat("c", 32), ExpiresAt: "not-a-time", StorageVersion: 2}
	if !malformed.Expired() {
		t.Fatal("malformed expiry failed open")
	}
}

func TestRegistryCompactionRetainsLatestCompatibleState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "transcripts.jsonl")
	r, err := Open(path, filepath.Join(dir, "transcripts"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 32)
	record := Record{UUID: id, ChannelID: "1", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), IsPublic: Public(true)}
	if err := r.Add(record); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.Update(id, func(latest *Record) { latest.ChannelName = "latest" }); err != nil || !ok {
		t.Fatalf("update failed: %v", err)
	}
	compacted, err := r.compactIfLargerThan(1)
	if err != nil || !compacted {
		t.Fatalf("compaction failed: compacted=%v err=%v", compacted, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.Count(strings.TrimSpace(string(raw)), "\n") != 0 {
		t.Fatalf("registry was not reduced to one record: %q %v", raw, err)
	}
	reloaded, err := Open(path, filepath.Join(dir, "transcripts"))
	if err != nil {
		t.Fatal(err)
	}
	if latest, ok := reloaded.Get(id); !ok || latest.ChannelName != "latest" {
		t.Fatalf("compaction lost latest state: %#v", latest)
	}
}
