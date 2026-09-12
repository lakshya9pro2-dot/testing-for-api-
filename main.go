package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type ssoTicket struct {
	code        string
	userID      string
	email       string
	accessToken string
	returnURL   string
	expiresAt   time.Time
}

type App struct {
	db             *sql.DB
	supabaseURL    string
	publishableKey string
	baseURL        string
	httpClient     *http.Client
	ssoMutex       sync.RWMutex
	ssoTickets     map[string]*ssoTicket
}

type progressRequest struct {
	VideoID        int    `json:"video_id"`
	MediaType      string `json:"media_type"`
	TotalTimeMS    int64  `json:"total_time_ms"`
	PlaybackTimeMS int64  `json:"playback_time_ms"`
	SeasonNumber   int    `json:"season_number"`
	EpisodeNumber  int    `json:"episode_number"`
	DeviceHost     string `json:"device_host"`
}

type progressResponse struct {
	ID             string    `json:"id"`
	UserID         string    `json:"user_id"`
	VideoID        int       `json:"video_id"`
	MediaType      string    `json:"media_type"`
	TotalTimeMS    int64     `json:"total_time_ms"`
	PlaybackTimeMS int64     `json:"playback_time_ms"`
	SeasonNumber   int       `json:"season_number"`
	EpisodeNumber  int       `json:"episode_number"`
	DeviceHost     string    `json:"device_host,omitempty"`
	WatchedID      string    `json:"watched_id"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type authCredentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type supabaseUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func main() {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	supabaseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("SUPABASE_URL")), "/")
	if supabaseURL == "" {
		log.Fatal("SUPABASE_URL is required")
	}

	publishableKey := strings.TrimSpace(os.Getenv("SUPABASE_PUBLISHABLE_KEY"))
	if publishableKey == "" {
		log.Fatal("SUPABASE_PUBLISHABLE_KEY is required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}
	if _, err := strconv.Atoi(port); err != nil {
		log.Fatalf("invalid PORT %q: %v", port, err)
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("database ping: %v", err)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("API_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = "https://testing-for-api.onrender.com"
	}

	app := &App{
		db:             db,
		supabaseURL:    supabaseURL,
		publishableKey: publishableKey,
		baseURL:        baseURL,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		ssoTickets:     make(map[string]*ssoTicket),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", app.health)
	mux.HandleFunc("/api/auth/signup", app.signup)
	mux.HandleFunc("/api/auth/signin", app.signin)
	mux.HandleFunc("/api/auth/user", app.currentUser)
	mux.HandleFunc("/api/auth/google", app.googleLogin)
	mux.HandleFunc("/api/auth/callback", app.authCallback)
	mux.HandleFunc("/api/auth/callback/exchange", app.authCallbackExchange)
	mux.HandleFunc("/api/auth/session", app.sessionInfo)
	mux.HandleFunc("/api/auth/session/save", app.saveSession)
	mux.HandleFunc("/api/auth/sso", app.ssoRedirect)
	mux.HandleFunc("/api/auth/sso/ticket", app.ssoTicketHandler)
	mux.HandleFunc("/api/auth/sso/exchange", app.ssoExchange)
	mux.HandleFunc("/api/auth/logout", app.logout)
	mux.HandleFunc("/api/progress", app.progress)
	mux.HandleFunc("/api/progress/", app.progressByID)
	mux.HandleFunc("/", app.index)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           withCORS(withLogging(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("watch-progress API listening on :%s", port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "index.html")
}

func (a *App) signup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.handleAuth(w, r, "/auth/v1/signup")
}

func (a *App) signin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.handleAuth(w, r, "/auth/v1/token?grant_type=password")
}

func (a *App) handleAuth(w http.ResponseWriter, r *http.Request, path string) {
	var req authCredentials
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}
	if len(req.Password) < 6 {
		writeError(w, http.StatusBadRequest, "password must be at least 6 characters")
		return
	}

	body, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode request")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	reqURL := a.supabaseURL + path
	supaReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create auth request")
		return
	}
	supaReq.Header.Set("Content-Type", "application/json")
	supaReq.Header.Set("apikey", a.publishableKey)

	resp, err := a.httpClient.Do(supaReq)
	if err != nil {
		log.Printf("supabase auth request: %v", err)
		writeError(w, http.StatusBadGateway, "authentication service unavailable")
		return
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read authentication response")
		return
	}

	if resp.StatusCode == http.StatusOK {
		var tokenResp struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.Unmarshal(payload, &tokenResp); err == nil && tokenResp.AccessToken != "" {
			setSessionCookie(w, tokenResp.AccessToken)
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(payload)
}

func (a *App) currentUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user, err := a.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid or expired access token")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (a *App) authenticate(r *http.Request) (supabaseUser, error) {
	token := extractToken(r)
	if token == "" {
		return supabaseUser{}, errors.New("missing bearer token or session cookie")
	}
	return a.validateSupabaseToken(r.Context(), token)
}

func (a *App) validateSupabaseToken(ctx context.Context, token string) (supabaseUser, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, a.supabaseURL+"/auth/v1/user", nil)
	if err != nil {
		return supabaseUser{}, err
	}
	req.Header.Set("apikey", a.publishableKey)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return supabaseUser{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return supabaseUser{}, fmt.Errorf("supabase returned %s", resp.Status)
	}

	var user supabaseUser
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&user); err != nil {
		return supabaseUser{}, err
	}
	if user.ID == "" {
		return supabaseUser{}, errors.New("supabase user id missing")
	}
	return user, nil
}

func (a *App) googleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	returnURL := strings.TrimSpace(r.URL.Query().Get("return_url"))
	if returnURL == "" || !isAllowedRedirectURL(returnURL) {
		returnURL = "/"
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "kineflex_auth_return_url",
		Value:    returnURL,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	callbackURL := a.baseURL + "/api/auth/callback"
	authURL := fmt.Sprintf("%s/auth/v1/authorize?provider=google&redirect_to=%s", a.supabaseURL, url.QueryEscape(callbackURL))
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (a *App) authCallback(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code != "" {
		a.handlePKCECallback(w, r, code)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(authCallbackHTML))
}

func (a *App) authCallbackExchange(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		http.Redirect(w, r, "/?error=missing_code", http.StatusFound)
		return
	}
	a.handlePKCECallback(w, r, code)
}

func (a *App) handlePKCECallback(w http.ResponseWriter, r *http.Request, code string) {
	body, _ := json.Marshal(map[string]string{
		"auth_code": code,
	})
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	reqURL := fmt.Sprintf("%s/auth/v1/token?grant_type=pkce", a.supabaseURL)
	supaReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create auth request")
		return
	}
	supaReq.Header.Set("Content-Type", "application/json")
	supaReq.Header.Set("apikey", a.publishableKey)

	resp, err := a.httpClient.Do(supaReq)
	if err != nil || resp.StatusCode != http.StatusOK {
		http.Redirect(w, r, "/?error=oauth_failed", http.StatusFound)
		return
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken string       `json:"access_token"`
		User        supabaseUser `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil || tokenResp.AccessToken == "" {
		http.Redirect(w, r, "/?error=oauth_token_failed", http.StatusFound)
		return
	}

	setSessionCookie(w, tokenResp.AccessToken)

	returnURL := "/"
	if c, err := r.Cookie("kineflex_auth_return_url"); err == nil && c.Value != "" && isAllowedRedirectURL(c.Value) {
		returnURL = c.Value
		http.SetCookie(w, &http.Cookie{Name: "kineflex_auth_return_url", Value: "", Path: "/", MaxAge: -1})
	}

	if isExternalDomain(returnURL) {
		ssoCode := a.createSSOTicket(tokenResp.User.ID, tokenResp.User.Email, tokenResp.AccessToken, returnURL)
		http.Redirect(w, r, appendQueryParam(returnURL, "sso_code", ssoCode), http.StatusFound)
		return
	}

	http.Redirect(w, r, returnURL, http.StatusFound)
}

func (a *App) saveSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ReturnURL    string `json:"return_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	req.AccessToken = strings.TrimSpace(req.AccessToken)
	if req.AccessToken == "" {
		writeError(w, http.StatusBadRequest, "access_token is required")
		return
	}

	user, err := a.validateSupabaseToken(r.Context(), req.AccessToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid access token: "+err.Error())
		return
	}

	setSessionCookie(w, req.AccessToken)

	returnURL := strings.TrimSpace(req.ReturnURL)
	if returnURL == "" {
		if c, err := r.Cookie("kineflex_auth_return_url"); err == nil && c.Value != "" {
			returnURL = c.Value
			http.SetCookie(w, &http.Cookie{Name: "kineflex_auth_return_url", Value: "", Path: "/", MaxAge: -1})
		}
	}
	if returnURL == "" || !isAllowedRedirectURL(returnURL) {
		returnURL = "/"
	}

	var redirectURL string
	if isExternalDomain(returnURL) {
		ssoCode := a.createSSOTicket(user.ID, user.Email, req.AccessToken, returnURL)
		redirectURL = appendQueryParam(returnURL, "sso_code", ssoCode)
	} else {
		redirectURL = returnURL
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"redirect_url": redirectURL,
		"access_token": req.AccessToken,
		"user":         user,
	})
}

func (a *App) sessionInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	token := extractToken(r)
	if token == "" {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}

	user, err := a.validateSupabaseToken(r.Context(), token)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          user,
	})
}

func (a *App) ssoRedirect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	returnURL := strings.TrimSpace(r.URL.Query().Get("return_url"))
	if returnURL == "" || !isAllowedRedirectURL(returnURL) {
		returnURL = "https://testingplaty.netlify.app/"
	}

	token := extractToken(r)
	if token != "" {
		user, err := a.validateSupabaseToken(r.Context(), token)
		if err == nil && user.ID != "" {
			code := a.createSSOTicket(user.ID, user.Email, token, returnURL)
			redirectURL := appendQueryParam(returnURL, "sso_code", code)
			http.Redirect(w, r, redirectURL, http.StatusFound)
			return
		}
	}

	loginURL := fmt.Sprintf("/api/auth/google?return_url=%s", url.QueryEscape(returnURL))
	http.Redirect(w, r, loginURL, http.StatusFound)
}

func (a *App) ssoTicketHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	token := extractToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	user, err := a.validateSupabaseToken(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session: "+err.Error())
		return
	}

	var req struct {
		ReturnURL string `json:"return_url"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req)

	code := a.createSSOTicket(user.ID, user.Email, token, req.ReturnURL)

	writeJSON(w, http.StatusOK, map[string]any{
		"code":       code,
		"expires_in": 60,
	})
}

func (a *App) ssoExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	a.ssoMutex.Lock()
	ticket, exists := a.ssoTickets[code]
	if exists {
		delete(a.ssoTickets, code)
	}
	a.ssoMutex.Unlock()

	if !exists || time.Now().After(ticket.expiresAt) {
		writeError(w, http.StatusBadRequest, "invalid or expired SSO code")
		return
	}

	setSessionCookie(w, ticket.accessToken)

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": ticket.accessToken,
		"user": map[string]string{
			"id":    ticket.userID,
			"email": ticket.email,
		},
	})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": "logged out",
	})
}

func (a *App) createSSOTicket(userID, email, token, returnURL string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	code := hex.EncodeToString(b)

	a.ssoMutex.Lock()
	defer a.ssoMutex.Unlock()

	now := time.Now()
	for k, v := range a.ssoTickets {
		if now.After(v.expiresAt) {
			delete(a.ssoTickets, k)
		}
	}

	a.ssoTickets[code] = &ssoTicket{
		code:        code,
		userID:      userID,
		email:       email,
		accessToken: token,
		returnURL:   returnURL,
		expiresAt:   now.Add(60 * time.Second),
	}
	return code
}

func (a *App) progress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	user, err := a.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req progressRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if err := validateProgress(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	watchedID := makeWatchedID(req.MediaType, req.VideoID)

	const query = `
        insert into public.watch_progress (
            user_id, video_id, media_type, total_time_ms, playback_time_ms,
            season_number, episode_number, device_host, watched_id
        )
        values ($1,$2,$3,$4,$5,$6,$7,$8,$9)
        on conflict (user_id, watched_id)
        do update set
            video_id = excluded.video_id,
            media_type = excluded.media_type,
            total_time_ms = excluded.total_time_ms,
            playback_time_ms = excluded.playback_time_ms,
            season_number = excluded.season_number,
            episode_number = excluded.episode_number,
            device_host = excluded.device_host,
            updated_at = now()
        returning id,user_id,video_id,media_type,total_time_ms,playback_time_ms,
                  season_number,episode_number,device_host,watched_id,created_at,updated_at
    `

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	var out progressResponse
	err = a.db.QueryRowContext(
		ctx,
		query,
		user.ID,
		req.VideoID,
		req.MediaType,
		req.TotalTimeMS,
		req.PlaybackTimeMS,
		req.SeasonNumber,
		req.EpisodeNumber,
		nullableString(req.DeviceHost),
		watchedID,
	).Scan(
		&out.ID,
		&out.UserID,
		&out.VideoID,
		&out.MediaType,
		&out.TotalTimeMS,
		&out.PlaybackTimeMS,
		&out.SeasonNumber,
		&out.EpisodeNumber,
		&out.DeviceHost,
		&out.WatchedID,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if err != nil {
		log.Printf("upsert progress: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to save progress")
		return
	}

	writeJSON(w, http.StatusOK, out)
}

func (a *App) progressByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	user, err := a.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/progress/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeError(w, http.StatusBadRequest, "path must be /api/progress/{user_id}/{watched_id}")
		return
	}

	if parts[0] != user.ID {
		writeError(w, http.StatusForbidden, "user_id does not match the authenticated account")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	const query = `
        select id,user_id,video_id,media_type,total_time_ms,playback_time_ms,
               season_number,episode_number,device_host,watched_id,created_at,updated_at
        from public.watch_progress
        where user_id = $1 and watched_id = $2
    `

	var out progressResponse
	err = a.db.QueryRowContext(ctx, query, user.ID, parts[1]).Scan(
		&out.ID,
		&out.UserID,
		&out.VideoID,
		&out.MediaType,
		&out.TotalTimeMS,
		&out.PlaybackTimeMS,
		&out.SeasonNumber,
		&out.EpisodeNumber,
		&out.DeviceHost,
		&out.WatchedID,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "progress not found")
		return
	}
	if err != nil {
		log.Printf("get progress: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load progress")
		return
	}

	writeJSON(w, http.StatusOK, out)
}

func validateProgress(req progressRequest) error {
	if req.VideoID <= 0 {
		return errors.New("video_id must be greater than 0")
	}
	if req.MediaType != "tv" && req.MediaType != "movies" {
		return errors.New(`media_type must be exactly "tv" or "movies"`)
	}
	if req.TotalTimeMS < 0 || req.PlaybackTimeMS < 0 {
		return errors.New("time values cannot be negative")
	}
	if req.PlaybackTimeMS > req.TotalTimeMS && req.TotalTimeMS > 0 {
		return errors.New("playback_time_ms cannot exceed total_time_ms")
	}
	if req.SeasonNumber < 0 || req.EpisodeNumber < 0 {
		return errors.New("season_number and episode_number cannot be negative")
	}
	return nil
}

func makeWatchedID(mediaType string, videoID int) string {
	if mediaType == "tv" {
		return fmt.Sprintf("tv%d", videoID)
	}
	return fmt.Sprintf("m%d", videoID)
}

func nullableString(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message, "status": status})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func extractToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth != "" && strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		token := strings.TrimSpace(auth[len("Bearer "):])
		if token != "" {
			return token
		}
	}
	if cookie, err := r.Cookie("kineflex_session"); err == nil && cookie != nil {
		val := strings.TrimSpace(cookie.Value)
		if val != "" {
			return val
		}
	}
	return ""
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "kineflex_session",
		Value:    token,
		Path:     "/",
		MaxAge:   30 * 24 * 60 * 60, // 30 days
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "kineflex_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func isAllowedRedirectURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return false
	}
	if strings.HasPrefix(rawURL, "/") && !strings.HasPrefix(rawURL, "//") {
		return true
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	allowed := []string{
		"testingplaty.netlify.app",
		"testing-for-api.onrender.com",
		"kinflexbackend.onrender.com",
		"kineflex.site",
		"localhost",
		"127.0.0.1",
	}
	for _, host := range allowed {
		if hostname == host || strings.HasSuffix(hostname, "."+host) {
			return true
		}
	}
	return false
}

func isExternalDomain(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if strings.HasPrefix(rawURL, "/") && !strings.HasPrefix(rawURL, "//") {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	return hostname != "" && !strings.Contains(hostname, "onrender.com") && hostname != "localhost" && hostname != "127.0.0.1"
}

func appendQueryParam(rawURL, key, value string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		sep := "?"
		if strings.Contains(rawURL, "?") {
			sep = "&"
		}
		return rawURL + sep + url.QueryEscape(key) + "=" + url.QueryEscape(value)
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

const authCallbackHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>Kineflex – Authenticating</title>
  <style>
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      min-height: 100vh;
      background: #0a0a14;
      color: #fff;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
    }
    .spinner {
      width: 44px;
      height: 44px;
      border: 3px solid rgba(255,255,255,.1);
      border-top-color: #e8a020;
      border-radius: 50%;
      animation: spin 0.8s linear infinite;
      margin-bottom: 1rem;
    }
    @keyframes spin { to { transform: rotate(360deg); } }
    .msg { font-size: 1rem; color: rgba(255,255,255,.8); }
    .err { color: #f87171; font-size: 0.95rem; margin-top: 0.5rem; max-width: 400px; text-align: center; }
  </style>
</head>
<body>
  <div class="spinner"></div>
  <div class="msg" id="statusMsg">Completing authentication...</div>
  <div class="err" id="errorMsg"></div>

  <script>
    (function () {
      const statusEl = document.getElementById('statusMsg');
      const errorEl = document.getElementById('errorMsg');

      let rawHash = window.location.hash;
      if (rawHash.startsWith('#')) rawHash = rawHash.substring(1);
      const hashParams = new URLSearchParams(rawHash);
      const queryParams = new URLSearchParams(window.location.search);

      const accessToken = hashParams.get('access_token') || queryParams.get('access_token');
      const refreshToken = hashParams.get('refresh_token') || queryParams.get('refresh_token');
      const code = queryParams.get('code') || hashParams.get('code');
      const errorDesc = hashParams.get('error_description') || queryParams.get('error_description') || queryParams.get('error');

      if (errorDesc) {
        statusEl.innerText = "Authentication failed";
        errorEl.innerText = errorDesc;
        setTimeout(() => { window.location.href = '/'; }, 3000);
        return;
      }

      if (accessToken) {
        fetch('/api/auth/session/save', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ access_token: accessToken, refresh_token: refreshToken })
        })
        .then(res => res.json())
        .then(data => {
          if (data && data.redirect_url) {
            window.location.href = data.redirect_url;
          } else {
            window.location.href = '/';
          }
        })
        .catch(err => {
          console.error(err);
          statusEl.innerText = "Error completing sign-in";
          errorEl.innerText = "Please try logging in again.";
          setTimeout(() => { window.location.href = '/'; }, 3000);
        });
        return;
      }

      if (code) {
        window.location.href = '/api/auth/callback/exchange?code=' + encodeURIComponent(code);
        return;
      }

      statusEl.innerText = "No credentials received";
      setTimeout(() => { window.location.href = '/'; }, 2000);
    })();
  </script>
</body>
</html>`
