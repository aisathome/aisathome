// Checks of what the task queue takes in (tasks.go): every field a client sends.
//
// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"unicode/utf8"
)

var (
	// publicSecret: key-shaped text (secretLike without its long-run rule: public text quotes long
	// Lean names). A key's body may hold '_' and '-' (base64url), so they count after the prefix.
	// A node token is "nt-<node id>.<secret>" (nodes.go; a BOINC pilot's credential is one too); an
	// account node's ("nt-n-<8 hex>.") counts even right after a letter or digit.
	publicSecret = regexp.MustCompile(`(?i:\b(?:ak|sk|ghp|gho|ghs|ghu|github_pat|xox[abp])[-_][a-z0-9_-]{8})|(?i:\bnt-[a-z0-9-]{1,32}\.[a-z0-9_-]{8})|nt-n-[0-9a-f]{8}\.[A-Za-z0-9_-]{8}|\bAKIA[0-9A-Z]{16}\b|(?i:\bbearer\s)|-----BEGIN`)
	// publicEmail: an e-mail address, which no public label carries.
	publicEmail = regexp.MustCompile(`[^\s@]+@[^\s@]+\.[^\s@]+`)
)

func (r *TaskResult) validate() error {
	errs := []error{checkText("result.outcome", r.Outcome, 200), checkNum("result.model_min", r.ModelMin, 1e5),
		checkNum("result.awake_min", r.AwakeMin, 1e5)}
	if r.Outcome == "" {
		errs = append(errs, errors.New("result.outcome: required"))
	}
	if r.Score != nil {
		if err := checkObject("result.score", r.Score, maxTaskScore); err != nil {
			errs = append(errs, err)
		}
	}
	if r.Steps < 0 || r.Steps > 100000 || r.PromptTokens < 0 || r.PromptTokens > 1e12 || (r.CachedTokens != nil && (*r.CachedTokens < 0 || *r.CachedTokens > 1e12)) || r.CompletionTokens < 0 || r.CompletionTokens > 1e12 {
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

// checkKind accepts a kind or a role: lower-case letters, digits, '_' and '-', 1-32, a letter first.
func checkKind(field, s string) error {
	if !taskKindRe.MatchString(s) {
		return fmt.Errorf("%s: 1-32 lower-case letters, digits, '_' or '-', a letter first", field)
	}
	return nil
}

// checkClosedBy accepts what closed a task: a task's id, or "reuse:<by>".
func checkClosedBy(s string) error {
	if !taskClosedByRe.MatchString(s) || publicSecret.MatchString(s) {
		return errors.New("closed_by: a task's id or reuse:<by>, 1-200 characters without spaces")
	}
	return nil
}

// checkLabels accepts at most maxTaskLabels public labels: a name of lower-case letters, digits and
// '_', and a value of 1-maxTaskLabelValue printable bytes that holds nothing key-shaped and no
// e-mail address. Labels are public (GET /v1/tasks).
func checkLabels(labels map[string]string) error {
	if len(labels) > maxTaskLabels {
		return fmt.Errorf("labels: at most %d", maxTaskLabels)
	}
	var errs []error
	for k, v := range labels {
		switch {
		case !taskLabelKeyRe.MatchString(k):
			errs = append(errs, fmt.Errorf("labels: name %q: 1-32 lower-case letters, digits or '_', a letter first", k))
		case v == "" || len(v) > maxTaskLabelValue || !utf8.ValidString(v) || !utf8Printable(v):
			errs = append(errs, fmt.Errorf("labels.%s: 1-%d printable bytes", k, maxTaskLabelValue))
		case publicSecret.MatchString(v) || publicEmail.MatchString(v):
			errs = append(errs, fmt.Errorf("labels.%s: looks like a key or an e-mail address; labels are public", k))
		}
	}
	return errors.Join(errs...)
}

// validate checks a create request. kinds are the project's declared kinds: a task's kind is one of
// them or "other".
func (n *taskNew) validate(kinds []string) error {
	errs := []error{checkID("id", n.ID), checkID("created_by", n.CreatedBy), checkText("title", n.Title, 200)}
	if err := checkKind("kind", n.Kind); err != nil {
		errs = append(errs, err)
	} else if n.Kind != kindOther && !slices.Contains(kinds, n.Kind) {
		errs = append(errs, fmt.Errorf("kind: %q is not one of this project's kinds (other, or one its configuration declares)", n.Kind))
	}
	if n.Spec == nil {
		errs = append(errs, errors.New("spec: required (a JSON object)"))
	} else if err := checkObject("spec", n.Spec, maxTaskSpec); err != nil {
		errs = append(errs, err)
	}
	if n.Order < -maxTaskOrder || n.Order > maxTaskOrder {
		errs = append(errs, fmt.Errorf("order: %d to %d (lower is claimed first)", -maxTaskOrder, maxTaskOrder))
	}
	if n.MaxTries < 0 || n.MaxTries > maxMaxTries {
		errs = append(errs, fmt.Errorf("max_tries: 1 to %d (0: %d)", maxMaxTries, defaultMaxTries))
	}
	if n.Role != "" {
		if err := checkKind("role", n.Role); err != nil {
			errs = append(errs, err)
		}
	}
	for _, f := range []struct{ field, ref string }{{"parent", n.Parent}, {"retry_of", n.RetryOf}, {"replaces", n.Replaces}} {
		if f.ref == "" {
			continue
		}
		if err := checkID(f.field, f.ref); err != nil {
			errs = append(errs, err)
		} else if f.ref == n.ID {
			errs = append(errs, fmt.Errorf("%s: not the task itself", f.field))
		}
	}
	if len(n.Needs) > maxTaskNeeds {
		errs = append(errs, fmt.Errorf("needs: at most %d tasks", maxTaskNeeds))
	} else {
		seen := map[string]bool{}
		for _, ref := range n.Needs {
			if err := checkID("needs", ref); err != nil {
				errs = append(errs, err)
			} else if ref == n.ID {
				errs = append(errs, errors.New("needs: not the task itself"))
			} else if seen[ref] {
				errs = append(errs, fmt.Errorf("needs: %s twice", ref))
			}
			seen[ref] = true
		}
	}
	if n.HoleID != "" && !taskHoleRe.MatchString(n.HoleID) {
		errs = append(errs, errors.New("hole_id: 1-64 lower-case letters, digits, '.', '_', ':' or '-'"))
	}
	if err := checkLabels(n.Labels); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
