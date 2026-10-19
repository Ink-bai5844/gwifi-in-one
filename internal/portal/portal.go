// Package portal implements the observed Gwifi web login form protocol.
// Device rebinding requires a separate, explicitly confirmed call.
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
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

const (
	maxPageBytes  = 2 << 20
	maxReplyBytes = 256 << 10
	loginPath     = "/gportal/Web/loginAction"
	protocolKey   = "1234567887654321"
)

// UserAgent selects the real Windows desktop portal variant. This deployment
// returns HTTP 403 to Go's default User-Agent, including for its public form.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Page contains current fields obtained through the selected network exit.
// Fields may contain session values and must not be logged or persisted.
type Page struct {
	URL    string
	Action string
	Fields url.Values
	IV     string
}

// LoginError is a rejected login, with bounded, credential-redacted server text.
type LoginError struct {
	Info       string
	ResultCode string
	rebind     *rebindAction
}

func (e *LoginError) Error() string {
	message := "portal rejected login"
	if e.ResultCode != "" {
		message += " (code " + e.ResultCode + ")"
	}
	if e.Info != "" {
		message += ": " + e.Info
	}
	return message
}

// Inspect performs only a GET and reads the current login form. Redirects must
// stay on the original origin, including scheme and effective port.
func Inspect(ctx context.Context, client *http.Client, rawURL string) (*Page, error) {
	u, err := endpoint(rawURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("cannot create portal inspection request")
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := guardedClient(client, u, false).Do(req)
	if err != nil {
		return nil, requestError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("portal inspection returned HTTP %d", resp.StatusCode)
	}
	body, err := readBounded(resp.Body, maxPageBytes)
	if err != nil {
		return nil, err
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("cannot parse portal HTML")
	}
	var form *html.Node
	var forms int
	walk(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "form" && attr(n, "id") == "loginForm" {
			form = n
			forms++
		}
	})
	if forms != 1 {
		return nil, errors.New("expected exactly one Gwifi loginForm")
	}
	fields := formFields(form)
	if _, ok := fields["user_account"]; !ok {
		return nil, errors.New("portal form lacks user_account")
	}
	if _, ok := fields["user_password"]; !ok {
		return nil, errors.New("portal form lacks user_password")
	}
	iv := fields.Get("iv")
	if len([]byte(iv)) != aes.BlockSize || len(fields["iv"]) != 1 {
		return nil, errors.New("portal form IV must be exactly 16 bytes")
	}
	base := resp.Request.URL
	if requestedIP := base.Query().Get("wlanuserip"); requestedIP != "" && fields.Get("sta_ip") != requestedIP {
		return nil, errors.New("portal URL wlanuserip and form sta_ip do not match; obtain a fresh portal URL through this exit")
	}
	action := strings.TrimSpace(attr(form, "action"))
	if action == "" {
		action = loginPath
	}
	actionURL, err := url.Parse(action)
	if err != nil {
		return nil, errors.New("invalid portal form action")
	}
	actionURL = base.ResolveReference(actionURL)
	if _, err := endpoint(actionURL.String()); err != nil || !sameOrigin(base, actionURL) {
		return nil, errors.New("portal form action must stay on the same origin")
	}
	return &Page{URL: base.String(), Action: actionURL.String(), Fields: fields, IV: iv}, nil
}

// Login submits one explicit account login. It never follows redirects or any
// resultData action, including device eviction and password-change actions.
func Login(ctx context.Context, client *http.Client, page *Page, account, password string) error {
	if page == nil {
		return errors.New("portal page is required")
	}
	base, err := endpoint(page.URL)
	if err != nil {
		return err
	}
	action, err := endpoint(page.Action)
	if err != nil {
		return err
	}
	if !sameOrigin(base, action) {
		return errors.New("portal login action must stay on the same origin")
	}
	if strings.TrimSpace(account) == "" || password == "" {
		return errors.New("account and password are required")
	}
	if len([]byte(page.IV)) != aes.BlockSize || page.Fields.Get("iv") != page.IV {
		return errors.New("invalid portal form IV")
	}
	fields := make(url.Values, len(page.Fields))
	for key, values := range page.Fields {
		fields[key] = append([]string(nil), values...)
	}
	fields.Set("user_account", account)
	fields.Set("user_password", password)
	encoded, err := encryptForm(serialize(fields), page.IV)
	if err != nil {
		return err
	}
	body := url.Values{"data": {encoded}, "iv": {page.IV}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action.String(), strings.NewReader(body))
	if err != nil {
		return errors.New("cannot create portal login request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin(base))
	req.Header.Set("Referer", base.String())
	resp, err := guardedClient(client, base, true).Do(req)
	if err != nil {
		return requestError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("portal login returned HTTP %d; redirects and follow-up actions are not executed", resp.StatusCode)
	}
	reply, err := readBounded(resp.Body, maxReplyBytes)
	if err != nil {
		return err
	}
	var result struct {
		Status json.RawMessage `json:"status"`
		Info   string          `json:"info"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(reply, &result); err != nil {
		return errors.New("portal login returned invalid JSON")
	}
	statusText := strings.Trim(string(result.Status), `"`)
	status, err := strconv.ParseInt(statusText, 10, 64)
	if err != nil {
		return errors.New("portal login response has no valid top-level status")
	}
	if status != 0 {
		return nil
	}
	// On success, data may be a redirect URL string; on failure it may be an
	// object with a resultCode. Neither data nor resultData is ever executed.
	var failure struct {
		ResultCode json.RawMessage `json:"resultCode"`
		ResultData json.RawMessage `json:"resultData"`
	}
	var code string
	if json.Unmarshal(result.Data, &failure) == nil {
		if json.Unmarshal(failure.ResultCode, &code) != nil {
			var numeric json.Number
			if json.Unmarshal(failure.ResultCode, &numeric) == nil {
				code = numeric.String()
			}
		}
	}
	secrets := []string{account, password, encoded}
	for key, values := range fields {
		name := strings.ToLower(key)
		if strings.Contains(name, "sign") || strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "password") || name == "iv" {
			secrets = append(secrets, values...)
		}
	}
	var confirmationURL string
	_ = json.Unmarshal(failure.ResultData, &confirmationURL)
	safeCode := safeMessage(code, secrets)
	secrets = appendActionSecrets(secrets, confirmationURL)
	rejected := &LoginError{Info: safeMessage(result.Info, secrets), ResultCode: safeCode}
	if code == "124" {
		if target, err := rebindEndpoint(base, confirmationURL); err == nil {
			if client == nil {
				client = http.DefaultClient
			}
			rejected.rebind = &rebindAction{target: target.String(), pageURL: page.URL, client: client, secrets: secrets}
		}
	}
	return rejected
}

func endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Fragment != "" {
		return nil, errors.New("portal URL must be absolute HTTP(S), without credentials or fragment")
	}
	return u, nil
}

func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func guardedClient(client *http.Client, base *url.URL, noRedirects bool) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	previous := client.CheckRedirect
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if noRedirects || !sameOrigin(base, req.URL) || req.URL.User != nil {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("too many portal redirects")
		}
		if previous != nil {
			if err := previous(req, via); err != nil {
				return err
			}
		}
		if !sameOrigin(base, req.URL) || req.URL.User != nil {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &copyClient
}

func requestError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	// Transport errors can contain the entire request URL, including session data.
	return errors.New("portal network request failed")
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, errors.New("cannot read portal response")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("portal response exceeds size limit")
	}
	return data, nil
}

// serialize matches jQuery.param's encodeURIComponent escaping. Key order is
// deterministic; repeated values retain their form order.
func serialize(fields url.Values) string {
	return strings.NewReplacer("+", "%20", "%21", "!", "%27", "'", "%28", "(", "%29", ")", "%2A", "*").Replace(fields.Encode())
}

func encryptForm(plain, iv string) (string, error) {
	if len([]byte(iv)) != aes.BlockSize {
		return "", errors.New("invalid encryption IV")
	}
	block, err := aes.NewCipher([]byte(protocolKey))
	if err != nil {
		return "", errors.New("cannot initialize portal encryption")
	}
	data := []byte(plain)
	if remainder := len(data) % aes.BlockSize; remainder != 0 {
		data = append(data, make([]byte, aes.BlockSize-remainder)...)
	}
	cipher.NewCBCEncrypter(block, []byte(iv)).CryptBlocks(data, data)
	return base64.StdEncoding.EncodeToString(data), nil
}

func safeMessage(message string, secrets []string) string {
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, value := range secrets {
		if value == "" {
			continue
		}
		message = strings.ReplaceAll(message, value, "[redacted]")
		message = strings.ReplaceAll(message, url.QueryEscape(value), "[redacted]")
		jqueryEscaped := strings.TrimPrefix(serialize(url.Values{"": {value}}), "=")
		message = strings.ReplaceAll(message, jqueryEscaped, "[redacted]")
	}
	message = strings.Map(func(r rune) rune {
		if r < ' ' || r == 127 {
			return ' '
		}
		return r
	}, message)
	runes := []rune(message)
	if len(runes) > 240 {
		message = string(runes[:240]) + "..."
	}
	return message
}

func walk(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func textContent(n *html.Node) string {
	var text strings.Builder
	walk(n, func(child *html.Node) {
		if child.Type == html.TextNode {
			text.WriteString(child.Data)
		}
	})
	return text.String()
}

func disabled(n, form *html.Node) bool {
	for current := n; current != nil && current != form; current = current.Parent {
		if hasAttr(current, "disabled") {
			return true
		}
	}
	return false
}

func formFields(form *html.Node) url.Values {
	fields := make(url.Values)
	walk(form, func(n *html.Node) {
		name := attr(n, "name")
		if n.Type != html.ElementNode || name == "" || disabled(n, form) {
			return
		}
		switch n.Data {
		case "input":
			kind := strings.ToLower(attr(n, "type"))
			switch kind {
			case "submit", "button", "reset", "file", "image":
				return
			case "checkbox", "radio":
				if !hasAttr(n, "checked") {
					return
				}
			}
			value := attr(n, "value")
			if (kind == "checkbox" || kind == "radio") && !hasAttr(n, "value") {
				value = "on"
			}
			fields.Add(name, value)
		case "textarea":
			value := strings.ReplaceAll(textContent(n), "\r\n", "\n")
			fields.Add(name, strings.ReplaceAll(value, "\n", "\r\n"))
		case "select":
			var first *html.Node
			selected := false
			multiple := hasAttr(n, "multiple")
			walk(n, func(option *html.Node) {
				if option.Type != html.ElementNode || option.Data != "option" || disabled(option, n) {
					return
				}
				if first == nil {
					first = option
				}
				if !hasAttr(option, "selected") || (selected && !multiple) {
					return
				}
				fields.Add(name, optionValue(option))
				selected = true
			})
			if !multiple && !selected && first != nil {
				fields.Add(name, optionValue(first))
			}
		}
	})
	return fields
}

func optionValue(n *html.Node) string {
	if hasAttr(n, "value") {
		return attr(n, "value")
	}
	return strings.Join(strings.Fields(textContent(n)), " ")
}
