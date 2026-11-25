package step

import "testing"

func TestUsedFunctions(t *testing.T) {
	defs := `log() { printf 'log %s\n' "$*"; }
die() {
  printf 'error %s\n' "$*"
  exit 1
}
# a calls die.
a() {
  die x
}

# b is not called.
b() {
  true
}
`
	want := `die() {
  printf 'error %s\n' "$*"
  exit 1
}
# a calls die.
a() {
  die x
}
`
	if got := UsedFunctions(defs, "a"); got != want {
		t.Fatalf("a: %q\nwant %q", got, want)
	}
	if got := UsedFunctions(defs, "log hello", "true"); got != `log() { printf 'log %s\n' "$*"; }`+"\n" {
		t.Fatalf("log: %q", got)
	}
	if got := UsedFunctions(defs, "echo nothing"); got != "" {
		t.Fatalf("nothing: %q", got)
	}
}
