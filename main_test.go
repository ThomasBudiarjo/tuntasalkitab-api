package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/api/idtoken"
)

func testServer(t *testing.T) (*server, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Simulate the current website's schema and data before API startup.
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (google_id,email,name) VALUES ('google-123','old@example.com','Old'),('another','other@example.com','Other')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO reading_progress(user_id,day_of_year,completed,completed_at) VALUES (1,12,1,'2025-02-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	s := newServer(db, "web-client", []byte("12345678901234567890123456789012"))
	s.verifyGoogle = func(_ context.Context, token, audience string) (*idtoken.Payload, error) {
		if token != "valid" || audience != "web-client" {
			return nil, sql.ErrNoRows
		}
		return &idtoken.Payload{Subject: "google-123", Issuer: "https://accounts.google.com", Audience: audience, Expires: time.Now().Add(time.Hour).Unix(), Claims: map[string]any{"email": "new@example.com", "name": "New"}}, nil
	}
	return s, db
}

func request(s *server, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

func TestAuthAndProgressSync(t *testing.T) {
	s, db := testServer(t)
	if w := request(s, "GET", "/api/v1/progress?since=0", "", ""); w.Code != 401 {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	if w := request(s, "POST", "/api/v1/auth/google", `{"idToken":"invalid"}`, ""); w.Code != 401 {
		t.Fatalf("invalid Google token: %d", w.Code)
	}
	w := request(s, "POST", "/api/v1/auth/google", `{"idToken":"valid"}`, "")
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		User         user   `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &auth); err != nil {
		t.Fatal(err)
	}
	if auth.User.ID != 1 || auth.User.Email != "new@example.com" {
		t.Fatalf("did not reuse website account: %+v", auth.User)
	}
	w = request(s, "GET", "/api/v1/progress?since=0", "", auth.AccessToken)
	var snapshot progressResponse
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 1 || snapshot.Items[0].Day != 12 || !snapshot.Items[0].Completed || snapshot.Items[0].UpdatedAt != snapshot.ServerTime {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	// With no modification column the device's pending intent wins when pushed,
	// even if its timestamp predates the existing website value.
	w = request(s, "POST", "/api/v1/progress", `{"items":[{"dayOfYear":12,"completed":false,"updatedAt":1738367999999},{"dayOfYear":25,"completed":true,"updatedAt":1738368000100}]}`, auth.AccessToken)
	if w.Code != 200 {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	var echo progressResponse
	json.Unmarshal(w.Body.Bytes(), &echo)
	if len(echo.Items) != 2 || echo.Items[0].Completed || !echo.Items[1].Completed || echo.Items[0].UpdatedAt != echo.ServerTime {
		t.Fatalf("push echo: %+v", echo)
	}
	var completed int
	var completedAt sql.NullString
	if err := db.QueryRow(`SELECT completed,completed_at FROM reading_progress WHERE user_id=1 AND day_of_year=12`).Scan(&completed, &completedAt); err != nil || completed != 0 || completedAt.Valid {
		t.Fatalf("uncheck not persisted: completed=%d at=%v err=%v", completed, completedAt, err)
	}
	if w := request(s, "GET", "/api/v1/progress?since=1", "", auth.AccessToken); w.Code != 400 {
		t.Fatalf("nonzero delta accepted: %d", w.Code)
	}
	// Web uncheck clears completed_at. The API must still return that false row.
	if _, err := db.Exec(`INSERT INTO reading_progress(user_id,day_of_year,completed,completed_at) VALUES(1,12,1,CURRENT_TIMESTAMP) ON CONFLICT(user_id,day_of_year) DO UPDATE SET completed=excluded.completed,completed_at=excluded.completed_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO reading_progress(user_id,day_of_year,completed,completed_at) VALUES(1,12,0,NULL) ON CONFLICT(user_id,day_of_year) DO UPDATE SET completed=excluded.completed,completed_at=excluded.completed_at`); err != nil {
		t.Fatal(err)
	}
	w = request(s, "GET", "/api/v1/progress?since=0", "", auth.AccessToken)
	json.Unmarshal(w.Body.Bytes(), &snapshot)
	if snapshot.Items[0].Completed || snapshot.Items[0].UpdatedAt <= 0 {
		t.Fatalf("web uncheck not synced: %s", w.Body)
	}
	// The only tables are the website's original tables (and SQLite sequence).
	var extra int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT IN ('users','reading_progress','sqlite_sequence')`).Scan(&extra); err != nil || extra != 0 {
		t.Fatalf("extra tables: %d %v", extra, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('reading_progress') WHERE name='updated_at'`).Scan(&extra); err != nil || extra != 0 {
		t.Fatalf("schema changed: %d %v", extra, err)
	}
	if _, err := db.Exec(`INSERT INTO reading_progress(user_id,day_of_year,completed,completed_at) VALUES(1,30,0,NULL)`); err != nil {
		t.Fatal(err)
	}
	w = request(s, "GET", "/api/v1/progress?since=0", "", auth.AccessToken)
	json.Unmarshal(w.Body.Bytes(), &snapshot)
	if len(snapshot.Items) != 3 || snapshot.Items[2].UpdatedAt <= 0 {
		t.Fatalf("web insert missing: %s", w.Body)
	}
	// No cross-account reads.
	if _, err := db.Exec(`INSERT INTO reading_progress(user_id,day_of_year,completed) VALUES(2,77,1)`); err != nil {
		t.Fatal(err)
	}
	w = request(s, "GET", "/api/v1/progress?since=0", "", auth.AccessToken)
	json.Unmarshal(w.Body.Bytes(), &snapshot)
	if len(snapshot.Items) != 3 {
		t.Fatalf("leaked other user: %s", w.Body)
	}
	w = request(s, "POST", "/api/v1/auth/refresh", `{"refreshToken":"`+auth.RefreshToken+`"}`, "")
	if w.Code != 200 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body)
	}
	var next struct {
		RefreshToken string `json:"refreshToken"`
	}
	json.Unmarshal(w.Body.Bytes(), &next)
	if next.RefreshToken == auth.RefreshToken || next.RefreshToken == "" {
		t.Fatal("refresh not rotated")
	}
	oldClaims, newClaims := &refreshClaims{}, &refreshClaims{}
	for _, pair := range []struct {
		token  string
		claims *refreshClaims
	}{{auth.RefreshToken, oldClaims}, {next.RefreshToken, newClaims}} {
		if _, err := jwt.ParseWithClaims(pair.token, pair.claims, func(*jwt.Token) (any, error) { return s.secret, nil }, jwt.WithValidMethods([]string{"HS256"})); err != nil {
			t.Fatal(err)
		}
	}
	if !oldClaims.ExpiresAt.Time.Equal(newClaims.ExpiresAt.Time) {
		t.Fatal("refresh extended original expiry")
	}
	// Stateless refresh tokens cannot be invalidated after use.
	if w := request(s, "POST", "/api/v1/auth/refresh", `{"refreshToken":"`+auth.RefreshToken+`"}`, ""); w.Code != 200 {
		t.Fatalf("stateless refresh replay: %d", w.Code)
	}
	if w := request(s, "POST", "/api/v1/auth/refresh", `{"refreshToken":"`+auth.AccessToken+`"}`, ""); w.Code != 401 {
		t.Fatalf("access token used for refresh: %d", w.Code)
	}
	if w := request(s, "GET", "/api/v1/progress?since=0", "", auth.RefreshToken); w.Code != 401 {
		t.Fatalf("refresh token used as access: %d", w.Code)
	}
}

func TestValidationAndPlan(t *testing.T) {
	s, _ := testServer(t)
	w := request(s, "POST", "/api/v1/auth/google", `{"idToken":"valid"}`, "")
	var auth struct {
		AccessToken string `json:"accessToken"`
	}
	json.Unmarshal(w.Body.Bytes(), &auth)
	for _, body := range []string{`{"items":[{"dayOfYear":366,"completed":true,"updatedAt":1}]}`, `{"items":[{"dayOfYear":1,"completed":true,"updatedAt":1},{"dayOfYear":1,"completed":false,"updatedAt":2}]}`, `{"items":[{"dayOfYear":1,"completed":true,"updatedAt":0}]}`} {
		if w := request(s, "POST", "/api/v1/progress", body, auth.AccessToken); w.Code != 400 {
			t.Fatalf("accepted %s: %d", body, w.Code)
		}
	}
	if w := request(s, "GET", "/api/v1/progress?since=-1", "", auth.AccessToken); w.Code != 400 {
		t.Fatalf("negative cursor: %d", w.Code)
	}
	w = request(s, "GET", "/api/v1/plan", "", "")
	if w.Code != 200 || w.Header().Get("ETag") == "" {
		t.Fatalf("plan: %d", w.Code)
	}
	var plan struct {
		Days map[string]string `json:"days"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Days) != 365 || plan.Days["1"] == "" {
		t.Fatalf("plan incomplete: %d", len(plan.Days))
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/plan", nil)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	cached := httptest.NewRecorder()
	s.routes().ServeHTTP(cached, r)
	if cached.Code != 304 || cached.Body.Len() != 0 {
		t.Fatalf("plan cache: %d %s", cached.Code, cached.Body)
	}
}

func TestLocalSQLiteBootstrap(t *testing.T) {
	t.Setenv("TURSO_DATABASE_URL", "")
	t.Setenv("TURSO_AUTH_TOKEN", "")
	t.Setenv("DATABASE_PATH", filepath.Join(t.TempDir(), "local.db"))
	for i := 0; i < 2; i++ {
		db, err := openDB()
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('users','reading_progress')`).Scan(&count); err != nil || count != 2 {
			t.Fatalf("original schema: count=%d err=%v", count, err)
		}
		db.Close()
	}
}
