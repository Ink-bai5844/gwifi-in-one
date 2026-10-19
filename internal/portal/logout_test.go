package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const logoutHTML = `<form id="formData">
<input name="si" type="hidden" value="signed &amp; private!">
<input name="user_account" value="private-account">
<input name="user_password" type="password" value="private-password">
<input name="user_vcode" value="123456">
<input name="unknown" value="not-allowed">
<button type="button" onclick="javascript:offline(this,'formData');">下线</button>
</form>`

func prepareLogout(t *testing.T, client *http.Client, serverURL string) *LogoutSession {
	t.Helper()
	session, err := PrepareLogout(context.Background(), client, serverURL+"/gportal/web/login?token=private-query-token")
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestLogoutPreservesExitCookieAndHeadersWithoutCredentials(t *testing.T) {
	var gets, posts, dials atomic.Int32
	var originURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/gportal/web/index" {
			gets.Add(1)
			if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.UserAgent() != UserAgent {
				t.Error("preparation must be read only")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "original-cookie", Path: "/"})
			if r.URL.Path == "/" {
				http.Redirect(w, r, "/gportal/web/index", http.StatusFound)
				return
			}
			fmt.Fprint(w, logoutHTML)
			return
		}
		posts.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != logoutPath || r.URL.RawQuery != "" {
			t.Error("wrong logout endpoint or method")
		}
		if !r.Close || r.ProtoMajor != 1 {
			t.Error("logout must use a fresh HTTP/1 connection")
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != "original-cookie" {
			t.Error("original session cookie was not retained")
		}
		if r.UserAgent() != UserAgent || r.Header.Get("Origin") != originURL || r.Header.Get("Referer") != originURL+"/gportal/web/index" || r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded; charset=UTF-8" {
			t.Error("logout lost portal headers")
		}
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil || len(form) != 1 || form.Get("si") != "signed & private!" {
			t.Error("logout did not serialize exactly the online session identifier")
		}
		for _, key := range []string{"user_account", "user_password", "user_vcode", "unknown", "data"} {
			if _, exists := form[key]; exists {
				t.Errorf("logout included disallowed field %s", key)
			}
		}
		if string(body) != "si=signed%20%26%20private!" {
			t.Error("logout escaping differs from jQuery serialize")
		}
		fmt.Fprint(w, `{"status":1,"data":"http://1.2.3.4"}`)
	}))
	defer server.Close()
	originURL = server.URL
	client := server.Client()
	client.Jar, _ = cookiejar.New(nil)
	transport := client.Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	client.Transport = transport
	defer client.CloseIdleConnections()
	session := prepareLogout(t, client, server.URL)
	if gets.Load() != 2 || posts.Load() != 0 || session.TerminalIP() != "" {
		t.Fatal("preparation did not remain read only or invented a terminal IP")
	}
	if err := Logout(context.Background(), client, session); err != nil {
		t.Fatal(err)
	}
	copySession := *session
	if Logout(context.Background(), client, &copySession) == nil {
		t.Fatal("copied logout session allowed a replay")
	}
	if posts.Load() != 1 || dials.Load() != 2 || transport.DisableKeepAlives {
		t.Fatalf("logout did not preserve a fresh original dialer: posts=%d dials=%d", posts.Load(), dials.Load())
	}
}

func TestPrepareLogoutRejectsMissingDuplicateOrInvalidContext(t *testing.T) {
	cases := map[string]string{
		"plain page":            `<html>登录成功</html>`,
		"login form":            strings.Replace(logoutHTML, `id="formData"`, `id="loginForm"`, 1),
		"duplicate form":        logoutHTML + logoutHTML,
		"missing si":            strings.Replace(logoutHTML, `name="si"`, `name="other"`, 1),
		"empty si":              strings.Replace(logoutHTML, "signed &amp; private!", "", 1),
		"blank si":              strings.Replace(logoutHTML, "signed &amp; private!", "   ", 1),
		"control si":            strings.Replace(logoutHTML, "signed &amp; private!", "value&#10;other", 1),
		"not hidden":            strings.Replace(logoutHTML, `type="hidden"`, `type="text"`, 1),
		"disabled si":           strings.Replace(logoutHTML, `name="si"`, `name="si" disabled`, 1),
		"other form si":         strings.Replace(logoutHTML, `name="si"`, `name="si" form="otherForm"`, 1),
		"duplicate si":          strings.Replace(logoutHTML, "</form>", `<input name="si" type="hidden" value="second"></form>`, 1),
		"duplicate disabled si": strings.Replace(logoutHTML, "</form>", `<input name="si" type="hidden" disabled value="second"></form>`, 1),
		"missing button":        strings.Replace(logoutHTML, "offline(this,'formData')", "other(this,'formData')", 1),
		"wrong button target":   strings.Replace(logoutHTML, "offline(this,'formData')", "offline(this,'loginForm')", 1),
		"disabled button":       strings.Replace(logoutHTML, "<button", "<button disabled", 1),
		"duplicate button":      strings.Replace(logoutHTML, "</form>", `<button onclick="offline(this,'formData')"></button></form>`, 1),
		"button outside form":   strings.Replace(logoutHTML, `<button`, `</form><button`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var actions atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path == logoutPath {
					actions.Add(1)
				}
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			if _, err := PrepareLogout(context.Background(), server.Client(), server.URL+"/gportal/web/login"); err == nil {
				t.Fatal("invalid logout context accepted")
			}
			if actions.Load() != 0 {
				t.Fatal("preparation executed a logout action")
			}
		})
	}
}

func TestPrepareLogoutDoesNotFollowCrossOriginRedirect(t *testing.T) {
	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		fmt.Fprint(w, logoutHTML)
	}))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/?token=private-session", http.StatusFound)
	}))
	defer server.Close()
	_, err := PrepareLogout(context.Background(), server.Client(), server.URL+"/gportal/web/login")
	if err == nil || foreignRequests.Load() != 0 || strings.Contains(err.Error(), "private-session") {
		t.Fatal("logout preparation followed a foreign origin or leaked its token")
	}
}

func TestPrepareLogoutPreservesOpaqueSessionWhitespace(t *testing.T) {
	const sessionID = "  opaque + & ! session  "
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != logoutPath {
			fmt.Fprint(w, strings.Replace(logoutHTML, "signed &amp; private!", "  opaque + &amp; ! session  ", 1))
			return
		}
		if err := r.ParseForm(); err != nil || len(r.PostForm) != 1 || r.PostForm.Get("si") != sessionID {
			t.Error("session identifier was trimmed, transformed, or mixed with other fields")
		}
		fmt.Fprint(w, `{"status":1}`)
	}))
	defer server.Close()
	client := server.Client()
	if err := Logout(context.Background(), client, prepareLogout(t, client, server.URL)); err != nil {
		t.Fatal(err)
	}
}

func TestLogoutRejectsDifferentClientAndLocalCancellationWithoutConsumingSession(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == logoutPath {
			posts.Add(1)
			fmt.Fprint(w, `{"status":1}`)
			return
		}
		fmt.Fprint(w, logoutHTML)
	}))
	defer server.Close()
	client := server.Client()
	session := prepareLogout(t, client, server.URL)
	copyClient := *client
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{
		Logout(context.Background(), &copyClient, session),
		Logout(ctx, client, session),
		Logout(context.Background(), client, nil),
		Logout(context.Background(), client, &LogoutSession{}),
	} {
		if err == nil {
			t.Fatal("invalid logout attempt was accepted")
		}
	}
	if posts.Load() != 0 {
		t.Fatal("local rejection submitted a logout")
	}
	if err := Logout(context.Background(), client, session); err != nil {
		t.Fatal("local rejection consumed the logout session")
	}
	if (*LogoutSession)(nil).TerminalIP() != "" || (&LogoutSession{}).TerminalIP() != "" {
		t.Fatal("empty session terminal IP must be empty")
	}
}

func TestLogoutOnlyAcceptsExplicitSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		success bool
	}{
		{"numeric success", `{"status":1}`, true},
		{"string success", `{"status":"1"}`, true},
		{"failure", `{"status":0,"info":"rejected"}`, false},
		{"data 133", `{"status":0,"data":"133"}`, false},
		{"nested 133", `{"status":0,"data":{"resultCode":133}}`, false},
		{"status 133", `{"status":133}`, false},
		{"status 2", `{"status":2}`, false},
		{"negative", `{"status":-1}`, false},
		{"float", `{"status":1.0}`, false},
		{"boolean", `{"status":true}`, false},
		{"null", `{"status":null}`, false},
		{"missing", `{}`, false},
		{"invalid JSON", `not JSON`, false},
		{"empty", ``, false},
		{"login form", logoutHTML, false},
		{"too large", strings.Repeat("x", maxReplyBytes+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == logoutPath {
					posts.Add(1)
					fmt.Fprint(w, tc.body)
					return
				}
				fmt.Fprint(w, logoutHTML)
			}))
			defer server.Close()
			client := server.Client()
			session := prepareLogout(t, client, server.URL)
			if err := Logout(context.Background(), client, session); (err == nil) != tc.success {
				t.Errorf("wrong success classification: %v", err)
			}
			if Logout(context.Background(), client, session) == nil || posts.Load() != 1 {
				t.Error("attempt remained reusable or was automatically retried")
			}
		})
	}
}

func TestLogoutDoesNotFollowRedirectsOrRetryDroppedResponses(t *testing.T) {
	for _, mode := range []string{"same-origin", "cross-origin", "dropped-response", "HTTP error"} {
		t.Run(mode, func(t *testing.T) {
			var posts, followups atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				followups.Add(1)
			}))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/" {
					fmt.Fprint(w, logoutHTML)
					return
				}
				if r.URL.Path != logoutPath {
					followups.Add(1)
					return
				}
				posts.Add(1)
				switch mode {
				case "same-origin":
					http.Redirect(w, r, "/followup?token=never-leak-this", http.StatusTemporaryRedirect)
				case "cross-origin":
					http.Redirect(w, r, target.URL+"/followup", http.StatusFound)
				case "dropped-response":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				case "HTTP error":
					w.WriteHeader(http.StatusBadGateway)
					fmt.Fprint(w, `{"status":1}`)
				}
			}))
			defer server.Close()
			client := server.Client()
			session := prepareLogout(t, client, server.URL)
			if err := Logout(context.Background(), client, session); err == nil || strings.Contains(err.Error(), "never-leak-this") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("failed action accepted or secret URL leaked")
			}
			if Logout(context.Background(), client, session) == nil || posts.Load() != 1 || followups.Load() != 0 {
				t.Fatal("logout was retried or followed a redirect")
			}
		})
	}
}

func TestLogoutCancellationDuringRequestIsNotRetried(t *testing.T) {
	entered := make(chan struct{})
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != logoutPath {
			fmt.Fprint(w, logoutHTML)
			return
		}
		posts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	client := server.Client()
	session := prepareLogout(t, client, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- Logout(ctx, client, session) }()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation lost its cause: %v", err)
	}
	if Logout(context.Background(), client, session) == nil || posts.Load() != 1 {
		t.Fatal("in-flight cancellation allowed a logout retry")
	}
}

func TestLogoutConcurrentSessionCopiesSubmitOnce(t *testing.T) {
	var posts, successes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == logoutPath {
			posts.Add(1)
			fmt.Fprint(w, `{"status":1}`)
			return
		}
		fmt.Fprint(w, logoutHTML)
	}))
	defer server.Close()
	client := server.Client()
	session := prepareLogout(t, client, server.URL)
	var tasks sync.WaitGroup
	for range 16 {
		copySession := *session
		tasks.Add(1)
		go func() {
			defer tasks.Done()
			if Logout(context.Background(), client, &copySession) == nil {
				successes.Add(1)
			}
		}()
	}
	tasks.Wait()
	if posts.Load() != 1 || successes.Load() != 1 {
		t.Fatalf("parallel logout copies sent %d actions with %d successes", posts.Load(), successes.Load())
	}
}

func TestLogoutRedactsSessionValuesAndURLs(t *testing.T) {
	var sourceURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != logoutPath {
			fmt.Fprint(w, logoutHTML)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 0,
			"info": sourceURL + "/gportal/web/login?token=private-query-token " +
				"signed & private! signed%20%26%20private! signed+%26+private%21\nerror",
		})
	}))
	defer server.Close()
	sourceURL = server.URL
	client := server.Client()
	session := prepareLogout(t, client, server.URL)
	err := Logout(context.Background(), client, session)
	if err == nil || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatal("expected redacted failure")
	}
	for _, secret := range []string{sourceURL, "private-query-token", "signed", "\n"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("logout error leaked sensitive data or a control character")
		}
	}
}
