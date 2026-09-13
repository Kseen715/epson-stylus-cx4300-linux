package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAuth(t *testing.T) *auth {
	t.Helper()
	a, err := newAuth("you", "secret", strings.Repeat("ab", 32), 15*time.Minute, 365*24*time.Hour)
	if err != nil {
		t.Fatalf("newAuth: %v", err)
	}
	return a
}

func TestNewAuthOffWhenUnset(t *testing.T) {
	a, err := newAuth("", "", "", time.Minute, time.Hour)
	if err != nil || a != nil {
		t.Fatalf("no credentials should mean no authentication, got %v, %v", a, err)
	}
}

func TestNewAuthRejectsHalfCredentials(t *testing.T) {
	if _, err := newAuth("you", "", "", time.Minute, time.Hour); err == nil {
		t.Fatal("a user with no password should be refused")
	}
	if _, err := newAuth("", "secret", "", time.Minute, time.Hour); err == nil {
		t.Fatal("a password with no user should be refused")
	}
}

func TestNewAuthRejectsWeakOrBadSecret(t *testing.T) {
	if _, err := newAuth("you", "secret", "not-hex", time.Minute, time.Hour); err == nil {
		t.Fatal("a non-hex jwt-secret should be refused")
	}
	if _, err := newAuth("you", "secret", "abcd", time.Minute, time.Hour); err == nil {
		t.Fatal("a 2-byte jwt-secret should be refused")
	}
}

func TestNewAuthRejectsRefreshShorterThanAccess(t *testing.T) {
	if _, err := newAuth("you", "secret", "", time.Hour, time.Minute); err == nil {
		t.Fatal("a refresh token shorter-lived than an access token should be refused")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	a := testAuth(t)
	now := time.Now()
	tok, err := a.mint(accessKind, a.ttl, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := a.verify(tok, accessKind, now); err != nil {
		t.Fatalf("a freshly minted token should verify: %v", err)
	}
}

func TestTokenExpires(t *testing.T) {
	a := testAuth(t)
	now := time.Now()
	tok, _ := a.mint(accessKind, a.ttl, now)
	if err := a.verify(tok, accessKind, now.Add(14*time.Minute)); err != nil {
		t.Fatalf("still inside 15m: %v", err)
	}
	if err := a.verify(tok, accessKind, now.Add(16*time.Minute)); err == nil {
		t.Fatal("a token past its 15 minutes should be refused")
	}
}

// A refresh token outlives the access token by a year and does much more when
// accepted, so presenting one as an access token must fail.
func TestRefreshTokenIsNotAnAccessToken(t *testing.T) {
	a := testAuth(t)
	now := time.Now()
	tok, _ := a.mint(refreshKind, a.refreshTTL, now)
	if err := a.verify(tok, accessKind, now); err == nil {
		t.Fatal("a refresh token should not pass as an access token")
	}
	if err := a.verify(tok, refreshKind, now.Add(300*24*time.Hour)); err != nil {
		t.Fatalf("a refresh token should last a year: %v", err)
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	a := testAuth(t)
	now := time.Now()
	tok, _ := a.mint(accessKind, a.ttl, now)

	// Re-signed by someone else's key.
	other, _ := newAuth("you", "secret", strings.Repeat("cd", 32), a.ttl, a.refreshTTL)
	forged, _ := other.mint(accessKind, a.ttl, now)
	if err := a.verify(forged, accessKind, now); err == nil {
		t.Fatal("a token signed with another key should be refused")
	}

	// Payload edited, signature left alone.
	parts := strings.Split(tok, ".")
	parts[1] = parts[1][:len(parts[1])-1] + "A"
	if err := a.verify(strings.Join(parts, "."), accessKind, now); err == nil {
		t.Fatal("an edited payload should be refused")
	}

	// The "alg":"none" trick: no signature at all.
	if err := a.verify(parts[0]+"."+parts[1]+".", accessKind, now); err == nil {
		t.Fatal("an unsigned token should be refused")
	}
	for _, bad := range []string{"", "x", "x.y", "not a token at all"} {
		if err := a.verify(bad, accessKind, now); err == nil {
			t.Fatalf("%q should be refused", bad)
		}
	}
}

func TestTokenIsForTheConfiguredUser(t *testing.T) {
	a := testAuth(t)
	now := time.Now()
	tok, _ := a.mint(accessKind, a.ttl, now)
	a.user = "someone-else"
	if err := a.verify(tok, accessKind, now); err == nil {
		t.Fatal("changing auth-user should invalidate existing tokens")
	}
}

func login(t *testing.T, a *auth, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader(`{"user":"` + user + `","password":"` + pass + `"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/login", body)
	w := httptest.NewRecorder()
	a.handleLogin(w, r)
	return w
}

func TestLogin(t *testing.T) {
	a := testAuth(t)

	if w := login(t, a, "you", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", w.Code)
	}
	if w := login(t, a, "nobody", "secret"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong user: got %d, want 401", w.Code)
	}

	w := login(t, a, "you", "secret")
	if w.Code != http.StatusOK {
		t.Fatalf("correct credentials: got %d, want 200", w.Code)
	}
	var got struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if err := a.verify(got.Token, accessKind, time.Now()); err != nil {
		t.Fatalf("the returned access token should verify: %v", err)
	}
	if err := a.verify(got.RefreshToken, refreshKind, time.Now()); err != nil {
		t.Fatalf("the returned refresh token should verify: %v", err)
	}
	if got.ExpiresIn != int(15*time.Minute/time.Second) {
		t.Fatalf("expiresIn = %d, want 900", got.ExpiresIn)
	}

	names := map[string]bool{}
	for _, c := range w.Result().Cookies() {
		names[c.Name] = true
		if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
			t.Fatalf("%s should be HttpOnly and SameSite=Strict, got %+v", c.Name, c)
		}
	}
	if !names[sessionCookie] || !names[refreshCookie] {
		t.Fatalf("both cookies should be set, got %v", names)
	}
}

// guarded returns the status code the guard produces for a request carrying the
// given cookies, and the response, so cookie renewal can be inspected.
func guarded(t *testing.T, a *auth, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	a.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(w, r)
	return w
}

func TestGuard(t *testing.T) {
	a := testAuth(t)
	now := time.Now()
	access, _ := a.mint(accessKind, a.ttl, now)

	if w := guarded(t, a, "/api/status"); w.Code != http.StatusUnauthorized {
		t.Fatalf("an API call with no token: got %d, want 401", w.Code)
	}
	if w := guarded(t, a, "/"); w.Code != http.StatusSeeOther {
		t.Fatalf("a page with no token: got %d, want 303 to the login page", w.Code)
	}
	if w := guarded(t, a, "/login"); w.Code != http.StatusOK {
		t.Fatalf("the login page must be reachable without a token: got %d", w.Code)
	}
	if w := guarded(t, a, "/api/login"); w.Code != http.StatusOK {
		t.Fatalf("the login endpoint must be reachable without a token: got %d", w.Code)
	}
	w := guarded(t, a, "/api/status", &http.Cookie{Name: sessionCookie, Value: access})
	if w.Code != http.StatusOK {
		t.Fatalf("a valid token: got %d, want 200", w.Code)
	}

	// The whole point of the refresh token: an expired access token is renewed
	// in passing rather than bouncing the user to the login page.
	stale, _ := a.mint(accessKind, a.ttl, now.Add(-time.Hour))
	refresh, _ := a.mint(refreshKind, a.refreshTTL, now)
	w = guarded(t, a, "/api/status",
		&http.Cookie{Name: sessionCookie, Value: stale},
		&http.Cookie{Name: refreshCookie, Value: refresh})
	if w.Code != http.StatusOK {
		t.Fatalf("a stale access token with a good refresh token: got %d, want 200", w.Code)
	}
	renewed := ""
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			renewed = c.Value
		}
	}
	if renewed == "" || renewed == stale {
		t.Fatal("the guard should have issued a fresh access token")
	}
	if err := a.verify(renewed, accessKind, now); err != nil {
		t.Fatalf("the renewed token should verify: %v", err)
	}

	// An expired refresh token renews nothing.
	oldRefresh, _ := a.mint(refreshKind, a.refreshTTL, now.Add(-400*24*time.Hour))
	w = guarded(t, a, "/api/status",
		&http.Cookie{Name: sessionCookie, Value: stale},
		&http.Cookie{Name: refreshCookie, Value: oldRefresh})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an expired refresh token: got %d, want 401", w.Code)
	}
}

func TestGuardAcceptsBearer(t *testing.T) {
	a := testAuth(t)
	access, _ := a.mint(accessKind, a.ttl, time.Now())
	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	r.Header.Set("Authorization", "Bearer "+access)
	w := httptest.NewRecorder()
	a.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a bearer token: got %d, want 200", w.Code)
	}
}

func TestRefreshEndpoint(t *testing.T) {
	a := testAuth(t)
	refresh, _ := a.mint(refreshKind, a.refreshTTL, time.Now())

	r := httptest.NewRequest(http.MethodPost, "/api/refresh", nil)
	r.Header.Set("Authorization", "Bearer "+refresh)
	w := httptest.NewRecorder()
	a.handleRefresh(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a good refresh token: got %d, want 200", w.Code)
	}
	var got struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if err := a.verify(got.Token, accessKind, time.Now()); err != nil {
		t.Fatalf("the refreshed access token should verify: %v", err)
	}

	// An access token is not a refresh token, however valid it is.
	access, _ := a.mint(accessKind, a.ttl, time.Now())
	r = httptest.NewRequest(http.MethodPost, "/api/refresh", nil)
	r.Header.Set("Authorization", "Bearer "+access)
	w = httptest.NewRecorder()
	a.handleRefresh(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an access token at /api/refresh: got %d, want 401", w.Code)
	}
}

func TestLogoutClearsBothCookies(t *testing.T) {
	a := testAuth(t)
	w := httptest.NewRecorder()
	a.handleLogout(w, httptest.NewRequest(http.MethodPost, "/api/logout", nil))
	cleared := 0
	for _, c := range w.Result().Cookies() {
		if c.Value == "" && c.MaxAge < 0 {
			cleared++
		}
	}
	if cleared != 2 {
		t.Fatalf("logout cleared %d cookies, want 2", cleared)
	}
}
