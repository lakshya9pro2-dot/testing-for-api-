package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type App struct {
	db *sql.DB
}

type progressRequest struct {
	UserID         string `json:"user_id"`
	VideoID        int    `json:"video_id"`
	MediaType      string `json:"media_type"`
	TotalTimeMS    int64  `json:"total_time_ms"`
	PlaybackTimeMS int64  `json:"playback_time_ms"`
	SeasonNumber   int    `json:"season_number"`
	EpisodeNumber  int    `json:"episode_number"`
	DeviceHost     string `json:"device_host"`
	WatchedID      string `json:"watched_id,omitempty"`
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

func main() {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
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

	// Small application-side pool: Render runs one persistent service and Supabase
	// handles the database side. These values avoid opening unnecessary connections.
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("database ping: %v", err)
	}

	app := &App{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", app.health)
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

func (a *App) progress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
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
        ) values ($1,$2,$3,$4,$5,$6,$7,$8,$9)
        on conflict (user_id, watched_id) do update set
            video_id = excluded.video_id,
            media_type = excluded.media_type,
            total_time_ms = excluded.total_time_ms,
            playback_time_ms = excluded.playback_time_ms,
            season_number = excluded.season_number,
            episode_number = excluded.episode_number,
            device_host = excluded.device_host,
            updated_at = now()
        returning id, user_id, video_id, media_type, total_time_ms, playback_time_ms,
                  season_number, episode_number, device_host, watched_id, created_at, updated_at`

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	var out progressResponse
	err := a.db.QueryRowContext(ctx, query,
		req.UserID,
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

	raw := strings.TrimPrefix(r.URL.Path, "/api/progress/")
	pathParts := strings.Split(raw, "/")
	if len(pathParts) != 2 || pathParts[0] == "" || pathParts[1] == "" {
		writeError(w, http.StatusBadRequest, "path must be /api/progress/{user_id}/{watched_id}")
		return
	}
	userID, watchedID := pathParts[0], pathParts[1]

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	const query = `
        select id, user_id, video_id, media_type, total_time_ms, playback_time_ms,
               season_number, episode_number, device_host, watched_id, created_at, updated_at
        from public.watch_progress
        where user_id = $1 and watched_id = $2`

	var out progressResponse
	err := a.db.QueryRowContext(ctx, query, userID, watchedID).Scan(
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
	if strings.TrimSpace(req.UserID) == "" {
		return errors.New("user_id is required")
	}
	if req.VideoID <= 0 {
		return errors.New("video_id must be greater than 0")
	}
	if req.MediaType != "tv" && req.MediaType != "movies" {
		return errors.New(`media_type must be exactly "tv" or "movies"`)
	}
	if req.TotalTimeMS < 0 || req.PlaybackTimeMS < 0 {
		return errors.New("time values cannot be negative")
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

func nullableString(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error":  message,
		"status": status,
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
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
