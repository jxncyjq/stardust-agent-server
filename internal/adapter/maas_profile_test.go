package adapter

import (
	"testing"

	"github.com/stardust/legion-agent/internal/config"
)

func TestNewMaasClientFromProfileUsesNamedProfile(t *testing.T) {
	t.Parallel()
	client, err := NewMaasClientFromProfile(config.MaasConfig{
		DefaultProfile: "fast",
		Profiles: map[string]config.MaasProfile{
			"review": {BaseURL: "https://review.example.test", APIKey: "review-key"},
		},
	}, "review")
	if err != nil {
		t.Fatalf("NewMaasClientFromProfile(review) error = %v, want nil", err)
	}
	httpClient, ok := client.(*HTTPMaasClient)
	if !ok {
		t.Fatalf("NewMaasClientFromProfile(review) = %T, want *HTTPMaasClient", client)
	}
	if httpClient.baseURL != "https://review.example.test" {
		t.Fatalf("HTTPMaasClient.baseURL = %q, want review URL", httpClient.baseURL)
	}
}
