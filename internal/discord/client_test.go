package discord

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (fn roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestGetRejectsDeclaredOversizeResponse(t *testing.T) {
	client := &Client{http: &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: MaxDiscordResponseBytes + 1, Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
	})}}
	var target map[string]any
	if _, err := client.get(context.Background(), "token", "/test", &target); err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("oversize response accepted: %v", err)
	}
}

func TestGetDecodesBoundedResponse(t *testing.T) {
	client := &Client{http: &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 11, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: req}, nil
	})}}
	var target map[string]any
	bytes, err := client.get(context.Background(), "token", "/test", &target)
	if err != nil || bytes != 11 || target["ok"] != true {
		t.Fatalf("bounded response failed: bytes=%d target=%#v err=%v", bytes, target, err)
	}
}
