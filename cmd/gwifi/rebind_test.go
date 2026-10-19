package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gwifi-in-one/internal/portal"
)

func pendingRebind(t *testing.T, reply string) (*httptest.Server, *portal.Page, *portal.LoginError, *atomic.Int32) {
	t.Helper()
	var confirms atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gportal/Web/loginAction":
			if r.Method != http.MethodPost {
				t.Errorf("login method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": 0,
				"info":   "系统检测到您当前可更换上网设备次数仅剩4次,是否重新绑定至当前设备？",
				"data": map[string]any{
					"resultCode": 124,
					"resultData": "http://" + r.Host + "/confirm?token=test-confirm-token",
				},
			})
		case "/confirm":
			confirms.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("confirmation method = %s", r.Method)
			}
			body, _ := io.ReadAll(r.Body)
			if len(body) != 0 {
				t.Error("confirmation must not resend credentials")
			}
			fmt.Fprint(w, reply)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	page := &portal.Page{
		URL:    server.URL + "/gportal/web/login",
		Action: server.URL + "/gportal/Web/loginAction",
		Fields: url.Values{"iv": {"0123456789abcdef"}},
		IV:     "0123456789abcdef",
	}
	err := portal.Login(context.Background(), server.Client(), page, "test-account", "test-password")
	var pending *portal.LoginError
	if !errors.As(err, &pending) || !pending.CanRebind() {
		t.Fatalf("expected pending confirmation, got %v", err)
	}
	return server, page, pending, &confirms
}

func TestHandleRebindConfirmOnce(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\r\n", "invalid\ny\n", "yes\nY\n"} {
		t.Run(strings.ReplaceAll(answer, "\n", "_"), func(t *testing.T) {
			server, page, pending, confirms := pendingRebind(t, `{"status":1,"info":"ok"}`)
			var output bytes.Buffer
			checks := 0
			err := handleRebind(context.Background(), server.Client(), page, pending, strings.NewReader(answer), &output, func(context.Context) (bool, error) {
				checks++
				return true, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if confirms.Load() != 1 || checks != 1 {
				t.Fatalf("confirmation requests = %d, connectivity checks = %d", confirms.Load(), checks)
			}
			if !strings.Contains(output.String(), pending.Info) || !strings.Contains(output.String(), "消耗剩余换绑次数") || !strings.Contains(output.String(), "连通性检测通过") {
				t.Fatalf("missing warning or outcome: %s", output.String())
			}
			if strings.Contains(output.String(), "test-confirm-token") {
				t.Fatal("confirmation token was printed")
			}
		})
	}
}

func TestHandleRebindCancelDoesNotRequest(t *testing.T) {
	for _, answer := range []string{"n\n", "N\r\n", "\n", "", "y", "invalid\n", "\x03\n"} {
		t.Run(fmt.Sprintf("%q", answer), func(t *testing.T) {
			server, page, pending, confirms := pendingRebind(t, `{"status":1}`)
			var output bytes.Buffer
			err := handleRebind(context.Background(), server.Client(), page, pending, strings.NewReader(answer), &output, func(context.Context) (bool, error) {
				t.Error("connectivity check must not run after cancellation")
				return true, nil
			})
			if !errors.Is(err, errRebindCanceled) || confirms.Load() != 0 {
				t.Fatalf("err = %v, confirmation requests = %d", err, confirms.Load())
			}
			if strings.Contains(output.String(), "连通性检测通过") {
				t.Fatal("cancellation was reported as success")
			}
		})
	}
}

func TestHandleRebindFailureDoesNotRetry(t *testing.T) {
	server, page, pending, confirms := pendingRebind(t, `{"status":0,"info":"failure"}`)
	err := handleRebind(context.Background(), server.Client(), page, pending, strings.NewReader("y\ny\n"), io.Discard, func(context.Context) (bool, error) {
		t.Error("connectivity callback should not run after a failed confirmation response")
		return true, nil
	})
	if err == nil || !strings.Contains(err.Error(), "无法确定是否已经换绑") || !strings.Contains(err.Error(), "check") || confirms.Load() != 1 {
		t.Fatalf("err = %v, confirmation requests = %d", err, confirms.Load())
	}
}

func TestHandleRebindSuccessRequiresConnectivity(t *testing.T) {
	for _, checkErr := range []error{nil, errors.New("test network failure")} {
		server, page, pending, confirms := pendingRebind(t, `{"status":1}`)
		var output bytes.Buffer
		err := handleRebind(context.Background(), server.Client(), page, pending, strings.NewReader("y\n"), &output, func(context.Context) (bool, error) {
			return false, checkErr
		})
		if err == nil || !strings.Contains(err.Error(), "换绑请求已获成功响应") || !strings.Contains(err.Error(), "尚未确认此出口联网") || confirms.Load() != 1 {
			t.Fatalf("err = %v, confirmation requests = %d", err, confirms.Load())
		}
		if strings.Contains(output.String(), "连通性检测通过") {
			t.Fatal("offline exit was reported as online")
		}
	}
}

func TestHandleRebindCanceledContext(t *testing.T) {
	server, page, pending, confirms := pendingRebind(t, `{"status":1}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := handleRebind(ctx, server.Client(), page, pending, strings.NewReader("y\n"), io.Discard, func(context.Context) (bool, error) {
		t.Error("connectivity callback must not run after cancellation")
		return true, nil
	})
	if !errors.Is(err, errRebindCanceled) || confirms.Load() != 0 {
		t.Fatalf("err = %v, confirmation requests = %d", err, confirms.Load())
	}
}

func TestAskRebindConfirmationInterruptsBlockedRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		finished <- askRebindConfirmation(ctx, signaledReader{reader: reader, started: started}, io.Discard)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation did not start reading input")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, errRebindCanceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation waited for terminal input")
	}
}

type signaledReader struct {
	reader  io.Reader
	started chan struct{}
}

func (r signaledReader) Read(p []byte) (int, error) {
	close(r.started)
	return r.reader.Read(p)
}

type failedConfirmationReader struct{}

func (failedConfirmationReader) Read([]byte) (int, error) {
	return 0, errors.New("test input failure")
}

func TestAskRebindConfirmationReadFailureCancels(t *testing.T) {
	if err := askRebindConfirmation(context.Background(), failedConfirmationReader{}, io.Discard); !errors.Is(err, errRebindCanceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}
