package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-github/v60/github"
)

var testLifecycleKey = []byte("0123456789abcdef0123456789abcdef")
var testLifecycleSHA = strings.Repeat("a", 40)
var testLifecycleStateSHA = strings.Repeat("b", 40)

const testLifecycleRuleset = `{"name":"governance-lifecycle-state","target":"branch","enforcement":"active","conditions":{"ref_name":{"include":["refs/heads/governance-applied-state"],"exclude":[]}},"bypass_actors":[{"actor_id":4081860,"actor_type":"Integration","bypass_mode":"always"}],"rules":[{"type":"creation"},{"type":"update","parameters":{"update_allows_fetch_and_merge":false}},{"type":"deletion"},{"type":"non_fast_forward"}]}`

func TestExplicitRepositoriesRejectAmbiguousNames(t *testing.T) {
	for _, tt := range []struct {
		name    string
		repos   []RepoConfig
		wantErr bool
	}{
		{"missing list", nil, true},
		{"explicit empty list", []RepoConfig{}, false},
		{"valid", []RepoConfig{{Name: "org/.github"}}, false},
		{"foreign owner", []RepoConfig{{Name: "other/project"}}, true},
		{"case-insensitive duplicate", []RepoConfig{{Name: "org/project"}, {Name: "ORG/PROJECT"}}, true},
		{"path traversal", []RepoConfig{{Name: "org/.."}}, true},
		{"query injection", []RepoConfig{{Name: "org/project?x=1"}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateExplicitRepositories(tt.repos, "org"); (err != nil) != tt.wantErr {
				t.Fatalf("validation error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestTransitionCandidates(t *testing.T) {
	markedRepo := RepoConfig{Name: "org/example", ArchiveOnRemoval: true}
	unmarkedRepo := RepoConfig{Name: "org/example"}
	other := RepoConfig{Name: "org/other"}
	key := lifecycleNameKey(testLifecycleKey, markedRepo.Name)
	marks := map[string]string{key: lifecycleIdentity(testLifecycleKey, markedRepo.Name, 42)}
	for _, tt := range []struct {
		name        string
		previous    []RepoConfig
		transitions [][]RepoConfig
		marked      map[string]string
		want        bool
	}{
		{"marked entry alone", []RepoConfig{markedRepo}, [][]RepoConfig{{markedRepo, other}}, marks, false},
		{"applied marker then removed", []RepoConfig{markedRepo}, [][]RepoConfig{{markedRepo, other}, {other}}, marks, true},
		{"unmarked removal", []RepoConfig{unmarkedRepo}, [][]RepoConfig{{other}}, marks, false},
		{"no successfully applied marker", []RepoConfig{markedRepo}, [][]RepoConfig{{other}}, map[string]string{}, false},
		{"intermediate pause cancels", []RepoConfig{markedRepo}, [][]RepoConfig{{{Name: markedRepo.Name, ArchiveOnRemoval: true, SyncPaused: true}, other}, {other}}, marks, false},
		{"intermediate unmark cancels", []RepoConfig{markedRepo}, [][]RepoConfig{{unmarkedRepo, other}, {other}}, marks, false},
		{"reintroduction cancels old intent", []RepoConfig{markedRepo}, [][]RepoConfig{{other}, {markedRepo, other}, {other}}, marks, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, got := transitionCandidates(tt.previous, tt.transitions, tt.marked, testLifecycleKey)[key]
			if got != tt.want {
				t.Fatalf("candidate = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLifecycleCreationUsesPublicDefaultAndFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		name       string
		private    bool
		lookupCode int
		wantCreate bool
	}{
		{"public default", false, http.StatusNotFound, true},
		{"explicit private", true, http.StatusNotFound, true},
		{"permission error", false, http.StatusForbidden, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			creates := 0
			var output strings.Builder
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				switch {
				case req.Method == http.MethodGet && req.URL.Path == "/repos/org/example":
					r.WriteHeader(tt.lookupCode)
					_, _ = io.WriteString(r, `{"message":"not available"}`)
				case req.Method == http.MethodGet && req.URL.Path == "/repos/org/actions/git/ref/heads/main":
					_, _ = fmt.Fprintf(r, `{"object":{"sha":%q}}`, testLifecycleSHA)
				case req.Method == http.MethodPost && req.URL.Path == "/orgs/org/repos":
					creates++
					var body struct {
						Name     string `json:"name"`
						Private  bool   `json:"private"`
						AutoInit bool   `json:"auto_init"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.Name != "example" || body.Private != tt.private || !body.AutoInit || !strings.Contains(output.String(), "planned change") {
						t.Errorf("invalid creation request or missing prior checkpoint: %+v", body)
					}
					r.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprintf(r, `{"full_name":"org/example","private":%t}`, tt.private)
				default:
					t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
				}
				return r.Result(), nil
			})})
			controller := &lifecycleController{owner: "org", repo: "actions", revision: testLifecycleSHA}
			var private *bool
			if tt.private {
				private = github.Bool(true)
			}
			err := controller.ensureRepositories(context.Background(), client, []RepoConfig{{Name: "org/example", Private: private}}, newDriftReport(&output))
			if (err == nil) != tt.wantCreate || (creates == 1) != tt.wantCreate {
				t.Fatalf("create calls=%d error=%v", creates, err)
			}
			if strings.Contains(output.String(), "org/example") {
				t.Fatal("unknown repository name leaked")
			}
		})
	}
}

func TestExistingRepositoryIgnoresCreationVisibility(t *testing.T) {
	for _, tt := range []struct {
		name          string
		actualPrivate bool
		createPrivate bool
	}{
		{"existing public, private creation setting", false, true},
		{"existing private, public creation setting", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Method != http.MethodGet || req.URL.Path != "/repos/org/example" {
					t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
				}
				r := httptest.NewRecorder()
				_, _ = fmt.Fprintf(r, `{"full_name":"org/example","private":%t}`, tt.actualPrivate)
				return r.Result(), nil
			})})
			controller := &lifecycleController{owner: "org", repo: "actions", revision: testLifecycleSHA}
			repos := []RepoConfig{{Name: "org/example", Private: github.Bool(tt.createPrivate)}}
			if err := controller.ensureRepositories(context.Background(), client, repos, newDriftReport(io.Discard)); err != nil {
				t.Fatalf("existing repository blocked by creation setting: %v", err)
			}
			if requests != 1 {
				t.Fatalf("got %d requests, want one read", requests)
			}
		})
	}
}

func TestLifecycleArchiveVerifiesIdentityAndUsesNodeID(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(fmt.Sprintf("free=%t", free), func(t *testing.T) {
			for _, tt := range []struct {
				name         string
				id           int
				fork         bool
				fullName     string
				archived     bool
				discovered   bool
				wantMutation bool
				wantErr      bool
			}{
				{"applied marked repository", 42, false, "org/example", false, false, true, false},
				{"replaced repository", 43, false, "org/example", false, false, false, true},
				{"fork", 42, true, "org/example", false, false, false, true},
				{"redirected repository", 42, false, "org/renamed", false, false, false, true},
				{"controller main changed", 42, false, "org/example", false, false, false, true},
				{"applied state changed", 42, false, "org/example", false, false, false, true},
				{"already archived", 42, false, "org/example", true, false, false, false},
				{"now discovered", 42, false, "org/example", false, true, false, false},
			} {
				t.Run(tt.name, func(t *testing.T) {
					writes := 0
					client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
						r := httptest.NewRecorder()
						r.Header().Set("Content-Type", "application/json")
						switch {
						case req.Method == http.MethodGet && req.URL.Path == "/repos/org/example":
							_, _ = fmt.Fprintf(r, `{"id":%d,"node_id":"repo-node","full_name":%q,"fork":%t,"archived":%t,"private":true}`, tt.id, tt.fullName, tt.fork, tt.archived)
						case req.Method == http.MethodGet && req.URL.Path == "/repos/org/actions/git/ref/heads/main":
							head := testLifecycleSHA
							if tt.name == "controller main changed" {
								head = strings.Repeat("d", 40)
							}
							_, _ = fmt.Fprintf(r, `{"object":{"sha":%q}}`, head)
						case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/git/ref/heads/"+lifecycleBranch):
							head := testLifecycleStateSHA
							if tt.name == "applied state changed" {
								head = strings.Repeat("d", 40)
							}
							_, _ = fmt.Fprintf(r, `{"object":{"sha":%q}}`, head)
						case req.Method == http.MethodGet && req.URL.Path == "/repos/org/actions/rulesets":
							if free {
								t.Fatal("Free archival queried paid rulesets")
							}
							_, _ = io.WriteString(r, `[{"id":7,"name":"governance-lifecycle-state"}]`)
						case req.Method == http.MethodGet && req.URL.Path == "/repos/org/actions/rulesets/7":
							_, _ = io.WriteString(r, testLifecycleRuleset)
						case req.Method == http.MethodPost && req.URL.Path == "/graphql":
							writes++
							var body struct {
								Query     string            `json:"query"`
								Variables map[string]string `json:"variables"`
							}
							if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
								t.Fatal(err)
							}
							if body.Variables["id"] != "repo-node" || !strings.Contains(body.Query, "archiveRepository") {
								t.Fatalf("unexpected mutation: %+v", body)
							}
							_, _ = io.WriteString(r, `{"data":{"archiveRepository":{"repository":{"id":"repo-node","isArchived":true}}}}`)
						default:
							t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
						}
						return r.Result(), nil
					})})
					name := "org/example"
					key := lifecycleNameKey(testLifecycleKey, name)
					c := &lifecycleController{trustedWriters: free, owner: "org", repo: "actions", revision: testLifecycleSHA, stateHead: testLifecycleStateSHA, key: testLifecycleKey,
						state: lifecycleState{Marked: map[string]string{key: lifecycleIdentity(testLifecycleKey, name, 42)}}}
					var current []RepoConfig
					if tt.discovered {
						current = []RepoConfig{{Name: name}}
					}
					var output strings.Builder
					report := newDriftReport(&output)
					err := c.archiveRemoved(context.Background(), client, map[string]RepoConfig{key: {Name: name}}, current, report)
					if (err != nil) != tt.wantErr || (writes == 1) != tt.wantMutation {
						t.Fatalf("writes=%d err=%v", writes, err)
					}
					if err := report.render(); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(output.String(), name) || strings.Contains(output.String(), "repo-node") {
						t.Fatal("private repository leaked in report")
					}
				})
			}
		})
	}
}

func TestLifecycleStateSignatureHidesIdentityAndDetectsTampering(t *testing.T) {
	c := &lifecycleController{owner: "org", repo: "actions", key: testLifecycleKey}
	name := "org/example-private"
	state := lifecycleState{Version: 1, Revision: testLifecycleSHA, Marked: map[string]string{
		lifecycleNameKey(c.key, name): lifecycleIdentity(c.key, name, 42),
	}}
	state.Signature = c.stateSignature(state)
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), name) || strings.Contains(string(data), `:42`) {
		t.Fatalf("state leaked identity: %s", data)
	}
	state.Revision = testLifecycleStateSHA
	if state.Signature == c.stateSignature(state) {
		t.Fatal("signature did not detect a changed revision")
	}
}

func TestLifecycleStateRequiresAppOnlyBranchProtection(t *testing.T) {
	for _, tt := range []struct {
		name, ruleset string
		wantErr       bool
	}{
		{"governance app only", testLifecycleRuleset, false},
		{"GitHub omits disabled update parameter", strings.Replace(testLifecycleRuleset, `,"parameters":{"update_allows_fetch_and_merge":false}`, "", 1), false},
		{"missing update restriction", strings.Replace(testLifecycleRuleset, `{"type":"update","parameters":{"update_allows_fetch_and_merge":false}},`, "", 1), true},
		{"update restriction allows fetch and merge", strings.Replace(testLifecycleRuleset, `"update_allows_fetch_and_merge":false`, `"update_allows_fetch_and_merge":true`, 1), true},
		{"different app", strings.Replace(testLifecycleRuleset, `4081860`, `42`, 1), true},
		{"broad target", strings.Replace(testLifecycleRuleset, `refs/heads/governance-applied-state`, `refs/heads/*`, 1), true},
		{"disabled", strings.Replace(testLifecycleRuleset, `"active"`, `"disabled"`, 1), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				if req.URL.Path == "/repos/org/actions/rulesets" {
					_, _ = io.WriteString(r, `[{"id":7,"name":"governance-lifecycle-state"}]`)
				} else {
					_, _ = io.WriteString(r, tt.ruleset)
				}
				return r.Result(), nil
			})})
			c := &lifecycleController{owner: "org", repo: "actions"}
			if err := c.requireStateProtection(context.Background(), client); (err != nil) != tt.wantErr {
				t.Fatalf("protection error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

func TestLifecycleStatePersistsAndRejectsTampering(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(fmt.Sprintf("free=%t", free), func(t *testing.T) {
			var stored string
			var stateHead string
			mainHead := testLifecycleSHA
			commits := 0
			client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
				r := httptest.NewRecorder()
				r.Header().Set("Content-Type", "application/json")
				switch {
				case req.Method == http.MethodGet && req.URL.Path == "/repos/org/actions":
					_, _ = io.WriteString(r, `{"full_name":"org/actions","node_id":"controller-node"}`)
				case req.Method == http.MethodGet && req.URL.Path == "/repos/org/actions/rulesets":
					if free {
						t.Fatal("Free mode queried paid state rulesets")
					}
					_, _ = io.WriteString(r, `[{"id":7,"name":"governance-lifecycle-state"}]`)
				case req.Method == http.MethodGet && req.URL.Path == "/repos/org/actions/rulesets/7":
					_, _ = io.WriteString(r, testLifecycleRuleset)
				case req.Method == http.MethodGet && req.URL.Path == "/repos/org/actions/git/ref/heads/main":
					_, _ = fmt.Fprintf(r, `{"object":{"sha":%q}}`, mainHead)
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/git/ref/heads/"+lifecycleBranch):
					if stateHead == "" {
						r.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(r, `{"message":"Not Found"}`)
					} else {
						_, _ = fmt.Fprintf(r, `{"object":{"sha":%q}}`, stateHead)
					}
				case req.Method == http.MethodGet && req.URL.Path == "/repos/org/actions/contents/"+lifecyclePath:
					_, _ = fmt.Fprintf(r, `{"type":"file","encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(stored)))
				case req.Method == http.MethodPost && req.URL.Path == "/repos/org/actions/git/trees":
					var body struct {
						Tree []struct{ Path, Content string } `json:"tree"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil || len(body.Tree) != 1 || body.Tree[0].Path != lifecyclePath {
						t.Fatalf("invalid state tree: %+v %v", body, err)
					}
					stored = body.Tree[0].Content
					_, _ = fmt.Fprintf(r, `{"sha":%q}`, strings.Repeat("c", 40))
				case req.Method == http.MethodPost && req.URL.Path == "/repos/org/actions/git/commits":
					commits++
					_, _ = fmt.Fprintf(r, `{"sha":%q}`, strings.Repeat(strconv.Itoa(commits), 40))
				case req.Method == http.MethodPost && req.URL.Path == "/repos/org/actions/git/refs":
					var body struct{ Ref, SHA string }
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Ref != "refs/heads/"+lifecycleBranch {
						t.Fatalf("invalid state ref: %+v %v", body, err)
					}
					stateHead = body.SHA
					_, _ = io.WriteString(r, `{}`)
				case req.Method == http.MethodPost && req.URL.Path == "/graphql":
					var body struct {
						Variables struct {
							Input struct {
								RefUpdates []struct {
									BeforeOID string `json:"beforeOid"`
									AfterOID  string `json:"afterOid"`
								} `json:"refUpdates"`
							} `json:"input"`
						} `json:"variables"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil || len(body.Variables.Input.RefUpdates) != 1 {
						t.Fatalf("invalid update refs: %+v %v", body, err)
					}
					update := body.Variables.Input.RefUpdates[0]
					if update.BeforeOID != stateHead {
						t.Fatalf("state update did not compare observed head: %+v", update)
					}
					stateHead = update.AfterOID
					_, _ = io.WriteString(r, `{"data":{"updateRefs":{"__typename":"UpdateRefsPayload"}}}`)
				default:
					t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
				}
				return r.Result(), nil
			})})
			c, err := loadLifecycle(context.Background(), client, "org/actions", mainHead, testLifecycleKey)
			if err != nil {
				t.Fatal(err)
			}
			c.trustedWriters = free
			state := lifecycleState{Version: 1, Revision: mainHead, Marked: map[string]string{}}
			state.Signature = c.stateSignature(state)
			if err := c.saveApplied(context.Background(), client, state); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadLifecycle(context.Background(), client, "org/actions", mainHead, testLifecycleKey)
			if err != nil || loaded.state.Revision != mainHead {
				t.Fatalf("could not reload state: %v", err)
			}
			loaded.trustedWriters = free
			if err := loaded.saveApplied(context.Background(), client, loaded.state); err != nil || commits != 1 {
				t.Fatalf("unchanged state created a commit: %d %v", commits, err)
			}
			mainHead = testLifecycleStateSHA
			loaded.revision = mainHead
			state.Revision = mainHead
			state.Signature = loaded.stateSignature(state)
			if err := loaded.saveApplied(context.Background(), client, state); err != nil || commits != 2 {
				t.Fatalf("updated state was not recorded: %d %v", commits, err)
			}
			stored = strings.Replace(stored, mainHead, testLifecycleSHA, 1)
			if _, err := loadLifecycle(context.Background(), client, "org/actions", mainHead, testLifecycleKey); err == nil {
				t.Fatal("tampered state was accepted")
			}
		})
	}
}

func TestAppliedHistoryRejectsSkippedUnmark(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	write := func(content string) string {
		if err := os.WriteFile(filepath.Join(dir, "repos-config.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		run("add", "repos-config.yaml")
		run("-c", "commit.gpgsign=false", "commit", "-q", "-s", "-m", "test: change config")
		return run("rev-parse", "HEAD")
	}
	previous := write("repositories:\n  - name: org/example\n    archive_on_removal: true\n  - name: org/other\n")
	write("repositories:\n  - name: org/example\n  - name: org/other\n")
	current := write("repositories:\n  - name: org/other\n")
	t.Chdir(dir)
	before, transitions, gap, err := appliedHistory(previous, current, "org")
	if err != nil {
		t.Fatal(err)
	}
	if gap {
		t.Fatal("valid history must not be treated as a gap")
	}
	key := lifecycleNameKey(testLifecycleKey, "org/example")
	marks := map[string]string{key: lifecycleIdentity(testLifecycleKey, "org/example", 42)}
	if len(transitionCandidates(before, transitions, marks, testLifecycleKey)) != 0 {
		t.Fatal("a skipped unmark must cancel archival")
	}
}

func TestAppliedHistoryRecoversFromInvalidIntermediateConfig(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	write := func(content string) string {
		if err := os.WriteFile(filepath.Join(dir, "repos-config.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		run("add", "repos-config.yaml")
		run("-c", "commit.gpgsign=false", "commit", "-q", "-s", "-m", "test: change config")
		return run("rev-parse", "HEAD")
	}
	applied := write("repositories:\n  - name: org/old\n    archive_on_removal: true\n")
	invalid := write("repositories: [\n")
	corrected := write("repositories:\n  - name: org/new\n    archive_on_removal: true\n")
	later := write("repositories:\n  - name: org/other\n")
	t.Chdir(dir)
	before, transitions, gap, err := appliedHistory(applied, corrected, "org")
	if err != nil {
		t.Fatalf("corrected configuration should recover: %v", err)
	}
	if !gap {
		t.Fatal("invalid intermediate configuration must be recorded as a gap")
	}
	oldKey := lifecycleNameKey(testLifecycleKey, "org/old")
	oldMarks := map[string]string{oldKey: lifecycleIdentity(testLifecycleKey, "org/old", 42)}
	if candidates := transitionCandidates(before, transitions, oldMarks, testLifecycleKey); len(candidates) != 0 {
		t.Fatal("invalid history must cancel previously applied archival intent")
	}
	if _, _, _, err := appliedHistory(applied, invalid, "org"); err == nil {
		t.Fatal("invalid current configuration must fail")
	}
	if _, _, _, err := appliedHistory(invalid, corrected, "org"); err == nil {
		t.Fatal("invalid applied configuration must fail")
	}
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != "/repos/org/new" {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
		}
		r := httptest.NewRecorder()
		_, _ = io.WriteString(r, `{"id":43,"full_name":"org/new","fork":false,"archived":false}`)
		return r.Result(), nil
	})})
	controller := &lifecycleController{owner: "org", repo: "actions", revision: corrected, key: testLifecycleKey}
	current := []RepoConfig{{Name: "org/new", ArchiveOnRemoval: true}}
	state, err := controller.prepareApplied(context.Background(), client, current, map[string]int64{"org/new": 43})
	if err != nil {
		t.Fatal(err)
	}
	newKey := lifecycleNameKey(testLifecycleKey, "org/new")
	if len(state.Marked) != 1 || state.Marked[newKey] != lifecycleIdentity(testLifecycleKey, "org/new", 43) {
		t.Fatalf("recovery should record only the current applied marker: %v", state.Marked)
	}
	before, transitions, gap, err = appliedHistory(corrected, later, "org")
	if err != nil {
		t.Fatal(err)
	}
	if gap {
		t.Fatal("valid history after recovery must not be treated as a gap")
	}
	if _, found := transitionCandidates(before, transitions, state.Marked, testLifecycleKey)[newKey]; !found {
		t.Fatal("a subsequently applied marker should allow a later removal")
	}
	write("repositories:\n  - name: org/example\n  - name: ORG/example\n")
	newBaseline := write("repositories:\n  - name: org/other\n  - name: org/new\n")
	if _, _, gap, err := appliedHistory(later, newBaseline, "org"); err != nil || !gap {
		t.Fatalf("invalid intermediate repository list must create a gap: gap=%t error=%v", gap, err)
	}
	run("rm", "-q", "repos-config.yaml")
	run("-c", "commit.gpgsign=false", "commit", "-q", "-s", "-m", "test: remove config")
	restored := write("repositories:\n  - name: org/restored\n")
	if _, _, _, err := appliedHistory(newBaseline, restored, "org"); err == nil {
		t.Fatal("an unreadable historical config must fail")
	}
}

func TestPausedRepositorySkipsGitHubAndLifecycle(t *testing.T) {
	ctx := context.Background()
	client := github.NewClient(&http.Client{Transport: settingsTestTransport(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("paused repository received %s %s", req.Method, req.URL.Path)
		return nil, nil
	})})
	controller := &lifecycleController{owner: "o", repo: "actions", key: testLifecycleKey}
	repo := RepoConfig{Name: "o/paused", SyncPaused: true, ArchiveOnRemoval: true}
	repos := []RepoConfig{repo}
	report := newDriftReport(io.Discard)
	if err := controller.ensureRepositories(ctx, client, repos, report); err != nil {
		t.Fatal(err)
	}
	if err := reconcileRepositories(ctx, client, repos, report); err != nil {
		t.Fatal(err)
	}
	state, err := controller.prepareApplied(ctx, client, repos, nil)
	if err != nil || len(state.Marked) != 0 {
		t.Fatalf("paused repo armed for archival: %v, %v", state.Marked, err)
	}
	key := lifecycleNameKey(controller.key, repo.Name)
	if err := controller.archiveRemoved(ctx, client, map[string]RepoConfig{key: repo}, repos, report); err != nil {
		t.Fatal(err)
	}
}
