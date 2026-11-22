package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-github/v60/github"
	"gopkg.in/yaml.v3"
)

const lifecycleBranch = "governance-applied-state"
const lifecyclePath = "applied-state.json"
const lifecycleRulesetName = "governance-lifecycle-state"
const lifecycleAppID = 4081860

var lifecycleRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)
var lifecycleHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var lifecycleRepoName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
var errInvalidLifecycleConfig = errors.New("configuration history is invalid")

// Names and repository IDs are not stored in the public controller state.
type lifecycleState struct {
	Version   int               `json:"version"`
	Revision  string            `json:"revision"`
	Marked    map[string]string `json:"marked"`
	Signature string            `json:"signature"`
}

type lifecycleController struct {
	trustedWriters                           bool
	owner, repo, nodeID, revision, stateHead string
	state                                    lifecycleState
	key                                      []byte
}

func lifecycleDigest(key []byte, value string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

func lifecycleNameKey(key []byte, name string) string {
	return lifecycleDigest(key, "name:"+strings.ToLower(name))
}

func lifecycleIdentity(key []byte, name string, id int64) string {
	return lifecycleDigest(key, "identity:"+strings.ToLower(name)+":"+strconv.FormatInt(id, 10))
}

func (c *lifecycleController) stateSignature(state lifecycleState) string {
	state.Signature = ""
	data, _ := json.Marshal(state)
	return lifecycleDigest(c.key, "state:"+strings.ToLower(c.owner+"/"+c.repo)+":"+string(data))
}

func validateExplicitRepositories(repos []RepoConfig, owner string) error {
	if repos == nil {
		return errors.New("the explicit repositories list is required")
	}
	seen := make(map[string]bool, len(repos))
	for _, repo := range repos {
		repoOwner, name, ok := strings.Cut(repo.Name, "/")
		if !ok || !lifecycleRepoName.MatchString(name) || name == "." || name == ".." || !strings.EqualFold(repoOwner, owner) ||
			strings.TrimSpace(repo.Name) != repo.Name {
			return errors.New("configured repository must belong to the controller organization")
		}
		key := strings.ToLower(repo.Name)
		if seen[key] {
			return errors.New("duplicate configured repository")
		}
		seen[key] = true
	}
	return nil
}

func loadLifecycle(ctx context.Context, client *github.Client, controller, revision string, key []byte) (*lifecycleController, error) {
	owner, repo, ok := strings.Cut(controller, "/")
	if !ok || !lifecycleRepoName.MatchString(owner) || !lifecycleRepoName.MatchString(repo) ||
		owner == "." || owner == ".." || repo == "." || repo == ".." || !lifecycleRevision.MatchString(revision) || len(key) < 32 {
		return nil, errors.New("controller repository or revision is invalid")
	}
	info, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil || info == nil || info.GetNodeID() == "" || !strings.EqualFold(info.GetFullName(), controller) {
		return nil, errors.New("could not verify controller repository")
	}
	c := &lifecycleController{owner: owner, repo: repo, nodeID: info.GetNodeID(), revision: revision, key: key,
		state: lifecycleState{Version: 1, Marked: map[string]string{}}}
	ref, response, err := client.Git.GetRef(ctx, owner, repo, "heads/"+lifecycleBranch)
	if err != nil {
		if response != nil && response.StatusCode == http.StatusNotFound {
			return c, nil
		}
		return nil, errors.New("could not read lifecycle state reference")
	}
	if ref == nil || ref.GetObject() == nil || !lifecycleRevision.MatchString(ref.GetObject().GetSHA()) {
		return nil, errors.New("lifecycle state reference is invalid")
	}
	c.stateHead = ref.GetObject().GetSHA()
	file, _, _, err := client.Repositories.GetContents(ctx, owner, repo, lifecyclePath, &github.RepositoryContentGetOptions{Ref: c.stateHead})
	if err != nil || file == nil {
		return nil, errors.New("could not read lifecycle state")
	}
	content, err := file.GetContent()
	if err != nil || json.Unmarshal([]byte(content), &c.state) != nil || c.state.Version != 1 ||
		!lifecycleRevision.MatchString(c.state.Revision) || c.state.Marked == nil {
		return nil, errors.New("lifecycle state is invalid")
	}
	for key, identity := range c.state.Marked {
		if !lifecycleHash.MatchString(key) || !lifecycleHash.MatchString(identity) {
			return nil, errors.New("lifecycle state contains an invalid identity")
		}
	}
	if !hmac.Equal([]byte(c.state.Signature), []byte(c.stateSignature(c.state))) {
		return nil, errors.New("lifecycle state signature is invalid")
	}
	return c, nil
}

func (c *lifecycleController) requireCurrentMain(ctx context.Context, client *github.Client) error {
	ref, _, err := client.Git.GetRef(ctx, c.owner, c.repo, "heads/main")
	if err != nil || ref == nil || ref.GetObject() == nil || ref.GetObject().GetSHA() != c.revision {
		return errors.New("controller main changed or could not be observed")
	}
	return nil
}

func (c *lifecycleController) requireCurrentState(ctx context.Context, client *github.Client) error {
	ref, _, err := client.Git.GetRef(ctx, c.owner, c.repo, "heads/"+lifecycleBranch)
	if err != nil || ref == nil || ref.GetObject() == nil || ref.GetObject().GetSHA() != c.stateHead {
		return errors.New("lifecycle state changed or could not be observed")
	}
	return nil
}

// Rulesets prevent writers from replaying authentic older state. Free private
// repositories cannot enforce this boundary, so their writers must be trusted.
func (c *lifecycleController) requireStateProtection(ctx context.Context, client *github.Client) error {
	if c.trustedWriters {
		return nil
	}
	var ruleID int64
	for page := 1; ; page++ {
		path := fmt.Sprintf("repos/%s/%s/rulesets?includes_parents=false&per_page=100&page=%d", c.owner, c.repo, page)
		req, err := client.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			return errors.New("could not inspect lifecycle state protection")
		}
		var rules []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		response, err := client.Do(ctx, req, &rules)
		if err != nil || response == nil {
			return errors.New("could not inspect lifecycle state protection")
		}
		for _, rule := range rules {
			if rule.Name == lifecycleRulesetName {
				if ruleID != 0 || rule.ID <= 0 {
					return errors.New("lifecycle state protection is ambiguous")
				}
				ruleID = rule.ID
			}
		}
		if response.NextPage == 0 {
			break
		}
		if response.NextPage != page+1 {
			return errors.New("lifecycle state protection pagination is invalid")
		}
	}
	if ruleID == 0 {
		return errors.New("lifecycle state protection is missing")
	}
	req, err := client.NewRequest(http.MethodGet, fmt.Sprintf("repos/%s/%s/rulesets/%d", c.owner, c.repo, ruleID), nil)
	if err != nil {
		return errors.New("could not inspect lifecycle state protection")
	}
	var detail struct {
		Name        string `json:"name"`
		Target      string `json:"target"`
		Enforcement string `json:"enforcement"`
		Conditions  struct {
			RefName struct {
				Include []string `json:"include"`
				Exclude []string `json:"exclude"`
			} `json:"ref_name"`
		} `json:"conditions"`
		Bypass []struct {
			ActorID int64  `json:"actor_id"`
			Type    string `json:"actor_type"`
			Mode    string `json:"bypass_mode"`
		} `json:"bypass_actors"`
		Rules []struct {
			Type       string `json:"type"`
			Parameters *struct {
				UpdateAllowsFetchAndMerge *bool `json:"update_allows_fetch_and_merge"`
			} `json:"parameters"`
		} `json:"rules"`
	}
	if _, err := client.Do(ctx, req, &detail); err != nil || detail.Name != lifecycleRulesetName || detail.Target != "branch" ||
		detail.Enforcement != "active" || len(detail.Conditions.RefName.Include) != 1 ||
		detail.Conditions.RefName.Include[0] != "refs/heads/"+lifecycleBranch || len(detail.Conditions.RefName.Exclude) != 0 ||
		len(detail.Bypass) != 1 || detail.Bypass[0].ActorID != lifecycleAppID || detail.Bypass[0].Type != "Integration" || detail.Bypass[0].Mode != "always" {
		return errors.New("lifecycle state protection is invalid")
	}
	found := make(map[string]bool)
	for _, rule := range detail.Rules {
		if rule.Type == "update" && rule.Parameters != nil && rule.Parameters.UpdateAllowsFetchAndMerge != nil && *rule.Parameters.UpdateAllowsFetchAndMerge {
			return errors.New("lifecycle state update restriction is invalid")
		}
		found[rule.Type] = true
	}
	if !found["creation"] || !found["update"] || !found["deletion"] || !found["non_fast_forward"] {
		return errors.New("lifecycle state protection is incomplete")
	}
	return nil
}

func archiveRepository(ctx context.Context, client *github.Client, nodeID string) error {
	request := struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}{
		Query:     `mutation ArchiveRepository($id: ID!) { archiveRepository(input: {repositoryId: $id}) { repository { id isArchived } } }`,
		Variables: map[string]string{"id": nodeID},
	}
	req, err := client.NewRequest(http.MethodPost, "graphql", request)
	if err != nil {
		return errors.New("could not prepare repository archival")
	}
	var result struct {
		Data struct {
			ArchiveRepository struct {
				Repository struct {
					ID         string `json:"id"`
					IsArchived bool   `json:"isArchived"`
				} `json:"repository"`
			} `json:"archiveRepository"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if _, err := client.Do(ctx, req, &result); err != nil || len(result.Errors) > 0 ||
		result.Data.ArchiveRepository.Repository.ID != nodeID || !result.Data.ArchiveRepository.Repository.IsArchived {
		return errors.New("repository archival was not confirmed")
	}
	return nil
}

func (c *lifecycleController) ensureRepositories(ctx context.Context, client *github.Client, repos []RepoConfig, report *driftReport) error {
	var missing []RepoConfig
	for _, repo := range repos {
		owner, name, _ := strings.Cut(repo.Name, "/")
		if repo.ArchiveOnRemoval && strings.EqualFold(repo.Name, c.owner+"/"+c.repo) {
			return errors.New("the controller repository cannot be marked for archival")
		}
		info, response, err := client.Repositories.Get(ctx, owner, name)
		if err == nil && info != nil {
			if !strings.EqualFold(info.GetFullName(), repo.Name) {
				return errors.New("configured repository was renamed or redirected")
			}
			if repo.ArchiveOnRemoval && (info.ID == nil || info.GetID() <= 0 || info.Fork == nil || info.GetFork() || info.Archived == nil || info.GetArchived()) {
				return errors.New("could not verify marked repository")
			}
			continue
		}
		if err == nil || response == nil || response.StatusCode != http.StatusNotFound {
			return errors.New("could not observe configured repository")
		}
		missing = append(missing, repo)
	}
	for _, repo := range missing {
		owner, name, _ := strings.Cut(repo.Name, "/")
		if err := c.requireCurrentMain(ctx, client); err != nil {
			return err
		}
		// Until creation succeeds, even the existence and visibility are unknown.
		report.beginRepo("", false)
		entry := report.change("repository", "creation", "absent", "created")
		if err := report.checkpoint(entry); err != nil {
			entry.status = "skipped"
			return errors.New("could not record planned repository creation")
		}
		private := repo.Private != nil && *repo.Private
		created, _, err := client.Repositories.Create(ctx, owner, &github.Repository{
			Name: github.String(name), Private: github.Bool(private), AutoInit: github.Bool(true),
		})
		if err != nil || created == nil || created.Private == nil || created.GetPrivate() != private || !strings.EqualFold(created.GetFullName(), repo.Name) {
			entry.status = "failed"
			return errors.New("could not create configured repository")
		}
		entry.status = "corrected"
	}
	return nil
}

// transitionCandidates retains only applied markers that were never removed or
// reintroduced between the recorded configuration and the current revision.
func transitionCandidates(previous []RepoConfig, transitions [][]RepoConfig, marked map[string]string, secret []byte) map[string]RepoConfig {
	candidates := make(map[string]RepoConfig)
	removed := make(map[string]bool)
	for _, repo := range previous {
		key := lifecycleNameKey(secret, repo.Name)
		if repo.ArchiveOnRemoval && marked[key] != "" {
			candidates[key] = repo
		}
	}
	for _, repos := range transitions {
		present := make(map[string]RepoConfig, len(repos))
		for _, repo := range repos {
			present[lifecycleNameKey(secret, repo.Name)] = repo
		}
		for key := range candidates {
			repo, exists := present[key]
			if exists && (removed[key] || !repo.ArchiveOnRemoval) {
				delete(candidates, key)
			} else if !exists {
				removed[key] = true
			}
		}
	}
	for key := range candidates {
		if !removed[key] {
			delete(candidates, key)
		}
	}
	return candidates
}

// appliedHistory follows main's first-parent history so an unapplied, skipped
// unmark cannot be mistaken for a continuing archival intent. An invalid
// intermediate configuration creates a gap that cancels all prior intent.
func appliedHistory(previous, current, owner string) ([]RepoConfig, [][]RepoConfig, bool, error) {
	if !lifecycleRevision.MatchString(previous) || !lifecycleRevision.MatchString(current) {
		return nil, nil, false, errors.New("invalid configuration revision")
	}
	out, err := exec.Command("git", "rev-list", "--first-parent", current).Output()
	if err != nil {
		return nil, nil, false, errors.New("could not inspect configuration history")
	}
	revisions := strings.Fields(string(out))
	index := -1
	for i, revision := range revisions {
		if revision == previous {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, nil, false, errors.New("applied configuration is not on main history")
	}
	read := func(revision string) ([]RepoConfig, error) {
		content, err := exec.Command("git", "show", revision+":repos-config.yaml").Output()
		if err != nil {
			return nil, errors.New("could not read configuration history")
		}
		var cfg Config
		if yaml.Unmarshal(content, &cfg) != nil || validateExplicitRepositories(cfg.Repositories, owner) != nil {
			return nil, errInvalidLifecycleConfig
		}
		return cfg.Repositories, nil
	}
	before, err := read(previous)
	if err != nil {
		return nil, nil, false, err
	}
	transitions := make([][]RepoConfig, 0, index)
	gap := false
	for i := index - 1; i >= 0; i-- {
		repos, err := read(revisions[i])
		if err != nil {
			if i > 0 && errors.Is(err, errInvalidLifecycleConfig) {
				gap = true
				continue
			}
			return nil, nil, false, err
		}
		transitions = append(transitions, repos)
	}
	if gap {
		return before, nil, true, nil
	}
	return before, transitions, false, nil
}

func (c *lifecycleController) archiveRemoved(ctx context.Context, client *github.Client, candidates map[string]RepoConfig, current []RepoConfig, report *driftReport) error {
	present := make(map[string]bool, len(current))
	for _, repo := range current {
		present[lifecycleNameKey(c.key, repo.Name)] = true
	}
	keys := make([]string, 0, len(candidates))
	for key := range candidates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		repo := candidates[key]
		if present[key] {
			continue
		}
		if strings.EqualFold(repo.Name, c.owner+"/"+c.repo) {
			return errors.New("the controller repository cannot be archived")
		}
		owner, name, _ := strings.Cut(repo.Name, "/")
		if !strings.EqualFold(owner, c.owner) || name == "" {
			return errors.New("archival target is outside the controller organization")
		}
		info, _, err := client.Repositories.Get(ctx, owner, name)
		if err != nil || info == nil || info.ID == nil || info.GetID() <= 0 || info.GetNodeID() == "" || info.Fork == nil || info.Archived == nil ||
			!strings.EqualFold(info.GetFullName(), repo.Name) || info.GetFork() || c.state.Marked[key] != lifecycleIdentity(c.key, repo.Name, info.GetID()) {
			return errors.New("could not safely verify archival target")
		}
		if info.GetArchived() {
			continue
		}
		if err := c.requireCurrentState(ctx, client); err != nil {
			return err
		}
		if err := c.requireStateProtection(ctx, client); err != nil {
			return err
		}
		public := info.Private != nil && !info.GetPrivate() && info.GetVisibility() == "public"
		report.beginRepo(repo.Name, public)
		entry := report.change("repository", "archived", "false", "true")
		if err := report.checkpoint(entry); err != nil {
			entry.status = "skipped"
			return errors.New("could not record planned archival")
		}
		if err := c.requireCurrentMain(ctx, client); err != nil {
			entry.status = "skipped"
			return err
		}
		if err := archiveRepository(ctx, client, info.GetNodeID()); err != nil {
			entry.status = "failed"
			return errors.New("repository archival failed")
		}
		entry.status = "corrected"
	}
	return nil
}

func (c *lifecycleController) prepareApplied(ctx context.Context, client *github.Client, repos []RepoConfig, reconciled map[string]int64) (lifecycleState, error) {
	state := lifecycleState{Version: 1, Revision: c.revision, Marked: map[string]string{}}
	for _, repo := range repos {
		if !repo.ArchiveOnRemoval {
			continue
		}
		if strings.EqualFold(repo.Name, c.owner+"/"+c.repo) {
			return lifecycleState{}, errors.New("the controller repository cannot be marked for archival")
		}
		owner, name, _ := strings.Cut(repo.Name, "/")
		info, _, err := client.Repositories.Get(ctx, owner, name)
		if err != nil || info == nil || info.ID == nil || info.GetID() <= 0 || reconciled[strings.ToLower(repo.Name)] != info.GetID() || info.Fork == nil || info.GetFork() ||
			info.Archived == nil || info.GetArchived() || !strings.EqualFold(info.GetFullName(), repo.Name) {
			return lifecycleState{}, errors.New("could not verify marked repository")
		}
		state.Marked[lifecycleNameKey(c.key, repo.Name)] = lifecycleIdentity(c.key, repo.Name, info.GetID())
	}
	state.Signature = c.stateSignature(state)
	return state, nil
}

func (c *lifecycleController) saveApplied(ctx context.Context, client *github.Client, state lifecycleState) error {
	if err := c.requireStateProtection(ctx, client); err != nil {
		return err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return errors.New("could not encode lifecycle state")
	}
	old, _ := json.Marshal(c.state)
	if c.stateHead != "" && bytes.Equal(old, encoded) {
		return nil
	}
	if err := c.requireCurrentMain(ctx, client); err != nil {
		return err
	}
	text := string(encoded) + "\n"
	tree, _, err := client.Git.CreateTree(ctx, c.owner, c.repo, "", []*github.TreeEntry{{
		Path: github.String(lifecyclePath), Mode: github.String("100644"), Type: github.String("blob"), Content: &text,
	}})
	if err != nil || tree == nil || tree.GetSHA() == "" {
		return errors.New("could not create lifecycle state tree")
	}
	parent := c.stateHead
	if parent == "" {
		parent = c.revision
	}
	commit, _, err := client.Git.CreateCommit(ctx, c.owner, c.repo, &github.Commit{
		Message: github.String("chore: record applied governance state"), Tree: tree,
		Parents: []*github.Commit{{SHA: github.String(parent)}},
	}, nil)
	if err != nil || commit == nil || !lifecycleRevision.MatchString(commit.GetSHA()) {
		return errors.New("could not create lifecycle state commit")
	}
	if err := c.requireCurrentMain(ctx, client); err != nil {
		return err
	}
	if c.stateHead == "" {
		_, _, err = client.Git.CreateRef(ctx, c.owner, c.repo, &github.Reference{
			Ref: github.String("refs/heads/" + lifecycleBranch), Object: &github.GitObject{SHA: github.String(commit.GetSHA())},
		})
	} else {
		err = updateManagedRef(ctx, client, c.nodeID, lifecycleBranch, c.stateHead, commit.GetSHA())
	}
	if err != nil {
		return errors.New("could not publish lifecycle state")
	}
	return nil
}
