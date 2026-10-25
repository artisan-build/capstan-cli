package work

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRepositoryShapeNegotiationAndStrictOptionalRepository(t *testing.T) {
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["item_shape"]) != `"repository-v1"` {
			t.Error("missing repository negotiation")
		}
		seen[r.URL.Path] = true
		if strings.HasSuffix(r.URL.Path, "claim") {
			writeJSON(t, w, claimResponse(ampClaim()))
			return
		}
		writeJSON(t, w, map[string]any{"results": []RenewResult{ampRenew("awaiting_verification")}})
	}))
	defer server.Close()
	c := testConfig(t)
	c.CapstanURL = server.URL
	client := NewClient(c, "never-forward-me")
	claim, err := client.Claim(context.Background(), ClaimRequest{ItemShape: "attacker-override"})
	if err != nil || claim.Items[0].Repository == nil || *claim.Items[0].Repository != testRepository {
		t.Fatal("claim repository not decoded")
	}
	renew, err := client.Renew(context.Background(), []RenewRequestItem{{ItemID: testItemID, Fence: 1}})
	if err != nil || renew[0].Item.Repository == nil || renew[0].Item.State != "awaiting_verification" {
		t.Fatal("renew repository/waiting state not decoded")
	}
	if len(seen) != 2 {
		t.Fatal("both claim and renew not observed")
	}
}

func TestCorrectionHTTPBoundedStrictNoRedirectOrBearer(t *testing.T) {
	body := `{"steers":[{"id":"` + testDirectiveID + `","task_id":"` + testTaskID + `","item_id":"` + testItemID + `","text":"fix the assertion","author_actor":"actor:1","created_at":"2026-10-25T00:00:00Z","read_at":"2026-10-25T00:00:01Z"}]}`
	status := http.StatusOK
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Capstan-Actor-ID") != "" {
			t.Error("signed GET forwarded machine credential")
		}
		w.Header().Set("Location", "https://unreachable.example/")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	c := testConfig(t)
	c.CapstanURL = server.URL
	client := NewClient(c, "credential-secret")
	steers, err := client.Steers(context.Background(), server.URL+"/steers?signature=secret")
	if err != nil || len(steers) != 1 || steers[0].ItemID != testItemID {
		t.Fatalf("steers: %#v %v", steers, err)
	}
	for _, bad := range []string{`{}`, `{"steers":null}`, `{"steers":[],"unknown":true}`, `{"steers":[],"steers":[]}`, `{"steers":[]} {}`, strings.Repeat(" ", 256*(12*8000+4096)+4096) + `{"steers":[]}`} {
		body = bad
		_, err := client.Steers(context.Background(), server.URL+"/steers?signature=secret")
		if err == nil {
			t.Fatal("malformed/oversize steers accepted")
		}
		for _, secret := range []string{"signature=", "credential-secret", "unreachable.example"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatal("HTTP error leaked URL/credential")
			}
		}
	}
	status = http.StatusFound
	if _, err := client.Steers(context.Background(), server.URL+"/steers?signature=secret"); err == nil {
		t.Fatal("redirect accepted")
	}
	before := calls
	foreignCalls := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls++
		_, _ = io.WriteString(w, `{"steers":[]}`)
	}))
	defer foreign.Close()
	_, err = client.Steers(context.Background(), foreign.URL+"/steers?signature=secret")
	var apiErr APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_steer_endpoint" || foreignCalls != 0 || calls != before {
		t.Fatal("foreign endpoint was not refused by the same-origin guard")
	}
}
