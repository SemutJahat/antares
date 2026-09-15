package llm

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestAdapterAuthorizationOverridesConfiguredHeader(t *testing.T) {
	var authorization string
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(fixture.Close)

	externalBaseURL := os.Getenv("ANTARES_HEADER_PRECEDENCE_URL")
	baseURL := fixture.URL + "/v1"
	if externalBaseURL != "" {
		baseURL = externalBaseURL
	}
	client, err := New(Options{
		Kind:    "openai-compatible",
		BaseURL: baseURL,
		APIKey:  "real-key",
		Headers: map[string]string{"Authorization": "Bearer configured-header"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Models(t.Context()); err != nil {
		t.Fatal(err)
	}
	if externalBaseURL == "" && authorization != "Bearer real-key" {
		t.Fatalf("Authorization = %q, want adapter-generated API key", authorization)
	}
}
