package gitops

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyWebhookSignature(t *testing.T) {
	secret := "secret-key-123"
	payload := []byte(`{"ref":"refs/heads/main","after":"a1b2c3d4"}`)

	// Compute HMAC
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	hexSig := hex.EncodeToString(mac.Sum(nil))

	// 1. Forgejo
	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Forgejo-Signature", hexSig)
	prov, err := VerifyWebhookSignature(req, payload, secret)
	if err != nil || prov != "forgejo" {
		t.Errorf("Forgejo verification failed: prov=%s, err=%v", prov, err)
	}

	// 2. Gitea
	req = httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Gitea-Signature", hexSig)
	prov, err = VerifyWebhookSignature(req, payload, secret)
	if err != nil || prov != "gitea" {
		t.Errorf("Gitea verification failed: prov=%s, err=%v", prov, err)
	}

	// 3. GitHub
	req = httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hexSig)
	prov, err = VerifyWebhookSignature(req, payload, secret)
	if err != nil || prov != "github" {
		t.Errorf("GitHub verification failed: prov=%s, err=%v", prov, err)
	}

	// 4. GitLab
	req = httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Gitlab-Token", secret)
	prov, err = VerifyWebhookSignature(req, payload, secret)
	if err != nil || prov != "gitlab" {
		t.Errorf("GitLab verification failed: prov=%s, err=%v", prov, err)
	}

	// 5. Invalid Signature
	req = httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Forgejo-Signature", "invalid-sig")
	_, err = VerifyWebhookSignature(req, payload, secret)
	if err == nil {
		t.Error("expected error for invalid signature")
	}
}

func TestParseWebhookPayload(t *testing.T) {
	payload := []byte(`{
		"ref": "refs/heads/release/v1",
		"after": "9f8e7d6c5b4a",
		"pusher": { "name": "developer" },
		"commits": [
			{ "id": "9f8e7d6c5b4a", "message": "feat: update release" }
		]
	}`)

	event, err := ParseWebhookPayload("github", "push", payload)
	if err != nil {
		t.Fatalf("failed to parse payload: %v", err)
	}

	if event.Branch != "release/v1" {
		t.Errorf("expected branch release/v1, got %s", event.Branch)
	}
	if event.CommitSHA != "9f8e7d6c5b4a" {
		t.Errorf("expected commit 9f8e7d6c5b4a, got %s", event.CommitSHA)
	}
	if event.Author != "developer" {
		t.Errorf("expected author developer, got %s", event.Author)
	}
	if event.Message != "feat: update release" {
		t.Errorf("expected message 'feat: update release', got %s", event.Message)
	}

	// Match event
	if !MatchEvent(event, []string{"push"}, []string{"release/*"}, nil, "") {
		t.Error("expected event to match branch filter release/*")
	}
	if MatchEvent(event, []string{"push"}, []string{"main"}, nil, "") {
		t.Error("event should not match main branch")
	}
}
