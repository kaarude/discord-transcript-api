package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/kaarude/discord-transcript-api/internal/model"
)

const (
	apiBase                            = "https://discord.com/api/v10"
	MaxDiscordResponseBytes      int64 = 32 << 20
	MaxTranscriptResponseBytes   int64 = 32 << 20
	MaxMemberLookupResponseBytes int64 = 32 << 20
)

type APIError struct {
	Status  int
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("Discord API returned HTTP %d", e.Status)
}

type Client struct {
	http *http.Client
}

func New() *Client {
	return &Client{http: &http.Client{Timeout: 45 * time.Second}}
}

func (c *Client) get(ctx context.Context, token, path string, target any) (int64, error) {
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bot "+token)
		req.Header.Set("User-Agent", "DiscordTranscriptAPI/1.0 (+https://github.com/kaarude/discord-transcript-api)")
		resp, err := c.http.Do(req)
		if err != nil {
			return 0, err
		}
		if resp.ContentLength > MaxDiscordResponseBytes {
			_ = resp.Body.Close()
			return 0, fmt.Errorf("Discord response exceeds %d bytes", MaxDiscordResponseBytes)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxDiscordResponseBytes+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return 0, readErr
		}
		if int64(len(body)) > MaxDiscordResponseBytes {
			return 0, fmt.Errorf("Discord response exceeds %d bytes", MaxDiscordResponseBytes)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			var rate struct {
				RetryAfter float64 `json:"retry_after"`
			}
			_ = json.Unmarshal(body, &rate)
			delay := time.Duration(rate.RetryAfter * float64(time.Second))
			if delay <= 0 {
				delay = time.Second
			}
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return 0, ctx.Err()
			case <-timer.C:
				continue
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			var payload struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(body, &payload)
			return 0, &APIError{Status: resp.StatusCode, Code: payload.Code, Message: payload.Message}
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(target); err != nil {
			return 0, err
		}
		return int64(len(body)), nil
	}
	return 0, fmt.Errorf("Discord request retry limit reached")
}

func (c *Client) Messages(ctx context.Context, token, channelID string, limit int) ([]model.Object, error) {
	if limit < 1 {
		limit = 1000
	}
	all := make([]model.Object, 0, limit)
	before := ""
	var received int64
	for len(all) < limit {
		batchSize := min(100, limit-len(all))
		query := url.Values{"limit": {strconv.Itoa(batchSize)}}
		if before != "" {
			query.Set("before", before)
		}
		var batch []model.Object
		path := "/channels/" + url.PathEscape(channelID) + "/messages?" + query.Encode()
		bytes, err := c.get(ctx, token, path, &batch)
		if err != nil {
			return nil, err
		}
		received += bytes
		if received > MaxTranscriptResponseBytes {
			return nil, fmt.Errorf("Discord transcript response exceeds %d bytes", MaxTranscriptResponseBytes)
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		before = model.String(batch[len(batch)-1]["id"])
		if len(batch) < batchSize {
			break
		}
	}
	return all, nil
}

func (c *Client) Channel(ctx context.Context, token, channelID string) (model.Object, error) {
	var channel model.Object
	_, err := c.get(ctx, token, "/channels/"+url.PathEscape(channelID), &channel)
	return channel, err
}

func (c *Client) Roles(ctx context.Context, token, guildID string) ([]model.Object, error) {
	var roles []model.Object
	_, err := c.get(ctx, token, "/guilds/"+url.PathEscape(guildID)+"/roles", &roles)
	return roles, err
}

func (c *Client) Members(ctx context.Context, token, guildID string, userIDs []string) []model.Object {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	unique := make([]string, 0, len(userIDs))
	seen := map[string]bool{}
	for _, id := range userIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	jobs := make(chan string)
	var result []model.Object
	var received int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	workers := min(6, len(unique))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				var member model.Object
				path := "/guilds/" + url.PathEscape(guildID) + "/members/" + url.PathEscape(id)
				if bytes, err := c.get(ctx, token, path, &member); err == nil {
					mu.Lock()
					if received+bytes <= MaxMemberLookupResponseBytes {
						received += bytes
						result = append(result, member)
					} else {
						cancel()
					}
					mu.Unlock()
				}
			}
		}()
	}
sendJobs:
	for _, id := range unique {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break sendJobs
		}
	}
	close(jobs)
	wg.Wait()
	return result
}

func AuthorIDs(messages []model.Object) []string {
	seen := map[string]bool{}
	var ids []string
	remember := func(message model.Object) {
		if message == nil || model.String(message["webhook_id"]) != "" {
			return
		}
		id := model.String(model.Obj(message["author"])["id"])
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, message := range messages {
		remember(message)
		remember(model.Obj(message["referenced_message"]))
	}
	return ids
}

func AttachMembers(messages, members []model.Object) []model.Object {
	byID := map[string]model.Object{}
	for _, member := range members {
		if id := model.String(model.Obj(member["user"])["id"]); id != "" {
			byID[id] = member
		}
	}
	attach := func(message model.Object) {
		if message == nil {
			return
		}
		id := model.String(model.Obj(message["author"])["id"])
		member := byID[id]
		if member == nil {
			return
		}
		merged := model.Object{}
		for key, value := range model.Obj(message["member"]) {
			merged[key] = value
		}
		for key, value := range member {
			merged[key] = value
		}
		message["member"] = merged
	}
	for _, message := range messages {
		attach(message)
		attach(model.Obj(message["referenced_message"]))
	}
	return messages
}
