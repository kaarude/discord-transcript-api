package render

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kaarude/discord-transcript-api/internal/model"
)

func message(id, content, name, at string) model.Object {
	return model.Object{"id": id, "type": 0, "content": content, "author": model.Object{"id": id, "username": name}, "timestamp": at, "embeds": []any{}, "attachments": []any{}, "reactions": []any{}}
}

func TestTranscriptRendersDiscordContent(t *testing.T) {
	messages := []model.Object{message("1", "Hello **world** <@2> <#44>", "Alice", "2026-01-01T12:00:00Z"), message("2", "reply", "Bob", "2026-01-01T12:01:00Z")}
	messages[0]["mentions"] = []any{model.Object{"id": "2", "username": "Bob"}}
	messages[1]["type"] = 19
	messages[1]["message_reference"] = model.Object{"message_id": "1"}
	messages[1]["referenced_message"] = messages[0]
	html := Transcript(messages, "123", model.Object{"name": "general", "topic": "Topic"}, Options{TranscriptID: strings.Repeat("a", 32)})
	for _, want := range []string{"<!DOCTYPE html>", "<strong>world</strong>", `class="mention user-mention" data-user-id="2"`, `>#channel</span>`, `href="#message-1"`, `/download/json`, `id="transcript-end"`, `color-scheme: dark`, `http-equiv="Content-Security-Policy"`, `default-src &#39;none&#39;`, `<meta name="referrer" content="no-referrer">`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRichUpstreamParity(t *testing.T) {
	m := message("1", "Hello <@2>", "support-bot", "2024-01-01T12:00:00Z")
	m["author"] = model.Object{"id": "1", "username": "support-bot", "global_name": "Support Bot", "bot": true}
	m["member"] = model.Object{"nick": "Ticket Helper", "roles": []any{"icon-role"}}
	m["mentions"] = []any{model.Object{"id": "2", "username": "customer", "global_name": "Customer"}}
	m["embeds"] = []any{model.Object{"provider": model.Object{"name": "Status Service"}, "author": model.Object{"name": "Incident Bot", "url": "https://example.com/bot", "icon_url": "https://example.com/author.png"}, "title": "Resolved", "url": "https://example.com/incident", "image": model.Object{"url": "https://example.com/image.png"}, "thumbnail": model.Object{"url": "https://example.com/thumb.png"}, "footer": model.Object{"text": "Incident #42", "icon_url": "https://example.com/footer.png"}, "timestamp": "2024-01-01T12:30:00Z"}}
	m["attachments"] = []any{model.Object{"id": "file-1", "filename": "guide.pdf", "url": "https://example.com/guide.pdf", "content_type": "application/pdf", "size": 2048}, model.Object{"id": "thumb-1", "filename": "thumb.png", "url": "https://example.com/thumb.png", "content_type": "image/png"}}
	m["components"] = []any{model.Object{"type": 17, "accent_color": 0xff6600, "components": []any{model.Object{"type": 10, "content": "## Ticket details\nCustomer: <@2>"}, model.Object{"type": 14, "divider": true, "spacing": 2}, model.Object{"type": 9, "components": []any{model.Object{"type": 10, "content": "A helpful section"}}, "accessory": model.Object{"type": 11, "media": model.Object{"url": "attachment://thumb.png"}, "description": "Preview"}}, model.Object{"type": 12, "items": []any{model.Object{"media": model.Object{"url": "https://example.com/gallery.png", "content_type": "image/png"}, "description": "Gallery image"}, model.Object{"media": model.Object{"url": "https://example.com/demo.mp4", "content_type": "video/mp4"}, "description": "Demo video"}}}, model.Object{"type": 13, "file": model.Object{"url": "attachment://guide.pdf"}}, model.Object{"type": 1, "components": []any{model.Object{"type": 2, "style": 5, "label": "Open ticket", "url": "https://example.com/ticket"}}}}}}
	html := Transcript([]model.Object{m}, "123", model.Object{"name": "general"}, Options{Roles: []model.Object{{"id": "icon-role", "name": "Support Lead", "position": 5, "icon": "rolehash", "color": 0xff8800}}})
	for _, want := range []string{`<span class="bot-tag">BOT</span>`, `class="role-icon"`, `role-icons/icon-role/rolehash.png?size=64&amp;quality=lossless`, `id="profile-card" class="profile-card"`, `"roleName":"Support Lead"`, `class="embed-provider">Status Service`, `class="embed-thumbnail"`, `class="embed-media"`, `class="embed-footer-icon"`, `class="component-container" style="--component-accent:#ff6600"`, `component-separator-large`, `class="component-section"`, `media-trigger component-thumbnail`, `component-media-gallery gallery-count-2`, `<video src="https://example.com/demo.mp4" controls`, `2.0 KB`, `href="https://example.com/ticket"`, `openProfile(profileTrigger)`, `openComponentSelect`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing rich parity marker %q", want)
		}
	}
}

func TestTranscriptEscapesHTMLAndProtectsCodeAndLinks(t *testing.T) {
	m := message("1", "<script>alert(1)</script> `ping <@2>` https://example.com/@everyone", "Alice", "2026-01-01T12:00:00Z")
	m["mentions"] = []any{model.Object{"id": "2", "username": "Bob"}}
	html := Transcript([]model.Object{m}, "1", model.Object{"name": "safe"}, Options{})
	if strings.Contains(html, "<script>alert") || !strings.Contains(html, "&lt;script&gt;alert") {
		t.Fatal("raw HTML was not escaped")
	}
	if !strings.Contains(html, `<code class="md-inline-code">ping &lt;@2&gt;</code>`) {
		t.Fatal("inline code was not protected")
	}
	if !strings.Contains(html, `href="https://example.com/@everyone"`) {
		t.Fatal("automatic URL was not protected")
	}
}

func TestTranscriptBoundedRejectsAmplifiedOutput(t *testing.T) {
	m := message("1", strings.Repeat("<>&", 1000), "Alice", "2026-01-01T12:00:00Z")
	if _, err := TranscriptBounded([]model.Object{m}, "1", model.Object{"name": "general"}, Options{}, 1024); err == nil {
		t.Fatal("oversized rendered transcript was accepted")
	}
	if rendered, err := TranscriptBounded([]model.Object{message("1", "hello", "Alice", "2026-01-01T12:00:00Z")}, "1", model.Object{"name": "general"}, Options{}, 1<<20); err != nil || !strings.Contains(rendered, "hello") {
		t.Fatalf("bounded legitimate transcript failed: %v", err)
	}
}

func TestRoleColorAndOrdering(t *testing.T) {
	newer := message("2", "newer", "Alice", "2026-01-02T12:00:00Z")
	older := message("1", "older", "Alice", "2026-01-01T12:00:00Z")
	older["member"] = model.Object{"roles": []any{"role"}}
	html := Transcript([]model.Object{newer, older}, "1", model.Object{"name": "general"}, Options{Roles: []model.Object{{"id": "role", "position": 2, "colors": model.Object{"primary_color": 0x123456}}}})
	if strings.Index(html, "older") > strings.Index(html, "newer") {
		t.Fatal("messages are not chronological")
	}
	if !strings.Contains(html, `style="color:#123456"`) {
		t.Fatal("primary role color missing")
	}
}

func TestEmbeddedScriptIsWellFormed(t *testing.T) {
	// A parse error anywhere in the inline script kills every feature at
	// once (theme toggle, download menu, spoilers, profile cards), so the
	// script must survive basic structural checks.
	script := strings.TrimRight(Script, "\n")
	if strings.Contains(script, `\\/`) {
		t.Error(`script contains double-escaped forward slash "\\/" (regex literal bug)`)
	}
	if !strings.HasPrefix(script, "(function () {") {
		t.Error("script does not open its IIFE")
	}
	if !strings.HasSuffix(script, "})();") {
		t.Error("script does not close its IIFE")
	}
	if strings.Count(script, "{") != strings.Count(script, "}") {
		t.Error("script braces are unbalanced")
	}
}

func BenchmarkTranscript1000(b *testing.B) {
	messages := make([]model.Object, 1000)
	for index := range messages {
		messages[index] = message(
			strconv.Itoa(index+1),
			"message **bold** <@2>",
			"Benchmark User",
			time.Unix(1_700_000_000+int64(index), 0).UTC().Format(time.RFC3339Nano),
		)
		messages[index]["mentions"] = []any{model.Object{"id": "2", "username": "Other User"}}
	}
	b.ResetTimer()
	for range b.N {
		_ = Transcript(messages, "123456789012345678", model.Object{"name": "benchmark"}, Options{})
	}
}
