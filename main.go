package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
	"google.golang.org/api/idtoken"
	_ "modernc.org/sqlite"
)

//go:embed plan.json
var planJSON []byte

//go:embed schema.sql
var schemaSQL string

type server struct {
	db           *sql.DB
	clientID     string
	secret       []byte
	verifyGoogle func(context.Context, string, string) (*idtoken.Payload, error)
	plan         json.RawMessage
	planVersion  string
}

type user struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}
type progress struct {
	Day       int   `json:"dayOfYear"`
	Completed bool  `json:"completed"`
	UpdatedAt int64 `json:"updatedAt"`
}
type progressResponse struct {
	ServerTime int64      `json:"serverTime"`
	Items      []progress `json:"items"`
}

func main() {
	clientID, secret := os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("API_TOKEN_SECRET")
	if clientID == "" || len(secret) < 32 {
		log.Fatal("GOOGLE_CLIENT_ID and API_TOKEN_SECRET (at least 32 bytes) are required")
	}
	db, err := openDB()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	s := newServer(db, clientID, []byte(secret))
	port := os.Getenv("PORT")
	if port == "" {
		port = "8494"
	}
	log.Fatal(http.ListenAndServe(":"+port, s.routes()))
}

func openDB() (*sql.DB, error) {
	u, token := os.Getenv("TURSO_DATABASE_URL"), os.Getenv("TURSO_AUTH_TOKEN")
	if (u == "") != (token == "") {
		return nil, errors.New("TURSO_DATABASE_URL and TURSO_AUTH_TOKEN must both be set")
	}
	if u != "" {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "libsql" && parsed.Scheme != "https" && parsed.Scheme != "wss") {
			return nil, errors.New("invalid TURSO_DATABASE_URL")
		}
		q := parsed.Query()
		q.Set("authToken", token)
		parsed.RawQuery = q.Encode()
		return sql.Open("libsql", parsed.String())
	}
	path := os.Getenv("DATABASE_PATH")
	if path == "" {
		path = "bible-tracker.db"
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Only the local SQLite file is bootstrapped, using the website's unchanged schema.
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func newServer(db *sql.DB, clientID string, secret []byte) *server {
	h := sha256.Sum256(planJSON)
	return &server{db: db, clientID: clientID, secret: secret, verifyGoogle: idtoken.Validate, plan: planJSON, planVersion: hex.EncodeToString(h[:])}
}

func (s *server) routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		if s.db.PingContext(r.Context()) != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.WriteHeader(204)
	})
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/plan", s.getPlan)
		r.Post("/auth/google", s.google)
		r.Post("/auth/refresh", s.refresh)
		r.Group(func(r chi.Router) {
			r.Use(s.authenticate)
			r.Get("/progress", s.getProgress)
			r.Post("/progress", s.pushProgress)
		})
	})
	return r
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	d := json.NewDecoder(io.LimitReader(r.Body, 128<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		http.Error(w, "invalid JSON", 400)
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid JSON", 400)
		return false
	}
	return true
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, err error) {
	log.Printf("request failed: %v", err)
	http.Error(w, "internal error", 500)
}
func now() int64 { return time.Now().UnixMilli() }

func (s *server) getPlan(w http.ResponseWriter, r *http.Request) {
	etag := `"` + s.planVersion + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	respond(w, struct {
		Version string          `json:"version"`
		Days    json.RawMessage `json:"days"`
	}{s.planVersion, s.plan})
}

func (s *server) google(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDToken string `json:"idToken"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.IDToken == "" {
		http.Error(w, "idToken required", 400)
		return
	}
	p, err := s.verifyGoogle(r.Context(), req.IDToken, s.clientID)
	if err != nil || p == nil || p.Subject == "" || (p.Issuer != "https://accounts.google.com" && p.Issuer != "accounts.google.com") || p.Expires <= time.Now().Unix() || p.Audience != s.clientID {
		http.Error(w, "invalid Google token", 401)
		return
	}
	email, _ := p.Claims["email"].(string)
	name, _ := p.Claims["name"].(string)
	var u user
	err = s.db.QueryRowContext(r.Context(), `INSERT INTO users (google_id,email,name) VALUES (?,?,?) ON CONFLICT(google_id) DO UPDATE SET email=excluded.email,name=excluded.name RETURNING id,email,name`, p.Subject, email, name).Scan(&u.ID, &u.Email, &u.Name)
	if err != nil {
		fail(w, err)
		return
	}
	s.issue(w, r, u)
}

type refreshClaims struct {
	jwt.RegisteredClaims
	Purpose string `json:"purpose"`
}

func (s *server) issue(w http.ResponseWriter, _ *http.Request, u user) {
	s.respondAuth(w, u, time.Now().Add(30*24*time.Hour))
}

func (s *server) respondAuth(w http.ResponseWriter, u user, refreshExpiry time.Time) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		fail(w, err)
		return
	}
	refresh, err := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "tuntasalkitab-api", Subject: strconv.FormatInt(u.ID, 10), Audience: jwt.ClaimStrings{"tuntasalkitab-refresh"}, ExpiresAt: jwt.NewNumericDate(refreshExpiry), ID: hex.EncodeToString(nonce)},
		Purpose:          "refresh",
	}).SignedString(s.secret)
	if err != nil {
		fail(w, err)
		return
	}
	expires := time.Now().Add(15 * time.Minute)
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{Issuer: "tuntasalkitab-api", Subject: strconv.FormatInt(u.ID, 10), Audience: jwt.ClaimStrings{"tuntasalkitab-android"}, ExpiresAt: jwt.NewNumericDate(expires), IssuedAt: jwt.NewNumericDate(time.Now())}).SignedString(s.secret)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, struct {
		Access  string `json:"accessToken"`
		Refresh string `json:"refreshToken"`
		Expires int64  `json:"expiresAt"`
		User    user   `json:"user"`
	}{access, refresh, expires.UnixMilli(), u})
}

func (s *server) refresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if !decode(w, r, &req) {
		return
	}
	claims := &refreshClaims{}
	_, err := jwt.ParseWithClaims(req.RefreshToken, claims, func(t *jwt.Token) (any, error) { return s.secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("tuntasalkitab-api"), jwt.WithAudience("tuntasalkitab-refresh"), jwt.WithExpirationRequired())
	if err != nil || claims.Purpose != "refresh" || claims.ExpiresAt == nil {
		http.Error(w, "invalid refresh token", 401)
		return
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid refresh token", 401)
		return
	}
	var u user
	u.ID = id
	if err = s.db.QueryRowContext(r.Context(), `SELECT COALESCE(email,''),COALESCE(name,'') FROM users WHERE id=?`, u.ID).Scan(&u.Email, &u.Name); errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "invalid refresh token", 401)
		return
	} else if err != nil {
		fail(w, err)
		return
	}
	s.respondAuth(w, u, claims.ExpiresAt.Time)
}

type userKey struct{}

func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			http.Error(w, "unauthorized", 401)
			return
		}
		claims := &jwt.RegisteredClaims{}
		_, err := jwt.ParseWithClaims(strings.TrimPrefix(header, "Bearer "), claims, func(t *jwt.Token) (any, error) { return s.secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("tuntasalkitab-api"), jwt.WithAudience("tuntasalkitab-android"), jwt.WithExpirationRequired())
		if err != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		id, err := strconv.ParseInt(claims.Subject, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "unauthorized", 401)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, id)))
	})
}

func (s *server) getProgress(w http.ResponseWriter, r *http.Request) {
	if q := r.URL.Query().Get("since"); q != "" && q != "0" {
		http.Error(w, "only full snapshots (since=0) are supported", 400)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT day_of_year,completed FROM reading_progress WHERE user_id=? ORDER BY day_of_year`, r.Context().Value(userKey{}))
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	items := []progress{}
	for rows.Next() {
		var item progress
		if err = rows.Scan(&item.Day, &item.Completed); err != nil {
			fail(w, err)
			return
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		fail(w, err)
		return
	}
	// The website clears completed_at on uncheck. No stored modification time exists;
	// the read time is a synthetic timestamp for the Android DTO, not a delta cursor.
	readTime := now()
	for i := range items {
		items[i].UpdatedAt = readTime
	}
	respond(w, progressResponse{readTime, items})
}

func (s *server) pushProgress(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items []progress `json:"items"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Items == nil || len(req.Items) > 400 {
		http.Error(w, "invalid items", 400)
		return
	}
	seen := map[int]bool{}
	for _, p := range req.Items {
		if p.Day < 1 || p.Day > 365 || p.UpdatedAt <= 0 || seen[p.Day] {
			http.Error(w, "invalid progress item", 400)
			return
		}
		seen[p.Day] = true
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	result := progressResponse{Items: make([]progress, 0, len(req.Items))}
	for _, p := range req.Items {
		var completedAt any
		if p.Completed {
			completedAt = time.Now().UTC().Format("2006-01-02 15:04:05")
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO reading_progress (user_id,day_of_year,completed,completed_at) VALUES (?,?,?,?)
		ON CONFLICT(user_id,day_of_year) DO UPDATE SET completed=excluded.completed,completed_at=excluded.completed_at`, r.Context().Value(userKey{}), p.Day, p.Completed, completedAt)
		if err != nil {
			fail(w, err)
			return
		}
		result.Items = append(result.Items, progress{Day: p.Day, Completed: p.Completed})
	}
	if err = tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	result.ServerTime = now()
	for i := range result.Items {
		result.Items[i].UpdatedAt = result.ServerTime
	}
	respond(w, result)
}
