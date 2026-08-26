package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/kaarude/discord-transcript-api/internal/model"
	"github.com/kaarude/discord-transcript-api/internal/store"
)

func Participants(messages []model.Object) []store.Participant {
	byID := map[string]*store.Participant{}
	for _, message := range messages {
		author := model.Obj(message["author"])
		id := model.String(author["id"])
		if id == "" {
			continue
		}
		participant := byID[id]
		if participant == nil {
			participant = &store.Participant{
				ID: id, Username: fallback(model.String(author["username"]), "Unknown User"),
				DisplayName: model.DisplayName(message), Bot: model.Bool(author["bot"]),
			}
			byID[id] = participant
		}
		participant.MessageCount++
	}
	result := make([]store.Participant, 0, len(byID))
	for _, participant := range byID {
		result = append(result, *participant)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].MessageCount != result[j].MessageCount {
			return result[i].MessageCount > result[j].MessageCount
		}
		return strings.ToLower(result[i].DisplayName) < strings.ToLower(result[j].DisplayName)
	})
	return result
}

func JSON(uuid, channelID string, channel model.Object, roles, messages []model.Object, participants []store.Participant, createdAt string) ([]byte, error) {
	var output bytes.Buffer
	if err := WriteJSON(&output, uuid, channelID, channel, roles, messages, participants, createdAt); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// WriteJSON writes one message at a time so a transcript cannot require a
// second, full-size in-memory representation while it is being persisted.
func WriteJSON(writer io.Writer, uuid, channelID string, channel model.Object, roles, messages []model.Object, participants []store.Participant, createdAt string) error {
	metadata := model.Object{
		"id": uuid, "createdAt": createdAt,
		"channel": model.Object{
			"id": channelID, "name": fallback(model.String(channel["name"]), channelID),
			"topic": nullable(channel["topic"]), "guildId": nullable(channel["guild_id"]),
		},
		"messageCount": len(messages), "participants": participants, "roles": roles,
	}
	encoded, err := json.MarshalIndent(metadata, "  ", "  ")
	if err != nil {
		return err
	}
	if _, err := io.WriteString(writer, "{\n  \"transcript\": "); err != nil {
		return err
	}
	if _, err := writer.Write(encoded); err != nil {
		return err
	}
	if _, err := io.WriteString(writer, ",\n  \"messages\": ["); err != nil {
		return err
	}
	for index, message := range messages {
		encoded, err = json.MarshalIndent(message, "    ", "  ")
		if err != nil {
			return err
		}
		separator := "\n    "
		if index > 0 {
			separator = ",\n    "
		}
		if _, err := io.WriteString(writer, separator); err != nil {
			return err
		}
		if _, err := writer.Write(encoded); err != nil {
			return err
		}
	}
	if len(messages) > 0 {
		_, err = io.WriteString(writer, "\n  ]\n}")
	} else {
		_, err = io.WriteString(writer, "]\n}")
	}
	return err
}

func Text(channelID string, channel model.Object, messages []model.Object, createdAt string) []byte {
	var output bytes.Buffer
	_ = WriteText(&output, channelID, channel, messages, createdAt)
	return output.Bytes()
}

// WriteText streams the text representation and keeps at most one rendered
// message block in memory at a time.
func WriteText(writer io.Writer, channelID string, channel model.Object, messages []model.Object, createdAt string) error {
	sorted := append([]model.Object(nil), messages...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return model.String(sorted[i]["timestamp"]) < model.String(sorted[j]["timestamp"])
	})
	headerEnding := "\n"
	if len(sorted) > 0 {
		headerEnding = "\n\n"
	}
	if _, err := fmt.Fprintf(writer, "#%s\n%s\nExported %s%s", fallback(model.String(channel["name"]), channelID), model.String(channel["topic"]), createdAt, headerEnding); err != nil {
		return err
	}
	for index, message := range sorted {
		var block strings.Builder
		bot := ""
		if model.Bool(model.Obj(message["author"])["bot"]) {
			bot = " [APP]"
		}
		fmt.Fprintf(&block, "[%s] %s%s\n", model.String(message["timestamp"]), model.DisplayName(message), bot)
		if content := model.String(message["content"]); content != "" {
			block.WriteString(content + "\n")
		}
		for _, embed := range model.Objects(message["embeds"]) {
			if title := model.String(embed["title"]); title != "" {
				fmt.Fprintf(&block, "Embed: %s\n", title)
			}
			if description := model.String(embed["description"]); description != "" {
				block.WriteString(description + "\n")
			}
			for _, field := range model.Objects(embed["fields"]) {
				fmt.Fprintf(&block, "%s: %s\n", model.String(field["name"]), model.String(field["value"]))
			}
		}
		for _, attachment := range model.Objects(message["attachments"]) {
			name := fallback(model.String(attachment["filename"]), model.String(attachment["url"]))
			fmt.Fprintf(&block, "Attachment: %s — %s\n", name, model.String(attachment["url"]))
		}
		if reactions := model.Objects(message["reactions"]); len(reactions) > 0 {
			labels := make([]string, 0, len(reactions))
			for _, reaction := range reactions {
				labels = append(labels, fmt.Sprintf("%s ×%d", fallback(model.String(model.Obj(reaction["emoji"])["name"]), "?"), model.Int(reaction["count"])))
			}
			block.WriteString("Reactions: " + strings.Join(labels, ", ") + "\n")
		}
		if _, err := io.WriteString(writer, strings.TrimRight(block.String(), "\n")); err != nil {
			return err
		}
		ending := "\n"
		if index+1 < len(sorted) {
			ending = "\n\n"
		}
		if _, err := io.WriteString(writer, ending); err != nil {
			return err
		}
	}
	return nil
}

func fallback(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func nullable(value any) any {
	if model.String(value) == "" {
		return nil
	}
	return value
}
