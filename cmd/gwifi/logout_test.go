package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gwifi-in-one/internal/egress"
)

func TestParseLogoutArgsRequiresOneExplicitExit(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"-exit", ""},
		{"-exit", " "},
		{"-exit", "wired,wifi"},
		{"-exit", "wired", "-exit", "wifi"},
		{"-exit", "wifi", "-exit", "wifi"},
		{"-exits", "wired,wifi"},
		{"-exit", "wifi", "-exits="},
		{"-exit", "wifi", "-workers", "4"},
		{"-exit", "wifi", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, _, err := parseLogoutArgs(args); err == nil {
				t.Fatal("invalid logout selection accepted")
			}
		})
	}
	path, selected, err := parseLogoutArgs([]string{"-config", "chosen.json", "-exit", " wifi "})
	if err != nil || path != "chosen.json" || selected != "wifi" {
		t.Fatalf("unexpected arguments: %q %q %v", path, selected, err)
	}
}

func TestLogoutSelectionRejectedBeforeOpeningInterfaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"exits":[{"name":"wired","interface":"missing-test-interface"},{"name":"wifi","interface":"missing-test-interface-2"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "必须用 -exit"},
		{[]string{"-exit", " "}, "必须用 -exit"},
		{[]string{"-exit", "wired,wifi"}, "必须用 -exit"},
		{[]string{"-exit", "wired", "-exit", "wifi"}, "必须用 -exit"},
		{[]string{"-exits", "wired,wifi"}, "flag provided but not defined"},
		{[]string{"-exit", "wifi", "-exits="}, "flag provided but not defined"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			args := append([]string{"logout", "-config", path}, tc.args...)
			err := run(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want %q before reading interfaces", err, tc.want)
			}
		})
	}
}

func TestHandleLogoutRequiresVerifiedLoginPage(t *testing.T) {
	for _, tc := range []struct {
		name           string
		alreadyOffline bool
		prepareErr     error
		logoutErr      error
		verified       bool
		verifyErr      error
		calls          []string
		wantError      bool
		wantText       string
	}{
		{name: "already_offline", alreadyOffline: true, calls: []string{"prepare"}, wantText: "已处于未登录状态"},
		{name: "verified", verified: true, calls: []string{"prepare", "logout", "verify"}, wantText: "已退出账号，并确认此出口需要重新认证"},
		{name: "prepare_failed", prepareErr: errors.New("not a portal page"), calls: []string{"prepare"}, wantError: true, wantText: "未发送退出请求"},
		{name: "request_failed", logoutErr: errors.New("timeout"), calls: []string{"prepare", "logout"}, wantError: true, wantText: "无法确定会话是否已退出"},
		{name: "still_online", calls: []string{"prepare", "logout", "verify"}, wantError: true, wantText: "退出接口已接受请求，但尚未确认"},
		{name: "verification_failed", verifyErr: errors.New("network unavailable"), calls: []string{"prepare", "logout", "verify"}, wantError: true, wantText: "退出接口已接受请求，但尚未确认"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			var out bytes.Buffer
			err := handleLogout(context.Background(), "wifi", &out,
				func(context.Context) (bool, error) {
					calls = append(calls, "prepare")
					return tc.alreadyOffline, tc.prepareErr
				},
				func(context.Context) error { calls = append(calls, "logout"); return tc.logoutErr },
				func(context.Context) (bool, error) { calls = append(calls, "verify"); return tc.verified, tc.verifyErr },
			)
			if !reflect.DeepEqual(calls, tc.calls) {
				t.Fatalf("calls %v; want %v", calls, tc.calls)
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			text := out.String()
			if err != nil {
				text += err.Error()
				if strings.Contains(out.String(), "已退出账号") {
					t.Fatal("reported logout success without a verified login page")
				}
			}
			if !strings.Contains(text, tc.wantText) {
				t.Fatalf("text %q does not contain %q", text, tc.wantText)
			}
		})
	}
}

type logoutRoundTripper func(*http.Request) (*http.Response, error)

func (f logoutRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLogoutNeedsAuthenticationDoesNotMistakeFailureForOffline(t *testing.T) {
	const loginHTML = `<form id="loginForm"><input name="iv" value="0123456789abcdef"><input name="user_account"><input name="user_password"></form>`
	for _, tc := range []struct {
		name      string
		body      string
		redirect  bool
		failed    bool
		want      bool
		wantError bool
	}{
		{name: "online", body: "Microsoft Connect Test"},
		{name: "failed_request", failed: true, wantError: true},
		{name: "non_portal_redirect", redirect: true, body: "unrelated page", wantError: true},
		{name: "login_page_without_redirect", body: loginHTML},
		{name: "captive_portal_redirect", redirect: true, body: loginHTML, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: logoutRoundTripper(func(r *http.Request) (*http.Response, error) {
				if tc.failed {
					return nil, errors.New("network unavailable")
				}
				status, body := http.StatusOK, tc.body
				header := make(http.Header)
				if tc.redirect && r.URL.String() == discoveryURL {
					status, body = http.StatusFound, ""
					header.Set("Location", "http://portal.invalid/gportal/web/login")
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			got, err := logoutNeedsAuthentication(context.Background(), &egress.Bound{Client: client})
			if got != tc.want || (err != nil) != tc.wantError {
				t.Fatalf("got %v, %v; want %v, error=%v", got, err, tc.want, tc.wantError)
			}
		})
	}
}

func TestHandleLogoutCanceledBeforeRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	err := handleLogout(ctx, "wifi", &out,
		func(context.Context) (bool, error) { cancel(); return false, nil },
		func(context.Context) error { t.Fatal("logout called after cancellation"); return nil },
		func(context.Context) (bool, error) { t.Fatal("verify called after cancellation"); return false, nil },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v; want canceled", err)
	}
}
