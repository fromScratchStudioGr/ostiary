package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"

	"golang.org/x/crypto/bcrypt"
)

func TestRequireActiveUser(t *testing.T) {
	p := newPanel(t)
	revoked := p.createUser(t, "revoked-rita", "password", "revoked")
	removed := p.createUser(t, "removed-ray", "password", "active")
	if err := p.db.Delete(&removed).Error; err != nil {
		t.Fatal(err)
	}

	var reached bool
	handler := adminOnly(p.cfg, p.db, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	})

	tests := []struct {
		name    string
		user    models.User
		status  int
		message string
		reached bool
	}{
		{"lets an active admin through", p.admin, http.StatusNoContent, "", true},
		{"turns away an admin whose access was revoked", revoked, http.StatusForbidden, "User access revoked", false},
		{"turns away an admin deleted after getting a session", removed, http.StatusForbidden, "Forbidden", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached = false

			rec := p.callAs(t, tt.user, handler, "GET /anything", "/anything", "")

			if tt.message == "" {
				if rec.Code != tt.status {
					t.Errorf("status = %d, want %d", rec.Code, tt.status)
				}
			} else {
				wantError(t, rec, tt.status, tt.message)
			}
			if reached != tt.reached {
				t.Errorf("handler reached = %v, want %v", reached, tt.reached)
			}
		})
	}

	t.Run("turns away a  request that carriesno session claims", func(t *testing.T) {
		reached = false
		unwrapped := requireActiveUser(p.db, func(w http.ResponseWriter, r *http.Request) { reached = true })

		rec := serve(unwrapped, "GET /anything", "/anything", "", nil)

		wantError(t, rec, http.StatusForbidden, "Forbidden")
		if reached {
			t.Error("the handler ran without any claims")
		}
	})

	t.Run("does not run the handler without a session at all", func(t *testing.T) {
		reached = false

		rec := serve(handler, "GET /anything", "/anything", "", nil)

		wantError(t, rec, http.StatusUnauthorized, "Missing access token")
		if reached {
			t.Error("the handler ran without a session")
		}
	})
}

func TestMeHandler(t *testing.T) {
	p := newPanel(t)
	alice := p.createUser(t, "alice", "password", "active")

	rec := p.callAs(t, alice, meHandler(p.cfg, p.db), "GET /me", "/me", "")

	var me api.MeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decoding the response: %v (%q)", err, rec.Body.String())
	}
	if rec.Code != http.StatusOK || me.Username != "alice" {
		t.Errorf("GET /me = %d %+v, want 200 for alice", rec.Code, me)
	}
	if got := rec.Result().Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	t.Run("answers with exactly the username, under its JSON name", func(t *testing.T) {
		object := jsonObject(t, rec)

		wantFields(t, object, "username")
		if object["username"] != "alice" {
			t.Errorf("username = %v, want alice", object["username"])
		}
	})
}

// strongPassword passes the password rule, for requests that need to get past it.
const strongPassword = "Str0ng!pw"

func TestPasswordProblem(t *testing.T) {
	tests := []struct {
		password string
		want     string
	}{
		{"Str0ng!pw", ""},
		{"Str0ng pw", ""},
		{"Str0ngépw", ""},
		{"Sh0rt!p", "Password must be at least 8 characters"},
		{"str0ng!pw", "Password must include an uppercase letter"},
		{"STR0NG!PW", "Password must include a lowercase letter"},
		{"Strong!pw", "Password must include a number"},
		{"Str0ngXpw", "Password must include a symbol"},
	}
	for _, tt := range tests {
		if got := passwordProblem(tt.password); got != tt.want {
			t.Errorf("passwordProblem(%q) = %q, want %q", tt.password, got, tt.want)
		}
	}
}

func TestRegisterHandler(t *testing.T) {
	p := newPanel(t)
	handler := registerHandler(p.cfg, p.db)
	register := func(body string) *httptest.ResponseRecorder {
		return p.call(t, handler, "POST /register", "/register", body)
	}
	userCount := func() int64 {
		var n int64
		p.db.Model(&models.User{}).Count(&n)
		return n
	}

	t.Run("creates an active admin whose password is stored hashed", func(t *testing.T) {
		rec := register(`{"username":"bob","password":"` + strongPassword + `","password_confirm":"` + strongPassword + `"}`)

		if rec.Code != http.StatusCreated || rec.Body.String() != "Account created for bob" {
			t.Fatalf("response = %d %q, want 201 \"Account created for bob\"", rec.Code, rec.Body.String())
		}
		var bob models.User
		if err := p.db.Where("username = ?", "bob").First(&bob).Error; err != nil {
			t.Fatalf("bob was not stored: %v", err)
		}
		if bob.Status != "active" {
			t.Errorf("status = %q, want active", bob.Status)
		}
		if bob.Password == strongPassword {
			t.Fatal("the password was stored in plain text")
		}
		if err := bcrypt.CompareHashAndPassword([]byte(bob.Password), []byte(strongPassword)); err != nil {
			t.Errorf("stored password is not a hash of the one given: %v", err)
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		before := userCount()
		tests := []struct {
			name string
			body string
			want func(t *testing.T, rec *httptest.ResponseRecorder)
		}{
			{"not JSON", `nope`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusBadRequest, "Invalid payload")
			}},
			{"no username", `{"password":"x","password_confirm":"x"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Username")
			}},
			{"no password", `{"username":"carl","password_confirm":"x"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Password")
			}},
			{"no confirmation", `{"username":"carl","password":"x"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "PasswordConfirm")
			}},
			{"passwords that differ", `{"username":"carl","password":"one","password_confirm":"two"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusBadRequest, "Passwords do not match")
			}},
			{"a weak password", `{"username":"carl","password":"password","password_confirm":"password"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusBadRequest, "Password must include an uppercase letter")
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tt.want(t, register(tt.body))
			})
		}

		if got := userCount(); got != before {
			t.Errorf("%d users were created by rejected requests", got-before)
		}
	})

	t.Run("refuses a username that is taken, whether or not its owner is still around", func(t *testing.T) {
		gone := p.createUser(t, "gone-gary", "password", "active")
		if err := p.db.Delete(&gone).Error; err != nil {
			t.Fatal(err)
		}

		for _, username := range []string{"root", "gone-gary"} {
			rec := register(`{"username":"` + username + `","password":"` + strongPassword + `","password_confirm":"` + strongPassword + `"}`)

			wantError(t, rec, http.StatusConflict, "Could not create account")
		}
	})

	t.Run("reports a failure to store the account", func(t *testing.T) {
		tooLong := strings.Repeat("a", 65)

		rec := register(`{"username":"` + tooLong + `","password":"` + strongPassword + `","password_confirm":"` + strongPassword + `"}`)

		wantError(t, rec, http.StatusInternalServerError, "Could not create account")
	})

	t.Run("reports a password too long to hash", func(t *testing.T) {
		before := userCount()
		password := strongPassword + strings.Repeat("p", 73-len(strongPassword))

		rec := register(`{"username":"long-larry","password":"` + password + `","password_confirm":"` + password + `"}`)

		wantError(t, rec, http.StatusInternalServerError, "Could not hash password")
		if userCount() != before {
			t.Error("an account was created without a password hash")
		}
	})
}

func TestGetUsersHandler(t *testing.T) {
	p := newPanel(t)
	p.createUser(t, "revoked-rita", "password", "revoked")
	gone := p.createUser(t, "gone-gary", "password", "active")
	if err := p.db.Delete(&gone).Error; err != nil {
		t.Fatal(err)
	}

	rec := p.call(t, getUsersHandler(p.cfg, p.db), "GET /users", "/users", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var users []api.UserResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	got := map[string]string{}
	for _, u := range users {
		got[u.Username] = u.Status
		if u.ID == 0 || u.CreatedAt.IsZero() {
			t.Errorf("%s comes without an id or creation time: %+v", u.Username, u)
		}
	}
	want := map[string]string{"root": "active", "revoked-rita": "revoked"}
	if len(got) != len(want) || got["root"] != "active" || got["revoked-rita"] != "revoked" {
		t.Errorf("users = %v, want %v (revoked ones listed, deleted ones not)", got, want)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "password") {
		t.Errorf("the response mentions passwords: %s", rec.Body.String())
	}

	t.Run("answers with exactly the fields the client expects, with the stored values", func(t *testing.T) {
		var stored []models.User
		if err := p.db.Find(&stored).Error; err != nil {
			t.Fatal(err)
		}
		var objects []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &objects); err != nil {
			t.Fatalf("the response is not a JSON list of objects: %v (%q)", err, rec.Body.String())
		}
		if len(objects) != len(stored) {
			t.Fatalf("got %d users, want %d: %s", len(objects), len(stored), rec.Body.String())
		}

		byID := map[uint]map[string]any{}
		for _, object := range objects {
			if fields, want := fieldsOf(object), []string{"createdAt", "id", "status", "username"}; !slices.Equal(fields, want) {
				t.Fatalf("fields = %v, want exactly %v", fields, want)
			}
			id, _ := object["id"].(float64)
			byID[uint(id)] = object
		}
		for _, user := range stored {
			object := byID[user.ID]
			if object == nil {
				t.Fatalf("user %d is not in the response", user.ID)
			}
			createdAt, err := time.Parse(time.RFC3339, fmt.Sprint(object["createdAt"]))
			if err != nil {
				t.Fatalf("createdAt = %v, want an RFC 3339 timestamp: %v", object["createdAt"], err)
			}
			if object["username"] != user.Username || object["status"] != user.Status || createdAt.Sub(user.CreatedAt).Abs() > time.Millisecond {
				t.Errorf("user %d = %v, want username %q, status %q, created at %v", user.ID, object, user.Username, user.Status, user.CreatedAt)
			}
		}
	})
}

func TestUpdateUserPasswordHandler(t *testing.T) {
	p := newPanel(t)
	handler := updateUserPasswordHandler(p.cfg, p.db)
	alice := p.createUser(t, "alice", "old-password", "active")
	update := func(body string) *httptest.ResponseRecorder {
		return p.call(t, handler, "PATCH /users/password", "/users/password", body)
	}
	passwordOf := func(id uint) string {
		var u models.User
		if err := p.db.First(&u, id).Error; err != nil {
			t.Fatal(err)
		}
		return u.Password
	}

	t.Run("sets another admin's password, and only theirs", func(t *testing.T) {
		rootBefore := passwordOf(p.admin.ID)

		rec := update(`{"userId":` + strconv.Itoa(int(alice.ID)) + `,"password":"New-passw0rd","passwordConfirm":"New-passw0rd"}`)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		stored := passwordOf(alice.ID)
		if bcrypt.CompareHashAndPassword([]byte(stored), []byte("New-passw0rd")) != nil {
			t.Error("alice's password is not the new one")
		}
		if bcrypt.CompareHashAndPassword([]byte(stored), []byte("old-password")) == nil {
			t.Error("the old password still works")
		}
		if passwordOf(p.admin.ID) != rootBefore {
			t.Error("the caller's own password changed")
		}
	})

	t.Run("lets an admin set their own password", func(t *testing.T) {
		rec := update(`{"userId":` + strconv.Itoa(int(p.admin.ID)) + `,"password":"M1ne!pass","passwordConfirm":"M1ne!pass"}`)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if bcrypt.CompareHashAndPassword([]byte(passwordOf(p.admin.ID)), []byte("M1ne!pass")) != nil {
			t.Error("the caller's password is not the new one")
		}
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		before := passwordOf(alice.ID)
		id := strconv.Itoa(int(alice.ID))
		gone := p.createUser(t, "gone-gary", "password", "active")
		if err := p.db.Delete(&gone).Error; err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			name string
			body string
			want func(t *testing.T, rec *httptest.ResponseRecorder)
		}{
			{"not JSON", `nope`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusBadRequest, "Invalid payload")
			}},
			{"no user", `{"password":"x","passwordConfirm":"x"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "UserId")
			}},
			{"no password", `{"userId":` + id + `,"passwordConfirm":"x"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "Password")
			}},
			{"no confirmation", `{"userId":` + id + `,"password":"x"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantValidationError(t, rec, "PasswordConfirm")
			}},
			{"passwords that differ", `{"userId":` + id + `,"password":"one","passwordConfirm":"two"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusBadRequest, "Passwords do not match")
			}},
			{"a weak password", `{"userId":` + id + `,"password":"Passw0rd","passwordConfirm":"Passw0rd"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusBadRequest, "Password must include a symbol")
			}},
			{"a user that doesn't exist", `{"userId":999999,"password":"` + strongPassword + `","passwordConfirm":"` + strongPassword + `"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "User not found")
			}},
			{"a user that was deleted", `{"userId":` + strconv.Itoa(int(gone.ID)) + `,"password":"` + strongPassword + `","passwordConfirm":"` + strongPassword + `"}`, func(t *testing.T, rec *httptest.ResponseRecorder) {
				wantError(t, rec, http.StatusNotFound, "User not found")
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tt.want(t, update(tt.body))
			})
		}

		if passwordOf(alice.ID) != before {
			t.Error("a rejected request changed alice's password")
		}
	})

	t.Run("reports a password too long to hash, leaving the old one", func(t *testing.T) {
		before := passwordOf(alice.ID)
		password := strongPassword + strings.Repeat("p", 73-len(strongPassword))

		rec := update(`{"userId":` + strconv.Itoa(int(alice.ID)) + `,"password":"` + password + `","passwordConfirm":"` + password + `"}`)

		wantError(t, rec, http.StatusInternalServerError, "Could not hash password")
		if passwordOf(alice.ID) != before {
			t.Error("alice's password changed")
		}
	})

	t.Run("reports a failure to store the new password", func(t *testing.T) {
		victim := p.createUser(t, "victim-vic", "password", "active")
		p.rejectWrites(t, "users", "UPDATE", "ROW.id = "+strconv.Itoa(int(victim.ID)))

		rec := update(`{"userId":` + strconv.Itoa(int(victim.ID)) + `,"password":"` + strongPassword + `","passwordConfirm":"` + strongPassword + `"}`)

		wantError(t, rec, http.StatusInternalServerError, "Could not update password")
	})
}

func TestDeleteUserHandler(t *testing.T) {
	p := newPanel(t)
	handler := deleteUserHandler(p.cfg, p.db)
	remove := func(id string) *httptest.ResponseRecorder {
		return p.call(t, handler, "DELETE /users/{id}", "/users/"+id, "")
	}
	exists := func(id uint) bool {
		var n int64
		p.db.Model(&models.User{}).Where("id = ?", id).Count(&n)
		return n == 1
	}

	t.Run("deletes another admin, who then loses access", func(t *testing.T) {
		bob := p.createUser(t, "bob", "password", "active")
		bobSession := p.sessionFor(t, bob)

		rec := remove(strconv.Itoa(int(bob.ID)))

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
		}
		if exists(bob.ID) {
			t.Error("bob is still there")
		}
		if !exists(p.admin.ID) {
			t.Error("the caller was deleted too")
		}
		rec = serve(meHandler(p.cfg, p.db), "GET /me", "/me", "", &http.Cookie{Name: "session", Value: bobSession})
		wantError(t, rec, http.StatusForbidden, "Forbidden")
	})

	t.Run("turns the request down when it can't be used", func(t *testing.T) {
		gone := p.createUser(t, "gone-gary", "password", "active")
		if err := p.db.Delete(&gone).Error; err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			name    string
			id      string
			status  int
			message string
		}{
			{"an id that is not a number", "abc", http.StatusBadRequest, "Invalid user id"},
			{"a negative id", "-1", http.StatusBadRequest, "Invalid user id"},
			{"the caller's own account", strconv.Itoa(int(p.admin.ID)), http.StatusForbidden, "Cannot delete your own account"},
			{"a user that doesn't exist", "999999", http.StatusNotFound, "User not found"},
			{"a user that was already deleted", strconv.Itoa(int(gone.ID)), http.StatusNotFound, "User not found"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				wantError(t, remove(tt.id), tt.status, tt.message)
			})
		}

		if !exists(p.admin.ID) {
			t.Error("the caller's account is gone")
		}
	})

	t.Run("reports a failure to delete", func(t *testing.T) {
		victim := p.createUser(t, "victim-vic", "password", "active")
		p.rejectWrites(t, "users", "UPDATE", "ROW.id = "+strconv.Itoa(int(victim.ID)))

		rec := remove(strconv.Itoa(int(victim.ID)))

		wantError(t, rec, http.StatusInternalServerError, "Could not delete user")
		if !exists(victim.ID) {
			t.Error("the user is gone despite the error")
		}
	})
}
