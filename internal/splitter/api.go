package splitter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type apiCredentials struct {
	Token     string
	KeyID     string
	KeySecret string
}

func credentialsFromEnvironment() apiCredentials {
	return apiCredentials{
		Token: os.Getenv("SPACELIFT_API_TOKEN"), KeyID: os.Getenv("SPACELIFT_API_KEY_ID"),
		KeySecret: os.Getenv("SPACELIFT_API_KEY_SECRET"),
	}
}

func accountEndpoint(endpoint string) (string, error) {
	account, err := url.Parse(endpoint)
	if err != nil || account.Scheme != "https" || account.Host == "" || account.User != nil || account.RawQuery != "" || account.Fragment != "" ||
		(account.Path != "" && account.Path != "/" && account.Path != "/graphql" && account.Path != "/graphql/") {
		return "", fmt.Errorf("endpoint must be an HTTPS account URL or its /graphql endpoint")
	}
	account.Path = "/graphql"
	account.RawPath = ""
	return account.String(), nil
}

func authenticate(ctx context.Context, endpoint string, credentials apiCredentials, client *http.Client) (string, error) {
	if _, err := accountEndpoint(endpoint); err != nil {
		return "", err
	}
	if credentials.Token != "" {
		if credentials.KeyID != "" || credentials.KeySecret != "" {
			return "", fmt.Errorf("set either SPACELIFT_API_TOKEN or SPACELIFT_API_KEY_ID and SPACELIFT_API_KEY_SECRET, not both")
		}
		if strings.TrimSpace(credentials.Token) == "" || strings.ContainsAny(credentials.Token, "\r\n\t ") {
			return "", fmt.Errorf("SPACELIFT_API_TOKEN must be a nonempty token without whitespace")
		}
		return credentials.Token, nil
	}
	if strings.TrimSpace(credentials.KeyID) == "" || strings.TrimSpace(credentials.KeySecret) == "" {
		return "", fmt.Errorf("set SPACELIFT_API_KEY_ID and SPACELIFT_API_KEY_SECRET, or SPACELIFT_API_TOKEN")
	}
	raw, err := accountRequest(ctx, endpoint, "", "API authentication", `mutation AdminSplitterAuthenticate($id: ID!, $secret: String!) {
  apiKeyUser(id: $id, secret: $secret) { jwt }
}`, map[string]string{"id": credentials.KeyID, "secret": credentials.KeySecret}, client)
	if err != nil {
		return "", err
	}
	var response struct {
		Data struct {
			User struct {
				JWT string `json:"jwt"`
			} `json:"apiKeyUser"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || strings.TrimSpace(response.Data.User.JWT) == "" || strings.ContainsAny(response.Data.User.JWT, "\r\n\t ") {
		return "", fmt.Errorf("API authentication did not return a valid token; check the API key ID and secret")
	}
	return response.Data.User.JWT, nil
}

func accountRequest(ctx context.Context, endpoint, token, operation, query string, variables any, client *http.Client) ([]byte, error) {
	endpoint, err := accountEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Query     string `json:"query"`
		Variables any    `json:"variables"`
	}{query, variables})
	if err != nil {
		return nil, fmt.Errorf("cannot encode %s request", operation)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("cannot create %s request", operation)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	bounded := boundedHTTPClient(client, 30*time.Second)
	response, err := bounded.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s request failed; check connectivity and authentication", operation)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d; check account access", operation, response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxStackSettingsBytes+1))
	if err != nil || len(raw) > maxStackSettingsBytes {
		return nil, fmt.Errorf("%s response could not be read within the size limit", operation)
	}
	var envelope struct {
		Errors []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, fmt.Errorf("invalid %s response", operation)
	}
	if len(envelope.Errors) != 0 {
		return nil, fmt.Errorf("%s failed; check credentials, stack access and external state access if downloading state", operation)
	}
	return raw, nil
}

func boundedHTTPClient(client *http.Client, timeout time.Duration) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	bounded := *client
	if bounded.Timeout <= 0 || bounded.Timeout > timeout {
		bounded.Timeout = timeout
	}
	bounded.Jar = nil
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &bounded
}
