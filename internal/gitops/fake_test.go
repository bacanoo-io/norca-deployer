// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// stackHCL is the stack.hcl of the stacks of the tests. The version of its
// module tells the commits of a test repository apart.
const stackHCL = "rigger {\n  name      = \"demo\"\n  namespace = \"production\"\n}\n\nmodule \"cache\" {\n  source  = \"builtin/redis\"\n  version = \"0.1.0\"\n}\n"

const sha = "7f8b91a24d8c910283e5891d1209384bcdaef190"

// conflictText is what the servers answer while a stack has an operation.
const conflictText = "the stack demo has a running operation, demo-20261125-115900-ffff: cancel it or wait for its end"

// fakeAnswer is an answer of the fake cluster. The zero value stands for
// what the servers do by default: a plan with one install, an operation
// that starts.
type fakeAnswer struct {
	status int
	body   string
	err    error
}

// conflict is the answer while another operation of the stack runs.
var conflict = fakeAnswer{status: http.StatusConflict, body: conflictText}

// fakeRelease is a release of a plan.
type fakeRelease struct{ name, action, takesOver string }

// planOf is the answer of a plan of the stack demo with these releases,
// with more of the plan (secrets, generate) and of the answer (Warnings) as
// JSON members.
func planOf(more string, releases ...fakeRelease) fakeAnswer {
	list := make([]map[string]any, 0, len(releases))
	for _, r := range releases {
		rp := map[string]any{
			"release": map[string]string{"namespace": "production", "name": r.name},
			"action":  r.action,
			"module":  map[string]string{"name": "redis", "source": "builtin/redis", "version": "0.1.0"},
		}
		if r.takesOver != "" {
			rp["takes_over"] = r.takesOver
		}
		if r.action == "upgrade" {
			rp["changes"] = []map[string]string{{"kind": "Job", "name": r.name, "action": "update"}}
		}
		list = append(list, rp)
	}
	plan := map[string]any{"stack": "demo", "region": "eu", "releases": list, "outputs": []any{}}
	answer := map[string]any{"Plan": plan, "Index": 7}
	if more != "" {
		var extra struct {
			Plan   map[string]any
			Answer map[string]any
		}
		if err := json.Unmarshal([]byte(more), &extra); err != nil {
			panic(err)
		}
		maps.Copy(plan, extra.Plan)
		maps.Copy(answer, extra.Answer)
	}
	body, err := json.Marshal(answer)
	if err != nil {
		panic(err)
	}
	return fakeAnswer{status: http.StatusOK, body: string(body)}
}

// fakeRequest is a body of a plan or an apply that the fake cluster got.
type fakeRequest struct {
	Path string
	// Keys are the members of the body.
	Keys       map[string]json.RawMessage
	Files      []byte
	ValuesJSON json.RawMessage
	Prune      bool
	Origin     map[string]string
}

// fakeNorca answers the stacks API of the servers by a script, and records
// what it is asked.
type fakeNorca struct {
	mu       sync.Mutex
	calls    []string
	requests []fakeRequest

	// refuse answers every call with this status.
	refuse int

	// plans and applies are the answers in their order; the last one stays.
	plans   []fakeAnswer
	applies []fakeAnswer
	// ends are the phases the operations end with, in the order they start;
	// the last one stays, and none is Succeeded. opError is the error of
	// one that does not succeed.
	ends    []string
	opError string
	// logs and objects answer the n-th read (from 0) of the log and of the
	// object of an operation; nil, or a nil answer, is the default.
	logs    func(ctx context.Context, n int) *fakeAnswer
	objects func(ctx context.Context, n int) *fakeAnswer

	started     int
	logReads    int
	objectReads int
}

// operationName is the name of the n-th operation the fake starts.
func operationName(n int) string { return fmt.Sprintf("demo-20261125-120000-%04d", n) }

func (f *fakeNorca) API(ctx context.Context, method, path string, body []byte) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+path)
	if f.refuse != 0 {
		return f.refuse, "refused", nil
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}

	u, err := url.Parse(path)
	if err != nil {
		return 0, "", err
	}
	var a fakeAnswer
	switch {
	case method == http.MethodPut && u.Path == "/v1/rigger/stack/plan":
		f.record(path, body)
		if a = next(&f.plans); a == (fakeAnswer{}) {
			a = planOf("", fakeRelease{name: "cache", action: "install"})
		}
	case method == http.MethodPut && u.Path == "/v1/rigger/stack/apply":
		f.record(path, body)
		if a = next(&f.applies); a == (fakeAnswer{}) {
			a = f.start()
		}
	case method == http.MethodGet && strings.HasPrefix(u.Path, "/v1/rigger/operation/") && strings.HasSuffix(u.Path, "/log"):
		name := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/v1/rigger/operation/"), "/log")
		n := f.logReads
		f.logReads++
		if f.logs != nil {
			f.mu.Unlock()
			scripted := f.logs(ctx, n)
			f.mu.Lock()
			if scripted != nil {
				return scripted.status, scripted.body, scripted.err
			}
		}
		after, _ := strconv.ParseUint(u.Query().Get("after"), 10, 64)
		a = f.logPage(name, after)
	case method == http.MethodGet && strings.HasPrefix(u.Path, "/v1/object/rigger.norca.io/Operation/"):
		name := strings.TrimPrefix(u.Path, "/v1/object/rigger.norca.io/Operation/")
		n := f.objectReads
		f.objectReads++
		if f.objects != nil {
			f.mu.Unlock()
			scripted := f.objects(ctx, n)
			f.mu.Lock()
			if scripted != nil {
				return scripted.status, scripted.body, scripted.err
			}
		}
		a = f.object(name)
	default:
		return http.StatusNotFound, "unknown path", nil
	}
	return a.status, a.body, a.err
}

// next takes the next answer of a script; its last one stays.
func next(script *[]fakeAnswer) fakeAnswer {
	if len(*script) == 0 {
		return fakeAnswer{}
	}
	a := (*script)[0]
	if len(*script) > 1 {
		*script = (*script)[1:]
	}
	return a
}

func (f *fakeNorca) record(path string, body []byte) {
	req := fakeRequest{Path: path}
	if err := json.Unmarshal(body, &req.Keys); err != nil {
		panic(err)
	}
	if err := json.Unmarshal(body, &req); err != nil {
		panic(err)
	}
	f.requests = append(f.requests, req)
}

// start starts an operation, as an apply answers it.
func (f *fakeNorca) start() fakeAnswer {
	name := operationName(f.started)
	f.started++
	body, _ := json.Marshal(map[string]any{
		"namespace": "production", "name": name,
		"spec":   map[string]any{"stack": "demo", "action": "apply"},
		"status": map[string]any{"phase": "Running", "plan": []map[string]any{{"namespace": "production", "release": "cache", "action": "install"}}},
	})
	return fakeAnswer{status: http.StatusOK, body: string(body)}
}

// end is the phase an operation ends with.
func (f *fakeNorca) end(name string) string {
	if len(f.ends) == 0 {
		return opSucceeded
	}
	for n := range f.ends {
		if operationName(n) == name {
			return f.ends[n]
		}
	}
	return f.ends[len(f.ends)-1]
}

// logPage is the log of an operation in two pages: the first of a running
// operation, the second its end.
func (f *fakeNorca) logPage(name string, after uint64) fakeAnswer {
	page := map[string]any{"Phase": "Running", "Finished": false, "Lines": []map[string]any{
		{"Seq": 1, "Line": map[string]any{"time": "2026-11-25T12:00:01Z", "level": "INFO", "msg": "started", "op": "release.apply", "release": "cache"}},
		{"Seq": 2, "Line": map[string]any{"time": "2026-11-25T12:00:01Z", "level": "DEBUG", "msg": "evaluated", "release": "cache"}},
	}}
	if after >= 2 {
		page = map[string]any{"Phase": f.end(name), "Finished": true, "Lines": []map[string]any{
			{"Seq": 3, "Line": map[string]any{"time": "2026-11-25T12:00:02Z", "level": "INFO", "msg": "done", "release": "cache", "revision": 1}},
		}}
	}
	body, _ := json.Marshal(page)
	return fakeAnswer{status: http.StatusOK, body: string(body)}
}

// object is an operation as the object API answers it. The spec and the
// status of an object are stored as they were sent, without a type, so the
// servers write a whole number of them with a fraction: 1.0.
func (f *fakeNorca) object(name string) fakeAnswer {
	status := map[string]any{"phase": f.end(name), "outcomes": []map[string]any{
		{"namespace": "production", "release": "cache", "action": "install", "revision": json.RawMessage("1.0")},
		{"namespace": "production", "release": "db", "action": "unchanged", "revision": json.RawMessage("4.0")},
	}}
	if f.end(name) != opSucceeded {
		status["error"] = f.opError
		status["outcomes"] = []map[string]any{{"namespace": "production", "release": "cache", "action": "install", "failed": true}}
	}
	body, _ := json.Marshal(map[string]any{
		"Group": "rigger.norca.io", "Kind": "Operation", "Namespace": "production", "Name": name,
		"Spec": map[string]any{"stack": "demo", "action": "apply"}, "Status": status,
	})
	return fakeAnswer{status: http.StatusOK, body: string(body)}
}

// count is the number of calls that start with a prefix.
func (f *fakeNorca) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// called returns the calls so far.
func (f *fakeNorca) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// asked returns the bodies of the plans and the applies so far.
func (f *fakeNorca) asked() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

const (
	planCall  = "PUT /v1/rigger/stack/plan"
	applyCall = "PUT /v1/rigger/stack/apply"
)
