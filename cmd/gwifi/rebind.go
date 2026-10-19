package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gwifi-in-one/internal/portal"
)

var errRebindCanceled = errors.New("已取消换绑，未发送确认请求；本次登录未完成")

// handleRebind is entered only after the portal returns a supported confirmation
// action. The user's answer authorizes one request; failure never retries it.
func handleRebind(ctx context.Context, client *http.Client, page *portal.Page, pending *portal.LoginError, input io.Reader, output io.Writer, checkOnline func(context.Context) (bool, error)) error {
	if pending == nil || !pending.CanRebind() {
		return errors.New("没有可确认的设备换绑请求")
	}
	fmt.Fprintln(output, pending.Info)
	fmt.Fprintln(output, "确认换绑会消耗剩余换绑次数。")
	if err := askRebindConfirmation(ctx, input, output); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %v", errRebindCanceled, ctx.Err())
	}
	if err := portal.ConfirmRebind(ctx, client, page, pending); err != nil {
		return fmt.Errorf("换绑确认未获成功响应，无法确定是否已经换绑；请先运行 check 核实，不要直接重复确认: %w", err)
	}
	online, err := checkOnline(ctx)
	if err != nil || !online {
		return errors.New("换绑请求已获成功响应，但尚未确认此出口联网；请先运行 check 检查，若仍未联网再运行 login")
	}
	fmt.Fprintln(output, "换绑请求已获成功响应，当前出口连通性检测通过。可运行 check 检查两个出口，再测试双出口下载。")
	return nil
}

func askRebindConfirmation(ctx context.Context, input io.Reader, output io.Writer) error {
	// Use a fresh reader after ReadPassword has restored the terminal settings.
	reader := bufio.NewReader(input)
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %v", errRebindCanceled, ctx.Err())
		}
		fmt.Fprint(output, "是否确认换绑至当前设备？[y/N]: ")
		type lineResult struct {
			line string
			err  error
		}
		result := make(chan lineResult, 1)
		// A terminal read may remain blocked after Ctrl+C. Selecting on the
		// context lets the CLI exit without waiting for an additional Enter.
		go func() {
			line, err := reader.ReadString('\n')
			result <- lineResult{line: line, err: err}
		}()
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", errRebindCanceled, ctx.Err())
		case answer := <-result:
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %v", errRebindCanceled, ctx.Err())
			}
			if answer.err != nil {
				return fmt.Errorf("%w（输入结束或读取失败）", errRebindCanceled)
			}
			if strings.ContainsRune(answer.line, '\x03') {
				return errRebindCanceled
			}
			switch strings.ToLower(strings.TrimSpace(answer.line)) {
			case "y":
				return nil
			case "n", "":
				return errRebindCanceled
			default:
				fmt.Fprintln(output, "请输入 y 确认，或 n / 直接回车取消。")
			}
		}
	}
}
