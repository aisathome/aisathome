// SPDX-License-Identifier: Apache-2.0
package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	campPath = "/v1/projects/prove2me/campaigns"
	resPath  = "/v1/projects/prove2me/results"
	itemLink = "https://items.example/theorems/"
)

// sciRig: prove2me (public, links under itemLink), lab (not public, no links).
func sciRig(t *testing.T, dir string) *taskRig {
	t.Helper()
	g := newTestGateway(t, Config{StateDir: dir, Projects: []Project{
		{ID: "prove2me", KeySHA256: []string{keyHash(testProjectKey)}, PublicQueue: true, Kinds: []string{"p2m-v7"}, PublicLinks: []string{itemLink}},
		{ID: "lab", KeySHA256: []string{keyHash(otherProjectKey)}},
	}})
	r := &taskRig{t: t, g: g, now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(func() { g.Close() })
	return r
}

func (r *taskRig) want(code int, method, path, key string, body any) map[string]any {
	r.t.Helper()
	w, out := r.call(method, path, key, body)
	if w.Code != code {
		r.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, w.Code, code, w.Body)
	}
	return out
}

func trialBody() map[string]any {
	return map[string]any{"title": "Trial 8", "status": "running", "labels": map[string]string{"scaffold": "v9", "budget": "110 steps"},
		"text": "Eight leaves.\nOne calibration leaf.", "items": []map[string]any{
			{"id": "p2m-3ad60f", "title": "Foo.bar", "url": itemLink + "3ad60fc3", "role": "calibration", "labels": map[string]string{"alias": "C07"}},
			{"id": "p2m-754bbc", "title": "RAREPricing.Existence.observation_3", "url": itemLink + "754bbc99", "role": "open"},
		}}
}

const extID = "0f6c1d2e-1111-4a2b-9c3d-00000000a001" // a service's own id: never public

func resultBody(state string) map[string]any {
	return map[string]any{"source": "prove2me", "external_id": extID, "campaign": "t8", "item": "p2m-754bbc",
		"task": "t8-p2m-754bbc-s1-asm", "url": itemLink + "754bbc99", "state": state, "verdict": "",
		"submitted_at": "2026-11-17T12:05:00Z", "labels": map[string]string{"kind": "assembly"}}
}

// Write needs the project's own key; reads of a public project need none, another project's need its
// key; an unknown project is 404.
func TestCampaignAuth(t *testing.T) {
	r := sciRig(t, t.TempDir())
	r.want(401, "PUT", campPath+"/t8", "", trialBody())
	r.want(403, "PUT", campPath+"/t8", otherProjectKey, trialBody())
	r.want(401, "POST", resPath, "", resultBody("pending"))
	r.want(403, "POST", resPath, otherProjectKey, resultBody("pending"))
	r.want(201, "PUT", campPath+"/t8", testProjectKey, trialBody())
	r.want(401, "PATCH", campPath+"/t8", "", map[string]any{"status": "finished"})
	r.want(401, "DELETE", campPath+"/t8", "", nil)
	r.want(403, "DELETE", campPath+"/t8", otherProjectKey, nil)

	w, _ := r.call("GET", campPath, "", nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=15" {
		t.Fatalf("public list: HTTP %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	r.want(200, "GET", campPath+"/t8", "", nil)
	r.want(200, "GET", resPath, "", nil)
	r.want(404, "GET", "/v1/projects/nope/campaigns", "", nil)

	// lab: not public
	r.want(201, "PUT", "/v1/projects/lab/campaigns/c1", otherProjectKey, map[string]any{"status": "preparing"})
	r.want(401, "GET", "/v1/projects/lab/campaigns", "", nil)
	r.want(401, "GET", "/v1/projects/lab/campaigns/c1", "", nil)
	r.want(401, "GET", "/v1/projects/lab/results", "", nil)
	r.want(403, "GET", "/v1/projects/lab/campaigns", testProjectKey, nil)
	w, out := r.call("GET", "/v1/projects/lab/campaigns", otherProjectKey, nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || len(out["campaigns"].([]any)) != 1 {
		t.Fatalf("lab with its key: HTTP %d %q %v", w.Code, w.Header().Get("Cache-Control"), out)
	}
	// one project's campaigns are not another's
	if out := r.want(200, "GET", campPath, "", nil); len(out["campaigns"].([]any)) != 1 {
		t.Fatalf("prove2me sees %v", out)
	}
}

// A PUT stores the whole campaign: 201 the first time, 200 and unchanged when repeated, 200 and
// changed when it differs. Running sets started_at; finishing sets ended_at; reopening clears it.
func TestCampaignPutPatch(t *testing.T) {
	r := sciRig(t, t.TempDir())
	out := r.want(201, "PUT", campPath+"/t8", testProjectKey, trialBody())
	c := out["campaign"].(map[string]any)
	if out["created"] != true || c["status"] != "running" || c["started_at"] == nil || c["ended_at"] != nil || c["title"] != "Trial 8" ||
		c["project"] != "prove2me" || c["id"] != "t8" || len(c["items"].([]any)) != 2 {
		t.Fatalf("created %v", out)
	}
	started, updated := c["started_at"], c["updated_at"]
	time.Sleep(1100 * time.Millisecond) // the records keep seconds: a write would show
	out = r.want(200, "PUT", campPath+"/t8", testProjectKey, trialBody())
	if c := out["campaign"].(map[string]any); out["created"] != false || c["updated_at"] != updated || c["started_at"] != started {
		t.Fatalf("repeated PUT changed it: %v", out)
	}
	b := trialBody()
	b["title"] = "Trial 8 (v9)"
	out = r.want(200, "PUT", campPath+"/t8", testProjectKey, b)
	if c := out["campaign"].(map[string]any); c["title"] != "Trial 8 (v9)" || c["updated_at"] == updated || c["started_at"] != started {
		t.Fatalf("changed PUT: %v", out)
	}
	// items are in order, with their fields
	it := out["campaign"].(map[string]any)["items"].([]any)
	first := it[0].(map[string]any)
	if first["id"] != "p2m-3ad60f" || first["role"] != "calibration" || first["url"] != itemLink+"3ad60fc3" ||
		first["labels"].(map[string]any)["alias"] != "C07" || it[1].(map[string]any)["id"] != "p2m-754bbc" {
		t.Fatalf("items %v", it)
	}

	// finish, with the owner's closing text
	out = r.want(200, "PATCH", campPath+"/t8", testProjectKey, map[string]any{"status": "finished", "text": "Closing words."})
	c = out["campaign"].(map[string]any)
	if c["status"] != "finished" || c["ended_at"] == nil || c["text"] != "Closing words." || c["title"] != "Trial 8 (v9)" ||
		len(c["items"].([]any)) != 2 || c["started_at"] != started {
		t.Fatalf("finished %v", c)
	}
	ended := c["ended_at"]
	if c := r.want(200, "PATCH", campPath+"/t8", testProjectKey, map[string]any{"status": "finished"})["campaign"].(map[string]any); c["ended_at"] != ended {
		t.Fatalf("finishing again moved ended_at: %v", c)
	}
	if c := r.want(200, "PATCH", campPath+"/t8", testProjectKey, map[string]any{"status": "running"})["campaign"].(map[string]any); c["ended_at"] != nil {
		t.Fatalf("reopened keeps ended_at: %v", c)
	}
	r.want(404, "PATCH", campPath+"/t9", testProjectKey, map[string]any{"status": "finished"})

	// a backfilled campaign with its own dates
	out = r.want(201, "PUT", campPath+"/t4", testProjectKey, map[string]any{"title": "Trial 4", "status": "finished",
		"started_at": "2026-11-02T08:00:00+01:00", "ended_at": "2026-11-03T18:30:00Z"})
	if c := out["campaign"].(map[string]any); c["started_at"] != "2026-11-02T07:00:00Z" || c["ended_at"] != "2026-11-03T18:30:00Z" {
		t.Fatalf("backfill %v", c)
	}
	// preparing has no times
	if c := r.want(201, "PUT", campPath+"/t10", testProjectKey, map[string]any{"status": "preparing"})["campaign"].(map[string]any); c["started_at"] != nil || c["ended_at"] != nil {
		t.Fatalf("preparing %v", c)
	}
}

// Everything a campaign or a result carries is checked: shapes, states, times, public text, links
// under the project's prefixes, labels, sizes, unknown fields.
func TestCampaignValidation(t *testing.T) {
	r := sciRig(t, t.TempDir())
	mod := func(f func(b map[string]any)) map[string]any {
		b := trialBody()
		f(b)
		return b
	}
	item := func(k string, v any) map[string]any {
		return mod(func(b map[string]any) { b["items"].([]map[string]any)[0][k] = v })
	}
	for name, b := range map[string]any{
		"status":        mod(func(b map[string]any) { b["status"] = "done" }),
		"ended running": mod(func(b map[string]any) { b["ended_at"] = "2026-11-17T13:00:00Z" }),
		"ended before": mod(func(b map[string]any) {
			b["status"], b["started_at"], b["ended_at"] = "finished", "2026-11-17T13:00:00Z", "2026-11-17T12:00:00Z"
		}),
		"year":             mod(func(b map[string]any) { b["started_at"] = "1999-01-01T00:00:00Z" }),
		"text email":       mod(func(b map[string]any) { b["text"] = "write to someone@example.org" }),
		"text key":         mod(func(b map[string]any) { b["text"] = "key sk-abcdefgh12345678" }),
		"text control":     mod(func(b map[string]any) { b["text"] = "a\x00b" }),
		"text long":        mod(func(b map[string]any) { b["text"] = strings.Repeat("x", maxCampaignText+1) }),
		"title long":       mod(func(b map[string]any) { b["title"] = strings.Repeat("x", maxCampaignTitle+1) }),
		"label key":        mod(func(b map[string]any) { b["labels"] = map[string]string{"Bad-Key": "x"} }),
		"label email":      mod(func(b map[string]any) { b["labels"] = map[string]string{"who": "a@b.org"} }),
		"unknown field":    mod(func(b map[string]any) { b["owner"] = "someone" }),
		"item id":          item("id", "bad id"),
		"item twice":       mod(func(b map[string]any) { b["items"].([]map[string]any)[1]["id"] = "p2m-3ad60f" }),
		"item role":        item("role", "Calibration!"),
		"item other host":  item("url", "https://elsewhere.example/theorems/x"),
		"item prefix only": item("url", "https://items.example/theoremsX"),
		"item http":        item("url", "http://items.example/theorems/x"),
		"item query":       item("url", itemLink+"x?token=abc"),
		"item fragment":    item("url", itemLink+"x#y"),
		"item userinfo":    item("url", "https://u:p@items.example/theorems/x"),
		"item dot-dot":     item("url", itemLink+"../submissions/private"),
		"item dot":         item("url", itemLink+"./x"),
		"item escaped dot": item("url", itemLink+"%2e%2e/submissions/private"),
		"item backslash":   item("url", itemLink+"..\\submissions"),
		"item space":       item("url", itemLink+".. "),
		"item inner space": item("url", itemLink+"a b"),
		"item label":       item("labels", map[string]string{"alias": strings.Repeat("x", maxTaskLabelValue+1)}),
		"item title":       item("title", "mail someone@example.org"),
	} {
		if w, _ := r.call("PUT", campPath+"/t8", testProjectKey, b); w.Code != 400 {
			t.Errorf("%s: HTTP %d %s", name, w.Code, w.Body)
		}
	}
	r.want(400, "PUT", campPath+"/bad%20id", testProjectKey, trialBody())
	many := mod(func(b map[string]any) {
		var its []map[string]any
		for i := range maxCampaignItems + 1 {
			its = append(its, map[string]any{"id": "i" + strconv.Itoa(i)})
		}
		b["items"] = its
	})
	r.want(400, "PUT", campPath+"/t8", testProjectKey, many)
	if w, _ := r.call("PUT", campPath+"/t8", testProjectKey, strings.Repeat(" ", maxCampaignBody+1)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("large body: HTTP %d", w.Code)
	}
	// a project without public_links may not link
	r.want(400, "PUT", "/v1/projects/lab/campaigns/c1", otherProjectKey, map[string]any{"status": "running",
		"items": []map[string]any{{"id": "x", "url": itemLink + "x"}}})
	r.want(201, "PUT", "/v1/projects/lab/campaigns/c1", otherProjectKey, map[string]any{"status": "running",
		"items": []map[string]any{{"id": "x", "title": "A theorem"}}})
	if out := r.want(200, "GET", campPath, "", nil); len(out["campaigns"].([]any)) != 0 {
		t.Fatalf("refused campaigns stored: %v", out)
	}

	// results
	r.want(201, "PUT", campPath+"/t8", testProjectKey, trialBody())
	res := func(k string, v any) map[string]any {
		b := resultBody("accepted")
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
		return b
	}
	for name, b := range map[string]any{
		"source":           res("source", "Prove2Me"),
		"external_id":      res("external_id", "has space"),
		"no external_id":   res("external_id", nil),
		"state":            res("state", "ACCEPTED"),
		"no submitted_at":  res("submitted_at", nil),
		"campaign unknown": res("campaign", "t99"),
		"campaign shape":   res("campaign", "t 8"),
		"item":             res("item", "bad item"),
		"task":             res("task", "bad task"),
		"url":              res("url", "https://elsewhere.example/x"),
		"verdict email":    res("verdict", "contact x@y.org"),
		"verdict long":     res("verdict", strings.Repeat("x", maxResultVerdict+1)),
		"verdict before":   res("verdict_at", "2026-11-17T12:00:00Z"),
		"pending verdict":  map[string]any{"source": "prove2me", "external_id": "x", "state": "pending", "submitted_at": "2026-11-17T12:00:00Z", "verdict_at": "2026-11-17T12:10:00Z"},
		"labels":           res("labels", map[string]string{"k": "sk-abcdefgh12345678"}),
		"unknown field":    res("user_id", "u-0000feed"),
	} {
		if w, _ := r.call("POST", resPath, testProjectKey, b); w.Code != 400 {
			t.Errorf("result %s: HTTP %d %s", name, w.Code, w.Body)
		}
	}
	if w, _ := r.call("POST", resPath, testProjectKey, strings.Repeat(" ", maxResultBody+1)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("large result: HTTP %d", w.Code)
	}
	if out := r.want(200, "GET", resPath, "", nil); len(out["results"].([]any)) != 0 {
		t.Fatalf("refused results stored: %v", out)
	}
}

// A result is idempotent by (source, external_id): 201, then 200 unchanged; a verdict updates it and
// stamps verdict_at; a late "pending" retry after the verdict changes nothing. The external id is in
// the writer's answer only, never in a read.
func TestResultsIdempotentAndPublic(t *testing.T) {
	r := sciRig(t, t.TempDir())
	r.want(201, "PUT", campPath+"/t8", testProjectKey, trialBody())
	out := r.want(201, "POST", resPath, testProjectKey, resultBody("pending"))
	res := out["result"].(map[string]any)
	id := res["id"].(string)
	if out["created"] != true || !resultIDRe.MatchString(id) || res["external_id"] != extID || res["state"] != "pending" || res["verdict_at"] != nil ||
		res["campaign"] != "t8" || res["item"] != "p2m-754bbc" || res["task"] != "t8-p2m-754bbc-s1-asm" {
		t.Fatalf("created %v", out)
	}
	updated := res["updated_at"]
	time.Sleep(1100 * time.Millisecond)
	if out := r.want(200, "POST", resPath, testProjectKey, resultBody("pending")); out["created"] != false || out["result"].(map[string]any)["updated_at"] != updated {
		t.Fatalf("repeat %v", out)
	}
	acc := resultBody("accepted")
	acc["verdict"] = "Proof verified."
	out = r.want(200, "POST", resPath, testProjectKey, acc)
	res = out["result"].(map[string]any)
	if out["created"] != false || res["id"] != id || res["state"] != "accepted" || res["verdict_at"] == nil || res["verdict"] != "Proof verified." {
		t.Fatalf("verdict %v", out)
	}
	verdictAt, updated := res["verdict_at"], res["updated_at"]
	time.Sleep(1100 * time.Millisecond)
	if res := r.want(200, "POST", resPath, testProjectKey, acc)["result"].(map[string]any); res["verdict_at"] != verdictAt || res["updated_at"] != updated {
		t.Fatalf("the same verdict again moved verdict_at: %v", res)
	}
	// a late retry of the submission
	if res := r.want(200, "POST", resPath, testProjectKey, resultBody("pending"))["result"].(map[string]any); res["state"] != "accepted" || res["verdict_at"] != verdictAt {
		t.Fatalf("a late pending write undid the verdict: %v", res)
	}
	// a second result, another source with the same external id is another result
	other := resultBody("rejected")
	other["source"] = "elsewhere"
	other["submitted_at"] = "2026-11-17T12:30:00Z"
	r.want(201, "POST", resPath, testProjectKey, other)
	third := resultBody("error")
	third["external_id"], third["campaign"], third["item"], third["task"], third["url"] = "x-3", "", "", "", ""
	third["submitted_at"] = "2026-11-17T11:00:00Z"
	r.want(201, "POST", resPath, testProjectKey, third)

	// the reads: never the external id
	for _, path := range []string{resPath, resPath + "?campaign=t8", campPath, campPath + "/t8"} {
		w, _ := r.call("GET", path, "", nil)
		for _, bad := range []string{extID, "external_id", "x-3", "u-0000feed"} {
			if strings.Contains(w.Body.String(), bad) {
				t.Errorf("GET %s shows %q: %s", path, bad, w.Body)
			}
		}
	}
	out = r.want(200, "GET", resPath, "", nil)
	rs := out["results"].([]any)
	counts := out["counts"].(map[string]any)
	if len(rs) != 3 || rs[0].(map[string]any)["source"] != "elsewhere" || rs[1].(map[string]any)["id"] != id ||
		counts["accepted"] != 1.0 || counts["rejected"] != 1.0 || counts["error"] != 1.0 || counts["pending"] != 0.0 {
		t.Fatalf("list %v", out)
	}
	if third := rs[2].(map[string]any); third["campaign"] != nil || third["item"] != nil || third["url"] != nil || third["task"] != nil {
		t.Errorf("empty refs are null: %v", third)
	}
	// filters: state narrows the list, not the counts
	out = r.want(200, "GET", resPath+"?state=accepted,rejected&campaign=t8", "", nil)
	if rs := out["results"].([]any); len(rs) != 2 || out["counts"].(map[string]any)["error"] != 0.0 {
		t.Fatalf("filtered %v", out)
	}
	if rs := r.want(200, "GET", resPath+"?source=prove2me&item=p2m-754bbc", "", nil)["results"].([]any); len(rs) != 1 {
		t.Fatalf("by source and item %v", rs)
	}
	for _, q := range []string{"?state=ok", "?campaign=t%208", "?limit=0", "?limit=501", "?after=x", "?source=BAD"} {
		r.want(400, "GET", resPath+q, "", nil)
	}
	// the campaign counts its results
	c := r.want(200, "GET", campPath+"/t8", "", nil)["campaign"].(map[string]any)
	if rc := c["results"].(map[string]any); rc["accepted"] != 1.0 || rc["rejected"] != 1.0 || rc["error"] != 0.0 {
		t.Fatalf("campaign counts %v", rc)
	}

	// deleting: a campaign with results stays; a result goes
	r.want(409, "DELETE", campPath+"/t8", testProjectKey, nil)
	r.want(404, "DELETE", resPath+"/r-0000000000000000", testProjectKey, nil)
	r.want(403, "DELETE", resPath+"/"+id, otherProjectKey, nil)
	r.want(200, "DELETE", resPath+"/"+id, testProjectKey, nil)
	r.want(200, "DELETE", resPath+"/"+resultID("elsewhere", extID), testProjectKey, nil)
	r.want(200, "DELETE", campPath+"/t8", testProjectKey, nil)
	r.want(404, "GET", campPath+"/t8", "", nil)
	r.want(404, "DELETE", campPath+"/t8", testProjectKey, nil)
}

// Lists page newest first with a cursor that neither repeats nor skips; caps refuse more records.
func TestCampaignAndResultPages(t *testing.T) {
	r := sciRig(t, t.TempDir())
	for i := 1; i <= 5; i++ {
		r.want(201, "PUT", campPath+"/t"+strconv.Itoa(i), testProjectKey, map[string]any{"status": "finished",
			"started_at": time.Date(2026, 11, i, 8, 0, 0, 0, time.UTC).Format(time.RFC3339)})
	}
	var ids []string
	after := ""
	for range 5 {
		out := r.want(200, "GET", campPath+"?limit=2"+after, "", nil)
		for _, c := range out["campaigns"].([]any) {
			ids = append(ids, c.(map[string]any)["id"].(string))
		}
		if out["next"] == nil {
			break
		}
		after = "&after=" + out["next"].(string)
	}
	if strings.Join(ids, ",") != "t5,t4,t3,t2,t1" {
		t.Fatalf("campaign pages %v", ids)
	}
	for i := range 5 {
		b := map[string]any{"source": "s", "external_id": "e" + strconv.Itoa(i), "state": "pending",
			"submitted_at": time.Date(2026, 11, 17, 12, i, 0, 0, time.UTC).Format(time.RFC3339)}
		r.want(201, "POST", resPath, testProjectKey, b)
	}
	ids, after = nil, ""
	for range 5 {
		out := r.want(200, "GET", resPath+"?limit=2"+after, "", nil)
		for _, c := range out["results"].([]any) {
			ids = append(ids, c.(map[string]any)["id"].(string))
		}
		if out["next"] == nil {
			break
		}
		after = "&after=" + out["next"].(string)
	}
	if len(ids) != 5 || ids[0] != resultID("s", "e4") || ids[4] != resultID("s", "e0") {
		t.Fatalf("result pages %v", ids)
	}

	defer func(c, n int) { maxCampaignsPerProj, maxResultsPerProj = c, n }(maxCampaignsPerProj, maxResultsPerProj)
	maxCampaignsPerProj, maxResultsPerProj = 5, 5
	r.want(409, "PUT", campPath+"/t6", testProjectKey, map[string]any{"status": "preparing"})
	r.want(200, "PUT", campPath+"/t5", testProjectKey, map[string]any{"status": "running"}) // an update is not a new one
	r.want(409, "POST", resPath, testProjectKey, map[string]any{"source": "s", "external_id": "e9", "state": "pending", "submitted_at": "2026-11-17T12:00:00Z"})
	r.want(200, "POST", resPath, testProjectKey, map[string]any{"source": "s", "external_id": "e1", "state": "accepted", "submitted_at": "2026-11-17T12:01:00Z"})
}

// public_links in the config: https prefixes ending in "/".
func TestProjectPublicLinks(t *testing.T) {
	for _, l := range []string{"http://x.example/", "https://x.example/items", "https://u@x.example/", "https://x.example/?a=1", "https:///", "https://x.example/a/../b/", "https://x.example/%2e/"} {
		if _, err := newProjectTable([]Project{{ID: "p", KeySHA256: []string{keyHash("k-1234567890")}, PublicLinks: []string{l}}}); err == nil {
			t.Errorf("%q accepted", l)
		}
	}
	if _, err := newProjectTable([]Project{{ID: "p", KeySHA256: []string{keyHash("k-1234567890")}, PublicLinks: []string{itemLink, "https://x.example/"}}}); err != nil {
		t.Error(err)
	}
}
