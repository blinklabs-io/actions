package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-github/v60/github"
)

func driftSet(values []string) string {
	values = append([]string{}, values...)
	sort.Strings(values)
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

// Refuse replacement updates if they could remove protection that is outside
// the configuration model. The API replaces the complete protection object.
func hasUnmanagedProtection(p *github.Protection) bool {
	if p == nil {
		return false
	}
	if p.BlockCreations != nil && p.BlockCreations.GetEnabled() || p.AllowForkSyncing != nil && p.AllowForkSyncing.GetEnabled() {
		return true
	}
	if r := p.RequiredPullRequestReviews; r != nil {
		if r.RequireLastPushApproval {
			return true
		}
		if d := r.DismissalRestrictions; d != nil && (len(d.Users) > 0 || len(d.Teams) > 0 || len(d.Apps) > 0) {
			return true
		}
	}
	if s := p.RequiredStatusChecks; s != nil && s.Checks != nil {
		for _, check := range *s.Checks {
			if check != nil && check.AppID != nil && *check.AppID != -1 {
				return true
			}
		}
	}
	return false
}

// An arbitrary 404 can also indicate missing access or a missing branch.
func branchIsUnprotected(err error) bool {
	if errors.Is(err, github.ErrBranchNotProtected) {
		return true
	}
	var response *github.ErrorResponse
	return errors.As(err, &response) && response.Response != nil &&
		response.Response.StatusCode == http.StatusNotFound && response.Message == "Branch not protected"
}

// branchProtectionDiff compares the fields governed by the branch configuration.
func branchProtectionDiff(current *github.Protection, desired BranchProtection) (string, string) {
	var before, after []string
	add := func(name, old, new string) {
		if old != new {
			before = append(before, name+": "+old)
			after = append(after, name+": "+new)
		}
	}
	if current == nil {
		add("protection", "absent", "configured")
		current = &github.Protection{}
	}
	checks, strict := []string(nil), false
	if c := current.GetRequiredStatusChecks(); c != nil {
		if c.Contexts != nil {
			checks = *c.Contexts
		} else if c.Checks != nil {
			for _, check := range *c.Checks {
				if check != nil {
					checks = append(checks, check.Context)
				}
			}
		}
		strict = c.Strict
	}
	add("required_status_checks", driftSet(checks), driftSet(desired.RequiredStatusChecks))
	add("require_up_to_date", strconv.FormatBool(strict), strconv.FormatBool(desired.RequireUpToDate))
	reviews := current.GetRequiredPullRequestReviews()
	wantReviews := !desired.BypassPullRequest
	add("require_pull_request_reviews", strconv.FormatBool(reviews != nil), strconv.FormatBool(wantReviews))
	if wantReviews {
		var approvals int
		var dismiss, codeOwners bool
		var users, teams, apps []string
		if reviews != nil {
			approvals = reviews.RequiredApprovingReviewCount
			dismiss = reviews.DismissStaleReviews
			codeOwners = reviews.RequireCodeOwnerReviews
			if b := reviews.BypassPullRequestAllowances; b != nil {
				for _, u := range b.Users {
					users = append(users, u.GetLogin())
				}
				for _, t := range b.Teams {
					teams = append(teams, t.GetSlug())
				}
				for _, a := range b.Apps {
					apps = append(apps, a.GetSlug())
				}
			}
		}
		add("required_approving_review_count", strconv.Itoa(approvals), strconv.Itoa(desired.RequiredApprovingReviewCount))
		add("dismiss_stale_reviews", strconv.FormatBool(dismiss), strconv.FormatBool(desired.DismissStaleReviews))
		add("require_code_owner_reviews", strconv.FormatBool(codeOwners), strconv.FormatBool(desired.RequireCodeOwnerReviews))
		addActors := func(name string, old, new []string) {
			if driftSet(old) != driftSet(new) {
				before = append(before, fmt.Sprintf("%s: %d previous actor(s), names redacted", name, len(old)))
				after = append(after, fmt.Sprintf("%s: %d desired actor(s), names redacted", name, len(new)))
			}
		}
		addActors("bypass_actors_users", users, desired.BypassActorsUsers)
		addActors("bypass_actors_teams", teams, desired.BypassActorsTeams)
		addActors("bypass_actors_apps", apps, desired.BypassActorsApps)
	}
	boolField := func(name string, old, new bool) { add(name, strconv.FormatBool(old), strconv.FormatBool(new)) }
	boolField("enforce_admins", current.GetEnforceAdmins() != nil && current.GetEnforceAdmins().Enabled, desired.EnforceAdmins)
	boolField("allow_force_pushes", current.GetAllowForcePushes() != nil && current.GetAllowForcePushes().Enabled, desired.AllowForcePushes)
	boolField("allow_deletions", current.GetAllowDeletions() != nil && current.GetAllowDeletions().Enabled, desired.AllowDeletions)
	boolField("require_linear_history", current.GetRequireLinearHistory() != nil && current.GetRequireLinearHistory().Enabled, desired.RequireLinearHistory)
	boolField("required_conversation_resolution", current.GetRequiredConversationResolution() != nil && current.GetRequiredConversationResolution().Enabled, desired.RequiredConversationResolution)
	boolField("lock_branch", current.GetLockBranch() != nil && current.GetLockBranch().GetEnabled(), desired.LockBranch)
	return strings.Join(before, "\n"), strings.Join(after, "\n")
}
