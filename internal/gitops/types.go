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

// FetchOptions contains parameters for an in-memory fetch of a stack
// directory.
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

	// VerifyKeys are armored PGP public keys. With them the stack needs a
	// signed commit or tag of one of them. An archive has no signatures, so
	// it is always cloned.
	VerifyKeys []string
}

// FetchResult is a fetched stack directory, in memory.
type FetchResult struct {
	Provider  string
	CommitSHA string

	// Ref is the branch, the tag or the revision that was fetched; empty
	// when only a commit was named.
	Ref string

	// Files are the files of the stack directory, by their paths below it.
	Files map[string][]byte

	// Signer is who signed the commit or the tag, when it was verified, and
	// Signed what carries the signature: "commit" or "tag <name>".
	Signer string
	Signed string
}

// Signature says what was verified: "commit signed by <key>" or "tag <name>
// signed by <key>". It is empty when nothing was.
func (r *FetchResult) Signature() string {
	if r.Signer == "" {
		return ""
	}
	return r.Signed + " signed by " + r.Signer
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
	Stack       string    `json:"stack,omitempty"`     // the stack, once a plan of the servers named it
	Operation   string    `json:"operation,omitempty"` // the operation of the servers, once it started
	Author      string    `json:"author,omitempty"`
	Message     string    `json:"message,omitempty"`
	Phase       string    `json:"phase"` // "TRIGGER", "FETCH", "VERIFY", "PLAN", "APPLY", "OPERATION", "SUCCESS", "FAILED"
	Details     string    `json:"details"`
	DurationMs  int64     `json:"duration_ms,omitempty"`
	Error       string    `json:"error,omitempty"`
}
