// This file (requiredchecks.go) implements ports.SourceControl.
// ListRequiredChecks: the checks a base branch requires before a pull
// request into it may merge (§21.2, "CI green means the required checks,
// not the checks that reported").
//
// # Two sources, both readable without admin rights
//
// GitHub declares required checks in two places, and a branch can use
// either or both:
//
//   - its branch protection, read from the BRANCH OBJECT (GET
//     /repos/{owner}/{repo}/branches/{branch}), whose protection.
//     required_status_checks carries contexts (names) and checks (names
//     with an optional app_id). Never the /branches/{branch}/protection
//     endpoint mergedbetween.go reads for review requirements: that one
//     requires admin rights, and the bot is not an admin.
//   - the rulesets that apply to the branch (GET /repos/{owner}/{repo}/
//     rules/branches/{branch}): a list of active rules, of which those of
//     type required_status_checks carry parameters.required_status_checks
//     (a context with an optional integration_id, the App that must
//     report it).
//
// Only those fields are modelled. An App id that is absent, null, or not
// positive names no App (GitHub uses -1 for "any source" when a check is
// configured).
//
// # What counts as a failed read
//
// A source the repository's plan does not offer answers 403 with a message
// asking to upgrade the plan; that source declares nothing
// (isPlanUnavailable). Every other failure -- any other status, a
// transport error, a body that does not decode, a branch object with no
// protection object at all, more rule pages than maxRequiredCheckRulePages
// -- is an error, and the caller must not read it as "requires nothing".

package githubapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/narvidev/narvi/internal/app/ports"
)

// requiredCheckRulesPerPage is the page size asked of the rulesets
// endpoint -- the largest GitHub serves, like every other list GET in this
// package.
const requiredCheckRulesPerPage = 100

// maxRequiredCheckRulePages bounds how many pages of rules one read
// follows. A branch with more active rules than this reads as failed,
// never as the prefix that was read.
const maxRequiredCheckRulePages = 5

// branchWithProtectionResponse is the subset of GitHub's GET
// /repos/{owner}/{repo}/branches/{branch} response ListRequiredChecks
// needs. Protection is a pointer so a response carrying no protection
// object at all -- which GitHub returns, disabled and empty, even for an
// unprotected branch -- reads as a failed read rather than as "nothing
// required".
type branchWithProtectionResponse struct {
	Protection *struct {
		RequiredStatusChecks *struct {
			Contexts []string `json:"contexts"`
			Checks   []struct {
				Context string `json:"context"`
				AppID   *int64 `json:"app_id"`
			} `json:"checks"`
		} `json:"required_status_checks"`
	} `json:"protection"`
}

// branchRuleResponse is one element of GitHub's GET
// /repos/{owner}/{repo}/rules/branches/{branch} response. Parameters is
// decoded only for a rule of type requiredStatusChecksRuleType, since each
// rule type carries its own parameters.
type branchRuleResponse struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

const requiredStatusChecksRuleType = "required_status_checks"

type requiredStatusChecksRuleParameters struct {
	RequiredStatusChecks []struct {
		Context       string `json:"context"`
		IntegrationID *int64 `json:"integration_id"`
	} `json:"required_status_checks"`
}

// ListRequiredChecks implements ports.SourceControl -- see this file's
// doc comment. The branch object is read first, then the rulesets; the
// answer is both sources' checks together, duplicates included.
func (a *Adapter) ListRequiredChecks(ctx context.Context, spec ports.ListRequiredChecksSpec) ([]ports.RequiredCheck, error) {
	fromProtection, err := a.requiredChecksFromBranchProtection(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("githubapi: list required checks of %s/%s@%s: branch protection: %w", spec.Owner, spec.Repo, spec.Branch, err)
	}
	fromRulesets, err := a.requiredChecksFromRulesets(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("githubapi: list required checks of %s/%s@%s: rulesets: %w", spec.Owner, spec.Repo, spec.Branch, err)
	}
	return append(fromProtection, fromRulesets...), nil
}

func (a *Adapter) requiredChecksFromBranchProtection(ctx context.Context, spec ports.ListRequiredChecksSpec) ([]ports.RequiredCheck, error) {
	path := fmt.Sprintf("%s/repos/%s/%s/branches/%s", a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo), url.PathEscape(spec.Branch))
	body, err := a.doGet(ctx, path, spec.Token)
	if err != nil {
		if isPlanUnavailable(err) {
			return nil, nil
		}
		return nil, err
	}
	var branch branchWithProtectionResponse
	if err := json.Unmarshal(body, &branch); err != nil {
		return nil, fmt.Errorf("decode branch: %w", err)
	}
	if branch.Protection == nil {
		return nil, errors.New("the branch reported no protection object")
	}
	rsc := branch.Protection.RequiredStatusChecks
	if rsc == nil {
		return nil, nil
	}
	var out []ports.RequiredCheck
	named := make(map[string]bool, len(rsc.Checks))
	for _, c := range rsc.Checks {
		named[c.Context] = true
		out = append(out, ports.RequiredCheck{Name: c.Context, AppID: namedAppID(c.AppID)})
	}
	// contexts is the older, name-only list of the same requirement; a
	// name already carried by checks, with its App, is not added again as
	// "any source".
	for _, name := range rsc.Contexts {
		if !named[name] {
			out = append(out, ports.RequiredCheck{Name: name})
		}
	}
	return out, nil
}

func (a *Adapter) requiredChecksFromRulesets(ctx context.Context, spec ports.ListRequiredChecksSpec) ([]ports.RequiredCheck, error) {
	var out []ports.RequiredCheck
	for page := 1; page <= maxRequiredCheckRulePages; page++ {
		path := fmt.Sprintf("%s/repos/%s/%s/rules/branches/%s?per_page=%d&page=%d", a.apiBaseURL, url.PathEscape(spec.Owner), url.PathEscape(spec.Repo), url.PathEscape(spec.Branch), requiredCheckRulesPerPage, page)
		body, err := a.doGet(ctx, path, spec.Token)
		if err != nil {
			if page == 1 && isPlanUnavailable(err) {
				return nil, nil
			}
			return nil, err
		}
		var rules []branchRuleResponse
		if err := json.Unmarshal(body, &rules); err != nil {
			return nil, fmt.Errorf("decode rules page %d: %w", page, err)
		}
		for _, rule := range rules {
			if rule.Type != requiredStatusChecksRuleType {
				continue
			}
			var params requiredStatusChecksRuleParameters
			if err := json.Unmarshal(rule.Parameters, &params); err != nil {
				return nil, fmt.Errorf("decode %s rule parameters: %w", requiredStatusChecksRuleType, err)
			}
			for _, c := range params.RequiredStatusChecks {
				out = append(out, ports.RequiredCheck{Name: c.Context, AppID: namedAppID(c.IntegrationID)})
			}
		}
		if len(rules) < requiredCheckRulesPerPage {
			return out, nil
		}
	}
	return nil, fmt.Errorf("more than %d pages of rules apply to the branch", maxRequiredCheckRulePages)
}

// namedAppID is the App a requirement names: a positive id, or zero for
// none -- absent, null, or GitHub's -1 for "any source".
func namedAppID(id *int64) int64 {
	if id == nil || *id <= 0 {
		return 0
	}
	return *id
}

// isPlanUnavailable reports whether err is GitHub saying the repository's
// plan does not offer the feature read: a 403, not a rate limit, whose
// message asks to upgrade the plan ("Upgrade to GitHub Pro or make this
// repository public to enable this feature."). Matched narrowly on
// purpose: a 403 this does not recognise stays a failed read, which makes
// the pull request ineligible -- the safe direction for a wording this
// adapter has not seen.
func isPlanUnavailable(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Status != http.StatusForbidden || apiErr.RateLimited {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.Message), "upgrade to github")
}
