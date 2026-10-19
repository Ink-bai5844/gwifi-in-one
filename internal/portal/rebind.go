package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// Copies of LoginError share this state, so a confirmation can only be sent once.
// The signed target and credentials used for redaction must never be logged.
type rebindAction struct {
	target    string
	pageURL   string
	client    *http.Client
	secrets   []string
	attempted atomic.Bool
}

// CanRebind reports whether this rejection has an unused, same-origin code-124
// action. It does not grant consent; the caller must first ask the user.
func (e *LoginError) CanRebind() bool {
	return e != nil && e.ResultCode == "124" && e.rebind != nil && !e.rebind.attempted.Load()
}

func rebindEndpoint(base *url.URL, raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("portal did not provide a rebind confirmation URL")
	}
	relative, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid portal rebind confirmation URL")
	}
	target := base.ResolveReference(relative)
	if _, err := endpoint(target.String()); err != nil || !sameOrigin(base, target) {
		return nil, errors.New("portal rebind confirmation must stay on the same origin")
	}
	return target, nil
}

func appendActionSecrets(secrets []string, raw string) []string {
	if raw == "" {
		return secrets
	}
	secrets = append(secrets, raw)
	if u, err := url.Parse(raw); err == nil {
		secrets = append(secrets, u.RawQuery)
		for key, values := range u.Query() {
			name := strings.ToLower(key)
			for _, value := range values {
				if len(value) >= 8 || strings.Contains(name, "token") || strings.Contains(name, "sign") || strings.Contains(name, "secret") || strings.Contains(name, "password") {
					secrets = append(secrets, value)
				}
			}
		}
	}
	return secrets
}

// ConfirmRebind sends the empty POST used by the official page's code-124
// confirmation. Call only after explicit user consent. It uses the original
// exit and cookie jar, follows no redirects, and never retries the action.
// A nil result confirms the action response, not Internet connectivity.
func ConfirmRebind(ctx context.Context, client *http.Client, page *Page, pending *LoginError) error {
	if pending == nil || !pending.CanRebind() {
		return errors.New("没有可用的换绑确认，或本次确认已经提交；请先检查联网状态")
	}
	if client == nil {
		client = http.DefaultClient
	}
	action := pending.rebind
	if page == nil || page.URL != action.pageURL || client != action.client {
		return errors.New("换绑必须使用本次登录的原出口、页面和会话")
	}
	base, err := endpoint(page.URL)
	if err != nil {
		return err
	}
	target, err := rebindEndpoint(base, action.target)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	confirmClient := guardedClient(client, base, true)
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	standard, ok := transport.(*http.Transport)
	if !ok {
		return errors.New("换绑需要可禁用连接复用的 HTTP transport")
	}
	fresh := standard.Clone()
	fresh.DisableKeepAlives = true
	// A fresh HTTP/1 connection avoids net/http's retry of an empty POST on a
	// stale pooled connection, while keeping the selected exit's DialContext.
	fresh.Protocols = new(http.Protocols)
	fresh.Protocols.SetHTTP1(true)
	confirmClient.Transport = fresh
	defer fresh.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	if err != nil {
		return errors.New("无法创建换绑确认请求")
	}
	req.Close = true
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin(base))
	req.Header.Set("Referer", base.String())
	if !action.attempted.CompareAndSwap(false, true) {
		return errors.New("本次换绑确认已经提交；请先检查联网状态")
	}
	resp, err := confirmClient.Do(req)
	if err != nil {
		return fmt.Errorf("换绑结果未确认，请先检查联网状态，勿重复提交: %w", requestError(ctx, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("换绑返回 HTTP %d；未执行重定向，结果未确认，请先检查联网状态", resp.StatusCode)
	}
	reply, err := readBounded(resp.Body, maxReplyBytes)
	if err != nil {
		return errors.New("无法读取换绑响应，结果未确认，请先检查联网状态")
	}
	var result struct {
		Status json.RawMessage `json:"status"`
		Info   string          `json:"info"`
	}
	if json.Unmarshal(reply, &result) != nil {
		return errors.New("换绑响应格式无法识别，结果未确认，请先检查联网状态")
	}
	status := strings.TrimSpace(string(result.Status))
	if status == "1" || status == `"1"` {
		return nil
	}
	info := safeMessage(result.Info, action.secrets)
	if info != "" {
		return fmt.Errorf("换绑未获成功确认，请先检查联网状态: %s", info)
	}
	return errors.New("换绑未获成功确认，请先检查联网状态，勿重复提交")
}
