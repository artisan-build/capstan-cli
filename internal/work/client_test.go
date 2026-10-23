package work

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientClaimNormalizesNilCollectionsAtWireBoundary(t *testing.T) {
	tests := []struct {
		name      string
		request   ClaimRequest
		want      string
		forbidden string
	}{
		{name: "roles", request: ClaimRequest{Capabilities: []string{}, RoleProviders: map[string]string{}, HeldItems: []HeldItem{}}, want: `"roles":[]`, forbidden: `"roles":null`},
		{name: "capabilities", request: ClaimRequest{Roles: []string{"builder"}, RoleProviders: map[string]string{}, HeldItems: []HeldItem{}}, want: `"capabilities":[]`, forbidden: `"capabilities":null`},
		{name: "role providers", request: ClaimRequest{Roles: []string{"builder"}, Capabilities: []string{}, HeldItems: []HeldItem{}}, want: `"role_providers":{}`, forbidden: `"role_providers":null`},
		{name: "held items", request: ClaimRequest{Roles: []string{"builder"}, Capabilities: []string{}, RoleProviders: map[string]string{}}, want: `"held_items":[]`, forbidden: `"held_items":null`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var body []byte
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var err error
				body, err = io.ReadAll(request.Body)
				if err != nil {
					t.Fatalf("read claim body: %v", err)
				}
				writeJSON(t, writer, ClaimResponse{Items: []ClaimItem{}, Directives: []Directive{}})
			}))
			defer server.Close()

			config := testConfig(t)
			config.CapstanURL = server.URL
			if _, err := NewClient(config, "secret").Claim(context.Background(), test.request); err != nil {
				t.Fatalf("Claim() error = %v", err)
			}
			if !strings.Contains(string(body), test.want) || strings.Contains(string(body), test.forbidden) {
				t.Fatalf("claim body = %s, want %s and no %s", body, test.want, test.forbidden)
			}
		})
	}
}

func TestClientRejectsUnsafeResponsesWithoutLeakingMaterial(t *testing.T) {
	const (
		bearer = "super-secret-bound-bearer"
		signed = "https://capstan.example/brief?signature=secret-signature"
	)
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "malformed", handler: func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"items":` + signed))
		}},
		{name: "oversized", handler: func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"items":[],"directives":[],"padding":"` + strings.Repeat("x", maxResponseBytes) + `"}`))
		}},
		{name: "non-2xx", handler: func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(bearer + " " + signed))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			config := testConfig(t)
			config.CapstanURL = server.URL
			config.RequestTimeout = time.Second
			_, err := NewClient(config, bearer).Claim(context.Background(), ClaimRequest{
				Roles: []string{"builder"}, Capabilities: []string{}, RoleProviders: map[string]string{}, HeldItems: []HeldItem{},
			})
			if err == nil {
				t.Fatal("Claim() error = nil")
			}
			for _, forbidden := range []string{bearer, signed, "secret-signature"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error leaked %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestClientDoesNotFollowRedirect(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalls.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/brief?signature=must-not-leak", http.StatusFound)
	}))
	defer source.Close()
	config := testConfig(t)
	config.CapstanURL = source.URL
	_, err := NewClient(config, "redirect-secret").Claim(context.Background(), ClaimRequest{
		Roles: []string{"builder"}, Capabilities: []string{}, RoleProviders: map[string]string{}, HeldItems: []HeldItem{},
	})
	if err == nil {
		t.Fatal("Claim() followed redirect")
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d", targetCalls.Load())
	}
	if strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "redirect-secret") {
		t.Fatalf("redirect error leaked material: %v", err)
	}
}

func TestClientRenewEnforcesBatchCapAndExactJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Items []RenewRequestItem `json:"items"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Items) != MaxBatch {
			t.Fatalf("renew items = %d", len(body.Items))
		}
		results := make([]RenewResult, 0, len(body.Items))
		for _, item := range body.Items {
			results = append(results, RenewResult{ItemID: item.ItemID, Status: "lost"})
		}
		writeJSON(t, writer, map[string]any{"results": results})
	}))
	defer server.Close()
	config := testConfig(t)
	config.CapstanURL = server.URL
	client := NewClient(config, "secret")
	items := make([]RenewRequestItem, MaxBatch)
	for index := range items {
		items[index] = RenewRequestItem{ItemID: fmt.Sprintf("018f0000-0000-7000-8000-%012d", index+1), Fence: 1}
	}
	if _, err := client.Renew(context.Background(), items); err != nil {
		t.Fatalf("Renew(25) error = %v", err)
	}
	if _, err := client.Renew(context.Background(), append(items, RenewRequestItem{})); err == nil {
		t.Fatal("Renew(26) error = nil")
	}
}
