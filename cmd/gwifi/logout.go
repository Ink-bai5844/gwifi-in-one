package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gwifi-in-one/internal/egress"
	"gwifi-in-one/internal/portal"
)

const defaultLogoutPortalURL = "http://100.100.100.2/"

func runLogout(ctx context.Context, args []string) error {
	path, selected, err := parseLogoutArgs(args)
	if err != nil {
		return err
	}
	cfg, err := readConfig(path)
	if err != nil {
		return err
	}
	chosen, err := choose(cfg.Exits, selected)
	if err != nil {
		return err
	}
	if len(chosen) != 1 {
		return errors.New("logout 每次只能指定一个出口")
	}
	b, err := egress.New(chosen[0])
	if err != nil {
		return fmt.Errorf("%s: %w", selected, err)
	}
	defer b.Close()
	fmt.Printf("[%s] %s / IPv4 %s / 网卡索引 %d\n", b.Config.Name, b.Config.Interface, b.IP, b.Index)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	rawURL := strings.TrimSpace(b.Config.PortalURL)
	if rawURL == "" {
		rawURL = defaultLogoutPortalURL
	}
	var session *portal.LogoutSession
	return handleLogout(ctx, selected, os.Stdout,
		func(ctx context.Context) (bool, error) {
			// A public probe can fail while the local portal remains available.
			// Only positive captive-portal evidence makes logout unnecessary.
			if offline, _ := logoutNeedsAuthentication(ctx, b); offline {
				return true, nil
			}
			var err error
			session, err = portal.PrepareLogout(ctx, b.Client, rawURL)
			if err != nil {
				return false, err
			}
			fmt.Printf("[%s] 已从门户下线页面读取当前出口的在线会话。\n", selected)
			return false, nil
		},
		func(ctx context.Context) error { return portal.Logout(ctx, b.Client, session) },
		func(ctx context.Context) (bool, error) {
			// Verify on a new connection: an already established connection may
			// remain usable briefly after the portal revokes authentication.
			b.Client.CloseIdleConnections()
			return logoutNeedsAuthentication(ctx, b)
		},
	)
}

// Parse logout independently so no download or multi-exit flags can silently
// select a different session. Validation happens before configuration or I/O.
func parseLogoutArgs(args []string) (path, selected string, err error) {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&path, "config", "config.local.json", "配置文件")
	var exitCount int
	fs.Func("exit", "要退出的单个出口名称", func(value string) error {
		exitCount++
		selected = strings.TrimSpace(value)
		return nil
	})
	if err = fs.Parse(args); err != nil {
		return "", "", err
	}
	if len(fs.Args()) != 0 {
		return "", "", errors.New("logout 有未识别的参数；请使用 logout -exit wired 或 logout -exit wifi")
	}
	if exitCount != 1 || selected == "" || strings.Contains(selected, ",") {
		return "", "", errors.New("logout 必须用 -exit 明确指定一个出口，每次只退出一路")
	}
	return path, selected, nil
}

// handleLogout separates an accepted logout request from verified captive
// authentication. Failures never retry a request or start a new login.
func handleLogout(ctx context.Context, exit string, output io.Writer, prepare func(context.Context) (bool, error), logout func(context.Context) error, verify func(context.Context) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	alreadyOffline, err := prepare(ctx)
	if err != nil {
		return fmt.Errorf("无法确认当前出口的认证会话，未发送退出请求: %w", err)
	}
	if alreadyOffline {
		fmt.Fprintf(output, "[%s] 当前出口已处于未登录状态，无需退出。\n", exit)
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Fprintf(output, "[%s] 正在退出当前出口会话；该出口后共用此会话的设备也会断开校园网认证。\n", exit)
	if err := logout(ctx); err != nil {
		return fmt.Errorf("退出请求未获成功响应，无法确定会话是否已退出；请运行 check 核实，不会自动重试: %w", err)
	}
	loggedOut, err := verify(ctx)
	if err != nil || !loggedOut {
		return errors.New("退出接口已接受请求，但尚未确认此出口需要重新认证；请运行 check 核实后再登录")
	}
	fmt.Fprintf(output, "[%s] 已退出账号，并确认此出口需要重新认证。可运行 gwifi login -exit %s 登录其他账号。\n", exit, exit)
	return nil
}

// A login form alone is not offline evidence: this deployment also serves it
// to authenticated clients. Only a public connectivity request being redirected
// to a valid Gwifi login form confirms captive authentication is active again.
func logoutNeedsAuthentication(ctx context.Context, b *egress.Bound) (bool, error) {
	raw, online, err := connectivity(ctx, b)
	if err != nil {
		return false, err
	}
	if online || raw == discoveryURL {
		return false, nil
	}
	if _, err := portal.Inspect(ctx, b.Client, raw); err != nil {
		return false, err
	}
	return true, nil
}
