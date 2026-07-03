package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTokenSourceRefreshesExpiredTokenAndWritesFile(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("path = %q, want /oauth/token", r.URL.Path)
		}
		assertRequestContentType(t, r, "application/x-www-form-urlencoded")
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"account_id":"acct-new","token_type":"Bearer"}`)
	}))
	defer server.Close()

	authPath := filepath.Join(t.TempDir(), "codex.json")
	store := Store{Path: authPath}
	if err := store.Save(TokenFile{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    now.Add(-time.Minute),
		AccountID:    "acct-old",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	source := &TokenSource{
		Store:      store,
		TokenURL:   server.URL + "/oauth/token",
		ClientID:   "test-client",
		HTTPClient: server.Client(),
		Now:        func() time.Time { return now },
	}
	token, err := source.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	if token.Token != "new-access" || token.AccountID != "acct-new" {
		t.Fatalf("AccessToken() = %#v, want refreshed token/account", token)
	}
	if gotForm.Get("grant_type") != "refresh_token" || gotForm.Get("refresh_token") != "old-refresh" || gotForm.Get("client_id") != "test-client" {
		t.Fatalf("refresh form = %#v", gotForm)
	}

	data, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var saved TokenFile
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if saved.AccessToken != "new-access" || saved.RefreshToken != "new-refresh" || saved.AccountID != "acct-new" {
		t.Fatalf("saved token = %#v, want refreshed values", saved)
	}
	if !saved.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("ExpiresAt = %s, want %s", saved.ExpiresAt, now.Add(time.Hour))
	}
}

func TestStoreSaveOverwritesWithPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix permission bits for this check")
	}

	authPath := filepath.Join(t.TempDir(), "codex.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"old"}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := (Store{Path: authPath}).Save(TokenFile{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 0600", got)
	}
}

func TestUserCodeResponseDecodesNumericFields(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		interval  int
		expiresIn int
	}{
		{
			name:      "numbers",
			response:  `{"device_auth_id":"device-auth-123","user_code":"USER-123","interval":1,"expires_in":600}`,
			interval:  1,
			expiresIn: 600,
		},
		{
			name:      "numeric strings",
			response:  `{"device_auth_id":"device-auth-123","user_code":"USER-123","interval":"1","expires_in":"600"}`,
			interval:  1,
			expiresIn: 600,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got userCodeResponse
			if err := json.Unmarshal([]byte(tt.response), &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if int(got.Interval) != tt.interval || int(got.ExpiresIn) != tt.expiresIn {
				t.Fatalf("userCodeResponse = %#v, want interval %d expires_in %d", got, tt.interval, tt.expiresIn)
			}
		})
	}
}

func TestDeviceLoginSlowDownIncreasesPollingInterval(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	var tokenPolls int
	var exchangeForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			assertRequestContentType(t, r, "application/json")
			body := readRequestJSON(t, r)
			if body["client_id"] != DefaultClientID || body["scope"] != DefaultScope {
				t.Fatalf("usercode body = %#v, want client_id and scope", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"device-auth-123","user_code":"USER-123","verification_uri":"https://example.test/device","interval":"1","expires_in":"600"}`)
		case "/api/accounts/deviceauth/token":
			assertRequestContentType(t, r, "application/json")
			body := readRequestJSON(t, r)
			if body["device_auth_id"] != "device-auth-123" || body["user_code"] != "USER-123" {
				t.Fatalf("device token body = %#v, want device_auth_id and user_code", body)
			}
			tokenPolls++
			w.Header().Set("Content-Type", "application/json")
			switch tokenPolls {
			case 1:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
			case 2:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"slow_down"}`)
			default:
				_, _ = io.WriteString(w, `{"authorization_code":"auth-code","code_verifier":"verifier-123"}`)
			}
		case "/oauth/token":
			assertRequestContentType(t, r, "application/x-www-form-urlencoded")
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm() error = %v", err)
			}
			exchangeForm = r.PostForm
			w.Header().Set("Content-Type", "application/json")
			switch r.PostForm.Get("grant_type") {
			case "authorization_code":
				_, _ = io.WriteString(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)
			default:
				t.Fatalf("unexpected grant_type %q", r.PostForm.Get("grant_type"))
			}
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	var sleeps []time.Duration
	_, err := DeviceLogin(context.Background(), DeviceLoginOptions{
		UserCodeURL:    server.URL + "/api/accounts/deviceauth/usercode",
		DeviceTokenURL: server.URL + "/api/accounts/deviceauth/token",
		TokenURL:       server.URL + "/oauth/token",
		RedirectURI:    server.URL + "/deviceauth/callback",
		HTTPClient:     server.Client(),
		Now:            func() time.Time { return now },
		Sleep: func(ctx context.Context, duration time.Duration) error {
			sleeps = append(sleeps, duration)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("DeviceLogin() error = %v", err)
	}
	want := []time.Duration{time.Second, 6 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %#v, want %#v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("sleeps[%d] = %s, want %s", i, sleeps[i], want[i])
		}
	}
	if exchangeForm.Get("grant_type") != "authorization_code" ||
		exchangeForm.Get("code") != "auth-code" ||
		exchangeForm.Get("code_verifier") != "verifier-123" ||
		exchangeForm.Get("redirect_uri") != server.URL+"/deviceauth/callback" {
		t.Fatalf("exchange form = %#v", exchangeForm)
	}
}

func TestDeviceLoginFallsBackToCodexDeviceURLAndPollsDeviceAuthPending(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	var tokenPolls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"device_auth_id":"device-auth-123","user_code":"USER-123","interval":1,"expires_in":600}`)
		case "/api/accounts/deviceauth/token":
			tokenPolls++
			w.Header().Set("Content-Type", "application/json")
			switch tokenPolls {
			case 1:
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"code":"deviceauth_authorization_pending","message":"Device authorization is pending. Please try again."}`)
			case 2:
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"code":"deviceauth_authorization_pending","message":"Device authorization is pending. Please try again."}}`)
			default:
				_, _ = io.WriteString(w, `{"authorization_code":"auth-code","code_verifier":"verifier-123"}`)
			}
		case "/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	var output bytes.Buffer
	var sleeps []time.Duration
	result, err := DeviceLogin(context.Background(), DeviceLoginOptions{
		UserCodeURL:    server.URL + "/api/accounts/deviceauth/usercode",
		DeviceTokenURL: server.URL + "/api/accounts/deviceauth/token",
		TokenURL:       server.URL + "/oauth/token",
		RedirectURI:    server.URL + "/deviceauth/callback",
		HTTPClient:     server.Client(),
		Now:            func() time.Time { return now },
		Output:         &output,
		Sleep: func(ctx context.Context, duration time.Duration) error {
			sleeps = append(sleeps, duration)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("DeviceLogin() error = %v", err)
	}
	if result.VerificationURI != server.URL+"/codex/device" {
		t.Fatalf("VerificationURI = %q, want %q", result.VerificationURI, server.URL+"/codex/device")
	}
	if !strings.Contains(output.String(), "Open "+server.URL+"/codex/device and enter code USER-123") {
		t.Fatalf("output = %q, want fallback verification URI", output.String())
	}
	if result.Token.AccessToken != "access" || tokenPolls != 3 {
		t.Fatalf("result token = %#v, tokenPolls = %d; want access token after pending polls", result.Token, tokenPolls)
	}
	wantSleeps := []time.Duration{time.Second, time.Second}
	if len(sleeps) != len(wantSleeps) {
		t.Fatalf("sleeps = %#v, want %#v", sleeps, wantSleeps)
	}
	for i := range wantSleeps {
		if sleeps[i] != wantSleeps[i] {
			t.Fatalf("sleeps[%d] = %s, want %s", i, sleeps[i], wantSleeps[i])
		}
	}
}

func assertRequestContentType(t *testing.T, r *http.Request, want string) {
	t.Helper()

	if got := r.Header.Get("Content-Type"); got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
}

func readRequestJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request JSON: %v", err)
	}
	return body
}
