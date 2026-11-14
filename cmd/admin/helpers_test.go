package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/keystore"
	"github.com/fromScratchStudioGr/ostiary/internal/testutil"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// cachedSessionPrefix is how the cache package names the sessions it keeps per
// API key: this prefix, the key's hash, a colon and the model.
const cachedSessionPrefix = "ostiary:cache:"

// panel is the admin panel's dependencies, on the real Postgres and Redis,
// with one admin account to act as.
type panel struct {
	db     *gorm.DB
	cfg    config.Config
	cache  *cache.Cache
	redis  *redis.Client
	kms    keystore.KMSClient
	admin  models.User
	secret string
}

func newPanel(t *testing.T) *panel {
	t.Helper()
	db := testutil.Postgres(t)
	addr, raw := testutil.Redis(t)

	t.Setenv("LOCAL_KMS_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	kms, err := keystore.NewLocalKMSFromEnv()
	if err != nil {
		t.Fatalf("local KMS: %v", err)
	}
	c, err := cache.New(addr, "", "", 5, 0, db, kms)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	p := &panel{
		db:     db,
		cache:  c,
		redis:  raw,
		kms:    kms,
		secret: "panel-test-secret",
	}
	p.cfg = config.Config{JWTAuthentication: config.JWTAuthentication{Secret: p.secret, ExpirationInHours: 1}}
	p.admin = p.createUser(t, "root", "root-password", "active")
	return p
}

// createUser stores an admin account with the given password and status.
func (p *panel) createUser(t *testing.T, username, password, status string) models.User {
	t.Helper()
	// The cheapest cost: tests only compare against the hash.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hashing password: %v", err)
	}
	user := models.User{Username: username, Password: string(hash), Status: status}
	if err := p.db.Create(&user).Error; err != nil {
		t.Fatalf("creating user: %v", err)
	}
	return user
}

// sessionFor signs the session token the panel would have given this admin.
func (p *panel) sessionFor(t *testing.T, user models.User) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{
		UserID: user.ID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.Username,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString([]byte(p.secret))
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return token
}

// call sends a request to the handler as the panel's own admin. route is a
// mux pattern such as "DELETE /users/{id}", and target the URL requested.
func (p *panel) call(t *testing.T, handler http.HandlerFunc, route, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return p.callAs(t, p.admin, handler, route, target, body)
}

// callAs is call, as another admin.
func (p *panel) callAs(t *testing.T, user models.User, handler http.HandlerFunc, route, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(handler, route, target, body, &http.Cookie{Name: auth.SessionCookieName, Value: p.sessionFor(t, user)})
}

// serve sends a request through a mux, so the handler can read path values.
func serve(handler http.HandlerFunc, route, target, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	method, _, _ := strings.Cut(route, " ")
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(route, handler)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// breakTable drops a table the handler depends on, so its queries fail.
func (p *panel) breakTable(t *testing.T, table string) {
	t.Helper()
	if err := p.db.Exec("DROP TABLE " + table + " CASCADE").Error; err != nil {
		t.Fatalf("dropping %s: %v", table, err)
	}
}

// rejectWrites makes Postgres refuse the statements of the given kind (INSERT,
// UPDATE or DELETE) on the table's rows that match the condition, as a
// failing disk or a constraint would. Deleting a soft-deletable row is an
// UPDATE. The trigger goes away with the test's schema.
func (p *panel) rejectWrites(t *testing.T, table, kind, condition string) {
	t.Helper()
	row := "OLD"
	if kind == "INSERT" {
		row = "NEW"
	}
	statements := []string{
		`CREATE OR REPLACE FUNCTION reject_write() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'write rejected'; END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER reject_` + table + `_` + strings.ToLower(kind) + ` BEFORE ` + kind + ` ON ` + table + ` FOR EACH ROW WHEN (` + strings.ReplaceAll(condition, "ROW.", row+".") + `) EXECUTE FUNCTION reject_write()`,
	}
	for _, statement := range statements {
		if err := p.db.Exec(statement).Error; err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// captureLogs sends everything logged during the test to the returned buffer.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// fieldsOf lists a JSON object's field names, sorted.
func fieldsOf(object map[string]any) []string {
	fields := make([]string, 0, len(object))
	for name := range object {
		fields = append(fields, name)
	}
	slices.Sort(fields)
	return fields
}

// jsonObject reads the response as a JSON object, so its field names are what
// the client sees, not what a Go struct says they are.
func jsonObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &object); err != nil {
		t.Fatalf("the response is not a JSON object: %v (%d %q)", err, rec.Code, rec.Body.String())
	}
	return object
}

// jsonList reads the response as a JSON list of objects.
func jsonList(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("the response is not a JSON list of objects: %v (%d %q)", err, rec.Code, rec.Body.String())
	}
	return list
}

// entriesOf reads a field of the object as a list of objects.
func entriesOf(t *testing.T, object map[string]any, field string) []map[string]any {
	t.Helper()
	raw, ok := object[field].([]any)
	if !ok {
		t.Fatalf("%s = %v, want a list", field, object[field])
	}
	entries := make([]map[string]any, len(raw))
	for i, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s[%d] = %v, want an object", field, i, item)
		}
		entries[i] = entry
	}
	return entries
}

// wantFields fails unless the object has exactly these fields.
func wantFields(t *testing.T, object map[string]any, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := fieldsOf(object); !slices.Equal(got, want) {
		t.Fatalf("fields = %v, want exactly %v", got, want)
	}
}

// timeField reads a field as an RFC 3339 timestamp.
func timeField(t *testing.T, object map[string]any, field string) time.Time {
	t.Helper()
	text, _ := object[field].(string)
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("%s = %v, want an RFC 3339 timestamp: %v", field, object[field], err)
	}
	return parsed
}

// sameInstant says whether two times are the same moment, allowing for the
// precision Postgres keeps.
func sameInstant(a, b time.Time) bool {
	return a.Sub(b).Abs() <= time.Millisecond
}

// idOf formats a row id for a URL or a JSON body.
func idOf(id uint) string {
	return strconv.FormatUint(uint64(id), 10)
}

// errorOf decodes the error a response carries.
func errorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var got api.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not an error (%d): %q", rec.Code, rec.Body.String())
	}
	return got.Error
}

// wantError fails unless the response is this status with this message.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	if rec.Code != status || errorOf(t, rec) != message {
		t.Errorf("response = %d %q, want %d %q", rec.Code, rec.Body.String(), status, message)
	}
}

// wantValidationError fails unless the response is a 400 that names the field.
func wantValidationError(t *testing.T, rec *httptest.ResponseRecorder, field string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, rec), "'"+field+"'") {
		t.Errorf("response = %d %q, want a 400 about %s", rec.Code, rec.Body.String(), field)
	}
}

// cacheSession caches a session for the API key hash in Redis, and returns its
// key.
func (p *panel) cacheSession(t *testing.T, keyHash string) string {
	t.Helper()
	key := cachedSessionPrefix + keyHash + ":model"
	if err := p.redis.Set(context.Background(), key, "cached", time.Minute).Err(); err != nil {
		t.Fatalf("caching a session: %v", err)
	}
	t.Cleanup(func() { p.redis.Del(context.Background(), key) })
	return key
}

// isCached says whether the cached session is still in Redis.
func (p *panel) isCached(t *testing.T, key string) bool {
	t.Helper()
	n, err := p.redis.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("checking %s: %v", key, err)
	}
	return n == 1
}

// failQueriesSelecting makes the panel's database fail every query whose
// SELECT list contains the fragment, so a handler that runs several queries
// can be failed at a chosen one.
func (p *panel) failQueriesSelecting(t *testing.T, fragment string) {
	t.Helper()
	fail := func(tx *gorm.DB) {
		if strings.Contains(strings.Join(tx.Statement.Selects, " "), fragment) {
			tx.AddError(errors.New("query rejected"))
		}
	}
	name := "reject_" + testutil.Unique()
	if err := p.db.Callback().Query().Before("gorm:query").Register(name, fail); err != nil {
		t.Fatalf("registering the query callback: %v", err)
	}
	if err := p.db.Callback().Row().Before("gorm:row").Register(name, fail); err != nil {
		t.Fatalf("registering the row callback: %v", err)
	}
}

// createConsumer stores an active consumer.
func (p *panel) createConsumer(t *testing.T, name string) models.Consumer {
	t.Helper()
	consumer := models.Consumer{Name: name}
	if err := p.db.Create(&consumer).Error; err != nil {
		t.Fatalf("creating consumer: %v", err)
	}
	return consumer
}

// createAPIKey stores an active gateway API key for the consumer, under a
// hash of its own.
func (p *panel) createAPIKey(t *testing.T, consumer models.Consumer) models.APIKey {
	t.Helper()
	key := models.APIKey{
		ConsumerID:  consumer.ID,
		Name:        "key of " + consumer.Name,
		KeyHash:     testutil.Unique(),
		DisplayHint: "abcd",
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}
	if err := p.db.Create(&key).Error; err != nil {
		t.Fatalf("creating API key: %v", err)
	}
	return key
}

// createProviderKey stores an active provider key with the given display hint.
func (p *panel) createProviderKey(t *testing.T, provider, hint string) models.ProviderKey {
	t.Helper()
	key := models.ProviderKey{Provider: provider, DisplayHint: hint}
	if err := p.db.Create(&key).Error; err != nil {
		t.Fatalf("creating provider key: %v", err)
	}
	return key
}

// link gives the consumer access to the provider key.
func (p *panel) link(t *testing.T, consumer models.Consumer, key models.ProviderKey, usageLimit float64) models.UserProviderAccess {
	t.Helper()
	access := models.UserProviderAccess{ConsumerID: consumer.ID, ProviderKeyID: key.ID, UsageLimit: usageLimit}
	if err := p.db.Create(&access).Error; err != nil {
		t.Fatalf("linking consumer to provider key: %v", err)
	}
	return access
}

// deadCache is a cache whose Redis connection has been closed, so anything
// that has to reach Redis fails.
func (p *panel) deadCache(t *testing.T) *cache.Cache {
	t.Helper()
	addr, _ := testutil.Redis(t)
	c, err := cache.New(addr, "", "", 1, 0, p.db, p.kms)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	c.Close()
	return c
}

// decode reads the JSON a response carries.
func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var got T
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the response: %v (%d %q)", err, rec.Code, rec.Body.String())
	}
	return got
}

// failingKMS is a KMS whose wrapping always fails.
type failingKMS struct{}

func (failingKMS) Encrypt(context.Context, []byte) ([]byte, string, error) {
	return nil, "", errors.New("kms is down")
}

func (failingKMS) Decrypt(context.Context, []byte, string) ([]byte, error) {
	return nil, errors.New("kms is down")
}
