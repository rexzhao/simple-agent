package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBaseURL   = "https://chatgpt.com/backend-api/codex"
	DefaultIssuerURL = "https://auth.openai.com"
	DefaultClientID  = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultScope     = "openid profile email offline_access"
	defaultModelID   = "gpt-5.5"
)

type TokenFile struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountID    string    `json:"account_id,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	TokenURL     string    `json:"token_url,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
}

type AccessToken struct {
	Token     string
	AccountID string
}

type Store struct {
	Path string
}

func (s Store) Load() (TokenFile, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return TokenFile{}, fmt.Errorf("read Codex auth file %q: %w", s.Path, err)
	}
	var token TokenFile
	if err := json.Unmarshal(data, &token); err != nil {
		return TokenFile{}, fmt.Errorf("parse Codex auth file %q: %w", s.Path, err)
	}
	return token, nil
}

func (s Store) Save(token TokenFile) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create Codex auth dir: %w", err)
	}
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Codex auth file: %w", err)
	}
	data = append(data, '\n')

	temp, err := os.CreateTemp(dir, ".codex-auth-*")
	if err != nil {
		return fmt.Errorf("create temporary Codex auth file: %w", err)
	}
	tempPath := temp.Name()
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod temporary Codex auth file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary Codex auth file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary Codex auth file: %w", err)
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return fmt.Errorf("chmod temporary Codex auth file: %w", err)
	}
	if runtime.GOOS == "windows" {
		if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("replace Codex auth file %q: %w", s.Path, err)
		}
	}
	if err := os.Rename(tempPath, s.Path); err != nil {
		return fmt.Errorf("write Codex auth file %q: %w", s.Path, err)
	}
	cleanupTemp = false
	if err := os.Chmod(s.Path, 0o600); err != nil {
		return fmt.Errorf("chmod Codex auth file %q: %w", s.Path, err)
	}
	return nil
}

type TokenSource struct {
	Store      Store
	TokenURL   string
	ClientID   string
	HTTPClient *http.Client
	Now        func() time.Time
}

func (s *TokenSource) AccessToken(ctx context.Context) (AccessToken, error) {
	token, err := s.Store.Load()
	if err != nil {
		return AccessToken{}, err
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return AccessToken{}, fmt.Errorf("Codex auth file %q has no access_token", s.Store.Path)
	}
	if !s.isExpired(token) {
		return AccessToken{Token: token.AccessToken, AccountID: token.AccountID}, nil
	}
	if strings.TrimSpace(token.RefreshToken) == "" {
		return AccessToken{}, fmt.Errorf("Codex auth file %q access token is expired and refresh_token is missing", s.Store.Path)
	}

	refreshed, err := Refresh(ctx, RefreshOptions{
		RefreshToken: token.RefreshToken,
		AccountID:    token.AccountID,
		HTTPClient:   s.HTTPClient,
		Now:          s.now,
		TokenURL:     firstNonEmpty(s.TokenURL, token.TokenURL),
		ClientID:     firstNonEmpty(s.ClientID, token.ClientID),
	})
	if err != nil {
		return AccessToken{}, err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = token.RefreshToken
	}
	if refreshed.AccountID == "" {
		refreshed.AccountID = token.AccountID
	}
	refreshed.TokenURL = firstNonEmpty(refreshed.TokenURL, token.TokenURL, s.TokenURL)
	refreshed.ClientID = firstNonEmpty(refreshed.ClientID, token.ClientID, s.ClientID)
	if err := s.Store.Save(refreshed); err != nil {
		return AccessToken{}, err
	}
	return AccessToken{Token: refreshed.AccessToken, AccountID: refreshed.AccountID}, nil
}

func (s *TokenSource) isExpired(token TokenFile) bool {
	if token.ExpiresAt.IsZero() {
		return false
	}
	return !token.ExpiresAt.After(s.now().Add(30 * time.Second))
}

func (s *TokenSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

type DeviceLoginOptions struct {
	UserCodeURL    string
	DeviceTokenURL string
	TokenURL       string
	RedirectURI    string
	ClientID       string
	Scope          string
	HTTPClient     *http.Client
	Now            func() time.Time
	Output         io.Writer
	PollInterval   time.Duration
	Sleep          func(context.Context, time.Duration) error
}

type DeviceLoginResult struct {
	Token           TokenFile
	VerificationURI string
	UserCode        string
}

func DeviceLogin(ctx context.Context, options DeviceLoginOptions) (DeviceLoginResult, error) {
	userCode, err := requestUserCode(ctx, options)
	if err != nil {
		return DeviceLoginResult{}, err
	}
	verificationURI := userCode.VerificationURI
	if options.Output != nil {
		fmt.Fprintf(options.Output, "Open %s and enter code %s\n", verificationURI, userCode.UserCode)
	}
	authorization, err := pollDeviceAuthorization(ctx, options, userCode)
	if err != nil {
		return DeviceLoginResult{}, err
	}
	token, err := exchangeAuthorizationCode(ctx, options, authorization)
	if err != nil {
		return DeviceLoginResult{}, err
	}
	return DeviceLoginResult{
		Token:           token,
		VerificationURI: verificationURI,
		UserCode:        userCode.UserCode,
	}, nil
}

type RefreshOptions struct {
	TokenURL     string
	ClientID     string
	RefreshToken string
	AccountID    string
	HTTPClient   *http.Client
	Now          func() time.Time
}

func Refresh(ctx context.Context, options RefreshOptions) (TokenFile, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", options.RefreshToken)
	form.Set("client_id", clientID(options.ClientID))
	response, err := postForm(ctx, options.HTTPClient, tokenURL(options.TokenURL), form)
	if err != nil {
		return TokenFile{}, fmt.Errorf("refresh Codex access token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return TokenFile{}, fmt.Errorf("refresh Codex access token: %s: %s", response.Status, readRedactedBody(response.Body, options.RefreshToken))
	}
	token, err := decodeTokenResponse(response.Body, nowFunc(options.Now))
	if err != nil {
		return TokenFile{}, fmt.Errorf("refresh Codex access token: %w", err)
	}
	if token.AccountID == "" {
		token.AccountID = options.AccountID
	}
	return token, nil
}

type userCodeResponse struct {
	DeviceAuthID    string  `json:"device_auth_id"`
	UserCode        string  `json:"user_code"`
	VerificationURI string  `json:"verification_uri"`
	ExpiresIn       jsonInt `json:"expires_in"`
	Interval        jsonInt `json:"interval"`
}

type authorizationResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

type jsonInt int

func (i *jsonInt) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		return nil
	}
	if strings.HasPrefix(raw, `"`) {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		raw = strings.TrimSpace(value)
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("invalid integer %q", raw)
	}
	*i = jsonInt(value)
	return nil
}

func requestUserCode(ctx context.Context, options DeviceLoginOptions) (userCodeResponse, error) {
	body := struct {
		ClientID string `json:"client_id"`
		Scope    string `json:"scope"`
	}{
		ClientID: clientID(options.ClientID),
		Scope:    scope(options.Scope),
	}
	response, err := postJSON(ctx, options.HTTPClient, userCodeURL(options.UserCodeURL), body)
	if err != nil {
		return userCodeResponse{}, fmt.Errorf("request Codex user code: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return userCodeResponse{}, fmt.Errorf("request Codex user code: %s: %s", response.Status, readRedactedBody(response.Body, ""))
	}
	var userCode userCodeResponse
	if err := json.NewDecoder(response.Body).Decode(&userCode); err != nil {
		return userCodeResponse{}, fmt.Errorf("decode Codex user code response: %w", err)
	}
	if strings.TrimSpace(userCode.UserCode) == "" {
		return userCodeResponse{}, fmt.Errorf("Codex user code response is missing user_code")
	}
	if strings.TrimSpace(userCode.DeviceAuthID) == "" {
		return userCodeResponse{}, fmt.Errorf("Codex user code response is missing device_auth_id")
	}
	if strings.TrimSpace(userCode.VerificationURI) == "" {
		userCode.VerificationURI = strings.TrimRight(DefaultIssuerURL, "/") + "/device"
	}
	return userCode, nil
}

func pollDeviceAuthorization(ctx context.Context, options DeviceLoginOptions, userCode userCodeResponse) (authorizationResponse, error) {
	interval := options.PollInterval
	if interval <= 0 {
		interval = time.Duration(userCode.Interval) * time.Second
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresAt := nowFunc(options.Now)().Add(time.Duration(userCode.ExpiresIn) * time.Second)

	for {
		if userCode.ExpiresIn > 0 && !nowFunc(options.Now)().Before(expiresAt) {
			return authorizationResponse{}, fmt.Errorf("Codex device login expired before authorization completed")
		}
		body := struct {
			DeviceAuthID string `json:"device_auth_id"`
			UserCode     string `json:"user_code"`
		}{
			DeviceAuthID: userCode.DeviceAuthID,
			UserCode:     userCode.UserCode,
		}
		response, err := postJSON(ctx, options.HTTPClient, deviceTokenURL(options.DeviceTokenURL), body)
		if err != nil {
			return authorizationResponse{}, fmt.Errorf("poll Codex device authorization: %w", err)
		}
		authorization, retry, slowDown, err := decodeAuthorizationPollResponse(response, userCode.UserCode)
		if err != nil {
			return authorizationResponse{}, err
		}
		if !retry {
			return authorization, nil
		}
		if slowDown {
			interval += 5 * time.Second
		}
		if err := sleepFunc(options.Sleep)(ctx, interval); err != nil {
			return authorizationResponse{}, err
		}
	}
}

func decodeAuthorizationPollResponse(response *http.Response, secret string) (authorizationResponse, bool, bool, error) {
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		var authorization authorizationResponse
		if err := json.NewDecoder(response.Body).Decode(&authorization); err != nil {
			return authorizationResponse{}, false, false, fmt.Errorf("decode Codex authorization response: %w", err)
		}
		if strings.TrimSpace(authorization.AuthorizationCode) == "" || strings.TrimSpace(authorization.CodeVerifier) == "" {
			return authorizationResponse{}, false, false, fmt.Errorf("Codex authorization response is missing authorization_code or code_verifier")
		}
		return authorization, false, false, nil
	}
	var oauthErr struct {
		Error string `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	_ = json.Unmarshal(data, &oauthErr)
	switch oauthErr.Error {
	case "authorization_pending":
		return authorizationResponse{}, true, false, nil
	case "slow_down":
		return authorizationResponse{}, true, true, nil
	default:
		body := strings.TrimSpace(string(data))
		if secret != "" {
			body = strings.ReplaceAll(body, secret, "<redacted>")
		}
		return authorizationResponse{}, false, false, fmt.Errorf("poll Codex device authorization: %s: %s", response.Status, body)
	}
}

func exchangeAuthorizationCode(ctx context.Context, options DeviceLoginOptions, authorization authorizationResponse) (TokenFile, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", authorization.AuthorizationCode)
	form.Set("redirect_uri", redirectURI(options.RedirectURI))
	form.Set("client_id", clientID(options.ClientID))
	form.Set("code_verifier", authorization.CodeVerifier)
	response, err := postForm(ctx, options.HTTPClient, tokenURL(options.TokenURL), form)
	if err != nil {
		return TokenFile{}, fmt.Errorf("exchange Codex authorization code: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body := readRedactedBody(response.Body, authorization.AuthorizationCode)
		body = strings.ReplaceAll(body, authorization.CodeVerifier, "<redacted>")
		return TokenFile{}, fmt.Errorf("exchange Codex authorization code: %s: %s", response.Status, body)
	}
	token, err := decodeTokenResponse(response.Body, nowFunc(options.Now))
	if err != nil {
		return TokenFile{}, fmt.Errorf("exchange Codex authorization code: %w", err)
	}
	return token, nil
}

func decodeTokenResponse(body io.Reader, now func() time.Time) (TokenFile, error) {
	var raw struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		AccountID        string `json:"account_id"`
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		TokenType        string `json:"token_type"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return TokenFile{}, err
	}
	if strings.TrimSpace(raw.AccessToken) == "" {
		return TokenFile{}, fmt.Errorf("token response is missing access_token")
	}
	expiresAt := time.Time{}
	if raw.ExpiresIn > 0 {
		expiresAt = now().Add(time.Duration(raw.ExpiresIn) * time.Second).UTC()
	}
	return TokenFile{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresAt:    expiresAt,
		AccountID:    firstNonEmpty(raw.AccountID, raw.ChatGPTAccountID),
		TokenType:    raw.TokenType,
	}, nil
}

func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return client.Do(request)
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, body any) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return client.Do(request)
}

func UserCodeURLForIssuer(issuer string) string {
	return strings.TrimRight(strings.TrimSpace(issuer), "/") + "/api/accounts/deviceauth/usercode"
}

func DeviceTokenURLForIssuer(issuer string) string {
	return strings.TrimRight(strings.TrimSpace(issuer), "/") + "/api/accounts/deviceauth/token"
}

func TokenURLForIssuer(issuer string) string {
	return strings.TrimRight(strings.TrimSpace(issuer), "/") + "/oauth/token"
}

func RedirectURIForIssuer(issuer string) string {
	return strings.TrimRight(strings.TrimSpace(issuer), "/") + "/deviceauth/callback"
}

func DefaultModelID() string {
	return defaultModelID
}

func clientID(value string) string {
	if strings.TrimSpace(value) == "" {
		return DefaultClientID
	}
	return strings.TrimSpace(value)
}

func scope(value string) string {
	if strings.TrimSpace(value) == "" {
		return DefaultScope
	}
	return strings.TrimSpace(value)
}

func userCodeURL(value string) string {
	if strings.TrimSpace(value) == "" {
		return UserCodeURLForIssuer(DefaultIssuerURL)
	}
	return strings.TrimSpace(value)
}

func deviceTokenURL(value string) string {
	if strings.TrimSpace(value) == "" {
		return DeviceTokenURLForIssuer(DefaultIssuerURL)
	}
	return strings.TrimSpace(value)
}

func tokenURL(value string) string {
	if strings.TrimSpace(value) == "" {
		return TokenURLForIssuer(DefaultIssuerURL)
	}
	return strings.TrimSpace(value)
}

func redirectURI(value string) string {
	if strings.TrimSpace(value) == "" {
		return RedirectURIForIssuer(DefaultIssuerURL)
	}
	return strings.TrimSpace(value)
}

func nowFunc(now func() time.Time) func() time.Time {
	if now != nil {
		return now
	}
	return func() time.Time { return time.Now().UTC() }
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func readRedactedBody(body io.Reader, secret string) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return "read response body: " + err.Error()
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		return "empty response body"
	}
	if strings.TrimSpace(secret) != "" {
		message = strings.ReplaceAll(message, secret, "<redacted>")
	}
	return message
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func sleepFunc(custom func(context.Context, time.Duration) error) func(context.Context, time.Duration) error {
	if custom != nil {
		return custom
	}
	return sleep
}
