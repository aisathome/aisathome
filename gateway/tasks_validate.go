// Checks of what the task queue takes in (tasks.go): every field a client sends.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

func (v *TaskReview) validate() error {
	errs := []error{checkID("by", v.By), checkText("summary", v.Summary, 500)}
	if v.Judge != "" {
		errs = append(errs, checkID("judge", v.Judge))
	}
	if v.Verdict != "green" && v.Verdict != "red" {
		errs = append(errs, errors.New("verdict: green or red"))
	}
	if v.Round < 0 || v.Round > 1000 || (v.Findings != nil && (*v.Findings < 0 || *v.Findings > 100000)) {
		errs = append(errs, errors.New("round or findings out of range"))
	}
	if v.Detail != nil {
		if err := checkObject("detail", v.Detail, maxTaskReviewDetail); err != nil {
			errs = append(errs, err)
		}
	}
	if !v.At.IsZero() {
		errs = append(errs, errors.New("at: set by the gateway"))
	}
	return errors.Join(errs...)
}

func (d *TaskDecision) validate() error {
	errs := []error{checkID("by", d.By), checkText("note", d.Note, 500)}
	if !slices.Contains([]string{"approved", "rejected", "opened"}, d.Decision) {
		errs = append(errs, errors.New("decision: approved, rejected or opened"))
	}
	if d.PR < 0 || d.PR > 999999999 || (d.Decision == "opened") != (d.PR > 0) {
		errs = append(errs, errors.New("pr: the pull request number, with opened only"))
	}
	if !d.At.IsZero() {
		errs = append(errs, errors.New("at: set by the gateway"))
	}
	return errors.Join(errs...)
}

func (r *TaskResult) validate() error {
	errs := []error{checkText("result.outcome", r.Outcome, 200), checkNum("result.model_min", r.ModelMin, 1e5),
		checkNum("result.awake_min", r.AwakeMin, 1e5)}
	if r.Outcome == "" {
		errs = append(errs, errors.New("result.outcome: required"))
	}
	if len(r.Diff) > maxTaskDiff || len(r.PRBody) > maxTaskPRBody || len(r.CommitMsg) > maxTaskCommitMsg {
		errs = append(errs, fmt.Errorf("result: diff at most %d bytes, pr_body %d, commit_msg %d", maxTaskDiff, maxTaskPRBody, maxTaskCommitMsg))
	}
	if r.Score != nil {
		if err := checkObject("result.score", r.Score, maxTaskScore); err != nil {
			errs = append(errs, err)
		}
	}
	if r.Review != nil {
		if err := r.Review.validate(); err != nil {
			errs = append(errs, err)
		}
		if r.Review.Judge != judgeQwen {
			errs = append(errs, errors.New("result.review: a review task's verdict names its judge, qwen"))
		}
	}
	if r.Steps < 0 || r.Steps > 100000 || r.PromptTokens < 0 || r.PromptTokens > 1e12 || r.CompletionTokens < 0 || r.CompletionTokens > 1e12 {
		errs = append(errs, errors.New("result: steps or tokens out of range"))
	}
	if r.RawRef != "" && (len(r.RawRef) > 512 || !utf8Printable(r.RawRef)) {
		errs = append(errs, errors.New("result.raw_ref: at most 512 printable bytes"))
	}
	return errors.Join(errs...)
}

func utf8Printable(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == 0xfffd {
			return false
		}
	}
	return true
}

// checkObject accepts a JSON object of at most max bytes.
func checkObject(field string, raw json.RawMessage, max int) error {
	if len(raw) > max {
		return fmt.Errorf("%s: at most %d bytes", field, max)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return fmt.Errorf("%s: must be a JSON object", field)
	}
	return nil
}

// canonicalJSON re-encodes a JSON value with sorted keys and no spaces (numbers kept as
// written), so that the same spec compares equal however a client formats it.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func (n *taskNew) validate() error {
	errs := []error{checkID("id", n.ID), checkID("created_by", n.CreatedBy), checkText("title", n.Title, 200)}
	if n.CreatedBy == gatewayCreator {
		errs = append(errs, errors.New("created_by: gateway is the review loop's own"))
	}
	if _, ok := taskKinds[n.Kind]; !ok {
		errs = append(errs, errors.New("kind: "+kindsHelp))
	}
	if _, ok := findingsClass[n.FindingsSource]; n.FindingsSource != "" && !ok {
		errs = append(errs, fmt.Errorf("findings_source: %s, %s or %s", findingsReview, findingsPrereview, findingsPrereviewQwen))
	}
	if n.FindingsSource != "" && n.Kind != "fix_round" && n.Kind != "followup" {
		errs = append(errs, errors.New("findings_source: for a fix round or a follow-up"))
	}
	if n.Kind == "review" {
		errs = append(errs, checkID("parent", n.Parent))
	} else if n.Parent != "" {
		errs = append(errs, errors.New("parent: for a review task"))
	}
	if n.Spec == nil {
		errs = append(errs, errors.New("spec: required (a JSON object)"))
	} else if err := checkObject("spec", n.Spec, maxTaskSpec); err != nil {
		errs = append(errs, err)
	}
	if n.BaseRef != "" && !taskRefRe.MatchString(n.BaseRef) {
		errs = append(errs, errors.New("base_ref: a branch or commit, 1-200 of letters, digits, '.', '_', '/' or '-'"))
	}
	if n.PR < 0 || n.PR > 999999999 {
		errs = append(errs, errors.New("pr: a pull request number"))
	}
	if n.Order < -maxTaskOrder || n.Order > maxTaskOrder {
		errs = append(errs, fmt.Errorf("order: %d to %d (lower is claimed first)", -maxTaskOrder, maxTaskOrder))
	}
	if n.MaxTries < 0 || n.MaxTries > maxMaxTries {
		errs = append(errs, fmt.Errorf("max_tries: 1 to %d (0: %d)", maxMaxTries, defaultMaxTries))
	}
	return errors.Join(errs...)
}
