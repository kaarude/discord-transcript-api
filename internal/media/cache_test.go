package media

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kaarude/discord-transcript-api/internal/model"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (fn roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestAllowedURL(t *testing.T) {
	for value, want := range map[string]bool{"https://cdn.discordapp.com/a.png": true, "https://media.discordapp.net/a.png": true, "http://cdn.discordapp.com/a": false, "https://cdn.discordapp.com.attacker.test/a": false} {
		if got := AllowedURL(value); got != want {
			t.Errorf("AllowedURL(%q)=%v", value, got)
		}
	}
}

func TestCacheStreamsAndRewritesAttachment(t *testing.T) {
	source := "https://cdn.discordapp.com/attachments/1/2/image.png?x=1"
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}, "Content-Length": {"11"}}, Body: io.NopCloser(strings.NewReader("image-bytes")), Request: req}, nil
	})}
	messages := []model.Object{{"author": model.Object{"id": "1"}, "attachments": []any{model.Object{"id": "2", "url": source, "size": 11}}, "reactions": []any{}, "embeds": []any{}}}
	result, err := Cache(context.Background(), messages, Options{TranscriptID: "abc", AssetsDir: t.TempDir(), PublicAssetURL: "https://api.test/transcripts/abc/assets", HTTPClient: client, FileLimit: 100, TotalLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := model.String(model.Objects(result.Messages[0]["attachments"])[0]["url"])
	if !strings.HasPrefix(got, "https://api.test/transcripts/abc/assets/") || result.Manifest.TotalBytes != 11 {
		t.Fatalf("unexpected cache result: %s %#v", got, result.Manifest)
	}
}

func TestCacheEnforcesDeclaredLimit(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) { called = true; return nil, nil })}
	messages := []model.Object{{"author": model.Object{"id": "1"}, "attachments": []any{model.Object{"url": "https://cdn.discordapp.com/large.bin", "size": 101}}}}
	result, err := Cache(context.Background(), messages, Options{TranscriptID: "abc", AssetsDir: t.TempDir(), PublicAssetURL: "/assets", HTTPClient: client, FileLimit: 100, TotalLimit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if called || result.Manifest.Assets[0].Reason != "file_too_large" {
		t.Fatal("oversized file was not skipped")
	}
}

func TestCollectsNestedComponentsAndOnlyUsedRoleIcons(t *testing.T) {
	messages := []model.Object{{
		"author": model.Object{"id": "1"},
		"member": model.Object{"roles": []any{"used"}},
		"components": []any{model.Object{"type": 17, "components": []any{
			model.Object{"type": 1, "components": []any{model.Object{"type": 2, "emoji": model.Object{"id": "123", "name": "ok"}}}},
			model.Object{"type": 12, "items": []any{model.Object{"media": model.Object{"url": "https://cdn.discordapp.com/attachments/gallery.png"}}}},
		}}},
	}}
	roles := []model.Object{{"id": "used", "icon": "yes"}, {"id": "unused", "icon": "no"}}
	assets := Collect(messages, "guild", roles)
	joined := ""
	for _, asset := range assets {
		joined += asset.URL + "\n"
	}
	for _, want := range []string{"/emojis/123.webp", "attachments/gallery.png", "role-icons/used/yes.png"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(joined, "role-icons/unused/") {
		t.Fatal("unused role icon was collected")
	}
}

func TestCacheRewritesNestedComponentsRolesAndSharesEmojiLookup(t *testing.T) {
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(strings.NewReader("x")), Request: req}, nil
	})}
	componentURL := "https://cdn.discordapp.com/attachments/demo.png"
	messages := []model.Object{
		{
			"author": model.Object{"id": "1"}, "member": model.Object{"roles": []any{"role"}}, "content": "<:ok:123>",
			"components": []any{model.Object{"type": 17, "components": []any{model.Object{"type": 11, "media": model.Object{"url": componentURL}}}}},
		},
		{"author": model.Object{"id": "2"}, "content": "second"},
	}
	roles := []model.Object{{"id": "role", "icon": "hash"}}
	result, err := Cache(context.Background(), messages, Options{TranscriptID: "abc", AssetsDir: t.TempDir(), PublicAssetURL: "/assets", HTTPClient: client, FileLimit: 100, TotalLimit: 1000, Roles: roles})
	if err != nil {
		t.Fatal(err)
	}
	media := model.Obj(model.Objects(model.Objects(result.Messages[0]["components"])[0]["components"])[0]["media"])
	if !strings.HasPrefix(model.String(media["url"]), "/assets/") {
		t.Fatalf("component not rewritten: %#v", media)
	}
	if !strings.HasPrefix(model.String(result.Roles[0]["cached_icon_url"]), "/assets/") {
		t.Fatalf("role not rewritten: %#v", result.Roles)
	}
	first, second := model.Obj(result.Messages[0]["cached_emojis"]), model.Obj(result.Messages[1]["cached_emojis"])
	first["shared-check"] = "yes"
	if model.String(second["shared-check"]) != "yes" {
		t.Fatal("emoji lookup was duplicated per message")
	}
}

func TestUnsupportedEmbedMediaIsRemoved(t *testing.T) {
	source := "https://tracker.example/pixel.png"
	messages := []model.Object{{"author": model.Object{"id": "1"}, "embeds": []any{model.Object{"image": model.Object{"url": source}}}}}
	result, err := Cache(context.Background(), messages, Options{TranscriptID: "abc", AssetsDir: t.TempDir(), PublicAssetURL: "/assets", FileLimit: 100, TotalLimit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	image := model.Obj(model.Objects(result.Messages[0]["embeds"])[0]["image"])
	if model.String(image["url"]) != "" || len(result.Manifest.Assets) != 1 || result.Manifest.Assets[0].Reason != "unsupported_host" {
		t.Fatalf("external embed was retained: %#v %#v", image, result.Manifest)
	}
}

func TestDiscordProxyCachesAndRewritesExternalEmbedURL(t *testing.T) {
	original := "https://images.example/photo.png"
	proxy := "https://media.discordapp.net/external/photo.png"
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != proxy {
			t.Fatalf("fetched %q, want Discord proxy", req.URL.String())
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(strings.NewReader("image")), Request: req}, nil
	})}
	messages := []model.Object{{"author": model.Object{"id": "1"}, "embeds": []any{model.Object{"image": model.Object{"url": original, "proxy_url": proxy}}}}}
	result, err := Cache(context.Background(), messages, Options{TranscriptID: "abc", AssetsDir: t.TempDir(), PublicAssetURL: "/assets", HTTPClient: client, FileLimit: 100, TotalLimit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	image := model.Obj(model.Objects(result.Messages[0]["embeds"])[0]["image"])
	if !strings.HasPrefix(model.String(image["url"]), "/assets/") || result.Manifest.Assets[0].Status != "cached" {
		t.Fatalf("proxied embed was not cached: %#v %#v", image, result.Manifest)
	}
}
