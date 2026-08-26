package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kaarude/discord-transcript-api/internal/config"
	"github.com/kaarude/discord-transcript-api/internal/securefile"
)

const (
	ExpiryDays               = 30
	registryCompactThreshold = 5 << 20
)

type Participant struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"displayName"`
	Bot          bool   `json:"bot"`
	MessageCount int    `json:"messageCount"`
}

type Record struct {
	UUID              string        `json:"uuid"`
	ChannelID         string        `json:"channelId"`
	ChannelName       string        `json:"channelName,omitempty"`
	CreatedAt         string        `json:"createdAt"`
	ExpiresAt         string        `json:"expiresAt"`
	StorageVersion    int           `json:"storageVersion,omitempty"`
	RendererVersion   int           `json:"rendererVersion,omitempty"`
	FilePath          string        `json:"filePath"`
	Directory         string        `json:"directory,omitempty"`
	Participants      []Participant `json:"participants"`
	MessageCount      int           `json:"messageCount,omitempty"`
	CachedBytes       int64         `json:"cachedBytes,omitempty"`
	Exports           []string      `json:"exports"`
	IsPublic          *bool         `json:"isPublic,omitempty"`
	AccessKey         string        `json:"accessKey,omitempty"`
	RenewedAt         string        `json:"renewedAt,omitempty"`
	RefreshedAt       string        `json:"refreshedAt,omitempty"`
	VisibilityUpdated string        `json:"visibilityUpdatedAt,omitempty"`
	DeletedAt         string        `json:"deletedAt,omitempty"`
	FileSize          int64         `json:"fileSize,omitempty"`
	IsExpired         bool          `json:"isExpired,omitempty"`
	ViewURL           string        `json:"viewUrl,omitempty"`
}

func Public(value bool) *bool { return &value }

func (r Record) Public() bool { return r.IsPublic == nil || *r.IsPublic }

func (r Record) ViewPath() string {
	base := "/transcripts/" + r.UUID
	if !r.Public() && r.AccessKey != "" {
		return base + "?access=" + r.AccessKey
	}
	return base
}

func (r Record) Expired() bool {
	expires, err := time.Parse(time.RFC3339Nano, r.ExpiresAt)
	return err != nil || expires.Before(time.Now())
}

type Registry struct {
	mu             sync.RWMutex
	path           string
	transcriptsDir string
	records        map[string]Record
	order          []string
}

func Open(path, transcriptsDir string) (*Registry, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(transcriptsDir, 0o750); err != nil {
		return nil, err
	}
	r := &Registry{path: path, transcriptsDir: transcriptsDir, records: map[string]Record{}}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var record Record
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.UUID == "" {
			continue
		}
		if record.DeletedAt != "" {
			delete(r.records, record.UUID)
			continue
		}
		if _, exists := r.records[record.UUID]; !exists {
			r.order = append(r.order, record.UUID)
		}
		r.records[record.UUID] = record
	}
	return r, scanner.Err()
}

func (r *Registry) Dir() string { return r.transcriptsDir }

func (r *Registry) Records() []Record {
	r.mu.RLock()
	defer r.mu.RUnlock()
	records := make([]Record, 0, len(r.records))
	for _, id := range r.order {
		if record, ok := r.records[id]; ok {
			records = append(records, record)
		}
	}
	return records
}

func (r *Registry) appendLocked(record Record) error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if record.DeletedAt != "" {
		delete(r.records, record.UUID)
		return nil
	}
	if _, exists := r.records[record.UUID]; !exists {
		r.order = append(r.order, record.UUID)
	}
	r.records[record.UUID] = record
	return nil
}

func (r *Registry) Add(record Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.appendLocked(record)
}

func (r *Registry) Get(uuid string) (Record, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	record, ok := r.records[uuid]
	if !ok {
		return Record{}, false
	}
	record.IsExpired = record.Expired()
	if len(record.Exports) == 0 {
		record.Exports = []string{"html"}
	}
	return record, true
}

func (r *Registry) Update(uuid string, fn func(*Record)) (Record, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[uuid]
	if !ok {
		return Record{}, false, nil
	}
	fn(&record)
	if err := r.appendLocked(record); err != nil {
		return Record{}, true, err
	}
	return record, true, nil
}

func (r *Registry) Renew(uuid string) (Record, bool, error) {
	return r.Update(uuid, func(record *Record) {
		now := time.Now().UTC()
		record.ExpiresAt = now.Add(ExpiryDays * 24 * time.Hour).Format(time.RFC3339Nano)
		record.RenewedAt = now.Format(time.RFC3339Nano)
	})
}

func (r *Registry) SetVisibility(uuid string, public bool) (Record, bool, error) {
	return r.Update(uuid, func(record *Record) {
		record.IsPublic = Public(public)
		if public {
			record.AccessKey = ""
		} else {
			record.AccessKey = config.RandomHex(24)
		}
		record.VisibilityUpdated = config.NowISO()
	})
}

func (r *Registry) Remove(uuid string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[uuid]
	if !ok {
		return false, nil
	}
	target := filepath.Join(r.transcriptsDir, uuid)
	if record.StorageVersion != 2 {
		target = filepath.Join(r.transcriptsDir, uuid+".html")
	}
	if err := os.RemoveAll(target); err != nil {
		return true, err
	}
	tombstone := Record{UUID: uuid, DeletedAt: config.NowISO()}
	return true, r.appendLocked(tombstone)
}

type ListOptions struct {
	UserQuery string
	ChannelID string
	Page      int
	Limit     int
}

type ListResult struct {
	Transcripts []Record `json:"transcripts"`
	Total       int      `json:"total"`
	Page        int      `json:"page"`
	Limit       int      `json:"limit"`
}

func (r *Registry) List(options ListOptions) ListResult {
	r.mu.RLock()
	defer r.mu.RUnlock()
	page, limit := options.Page, options.Limit
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	query := strings.ToLower(strings.TrimSpace(options.UserQuery))
	var filtered []Record
	for index := len(r.order) - 1; index >= 0; index-- {
		record, ok := r.records[r.order[index]]
		if !ok || options.ChannelID != "" && record.ChannelID != options.ChannelID {
			continue
		}
		if query != "" && !matchesParticipant(record, query) {
			continue
		}
		filtered = append(filtered, record)
	}
	total := len(filtered)
	start := (page - 1) * limit
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	result := append([]Record(nil), filtered[start:end]...)
	for i := range result {
		result[i].AccessKey = ""
		result[i].IsExpired = result[i].Expired()
		result[i].ViewURL = filtered[start+i].ViewPath()
		if len(result[i].Exports) == 0 {
			result[i].Exports = []string{"html"}
		}
		target := filepath.Join(r.transcriptsDir, result[i].UUID)
		if result[i].StorageVersion != 2 {
			target += ".html"
		}
		result[i].FileSize = directorySize(target)
	}
	return ListResult{Transcripts: result, Total: total, Page: page, Limit: limit}
}

func matchesParticipant(record Record, query string) bool {
	for _, participant := range record.Participants {
		for _, value := range []string{participant.ID, participant.Username, participant.DisplayName} {
			if strings.Contains(strings.ToLower(value), query) {
				return true
			}
		}
	}
	return false
}

func directorySize(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func (r *Registry) Resolve(record Record, format string) string {
	if record.StorageVersion == 2 {
		files := map[string]string{"html": "index.html", "json": "transcript.json", "txt": "transcript.txt"}
		if file := files[format]; file != "" {
			return filepath.Join(r.transcriptsDir, record.UUID, file)
		}
		return ""
	}
	if format == "html" {
		return filepath.Join(r.transcriptsDir, record.UUID+".html")
	}
	return ""
}

func (r *Registry) CleanupInterrupted() error {
	entries, err := os.ReadDir(r.transcriptsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") && len(name) == 37 {
			if err := os.RemoveAll(filepath.Join(r.transcriptsDir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Registry) StorageBytes() int64 {
	return directorySize(r.transcriptsDir)
}

func (r *Registry) Compact() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var records []Record
	for _, id := range r.order {
		if record, ok := r.records[id]; ok {
			records = append(records, record)
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].CreatedAt < records[j].CreatedAt })
	if err := securefile.WriteAtomic(r.path, 0o600, func(writer io.Writer) error {
		enc := json.NewEncoder(writer)
		for _, record := range records {
			if err := enc.Encode(record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(r.path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (r *Registry) compactIfLargerThan(limit int64) (bool, error) {
	info, err := os.Stat(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || info.Size() <= limit {
		return false, err
	}
	return true, r.Compact()
}

func (r *Registry) CompactIfNeeded() (bool, error) {
	return r.compactIfLargerThan(registryCompactThreshold)
}

func (r *Registry) String() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return fmt.Sprintf("Registry{%d records}", len(r.records))
}
