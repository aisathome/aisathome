// SPDX-License-Identifier: Apache-2.0
package bridge

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rpcServer answers like the stock create_account.php and am_get_info.php (BOINC's html/user/).
func rpcServer(t *testing.T, invite string) (*httptest.Server, *[]string) {
	var seen []string
	auth := strings.Repeat("ab", 16)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /project/create_account.php", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		seen = append(seen, r.URL.RawQuery)
		if q.Get("invite_code") != invite {
			fmt.Fprint(w, "<error>\n    <error_num>-1</error_num>\n    <error_msg>Invalid invitation code</error_msg>\n</error>\n")
			return
		}
		if q.Get("email_addr") == "taken@example.org" {
			fmt.Fprint(w, "<error>\n    <error_num>-137</error_num>\n    <error_msg>Name or email address is not unique</error_msg>\n</error>\n")
			return
		}
		fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"ISO-8859-1\" ?>\n <account_out>\n   <authenticator>%s</authenticator>\n</account_out>\n", auth)
	})
	mux.HandleFunc("GET /project/am_get_info.php", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("account_key") {
		case auth:
			fmt.Fprintf(w, "<am_get_info_reply>\n    <success/>\n    <id>42</id>\n<name>x</name>\n<country></country>\n<weak_auth>42_%s</weak_auth>\n<cpid>%s</cpid>\n<teamid>0</teamid>\n</am_get_info_reply>\n",
				strings.Repeat("cd", 16), strings.Repeat("ef", 16))
		case "mismatch0000000000000000000000000":
			fmt.Fprintf(w, "<am_get_info_reply><success/><id>42</id><weak_auth>43_%s</weak_auth></am_get_info_reply>", strings.Repeat("cd", 16))
		default:
			fmt.Fprint(w, "<error>\n    <error_num>-136</error_num>\n    <error_msg>Not found</error_msg>\n</error>\n")
		}
	})
	mux.HandleFunc("GET /redirect/am_get_info.php", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/?"+r.URL.RawQuery, http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestWebRPC(t *testing.T) {
	srv, seen := rpcServer(t, "code123")
	r := NewWebRPC(srv.URL+"/project/", "code123")
	auth, err := r.CreateAccount(ctx, "a+b@example.org", md5Hex("x"), "aisathome-abc123")
	if err != nil || auth != strings.Repeat("ab", 16) {
		t.Fatalf("CreateAccount: %q %v", auth, err)
	}
	if q := (*seen)[0]; !strings.Contains(q, "email_addr=a%2Bb%40example.org") || !strings.Contains(q, "invite_code=code123") {
		t.Errorf("query %q", q)
	}
	_, err = r.CreateAccount(ctx, "taken@example.org", md5Hex("x"), "n")
	var re *RPCError
	if !errors.As(err, &re) || re.Num != -137 {
		t.Errorf("taken: %v", err)
	}
	a, err := r.AccountInfo(ctx, auth)
	if err != nil || a.ID != 42 || a.WeakAuth != "42_"+strings.Repeat("cd", 16) {
		t.Errorf("AccountInfo: %+v %v", a, err)
	}
	if _, err = r.AccountInfo(ctx, "mismatch0000000000000000000000000"); err == nil || errors.As(err, &re) {
		t.Errorf("a weak key of another user: %v", err)
	}
	_, err = r.AccountInfo(ctx, strings.Repeat("0", 32))
	if !errors.As(err, &re) || re.Num != errDBNotFound {
		t.Errorf("unknown key: %v", err)
	}

	// A wrong invitation code is BOINC's -1 (create_account.php), not a failed call.
	_, err = NewWebRPC(srv.URL+"/project/", "wrong").CreateAccount(ctx, "a@example.org", md5Hex("x"), "n")
	if !errors.As(err, &re) || re.Num != -1 {
		t.Errorf("wrong code: %v", err)
	}
}

func TestWebRPCKeepsKeysOutOfErrors(t *testing.T) {
	srv, _ := rpcServer(t, "")
	key := strings.Repeat("9", 32)
	// A redirect is not followed (it would carry the key elsewhere) and is an error.
	_, err := NewWebRPC(srv.URL+"/redirect/", "").AccountInfo(ctx, key)
	if err == nil || strings.Contains(err.Error(), key) {
		t.Errorf("redirect: %v", err)
	}
	// A server that is not there: the error names the page, not the URL with its query.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	_, err = NewWebRPC(dead.URL+"/project/", "").AccountInfo(ctx, key)
	if err == nil || strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "am_get_info.php") {
		t.Errorf("down: %v", err)
	}
	_, err = NewWebRPC(dead.URL+"/project/", "").CreateAccount(ctx, "secret@example.org", key, "n")
	if err == nil || strings.Contains(err.Error(), "secret@example.org") || strings.Contains(err.Error(), key) {
		t.Errorf("down: %v", err)
	}
}
