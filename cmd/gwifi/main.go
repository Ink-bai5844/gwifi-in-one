package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
	"gwifi-in-one/internal/download"
	"gwifi-in-one/internal/egress"
	"gwifi-in-one/internal/portal"
)

const discoveryURL = "http://www.msftconnecttest.com/connecttest.txt"

var errOnline = errors.New("此出口已能访问连通性检测页；无需重复登录")

type config struct {
	Exits []egress.Config `json:"exits"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		// net/url errors can contain private URL query strings. Strip them.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = fmt.Errorf("网络请求失败: %v", ue.Err)
		}
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`Gwifi 双出口下载原型 (Windows / IPv4)

  gwifi interfaces
  gwifi check [-config config.local.json] [-exit wired]
  gwifi login -exit wifi [-config config.local.json]
  gwifi logout -exit wifi [-config config.local.json]
  gwifi fetch -exits wired,wifi -workers 8 -url https://mirrors.aliyun.com/golang/go1.27.2.windows-amd64.zip -out go-dual-8.zip

fetch 选项: -workers 2 (每出口并发上限，1..32), -chunk-mib 4 (分片大小，1..64 MiB)
  -exit-workers wired=16,wifi=4 为指定出口单独设置并发数；其他出口沿用 -workers。
  例如上面的 -workers 8 表示有线最多 8 连接、Wi-Fi 最多 8 连接。
复制 config.example.json 为 config.local.json 并确认网卡名称。
login 在本机交互输入账号和密码，不保存凭据。logout 只退出明确指定的出口。
Ctrl+C 可取消下载。
失败/取消会保留 .part；此版本不自动续传或覆盖文件。
多网卡不等于独立账号带宽，请先分别测速，再比较并发总速度。`)
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage()
		return nil
	}
	if args[0] == "interfaces" {
		if len(args) > 1 {
			return fmt.Errorf("interfaces 不接受参数；-exit-workers 仅用于 fetch")
		}
		return interfaces()
	}
	if args[0] == "logout" {
		return runLogout(ctx, args[1:])
	}
	if args[0] != "check" && args[0] != "login" && args[0] != "fetch" {
		return fmt.Errorf("未知命令 %q", args[0])
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	path := fs.String("config", "config.local.json", "配置文件")
	selected := fs.String("exit", "", "单个出口名称")
	names := fs.String("exits", "", "下载出口名称，逗号分隔")
	rawURL := fs.String("url", "", "下载链接")
	output := fs.String("out", "", "保存路径")
	workers := fs.Int("workers", 2, "每个出口并发上限，1..32")
	exitWorkers := fs.String("exit-workers", "", "各出口并发上限，例如 wired=16,wifi=4；未列出者沿用 -workers")
	chunk := fs.Int64("chunk-mib", 4, "分片 MiB")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("有未识别的参数；所有选项必须使用 -名称 值")
	}
	exitWorkersSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "exit-workers" {
			exitWorkersSet = true
		}
	})
	if exitWorkersSet && args[0] != "fetch" {
		return fmt.Errorf("-exit-workers 仅用于 fetch")
	}
	if args[0] == "fetch" {
		if *rawURL == "" || *output == "" {
			return fmt.Errorf("fetch 必须提供 -url 和 -out")
		}
		if *workers < 1 || *workers > 32 {
			return fmt.Errorf("-workers 必须是 1..32 的整数（每个出口的并发上限）")
		}
		if *chunk < 1 || *chunk > 64 {
			return fmt.Errorf("-chunk-mib 必须是 1..64 的整数")
		}
		if exitWorkersSet && strings.TrimSpace(*exitWorkers) == "" {
			return fmt.Errorf("-exit-workers 不能为空；格式为 wired=16,wifi=4")
		}
	}
	if args[0] == "login" && *selected == "" {
		return fmt.Errorf("login 必须用 -exit 指定一个出口")
	}
	if args[0] != "fetch" && *names != "" {
		return fmt.Errorf("-exits 仅用于 fetch；检查或登录单出口请用 -exit")
	}
	if *selected != "" && *names != "" {
		return fmt.Errorf("-exit 与 -exits 不能同时使用")
	}
	cfg, err := readConfig(*path)
	if err != nil {
		return err
	}
	selection := *selected
	if args[0] == "fetch" && *names != "" {
		selection = *names
	}
	chosen, err := choose(cfg.Exits, selection)
	if err != nil {
		return err
	}
	if args[0] == "login" && len(chosen) != 1 {
		return fmt.Errorf("login 每次只能指定一个出口，分别执行各出口的登录命令")
	}
	var workersByExit map[string]int
	if args[0] == "fetch" {
		workersByExit, err = parseExitWorkers(*exitWorkers, chosen, *workers)
		if err != nil {
			return err
		}
	}
	var bound []*egress.Bound
	defer func() {
		for _, b := range bound {
			b.Close()
		}
	}()
	seen := map[string]bool{}
	var issues []error
	for _, ec := range chosen {
		b, err := egress.New(ec)
		if err != nil {
			if args[0] != "check" {
				return fmt.Errorf("%s: %w", ec.Name, err)
			}
			fmt.Printf("[%s] 不可用: %v\n", ec.Name, err)
			issues = append(issues, err)
			continue
		}
		key := fmt.Sprintf("%d/%s", b.Index, b.IP)
		if seen[key] {
			b.Close()
			return fmt.Errorf("两个出口指向同一网卡和地址，不能视为独立出口")
		}
		seen[key] = true
		bound = append(bound, b)
		fmt.Printf("[%s] %s / IPv4 %s / 网卡索引 %d\n", ec.Name, ec.Interface, b.IP, b.Index)
	}
	if args[0] == "check" {
		identities := map[string]string{}
		for _, b := range bound {
			_, online, onlineErr := connectivity(ctx, b)
			if online {
				fmt.Printf("[%s] 连通性检测通过；尚未证明账号带宽独立。\n", b.Config.Name)
				continue
			}
			if onlineErr != nil {
				fmt.Printf("[%s] 互联网检测未通过，继续检查认证页: %s\n", b.Config.Name, safeError(onlineErr))
			}
			page, err := inspect(ctx, b)
			if errors.Is(err, errOnline) {
				fmt.Printf("[%s] 连通性检测通过；尚未证明账号带宽独立。\n", b.Config.Name)
				continue
			}
			if err != nil {
				fmt.Printf("[%s] 检查失败: %s\n", b.Config.Name, safeError(err))
				issues = append(issues, err)
				continue
			}
			identity := page.Fields.Get("sta_ip")
			fmt.Printf("[%s] 可读取登录表单；页面终端 IP=%s（页面声明，非独立带宽证明）\n", b.Config.Name, identity)
			if identity != "" {
				if old, ok := identities[identity]; ok {
					e := fmt.Errorf("%s 与 %s 页面报告同一个终端 IP，请检查是否共用路由器出口", old, b.Config.Name)
					fmt.Println(e)
					issues = append(issues, e)
				}
				identities[identity] = b.Config.Name
			}
		}
		if len(issues) > 0 {
			return fmt.Errorf("有 %d 个出口检查问题，见上方详情", len(issues))
		}
		return nil
	}
	if args[0] == "login" {
		b := bound[0]
		page, err := inspect(ctx, b)
		if err != nil {
			return err
		}
		fmt.Printf("将通过 %s 登录，页面报告终端 IP=%s。\n", b.Config.Name, page.Fields.Get("sta_ip"))
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("请在交互终端运行 login；不接受管道中的明文密码")
		}
		fmt.Print("账号: ")
		account, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return err
		}
		fmt.Print("密码（不回显）: ")
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return err
		}
		defer func() {
			for i := range password {
				password[i] = 0
			}
		}()
		if err := portal.Login(ctx, b.Client, page, strings.TrimSpace(account), string(password)); err != nil {
			var pending *portal.LoginError
			if errors.As(err, &pending) && pending.CanRebind() {
				return handleRebind(ctx, b.Client, page, pending, os.Stdin, os.Stdout, func(ctx context.Context) (bool, error) {
					_, online, err := connectivity(ctx, b)
					return online, err
				})
			}
			return err
		}
		fmt.Println("认证接口报告成功。请运行 check 验证连通性，再比较单路与双路下载速度。")
		return nil
	}
	var exits []download.Exit
	for _, b := range bound {
		n := workersByExit[b.Config.Name]
		exits = append(exits, download.Exit{Name: b.Config.Name, Client: b.Client, Workers: n})
		fmt.Printf("[%s] 最多 %d 个并发下载连接\n", b.Config.Name, n)
	}
	start := time.Now()
	last := time.Time{}
	lastBytes := map[string]int64{}
	var mu sync.Mutex
	err = download.Download(ctx, exits, download.Options{URL: *rawURL, Output: *output, ChunkSize: *chunk * 1024 * 1024, WorkersPerExit: *workers}, func(p download.Progress) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		window := now.Sub(last).Seconds()
		if window < 1 && p.Completed != p.Total {
			return
		}
		showWindow := !last.IsZero() && window >= 1
		elapsed := now.Sub(start).Seconds()
		if elapsed <= 0 {
			elapsed = 1
		}
		fmt.Printf("%.1f / %.1f MiB，平均 %.2f MB/s (%.1f Mbps)", float64(p.Completed)/(1<<20), float64(p.Total)/(1<<20), float64(p.Completed)/elapsed/1e6, float64(p.Completed)*8/elapsed/1e6)
		keys := make([]string, 0, len(p.BytesByExit))
		for k := range p.BytesByExit {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf(" | %s %.1f MiB", k, float64(p.BytesByExit[k])/(1<<20))
			if showWindow {
				fmt.Printf("（近 %.1f 秒 %.1f Mbps）", window, float64(p.BytesByExit[k]-lastBytes[k])*8/window/1e6)
			}
		}
		fmt.Println()
		last = now
		for k, n := range p.BytesByExit {
			lastBytes[k] = n
		}
	})
	if err != nil {
		return err
	}
	fmt.Println("下载完成:", *output)
	return nil
}

// parseExitWorkers resolves limits before opening any network interfaces.
func parseExitWorkers(raw string, chosen []egress.Config, fallback int) (map[string]int, error) {
	result := make(map[string]int, len(chosen))
	for _, exit := range chosen {
		result[exit.Name] = fallback
	}
	if raw == "" {
		return result, nil
	}
	seen := make(map[string]bool)
	for _, entry := range strings.Split(raw, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(entry), "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("-exit-workers 含空值或格式错误；格式为 wired=16,wifi=4")
		}
		if _, ok := result[name]; !ok {
			return nil, fmt.Errorf("-exit-workers 出口 %q 不在本次下载出口中，请检查 -exits 和配置", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("-exit-workers 出口重复: %s", name)
		}
		seen[name] = true
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 32 {
			return nil, fmt.Errorf("-exit-workers 出口 %s 的并发数必须是 1..32 的整数", name)
		}
		result[name] = n
	}
	return result, nil
}

func safeError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func readConfig(path string) (config, error) {
	var c config
	f, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("读取配置失败；先复制 config.example.json 为 config.local.json: %w", err)
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("配置格式错误: %w", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("配置包含多余内容")
	}
	if len(c.Exits) == 0 {
		return c, fmt.Errorf("至少配置一个出口")
	}
	seen := map[string]bool{}
	for _, e := range c.Exits {
		if e.Name == "" || seen[e.Name] {
			return c, fmt.Errorf("出口名称为空或重复")
		}
		seen[e.Name] = true
	}
	return c, nil
}

func choose(all []egress.Config, selection string) ([]egress.Config, error) {
	if selection == "" {
		return all, nil
	}
	var selected []egress.Config
	seen := map[string]bool{}
	for _, name := range strings.Split(selection, ",") {
		name = strings.TrimSpace(name)
		if seen[name] {
			return nil, fmt.Errorf("出口重复: %s", name)
		}
		seen[name] = true
		found := false
		for _, e := range all {
			if e.Name == name {
				selected = append(selected, e)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("配置中不存在出口 %q", name)
		}
	}
	return selected, nil
}

func inspect(ctx context.Context, b *egress.Bound) (*portal.Page, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	raw := b.Config.PortalURL
	if raw == "" {
		var online bool
		var err error
		raw, online, err = connectivity(ctx, b)
		if err != nil {
			return nil, fmt.Errorf("自动发现认证页失败；可填写此出口当前真实登录页 portal_url: %w", err)
		}
		if online {
			return nil, errOnline
		}
		if raw == discoveryURL {
			return nil, fmt.Errorf("未发现 HTTP 重定向认证页；请填写此出口实际登录页 portal_url（不支持自动执行脚本跳转）")
		}
	}
	return portal.Inspect(ctx, b.Client, raw)
}

func connectivity(ctx context.Context, b *egress.Bound) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("User-Agent", portal.UserAgent)
	res, err := b.Client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4096))
	if err != nil {
		return "", false, err
	}
	return res.Request.URL.String(), res.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "Microsoft Connect Test", nil
}

func interfaces() error {
	list, err := net.Interfaces()
	if err != nil {
		return err
	}
	for _, i := range list {
		if i.Flags&net.FlagLoopback != 0 {
			continue
		}
		var ips []string
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ip, _, _ := net.ParseCIDR(a.String())
			if ip.To4() != nil {
				ips = append(ips, ip.String())
			}
		}
		state := "down"
		if i.Flags&net.FlagUp != 0 {
			state = "up"
		}
		fmt.Printf("%-20s index=%-3d %-4s IPv4=%s\n", i.Name, i.Index, state, strings.Join(ips, ","))
	}
	return nil
}
