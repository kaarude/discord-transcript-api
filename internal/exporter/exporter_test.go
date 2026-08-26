package exporter

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kaarude/discord-transcript-api/internal/model"
)

var errWriterLimit = errors.New("writer limit reached")

type cappedWriter struct {
	remaining int
	written   int
}

func (writer *cappedWriter) Write(data []byte) (int, error) {
	if len(data) > writer.remaining {
		return 0, errWriterLimit
	}
	writer.remaining -= len(data)
	writer.written += len(data)
	return len(data), nil
}

func TestWriteJSONStreamsMessagesAndPropagatesBudgetError(t *testing.T) {
	member := model.Object{"display_name": strings.Repeat("x", 2048), "roles": []any{"role-1", "role-2"}}
	messages := make([]model.Object, 128)
	for index := range messages {
		messages[index] = model.Object{
			"id": "message", "content": "hello", "timestamp": "2026-07-15T12:00:00Z",
			"author": model.Object{"id": "user", "username": "user"}, "member": member,
		}
	}
	limited := &cappedWriter{remaining: 4096}
	err := WriteJSON(limited, "transcript", "channel", model.Object{"name": "general"}, nil, messages, nil, "2026-07-15T12:00:00Z")
	if !errors.Is(err, errWriterLimit) {
		t.Fatalf("budget error was not propagated: %v", err)
	}
	if limited.written == 0 || limited.written > 4096 {
		t.Fatalf("stream wrote an invalid byte count: %d", limited.written)
	}

	var output bytes.Buffer
	if err := WriteJSON(&output, "transcript", "channel", model.Object{"name": "general"}, nil, messages, nil, "2026-07-15T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Transcript struct {
			MessageCount int `json:"messageCount"`
		} `json:"transcript"`
		Messages []model.Object `json:"messages"`
	}
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("streamed JSON is invalid: %v", err)
	}
	if decoded.Transcript.MessageCount != len(messages) || len(decoded.Messages) != len(messages) {
		t.Fatalf("streamed JSON lost messages: metadata=%d messages=%d", decoded.Transcript.MessageCount, len(decoded.Messages))
	}
}

func TestWriteTextPreservesFormatAndPropagatesErrors(t *testing.T) {
	messages := []model.Object{
		{"timestamp": "2026-07-15T12:01:00Z", "content": "second", "author": model.Object{"username": "Beta"}},
		{"timestamp": "2026-07-15T12:00:00Z", "content": "first", "author": model.Object{"username": "Alpha"}},
	}
	var output bytes.Buffer
	if err := WriteText(&output, "channel", model.Object{"name": "general", "topic": "Topic"}, messages, "now"); err != nil {
		t.Fatal(err)
	}
	want := "#general\nTopic\nExported now\n\n[2026-07-15T12:00:00Z] Alpha\nfirst\n\n[2026-07-15T12:01:00Z] Beta\nsecond\n"
	if output.String() != want {
		t.Fatalf("unexpected text export:\n%q\nwant:\n%q", output.String(), want)
	}
	limited := &cappedWriter{remaining: 8}
	if err := WriteText(limited, "channel", model.Object{"name": "general"}, messages, "now"); !errors.Is(err, errWriterLimit) {
		t.Fatalf("text writer error was not propagated: %v", err)
	}
}
