package connector

import (
	"strings"
	"testing"
)

func TestPosixCommand(t *testing.T) {
	cmd := `echo "it's" && id -u`
	quoted := ShellQuote(cmd)
	tests := []struct {
		name     string
		esc      Escalation
		password bool
		want     string
	}{
		{"no escalation runs sh, not the login shell", EscalationNone, false, "sh -c " + quoted},
		{"sudo without a password", EscalationSudo, false, "sudo -n -- sh -c " + quoted},
		{"sudo reads the password on stdin", EscalationSudo, true, "sudo -S -p '' -- sh -c " + quoted},
		{"doas", EscalationDoas, false, "doas -n -- sh -c " + quoted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PosixCommand(cmd, tt.esc, tt.password); got != tt.want {
				t.Fatalf("PosixCommand = %s, want %s", got, tt.want)
			}
			if strings.Contains(PosixCommand(cmd, tt.esc, tt.password), "bash") {
				t.Fatal("the command runs in bash")
			}
		})
	}
}

func TestValidEscalation(t *testing.T) {
	for _, esc := range []Escalation{EscalationNone, EscalationSudo, EscalationDoas} {
		if err := ValidEscalation(esc, false); err != nil {
			t.Errorf("%s: %v", esc, err)
		}
	}
	if err := ValidEscalation(EscalationSudo, true); err != nil {
		t.Errorf("sudo with a password: %v", err)
	}
	if err := ValidEscalation(EscalationDoas, true); err == nil || !strings.Contains(err.Error(), "become_password") {
		t.Errorf("doas with a password: %v", err)
	}
	if err := ValidEscalation("su", false); err == nil {
		t.Error("an unknown method is accepted")
	}
}

// A connector without an escalation of its own runs the commands of the
// probe as usual.
func TestExecuteAsUserFallsBack(t *testing.T) {
	f := NewFakeConnector()
	f.Responses["id -u"] = "1000\n"
	if out, err := ExecuteAsUser(f, "id -u"); err != nil || out != "1000\n" {
		t.Fatalf("ExecuteAsUser = %q, %v", out, err)
	}
	if err := SetEscalation(f, EscalationDoas); err != nil {
		t.Fatal(err)
	}
}
