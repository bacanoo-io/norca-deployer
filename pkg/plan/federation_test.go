package plan

import (
	"strings"
	"testing"
)

func TestValidateFederationReplicationTokenFile(t *testing.T) {
	cases := map[string]struct {
		region, authoritative, tokenFile string
		err                              string
	}{
		"authoritative":                 {region: "eu", authoritative: "eu"},
		"replica with token file":       {region: "us", authoritative: "eu", tokenFile: "t"},
		"replica without token file":    {region: "us", authoritative: "eu", err: "replication_token_file is required"},
		"authoritative with token file": {region: "eu", authoritative: "eu", tokenFile: "t", err: "replication_token_file is for replica regions"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := validPlan()
			p.Metadata.Region = tc.region
			p.Security.ACL = &ACLConfig{Enabled: true, AutoBootstrap: true}
			p.Federation = &Federation{AuthoritativeRegion: tc.authoritative, ReplicationTokenFile: tc.tokenFile}
			err := p.Validate()
			if tc.err == "" {
				if err != nil {
					t.Fatalf("valid plan rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err %v, want %q", err, tc.err)
			}
		})
	}
}
