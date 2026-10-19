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
	"strings"
	"sync/atomic"
	"testing"
)

func pendingRebind(t *testing.T, client *http.Client, page *Page) *LoginError {
	t.Helper()
	err := Login(context.Background(), client, page, "alice", "secret-password")
	var pending *LoginError
	if !errors.As(err, &pending) || !pending.CanRebind() {
		t.Fatalf("expected usable code-124 confirmation, got %v", err)
	}
	return pending
}

func writeRebind(w http.ResponseWriter, target any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "info": "设备更换次数仅剩4次", "data": map[string]any{"resultCode": "124", "resultData": target}})
}

func TestRebindUsesOriginalExitSessionAndFreshConnectionOnce(t *testing.T) {
	var calls, dials atomic.Int32
	var originURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == loginPath {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "session-token", Path: "/"})
			writeRebind(w, "/confirm?token=signed-token")
			return
		}
		calls.Add(1)
		if r.URL.Path != "/confirm" || r.URL.Query().Get("token") != "signed-token" || r.Method != http.MethodPost {
			t.Error("wrong confirmation target or method")
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 || !r.Close {
			t.Error("confirmation must be an empty POST on a non-reused connection")
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != "session-token" {
			t.Error("original session cookie was not retained")
		}
		if r.Header.Get("Origin") != originURL || r.Header.Get("Referer") != originURL+"/gportal/web/login" || r.UserAgent() != UserAgent || r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
			t.Error("confirmation lost portal headers")
		}
		fmt.Fprint(w, `{"status":1,"data":"/never-follow-this"}`)
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
	page := testPage(server.URL)
	pending := pendingRebind(t, client, page)
	if calls.Load() != 0 {
		t.Fatal("login executed the confirmation without consent")
	}
	if err := ConfirmRebind(context.Background(), client, page, pending); err != nil {
		t.Fatal(err)
	}
	copyPending := *pending
	if pending.CanRebind() || ConfirmRebind(context.Background(), client, page, &copyPending) == nil {
		t.Fatal("copied confirmation can be submitted twice")
	}
	if calls.Load() != 1 || dials.Load() != 2 || transport.DisableKeepAlives {
		t.Fatalf("expected one confirmation on fresh preserved dialer; confirmations=%d dials=%d", calls.Load(), dials.Load())
	}
}

func TestRebindRejectsUnusableActionURLs(t *testing.T) {
	for _, target := range []any{nil, "", " ", 42, map[string]string{"url": "/confirm"}, "http://other.invalid/action", "//other.invalid/action", "javascript:alert(1)", "http://user:pass@127.0.0.1/action", "/action#fragment", "/action\nheader"} {
		t.Run(fmt.Sprintf("%T-%v", target, target), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeRebind(w, target)
			}))
			defer server.Close()
			page := testPage(server.URL)
			var pending *LoginError
			if err := Login(context.Background(), server.Client(), page, "alice", "secret-password"); !errors.As(err, &pending) || pending.CanRebind() {
				t.Fatalf("unusable URL became confirmable: %v", err)
			}
			if ConfirmRebind(context.Background(), server.Client(), page, pending) == nil || calls.Load() != 1 {
				t.Fatal("unusable action executed")
			}
		})
	}
}

func TestRebindRejectsDifferentExitPageAndCanceledContext(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeRebind(w, "/confirm")
	}))
	defer server.Close()
	client := server.Client()
	page := testPage(server.URL)
	pending := pendingRebind(t, client, page)
	copyClient := *client
	copyPage := *page
	copyPage.URL += "?different-page=1"
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{
		ConfirmRebind(context.Background(), &copyClient, page, pending),
		ConfirmRebind(context.Background(), client, &copyPage, pending),
		ConfirmRebind(canceled, client, page, pending),
		ConfirmRebind(context.Background(), client, page, &LoginError{ResultCode: "124"}),
	} {
		if err == nil {
			t.Fatal("invalid confirmation attempt accepted")
		}
	}
	if calls.Load() != 1 || !pending.CanRebind() {
		t.Fatal("local rejection sent or consumed an action")
	}
}

func TestRebindOnlyAcceptsExplicitSuccessAndNeverFollowsActions(t *testing.T) {
	for _, reply := range []struct {
		body    string
		success bool
	}{
		{`{"status":1}`, true}, {`{"status":"1"}`, true},
		{`{"status":0,"info":"rejected"}`, false}, {`{"status":2}`, false}, {`{"status":-1}`, false},
		{`{"status":true}`, false}, {`{"status":null}`, false}, {`{}`, false}, {`invalid`, false}, {``, false},
		{strings.Repeat("x", maxReplyBytes+1), false},
		{`{"status":0,"data":{"resultCode":124,"resultData":"/again"}}`, false},
	} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == loginPath {
				writeRebind(w, "/confirm")
				return
			}
			calls.Add(1)
			fmt.Fprint(w, reply.body)
		}))
		page := testPage(server.URL)
		pending := pendingRebind(t, server.Client(), page)
		err := ConfirmRebind(context.Background(), server.Client(), page, pending)
		if (err == nil) != reply.success {
			t.Errorf("wrong success classification for response length %d", len(reply.body))
		}
		if pending.CanRebind() || calls.Load() != 1 {
			t.Error("confirmation was replayed or left reusable")
		}
		server.Close()
	}
}

func TestRebindRedirectAndDroppedResponseDoNotRetry(t *testing.T) {
	for _, mode := range []string{"same-origin-redirect", "cross-origin-redirect", "dropped-response"} {
		t.Run(mode, func(t *testing.T) {
			var calls, followups atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followups.Add(1) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == loginPath {
					writeRebind(w, "/confirm")
					return
				}
				calls.Add(1)
				switch mode {
				case "same-origin-redirect":
					http.Redirect(w, r, "/must-not-run", http.StatusTemporaryRedirect)
				case "cross-origin-redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				default:
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
				}
			}))
			defer server.Close()
			page := testPage(server.URL)
			pending := pendingRebind(t, server.Client(), page)
			if err := ConfirmRebind(context.Background(), server.Client(), page, pending); err == nil {
				t.Fatal("unknown outcome reported successful")
			}
			if err := ConfirmRebind(context.Background(), server.Client(), page, pending); err == nil {
				t.Fatal("uncertain action can be replayed")
			}
			if calls.Load() != 1 || followups.Load() != 0 {
				t.Fatal("action or redirect replayed")
			}
		})
	}
}

func TestRebindRedactsActionAndCredentialsFromFailures(t *testing.T) {
	const token = "sensitive-action-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == loginPath {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "info": "confirm /confirm?token=" + token, "data": map[string]any{"resultCode": 124, "resultData": "/confirm?token=" + token}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "info": "failed alice secret-password current-sign " + token + " /confirm?token=" + token})
	}))
	defer server.Close()
	page := testPage(server.URL)
	pending := pendingRebind(t, server.Client(), page)
	err := ConfirmRebind(context.Background(), server.Client(), page, pending)
	if err == nil {
		t.Fatal("failed confirmation accepted")
	}
	for _, secret := range []string{token, "alice", "secret-password", "current-sign"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(pending.Error(), secret) {
			t.Fatal("confirmation error exposed private data")
		}
	}
}
