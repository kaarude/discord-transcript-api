package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/kaarude/discord-transcript-api/internal/auth"
	"github.com/kaarude/discord-transcript-api/internal/config"
	"github.com/kaarude/discord-transcript-api/internal/discord"
	"github.com/kaarude/discord-transcript-api/internal/exporter"
	"github.com/kaarude/discord-transcript-api/internal/health"
	"github.com/kaarude/discord-transcript-api/internal/media"
	"github.com/kaarude/discord-transcript-api/internal/model"
	"github.com/kaarude/discord-transcript-api/internal/render"
	"github.com/kaarude/discord-transcript-api/internal/store"
	"github.com/kaarude/discord-transcript-api/internal/web"
)

var transcriptID = regexp.MustCompile(`^(?:[0-9a-f]{16}|[0-9a-f]{32})$`)
var assetName = regexp.MustCompile(`^[0-9a-f]{64}\.[a-z0-9]{1,8}$`)
var preservedColor = regexp.MustCompile(`<span class="[^"]*\busername\b[^"]*" style="color:(#[0-9a-fA-F]{6})"[^>]*>(?:<span class="username-text">)?([^<]+)`)
var errExportTooLarge = errors.New("transcript bundle exceeds storage reservation")

const (
	RendererVersion    = 11
	maxRenderedHTML    = 128 << 20
	storageReservation = media.DefaultTotalLimit + maxRenderedHTML + 8*discord.MaxTranscriptResponseBytes
)

type App struct {
	settings        *config.Settings
	registry        *store.Registry
	discord         *discord.Client
	log             *slog.Logger
	health          *health.Monitor
	apiLimit        *limiter
	adminLimit      *limiter
	loginLimit      *limiter
	apiFailLimit    *limiter
	auth            Authenticator
	publicBase      string
	trustProxy      string
	storageMu       sync.Mutex
	storageReserved int64
	storageLimit    int64
	exportSlots     chan struct{}
}

type Authenticator interface {
	SetupRequired() bool
	Passkeys() []auth.Passkey
	Authenticated(*http.Request) (bool, time.Time)
	BeginLogin() (*protocol.CredentialAssertion, *http.Cookie, error)
	FinishLogin(*http.Request) (*http.Cookie, time.Time, error)
	BeginSetup(string, string) (*protocol.CredentialCreation, *http.Cookie, error)
	FinishSetup(*http.Request) (*http.Cookie, time.Time, error)
	BeginRegistration(string) (*protocol.CredentialCreation, *http.Cookie, error)
	FinishRegistration(*http.Request) error
	DeletePasskey(string) error
	Logout(*http.Request) *http.Cookie
	LoginPassword(string) (*http.Cookie, time.Time, error)
	CompletePasswordSetup(string, string) error
	SetPassword(current, next string) error
	ClearPassword(current string) error
	HasPassword() bool
}

type budgetWriter struct {
	writer    io.Writer
	remaining int64
}

func (writer *budgetWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.remaining {
		return 0, errExportTooLarge
	}
	written, err := writer.writer.Write(data)
	writer.remaining -= int64(written)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	return written, err
}

func New(settings *config.Settings, registry *store.Registry, log *slog.Logger, authenticator Authenticator) http.Handler {
	publicBase := strings.TrimSuffix(config.Env("PUBLIC_BASE_URL", ""), "/")
	storageLimit, err := strconv.ParseInt(config.Env("TRANSCRIPT_STORAGE_LIMIT_BYTES", "10737418240"), 10, 64)
	if err != nil || storageLimit <= 0 {
		storageLimit = 10 << 30
	}
	app := &App{settings: settings, registry: registry, discord: discord.New(), log: log, health: health.New(), apiLimit: newLimiter(), adminLimit: newLimiter(), loginLimit: newLimiter(), apiFailLimit: newLimiter(), auth: authenticator, publicBase: publicBase, trustProxy: strings.TrimSpace(os.Getenv("TRUST_PROXY")), storageLimit: storageLimit, exportSlots: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /access", app.accessPage)
	mux.HandleFunc("GET /access/status", app.accessStatus)
	mux.HandleFunc("POST /access/passkeys/login/start", app.passkeyLoginStart)
	mux.HandleFunc("POST /access/passkeys/login/finish", app.passkeyLoginFinish)
	mux.HandleFunc("POST /access/setup/passkeys/start", app.passkeySetupStart)
	mux.HandleFunc("POST /access/setup/passkeys/finish", app.passkeySetupFinish)
	mux.HandleFunc("POST /access/setup/password", app.passwordSetup)
	mux.HandleFunc("POST /access/login/password", app.passwordLogin)
	mux.HandleFunc("POST /access/logout", app.accessLogout)
	mux.Handle("GET /health", app.protected(http.HandlerFunc(app.healthRoot), true))
	mux.Handle("GET /health/{$}", app.protected(http.HandlerFunc(app.healthRoot), true))
	mux.Handle("GET /health/data", app.protected(http.HandlerFunc(app.healthData), true))
	mux.Handle("GET /health/ping", app.protected(http.HandlerFunc(app.healthPing), true))
	mux.Handle("GET /health/probe", app.protected(http.HandlerFunc(app.healthProbe), true))
	mux.HandleFunc("GET /favicon.svg", func(res http.ResponseWriter, req *http.Request) {
		res.Header().Set("Content-Type", "image/svg+xml")
		_, _ = io.WriteString(res, web.Favicon)
	})
	mux.Handle("GET /admin", app.protected(http.HandlerFunc(app.adminPage), true))
	mux.Handle("GET /admin/{$}", app.protected(http.HandlerFunc(app.adminPage), true))
	mux.Handle("GET /admin/index.html", app.protected(http.HandlerFunc(app.adminPage), true))
	mux.Handle("GET /health.html", app.protected(http.HandlerFunc(app.healthPage), true))
	mux.HandleFunc("GET /transcript", app.createTranscript)
	mux.HandleFunc("GET /transcripts/{uuid}", app.serveTranscript)
	mux.HandleFunc("GET /transcripts/{uuid}/download/{format}", app.downloadTranscript)
	mux.HandleFunc("GET /transcripts/{uuid}/assets/{filename}", app.serveAsset)
	mux.Handle("GET /api/admin/settings", app.protected(http.HandlerFunc(app.adminSettings), false))
	mux.Handle("POST /api/admin/tokens", app.protected(http.HandlerFunc(app.createToken), false))
	mux.Handle("DELETE /api/admin/tokens/{tokenID}", app.protected(http.HandlerFunc(app.deleteToken), false))
	mux.Handle("PUT /api/admin/rate-limit", app.protected(http.HandlerFunc(app.updateRateLimit), false))
	mux.Handle("PUT /api/admin/transcript-limit", app.protected(http.HandlerFunc(app.updateTranscriptLimit), false))
	mux.Handle("GET /api/admin/transcripts", app.protected(http.HandlerFunc(app.listTranscripts), false))
	mux.Handle("POST /api/admin/transcripts/{uuid}/renew", app.protected(http.HandlerFunc(app.renewTranscript), false))
	mux.Handle("PATCH /api/admin/transcripts/{uuid}/visibility", app.protected(http.HandlerFunc(app.visibility), false))
	mux.Handle("DELETE /api/admin/transcripts/{uuid}", app.protected(http.HandlerFunc(app.deleteTranscript), false))
	mux.Handle("GET /api/admin/passkeys", app.protected(http.HandlerFunc(app.listPasskeys), false))
	mux.Handle("POST /api/admin/passkeys/register/start", app.protected(http.HandlerFunc(app.passkeyRegistrationStart), false))
	mux.Handle("POST /api/admin/passkeys/register/finish", app.protected(http.HandlerFunc(app.passkeyRegistrationFinish), false))
	mux.Handle("DELETE /api/admin/passkeys/{passkeyID}", app.protected(http.HandlerFunc(app.deletePasskey), false))
	mux.Handle("PUT /api/admin/password", app.protected(http.HandlerFunc(app.updatePassword), false))
	mux.Handle("DELETE /api/admin/password", app.protected(http.HandlerFunc(app.removePassword), false))
	mux.HandleFunc("/", func(res http.ResponseWriter, req *http.Request) { writeError(res, http.StatusNotFound, "Not Found") })
	return app.health.Middleware(app.recover(mux))
}

func (a *App) protected(next http.Handler, browserRedirect bool) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		noCache(res)
		if !a.sameOrigin(req) {
			writeError(res, http.StatusForbidden, "Cross-origin request blocked")
			return
		}
		allowed, _ := a.auth.Authenticated(req)
		if !allowed {
			if browserRedirect && req.Method == http.MethodGet && strings.Contains(req.Header.Get("Accept"), "text/html") {
				target := safeReturnTo(req.URL.RequestURI())
				http.Redirect(res, req, "/access?returnTo="+url.QueryEscape(target), http.StatusFound)
				return
			}
			writeError(res, http.StatusUnauthorized, "Passkey sign-in required")
			return
		}
		next.ServeHTTP(res, req)
	})
}

func (a *App) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				a.log.Error("request panic", "error", value, "path", req.URL.Path)
				writeError(res, 500, "Internal Server Error")
			}
		}()
		next.ServeHTTP(res, req)
	})
}
func writeJSON(res http.ResponseWriter, status int, value any) {
	res.Header().Set("Content-Type", "application/json; charset=utf-8")
	res.WriteHeader(status)
	_ = json.NewEncoder(res).Encode(value)
}
func writeError(res http.ResponseWriter, status int, message string) {
	writeJSON(res, status, map[string]string{"error": message})
}
func decodeJSON(req *http.Request, target any) error {
	return json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(target)
}

func (a *App) clientIP(req *http.Request) string {
	remote := req.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	chain := forwardedIPs(req.Header.Get("X-Forwarded-For"))
	if hops, err := strconv.Atoi(a.trustProxy); err == nil && hops > 0 {
		chain = append(chain, remote)
		index := len(chain) - 1 - hops
		if index < 0 {
			index = 0
		}
		return chain[index]
	}
	if a.trustProxy == "true" {
		if len(chain) > 0 {
			return chain[len(chain)-1]
		}
		return remote
	}
	if a.proxyAllowed(remote) {
		current := remote
		for index := len(chain) - 1; index >= 0; index-- {
			if !a.proxyAllowed(current) {
				return current
			}
			current = chain[index]
		}
		return current
	}
	return remote
}

func forwardedIPs(header string) []string {
	chain := make([]string, 0)
	for _, value := range strings.Split(header, ",") {
		value = strings.TrimSpace(value)
		if net.ParseIP(value) != nil {
			chain = append(chain, value)
		}
	}
	return chain
}

func (a *App) proxyAllowed(remote string) bool {
	if a.trustProxy == "" {
		return false
	}
	if a.trustProxy == "true" {
		return true
	}
	ip := net.ParseIP(remote)
	if ip == nil {
		return false
	}
	for _, entry := range strings.Split(a.trustProxy, ",") {
		entry = strings.TrimSpace(entry)
		if candidate := net.ParseIP(entry); candidate != nil && candidate.Equal(ip) {
			return true
		}
		if _, network, err := net.ParseCIDR(entry); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
func secureEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (a *App) sameOrigin(req *http.Request) bool {
	if req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions {
		return true
	}
	origin := req.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	publicURL, err := url.Parse(a.publicBase)
	return err == nil && strings.EqualFold(parsed.Scheme, publicURL.Scheme) && strings.EqualFold(parsed.Host, req.Host)
}

func safeReturnTo(value string) string {
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") && !strings.ContainsAny(value, "\\\r\n") {
		return value
	}
	return "/"
}

func (a *App) apiAuth(res http.ResponseWriter, req *http.Request) (string, string, bool) {
	token := req.Header.Get("Authorization")
	if token == "" {
		writeError(res, 401, "Missing Authorization header")
		return "", "", false
	}
	token = strings.TrimPrefix(token, "Bot ")
	settings := a.settings.Snapshot()
	digest := config.HashToken(token)
	valid := false
	for _, entry := range settings.Tokens {
		if secureEqual(digest, entry.Hash) {
			valid = true
			break
		}
	}
	if !valid {
		// Failed guesses are throttled per source address so tokens cannot be
		// brute-forced; successful requests never consume this budget.
		clientIP := a.clientIP(req)
		if allowed, _, _ := a.apiFailLimit.allow(clientIP, 60, 15*time.Minute); !allowed {
			a.log.Warn("api token attempts throttled", "clientIp", clientIP)
			writeError(res, 429, "Too many failed attempts. Try again later.")
			return "", "", false
		}
		writeError(res, 401, "Invalid token")
		return "", "", false
	}
	discordToken := req.Header.Get("Discord-Bot-Token")
	if discordToken == "" {
		writeError(res, 400, "Missing Discord-Bot-Token header")
		return "", "", false
	}
	discordToken = strings.TrimPrefix(discordToken, "Bot ")
	allowed, remaining, reset := a.apiLimit.allow(digest, settings.RateLimit.Max, time.Duration(settings.RateLimit.WindowMS)*time.Millisecond)
	res.Header().Set("RateLimit-Limit", strconv.Itoa(settings.RateLimit.Max))
	res.Header().Set("RateLimit-Remaining", strconv.Itoa(remaining))
	res.Header().Set("RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	if !allowed {
		writeError(res, 429, "Rate limit exceeded. Try again later.")
		return "", "", false
	}
	return token, discordToken, true
}

func (a *App) adminAuth(res http.ResponseWriter, req *http.Request) bool {
	allowed, _, _ := a.adminLimit.allow(a.clientIP(req), 100, 15*time.Minute)
	if !allowed {
		writeError(res, 429, "Rate limit exceeded. Try again later.")
		return false
	}
	return true
}

func (a *App) acquireExport() bool {
	select {
	case a.exportSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *App) releaseExport() { <-a.exportSlots }

func (a *App) createTranscript(res http.ResponseWriter, req *http.Request) {
	_, botToken, ok := a.apiAuth(res, req)
	if !ok {
		return
	}
	channelID := req.Header.Get("Channel-Id")
	if channelID == "" {
		writeError(res, 400, "Missing Channel-Id header")
		return
	}
	if matched, _ := regexp.MatchString(`^[0-9]{17,20}$`, channelID); !matched {
		writeError(res, 400, "Invalid Channel-Id: must be a Discord snowflake ID")
		return
	}
	if !a.acquireExport() {
		writeError(res, http.StatusTooManyRequests, "A transcript export is already in progress")
		return
	}
	defer a.releaseExport()
	release, err := a.reserveTranscriptStorage()
	if err != nil {
		a.log.Error("check transcript storage", "error", err)
		writeError(res, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	if release == nil {
		writeError(res, http.StatusInsufficientStorage, "Transcript storage limit reached")
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Minute)
	defer cancel()
	settings := a.settings.Snapshot()
	var messages []model.Object
	var channel model.Object
	var messagesErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		messages, messagesErr = a.discord.Messages(ctx, botToken, channelID, settings.TranscriptLimit)
	}()
	go func() {
		defer wg.Done()
		var err error
		channel, err = a.discord.Channel(ctx, botToken, channelID)
		if err != nil {
			a.log.Warn("channel metadata unavailable", "error", err, "channelId", channelID)
			channel = model.Object{}
		}
	}()
	wg.Wait()
	if messagesErr != nil {
		var apiErr *discord.APIError
		if errors.As(messagesErr, &apiErr) {
			if apiErr.Code == 50001 || apiErr.Code == 50013 || apiErr.Status == 403 {
				writeError(res, 403, "Bot lacks permission for that channel")
				return
			}
			if apiErr.Code == 10003 || apiErr.Status == 404 {
				writeError(res, 404, "Channel not found")
				return
			}
		}
		a.log.Error("fetch messages", "error", messagesErr)
		writeError(res, 500, "Internal Server Error")
		return
	}
	roles := make([]model.Object, 0)
	if guildID := model.String(channel["guild_id"]); guildID != "" {
		var members []model.Object
		wg.Add(2)
		go func() {
			defer wg.Done()
			var err error
			roles, err = a.discord.Roles(ctx, botToken, guildID)
			if err != nil {
				a.log.Warn("guild roles unavailable", "error", err)
			}
		}()
		go func() {
			defer wg.Done()
			members = a.discord.Members(ctx, botToken, guildID, discord.AuthorIDs(messages))
		}()
		wg.Wait()
		messages = discord.AttachMembers(messages, members)
	}
	record, err := a.addBundle(ctx, channelID, channel, roles, messages)
	if err != nil {
		a.log.Error("store transcript", "error", err)
		writeError(res, 500, "Internal Server Error")
		return
	}
	writeJSON(res, 200, map[string]any{"url": a.publicBase + record.ViewPath(), "expiresAt": record.ExpiresAt, "expiresInDays": store.ExpiryDays})
}

func (a *App) reserveTranscriptStorage() (func(), error) {
	a.storageMu.Lock()
	defer a.storageMu.Unlock()
	if a.registry.StorageBytes()+a.storageReserved+storageReservation > a.storageLimit {
		return nil, nil
	}
	a.storageReserved += storageReservation
	return func() {
		a.storageMu.Lock()
		a.storageReserved -= storageReservation
		a.storageMu.Unlock()
	}, nil
}

func (a *App) addBundle(ctx context.Context, channelID string, channel model.Object, roles, messages []model.Object) (store.Record, error) {
	uuid := config.RandomHex(16)
	now := time.Now().UTC()
	root := a.registry.Dir()
	temp := filepath.Join(root, "."+uuid+".tmp")
	final := filepath.Join(root, uuid)
	_ = os.RemoveAll(temp)
	if err := os.MkdirAll(filepath.Join(temp, "assets"), 0o750); err != nil {
		return store.Record{}, err
	}
	failed := true
	defer func() {
		if failed {
			_ = os.RemoveAll(temp)
		}
	}()
	publicAssets := a.publicBase + "/transcripts/" + uuid + "/assets"
	cached, err := media.Cache(ctx, messages, media.Options{TranscriptID: uuid, AssetsDir: filepath.Join(temp, "assets"), PublicAssetURL: publicAssets, GuildID: model.String(channel["guild_id"]), Roles: roles})
	if err != nil {
		return store.Record{}, err
	}
	participants := exporter.Participants(cached.Messages)
	html, err := render.TranscriptBounded(cached.Messages, channelID, channel, render.Options{TranscriptID: uuid, Roles: cached.Roles, AssetOrigin: a.publicBase}, maxRenderedHTML)
	if err != nil {
		return store.Record{}, err
	}
	remaining := int64(storageReservation) - cached.Manifest.TotalBytes
	if remaining < 0 {
		return store.Record{}, errExportTooLarge
	}
	budget := &budgetWriter{remaining: remaining}
	writeExport := func(name string, generate func(io.Writer) error) error {
		file, err := os.OpenFile(filepath.Join(temp, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		budget.writer = file
		writeErr := generate(budget)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if err := writeExport("index.html", func(writer io.Writer) error {
		_, err := io.WriteString(writer, html)
		return err
	}); err != nil {
		return store.Record{}, err
	}
	html = ""
	if err := writeExport("transcript.json", func(writer io.Writer) error {
		return exporter.WriteJSON(writer, uuid, channelID, channel, cached.Roles, cached.Messages, participants, now.Format(time.RFC3339Nano))
	}); err != nil {
		return store.Record{}, err
	}
	if err := writeExport("transcript.txt", func(writer io.Writer) error {
		return exporter.WriteText(writer, channelID, channel, cached.Messages, now.Format(time.RFC3339Nano))
	}); err != nil {
		return store.Record{}, err
	}
	manifest, err := json.MarshalIndent(cached.Manifest, "", "  ")
	if err != nil {
		return store.Record{}, err
	}
	if err := writeExport("assets-manifest.json", func(writer io.Writer) error {
		_, err := writer.Write(manifest)
		return err
	}); err != nil {
		return store.Record{}, err
	}
	if err := os.Rename(temp, final); err != nil {
		return store.Record{}, err
	}
	failed = false
	record := store.Record{UUID: uuid, ChannelID: channelID, ChannelName: fallback(model.String(channel["name"]), channelID), CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(store.ExpiryDays * 24 * time.Hour).Format(time.RFC3339Nano), StorageVersion: 2, RendererVersion: RendererVersion, FilePath: "transcripts/" + uuid + "/index.html", Directory: "transcripts/" + uuid, Participants: participants, MessageCount: len(cached.Messages), CachedBytes: cached.Manifest.TotalBytes, Exports: []string{"html", "json", "txt"}, IsPublic: store.Public(true)}
	if err := a.registry.Add(record); err != nil {
		_ = os.RemoveAll(final)
		return store.Record{}, err
	}
	return record, nil
}
func fallback(value, other string) string {
	if value == "" {
		return other
	}
	return value
}

func (a *App) requireAccess(res http.ResponseWriter, req *http.Request, record store.Record) (bool, bool) {
	if record.Public() {
		return true, false
	}
	if record.AccessKey == "" {
		writeError(res, 404, "Transcript not found")
		return false, false
	}
	query := req.URL.Query().Get("access")
	cookie, _ := req.Cookie("transcript_access_" + record.UUID)
	cookieValue := ""
	if cookie != nil {
		cookieValue = cookie.Value
	}
	if !secureEqual(query, record.AccessKey) && !secureEqual(cookieValue, record.AccessKey) {
		writeError(res, 404, "Transcript not found")
		return false, false
	}
	fromQuery := secureEqual(query, record.AccessKey)
	if fromQuery {
		expires, _ := time.Parse(time.RFC3339Nano, record.ExpiresAt)
		maxAge := max(60, int(time.Until(expires).Seconds()))
		http.SetCookie(res, &http.Cookie{Name: "transcript_access_" + record.UUID, Value: record.AccessKey, Path: "/transcripts/" + record.UUID, Expires: time.Now().Add(time.Duration(maxAge) * time.Second), MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}
	return true, fromQuery
}
func (a *App) lookup(res http.ResponseWriter, req *http.Request, redirectQuery bool) (store.Record, bool) {
	uuid := req.PathValue("uuid")
	if !transcriptID.MatchString(uuid) {
		writeError(res, 400, "Invalid transcript ID")
		return store.Record{}, false
	}
	record, ok := a.registry.Get(uuid)
	if !ok {
		writeError(res, 404, "Transcript not found")
		return store.Record{}, false
	}
	allowed, fromQuery := a.requireAccess(res, req, record)
	if !allowed {
		return store.Record{}, false
	}
	if fromQuery && redirectQuery {
		http.Redirect(res, req, "/transcripts/"+record.UUID, http.StatusFound)
		return store.Record{}, false
	}
	if record.Expired() {
		writeJSON(res, 410, map[string]string{"error": "Link expired", "code": "LINK_EXPIRED"})
		return store.Record{}, false
	}
	return record, true
}
func sendFile(res http.ResponseWriter, req *http.Request, path, contentType, disposition string) {
	file, err := os.Open(path)
	if err != nil {
		writeError(res, 404, "Transcript file not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(res, 500, "Internal Server Error")
		return
	}
	res.Header().Set("Content-Type", contentType)
	res.Header().Set("Cache-Control", "no-store")
	res.Header().Set("X-Content-Type-Options", "nosniff")
	if disposition != "" {
		res.Header().Set("Content-Disposition", disposition)
	}
	http.ServeContent(res, req, info.Name(), info.ModTime(), file)
}
func (a *App) serveTranscript(res http.ResponseWriter, req *http.Request) {
	record, ok := a.lookup(res, req, true)
	if !ok {
		return
	}
	res.Header().Set("Content-Security-Policy", render.ContentSecurityPolicy(a.publicBase))
	res.Header().Set("Referrer-Policy", "no-referrer")
	sendFile(res, req, a.registry.Resolve(record, "html"), "text/html; charset=utf-8", fmt.Sprintf(`inline; filename="transcript-%s.html"`, record.UUID))
}
func (a *App) downloadTranscript(res http.ResponseWriter, req *http.Request) {
	format := req.PathValue("format")
	if !map[string]bool{"html": true, "json": true, "txt": true}[format] {
		writeError(res, 400, "Invalid transcript download")
		return
	}
	record, ok := a.lookup(res, req, false)
	if !ok {
		return
	}
	path := a.registry.Resolve(record, format)
	if path == "" {
		writeError(res, 404, "Export not available for this transcript")
		return
	}
	types := map[string]string{"html": "text/html; charset=utf-8", "json": "application/json; charset=utf-8", "txt": "text/plain; charset=utf-8"}
	name := model.SafeBaseName(fallback(record.ChannelName, record.ChannelID)) + "-" + record.UUID[:8] + "." + format
	sendFile(res, req, path, types[format], `attachment; filename="`+name+`"`)
}
func (a *App) serveAsset(res http.ResponseWriter, req *http.Request) {
	name := req.PathValue("filename")
	if !assetName.MatchString(name) {
		writeError(res, 400, "Invalid transcript asset")
		return
	}
	record, ok := a.lookup(res, req, false)
	if !ok {
		return
	}
	sendFile(res, req, filepath.Join(a.registry.Dir(), record.UUID, "assets", name), mimeFor(name), "")
}
func mimeFor(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg":
		return "audio/ogg"
	case ".pdf":
		return "application/pdf"
	}
	return "application/octet-stream"
}

func (a *App) adminSettings(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	settings := a.settings.Snapshot()
	tokens := make([]map[string]any, 0, len(settings.Tokens))
	for _, token := range settings.Tokens {
		tokens = append(tokens, map[string]any{"id": token.ID, "preview": token.Preview, "createdAt": token.CreatedAt})
	}
	writeJSON(res, 200, map[string]any{"apiTokens": tokens, "rateLimit": settings.RateLimit, "transcriptLimit": settings.TranscriptLimit})
}
func (a *App) createToken(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	value, id, now := config.RandomHex(32), config.RandomHex(8), config.NowISO()
	createdBy := a.clientIP(req)
	record := config.NewTokenRecord(id, value, createdBy)
	record.CreatedAt = &now
	_, err := a.settings.Update(func(settings *config.Values) error {
		settings.Tokens = append(settings.Tokens, record)
		return nil
	})
	if err != nil {
		writeError(res, 500, "Internal Server Error")
		return
	}
	writeJSON(res, 201, map[string]string{"id": id, "token": value, "message": "Token created successfully. Copy it now — it will not be shown again."})
}
func (a *App) deleteToken(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	found := false
	_, err := a.settings.Update(func(settings *config.Values) error {
		id := req.PathValue("tokenID")
		for index, token := range settings.Tokens {
			if token.ID == id {
				found = true
				settings.Tokens = append(settings.Tokens[:index], settings.Tokens[index+1:]...)
				break
			}
		}
		return nil
	})
	if err != nil {
		writeError(res, 500, "Internal Server Error")
		return
	}
	if !found {
		writeError(res, 404, "Token not found")
		return
	}
	writeJSON(res, 200, map[string]string{"message": "Token revoked successfully"})
}
func (a *App) updateRateLimit(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	var body struct {
		Max      int `json:"max"`
		WindowMS int `json:"windowMs"`
	}
	if decodeJSON(req, &body) != nil || body.Max < 1 || body.Max > 10000 {
		writeError(res, 400, "max must be a number between 1 and 10000")
		return
	}
	if body.WindowMS < 1000 || body.WindowMS > 3600000 {
		writeError(res, 400, "windowMs must be between 1000 and 3600000")
		return
	}
	_, err := a.settings.Update(func(settings *config.Values) error {
		settings.RateLimit = config.RateLimit{Max: body.Max, WindowMS: body.WindowMS}
		return nil
	})
	if err != nil {
		writeError(res, 500, "Internal Server Error")
		return
	}
	writeJSON(res, 200, map[string]any{"rateLimit": body, "message": "Rate limit updated. Changes take effect immediately."})
}
func (a *App) updateTranscriptLimit(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	var body struct {
		Limit int `json:"limit"`
	}
	if decodeJSON(req, &body) != nil || body.Limit < 1 || body.Limit > 50000 {
		writeError(res, 400, "limit must be between 1 and 50000")
		return
	}
	_, err := a.settings.Update(func(settings *config.Values) error { settings.TranscriptLimit = body.Limit; return nil })
	if err != nil {
		writeError(res, 500, "Internal Server Error")
		return
	}
	writeJSON(res, 200, map[string]any{"transcriptLimit": body.Limit, "message": "Transcript limit updated."})
}
func (a *App) listTranscripts(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	page, _ := strconv.Atoi(req.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
	writeJSON(res, 200, a.registry.List(store.ListOptions{UserQuery: req.URL.Query().Get("user"), Page: page, Limit: limit}))
}
func validAdminID(res http.ResponseWriter, id string) bool {
	if !transcriptID.MatchString(id) {
		writeError(res, 400, "Invalid transcript ID")
		return false
	}
	return true
}
func (a *App) renewTranscript(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	id := req.PathValue("uuid")
	if !validAdminID(res, id) {
		return
	}
	record, ok, err := a.registry.Renew(id)
	if err != nil {
		writeError(res, 500, "Internal Server Error")
		return
	}
	if !ok {
		writeError(res, 404, "Transcript not found")
		return
	}
	writeJSON(res, 200, map[string]any{"message": "Transcript renewed for 30 days", "expiresAt": record.ExpiresAt, "viewUrl": record.ViewPath()})
}
func (a *App) visibility(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	id := req.PathValue("uuid")
	if !validAdminID(res, id) {
		return
	}
	var raw map[string]any
	if decodeJSON(req, &raw) != nil {
		writeError(res, 400, "isPublic must be a boolean")
		return
	}
	public, ok := raw["isPublic"].(bool)
	if !ok {
		writeError(res, 400, "isPublic must be a boolean")
		return
	}
	record, found, err := a.registry.SetVisibility(id, public)
	if err != nil {
		writeError(res, 500, "Internal Server Error")
		return
	}
	if !found {
		writeError(res, 404, "Transcript not found")
		return
	}
	message := "Transcript is now private"
	if public {
		message = "Transcript is now public"
	}
	writeJSON(res, 200, map[string]any{"message": message, "isPublic": record.Public(), "viewUrl": record.ViewPath()})
}
func (a *App) deleteTranscript(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	id := req.PathValue("uuid")
	if !validAdminID(res, id) {
		return
	}
	found, err := a.registry.Remove(id)
	if err != nil {
		writeError(res, 500, "Internal Server Error")
		return
	}
	if !found {
		writeError(res, 404, "Transcript not found")
		return
	}
	writeJSON(res, 200, map[string]string{"message": "Transcript and cached media permanently deleted"})
}

func noCache(res http.ResponseWriter) { res.Header().Set("Cache-Control", "no-store, max-age=0") }
func secureAccessPage(res http.ResponseWriter) {
	res.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	res.Header().Set("Permissions-Policy", "publickey-credentials-create=(self), publickey-credentials-get=(self)")
	res.Header().Set("Referrer-Policy", "no-referrer")
	res.Header().Set("X-Content-Type-Options", "nosniff")
	res.Header().Set("X-Frame-Options", "DENY")
}
func (a *App) accessPage(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	secureAccessPage(res)
	res.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(res, web.AccessHTML)
}
func (a *App) accessStatus(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	authenticated, expires := a.auth.Authenticated(req)
	var validUntil any
	if authenticated {
		validUntil = expires.UTC().Format(time.RFC3339Nano)
	}
	passkeys := a.auth.Passkeys()
	writeJSON(res, http.StatusOK, map[string]any{
		"authenticated": authenticated,
		"setupRequired": a.auth.SetupRequired(),
		"validUntil":    validUntil,
		"hasPassword":   a.auth.HasPassword(),
		"hasPasskeys":   len(passkeys) > 0,
	})
}

func (a *App) allowLoginAttempt(res http.ResponseWriter, req *http.Request) bool {
	allowed, _, reset := a.loginLimit.allow(a.clientIP(req), 12, 15*time.Minute)
	res.Header().Set("RateLimit-Limit", "12")
	res.Header().Set("RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	if !allowed {
		writeError(res, http.StatusTooManyRequests, "Too many sign-in attempts. Try again later.")
		return false
	}
	return true
}

func (a *App) passkeyLoginStart(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	if !a.sameOrigin(req) {
		writeError(res, http.StatusForbidden, "Cross-origin request blocked")
		return
	}
	if !a.allowLoginAttempt(res, req) {
		return
	}
	options, cookie, err := a.auth.BeginLogin()
	if err != nil {
		if errors.Is(err, auth.ErrNotConfigured) {
			writeError(res, http.StatusConflict, "Finish first-time setup before signing in")
			return
		}
		a.log.Warn("begin passkey login", "error", err)
		writeError(res, http.StatusInternalServerError, "Could not start passkey sign-in")
		return
	}
	http.SetCookie(res, cookie)
	writeJSON(res, http.StatusOK, options)
}

func (a *App) passkeyLoginFinish(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	if !a.sameOrigin(req) {
		writeError(res, http.StatusForbidden, "Cross-origin request blocked")
		return
	}
	cookie, expires, err := a.auth.FinishLogin(req)
	if err != nil {
		a.log.Warn("finish passkey login", "error", err)
		writeError(res, http.StatusUnauthorized, "Passkey sign-in failed. Start again and use a registered passkey.")
		return
	}
	http.SetCookie(res, cookie)
	writeJSON(res, http.StatusOK, map[string]any{"authenticated": true, "validUntil": expires.UTC().Format(time.RFC3339Nano)})
}

func (a *App) passkeySetupStart(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	if !a.sameOrigin(req) {
		writeError(res, http.StatusForbidden, "Cross-origin request blocked")
		return
	}
	if !a.allowLoginAttempt(res, req) {
		return
	}
	var body struct {
		SetupCode string `json:"setupCode"`
		Name      string `json:"name"`
	}
	if decodeJSON(req, &body) != nil {
		writeError(res, http.StatusBadRequest, "Invalid request")
		return
	}
	options, cookie, err := a.auth.BeginSetup(body.SetupCode, body.Name)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidSetupCode):
			writeError(res, http.StatusUnauthorized, "Setup code is incorrect. Copy the current code from the server's setup-code file.")
		case errors.Is(err, auth.ErrSetupComplete):
			writeError(res, http.StatusConflict, "First-time setup is already complete")
		case errors.Is(err, auth.ErrInvalidName):
			writeError(res, http.StatusBadRequest, err.Error())
		default:
			a.log.Warn("begin passkey setup", "error", err)
			writeError(res, http.StatusInternalServerError, "Could not start passkey setup")
		}
		return
	}
	http.SetCookie(res, cookie)
	writeJSON(res, http.StatusOK, options)
}

func (a *App) passkeySetupFinish(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	if !a.sameOrigin(req) {
		writeError(res, http.StatusForbidden, "Cross-origin request blocked")
		return
	}
	cookie, expires, err := a.auth.FinishSetup(req)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrSetupComplete):
			writeError(res, http.StatusConflict, "First-time setup was already completed")
		default:
			a.log.Warn("finish passkey setup", "error", err)
			writeError(res, http.StatusBadRequest, "Passkey setup failed. Start again and complete the browser prompt.")
		}
		return
	}
	http.SetCookie(res, cookie)
	writeJSON(res, http.StatusCreated, map[string]any{"authenticated": true, "validUntil": expires.UTC().Format(time.RFC3339Nano)})
}

func (a *App) listPasskeys(res http.ResponseWriter, req *http.Request) {
	writeJSON(res, http.StatusOK, map[string]any{"passkeys": a.auth.Passkeys()})
}

// passwordSetup claims an unclaimed installation with a dashboard password
// instead of a passkey.
func (a *App) passwordSetup(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	if !a.sameOrigin(req) {
		writeError(res, http.StatusForbidden, "Cross-origin request blocked")
		return
	}
	if !a.allowLoginAttempt(res, req) {
		return
	}
	var body struct {
		SetupCode string `json:"setupCode"`
		Password  string `json:"password"`
	}
	if decodeJSON(req, &body) != nil {
		writeError(res, http.StatusBadRequest, "Invalid request")
		return
	}
	err := a.auth.CompletePasswordSetup(body.SetupCode, body.Password)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidSetupCode):
			writeError(res, http.StatusUnauthorized, "Setup code is incorrect. Copy the current code from the server's setup-code file.")
		case errors.Is(err, auth.ErrWeakPassword):
			writeError(res, http.StatusBadRequest, "Choose a password between 12 and 128 characters.")
		case errors.Is(err, auth.ErrSetupComplete):
			writeError(res, http.StatusConflict, "First-time setup is already complete")
		default:
			a.log.Warn("password setup", "error", err)
			writeError(res, http.StatusInternalServerError, "Could not configure the dashboard password")
		}
		return
	}
	cookie, expires, err := a.auth.LoginPassword(body.Password)
	if err != nil {
		a.log.Warn("password login after setup", "error", err)
		writeJSON(res, http.StatusCreated, map[string]any{"authenticated": false})
		return
	}
	http.SetCookie(res, cookie)
	writeJSON(res, http.StatusCreated, map[string]any{"authenticated": true, "validUntil": expires.UTC().Format(time.RFC3339Nano)})
}

func (a *App) passwordLogin(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	if !a.sameOrigin(req) {
		writeError(res, http.StatusForbidden, "Cross-origin request blocked")
		return
	}
	if !a.allowLoginAttempt(res, req) {
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if decodeJSON(req, &body) != nil {
		writeError(res, http.StatusBadRequest, "Invalid request")
		return
	}
	cookie, expires, err := a.auth.LoginPassword(body.Password)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrNoPassword):
			writeError(res, http.StatusConflict, "This server does not use a dashboard password")
		case errors.Is(err, auth.ErrWrongPassword):
			writeError(res, http.StatusUnauthorized, "Incorrect password")
		default:
			a.log.Warn("password login", "error", err)
			writeError(res, http.StatusInternalServerError, "Could not sign in")
		}
		return
	}
	http.SetCookie(res, cookie)
	writeJSON(res, http.StatusOK, map[string]any{"authenticated": true, "validUntil": expires.UTC().Format(time.RFC3339Nano)})
}

func (a *App) updatePassword(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	var body struct {
		Current string `json:"currentPassword,omitempty"`
		New     string `json:"newPassword"`
	}
	if decodeJSON(req, &body) != nil {
		writeError(res, http.StatusBadRequest, "Invalid request")
		return
	}
	err := a.auth.SetPassword(body.Current, body.New)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrWrongPassword):
			writeError(res, http.StatusUnauthorized, "Current password is incorrect")
		case errors.Is(err, auth.ErrWeakPassword):
			writeError(res, http.StatusBadRequest, "Choose a password between 12 and 128 characters.")
		default:
			a.log.Warn("update password", "error", err)
			writeError(res, http.StatusInternalServerError, "Could not save the new password")
		}
		return
	}
	message := "Dashboard password enabled"
	if body.Current != "" {
		message = "Dashboard password updated"
	}
	writeJSON(res, http.StatusOK, map[string]string{"message": message})
}

func (a *App) removePassword(res http.ResponseWriter, req *http.Request) {
	if !a.adminAuth(res, req) {
		return
	}
	var body struct {
		Current string `json:"currentPassword"`
	}
	if decodeJSON(req, &body) != nil {
		writeError(res, http.StatusBadRequest, "Invalid request")
		return
	}
	err := a.auth.ClearPassword(body.Current)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrNoPassword):
			writeError(res, http.StatusConflict, "No dashboard password is configured")
		case errors.Is(err, auth.ErrLastCredential):
			writeError(res, http.StatusConflict, "Add another sign-in method before removing the password")
		case errors.Is(err, auth.ErrWrongPassword):
			writeError(res, http.StatusUnauthorized, "Current password is incorrect")
		default:
			a.log.Warn("remove password", "error", err)
			writeError(res, http.StatusInternalServerError, "Could not remove the dashboard password")
		}
		return
	}
	writeJSON(res, http.StatusOK, map[string]string{"message": "Dashboard password removed"})
}

func (a *App) passkeyRegistrationStart(res http.ResponseWriter, req *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if decodeJSON(req, &body) != nil {
		writeError(res, http.StatusBadRequest, "Invalid request")
		return
	}
	options, cookie, err := a.auth.BeginRegistration(body.Name)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrSetupComplete):
			writeError(res, http.StatusConflict, "First-time setup is not complete")
		case errors.Is(err, auth.ErrInvalidName):
			writeError(res, http.StatusBadRequest, err.Error())
		default:
			a.log.Warn("begin passkey registration", "error", err)
			writeError(res, http.StatusInternalServerError, "Could not start passkey registration")
		}
		return
	}
	http.SetCookie(res, cookie)
	writeJSON(res, http.StatusOK, options)
}

func (a *App) passkeyRegistrationFinish(res http.ResponseWriter, req *http.Request) {
	if err := a.auth.FinishRegistration(req); err != nil {
		a.log.Warn("finish passkey registration", "error", err)
		writeError(res, http.StatusBadRequest, "Passkey registration failed. Start again and complete the browser prompt.")
		return
	}
	writeJSON(res, http.StatusCreated, map[string]string{"message": "Passkey added"})
}

func (a *App) deletePasskey(res http.ResponseWriter, req *http.Request) {
	err := a.auth.DeletePasskey(req.PathValue("passkeyID"))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrLastCredential):
			writeError(res, http.StatusConflict, "Add another sign-in method before removing the last passkey")
		case errors.Is(err, auth.ErrPasskeyNotFound):
			writeError(res, http.StatusNotFound, "Passkey not found")
		default:
			a.log.Warn("delete passkey", "error", err)
			writeError(res, http.StatusInternalServerError, "Could not remove passkey")
		}
		return
	}
	writeJSON(res, http.StatusOK, map[string]string{"message": "Passkey removed"})
}

func (a *App) accessLogout(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	if !a.sameOrigin(req) {
		writeError(res, http.StatusForbidden, "Cross-origin request blocked")
		return
	}
	http.SetCookie(res, a.auth.Logout(req))
	writeJSON(res, http.StatusOK, map[string]bool{"authenticated": false})
}
func (a *App) adminPage(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	secureAccessPage(res)
	res.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(res, web.AdminHTML)
}
func (a *App) healthPage(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	res.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(res, web.HealthHTML)
}
func (a *App) healthRoot(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	if strings.Contains(req.Header.Get("Accept"), "text/html") && req.URL.Query().Get("format") != "json" {
		a.healthPage(res, req)
		return
	}
	writeJSON(res, 200, a.healthPayload())
}
func (a *App) healthData(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	writeJSON(res, 200, a.healthPayload())
}

func (a *App) healthPayload() map[string]any {
	payload := a.health.Snapshot()
	updated := time.Now().Add(-time.Duration(payload["uptime"].(float64) * float64(time.Second)))
	if parsed, err := time.Parse(time.RFC3339Nano, os.Getenv("BUILD_DATE")); err == nil {
		updated = parsed
	}
	seconds := int(time.Since(updated).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	type unit struct {
		name string
		size int
	}
	units := []unit{{"day", 86400}, {"hour", 3600}, {"minute", 60}, {"second", 1}}
	label := "0 seconds ago"
	for _, candidate := range units {
		if seconds >= candidate.size || candidate.size == 1 {
			value := seconds / candidate.size
			suffix := ""
			if value != 1 {
				suffix = "s"
			}
			label = fmt.Sprintf("%d %s%s ago", value, candidate.name, suffix)
			break
		}
	}
	payload["lastUpdate"] = label
	return payload
}
func (a *App) healthPing(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	started := time.Now()
	duration := float64(time.Since(started).Microseconds()) / 1000
	res.Header().Set("Server-Timing", fmt.Sprintf("app;dur=%.3f", duration))
	writeJSON(res, 200, map[string]any{"status": "ok", "serverTime": time.Now().UTC().Format(time.RFC3339Nano), "handlerDurationMs": duration})
}
func (a *App) healthProbe(res http.ResponseWriter, req *http.Request) {
	noCache(res)
	bytes, _ := strconv.Atoi(req.URL.Query().Get("bytes"))
	if bytes == 0 {
		bytes = 128 << 10
	}
	bytes = max(1024, min(256<<10, bytes))
	res.Header().Set("Content-Type", "application/octet-stream")
	res.Header().Set("Content-Length", strconv.Itoa(bytes))
	res.Header().Set("X-Probe-Bytes", strconv.Itoa(bytes))
	_, _ = io.CopyN(res, strings.NewReader(strings.Repeat("transcript-health-probe\n", (bytes/23)+2)), int64(bytes))
}

var _ = url.QueryEscape
