package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kaarude/discord-transcript-api/internal/model"
)

const (
	DefaultFileLimit  int64 = 25 << 20
	DefaultTotalLimit int64 = 250 << 20
)

var (
	allowedHosts = map[string]bool{"cdn.discordapp.com": true, "media.discordapp.net": true}
	emojiPattern = regexp.MustCompile(`<(?P<animated>a?):(?P<name>[A-Za-z0-9_]+):(?P<id>[0-9]+)>`)
)

type Asset struct {
	URL          string
	FetchURL     string
	Kind         string
	ID           string
	DeclaredSize int64
}

type ManifestAsset struct {
	URL      string `json:"url"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Filename string `json:"filename,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
}

type Manifest struct {
	TranscriptID string          `json:"transcriptId"`
	TotalBytes   int64           `json:"totalBytes"`
	FileLimit    int64           `json:"fileLimit"`
	TotalLimit   int64           `json:"totalLimit"`
	Assets       []ManifestAsset `json:"assets"`
}

type Options struct {
	TranscriptID   string
	AssetsDir      string
	PublicAssetURL string
	GuildID        string
	FileLimit      int64
	TotalLimit     int64
	Timeout        time.Duration
	HTTPClient     *http.Client
	Roles          []model.Object
}

type Result struct {
	Messages []model.Object
	Roles    []model.Object
	Manifest Manifest
}

func AllowedURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && allowedHosts[strings.ToLower(u.Hostname())]
}

func avatarURL(user, member model.Object, guildID string) string {
	id := model.String(user["id"])
	if id != "" && guildID != "" && model.String(member["avatar"]) != "" {
		avatar := model.String(member["avatar"])
		suffix := ".png?size=160"
		if strings.HasPrefix(avatar, "a_") {
			suffix = ".webp?size=160&animated=true"
		}
		return "https://cdn.discordapp.com/guilds/" + guildID + "/users/" + id + "/avatars/" + avatar + suffix
	}
	avatar := model.String(user["avatar"])
	if id == "" || avatar == "" {
		return ""
	}
	suffix := ".png?size=160"
	if strings.HasPrefix(avatar, "a_") {
		suffix = ".webp?size=160&animated=true"
	}
	return "https://cdn.discordapp.com/avatars/" + id + "/" + avatar + suffix
}

func collectEmoji(text string, assets *[]Asset) {
	for _, match := range emojiPattern.FindAllStringSubmatch(text, -1) {
		suffix := ".webp"
		if match[1] != "" {
			suffix += "?animated=true"
		}
		*assets = append(*assets, Asset{URL: "https://cdn.discordapp.com/emojis/" + match[3] + suffix, Kind: "emoji", ID: match[3]})
	}
}

func collectComponents(components []model.Object, assets *[]Asset) {
	for _, component := range components {
		if component == nil {
			continue
		}
		emojis := []model.Object{model.Obj(component["emoji"])}
		for _, option := range model.Objects(component["options"]) {
			emojis = append(emojis, model.Obj(option["emoji"]))
		}
		for _, emoji := range emojis {
			if id := model.String(emoji["id"]); id != "" {
				suffix := ".webp"
				if model.Bool(emoji["animated"]) {
					suffix += "?animated=true"
				}
				*assets = append(*assets, Asset{URL: "https://cdn.discordapp.com/emojis/" + id + suffix, Kind: "emoji", ID: id})
			}
		}
		mediaValues := []any{component["media"], component["file"]}
		for _, item := range model.Objects(component["items"]) {
			mediaValues = append(mediaValues, item["media"])
		}
		for _, value := range mediaValues {
			media := model.Obj(value)
			source := model.String(value)
			proxy := ""
			if media != nil {
				source = model.String(media["url"])
				proxy = model.String(media["proxy_url"])
			}
			if source != "" && !strings.HasPrefix(source, "attachment://") {
				*assets = append(*assets, Asset{URL: source, FetchURL: proxy, Kind: "component"})
			}
		}
		collectComponents(model.Objects(component["components"]), assets)
		if accessory := model.Obj(component["accessory"]); accessory != nil {
			collectComponents([]model.Object{accessory}, assets)
		}
	}
}

func Collect(messages []model.Object, guildID string, roleSets ...[]model.Object) []Asset {
	var assets []Asset
	usedRoles := map[string]bool{}
	for _, message := range messages {
		if avatar := avatarURL(model.Obj(message["author"]), model.Obj(message["member"]), guildID); avatar != "" {
			assets = append(assets, Asset{URL: avatar, Kind: "avatar", ID: model.String(model.Obj(message["author"])["id"])})
		}
		collectEmoji(model.String(message["content"]), &assets)
		collectComponents(model.Objects(message["components"]), &assets)
		for _, role := range model.Slice(model.Obj(message["member"])["roles"]) {
			usedRoles[model.String(role)] = true
		}
		for _, reaction := range model.Objects(message["reactions"]) {
			emoji := model.Obj(reaction["emoji"])
			id := model.String(emoji["id"])
			if id != "" {
				suffix := ".webp"
				if model.Bool(emoji["animated"]) {
					suffix += "?animated=true"
				}
				assets = append(assets, Asset{URL: "https://cdn.discordapp.com/emojis/" + id + suffix, Kind: "emoji", ID: id})
			}
		}
		for _, attachment := range model.Objects(message["attachments"]) {
			assets = append(assets, Asset{URL: model.String(attachment["url"]), FetchURL: model.String(attachment["proxy_url"]), Kind: "attachment", ID: model.String(attachment["id"]), DeclaredSize: int64(model.Int(attachment["size"]))})
		}
		for _, embed := range model.Objects(message["embeds"]) {
			collectEmoji(model.String(embed["description"]), &assets)
			for _, field := range model.Objects(embed["fields"]) {
				collectEmoji(model.String(field["value"]), &assets)
			}
			for _, value := range [][2]string{
				{model.String(model.Obj(embed["image"])["url"]), model.String(model.Obj(embed["image"])["proxy_url"])},
				{model.String(model.Obj(embed["thumbnail"])["url"]), model.String(model.Obj(embed["thumbnail"])["proxy_url"])},
				{model.String(model.Obj(embed["author"])["icon_url"]), model.String(model.Obj(embed["author"])["proxy_icon_url"])},
				{model.String(model.Obj(embed["footer"])["icon_url"]), model.String(model.Obj(embed["footer"])["proxy_icon_url"])},
			} {
				if value[0] != "" {
					assets = append(assets, Asset{URL: value[0], FetchURL: value[1], Kind: "embed"})
				}
			}
		}
		ref := model.Obj(message["referenced_message"])
		for _, role := range model.Slice(model.Obj(ref["member"])["roles"]) {
			usedRoles[model.String(role)] = true
		}
		if avatar := avatarURL(model.Obj(ref["author"]), model.Obj(ref["member"]), guildID); avatar != "" {
			assets = append(assets, Asset{URL: avatar, Kind: "avatar", ID: model.String(model.Obj(ref["author"])["id"])})
		}
	}
	if len(roleSets) > 0 {
		for _, role := range roleSets[0] {
			id, icon := model.String(role["id"]), model.String(role["icon"])
			if usedRoles[id] && id != "" && icon != "" {
				assets = append(assets, Asset{URL: "https://cdn.discordapp.com/role-icons/" + id + "/" + icon + ".png?size=64&quality=lossless", Kind: "roleIcon", ID: id})
			}
		}
	}
	return assets
}

func Cache(ctx context.Context, messages []model.Object, options Options) (Result, error) {
	if options.FileLimit <= 0 {
		options.FileLimit = DefaultFileLimit
	}
	if options.TotalLimit <= 0 {
		options.TotalLimit = DefaultTotalLimit
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Second
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{}
	}
	if err := os.MkdirAll(options.AssetsDir, 0o750); err != nil {
		return Result{}, err
	}
	cloned, err := model.CloneObjects(messages)
	if err != nil {
		return Result{}, err
	}
	collected := Collect(messages, options.GuildID, options.Roles)
	unique := make([]Asset, 0, len(collected))
	seen := map[string]bool{}
	for _, asset := range collected {
		if asset.URL != "" && !seen[asset.URL] {
			seen[asset.URL] = true
			unique = append(unique, asset)
		}
	}

	manifest := Manifest{TranscriptID: options.TranscriptID, FileLimit: options.FileLimit, TotalLimit: options.TotalLimit, Assets: make([]ManifestAsset, 0)}
	urlMap := map[string]string{}
	blocked := map[string]bool{}
	var mu sync.Mutex
	var reserved int64
	jobs := make(chan Asset)
	var wg sync.WaitGroup
	for range min(4, len(unique)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for asset := range jobs {
				entry := ManifestAsset{URL: asset.URL, Kind: asset.Kind}
				source := asset.FetchURL
				if source == "" {
					source = asset.URL
				}
				if !AllowedURL(source) {
					entry.Status, entry.Reason = "skipped", "unsupported_host"
					mu.Lock()
					blocked[asset.URL] = true
					manifest.Assets = append(manifest.Assets, entry)
					mu.Unlock()
					continue
				}
				if asset.DeclaredSize > options.FileLimit {
					entry.Status, entry.Reason = "skipped", "file_too_large"
					mu.Lock()
					manifest.Assets = append(manifest.Assets, entry)
					mu.Unlock()
					continue
				}
				reservation := asset.DeclaredSize
				if reservation <= 0 {
					reservation = options.FileLimit
					if asset.Kind == "avatar" || asset.Kind == "emoji" {
						reservation = min(int64(5<<20), options.FileLimit)
					}
				}
				mu.Lock()
				if manifest.TotalBytes+reserved+reservation > options.TotalLimit {
					entry.Status, entry.Reason = "skipped", "transcript_limit"
					manifest.Assets = append(manifest.Assets, entry)
					mu.Unlock()
					continue
				}
				reserved += reservation
				mu.Unlock()

				filename, bytes, downloadErr := download(ctx, source, options)
				mu.Lock()
				reserved -= reservation
				if downloadErr != nil {
					entry.Status, entry.Reason = "failed", reason(downloadErr)
				} else if manifest.TotalBytes+bytes > options.TotalLimit {
					_ = os.Remove(filepath.Join(options.AssetsDir, filename))
					entry.Status, entry.Reason = "skipped", "transcript_limit"
				} else {
					manifest.TotalBytes += bytes
					entry.Status, entry.Filename, entry.Bytes = "cached", filename, bytes
					urlMap[asset.URL] = strings.TrimSuffix(options.PublicAssetURL, "/") + "/" + filename
				}
				manifest.Assets = append(manifest.Assets, entry)
				mu.Unlock()
			}
		}()
	}
	for _, asset := range unique {
		jobs <- asset
	}
	close(jobs)
	wg.Wait()
	rewrite(cloned, messages, urlMap, blocked, options.GuildID, collected)
	roles, _ := model.CloneObjects(options.Roles)
	for _, role := range roles {
		id, icon := model.String(role["id"]), model.String(role["icon"])
		if id != "" && icon != "" {
			source := "https://cdn.discordapp.com/role-icons/" + id + "/" + icon + ".png?size=64&quality=lossless"
			if cached := urlMap[source]; cached != "" {
				role["cached_icon_url"] = cached
			}
		}
	}
	return Result{Messages: cloned, Roles: roles, Manifest: manifest}, nil
}

func download(parent context.Context, source string, options Options) (string, int64, error) {
	ctx, cancel := context.WithTimeout(parent, options.Timeout)
	defer cancel()
	client := *options.HTTPClient
	redirects := 0
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		redirects++
		if redirects > 3 {
			return fmt.Errorf("unsafe_redirect")
		}
		if !AllowedURL(req.URL.String()) {
			return fmt.Errorf("unsupported_host")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("http_%d", resp.StatusCode)
	}
	if resp.ContentLength > options.FileLimit {
		return "", 0, fmt.Errorf("file_too_large")
	}
	hash := sha256.Sum256([]byte(source))
	ext := extension(source, resp.Header.Get("Content-Type"))
	filename := hex.EncodeToString(hash[:]) + ext
	temp := filepath.Join(options.AssetsDir, "."+filename+".download")
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return "", 0, err
	}
	written, copyErr := io.Copy(file, io.LimitReader(resp.Body, options.FileLimit+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written > options.FileLimit {
		_ = os.Remove(temp)
		if written > options.FileLimit {
			return "", 0, fmt.Errorf("file_too_large")
		}
		if copyErr != nil {
			return "", 0, copyErr
		}
		return "", 0, closeErr
	}
	if err := os.Rename(temp, filepath.Join(options.AssetsDir, filename)); err != nil {
		_ = os.Remove(temp)
		return "", 0, err
	}
	return filename, written, nil
}

func extension(source, contentType string) string {
	types := map[string]string{
		"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp", "image/avif": ".avif",
		"video/mp4": ".mp4", "video/webm": ".webm", "audio/ogg": ".ogg", "audio/mpeg": ".mp3", "audio/mp4": ".m4a",
		"application/pdf": ".pdf", "text/plain": ".txt",
	}
	if ext := types[strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))]; ext != "" {
		return ext
	}
	if parsed, err := url.Parse(source); err == nil {
		ext := strings.ToLower(filepath.Ext(parsed.Path))
		if matched, _ := regexp.MatchString(`^\.[a-z0-9]{1,8}$`, ext); matched {
			return ext
		}
	}
	return ".bin"
}

func reason(err error) string {
	if err == nil {
		return ""
	}
	if err == context.DeadlineExceeded || strings.Contains(err.Error(), "context deadline exceeded") {
		return "timeout"
	}
	for _, known := range []string{"unsupported_host", "unsafe_redirect", "file_too_large"} {
		if strings.Contains(err.Error(), known) {
			return known
		}
	}
	return err.Error()
}

func rewriteComponents(components []model.Object, urlMap map[string]string, blocked map[string]bool) {
	for _, component := range components {
		if component == nil {
			continue
		}
		for _, property := range []string{"media", "file"} {
			media := model.Obj(component[property])
			if media != nil {
				source := model.String(media["url"])
				if cached := urlMap[source]; cached != "" {
					media["url"] = cached
				} else if blocked[source] {
					media["url"] = ""
				}
			}
		}
		for _, item := range model.Objects(component["items"]) {
			media := model.Obj(item["media"])
			source := model.String(media["url"])
			if cached := urlMap[source]; cached != "" {
				media["url"] = cached
			} else if blocked[source] {
				media["url"] = ""
			}
		}
		rewriteComponents(model.Objects(component["components"]), urlMap, blocked)
		if accessory := model.Obj(component["accessory"]); accessory != nil {
			rewriteComponents([]model.Object{accessory}, urlMap, blocked)
		}
	}
}

func rewrite(cloned, original []model.Object, urlMap map[string]string, blocked map[string]bool, guildID string, collected []Asset) {
	cachedEmoji := model.Object{}
	for _, asset := range collected {
		if asset.Kind == "emoji" && urlMap[asset.URL] != "" {
			cachedEmoji[asset.ID] = urlMap[asset.URL]
		}
	}
	for index, message := range cloned {
		originalMessage := original[index]
		avatar := avatarURL(model.Obj(originalMessage["author"]), model.Obj(originalMessage["member"]), guildID)
		if cached := urlMap[avatar]; cached != "" {
			model.Obj(message["author"])["cached_avatar_url"] = cached
		}
		for _, attachment := range model.Objects(message["attachments"]) {
			source := model.String(attachment["url"])
			if cached := urlMap[source]; cached != "" {
				attachment["url"] = cached
			} else if blocked[source] {
				attachment["url"] = ""
			}
		}
		for _, reaction := range model.Objects(message["reactions"]) {
			emoji := model.Obj(reaction["emoji"])
			id := model.String(emoji["id"])
			if id == "" {
				continue
			}
			suffix := ".webp"
			if model.Bool(emoji["animated"]) {
				suffix += "?animated=true"
			}
			if cached := urlMap["https://cdn.discordapp.com/emojis/"+id+suffix]; cached != "" {
				emoji["cached_url"] = cached
			}
		}
		for _, embed := range model.Objects(message["embeds"]) {
			for _, pair := range [][2]string{{"image", "url"}, {"thumbnail", "url"}, {"author", "icon_url"}, {"footer", "icon_url"}} {
				child := model.Obj(embed[pair[0]])
				source := model.String(child[pair[1]])
				if cached := urlMap[source]; cached != "" {
					child[pair[1]] = cached
				} else if blocked[source] {
					child[pair[1]] = ""
				}
			}
		}
		rewriteComponents(model.Objects(message["components"]), urlMap, blocked)
		ref := model.Obj(message["referenced_message"])
		originalRef := model.Obj(originalMessage["referenced_message"])
		avatar = avatarURL(model.Obj(originalRef["author"]), model.Obj(originalRef["member"]), guildID)
		if cached := urlMap[avatar]; cached != "" {
			model.Obj(ref["author"])["cached_avatar_url"] = cached
		}
		message["cached_emojis"] = cachedEmoji
	}
}
