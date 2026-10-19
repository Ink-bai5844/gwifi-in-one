package portal

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const testIV = "0123456789abcdef"

const loginHTML = `<html><form id="loginForm">
<input name="sign" type="hidden" value="one&amp;two">
<input name="sta_ip" type="hidden" value="192.0.2.10">
<input name="iv" type="hidden" value="0123456789abcdef">
<input name="user_account" value="">
<input name="user_password" type="password" value="">
<input name="ignored" value="private" disabled>
<input name="choice" type="checkbox" value="no">
<input name="choice" type="checkbox" value="yes" checked>
<input name="repeat" type="hidden" value="a"><input name="repeat" type="hidden" value="b">
<select name="account_type"><option value="1">One</option><option value="2">Two</option></select>
<input type="submit" name="submit" value="Login">
</form></html>`

func TestInspectDecodesCurrentForm(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != UserAgent {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		fmt.Fprint(w, loginHTML)
	}))
	defer server.Close()
	page, err := Inspect(context.Background(), server.Client(), server.URL+"/gportal/web/login?wlanuserip=192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if page.Action != server.URL+loginPath || page.IV != testIV {
		t.Fatalf("wrong action or IV: %q", page.Action)
	}
	if page.Fields.Get("sign") != "one&two" || page.Fields.Get("choice") != "yes" || page.Fields.Get("account_type") != "1" {
		t.Fatalf("form was not decoded correctly: %v", page.Fields)
	}
	if !reflect.DeepEqual(page.Fields["repeat"], []string{"a", "b"}) {
		t.Fatal("repeated fields lost")
	}
	for _, name := range []string{"ignored", "submit"} {
		if _, exists := page.Fields[name]; exists {
			t.Errorf("unsuccessful control included: %s", name)
		}
	}
}

func TestInspectRejectsInvalidPages(t *testing.T) {
	for _, tc := range []struct{ name, body, query string }{
		{"not a login form", `<form id="other"></form>`, ""},
		{"invalid IV", strings.Replace(loginHTML, testIV, "short", 1), ""},
		{"missing account", strings.Replace(loginHTML, `name="user_account"`, `name="other"`, 1), ""},
		{"duplicate IV", strings.Replace(loginHTML, "</form>", `<input name="iv" value="0123456789abcdef"></form>`, 1), ""},
		{"cross origin action", strings.Replace(loginHTML, `<form id="loginForm">`, `<form id="loginForm" action="https://example.invalid/collect">`, 1), ""},
		{"javascript action", strings.Replace(loginHTML, `<form id="loginForm">`, `<form id="loginForm" action="javascript:alert(1)">`, 1), ""},
		{"stale terminal IP", loginHTML, "?wlanuserip=192.0.2.11"},
		{"oversized page", strings.Repeat("x", maxPageBytes+1), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer server.Close()
			if _, err := Inspect(context.Background(), server.Client(), server.URL+tc.query); err == nil {
				t.Fatal("invalid page accepted")
			}
		})
	}
}

func TestInspectDoesNotFollowCrossOriginRedirect(t *testing.T) {
	var requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); fmt.Fprint(w, loginHTML) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	if _, err := Inspect(context.Background(), source.Client(), source.URL); err == nil {
		t.Fatal("cross-origin redirect accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("cross-origin target was contacted")
	}
}

func TestSerializeMatchesJQueryEscaping(t *testing.T) {
	fields := url.Values{"pass": {"a b+c&=中文!~*'()"}}
	want := "pass=a%20b%2Bc%26%3D%E4%B8%AD%E6%96%87!~*'()"
	if got := serialize(fields); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestEncryptFormKnownVector(t *testing.T) {
	// Independently generated with System.Security.Cryptography AES-CBC, an
	// explicit zero-filled block buffer, and PaddingMode.None.
	plain := "user_account=alice&user_password=a%20b%2Bc%26%3D!"
	want := "45UYHfinftoJj3WqMjnZ8Ba0wBYVF3t5krM2BQWhNSrG5A5bsfyOOZPil1Udu9D3rEvlEXnj07rU2eCv+o+chQ=="
	got, err := encryptForm(plain, testIV)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("encryption did not match independent vector")
	}
	aligned, err := encryptForm("0123456789abcdef", testIV)
	if err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(aligned)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != aes.BlockSize {
		t.Fatal("ZeroPadding added an unnecessary block to aligned input")
	}
}

func decryptFields(t *testing.T, encoded, iv string) url.Values {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		t.Fatal("invalid ciphertext length")
	}
	block, err := aes.NewCipher([]byte(protocolKey))
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCDecrypter(block, []byte(iv)).CryptBlocks(data, data)
	fields, err := url.ParseQuery(string(bytes.TrimRight(data, "\x00")))
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func testPage(base string) *Page {
	return &Page{URL: base + "/gportal/web/login", Action: base + loginPath, IV: testIV,
		Fields: url.Values{"iv": {testIV}, "sign": {"current-sign"}, "sta_ip": {"192.0.2.10"}, "user_account": {""}, "user_password": {""}}}
}

func TestLoginSendsEncryptedCurrentFields(t *testing.T) {
	account, password := "alice", "a b+c&=中文!~*'()"
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != loginPath {
			t.Errorf("unexpected login request: %s %s", r.Method, r.URL.Path)
		}
		if r.UserAgent() != UserAgent {
			t.Error("login must use the same desktop User-Agent as the form")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if len(r.PostForm) != 2 || r.PostForm.Get("iv") != testIV {
			t.Fatal("outer payload must only contain encrypted data and IV")
		}
		fields := decryptFields(t, r.PostForm.Get("data"), r.PostForm.Get("iv"))
		if fields.Get("user_account") != account || fields.Get("user_password") != password || fields.Get("sign") != "current-sign" || fields.Get("sta_ip") != "192.0.2.10" {
			t.Error("login form round trip failed")
		}
		if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
			t.Error("missing AJAX header")
		}
		fmt.Fprint(w, `{"status":1,"info":"ok","data":"/disconnect-another-device"}`)
	}))
	defer server.Close()
	page := testPage(server.URL)
	if err := Login(context.Background(), server.Client(), page, account, password); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 {
		t.Fatal("follow-up action was executed")
	}
	if page.Fields.Get("user_account") != "" || page.Fields.Get("user_password") != "" {
		t.Fatal("Login mutated Page.Fields")
	}
}

func TestLoginFailureRedactsCredentials(t *testing.T) {
	account, password := "alice", "secret test&password"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"status": 0, "info": "failed " + account + " " + password + " " + r.Form.Get("data") + " current-sign", "data": map[string]any{"resultCode": 1001}})
	}))
	defer server.Close()
	err := Login(context.Background(), server.Client(), testPage(server.URL), account, password)
	var rejected *LoginError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected LoginError, got %T", err)
	}
	if rejected.ResultCode != "1001" {
		t.Errorf("wrong result code %q", rejected.ResultCode)
	}
	for _, secret := range []string{account, password, "current-sign"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("error exposed sensitive form data")
		}
	}
}

func TestLoginRejectsCrossOriginActionBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	page := testPage("http://portal.invalid")
	page.Action = server.URL + loginPath
	if err := Login(context.Background(), server.Client(), page, "alice", "secret"); err == nil {
		t.Fatal("cross-origin action accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("credentials sent to cross-origin target")
	}
}

func TestLoginNeverFollowsRedirect(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var targetRequests atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetRequests.Add(1) }))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
			defer source.Close()
			if err := Login(context.Background(), source.Client(), testPage(source.URL), "alice", "secret"); err == nil {
				t.Fatal("redirect treated as successful login")
			}
			if targetRequests.Load() != 0 {
				t.Fatal("login request followed cross-origin redirect")
			}
		})
	}
}

func TestLoginDoesNotExecuteSameOriginActionRedirect(t *testing.T) {
	var followups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == loginPath {
			http.Redirect(w, r, "/force-disconnect", http.StatusTemporaryRedirect)
			return
		}
		followups.Add(1)
	}))
	defer server.Close()
	if err := Login(context.Background(), server.Client(), testPage(server.URL), "alice", "secret"); err == nil {
		t.Fatal("same-origin redirect accepted")
	}
	if followups.Load() != 0 {
		t.Fatal("follow-up action was executed")
	}
}

func TestLoginRejectsMalformedAndOversizedReplies(t *testing.T) {
	for _, reply := range []string{`not json`, `{}`, `{"data":{"status":1}}`, `{"status":"maybe"}`, `{"status":null}`, strings.Repeat("x", maxReplyBytes+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, reply) }))
		err := Login(context.Background(), server.Client(), testPage(server.URL), "alice", "secret")
		server.Close()
		if err == nil {
			t.Fatal("invalid server reply treated as successful login")
		}
	}
}

func TestLoginFailureWithStringResultCodeDoesNotExecuteAction(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"status":0,"info":"confirmation required","data":{"resultCode":"124","resultData":"/force-disconnect"}}`)
	}))
	defer server.Close()
	err := Login(context.Background(), server.Client(), testPage(server.URL), "alice", "secret")
	var rejected *LoginError
	if !errors.As(err, &rejected) || rejected.ResultCode != "124" {
		t.Fatalf("expected code 124 rejection, got %v", err)
	}
	if requests.Load() != 1 {
		t.Fatal("failure resultData action was executed")
	}
}

func TestSameOriginUsesEffectivePort(t *testing.T) {
	base, _ := url.Parse("https://portal.example")
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"https://PORTAL.example:443/login", true},
		{"http://portal.example/login", false},
		{"https://portal.example:444/login", false},
		{"https://other.example/login", false},
	} {
		u, _ := url.Parse(tc.raw)
		if sameOrigin(base, u) != tc.want {
			t.Errorf("incorrect same-origin result for %s", tc.raw)
		}
	}
}
