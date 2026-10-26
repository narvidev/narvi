package slackapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
)

// installationTeam is the workspace the fake auth.test below names as the
// bot token's own.
const installationTeam = "T0HOME"

// fakeSlackUsers is a fake Slack Web API answering users.info with user
// (Slack's user object) and auth.test with authTest, each after its own
// delay, recording every request's path, Authorization header and
// users.info user parameter.
type fakeSlackUsers struct {
	mu       sync.Mutex
	paths    []string
	auths    []string
	userIDs  []string
	user     map[string]any
	authTest map[string]any

	usersInfoDelay time.Duration
	authTestDelay  time.Duration
}

func newFakeSlackUsers(t *testing.T, user, authTest map[string]any) (*fakeSlackUsers, *slackapi.Client) {
	t.Helper()
	return newSlowFakeSlackUsers(t, user, authTest, 0, 0)
}

func newSlowFakeSlackUsers(t *testing.T, user, authTest map[string]any, usersInfoDelay, authTestDelay time.Duration) (*fakeSlackUsers, *slackapi.Client) {
	t.Helper()
	f := &fakeSlackUsers{user: user, authTest: authTest, usersInfoDelay: usersInfoDelay, authTestDelay: authTestDelay}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		if r.URL.Path == "/users.info" {
			f.userIDs = append(f.userIDs, r.URL.Query().Get("user"))
		}
		f.mu.Unlock()
		delay := f.usersInfoDelay
		if r.URL.Path == "/auth.test" {
			delay = f.authTestDelay
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users.info":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": f.user})
		case "/auth.test":
			_ = json.NewEncoder(w).Encode(f.authTest)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return f, slackapi.New(server.Client(), server.URL, "xoxb-test-token")
}

func (f *fakeSlackUsers) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if p == path {
			n++
		}
	}
	return n
}

// fullMember is users.info's user object for a full member of the
// installation's own workspace whose email Slack reports as confirmed.
// Every case below starts from it and changes one thing.
func fullMember() map[string]any {
	return map[string]any{
		"id":                  "U123",
		"team_id":             installationTeam,
		"deleted":             false,
		"is_bot":              false,
		"is_restricted":       false,
		"is_ultra_restricted": false,
		"is_email_confirmed":  true,
		"profile":             map[string]any{"email": "ada@example.com"},
	}
}

func memberWith(key string, value any) map[string]any {
	u := fullMember()
	u[key] = value
	return u
}

func memberWithout(keys ...string) map[string]any {
	u := fullMember()
	for _, k := range keys {
		delete(u, k)
	}
	return u
}

// TestGetUserEmail_Verified is the table test of §13.2's definition of a
// verified Slack email (slackapi.UserEmail.Verified): Slack reports it as
// confirmed, for a full member of this installation's own workspace. Each
// case runs the real decoder on a users.info payload, so an absent field
// reads exactly as Slack's leaving it out would.
func TestGetUserEmail_Verified(t *testing.T) {
	t.Parallel()

	homeTeam := map[string]any{"ok": true, "team_id": installationTeam}
	tests := []struct {
		name         string
		user         map[string]any
		authTest     map[string]any
		wantVerified bool
	}{
		{"full member, confirmed email", fullMember(), homeTeam, true},
		{"every exclusion flag absent", memberWithout("deleted", "is_bot", "is_restricted", "is_ultra_restricted", "is_stranger"), homeTeam, true},
		{"is_stranger false", memberWith("is_stranger", false), homeTeam, true},
		{"email not confirmed", memberWith("is_email_confirmed", false), homeTeam, false},
		{"is_email_confirmed absent", memberWithout("is_email_confirmed"), homeTeam, false},
		{"deleted", memberWith("deleted", true), homeTeam, false},
		{"bot", memberWith("is_bot", true), homeTeam, false},
		{"guest (restricted)", memberWith("is_restricted", true), homeTeam, false},
		{"single-channel guest (ultra restricted)", memberWith("is_ultra_restricted", true), homeTeam, false},
		{"stranger", memberWith("is_stranger", true), homeTeam, false},
		{"another team's member", memberWith("team_id", "T0OTHER"), homeTeam, false},
		{"team_id absent", memberWithout("team_id"), homeTeam, false},
		{"team_id empty", memberWith("team_id", ""), homeTeam, false},
		{"auth.test names no team", fullMember(), map[string]any{"ok": true}, false},
		{"no team on either side", memberWithout("team_id"), map[string]any{"ok": true}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, client := newFakeSlackUsers(t, tt.user, tt.authTest)

			got, ok, err := client.GetUserEmail(context.Background(), "U123")
			if err != nil {
				t.Fatalf("GetUserEmail() error = %v, want nil", err)
			}
			if !ok {
				t.Fatal("GetUserEmail() ok = false, want true (the member has an email)")
			}
			if got.Email != "ada@example.com" {
				t.Errorf("Email = %q, want %q", got.Email, "ada@example.com")
			}
			if v := got.Verified(); v != tt.wantVerified {
				t.Errorf("Verified() = %v, want %v (decoded %+v)", v, tt.wantVerified, got)
			}
		})
	}
}

// TestGetUserEmail_DecodesEveryField proves each users.info field lands in
// the UserEmail field named for it, and auth.test's team_id in
// InstallationTeamID.
func TestGetUserEmail_DecodesEveryField(t *testing.T) {
	t.Parallel()

	_, client := newFakeSlackUsers(t, map[string]any{
		"team_id":             "T0OTHER",
		"deleted":             true,
		"is_bot":              true,
		"is_restricted":       true,
		"is_ultra_restricted": true,
		"is_stranger":         true,
		"is_email_confirmed":  true,
		"profile":             map[string]any{"email": "ada@example.com"},
	}, map[string]any{"ok": true, "team_id": installationTeam})

	got, ok, err := client.GetUserEmail(context.Background(), "U123")
	if err != nil || !ok {
		t.Fatalf("GetUserEmail() = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	want := slackapi.UserEmail{
		Email:              "ada@example.com",
		EmailConfirmed:     true,
		Deleted:            true,
		Bot:                true,
		Restricted:         true,
		UltraRestricted:    true,
		Stranger:           true,
		TeamID:             "T0OTHER",
		InstallationTeamID: installationTeam,
	}
	if got != want {
		t.Errorf("GetUserEmail() = %+v, want %+v", got, want)
	}
}

func TestGetUserEmail_Success(t *testing.T) {
	t.Parallel()

	fake, client := newFakeSlackUsers(t, fullMember(), map[string]any{"ok": true, "team_id": installationTeam})

	user, ok, err := client.GetUserEmail(context.Background(), "U123")
	if err != nil {
		t.Fatalf("GetUserEmail() error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("GetUserEmail() ok = false, want true")
	}
	if user.Email != "ada@example.com" {
		t.Errorf("email = %q, want %q", user.Email, "ada@example.com")
	}

	if fake.count("/users.info") != 1 || fake.count("/auth.test") != 1 {
		t.Errorf("users.info calls = %d, auth.test calls = %d, want 1 each", fake.count("/users.info"), fake.count("/auth.test"))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for i, auth := range fake.auths {
		if auth != "Bearer xoxb-test-token" {
			t.Errorf("request %d Authorization header = %q, want %q", i, auth, "Bearer xoxb-test-token")
		}
	}
	if len(fake.userIDs) != 1 || fake.userIDs[0] != "U123" {
		t.Errorf("users.info user params = %v, want [U123]", fake.userIDs)
	}
}

// TestGetUserEmail_AsksAuthTestOnce proves the installation's workspace is
// learned once per Client and kept -- but only once auth.test has named
// one: an answer with no team_id is asked again the next time.
func TestGetUserEmail_AsksAuthTestOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		authTest     map[string]any
		wantAuthTest int
	}{
		{"a named team is kept", map[string]any{"ok": true, "team_id": installationTeam}, 1},
		{"no team is asked again", map[string]any{"ok": true}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake, client := newFakeSlackUsers(t, fullMember(), tt.authTest)
			for i := 0; i < 3; i++ {
				if _, _, err := client.GetUserEmail(context.Background(), "U123"); err != nil {
					t.Fatalf("GetUserEmail() #%d error = %v", i, err)
				}
			}
			if n := fake.count("/auth.test"); n != tt.wantAuthTest {
				t.Errorf("auth.test calls = %d, want %d", n, tt.wantAuthTest)
			}
		})
	}
}

// TestGetUserEmail_AuthTestFailureIsRetryable proves a failing auth.test
// fails the fetch with an ordinary (retryable) error -- never a verdict on
// the member, and never ErrSlackUserNotFound.
func TestGetUserEmail_AuthTestFailureIsRetryable(t *testing.T) {
	t.Parallel()

	_, client := newFakeSlackUsers(t, fullMember(), map[string]any{"ok": false, "error": "ratelimited"})

	user, ok, err := client.GetUserEmail(context.Background(), "U123")
	if err == nil {
		t.Fatal("GetUserEmail() error = nil, want non-nil")
	}
	if errors.Is(err, slackapi.ErrSlackUserNotFound) {
		t.Error("GetUserEmail() error should NOT be ErrSlackUserNotFound for an auth.test failure")
	}
	if ok || user != (slackapi.UserEmail{}) {
		t.Errorf("GetUserEmail() = (%+v, %v), want the zero UserEmail and ok=false", user, ok)
	}
}

func TestGetUserEmail_NoEmailVisible(t *testing.T) {
	t.Parallel()

	fake, client := newFakeSlackUsers(t, map[string]any{"profile": map[string]any{}}, map[string]any{"ok": true, "team_id": installationTeam})

	user, ok, err := client.GetUserEmail(context.Background(), "U123")
	if err != nil {
		t.Fatalf("GetUserEmail() error = %v, want nil", err)
	}
	if ok {
		t.Fatal("GetUserEmail() ok = true, want false (no email visible)")
	}
	if user.Email != "" || user.Verified() {
		t.Errorf("user = %+v, want no email, not verified", user)
	}
	// The lookup starts alongside users.info, before the member is known,
	// so it may run; it is never asked twice.
	if n := fake.count("/auth.test"); n > 1 {
		t.Errorf("auth.test calls = %d, want at most 1", n)
	}
}

func TestGetUserEmail_UserNotFoundIsPermanent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")

	_, ok, err := client.GetUserEmail(context.Background(), "U123")
	if ok {
		t.Fatal("GetUserEmail() ok = true, want false")
	}
	if !errors.Is(err, slackapi.ErrSlackUserNotFound) {
		t.Fatalf("GetUserEmail() error = %v, want ErrSlackUserNotFound", err)
	}
}

func TestGetUserEmail_OtherAPIErrorIsRetryable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")

	_, ok, err := client.GetUserEmail(context.Background(), "U123")
	if ok {
		t.Fatal("GetUserEmail() ok = true, want false")
	}
	if err == nil {
		t.Fatal("GetUserEmail() error = nil, want non-nil")
	}
	if errors.Is(err, slackapi.ErrSlackUserNotFound) {
		t.Error("GetUserEmail() error should NOT be ErrSlackUserNotFound for a non-user_not_found API error")
	}
}

func TestGetUserEmail_HTTPFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")

	_, ok, err := client.GetUserEmail(context.Background(), "U123")
	if ok {
		t.Fatal("GetUserEmail() ok = true, want false")
	}
	if err == nil {
		t.Fatal("GetUserEmail() error = nil, want non-nil (malformed/non-JSON body)")
	}
}
