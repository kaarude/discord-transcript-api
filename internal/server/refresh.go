package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kaarude/discord-transcript-api/internal/model"
	"github.com/kaarude/discord-transcript-api/internal/render"
	"github.com/kaarude/discord-transcript-api/internal/store"
)

// RefreshStoredHTML upgrades older v2 bundles from their saved JSON export.
// The JSON and media are unchanged, so the operation is repeatable and does
// not require Discord credentials or network access.
func RefreshStoredHTML(registry *store.Registry) (int, []error) {
	refreshed := 0
	var failures []error
	for _, record := range registry.Records() {
		if record.StorageVersion != 2 || record.RendererVersion == RendererVersion {
			continue
		}
		root := filepath.Join(registry.Dir(), record.UUID)
		jsonPath, htmlPath := filepath.Join(root, "transcript.json"), filepath.Join(root, "index.html")
		raw, err := os.ReadFile(jsonPath)
		if err != nil {
			failures = append(failures, fmt.Errorf("refresh %s: %w", record.UUID, err))
			continue
		}
		previous, err := os.ReadFile(htmlPath)
		if err != nil {
			failures = append(failures, fmt.Errorf("refresh %s: %w", record.UUID, err))
			continue
		}
		var exported struct {
			Transcript struct {
				Channel model.Object   `json:"channel"`
				Roles   []model.Object `json:"roles"`
			} `json:"transcript"`
			Messages []model.Object `json:"messages"`
		}
		if err := json.Unmarshal(raw, &exported); err != nil || exported.Messages == nil {
			if err == nil {
				err = fmt.Errorf("messages are missing")
			}
			failures = append(failures, fmt.Errorf("refresh %s: %w", record.UUID, err))
			continue
		}
		colors := map[string]string{}
		for _, match := range preservedColor.FindAllSubmatch(previous, -1) {
			colors[string(match[2])] = string(match[1])
		}
		channel := exported.Transcript.Channel
		if channel == nil {
			channel = model.Object{"id": record.ChannelID, "name": record.ChannelName}
		}
		channelID := fallback(model.String(channel["id"]), record.ChannelID)
		next, err := render.TranscriptBounded(exported.Messages, channelID, channel, render.Options{TranscriptID: record.UUID, Roles: exported.Transcript.Roles, ColorOverrides: colors, AssetOrigin: os.Getenv("PUBLIC_BASE_URL")}, maxRenderedHTML)
		if err != nil {
			failures = append(failures, fmt.Errorf("refresh %s: %w", record.UUID, err))
			continue
		}
		temp := htmlPath + ".tmp"
		if err := os.WriteFile(temp, []byte(next), 0o640); err != nil {
			failures = append(failures, fmt.Errorf("refresh %s: %w", record.UUID, err))
			continue
		}
		if err := os.Rename(temp, htmlPath); err != nil {
			_ = os.Remove(temp)
			failures = append(failures, fmt.Errorf("refresh %s: %w", record.UUID, err))
			continue
		}
		_, found, err := registry.Update(record.UUID, func(latest *store.Record) {
			latest.RendererVersion = RendererVersion
			latest.RefreshedAt = time.Now().UTC().Format(time.RFC3339Nano)
		})
		if err != nil || !found {
			failures = append(failures, fmt.Errorf("refresh %s registry: %w", record.UUID, err))
			continue
		}
		refreshed++
	}
	return refreshed, failures
}
