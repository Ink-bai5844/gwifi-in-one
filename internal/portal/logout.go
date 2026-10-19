package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode"

	"golang.org/x/net/html"
)

const logoutPath = "/gportal/Web/logoutAction"

// LogoutSession is current terminal context read through one network exit.
// Its signed fields are private, are never persisted, and must not be logged.
// Copies share the attempt state so a logout action cannot be replayed.
type LogoutSession struct {
	state *logoutState
}

type logoutState struct {
	pageURL   string
	target    string
	fields    url.Values
	client    *http.Client
	secrets   []string
	attempted atomic.Bool
}

// TerminalIP is empty because the observed online page only exposes a session
// identifier. It must not invent a terminal IP from an unrelated login form.
func (s *LogoutSession) TerminalIP() string {
	return ""
}

// PrepareLogout only reads the portal; it does not log an account out.
// The portal origin root redirects to the current terminal's online page,
// whereas /gportal/web/login can keep returning a login form while online.
// The observed online formData contains only the hidden session identifier si;
// its offline button sends that form as an unencrypted POST to logoutPath.
func PrepareLogout(ctx context.Context, client *http.Client, rawURL string) (*LogoutSession, error) {
	if client == nil {
		client = http.DefaultClient
	}
	configured, err := endpoint(rawURL)
	if err != nil {
		return nil, err
	}
	entry := configured.ResolveReference(&url.URL{Path: "/"})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.String(), nil)
	if err != nil {
		return nil, errors.New("cannot create portal logout inspection request")
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := guardedClient(client, entry, false).Do(req)
	if err != nil {
		return nil, requestError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("portal logout inspection returned HTTP %d", resp.StatusCode)
	}
	body, err := readBounded(resp.Body, maxPageBytes)
	if err != nil {
		return nil, err
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("cannot parse portal online page")
	}
	var form *html.Node
	var forms int
	walk(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "form" && attr(n, "id") == "formData" {
			form = n
			forms++
		}
	})
	if forms != 1 {
		return nil, errors.New("未能读取唯一的校园网在线退出表单；没有提交退出请求")
	}
	var sessionID string
	var sessionFields, logoutButtons int
	invalidSession := false
	walk(form, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if attr(n, "name") == "si" {
			sessionFields++
			if n.Data != "input" || !strings.EqualFold(attr(n, "type"), "hidden") || disabled(n, form) || (attr(n, "form") != "" && attr(n, "form") != "formData") {
				invalidSession = true
			}
			sessionID = attr(n, "value")
		}
		if n.Data == "button" && !disabled(n, form) && logoutButtonCall.MatchString(attr(n, "onclick")) {
			logoutButtons++
		}
	})
	if sessionFields != 1 || invalidSession || strings.TrimSpace(sessionID) == "" || strings.IndexFunc(sessionID, unicode.IsControl) >= 0 {
		return nil, errors.New("在线退出表单缺少有效且唯一的会话标识；没有提交退出请求")
	}
	if logoutButtons != 1 {
		return nil, errors.New("在线页面没有匹配该表单的唯一退出按钮；没有提交退出请求")
	}
	base := resp.Request.URL
	target := base.ResolveReference(&url.URL{Path: logoutPath})
	if !sameOrigin(entry, base) || !sameOrigin(base, target) {
		return nil, errors.New("portal logout action must stay on the same origin")
	}
	fields := url.Values{"si": {sessionID}}
	secrets := appendActionSecrets([]string{sessionID}, rawURL)
	secrets = appendActionSecrets(secrets, base.String())
	secrets = appendActionSecrets(secrets, target.String())
	return &LogoutSession{state: &logoutState{
		pageURL: base.String(), target: target.String(),
		fields: fields, client: client, secrets: secrets,
	}}, nil
}

var logoutButtonCall = regexp.MustCompile(`^\s*(?:javascript:\s*)?offline\s*\(\s*this\s*,\s*(?:'formData'|"formData")\s*\)\s*;?\s*$`)

// Logout submits the selected terminal's logout exactly once. A nil error
// means the interface explicitly reported status 1, not that a separate
// connectivity check has confirmed the terminal is offline.
func Logout(ctx context.Context, client *http.Client, session *LogoutSession) error {
	if session == nil || session.state == nil || session.state.attempted.Load() {
		return errors.New("没有可用的退出会话，或本次退出已经提交；请先检查联网状态")
	}
	if client == nil {
		client = http.DefaultClient
	}
	action := session.state
	if client != action.client {
		return errors.New("退出账号必须使用取得当前会话的原出口")
	}
	base, err := endpoint(action.pageURL)
	if err != nil {
		return err
	}
	target, err := endpoint(action.target)
	if err != nil || !sameOrigin(base, target) || target.Path != logoutPath || target.RawQuery != "" {
		return errors.New("invalid portal logout action")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	standard, ok := transport.(*http.Transport)
	if !ok {
		return errors.New("退出账号需要可禁用连接复用的 HTTP transport")
	}
	fresh := standard.Clone()
	fresh.DisableKeepAlives = true
	fresh.Protocols = new(http.Protocols)
	fresh.Protocols.SetHTTP1(true)
	defer fresh.CloseIdleConnections()
	logoutClient := guardedClient(client, base, true)
	logoutClient.Transport = fresh
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader(serialize(action.fields)))
	if err != nil {
		return errors.New("无法创建退出账号请求")
	}
	req.Close = true
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin(base))
	req.Header.Set("Referer", base.String())
	if !action.attempted.CompareAndSwap(false, true) {
		return errors.New("本次退出已经提交；请先检查联网状态")
	}
	resp, err := logoutClient.Do(req)
	if err != nil {
		return fmt.Errorf("退出结果未确认，请先检查联网状态，勿重复提交: %w", requestError(ctx, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("退出返回 HTTP %d；未执行重定向，结果未确认，请先检查联网状态", resp.StatusCode)
	}
	reply, err := readBounded(resp.Body, maxReplyBytes)
	if err != nil {
		return errors.New("无法读取退出响应，结果未确认，请先检查联网状态")
	}
	var result struct {
		Status json.RawMessage `json:"status"`
		Info   string          `json:"info"`
	}
	if json.Unmarshal(reply, &result) != nil {
		return errors.New("退出响应格式无法识别，结果未确认，请先检查联网状态")
	}
	status := strings.TrimSpace(string(result.Status))
	if status == "1" || status == `"1"` {
		return nil
	}
	if info := safeMessage(result.Info, action.secrets); info != "" {
		return fmt.Errorf("退出未获成功确认，请先检查联网状态: %s", info)
	}
	return errors.New("退出未获成功确认，请先检查联网状态，勿重复提交")
}
