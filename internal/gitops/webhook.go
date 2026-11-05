package gitops

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
)

// VerifyWebhookSignature verifies the cryptographic HMAC or secret token from the request.
func VerifyWebhookSignature(r *http.Request, payload []byte, secret string) (string, error) {
	if secret == "" {
		return "", errors.New("empty webhook secret configured")
	}

	// 1. Forgejo
	if sig := r.Header.Get("X-Forgejo-Signature"); sig != "" {
		if !verifyHMACSHA256(payload, secret, sig) {
			return "", errors.New("invalid Forgejo HMAC signature")
		}
		return "forgejo", nil
	}

	// 2. Gitea
	if sig := r.Header.Get("X-Gitea-Signature"); sig != "" {
		if !verifyHMACSHA256(payload, secret, sig) {
			return "", errors.New("invalid Gitea HMAC signature")
		}
		return "gitea", nil
	}

	// 3. GitHub
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		sigHex := strings.TrimPrefix(sig, "sha256=")
		if !verifyHMACSHA256(payload, secret, sigHex) {
			return "", errors.New("invalid GitHub HMAC signature")
		}
		return "github", nil
	}

	// 4. GitLab
	if token := r.Header.Get("X-Gitlab-Token"); token != "" {
		if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
			return "", errors.New("invalid GitLab secret token")
		}
		return "gitlab", nil
	}

	return "", errors.New("missing or unrecognized webhook signature headers")
}

func verifyHMACSHA256(payload []byte, secret, expectedHex string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedMAC := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(expectedMAC), []byte(expectedHex)) == 1
}

// WebhookEventType returns the event of a request: push, release, or the
// event of the provider for the ones that deploy nothing, such as ping.
func WebhookEventType(provider string, r *http.Request) string {
	event := ""
	switch provider {
	case "forgejo":
		event = r.Header.Get("X-Forgejo-Event")
		if event == "" {
			event = r.Header.Get("X-Gitea-Event")
		}
	case "gitea":
		event = r.Header.Get("X-Gitea-Event")
	case "github":
		event = r.Header.Get("X-GitHub-Event")
	case "gitlab":
		switch h := r.Header.Get("X-Gitlab-Event"); h {
		case "Push Hook", "Tag Push Hook":
			event = "push"
		case "Release Hook":
			event = "release"
		default:
			event = h
		}
	}
	return strings.ToLower(event)
}

// ParseWebhookPayload extracts metadata from Forgejo, Gitea, GitHub, or GitLab
// push and release payloads. The event comes from WebhookEventType.
func ParseWebhookPayload(provider, eventType string, payload []byte) (*WebhookEvent, error) {
	var raw struct {
		Ref         string `json:"ref"`
		After       string `json:"after"`
		CheckoutSHA string `json:"checkout_sha"`
		Deleted     bool   `json:"deleted"`
		Action      string `json:"action"`
		Tag         string `json:"tag"` // GitLab release
		Release     struct {
			TagName string `json:"tag_name"`
		} `json:"release"`
		Pusher struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"pusher"`
		UserName   string `json:"user_name"`
		UserEmail  string `json:"user_email"`
		Repository struct {
			CloneURL string `json:"clone_url"`
			HTMLURL  string `json:"html_url"`
			GitHTTP  string `json:"git_http_url"`
		} `json:"repository"`
		Commits []struct {
			ID      string `json:"id"`
			Message string `json:"message"`
			Author  struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
		} `json:"commits"`
	}

	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse %s payload: %w", provider, err)
	}

	event := &WebhookEvent{
		Provider:    provider,
		Event:       eventType,
		Ref:         raw.Ref,
		DeliveredAt: time.Now().UTC(),
		RawPayload:  payload,
	}

	// A release counts when it is published.
	if eventType == "release" {
		event.Tag = raw.Release.TagName
		if event.Tag == "" {
			event.Tag = raw.Tag
		}
		if raw.Action != "published" && raw.Action != "create" {
			event.Event = "release:" + raw.Action
		}
	}

	// Determine branch or tag
	if after, ok := strings.CutPrefix(raw.Ref, "refs/heads/"); ok {
		event.Branch = after
	} else if after, ok := strings.CutPrefix(raw.Ref, "refs/tags/"); ok {
		event.Tag = after
	}

	// Commit SHA. A deleted ref has the zero commit.
	event.CommitSHA = raw.After
	if event.CommitSHA == "" {
		event.CommitSHA = raw.CheckoutSHA
	}
	if event.CommitSHA == "" && len(raw.Commits) > 0 {
		event.CommitSHA = raw.Commits[len(raw.Commits)-1].ID
	}
	event.Deleted = raw.Deleted || zeroCommit.MatchString(event.CommitSHA)

	// Author and Message
	if raw.Pusher.Name != "" {
		event.Author = raw.Pusher.Name
	} else if raw.UserName != "" {
		event.Author = raw.UserName
	}
	if len(raw.Commits) > 0 {
		last := raw.Commits[len(raw.Commits)-1]
		event.Message = strings.TrimSpace(last.Message)
		if event.Author == "" {
			event.Author = last.Author.Name
		}
	}

	// Repo URL
	if raw.Repository.CloneURL != "" {
		event.RepoURL = raw.Repository.CloneURL
	} else if raw.Repository.GitHTTP != "" {
		event.RepoURL = raw.Repository.GitHTTP
	} else {
		event.RepoURL = raw.Repository.HTMLURL
	}

	return event, nil
}

// MatchEvent verifies whether the incoming event matches the binding filters.
// Without events only pushes count, and an event without a branch or tag, or
// a deleted one, has nothing to deploy.
func MatchEvent(event *WebhookEvent, filterEvents, filterBranches, filterTags []string, targetBranch string) bool {
	if event.Deleted {
		return false
	}

	// 1. Event Type Filter
	if len(filterEvents) == 0 {
		filterEvents = []string{"push"}
	}
	matched := false
	for _, e := range filterEvents {
		if strings.EqualFold(e, event.Event) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}

	// 2. Tag Filter
	if event.Tag != "" {
		return matchAny(filterTags, event.Tag)
	}

	// 3. Branch Filter
	if event.Branch == "" {
		return false
	}
	if len(filterBranches) > 0 {
		return matchAny(filterBranches, event.Branch)
	}
	if targetBranch == "" {
		targetBranch = "main"
	}
	return event.Branch == targetBranch
}

// matchAny matches with path patterns: * stops at a /, on every platform.
func matchAny(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return false
}
