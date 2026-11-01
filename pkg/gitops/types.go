package gitops

import (
	"time"
)

// AuthCredentials holds authentication details for Git repositories.
type AuthCredentials struct {
	Username      string
	Password      string
	Token         string
	SSHPrivateKey string
	KnownHosts    string
}

// TLSConfig defines TLS options for Git and webhook requests.
type TLSConfig struct {
	InsecureSkipVerify bool
	CACertPEM          string
}

// FetchOptions contains parameters for an in-memory pack fetch.
type FetchOptions struct {
	Provider  string // forgejo, gitea, github, gitlab
	URL       string
	Branch    string
	Tag       string
	Ref       string
	CommitSHA string
	Subpath   string
	Auth      *AuthCredentials
	TLS       *TLSConfig
	APIURL    string // Custom base API URL for GitHub Enterprise or GitLab

	// ProjectPath is the project of a GitLab repository, by default the path
	// of its URL.
	ProjectPath string

	// VerifyKeys are armored PGP public keys. With them the pack needs a signed
	// commit or tag of one of them. An archive has no signatures, so it is
	// always cloned.
	VerifyKeys []string
}

// FetchResult contains the metadata and memory files of a fetched pack.
type FetchResult struct {
	Provider    string
	CommitSHA   string
	Branch      string
	Author      string
	Message     string
	PackFiles   map[string][]byte // Path -> file content in RAM
	PackName    string
	PackVersion string

	// Signer says who signed the commit or tag, when it was verified.
	Signer string
}

// WebhookEvent represents an ingested and verified webhook payload.
type WebhookEvent struct {
	Provider    string
	Event       string // push, release
	Ref         string // e.g. refs/heads/main or refs/tags/v1.0.0
	Branch      string
	Tag         string
	Deleted     bool // the push deleted the branch or tag
	CommitSHA   string
	Author      string
	Message     string
	RepoURL     string
	RawPayload  []byte
	DeliveredAt time.Time
}

// LogEntry represents an in-memory execution or audit event.
type LogEntry struct {
	ID          string    `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	BindingName string    `json:"binding_name"`
	Provider    string    `json:"provider"`
	Trigger     string    `json:"trigger"` // "webhook" | "on-demand"
	CommitSHA   string    `json:"commit_sha,omitempty"`
	Author      string    `json:"author,omitempty"`
	Message     string    `json:"message,omitempty"`
	Phase       string    `json:"phase"` // "TRIGGER", "FETCH", "RENDER", "APPLY", "SUCCESS", "FAILED"
	Details     string    `json:"details"`
	DurationMs  int64     `json:"duration_ms,omitempty"`
	Error       string    `json:"error,omitempty"`
}
