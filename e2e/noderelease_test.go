// The latest node release for the website (GET /v1/node/release): read from a stand-in for GitHub's
// API at start and served without authentication. The fetcher's failures and backoff:
// gateway/noderelease_test.go.
//
// SPDX-License-Identifier: Apache-2.0
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aisathome/aisathome/gateway"
)

func TestNodeReleaseIsPublic(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/aisathome/aisathome/releases/latest" {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"tag_name":"v0.8.0","draft":false,"prerelease":false,"assets":[
			{"name":"AIsAtHome-node-setup-v0.8.0.exe","size":391000000,"state":"uploaded"},
			{"name":"AIsAtHome-node-setup.exe","size":391000000,"state":"uploaded"},
			{"name":"aisathome-node-bundle-linux-amd64.tar.xz","size":480000000,"state":"uploaded"}]}`))
	}))
	defer gh.Close()
	r := newRigCfg(t, func(c *gateway.Config) { c.NodeReleaseURL = gh.URL + "/repos/aisathome/aisathome/releases/latest" })

	get := func() (int, map[string]any) {
		resp, err := http.Get(r.gw.URL + "/v1/node/release") // no API key
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}
	var got map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, m := get()
		if code == http.StatusOK {
			got = m
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no release after 5 s: %d %v", code, m)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got["version"] != "v0.8.0" || got["installerBytes"] != 391e6 || got["bundleBytes"] != 480e6 || got["versionFreeInstaller"] != true {
		t.Fatalf("release %v", got)
	}
}
