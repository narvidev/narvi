package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stepRefPin is one (file, Step number) pair Step 128's sweep verified
// against docs/IMPLEMENTATION_PLAN.md, paired with the exact title the
// plan gave that row at verification time (2026-09-08, after correcting
// the six citations the sweep found already wrong -- see this Step's own
// PR description for which and why). The key is the PAIR, not the number
// alone -- see stepref_pin.go's own top doc comment for the mutation that
// an earlier, number-only draft of this pin let through silently.
type stepRefPin struct {
	File  string
	Num   string
	Title string
}

// stepRefPins is the golden table. Adding a NEW citation under
// stepRefPinScanDirs means adding its (file, number) pair here too, after
// verifying it the way the sweep did: find the row whose TITLE AND CONTENT
// match what the citing text claims, never by arithmetic. That is what
// keeps this table honest rather than a rubber stamp -- an entry asserts "a
// human checked this pairing," not "a scanner once saw a number in this
// file."
var stepRefPins = []stepRefPin{
	{".github/workflows/ci.yml", "17", "OpenCode adapter"},
	{".github/workflows/ci.yml", "74", "sandbox substrate: docker, egress policy, toolchain"},
	{".github/workflows/ci.yml", "80", "ui data layer"},
	{"deploy/sandbox-image/Dockerfile", "13", "sandbox-agent: supervisor"},
	{"deploy/sandbox-image/Dockerfile", "74", "sandbox substrate: docker, egress policy, toolchain"},
	{"deploy/seed/example.yaml", "75", "config/data seeding"},
	{"docs/guides/README.md", "64", "plan mode: follow-up intent classification (amend vs answer)"},
	{"docs/guides/README.md", "77", "ops"},
	{"docs/guides/README.md", "78", "launch readiness"},
	{"docs/guides/github.md", "48", "sentinels + suggestions"},
	{"docs/guides/github.md", "63", "review: learned false-positive patterns"},
	{"docs/guides/github.md", "65", "review: automatic re-review on new commits"},
	{"docs/guides/github.md", "69", "review deep path: adversarial counter-review + readout measurement"},
	{"docs/guides/slack.md", "64", "plan mode: follow-up intent classification (amend vs answer)"},
	{"docs/runbooks/README.md", "73", "cloud identity: OIDC federation + kubeconfig"},
	{"docs/runbooks/README.md", "74", "sandbox substrate: docker, egress policy, toolchain"},
	{"docs/runbooks/README.md", "76", "cohort rollout"},
	{"docs/runbooks/sandbox-capability-refusals.md", "74", "sandbox substrate: docker, egress policy, toolchain"},
	{"docs/runbooks/signing-key-rotation.md", "73", "cloud identity: OIDC federation + kubeconfig"},
	{"docs/runbooks/signing-key-rotation.md", "74", "sandbox substrate: docker, egress policy, toolchain"},
	{"docs/runbooks/signing-key-rotation.md", "76", "cohort rollout"},
	{"docs/runbooks/slow-boot-and-spawn.md", "77", "ops"},
	{"docs/runbooks/watchdog-false-alarms.md", "24", "two-phase terminalization"},
	{"migrations/000015_sandbox_token_hash.up.sql", "13", "sandbox-agent: supervisor"},
	{"migrations/000015_sandbox_token_hash.up.sql", "15", "sandbox-agent: git + credentials"},
	{"migrations/000015_sandbox_token_hash.up.sql", "18", "wshub: sandbox socket"},
	{"migrations/000015_sandbox_token_hash.up.sql", "21", "e2e happy path"},
	{"migrations/000016_ws_tokens.up.sql", "18", "wshub: sandbox socket"},
	{"migrations/000016_ws_tokens.up.sql", "20", "auth v1"},
	{"migrations/000017_auth_v1.up.sql", "21", "e2e happy path"},
	{"migrations/000018_session_repos.up.sql", "4", "postgres: core schema"},
	{"migrations/000018_session_repos.up.sql", "21", "e2e happy path"},
	{"migrations/000022_sandbox_snapshot_id.up.sql", "21", "e2e happy path"},
	{"migrations/000022_sandbox_snapshot_id.up.sql", "22", "snapshots & restore"},
	{"migrations/000023_sandbox_pre_suspect_status.up.sql", "24", "two-phase terminalization"},
	{"migrations/000024_image_builds.up.sql", "26", "image builds"},
	{"migrations/000025_mock_config_contract_drift.up.sql", "10", "domain: Environment scoping"},
	{"migrations/000025_mock_config_contract_drift.up.sql", "27", "mocking + contract drift"},
	{"migrations/000026_turn_dispatch_gen.up.sql", "28", "turn recovery"},
	{"migrations/000027_webhook_deliveries.up.sql", "31", "webhook toolkit"},
	{"migrations/000027_webhook_deliveries.up.sql", "32", "GitHub ingress"},
	{"migrations/000027_webhook_deliveries.up.sql", "33", "Slack ingress"},
	{"migrations/000027_webhook_deliveries.up.sql", "34", "Linear ingress"},
	{"migrations/000028_github_pr_sessions.up.sql", "31", "webhook toolkit"},
	{"migrations/000028_github_pr_sessions.up.sql", "32", "GitHub ingress"},
	{"migrations/000029_slack_thread_sessions.up.sql", "33", "Slack ingress"},
	{"migrations/000030_linear_agent_sessions.up.sql", "34", "Linear ingress"},
	{"migrations/000031_linear_installations.up.sql", "20", "auth v1"},
	{"migrations/000031_linear_installations.up.sql", "34", "Linear ingress"},
	{"migrations/000032_github_pr_sessions_session_id_idx.up.sql", "32", "GitHub ingress"},
	{"migrations/000032_github_pr_sessions_session_id_idx.up.sql", "35", "outbox delivery"},
	{"migrations/000033_intent_classifier.up.sql", "36", "intent classifier"},
	{"migrations/000034_plan_mode.up.sql", "33", "Slack ingress"},
	{"migrations/000034_plan_mode.up.sql", "34", "Linear ingress"},
	{"migrations/000034_plan_mode.up.sql", "36", "intent classifier"},
	{"migrations/000034_plan_mode.up.sql", "37", "plan mode (web)"},
	{"migrations/000034_plan_mode.up.sql", "38", "plan mode (cross-channel)"},
	{"migrations/000035_plan_mode_cross_channel.up.sql", "37", "plan mode (web)"},
	{"migrations/000035_plan_mode_cross_channel.up.sql", "38", "plan mode (cross-channel)"},
	{"migrations/000036_identity_link_prompts.up.sql", "39", "identities + full RBAC"},
	{"migrations/000038_turn_progress_notified.up.sql", "35", "outbox delivery"},
	{"migrations/000039_image_builds_shared_fingerprint.up.sql", "41", "warm boot: shared image fingerprint"},
	{"migrations/000040_image_builds_refresh_pump.up.sql", "42", "warm boot: refresh pump + hook policy"},
	{"migrations/000044_repo_settings.up.sql", "47", "server-side verdict"},
	{"migrations/000045_sessions_child_sessions.up.sql", "48", "sentinels + suggestions"},
	{"migrations/000046_review_findings.up.sql", "45", "domain/review"},
	{"migrations/000046_review_findings.up.sql", "48", "sentinels + suggestions"},
	{"migrations/000046_review_findings.up.sql", "62", "review verdict persistence, analytics, digest & automated approval"},
	{"migrations/000046_review_findings.up.sql", "63", "review: learned false-positive patterns"},
	{"migrations/000047_sentinel_fixes.up.sql", "48", "sentinels + suggestions"},
	{"migrations/000048_repo_settings_sentinel_autofix.up.sql", "47", "server-side verdict"},
	{"migrations/000048_repo_settings_sentinel_autofix.up.sql", "48", "sentinels + suggestions"},
	{"migrations/000049_handoff_sentinel_runs.up.sql", "49", "handoff-readiness sentinel"},
	{"migrations/000050_release_manifest_pending.up.sql", "32", "GitHub ingress"},
	{"migrations/000051_automations.up.sql", "51", "automations: engine"},
	{"migrations/000051_automations.up.sql", "52", "automations: triggers & extras"},
	{"migrations/000052_automation_invocations.up.sql", "52", "automations: triggers & extras"},
	{"migrations/000052_automation_invocations.up.sql", "76", "cohort rollout"},
	{"migrations/000053_automation_runs.up.sql", "76", "cohort rollout"},
	{"migrations/000055_automations_triggers_and_extras.up.sql", "51", "automations: engine"},
	{"migrations/000055_automations_triggers_and_extras.up.sql", "52", "automations: triggers & extras"},
	{"migrations/000055_automations_triggers_and_extras.up.sql", "53", "provider credential injection"},
	{"migrations/000056_provider_credentials.up.sql", "52", "automations: triggers & extras"},
	{"migrations/000056_provider_credentials.up.sql", "53", "provider credential injection"},
	{"migrations/000056_provider_credentials.up.sql", "54", "domain/workflow + loopguard + schema"},
	{"migrations/000057_workflows.up.sql", "54", "domain/workflow + loopguard + schema"},
	{"migrations/000057_workflows.up.sql", "55", "workflow execution engine"},
	{"migrations/000057_workflows.up.sql", "56", "workflow HITL gate + circuit breaker"},
	{"migrations/000057_workflows.up.sql", "91", "workflow canvas editor"},
	{"migrations/000058_workflow_hitl.up.sql", "54", "domain/workflow + loopguard + schema"},
	{"migrations/000058_workflow_hitl.up.sql", "56", "workflow HITL gate + circuit breaker"},
	{"migrations/000059_repo_settings_rwx_preview.up.sql", "48", "sentinels + suggestions"},
	{"migrations/000059_repo_settings_rwx_preview.up.sql", "57", "RWX provider + previews"},
	{"migrations/000060_artifacts_upload_lifecycle.up.sql", "58", "uploads"},
	{"migrations/000061_provider_credentials_user_scope.up.sql", "59", "models"},
	{"migrations/000062_chatgpt_oauth_credentials.up.sql", "59", "models"},
	{"migrations/000063_turn_session_effort.up.sql", "59", "models"},
	{"migrations/000064_workflow_step_definitions_effort.up.sql", "59", "models"},
	{"migrations/000065_decision_inbox_indexes.up.sql", "60", "decision inbox: read model + API"},
	{"migrations/000066_builder_epistemic_check.up.sql", "61", "domain/turn: builder epistemic pre-action check"},
	{"migrations/000067_review_verdicts.up.sql", "45", "domain/review"},
	{"migrations/000067_review_verdicts.up.sql", "46", "review sessions"},
	{"migrations/000067_review_verdicts.up.sql", "47", "server-side verdict"},
	{"migrations/000067_review_verdicts.up.sql", "62", "review verdict persistence, analytics, digest & automated approval"},
	{"migrations/000068_github_pr_sessions_pending_head_sha.up.sql", "46", "review sessions"},
	{"migrations/000068_github_pr_sessions_pending_head_sha.up.sql", "62", "review verdict persistence, analytics, digest & automated approval"},
	{"migrations/000069_repo_settings_auto_approval.up.sql", "62", "review verdict persistence, analytics, digest & automated approval"},
	{"migrations/000070_auto_approval_outcomes.up.sql", "62", "review verdict persistence, analytics, digest & automated approval"},
	{"migrations/000071_digest_send_state.up.sql", "62", "review verdict persistence, analytics, digest & automated approval"},
	{"migrations/000072_turns_review_head_sha.up.sql", "46", "review sessions"},
	{"migrations/000072_turns_review_head_sha.up.sql", "47", "server-side verdict"},
	{"migrations/000072_turns_review_head_sha.up.sql", "61", "domain/turn: builder epistemic pre-action check"},
	{"migrations/000072_turns_review_head_sha.up.sql", "62", "review verdict persistence, analytics, digest & automated approval"},
	{"migrations/000073_review_false_positive_patterns.up.sql", "59", "models"},
	{"migrations/000073_review_false_positive_patterns.up.sql", "63", "review: learned false-positive patterns"},
	{"migrations/000074_plan_followup.up.sql", "64", "plan mode: follow-up intent classification (amend vs answer)"},
	{"migrations/000075_github_pr_sessions_retrigger.up.sql", "46", "review sessions"},
	{"migrations/000075_github_pr_sessions_retrigger.up.sql", "65", "review: automatic re-review on new commits"},
	{"migrations/000076_repo_settings_auto_retrigger_review.up.sql", "47", "server-side verdict"},
	{"migrations/000076_repo_settings_auto_retrigger_review.up.sql", "65", "review: automatic re-review on new commits"},
	{"migrations/000077_review_verdicts_digest.up.sql", "62", "review verdict persistence, analytics, digest & automated approval"},
	{"migrations/000077_review_verdicts_digest.up.sql", "66", "review digest: verdict as merge readout"},
	{"migrations/000078_review_verdicts_description_adequacy.up.sql", "66", "review digest: verdict as merge readout"},
	{"migrations/000078_review_verdicts_description_adequacy.up.sql", "67", "review digest: description adequacy + graduated remediation"},
	{"migrations/000079_repo_settings_description_autofix.up.sql", "47", "server-side verdict"},
	{"migrations/000079_repo_settings_description_autofix.up.sql", "67", "review digest: description adequacy + graduated remediation"},
	{"migrations/000080_turns_review_depth.up.sql", "68", "review triage: deterministic light/deep routing"},
	{"migrations/000081_review_verdicts_review_path.up.sql", "62", "review verdict persistence, analytics, digest & automated approval"},
	{"migrations/000081_review_verdicts_review_path.up.sql", "68", "review triage: deterministic light/deep routing"},
	{"migrations/000082_repo_settings_review_depth.up.sql", "47", "server-side verdict"},
	{"migrations/000082_repo_settings_review_depth.up.sql", "67", "review digest: description adequacy + graduated remediation"},
	{"migrations/000082_repo_settings_review_depth.up.sql", "68", "review triage: deterministic light/deep routing"},
	{"migrations/000083_turns_review_depth_decision.up.sql", "36", "intent classifier"},
	{"migrations/000083_turns_review_depth_decision.up.sql", "46", "review sessions"},
	{"migrations/000083_turns_review_depth_decision.up.sql", "68", "review triage: deterministic light/deep routing"},
	{"migrations/000084_review_verdicts_counter_review.up.sql", "66", "review digest: verdict as merge readout"},
	{"migrations/000084_review_verdicts_counter_review.up.sql", "69", "review deep path: adversarial counter-review + readout measurement"},
	{"migrations/000085_repo_settings_review_cost_budget.up.sql", "69", "review deep path: adversarial counter-review + readout measurement"},
	{"migrations/000086_review_digest_section_feedback.up.sql", "63", "review: learned false-positive patterns"},
	{"migrations/000086_review_digest_section_feedback.up.sql", "69", "review deep path: adversarial counter-review + readout measurement"},
	{"migrations/000087_image_cache_versions.up.sql", "43", "warm boot: dependency-work reduction (3 PRs — (a)+(b)+(c) all shipped)"},
	{"migrations/000088_plan_builtin_passthrough.up.sql", "37", "plan mode (web)"},
	{"migrations/000088_plan_builtin_passthrough.up.sql", "38", "plan mode (cross-channel)"},
	{"migrations/000088_plan_builtin_passthrough.up.sql", "55", "workflow execution engine"},
	{"migrations/000088_plan_builtin_passthrough.up.sql", "56", "workflow HITL gate + circuit breaker"},
	{"migrations/000088_plan_builtin_passthrough.up.sql", "91", "workflow canvas editor"},
	{"migrations/000089_turns_dispatched_event_id.up.sql", "71", "review: post-hoc sub-task corroboration"},
	{"migrations/000090_sandbox_secrets.up.sql", "52", "automations: triggers & extras"},
	{"migrations/000090_sandbox_secrets.up.sql", "53", "provider credential injection"},
	{"migrations/000090_sandbox_secrets.up.sql", "72", "sandbox secrets & opencode config"},
	{"migrations/000091_opencode_configs.up.sql", "72", "sandbox secrets & opencode config"},
	{"migrations/000092_oidc_signing_keys.up.sql", "73", "cloud identity: OIDC federation + kubeconfig"},
	{"migrations/000093_cloud_identity_bindings.up.sql", "73", "cloud identity: OIDC federation + kubeconfig"},
	{"migrations/000094_cluster_bindings.up.sql", "72", "sandbox secrets & opencode config"},
	{"migrations/000094_cluster_bindings.up.sql", "73", "cloud identity: OIDC federation + kubeconfig"},
	{"migrations/000095_environment_docker_egress.up.sql", "74", "sandbox substrate: docker, egress policy, toolchain"},
	{"migrations/000096_repo_settings_sessions_enabled.up.sql", "47", "server-side verdict"},
	{"migrations/000096_repo_settings_sessions_enabled.up.sql", "76", "cohort rollout"},
	{"migrations/000096_repo_settings_sessions_enabled.up.sql", "104", "shadow operator surface"},
	{"migrations/000097_release_manifest_checks.up.sql", "84", "ui code review + release review"},
	{"migrations/000101_repo_settings_live_egress_enabled.up.sql", "104", "shadow operator surface"},
	{"migrations/000103_outbox_shadow_epoch.up.sql", "104", "shadow operator surface"},
	{"migrations/000125_platform_analytics_indexes.up.sql", "120", "analytics: platform-wide rollup"},
	{"migrations/000127_release_manifest_checks_composition.up.sql", "125", "release composition findings"},
}

// stepRefPinKey is the map key TestStepRefsPinnedToPlanRows and the
// mutation-boundary test both use to match a scanned StepCitation to a
// stepRefPin: (file, number), never number alone.
func stepRefPinKey(file, num string) string { return file + "\x00" + num }

// TestStepRefsPinnedToPlanRows is Step 128's second half: rather than
// banning a Step citation under these roots (stepref.go's own
// TestNoStepRefInSource does that for internal/cmd/controlplane/extension/
// contracts/web-src, and stepref_pin.go's own top doc comment says why that
// is the wrong shape here), this PINS each one -- asserting it still
// resolves to the plan row it named when Step 128's sweep verified it, by
// title, not merely by number.
//
// Three ways to fail, all meaningful:
//
//  1. A citation exists in source whose row's title has moved on from the
//     pin. This is "a renumber silently invalidates a citation" made
//     concrete -- the number still resolves to SOME row, so nothing but a
//     title comparison catches it.
//  2. A citation exists with no pin for its (file, number) pair. Either
//     it is new since the last sweep (add it to stepRefPins above, after
//     verifying it against the plan) or an existing citation was edited to
//     name a different number (the same failure, because the OLD pin no
//     longer matches anything and the new pair is unpinned).
//  3. A pin has no matching citation left in source, so stepRefPins cannot
//     silently accumulate entries for citations since edited or removed.
func TestStepRefsPinnedToPlanRows(t *testing.T) {
	root := repoRoot(t)

	titles, err := LoadPlanStepTitles(root)
	if err != nil {
		t.Fatalf("LoadPlanStepTitles: %v", err)
	}
	if len(titles) == 0 {
		t.Fatal("LoadPlanStepTitles returned no rows at all -- the plan's table format probably changed under planStepRowPattern")
	}

	citations, err := ScanStepCitationsForPinning(root, stepRefPinScanDirs)
	if err != nil {
		t.Fatalf("ScanStepCitationsForPinning: %v", err)
	}

	cited := make(map[string][]StepCitation)
	for _, c := range citations {
		cited[stepRefPinKey(c.File, c.Num)] = append(cited[stepRefPinKey(c.File, c.Num)], c)
	}

	pins := make(map[string]stepRefPin, len(stepRefPins))
	for _, p := range stepRefPins {
		pins[stepRefPinKey(p.File, p.Num)] = p
	}

	for key, refs := range cited {
		pin, ok := pins[key]
		if !ok {
			t.Errorf("%s cites Step %s with no pin for this (file, number) pair (%s)\n"+
				"Either this is a new citation -- verify it against docs/IMPLEMENTATION_PLAN.md's own row %s "+
				"(title/content, never by shifting a number) and add it to stepRefPins -- or an existing "+
				"citation was edited to a different number, which is exactly the failure this check exists to catch.",
				refs[0].File, refs[0].Num, formatLocations(refs), refs[0].Num)
			continue
		}
		live, exists := titles[pin.Num]
		if !exists {
			t.Errorf("%s is pinned to Step %s (%q) but docs/IMPLEMENTATION_PLAN.md no longer has a row %s at all (%s)\n"+
				"The plan was renumbered out from under this citation -- find the row that now carries this content and re-pin.",
				pin.File, pin.Num, pin.Title, pin.Num, formatLocations(refs))
			continue
		}
		if live != pin.Title {
			t.Errorf("%s's Step %s citation has drifted from its pin: pinned=%q live=%q (%s)\n"+
				"This is what a renumber invalidating a citation looks like -- re-verify against the CURRENT plan "+
				"and either update stepRefPins or fix the citation in source.",
				pin.File, pin.Num, pin.Title, live, formatLocations(refs))
		}
	}

	for _, pin := range stepRefPins {
		if _, ok := cited[stepRefPinKey(pin.File, pin.Num)]; !ok {
			t.Errorf("stepRefPins has %s pinned to Step %s (%q) but no citation of it remains in that file under any of %v -- "+
				"remove the stale pin.", pin.File, pin.Num, pin.Title, stepRefPinScanDirs)
		}
	}
}

func formatLocations(refs []StepCitation) string {
	lines := make([]string, 0, len(refs))
	for _, r := range refs {
		lines = append(lines, fmt.Sprintf("%s:%d", r.File, r.Line))
	}
	return "cited at " + strings.Join(lines, ", ")
}

// TestStepRefsPinnedToPlanRows_MutationBoundary is Step 128's own
// mutation-verification, kept as a permanent test rather than a one-off
// manual check. It exercises the exact gap the manual pass against the real
// repo found (stepref_pin.go's own top doc comment): mutating a citation's
// number to ANOTHER REAL, already-pinned row must still fail, because the
// pin key is (file, number) and the mutated pair has no entry -- a
// number-only pin would have passed this silently, which is what happened
// on the first attempt at this file, before it shipped.
//
// Two independent mutations are proven, then both reverted (t.TempDir):
// (1) the plan's own row title changes under a still-cited number (a
// renumber in miniature), and (2) a citation is edited to a different,
// otherwise-valid number.
func TestStepRefsPinnedToPlanRows_MutationBoundary(t *testing.T) {
	dir := t.TempDir()

	writePlan := func(title74 string) {
		doc := "# plan\n\n| Step | Title | Content | Ref. |\n|---|---|---|---|\n" +
			"| 74 | " + title74 + " | irrelevant content | §27.5 |\n" +
			"| 75 | config/data seeding | irrelevant content | §10-P6 |\n"
		docsDir := filepath.Join(dir, "docs")
		if err := os.MkdirAll(docsDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(docsDir, "IMPLEMENTATION_PLAN.md"), []byte(doc), 0o644); err != nil {
			t.Fatalf("WriteFile plan: %v", err)
		}
	}

	migDir := filepath.Join(dir, "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fixturePath := filepath.Join(migDir, "000095_fixture.up.sql")
	writeFixture := func(citedNum string) {
		src := fmt.Sprintf("-- Step %s (\"sandbox substrate: docker, egress policy, toolchain\") adds this column.\n", citedNum)
		if err := os.WriteFile(fixturePath, []byte(src), 0o644); err != nil {
			t.Fatalf("WriteFile fixture: %v", err)
		}
	}

	pins := []stepRefPin{
		{"migrations/000095_fixture.up.sql", "74", "sandbox substrate: docker, egress policy, toolchain"},
	}

	runScan := func() []string {
		titles, err := LoadPlanStepTitles(dir)
		if err != nil {
			t.Fatalf("LoadPlanStepTitles: %v", err)
		}
		citations, err := ScanStepCitationsForPinning(dir, []string{"migrations"})
		if err != nil {
			t.Fatalf("ScanStepCitationsForPinning: %v", err)
		}
		pinsByKey := make(map[string]stepRefPin, len(pins))
		for _, p := range pins {
			pinsByKey[stepRefPinKey(p.File, p.Num)] = p
		}
		var failures []string
		for _, c := range citations {
			pin, ok := pinsByKey[stepRefPinKey(c.File, c.Num)]
			if !ok {
				failures = append(failures, fmt.Sprintf("unpinned (file,num) at %s:%d: Step %s", c.File, c.Line, c.Num))
				continue
			}
			live, exists := titles[pin.Num]
			if !exists || live != pin.Title {
				failures = append(failures, fmt.Sprintf("%s:%d Step %s mismatch: pinned=%q live=%q", c.File, c.Line, c.Num, pin.Title, live))
			}
		}
		return failures
	}

	// Baseline: the fixture cites Step 74, the plan's row 74 matches the
	// pin, so the scan is clean.
	writePlan("sandbox substrate: docker, egress policy, toolchain")
	writeFixture("74")
	if failures := runScan(); len(failures) != 0 {
		t.Fatalf("expected a clean pin before mutation, got: %v", failures)
	}

	// Mutation 1: renumber-in-miniature. Step 74 now names something else
	// entirely, exactly as if a real renumber had repointed the row.
	writePlan("a completely different capability")
	failures := runScan()
	if len(failures) == 0 {
		t.Fatal("expected a renumbered plan to fail the pin, got no failures")
	}
	if !containsSubstring(failures, "000095_fixture.up.sql:1 Step 74 mismatch") {
		t.Errorf("expected a failure naming migrations/000095_fixture.up.sql:1's Step 74 mismatch, got: %v", failures)
	}
	writePlan("sandbox substrate: docker, egress policy, toolchain") // restore

	// Mutation 2: the citation itself is edited to name a DIFFERENT, real,
	// already-pinned-elsewhere row (75, "config/data seeding") instead of
	// its own 74. A number-only pin would pass this silently, since 75 is a
	// real, correctly-pinned Step somewhere else in the golden table --
	// this is the exact gap the manual sweep against the real repo found.
	writeFixture("75")
	failures = runScan()
	if len(failures) == 0 {
		t.Fatal("expected a citation repointed at a different real row to fail the pin, got no failures")
	}
	if !containsSubstring(failures, "unpinned (file,num) at "+filepath.ToSlash("migrations/000095_fixture.up.sql")+":1: Step 75") {
		t.Errorf("expected a failure naming the unpinned (file,num) pair for the repointed citation, got: %v", failures)
	}

	// Restore: back to the original citation and plan, clean again --
	// proves both failures above were the mutations, not a leftover.
	writeFixture("74")
	if failures := runScan(); len(failures) != 0 {
		t.Fatalf("expected a clean pin after restoring both mutations, got: %v", failures)
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
