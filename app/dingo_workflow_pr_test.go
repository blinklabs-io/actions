package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
)

const (
	testConventional = ".github/workflows/conventional-commits.yml"
	testFanout       = ".github/workflows/release-fanout.yml"
)

type dingoPRFixture struct {
	branch, head, base                      string
	number                                  int
	proposed                                map[string]string
	trees, commits, refs, opened            int
	failCreate, unrelated, closedDuringSync bool
}

func (f *dingoPRFixture) client(t *testing.T) *github.Client {
	t.Helper()
	return github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		res := httptest.NewRecorder()
		res.Header().Set("Content-Type", "application/json")
		write := func(value any) {
			if err := json.NewEncoder(res).Encode(value); err != nil {
				t.Fatal(err)
			}
		}
		read := func(value any) {
			if err := json.NewDecoder(req.Body).Decode(value); err != nil {
				t.Fatal(err)
			}
		}
		full := "blinklabs-io/dingo"
		pr := func() map[string]any {
			repo := map[string]string{"full_name": full}
			return map[string]any{"state": "open", "number": f.number, "head": map[string]any{"ref": f.branch, "sha": f.head, "repo": repo}, "base": map[string]any{"ref": "main", "repo": repo}}
		}
		path := strings.TrimPrefix(req.URL.Path, "/repos/"+full)
		switch {
		case req.Method == "GET" && path == fmt.Sprintf("/pulls/%d", f.number):
			current := pr()
			if f.closedDuringSync {
				current["state"] = "closed"
			}
			write(current)
		case req.Method == "GET" && path == "/pulls":
			if req.URL.Query().Get("state") != "open" {
				t.Fatal("listing non-open pull requests")
			}
			if f.number > 0 && req.URL.Query().Get("head") == "blinklabs-io:"+f.branch {
				write([]any{pr()})
			} else {
				write([]any{})
			}
		case req.Method == "GET" && strings.HasPrefix(path, "/git/ref/heads/"):
			branch := strings.TrimPrefix(path, "/git/ref/heads/")
			switch {
			case branch == "main":
				write(map[string]any{"object": map[string]string{"sha": f.base}})
			case branch == f.branch && f.head != "":
				write(map[string]any{"object": map[string]string{"sha": f.head}})
			default:
				res.WriteHeader(404)
			}
		case req.Method == "GET" && path == "/compare/"+f.base+"..."+f.head:
			var files []map[string]string
			for path := range f.proposed {
				files = append(files, map[string]string{"filename": path, "sha": strings.TrimPrefix(path, ".github/workflows/"), "status": "modified"})
			}
			if f.unrelated {
				files = append(files, map[string]string{"filename": "go.mod", "status": "added"})
			}
			write(map[string]any{"merge_base_commit": map[string]string{"sha": f.base}, "files": files})
		case req.Method == "GET" && strings.HasPrefix(path, "/git/blobs/"):
			_, _ = io.WriteString(res, f.proposed[".github/workflows/"+strings.TrimPrefix(path, "/git/blobs/")])
		case req.Method == "GET" && path == "/git/commits/"+f.base:
			write(map[string]any{"sha": f.base, "tree": map[string]string{"sha": "main-tree"}})
		case req.Method == "POST" && path == "/git/trees":
			var body struct {
				Base string `json:"base_tree"`
				Tree []github.TreeEntry
			}
			read(&body)
			if body.Base != "main-tree" {
				t.Fatal("proposal tree not based on observed main")
			}
			f.trees++
			f.proposed = map[string]string{}
			for _, entry := range body.Tree {
				if !slices.Contains([]string{testConventional, testFanout}, entry.GetPath()) || entry.Content == nil {
					t.Fatalf("unexpected file operation %+v", entry)
				}
				f.proposed[entry.GetPath()] = entry.GetContent()
			}
			write(map[string]string{"sha": "proposal-tree"})
		case req.Method == "POST" && path == "/git/commits":
			var body struct {
				Message, Tree string
				Parents       []string
			}
			read(&body)
			if !slices.Contains(body.Parents, f.base) || (f.head != "" && !slices.Contains(body.Parents, f.head)) || !strings.Contains(body.Message, "Signed-off-by: blinklabs-governance[bot]") {
				t.Fatalf("unexpected proposal commit %+v", body)
			}
			f.commits++
			write(map[string]string{"sha": fmt.Sprintf("commit-%d", f.commits)})
		case req.Method == "POST" && path == "/git/refs":
			var body struct{ Ref, SHA string }
			read(&body)
			if f.head != "" || body.Ref != "refs/heads/"+dingoSyncBranch {
				t.Fatalf("unexpected branch creation %+v", body)
			}
			f.refs++
			f.branch = dingoSyncBranch
			f.head = body.SHA
			write(map[string]string{"ref": body.Ref})
		case req.Method == "POST" && path == "/graphql":
			var body struct {
				Variables struct {
					Input struct {
						RefUpdates []struct {
							Name, BeforeOID, AfterOID string
							Force                     bool
						}
					}
				}
			}
			read(&body)
			updates := body.Variables.Input.RefUpdates
			if len(updates) != 1 || updates[0].Name != "refs/heads/"+f.branch || f.branch == "main" || updates[0].Force || updates[0].BeforeOID != f.head {
				t.Fatalf("unexpected ref update %+v", updates)
			}
			f.head = updates[0].AfterOID
			f.refs++
			write(map[string]any{"data": map[string]any{"updateRefs": map[string]string{"__typename": "UpdateRefsPayload"}}})
		case req.Method == "POST" && path == "/pulls":
			var body github.NewPullRequest
			read(&body)
			if body.GetHead() != dingoSyncBranch || body.GetBase() != "main" || f.number != 0 || f.head == "" {
				t.Fatalf("unexpected pull request %+v", body)
			}
			if f.failCreate {
				res.WriteHeader(503)
				break
			}
			f.opened++
			f.number = 7000
			write(pr())
		default:
			t.Fatalf("unexpected GitHub request %s %s", req.Method, req.URL)
		}
		return res.Result(), nil
	})})
}

func reconcileDingoTestBatch(t *testing.T, f *dingoPRFixture, ops ...fileOp) (string, error) {
	t.Helper()
	var out strings.Builder
	report := newDriftReport(&out)
	report.beginRepo("blinklabs-io/dingo", true)
	ctx := context.WithValue(context.Background(), driftContextKey{}, report)
	batch := &commitBatch{ops: ops, baseCommitSHA: f.base, repositoryID: "dingo-id"}
	for _, op := range ops {
		reportManagedFile(ctx, batch, op.path, "old", "new", "pending")
	}
	err := flushBatch(ctx, f.client(t), "blinklabs-io", "dingo", "main", batch)
	if err := report.render(); err != nil {
		t.Fatal(err)
	}
	return out.String(), err
}

func TestDingoWorkflowPRLifecycle(t *testing.T) {
	ops := []fileOp{{path: testConventional, content: []byte("cc")}, {path: testFanout, content: []byte("fanout")}}
	f := &dingoPRFixture{base: "main-tip", branch: dingoInitialBranch, head: "existing", number: 6713, proposed: map[string]string{testConventional: "cc", testFanout: "fanout"}}
	check := func(wantPR int) {
		t.Helper()
		out, err := reconcileDingoTestBatch(t, f, ops...)
		if err != nil || !strings.Contains(out, fmt.Sprintf("https://github.com/blinklabs-io/dingo/pull/%d", wantPR)) || !strings.Contains(out, "- pending: 2") {
			t.Fatalf("unexpected report: %v\n%s", err, out)
		}
	}
	check(6713)
	if f.refs != 0 || f.commits != 0 || f.opened != 0 {
		t.Fatal("matching migration PR lost its review")
	}
	ops[0].content = []byte("new caller")
	check(6713)
	check(6713)
	if f.proposed[testConventional] != "new caller" || f.commits != 1 || f.refs != 1 || f.opened != 0 {
		t.Fatalf("migration PR update was not idempotent: %+v", f)
	}
	f.number = 0
	f.head = ""
	f.proposed = nil
	f.failCreate = true
	out, err := reconcileDingoTestBatch(t, f, ops...)
	if err == nil || !strings.Contains(out, "- failed: 2") {
		t.Fatal("failed PR creation reported as success")
	}
	f.failCreate = false
	check(7000)
	check(7000)
	if f.opened != 1 || f.commits != 2 || f.refs != 2 {
		t.Fatalf("retry did not reuse published branch: %+v", f)
	}
	out, err = reconcileDingoTestBatch(t, f)
	if err != nil || !strings.Contains(out, "- pending: 0") || f.opened != 1 || f.refs != 2 {
		t.Fatal("matching main started another proposal")
	}
}

func TestDingoWorkflowPRClosedDuringSync(t *testing.T) {
	f := &dingoPRFixture{base: "main-tip", branch: dingoInitialBranch, head: "existing", number: 6713,
		proposed: map[string]string{testConventional: "cc"}, closedDuringSync: true}
	for _, want := range []string{"cc", "changed"} {
		out, err := reconcileDingoTestBatch(t, f, fileOp{path: testConventional, content: []byte(want)})
		if err == nil || !strings.Contains(out, "- failed: 1") || strings.Contains(out, "pending workflow pull request:") || f.refs > 0 {
			t.Fatalf("reported or updated closed PR: %v, %s", err, out)
		}
	}
}

func TestDingoWorkflowPRScope(t *testing.T) {
	f := &dingoPRFixture{base: "main-tip", branch: dingoInitialBranch, head: "existing", number: 6713, unrelated: true}
	if _, err := reconcileDingoTestBatch(t, f, fileOp{path: testConventional, content: []byte("cc")}); err == nil {
		t.Fatal("accepted unrelated changes already on the PR branch")
	}
	f.unrelated = false
	for _, op := range []fileOp{{path: "README.md", content: []byte("x")}, {path: testFanout, remove: true}} {
		if _, err := reconcileDingoTestBatch(t, f, op); err == nil {
			t.Fatalf("accepted unsafe update to %s", op.path)
		}
	}
	if f.commits+f.refs+f.opened != 0 {
		t.Fatal("modified branch outside intended scope")
	}
}

func TestMissingDingoTokenStillSyncsNextRepository(t *testing.T) {
	t.Chdir("..")
	fake := &recordingGitHub{contents: map[string]string{}}
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		r := httptest.NewRecorder()
		if req.Method == "GET" && (req.URL.Path == "/repos/blinklabs-io/dingo" || req.URL.Path == "/repos/o/r") {
			r.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(r, `{"full_name":%q,"private":false,"visibility":"public","default_branch":"main","id":12,"node_id":"repo-id"}`, strings.TrimPrefix(req.URL.Path, "/repos/"))
		} else {
			fake.handler().ServeHTTP(r, req)
		}
		return r.Result(), nil
	})})
	var out strings.Builder
	repos := []RepoConfig{{Name: "blinklabs-io/dingo"}, {Name: "o/r", Dependabot: "version: 2\n"}}
	ids, err := reconcileRepositoriesWithIdentities(context.Background(), client, nil, repos, newDriftReport(&out))
	if err == nil || ids["o/r"] == 0 || !fake.saw("POST", "/graphql") || fake.saw("POST", "/pulls") {
		t.Fatalf("expected Dingo error and successful next repo: %v, %v, %v", err, ids, fake.requests)
	}
}
