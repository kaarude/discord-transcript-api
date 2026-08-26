package render

import (
	"encoding/json"
	"fmt"
	"html"
	"math/big"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kaarude/discord-transcript-api/internal/model"
)

type markupRule struct {
	re          *regexp.Regexp
	replacement string
}

var (
	regularTypes   = map[int]bool{0: true, 19: true, 20: true, 23: true}
	systemTypes    = map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true, 11: true, 12: true, 14: true, 15: true, 16: true, 17: true, 18: true, 22: true, 24: true, 25: true, 26: true, 27: true, 28: true, 29: true, 31: true, 32: true, 36: true, 37: true, 38: true, 39: true, 44: true, 46: true}
	channelMention = regexp.MustCompile(`&lt;#[0-9]+&gt;`)
	userMention    = regexp.MustCompile(`&lt;@!?([0-9]+)&gt;`)
	roleMention    = regexp.MustCompile(`&lt;@&amp;([0-9]+)&gt;`)
	customEmoji    = regexp.MustCompile(`&lt;(a?):([A-Za-z0-9_]+):([0-9]+)&gt;`)
	codeBlock      = regexp.MustCompile("(?s)```(?:([A-Za-z0-9_+.-]+)\\n)?(.*?)```")
	maskedLink     = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	inlineCode     = regexp.MustCompile("`([^`\\n]+)`")
	autoLink       = regexp.MustCompile(`https?://[^\s<\x00]+`)
	roleColorValue = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	markdownRules  = []markupRule{
		{regexp.MustCompile(`__\*\*\*([^\n]+?)\*\*\*__`), `<u><strong><em>$1</em></strong></u>`},
		{regexp.MustCompile(`__\*\*([^\n]+?)\*\*__`), `<u><strong>$1</strong></u>`},
		{regexp.MustCompile(`__\*([^\n]+?)\*__`), `<u><em>$1</em></u>`},
		{regexp.MustCompile(`\*\*\*([^\n]+?)\*\*\*`), `<strong><em>$1</em></strong>`},
		{regexp.MustCompile(`\*\*([^*\n]+?)\*\*`), `<strong>$1</strong>`},
		{regexp.MustCompile(`__([^_\n]+?)__`), `<u>$1</u>`},
		{regexp.MustCompile(`~~([^~\n]+?)~~`), `<del>$1</del>`},
		{regexp.MustCompile(`\|\|([^|\n]+?)\|\|`), `<span class="md-spoiler" tabindex="0" role="button" aria-label="Spoiler">$1</span>`},
		{regexp.MustCompile(`\*([^*\n]+?)\*`), `<em>$1</em>`},
		{regexp.MustCompile(`_([^_\n]+?)_`), `<em>$1</em>`},
	}
)

type Options struct {
	TranscriptID   string
	Roles          []model.Object
	ColorOverrides map[string]string
	AssetOrigin    string
}

// ContentSecurityPolicy confines generated transcripts to their own cached
// assets and Discord's media CDN. AssetOrigin keeps downloaded HTML exports
// able to load assets from the configured public origin.
func ContentSecurityPolicy(assetOrigin string) string {
	sources := []string{"'self'", "data:", "https://cdn.discordapp.com", "https://media.discordapp.net"}
	if parsed, err := url.Parse(assetOrigin); err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" {
		sources = append(sources, parsed.Scheme+"://"+parsed.Host)
	}
	media := strings.Join(sources, " ")
	return "default-src 'none'; img-src " + media + "; media-src " + media + "; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'none'; font-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'"
}

func esc(value any) string { return html.EscapeString(model.String(value)) }

func token(tokens *[]string, markup string) string {
	placeholder := fmt.Sprintf("\x00TOKEN%d\x00", len(*tokens))
	*tokens = append(*tokens, markup)
	return placeholder
}

func restore(value string, tokens []string) string {
	for index, markup := range tokens {
		value = strings.ReplaceAll(value, fmt.Sprintf("\x00TOKEN%d\x00", index), markup)
	}
	return value
}

func inlineMarkdown(value string, message model.Object, tokens *[]string) string {
	value = maskedLink.ReplaceAllStringFunc(value, func(match string) string {
		parts := maskedLink.FindStringSubmatch(match)
		return token(tokens, `<a href="`+parts[2]+`" target="_blank" rel="noopener noreferrer">`+parts[1]+`</a>`)
	})
	value = inlineCode.ReplaceAllStringFunc(value, func(match string) string {
		parts := inlineCode.FindStringSubmatch(match)
		return token(tokens, `<code class="md-inline-code">`+parts[1]+`</code>`)
	})
	value = autoLink.ReplaceAllStringFunc(value, func(match string) string {
		clean := strings.TrimRight(match, "),.!?;:")
		trailing := strings.TrimPrefix(match, clean)
		return token(tokens, `<a href="`+clean+`" target="_blank" rel="noopener noreferrer">`+clean+`</a>`) + trailing
	})
	value = enrichInline(value, message, tokens)
	for _, pattern := range markdownRules {
		value = pattern.re.ReplaceAllString(value, pattern.replacement)
	}
	return value
}

func FormatContent(content string, message model.Object) string {
	if content == "" {
		return ""
	}
	value := html.EscapeString(strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n"))
	var blocks []string
	value = codeBlock.ReplaceAllStringFunc(value, func(match string) string {
		parts := codeBlock.FindStringSubmatch(match)
		language := ""
		if parts[1] != "" {
			language = `<div class="md-code-language">` + parts[1] + `</div>`
		}
		code := strings.Trim(parts[2], "\n")
		placeholder := fmt.Sprintf("\x00CODEBLOCK%d\x00", len(blocks))
		blocks = append(blocks, `<div class="md-code-wrap">`+language+`<pre class="md-code-block"><code>`+code+`</code></pre></div>`)
		return placeholder
	})
	var tokens []string
	lines := strings.Split(value, "\n")
	var output []string
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		if strings.HasPrefix(line, "\x00CODEBLOCK") && strings.HasSuffix(line, "\x00") {
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, "\x00CODEBLOCK"), "\x00"))
			if n >= 0 && n < len(blocks) {
				output = append(output, blocks[n])
			}
			continue
		}
		if strings.HasPrefix(line, "&gt;&gt;&gt;") {
			quoted := []string{strings.TrimSpace(strings.TrimPrefix(line, "&gt;&gt;&gt;"))}
			quoted = append(quoted, lines[index+1:]...)
			for i := range quoted {
				quoted[i] = inlineMarkdown(quoted[i], message, &tokens)
			}
			output = append(output, `<blockquote class="md-quote">`+strings.Join(quoted, "<br>")+`</blockquote>`)
			break
		}
		if strings.HasPrefix(line, "&gt;") {
			var quoted []string
			for index < len(lines) && strings.HasPrefix(lines[index], "&gt;") {
				quoted = append(quoted, inlineMarkdown(strings.TrimSpace(strings.TrimPrefix(lines[index], "&gt;")), message, &tokens))
				index++
			}
			index--
			output = append(output, `<blockquote class="md-quote">`+strings.Join(quoted, "<br>")+`</blockquote>`)
			continue
		}
		headingLevel := 0
		for headingLevel < 3 && headingLevel < len(line) && line[headingLevel] == '#' {
			headingLevel++
		}
		if headingLevel > 0 && len(line) > headingLevel && line[headingLevel] == ' ' {
			tag := strconv.Itoa(headingLevel)
			output = append(output, `<h`+tag+` class="md-heading md-heading-`+tag+`">`+inlineMarkdown(line[headingLevel+1:], message, &tokens)+`</h`+tag+`>`)
			continue
		}
		if strings.HasPrefix(line, "-# ") {
			output = append(output, `<div class="md-subtext">`+inlineMarkdown(strings.TrimPrefix(line, "-# "), message, &tokens)+`</div>`)
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(line, " "), "- ") || strings.HasPrefix(strings.TrimLeft(line, " "), "* ") {
			var items []string
			for index < len(lines) {
				trimmed := strings.TrimLeft(lines[index], " ")
				if !strings.HasPrefix(trimmed, "- ") && !strings.HasPrefix(trimmed, "* ") {
					break
				}
				items = append(items, `<li>`+inlineMarkdown(trimmed[2:], message, &tokens)+`</li>`)
				index++
			}
			index--
			output = append(output, `<ul class="md-list">`+strings.Join(items, "")+`</ul>`)
			continue
		}
		if line == "" {
			output = append(output, `<div class="md-line md-empty"><br></div>`)
		} else {
			output = append(output, `<div class="md-line">`+inlineMarkdown(line, message, &tokens)+`</div>`)
		}
	}
	return restore(strings.Join(output, ""), tokens)
}

func enrichInline(value string, message model.Object, tokens *[]string) string {
	for _, user := range model.Objects(message["mentions"]) {
		id := model.String(user["id"])
		if id == "" {
			continue
		}
		name := model.String(user["global_name"])
		if name == "" {
			name = model.String(user["username"])
		}
		if name == "" {
			name = "user"
		}
		markup := `<span class="mention user-mention" data-user-id="` + html.EscapeString(id) + `" role="button" tabindex="0" aria-expanded="false" aria-controls="profile-card">@` + html.EscapeString(name) + `</span>`
		for _, mention := range []string{"&lt;@" + id + "&gt;", "&lt;@!" + id + "&gt;"} {
			if strings.Contains(value, mention) {
				value = strings.ReplaceAll(value, mention, token(tokens, markup))
			}
		}
	}
	participants := model.Obj(message["participant_names"])
	value = userMention.ReplaceAllStringFunc(value, func(match string) string {
		parts := userMention.FindStringSubmatch(match)
		name := model.String(participants[parts[1]])
		if name == "" {
			return token(tokens, `<span class="mention">@user</span>`)
		}
		return token(tokens, `<span class="mention user-mention" data-user-id="`+html.EscapeString(parts[1])+`" role="button" tabindex="0" aria-expanded="false" aria-controls="profile-card">@`+html.EscapeString(name)+`</span>`)
	})
	channels := model.Obj(message["channel_names"])
	value = channelMention.ReplaceAllStringFunc(value, func(match string) string {
		id := strings.TrimSuffix(strings.TrimPrefix(match, "&lt;#"), "&gt;")
		name := model.String(channels[id])
		if name == "" {
			name = "channel"
		}
		return token(tokens, `<span class="mention">#`+html.EscapeString(name)+`</span>`)
	})
	value = roleMention.ReplaceAllStringFunc(value, func(string) string { return token(tokens, `<span class="mention">@role</span>`) })
	value = customEmoji.ReplaceAllStringFunc(value, func(match string) string {
		parts := customEmoji.FindStringSubmatch(match)
		suffix := ".webp"
		if parts[1] != "" {
			suffix += "?animated=true"
		}
		source := model.String(model.Obj(message["cached_emojis"])[parts[3]])
		if source == "" {
			source = "https://cdn.discordapp.com/emojis/" + parts[3] + suffix
		}
		name := html.EscapeString(parts[2])
		return token(tokens, `<img class="reaction-emoji" src="`+html.EscapeString(source)+`" alt=":`+name+`:" title=":`+name+`:" onerror="this.replaceWith(this.alt)">`)
	})
	for _, mention := range []string{"@everyone", "@here"} {
		if strings.Contains(value, mention) {
			value = strings.ReplaceAll(value, mention, token(tokens, `<span class="mention">`+mention+`</span>`))
		}
	}
	return value
}

func userName(message model.Object) string { return model.DisplayName(message) }

func avatarURL(user model.Object) string {
	if cached := model.String(user["cached_avatar_url"]); cached != "" {
		return cached
	}
	id, avatar := model.String(user["id"]), model.String(user["avatar"])
	if id != "" && avatar != "" {
		suffix := ".png?size=80"
		if strings.HasPrefix(avatar, "a_") {
			suffix = ".webp?size=80&animated=true"
		}
		return "https://cdn.discordapp.com/avatars/" + id + "/" + avatar + suffix
	}
	index := int64(0)
	if discriminator := model.String(user["discriminator"]); discriminator != "" && discriminator != "0" {
		n, _ := strconv.ParseInt(discriminator, 10, 64)
		index = n % 5
	} else if id != "" {
		n := new(big.Int)
		if _, ok := n.SetString(id, 10); ok {
			n.Rsh(n, 22)
			index = new(big.Int).Mod(n, big.NewInt(6)).Int64()
		}
	}
	return fmt.Sprintf("https://cdn.discordapp.com/embed/avatars/%d.png", index)
}

func avatarMarkup(message model.Object, small bool) string {
	user := model.Obj(message["author"])
	shell, image := "avatar-shell", "avatar"
	if small {
		shell, image = "reply-avatar-shell", "reply-avatar"
	}
	fallback := avatarURL(model.Object{"id": user["id"], "discriminator": user["discriminator"]})
	return `<span class="` + shell + `" aria-hidden="true"><img class="` + image + `" src="` + html.EscapeString(avatarURL(user)) + `" alt="" loading="lazy" onerror="this.onerror=null;this.src='` + html.EscapeString(fallback) + `'"></span>`
}

func parseTime(value any) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, model.String(value))
	return t
}
func fullTimestamp(value any) string {
	t := parseTime(value)
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("1/2/2006, 3:04 PM")
}
func shortTimestamp(value any) string {
	t := parseTime(value)
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("3:04 PM")
}
func dateKey(value any) string {
	t := parseTime(value)
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-1-2")
}
func dateLabel(value any) string {
	t := parseTime(value)
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("January 2, 2006")
}

func safeURL(value any) string {
	parsed, err := url.Parse(model.String(value))
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return html.EscapeString(parsed.String())
}

func formatBytes(value any) string {
	bytes := float64(model.Int(value))
	if bytes < 0 {
		return ""
	}
	if bytes < 1024 {
		return fmt.Sprintf("%.0f B", bytes)
	}
	units := []string{"KB", "MB", "GB"}
	size, unit := bytes/1024, 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	if size >= 10 {
		return fmt.Sprintf("%.0f %s", size, units[unit])
	}
	return fmt.Sprintf("%.1f %s", size, units[unit])
}

func embedHTML(embed, message model.Object) string {
	color := "#80848e"
	if n := model.Int(embed["color"]); n > 0 {
		color = fmt.Sprintf("#%06x", n)
	}
	provider := ""
	if name := esc(model.Obj(embed["provider"])["name"]); name != "" {
		provider = `<div class="embed-provider">` + name + `</div>`
	}
	author := ""
	if name := esc(model.Obj(embed["author"])["name"]); name != "" {
		icon := ""
		if source := safeURL(model.Obj(embed["author"])["icon_url"]); source != "" {
			icon = `<img class="embed-author-icon" src="` + source + `" alt="" loading="lazy">`
		}
		label := name
		if target := safeURL(model.Obj(embed["author"])["url"]); target != "" {
			label = `<a href="` + target + `" target="_blank" rel="noopener noreferrer">` + name + `</a>`
		}
		author = `<div class="embed-author">` + icon + label + `</div>`
	}
	title := ""
	if text := esc(embed["title"]); text != "" {
		if target := safeURL(embed["url"]); target != "" {
			text = `<a href="` + target + `" target="_blank" rel="noopener noreferrer">` + text + `</a>`
		}
		title = `<div class="embed-title">` + text + `</div>`
	}
	description := ""
	if value := model.String(embed["description"]); value != "" {
		description = `<div class="embed-description">` + FormatContent(value, message) + `</div>`
	}
	var fields strings.Builder
	for _, field := range model.Objects(embed["fields"]) {
		class := "embed-field"
		if model.Bool(field["inline"]) {
			class += " inline"
		}
		fmt.Fprintf(&fields, `<div class="%s"><div class="embed-field-name">%s</div><div class="embed-field-value">%s</div></div>`, class, esc(field["name"]), FormatContent(model.String(field["value"]), message))
	}
	label := esc(embed["title"])
	if label == "" {
		label = "Embedded image"
	}
	media := ""
	if source := safeURL(model.Obj(embed["image"])["url"]); source != "" {
		media = `<button class="media-trigger embed-image-trigger" type="button" data-lightbox-src="` + source + `" data-lightbox-alt="` + label + `" data-lightbox-caption="` + label + `" aria-label="Open image preview"><img class="embed-media" src="` + source + `" alt="` + label + `" loading="lazy"></button>`
	}
	thumbnail := ""
	if source := safeURL(model.Obj(embed["thumbnail"])["url"]); source != "" {
		thumbnail = `<button class="media-trigger embed-thumbnail-trigger" type="button" data-lightbox-src="` + source + `" data-lightbox-alt="` + label + `" data-lightbox-caption="` + label + `" aria-label="Open thumbnail preview"><img class="embed-thumbnail" src="` + source + `" alt="` + label + `" loading="lazy"></button>`
	}
	footer := ""
	footerText := esc(model.Obj(embed["footer"])["text"])
	footerTime := ""
	if model.String(embed["timestamp"]) != "" {
		footerTime = `<time>` + html.EscapeString(fullTimestamp(embed["timestamp"])) + `</time>`
	}
	if footerText != "" || footerTime != "" {
		icon := ""
		if source := safeURL(model.Obj(embed["footer"])["icon_url"]); source != "" {
			icon = `<img class="embed-footer-icon" src="` + source + `" alt="" loading="lazy">`
		}
		separator := ""
		if footerText != "" && footerTime != "" {
			separator = `<span aria-hidden="true">•</span>`
		}
		footer = `<div class="embed-footer">` + icon + `<span>` + footerText + `</span>` + separator + footerTime + `</div>`
	}
	fieldHTML := ""
	if fields.Len() > 0 {
		fieldHTML = `<div class="embed-fields">` + fields.String() + `</div>`
	}
	class := "embed"
	if thumbnail != "" {
		class += " has-thumbnail"
	}
	return `<article class="` + class + `" style="--embed-color:` + color + `"><div class="embed-body">` + provider + author + title + description + fieldHTML + footer + `</div>` + thumbnail + media + `</article>`
}

// fileIcon is the inline glyph shown on downloadable file cards.
func fileIcon() string {
	return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>`
}

func attachmentHTML(attachment model.Object) string {
	rawName := model.String(attachment["filename"])
	spoiler := model.Bool(attachment["spoiler"]) || strings.HasPrefix(rawName, "SPOILER_")
	rawName = strings.TrimPrefix(rawName, "SPOILER_")
	target, name, contentType := safeURL(attachment["url"]), html.EscapeString(rawName), model.String(attachment["content_type"])
	if name == "" {
		name = "attachment"
	}
	wrap := func(content string) string {
		if !spoiler {
			return `<div class="attachment">` + content + `</div>`
		}
		return `<div class="attachment attachment-spoiler" data-spoiler-container><div class="spoiler-content" data-spoiler-content inert tabindex="-1">` + content + `</div><button class="spoiler-reveal" type="button" data-spoiler-reveal aria-label="Reveal spoiler attachment">SPOILER</button></div>`
	}
	if target == "" {
		return wrap(`<span class="attachment-file-icon" aria-hidden="true">` + fileIcon() + `</span><span class="attachment-file-copy"><span class="attachment-file-name">` + name + `</span></span>`)
	}
	if strings.HasPrefix(contentType, "image/") {
		return wrap(`<button class="media-trigger" type="button" data-lightbox-src="` + target + `" data-lightbox-alt="` + name + `" data-lightbox-caption="` + name + `" aria-label="Open image preview: ` + name + `"><img class="attachment-media" src="` + target + `" alt="` + name + `" loading="lazy"></button>`)
	}
	if strings.HasPrefix(contentType, "video/") {
		return wrap(`<video src="` + target + `" controls preload="metadata"></video>`)
	}
	if strings.HasPrefix(contentType, "audio/") {
		return wrap(`<audio src="` + target + `" controls preload="metadata"></audio>`)
	}
	meta := []string{}
	if size := formatBytes(attachment["size"]); size != "0 B" {
		meta = append(meta, size)
	}
	if contentType != "" {
		meta = append(meta, html.EscapeString(contentType))
	}
	metadata := ""
	if len(meta) > 0 {
		metadata = `<span class="attachment-file-meta">` + strings.Join(meta, " • ") + `</span>`
	}
	return wrap(`<span class="attachment-file-icon" aria-hidden="true">` + fileIcon() + `</span><span class="attachment-file-copy"><a class="attachment-file attachment-file-name" href="` + target + `" target="_blank" rel="noopener noreferrer">` + name + `</a>` + metadata + `</span>`)
}

func reactionHTML(reaction model.Object) string {
	emoji := model.Obj(reaction["emoji"])
	label := model.String(emoji["name"])
	if label == "" {
		label = "reaction"
	}
	rendered := html.EscapeString(label)
	if id := model.String(emoji["id"]); id != "" {
		source := model.String(emoji["cached_url"])
		if source == "" {
			suffix := ".webp"
			if model.Bool(emoji["animated"]) {
				suffix += "?animated=true"
			}
			source = "https://cdn.discordapp.com/emojis/" + id + suffix
		}
		rendered = `<img class="reaction-emoji" src="` + html.EscapeString(source) + `" alt=":` + html.EscapeString(label) + `:" onerror="this.replaceWith(this.alt)">`
	}
	return fmt.Sprintf(`<span class="reaction" title="%s">%s<span>%d</span></span>`, html.EscapeString(label), rendered, model.Int(reaction["count"]))
}

func componentEmoji(emoji, message model.Object) string {
	if emoji == nil {
		return ""
	}
	name, id := model.String(emoji["name"]), model.String(emoji["id"])
	if name == "" {
		name = "emoji"
	}
	if id == "" {
		return `<span class="component-emoji" aria-hidden="true">` + html.EscapeString(name) + `</span>`
	}
	source := model.String(model.Obj(message["cached_emojis"])[id])
	if source == "" {
		suffix := ".webp"
		if model.Bool(emoji["animated"]) {
			suffix += "?animated=true"
		}
		source = "https://cdn.discordapp.com/emojis/" + id + suffix
	}
	return `<img class="component-emoji" src="` + html.EscapeString(source) + `" alt=":` + html.EscapeString(name) + `:" onerror="this.replaceWith(this.alt)">`
}

func componentButton(component, message model.Object) string {
	style := model.Int(component["style"])
	if style == 0 {
		style = 2
	}
	names := map[int]string{1: "primary", 2: "secondary", 3: "success", 4: "danger", 5: "link", 6: "premium"}
	name := names[style]
	if name == "" {
		name = "secondary"
	}
	content := componentEmoji(model.Obj(component["emoji"]), message)
	if label := model.String(component["label"]); label != "" {
		content += `<span>` + html.EscapeString(label) + `</span>`
	} else if content == "" {
		content = `<span>Button</span>`
	}
	if style == 5 {
		if target := safeURL(component["url"]); target != "" {
			return `<a class="component-button component-button-` + name + `" href="` + target + `" target="_blank" rel="noopener noreferrer">` + content + `<span class="component-external" aria-hidden="true">↗</span></a>`
		}
	}
	disabled, archived, title := "", " data-archived-action", "Archived action — execution is only available in Discord"
	if model.Bool(component["disabled"]) {
		disabled, archived, title = " disabled", "", "This button was disabled in Discord"
	}
	return `<button class="component-button component-button-` + name + `" type="button"` + disabled + archived + ` title="` + title + `">` + content + `</button>`
}

func componentSelect(component, message model.Object, options Options) string {
	typeNames := map[int]string{3: "Select an option", 5: "Select a user", 6: "Select a role", 7: "Select a mentionable", 8: "Select a channel"}
	placeholder := model.String(component["placeholder"])
	if placeholder == "" {
		placeholder = typeNames[model.Int(component["type"])]
	}
	if placeholder == "" {
		placeholder = "Select an option"
	}
	maxValues := model.Int(component["max_values"])
	if maxValues < 1 {
		maxValues = 1
	}
	items := model.Objects(component["options"])
	if len(items) == 0 {
		for _, value := range model.Objects(component["default_values"]) {
			id := model.String(value["id"])
			label := id
			if found := model.String(model.Obj(message["participant_names"])[id]); found != "" {
				label = found
			}
			for _, role := range options.Roles {
				if model.String(role["id"]) == id {
					label = model.String(role["name"])
				}
			}
			items = append(items, model.Object{"label": label, "value": id, "default": true})
		}
	}
	selected := []string{}
	var rendered strings.Builder
	for _, item := range items {
		label := model.String(item["label"])
		if label == "" {
			label = model.String(item["value"])
		}
		if label == "" {
			label = "Option"
		}
		selectedAttr := "false"
		if model.Bool(item["default"]) {
			selectedAttr = "true"
			selected = append(selected, label)
		}
		description := ""
		if value := model.String(item["description"]); value != "" {
			description = `<span class="component-option-description">` + html.EscapeString(value) + `</span>`
		}
		fmt.Fprintf(&rendered, `<button class="component-option" type="button" role="option" aria-selected="%s" data-component-option data-option-label="%s">%s<span class="component-option-copy"><span class="component-option-label">%s</span>%s</span><span class="component-option-check" aria-hidden="true">✓</span></button>`, selectedAttr, html.EscapeString(label), componentEmoji(model.Obj(item["emoji"]), message), html.EscapeString(label), description)
	}
	initial := placeholder
	if len(selected) == 1 && maxValues == 1 {
		initial = selected[0]
	} else if len(selected) > 0 {
		initial = fmt.Sprintf("%d selected", len(selected))
	}
	disabled := ""
	if model.Bool(component["disabled"]) {
		disabled = " disabled"
	}
	menu := rendered.String()
	if menu == "" {
		menu = `<div class="component-select-empty">Options were populated dynamically inside Discord.</div>`
	}
	return `<div class="component-select" data-component-select data-max-values="` + strconv.Itoa(maxValues) + `" data-placeholder="` + html.EscapeString(placeholder) + `"><button class="component-select-trigger" type="button" data-component-select-trigger aria-haspopup="listbox" aria-expanded="false"` + disabled + `><span data-component-select-value>` + html.EscapeString(initial) + `</span><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m7.3 9.3 4.7 4.7 4.7-4.7a1 1 0 1 1 1.4 1.4l-5.4 5.4a1 1 0 0 1-1.4 0L5.9 10.7a1 1 0 1 1 1.4-1.4Z"/></svg></button><div class="component-select-menu" role="listbox" aria-multiselectable="` + strconv.FormatBool(maxValues > 1) + `" hidden>` + menu + `</div></div>`
}

func componentAttachment(mediaValue any, message model.Object) model.Object {
	media := model.Obj(mediaValue)
	value := model.String(mediaValue)
	if media != nil {
		value = model.String(media["url"])
	}
	if strings.HasPrefix(value, "attachment://") {
		name, _ := url.PathUnescape(strings.TrimPrefix(value, "attachment://"))
		for _, attachment := range model.Objects(message["attachments"]) {
			if model.String(attachment["filename"]) == name {
				return attachment
			}
		}
		return nil
	}
	if safeURL(value) == "" {
		return nil
	}
	return model.Object{"url": value, "filename": model.String(media["name"]), "content_type": media["content_type"], "size": media["size"]}
}

func renderComponent(component, message model.Object, options Options) string {
	if component == nil {
		return ""
	}
	children := func(values any) string {
		var out strings.Builder
		for _, child := range model.Objects(values) {
			out.WriteString(renderComponent(child, message, options))
		}
		return out.String()
	}
	switch model.Int(component["type"]) {
	case 1:
		content := children(component["components"])
		if content != "" {
			return `<div class="component-row">` + content + `</div>`
		}
	case 2:
		return componentButton(component, message)
	case 3, 5, 6, 7, 8:
		return componentSelect(component, message, options)
	case 9:
		content, accessory := children(component["components"]), renderComponent(model.Obj(component["accessory"]), message, options)
		if content != "" || accessory != "" {
			suffix := ""
			if accessory != "" {
				suffix = `<div class="component-section-accessory">` + accessory + `</div>`
			}
			return `<div class="component-section"><div class="component-section-content">` + content + `</div>` + suffix + `</div>`
		}
	case 10:
		return `<div class="component-text-display">` + FormatContent(model.String(component["content"]), message) + `</div>`
	case 11:
		attachment := componentAttachment(component["media"], message)
		if attachment == nil {
			return ""
		}
		source := safeURL(attachment["url"])
		label := esc(component["description"])
		if label == "" {
			label = esc(attachment["filename"])
		}
		media := `<button class="media-trigger component-thumbnail" type="button" data-lightbox-src="` + source + `" data-lightbox-alt="` + label + `" data-lightbox-caption="` + label + `" aria-label="Open thumbnail preview"><img src="` + source + `" alt="` + label + `" loading="lazy"></button>`
		if !model.Bool(component["spoiler"]) {
			return media
		}
		return `<div class="component-thumbnail-spoiler" data-spoiler-container><div data-spoiler-content inert tabindex="-1">` + media + `</div><button class="spoiler-reveal" type="button" data-spoiler-reveal>SPOILER</button></div>`
	case 12:
		var out strings.Builder
		count := 0
		for _, item := range model.Objects(component["items"]) {
			attachment := componentAttachment(item["media"], message)
			if attachment == nil {
				continue
			}
			source, label, kind := safeURL(attachment["url"]), esc(item["description"]), model.String(attachment["content_type"])
			if strings.HasPrefix(kind, "video/") {
				out.WriteString(`<video src="` + source + `" controls preload="metadata"></video>`)
			} else {
				out.WriteString(`<button class="media-trigger" type="button" data-lightbox-src="` + source + `" data-lightbox-alt="` + label + `"><img src="` + source + `" alt="` + label + `" loading="lazy"></button>`)
			}
			count++
		}
		if count > 0 {
			return `<div class="component-media-gallery gallery-count-` + strconv.Itoa(min(count, 5)) + `">` + out.String() + `</div>`
		}
	case 13:
		attachment := componentAttachment(component["file"], message)
		if attachment != nil {
			attachment["spoiler"] = component["spoiler"]
			return attachmentHTML(attachment)
		}
	case 14:
		spacing := "small"
		if model.Int(component["spacing"]) == 2 {
			spacing = "large"
		}
		divider := ""
		if value, ok := component["divider"].(bool); !ok || value {
			divider = "<hr>"
		}
		return `<div class="component-separator component-separator-` + spacing + `">` + divider + `</div>`
	case 17:
		content := children(component["components"])
		if content == "" {
			return ""
		}
		accent := "#80848e"
		if n := model.Int(component["accent_color"]); n >= 0 && n <= 0xffffff {
			accent = fmt.Sprintf("#%06x", n)
		}
		if !model.Bool(component["spoiler"]) {
			return `<div class="component-container" style="--component-accent:` + accent + `"><div class="component-container-content">` + content + `</div></div>`
		}
		return `<div class="component-container component-container-spoiler" style="--component-accent:` + accent + `" data-spoiler-container><div class="component-container-content" data-spoiler-content inert tabindex="-1">` + content + `</div><button class="spoiler-reveal" type="button" data-spoiler-reveal>SPOILER</button></div>`
	}
	return ""
}

func primaryRoleColor(role model.Object) int {
	if colors := model.Obj(role["colors"]); colors != nil {
		if _, exists := colors["primary_color"]; exists {
			return model.Int(colors["primary_color"])
		}
	}
	return model.Int(role["color"])
}

func roleColor(message model.Object, roles []model.Object, overrides map[string]string) string {
	memberRoles := map[string]bool{}
	for _, role := range model.Slice(model.Obj(message["member"])["roles"]) {
		memberRoles[model.String(role)] = true
	}
	var selected model.Object
	for _, role := range roles {
		if memberRoles[model.String(role["id"])] && primaryRoleColor(role) > 0 && (selected == nil || model.Int(role["position"]) > model.Int(selected["position"])) {
			selected = role
		}
	}
	if color := primaryRoleColor(selected); color > 0 {
		return fmt.Sprintf(` style="color:#%06x"`, color)
	}
	if color := overrides[userName(message)]; roleColorValue.MatchString(color) {
		return ` style="color:` + color + `"`
	}
	return ""
}

func highestRole(message model.Object, roles []model.Object, predicate func(model.Object) bool) model.Object {
	memberRoles := map[string]bool{}
	for _, value := range model.Slice(model.Obj(message["member"])["roles"]) {
		memberRoles[model.String(value)] = true
	}
	var selected model.Object
	for _, role := range roles {
		if memberRoles[model.String(role["id"])] && predicate(role) && (selected == nil || model.Int(role["position"]) > model.Int(selected["position"])) {
			selected = role
		}
	}
	return selected
}

func rawRoleIcon(role model.Object) string {
	if value := model.String(role["cached_icon_url"]); value != "" {
		return value
	}
	if id, icon := model.String(role["id"]), model.String(role["icon"]); id != "" && icon != "" {
		return "https://cdn.discordapp.com/role-icons/" + id + "/" + icon + ".png?size=64&quality=lossless"
	}
	return ""
}

func roleIcon(message model.Object, roles []model.Object) string {
	role := highestRole(message, roles, func(role model.Object) bool {
		return rawRoleIcon(role) != "" || model.String(role["unicode_emoji"]) != ""
	})
	if role == nil {
		return ""
	}
	title := esc(role["name"])
	if source := safeURL(rawRoleIcon(role)); source != "" {
		return `<img class="role-icon" src="` + source + `" alt="" title="` + title + `" loading="lazy">`
	}
	return `<span class="role-icon role-icon-emoji" title="` + title + `" aria-hidden="true">` + esc(role["unicode_emoji"]) + `</span>`
}

func usernameHTML(message model.Object, options Options) string {
	id := esc(model.Obj(message["author"])["id"])
	class := "username"
	attrs := ""
	if id != "" {
		class += " profile-trigger"
		attrs = ` data-user-id="` + id + `" role="button" tabindex="0" aria-expanded="false" aria-controls="profile-card"`
	}
	badge := ""
	if model.String(message["webhook_id"]) != "" {
		badge = `<span class="bot-tag">APP</span>`
	} else if model.Bool(model.Obj(message["author"])["bot"]) {
		badge = `<span class="bot-tag">BOT</span>`
	}
	return `<span class="` + class + `"` + roleColor(message, options.Roles, options.ColorOverrides) + attrs + `><span class="username-text">` + html.EscapeString(userName(message)) + `</span>` + roleIcon(message, options.Roles) + badge + `</span>`
}

func regular(message model.Object) bool {
	kind := model.Int(message["type"])
	if systemTypes[kind] {
		return false
	}
	return regularTypes[kind] || model.String(message["content"]) != "" || len(model.Objects(message["embeds"])) > 0 || len(model.Objects(message["attachments"])) > 0
}

func grouped(message, previous model.Object) bool {
	if previous == nil || !regular(message) || !regular(previous) || model.Int(message["type"]) == 19 || model.Int(message["type"]) == 20 || message["message_reference"] != nil || message["interaction"] != nil || message["interaction_metadata"] != nil {
		return false
	}
	current, before := parseTime(message["timestamp"]), parseTime(previous["timestamp"])
	gap := current.Sub(before)
	return model.String(model.Obj(message["author"])["id"]) == model.String(model.Obj(previous["author"])["id"]) && dateKey(message["timestamp"]) == dateKey(previous["timestamp"]) && gap >= 0 && gap < 7*time.Minute
}

func interactionHTML(message model.Object) string {
	interaction := model.Obj(message["interaction"])
	if interaction == nil {
		interaction = model.Obj(message["interaction_metadata"])
	}
	if interaction == nil && model.Int(message["type"]) != 20 {
		return ""
	}
	user := model.Obj(interaction["user"])
	if user == nil {
		user = model.Obj(message["author"])
	}
	name := model.String(model.Obj(message["participant_names"])[model.String(user["id"])])
	if name == "" {
		name = model.String(user["global_name"])
	}
	if name == "" {
		name = model.String(user["username"])
	}
	if name == "" {
		name = "Unknown User"
	}
	command := model.String(interaction["name"])
	if command == "" {
		command = model.String(interaction["command_name"])
	}
	action := "used an application command"
	if command != "" {
		action = `used <strong>/` + html.EscapeString(strings.TrimPrefix(command, "/")) + `</strong>`
	}
	return `<div class="interaction-context">` + avatarMarkup(model.Object{"author": user}, true) + `<span><strong>` + html.EscapeString(name) + `</strong> ` + action + `</span></div>`
}

func threadHTML(message model.Object) string {
	thread := model.Obj(message["thread"])
	if thread == nil {
		return ""
	}
	count := model.Int(thread["message_count"])
	if count == 0 {
		count = model.Int(thread["total_message_sent"])
	}
	label := "Thread"
	if count > 0 {
		label = fmt.Sprintf("%d messages", count)
		if count == 1 {
			label = "1 message"
		}
	}
	preview := ""
	if last := model.Obj(thread["last_message"]); last != nil && model.String(last["content"]) != "" {
		preview = `<div class="thread-preview"><strong>` + html.EscapeString(userName(last)) + `</strong><span>` + html.EscapeString(model.String(last["content"])) + `</span></div>`
	}
	return `<div class="thread-card"><span class="thread-icon" aria-hidden="true">#</span><div class="thread-copy"><strong>` + html.EscapeString(fallbackString(model.String(thread["name"]), "Thread")) + `</strong><span>` + html.EscapeString(label) + `</span>` + preview + `</div></div>`
}

func fallbackString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// systemIcon maps Discord system message types to the glyph family Discord
// uses in-channel, so archived events read like the original client.
func systemIcon(messageType int) string {
	switch messageType {
	case 6: // message pinned
		return `<svg viewBox="0 0 24 24" fill="currentColor"><path d="M16 9V4h1c.55 0 1-.45 1-1s-.45-1-1-1H7c-.55 0-1 .45-1 1s.45 1 1 1h1v5c0 1.66-1.34 3-3 3v2h5.97v7l1 1 1-1v-7H19v-2c-1.66 0-3-1.34-3-3z"/></svg>`
	case 7: // member joined
		return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12h14M13 5l7 7-7 7"/></svg>`
	case 8, 9, 10, 11: // server boost tiers
		return `<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 2.5 21 9l-9 12.5L3 9z"/></svg>`
	case 12: // channel follow added
		return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg>`
	case 18: // thread started
		return `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M4 6h16M4 12h10M4 18h7"/></svg>`
	default:
		return `<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 2l2.4 7.6L22 12l-7.6 2.4L12 22l-2.4-7.6L2 12z"/></svg>`
	}
}

func systemText(message model.Object) string {
	name := `<strong>` + html.EscapeString(userName(message)) + `</strong>`
	switch model.Int(message["type"]) {
	case 1:
		return name + " added a recipient."
	case 2:
		return name + " removed a recipient."
	case 4:
		return name + " changed the channel name."
	case 5:
		return name + " changed the channel icon."
	case 6:
		return name + " pinned a message to this channel."
	case 7:
		return name + " joined the server."
	case 8, 9, 10, 11:
		return name + " boosted the server."
	case 12:
		return name + " added a channel follow."
	case 18:
		return name + " started a thread: <strong>" + esc(message["content"]) + "</strong>"
	case 22:
		return "Wondering who to invite? Start with anyone who can help you build the server!"
	}
	if content := model.String(message["content"]); content != "" {
		return FormatContent(content, message)
	}
	return name + " sent a system message."
}

func replyHTML(message model.Object, available map[string]bool) string {
	if model.Int(message["type"]) != 19 && message["message_reference"] == nil {
		return ""
	}
	ref := model.Obj(message["referenced_message"])
	if ref == nil {
		return `<div class="reply-context reply-deleted">Original message was deleted</div>`
	}
	snippet := model.String(ref["content"])
	if snippet == "" {
		if len(model.Objects(ref["attachments"])) > 0 {
			snippet = "Click to see attachment"
		} else {
			snippet = "Original message"
		}
	}
	snippet = strings.ReplaceAll(strings.ReplaceAll(snippet, "\r", " "), "\n", " ")
	runes := []rune(snippet)
	if len(runes) > 120 {
		snippet = string(runes[:120])
	}
	tag, href := "div", ""
	if id := model.String(ref["id"]); id != "" && available[id] {
		tag, href = "a", ` href="#message-`+html.EscapeString(id)+`"`
	}
	return `<` + tag + ` class="reply-context"` + href + `>` + avatarMarkup(ref, true) + `<span class="reply-username">` + html.EscapeString(userName(ref)) + `</span><div class="reply-snippet">` + FormatContent(snippet, ref) + `</div></` + tag + `>`
}

func renderMessage(message, previous model.Object, available map[string]bool, options Options) string {
	if !regular(message) {
		return `<div class="message system-message"><span class="system-icon" aria-hidden="true">` + systemIcon(model.Int(message["type"])) + `</span>` + systemText(message) + ` <time class="timestamp">` + html.EscapeString(shortTimestamp(message["timestamp"])) + `</time></div>`
	}
	isGrouped := grouped(message, previous)
	id := esc(message["id"])
	header := `<time class="grouped-time">` + html.EscapeString(shortTimestamp(message["timestamp"])) + `</time>`
	if !isGrouped {
		edited := ""
		if model.String(message["edited_timestamp"]) != "" {
			edited = " (edited)"
		}
		header = avatarMarkup(message, false) + `<div class="message-header">` + usernameHTML(message, options) + `<time class="timestamp">` + html.EscapeString(fullTimestamp(message["timestamp"])) + edited + `</time></div>`
	}
	permalink := ""
	if id != "" {
		permalink = `<button class="message-link" type="button" data-copy-message-link="message-` + id + `" aria-label="Copy link to this message" title="Copy message link">#</button>`
	}
	var embeds, attachments, reactions, components strings.Builder
	for _, embed := range model.Objects(message["embeds"]) {
		embeds.WriteString(embedHTML(embed, message))
	}
	componentKeys := map[string]bool{}
	var collectComponentKeys func([]model.Object)
	collectComponentKeys = func(components []model.Object) {
		for _, component := range components {
			for _, value := range []any{component["media"], component["file"]} {
				media := model.Obj(value)
				source := model.String(value)
				if media != nil {
					source = model.String(media["url"])
					if id := model.String(media["attachment_id"]); id != "" {
						componentKeys[id] = true
					}
				}
				if source != "" {
					componentKeys[source] = true
					if strings.HasPrefix(source, "attachment://") {
						if name, err := url.PathUnescape(strings.TrimPrefix(source, "attachment://")); err == nil {
							componentKeys[name] = true
						}
					}
				}
			}
			for _, item := range model.Objects(component["items"]) {
				media := model.Obj(item["media"])
				source := model.String(media["url"])
				componentKeys[source] = source != ""
			}
			collectComponentKeys(model.Objects(component["components"]))
			if accessory := model.Obj(component["accessory"]); accessory != nil {
				collectComponentKeys([]model.Object{accessory})
			}
		}
	}
	collectComponentKeys(model.Objects(message["components"]))
	for _, attachment := range model.Objects(message["attachments"]) {
		if componentKeys[model.String(attachment["id"])] || componentKeys[model.String(attachment["filename"])] || componentKeys[model.String(attachment["url"])] {
			continue
		}
		attachments.WriteString(attachmentHTML(attachment))
	}
	for _, component := range model.Objects(message["components"]) {
		components.WriteString(renderComponent(component, message, options))
	}
	for _, reaction := range model.Objects(message["reactions"]) {
		reactions.WriteString(reactionHTML(reaction))
	}
	class := "group-start"
	prefix := replyHTML(message, available) + interactionHTML(message)
	if isGrouped {
		class = "grouped"
		prefix = ""
	}
	embedBlock := ""
	if embeds.Len() > 0 {
		embedBlock = `<div class="embeds">` + embeds.String() + `</div>`
	}
	reactionBlock := ""
	if reactions.Len() > 0 {
		reactionBlock = `<div class="reactions">` + reactions.String() + `</div>`
	}
	componentBlock := ""
	if components.Len() > 0 {
		componentBlock = `<div class="message-components">` + components.String() + `</div>`
	}
	return prefix + `<article id="message-` + id + `" class="message ` + class + `" data-message-id="` + id + `">` + header + permalink + `<div class="content">` + FormatContent(model.String(message["content"]), message) + `</div>` + embedBlock + attachments.String() + componentBlock + threadHTML(message) + reactionBlock + `</article>`
}

func safeRenderMessage(message, previous model.Object, available map[string]bool, options Options) (output string) {
	defer func() {
		if recover() != nil {
			id := esc(message["id"])
			if id == "" {
				id = "unknown"
			}
			output = `<article id="message-` + id + `" class="message message-render-error" data-message-id="` + id + `" role="note"><strong>Message could not be displayed</strong><span>This archived message used unsupported or malformed Discord data.</span></article>`
		}
	}()
	return renderMessage(message, previous, available, options)
}

func downloadMenu(id string) string {
	if id == "" {
		return ""
	}
	base := `/transcripts/` + html.EscapeString(id) + `/download`
	return `<div class="download-menu" data-download-menu><button class="download-trigger" type="button" data-download-trigger aria-expanded="false"><svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M11 3a1 1 0 1 1 2 0v10.59l3.3-3.3a1 1 0 0 1 1.4 1.42l-5 5a1 1 0 0 1-1.4 0l-5-5a1 1 0 0 1 1.4-1.42l3.3 3.3V3ZM5 19a1 1 0 0 1 1-1h12a1 1 0 1 1 0 2H6a1 1 0 0 1-1-1Z"/></svg><span class="download-label">Download</span><svg class="download-chevron" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M7.41 8.59 12 13.17l4.59-4.58L18 10l-6 6-6-6z"/></svg></button><div class="download-popover" data-download-popover role="menu" hidden><a class="download-option" role="menuitem" href="` + base + `/html">HTML <small>.html</small></a><a class="download-option" role="menuitem" href="` + base + `/json">Raw data <small>.json</small></a><a class="download-option" role="menuitem" href="` + base + `/txt">Plain text <small>.txt</small></a></div></div>`
}

func Transcript(messages []model.Object, channelID string, channel model.Object, options Options) string {
	result, _ := transcript(messages, channelID, channel, options, 0)
	return result
}

func TranscriptBounded(messages []model.Object, channelID string, channel model.Object, options Options, maxBytes int) (string, error) {
	if maxBytes < 1 {
		return "", fmt.Errorf("invalid transcript HTML limit")
	}
	return transcript(messages, channelID, channel, options, maxBytes)
}

func transcript(messages []model.Object, channelID string, channel model.Object, options Options, maxBytes int) (string, error) {
	sorted := model.NormalizeMessageMembers(messages)
	sort.SliceStable(sorted, func(i, j int) bool {
		return model.String(sorted[i]["timestamp"]) < model.String(sorted[j]["timestamp"])
	})
	available := map[string]bool{}
	participants := model.Object{}
	remember := func(message model.Object) {
		if message == nil {
			return
		}
		user := model.Obj(message["author"])
		id := model.String(user["id"])
		if id != "" {
			participants[id] = userName(message)
		}
		for _, mention := range model.Objects(message["mentions"]) {
			mid := model.String(mention["id"])
			name := model.String(mention["global_name"])
			if name == "" {
				name = model.String(mention["username"])
			}
			if mid != "" && name != "" {
				participants[mid] = name
			}
		}
	}
	channels := model.Object{}
	if id := model.String(channel["id"]); id != "" {
		channels[id] = fallbackString(model.String(channel["name"]), id)
	} else {
		channels[channelID] = fallbackString(model.String(channel["name"]), channelID)
	}
	for _, message := range sorted {
		available[model.String(message["id"])] = true
		remember(message)
		remember(model.Obj(message["referenced_message"]))
		for _, mentioned := range model.Objects(message["mention_channels"]) {
			channels[model.String(mentioned["id"])] = fallbackString(model.String(mentioned["name"]), model.String(mentioned["id"]))
		}
	}
	profiles := model.Object{}
	addProfile := func(message model.Object, overwrite bool) {
		if message == nil {
			return
		}
		user := model.Obj(message["author"])
		if user == nil {
			user = message
		}
		id := model.String(user["id"])
		if id == "" {
			return
		}
		if _, exists := profiles[id]; exists && !overwrite {
			return
		}
		role := highestRole(message, options.Roles, func(role model.Object) bool { return model.String(role["name"]) != "@everyone" })
		colorRole := highestRole(message, options.Roles, func(role model.Object) bool { return primaryRoleColor(role) > 0 })
		color := ""
		if n := primaryRoleColor(colorRole); n > 0 {
			color = fmt.Sprintf("#%06x", n)
		}
		displayName := userName(message)
		if message["author"] == nil {
			displayName = model.String(user["global_name"])
			if displayName == "" {
				displayName = model.String(user["username"])
			}
			if displayName == "" {
				displayName = "Unknown User"
			}
		}
		profiles[id] = model.Object{"id": id, "displayName": displayName, "username": model.String(user["username"]), "avatar": avatarURL(user), "bot": model.Bool(user["bot"]), "roleName": model.String(role["name"]), "roleColor": color, "roleIcon": rawRoleIcon(role), "roleEmoji": model.String(role["unicode_emoji"])}
	}
	for _, message := range sorted {
		message["participant_names"] = participants
		message["channel_names"] = channels
		addProfile(message, true)
		addProfile(model.Obj(message["referenced_message"]), false)
		for _, mention := range model.Objects(message["mentions"]) {
			addProfile(mention, false)
		}
	}
	profilesRaw, _ := json.Marshal(profiles)
	profilesJSON := strings.ReplaceAll(strings.ReplaceAll(string(profilesRaw), "<", "\\u003c"), "&", "\\u0026")
	if maxBytes > 0 && len(profilesJSON) > maxBytes {
		return "", fmt.Errorf("transcript HTML exceeds %d bytes", maxBytes)
	}
	var body strings.Builder
	var previous model.Object
	previousDate := ""
	for _, message := range sorted {
		current := dateKey(message["timestamp"])
		if current != "" && current != previousDate {
			body.WriteString(`<div class="date-divider" role="separator"><time>` + html.EscapeString(dateLabel(message["timestamp"])) + `</time></div>`)
			previous = nil
			previousDate = current
		}
		body.WriteString(safeRenderMessage(message, previous, available, options))
		if maxBytes > 0 && body.Len()+len(profilesJSON) > maxBytes {
			return "", fmt.Errorf("transcript HTML exceeds %d bytes", maxBytes)
		}
		previous = message
	}
	if len(sorted) == 0 {
		body.WriteString(`<div class="empty-state"><strong>No messages yet</strong>This channel does not contain any messages.</div>`)
	}
	display := model.String(channel["name"])
	if display == "" {
		display = channelID
	}
	topic := ""
	if model.String(channel["topic"]) != "" {
		topic = `<div class="channel-topic" title="` + esc(channel["topic"]) + `">` + FormatContent(model.String(channel["topic"]), nil) + `</div>`
	}
	csp := html.EscapeString(ContentSecurityPolicy(options.AssetOrigin))
	result := `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="Content-Security-Policy" content="` + csp + `"><meta name="referrer" content="no-referrer"><meta name="color-scheme" content="light dark"><link rel="icon" type="image/svg+xml" href="/favicon.svg"><title>#` + html.EscapeString(display) + ` — Discord transcript</title><style>` + Styles + `</style></head><body><script type="application/json" id="transcript-profiles">` + profilesJSON + `</script><header class="channel-header"><div class="channel-title"><span class="channel-hash">#</span><span class="channel-name">` + html.EscapeString(display) + `</span></div>` + topic + `<div class="header-spacer"></div>` + downloadMenu(options.TranscriptID) + `<button id="theme-toggle" class="theme-toggle" type="button" aria-label="Use light theme" title="Use light theme"><span class="theme-icon theme-icon-sun" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M4.93 4.93l1.41 1.41m11.32 11.32 1.41 1.41M2 12h2m16 0h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41"/></svg></span><span class="theme-icon theme-icon-moon" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3a7 7 0 0 0 9.79 9.79z"/></svg></span></button></header><main class="messages">` + body.String() + `<div id="transcript-end" aria-hidden="true"></div></main><aside id="profile-card" class="profile-card" role="region" aria-label="Member profile" hidden><div class="profile-card-accent"></div><div class="profile-card-body"><img class="profile-card-avatar" data-profile-avatar alt=""><div class="profile-card-name-row"><strong data-profile-name></strong><span class="bot-tag" data-profile-bot hidden>BOT</span></div><div class="profile-card-username" data-profile-username></div><div class="profile-card-role" data-profile-role hidden><span data-profile-role-icon></span><span data-profile-role-name></span></div></div></aside><dialog id="image-lightbox" class="image-lightbox" aria-label="Image preview"><button class="lightbox-close" type="button" data-lightbox-close aria-label="Close image preview">×</button><figure class="lightbox-figure"><img class="lightbox-image" data-lightbox-image alt=""><figcaption class="lightbox-caption" data-lightbox-caption></figcaption></figure></dialog><script>` + Script + `</script></body></html>`
	if maxBytes > 0 && len(result) > maxBytes {
		return "", fmt.Errorf("transcript HTML exceeds %d bytes", maxBytes)
	}
	return result, nil
}
