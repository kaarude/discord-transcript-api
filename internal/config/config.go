package config

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kaarude/discord-transcript-api/internal/securefile"
)

type RateLimit struct {
	Max      int `json:"max"`
	WindowMS int `json:"windowMs"`
}

type Token struct {
	ID        string  `json:"id"`
	Hash      string  `json:"hash"`
	Preview   string  `json:"preview,omitempty"`
	CreatedAt *string `json:"createdAt"`
	CreatedBy string  `json:"createdBy,omitempty"`
}

// storedToken accepts both the current hashed format and the legacy plaintext
// format at the JSON boundary. Runtime settings only expose Token, which has no
// field capable of retaining a plaintext secret.
type storedToken struct {
	ID        string  `json:"id"`
	Value     string  `json:"value,omitempty"`
	Hash      string  `json:"hash,omitempty"`
	Preview   string  `json:"preview,omitempty"`
	CreatedAt *string `json:"createdAt"`
	CreatedBy string  `json:"createdBy,omitempty"`
}

// HashToken derives the at-rest representation for an API token. The plaintext
// value is never persisted; callers keep it only in the response body that
// returns it to the operator.
func HashToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func previewOf(value string) string {
	if len(value) > 8 {
		return value[:8] + "..."
	}
	return value
}

// NewTokenRecord builds a token record from a freshly generated or legacy
// migrated secret. Only the hash and a short prefix are retained on disk.
func NewTokenRecord(id, value, createdBy string) Token {
	return Token{ID: id, Hash: HashToken(value), Preview: previewOf(value), CreatedBy: createdBy}
}

func tokenFromStorage(entry storedToken) (Token, error) {
	if entry.Value != "" {
		record := NewTokenRecord(entry.ID, entry.Value, entry.CreatedBy)
		record.CreatedAt = entry.CreatedAt
		return record, nil
	}
	decoded, err := hex.DecodeString(entry.Hash)
	if err != nil || len(decoded) != sha256.Size {
		return Token{}, fmt.Errorf("token %q has an invalid SHA-256 hash", entry.ID)
	}
	return Token{ID: entry.ID, Hash: strings.ToLower(entry.Hash), Preview: entry.Preview, CreatedAt: entry.CreatedAt, CreatedBy: entry.CreatedBy}, nil
}

func appendUniqueTokens(tokens []Token, candidates ...Token) []Token {
	known := make(map[string]bool, len(tokens)+len(candidates))
	for _, token := range tokens {
		known[token.Hash] = true
	}
	for _, candidate := range candidates {
		if !known[candidate.Hash] {
			known[candidate.Hash] = true
			tokens = append(tokens, candidate)
		}
	}
	return tokens
}

type Values struct {
	RateLimit       RateLimit `json:"rateLimit"`
	TranscriptLimit int       `json:"transcriptLimit"`
	Tokens          []Token   `json:"tokens"`
}

type Settings struct {
	mu     sync.RWMutex
	path   string
	v      Values
	extras map[string]json.RawMessage
}

func Env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func SettingsPath() string {
	return Env("SETTINGS_PATH", filepath.Join("data", "settings.json"))
}

func AuthPath() string {
	return Env("AUTH_PATH", filepath.Join("data", "auth.json"))
}

func RegistryPath() string {
	return Env("TEMP_TRANSCRIPT_REGISTRY", filepath.Join("data", "transcripts.jsonl"))
}

func TranscriptsDir() string {
	return Env("TEMP_TRANSCRIPT_DIR", "transcripts")
}

func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(strings.TrimSpace(name)); !exists {
			_ = os.Setenv(strings.TrimSpace(name), value)
		}
	}
}

func intEnv(name string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func defaults() Values {
	var apiTokens []string
	for _, token := range strings.Split(os.Getenv("API_TOKENS"), ",") {
		if token = strings.TrimSpace(token); token != "" {
			apiTokens = append(apiTokens, token)
		}
	}
	tokens := make([]Token, 0, len(apiTokens))
	for _, value := range apiTokens {
		tokens = append(tokens, NewTokenRecord(RandomHex(8), value, "environment"))
	}
	return Values{
		RateLimit:       RateLimit{Max: intEnv("RATE_LIMIT_MAX", 25), WindowMS: intEnv("RATE_LIMIT_WINDOW_MS", 60000)},
		TranscriptLimit: intEnv("TRANSCRIPT_LIMIT", 1000),
		Tokens:          tokens,
	}
}

func Open(path string) (*Settings, error) {
	s := &Settings{path: path, v: defaults(), extras: make(map[string]json.RawMessage)}
	environmentTokens := append([]Token(nil), s.v.Tokens...)
	raw, err := os.ReadFile(path)
	if err == nil {
		var disk struct {
			RateLimit       RateLimit     `json:"rateLimit"`
			TranscriptLimit int           `json:"transcriptLimit"`
			Tokens          []storedToken `json:"tokens"`
			APITokens       []string      `json:"apiTokens"`
			AdminToken      string        `json:"adminToken"`
		}
		if err := json.Unmarshal(raw, &disk); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if err := json.Unmarshal(raw, &s.extras); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		delete(s.extras, "rateLimit")
		delete(s.extras, "transcriptLimit")
		delete(s.extras, "tokens")
		delete(s.extras, "apiTokens")
		if disk.RateLimit.Max > 0 {
			s.v.RateLimit.Max = disk.RateLimit.Max
		}
		if disk.RateLimit.WindowMS > 0 {
			s.v.RateLimit.WindowMS = disk.RateLimit.WindowMS
		}
		if disk.TranscriptLimit > 0 {
			s.v.TranscriptLimit = disk.TranscriptLimit
		}
		if disk.Tokens != nil {
			s.v.Tokens = make([]Token, 0, len(disk.Tokens))
			for _, entry := range disk.Tokens {
				token, parseErr := tokenFromStorage(entry)
				if parseErr != nil {
					return nil, fmt.Errorf("parse %s: %w", path, parseErr)
				}
				s.v.Tokens = appendUniqueTokens(s.v.Tokens, token)
			}
		}
		for _, value := range disk.APITokens {
			if value = strings.TrimSpace(value); value != "" {
				s.v.Tokens = appendUniqueTokens(s.v.Tokens, NewTokenRecord(RandomHex(8), value, "legacy-migration"))
			}
		}
		s.v.Tokens = appendUniqueTokens(s.v.Tokens, environmentTokens...)
		if s.v.RateLimit.Max <= 0 {
			s.v.RateLimit.Max = 25
		}
		if s.v.RateLimit.WindowMS <= 0 {
			s.v.RateLimit.WindowMS = 60000
		}
		if s.v.TranscriptLimit <= 0 {
			s.v.TranscriptLimit = 1000
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Persist defaults and migrated token records while retaining unrelated
	// legacy and unknown fields. The plaintext apiTokens field is deliberately
	// removed after its values have been migrated.
	if err != nil || len(raw) > 0 {
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Settings) Snapshot() Values {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, _ := json.Marshal(s.v)
	var copy Values
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func (s *Settings) Update(fn func(*Values) error) (Values, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.v)
	var next Values
	_ = json.Unmarshal(raw, &next)
	if err := fn(&next); err != nil {
		return Values{}, err
	}
	previous := s.v
	s.v = next
	if err := s.saveLocked(); err != nil {
		s.v = previous
		return Values{}, err
	}
	return s.v, nil
}

func (s *Settings) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	disk := make(map[string]json.RawMessage, len(s.extras)+3)
	for key, value := range s.extras {
		disk[key] = append(json.RawMessage(nil), value...)
	}
	known := map[string]any{"rateLimit": s.v.RateLimit, "transcriptLimit": s.v.TranscriptLimit, "tokens": s.v.Tokens}
	for key, value := range known {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		disk[key] = raw
	}
	raw, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	return securefile.WriteFileAtomic(s.path, append(raw, '\n'), 0o600)
}

func RandomHex(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func NowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }
