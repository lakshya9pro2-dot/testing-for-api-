package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type App struct {
	db             *sql.DB
	supabaseURL    string
	publishableKey string
	httpClient     *http.Client
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

	app := &App{
		db:             db,
		supabaseURL:    supabaseURL,
		publishableKey: publishableKey,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", app.health)
	mux.HandleFunc("/api/auth/signup", app.signup)
	mux.HandleFunc("/api/auth/signin", app.signin)
	mux.HandleFunc("/api/auth/user", app.currentUser)
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
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" || !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return supabaseUser{}, errors.New("missing bearer token")
	}
	token := strings.TrimSpace(auth[len("Bearer "):])
	if token == "" {
		return supabaseUser{}, errors.New("empty bearer token")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.supabaseURL+"/auth/v1/user", nil)
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
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
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
