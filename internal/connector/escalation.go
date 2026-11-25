package connector

import "fmt"

// Escalation is how a POSIX command of the deployer gets root on a node: the
// SSH user runs it through sudo or doas, or runs it as it is (the user is
// root, or the plan has no become).
type Escalation string

// The escalation methods.
const (
	EscalationNone Escalation = "none"
	EscalationSudo Escalation = "sudo"
	EscalationDoas Escalation = "doas"
)

// PosixCommand wraps a command of the POSIX dialect for a node. The command
// runs in sh (dash on Debian and Ubuntu, bash on RHEL, busybox ash on
// Alpine), not in the login shell of the SSH user, so it must not use bash
// features. password says that sudo reads the password of the user on
// stdin; doas cannot (the plan refuses become_password with doas).
func PosixCommand(cmd string, esc Escalation, password bool) string {
	inner := "sh -c " + ShellQuote(cmd)
	switch esc {
	case EscalationSudo:
		if password {
			return "sudo -S -p '' -- " + inner
		}
		return "sudo -n -- " + inner
	case EscalationDoas:
		return "doas -n -- " + inner
	}
	return inner
}

// ValidEscalation checks an escalation method; password says that the plan
// has a become_password.
func ValidEscalation(esc Escalation, password bool) error {
	switch esc {
	case EscalationNone, EscalationSudo:
		return nil
	case EscalationDoas:
		if password {
			return fmt.Errorf("doas cannot read a password (become_password): allow the SSH user without a password (permit nopass) or use sudo")
		}
		return nil
	}
	return fmt.Errorf("unknown escalation method %q (sudo, doas, none)", esc)
}

// Escalator is a connector whose escalation is chosen after it connected,
// from the facts of the node: the probe of the facts runs as the SSH user,
// then the commands run with the escalation the node has.
type Escalator interface {
	// ExecuteAsUser runs a POSIX command as the SSH user, without
	// escalation.
	ExecuteAsUser(cmd string) (string, error)
	// SetEscalation sets the escalation of the commands that follow; it is
	// ignored when the connection needs none (the user is root, the plan
	// has no become, a Windows node).
	SetEscalation(esc Escalation) error
}

// ExecuteAsUser runs a command without escalation when the connector can,
// and as usual otherwise (a local or a fake connector).
func ExecuteAsUser(conn Connector, cmd string) (string, error) {
	if e, ok := conn.(Escalator); ok {
		return e.ExecuteAsUser(cmd)
	}
	return conn.ExecuteCommand(cmd)
}

// SetEscalation sets the escalation of a connector that has one.
func SetEscalation(conn Connector, esc Escalation) error {
	if e, ok := conn.(Escalator); ok {
		return e.SetEscalation(esc)
	}
	return nil
}
