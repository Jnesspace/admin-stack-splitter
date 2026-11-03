package splitter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAuthenticateExchangesKeyWithoutPrintingOrPersistingIt(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("authentication included unexpected request metadata")
		}
		var request struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Variables["id"] != "test-key-id" || request.Variables["secret"] != "private-secret-marker" || strings.Contains(request.Query, "private-secret-marker") {
			t.Error("API credentials were not passed solely in GraphQL variables")
		}
		_, _ = io.WriteString(w, `{"data":{"apiKeyUser":{"jwt":"test-jwt"}}}`)
	}))
	defer server.Close()
	client := server.Client()
	client.Jar, _ = cookiejar.New(nil)
	account, _ := url.Parse(server.URL)
	client.Jar.SetCookies(account, []*http.Cookie{{Name: "private-session", Value: "private-cookie-marker"}})
	got, err := authenticate(context.Background(), server.URL, apiCredentials{KeyID: "test-key-id", KeySecret: "private-secret-marker"}, client)
	if err != nil || got != "test-jwt" {
		t.Fatalf("authentication failed: %v", err)
	}
}

func TestAuthenticateRequiresOneCompleteCredentialMethod(t *testing.T) {
	var contacted atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Store(true) }))
	defer server.Close()
	for _, credentials := range []apiCredentials{
		{}, {KeyID: "id"}, {KeySecret: "secret"}, {KeyID: " ", KeySecret: "secret"},
		{Token: "token", KeyID: "id"}, {Token: "token", KeySecret: "secret"},
		{Token: "\r\nprivate-token-marker"}, {Token: " "}, {Token: "a b"},
	} {
		if _, err := authenticate(context.Background(), server.URL, credentials, server.Client()); err == nil || strings.Contains(err.Error(), "private-token-marker") {
			t.Fatalf("accepted or exposed invalid credentials: %v", err)
		}
	}
	got, err := authenticate(context.Background(), server.URL, apiCredentials{Token: "test-token"}, server.Client())
	if err != nil || got != "test-token" || contacted.Load() {
		t.Fatalf("token authentication made a request or failed: %v", err)
	}
}

func TestAuthenticateRejectsUnsafeResponsesWithoutLeaking(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"GraphQL error", `{"errors":[{"message":"private-response-marker"}],"data":{"apiKeyUser":{"jwt":"test-token"}}}`, 200},
		{"missing token", `{"data":{"apiKeyUser":null}}`, 200},
		{"token whitespace", `{"data":{"apiKeyUser":{"jwt":"private-response-marker\n"}}}`, 200},
		{"invalid JSON", `private-response-marker`, 200},
		{"oversized", strings.Repeat("x", maxStackSettingsBytes+1), 200},
		{"HTTP error", `private-response-marker`, 403},
		{"redirect", `private-response-marker`, 307},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var redirected atomic.Bool
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/graphql" {
					redirected.Store(true)
				}
				w.Header().Set("Location", "/private-response-marker")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			_, err := authenticate(context.Background(), server.URL, apiCredentials{KeyID: "key", KeySecret: "private-secret-marker"}, server.Client())
			if err == nil || redirected.Load() || strings.Contains(err.Error(), "private-response-marker") || strings.Contains(err.Error(), "private-secret-marker") {
				t.Fatalf("unsafe authentication response accepted or exposed: %v", err)
			}
		})
	}
}

func TestCredentialsFromEnvironment(t *testing.T) {
	t.Setenv("SPACELIFT_API_TOKEN", "test-token")
	t.Setenv("SPACELIFT_API_KEY_ID", "test-id")
	t.Setenv("SPACELIFT_API_KEY_SECRET", "test-secret")
	if got := credentialsFromEnvironment(); got != (apiCredentials{Token: "test-token", KeyID: "test-id", KeySecret: "test-secret"}) {
		t.Fatal("credential environment was not read")
	}
}
