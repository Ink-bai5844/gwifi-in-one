package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gwifi-in-one/internal/egress"
)

func TestParseExitWorkers(t *testing.T) {
	chosen := []egress.Config{{Name: "wired"}, {Name: "wifi"}}
	for _, tc := range []struct {
		name string
		raw  string
		want map[string]int
	}{
		{"global", "", map[string]int{"wired": 8, "wifi": 8}},
		{"override_one", "wired=16", map[string]int{"wired": 16, "wifi": 8}},
		{"override_both", "wired=16,wifi=4", map[string]int{"wired": 16, "wifi": 4}},
		{"whitespace_and_bounds", " wifi = 1 , wired = 32 ", map[string]int{"wired": 32, "wifi": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseExitWorkers(tc.raw, chosen, 8)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestParseExitWorkersRejectsInvalid(t *testing.T) {
	chosen := []egress.Config{{Name: "wired"}, {Name: "wifi"}}
	for _, tc := range []struct{ raw, message string }{
		{"mobile=4", "不在本次下载出口"},
		{"wired=4,wired=8", "出口重复"},
		{"wired=4,", "空值或格式错误"},
		{"wired=", "空值或格式错误"},
		{"=4", "空值或格式错误"},
		{"wired", "空值或格式错误"},
		{" ", "空值或格式错误"},
		{"wired=one", "1..32 的整数"},
		{"wired=1.5", "1..32 的整数"},
		{"wired=2=3", "1..32 的整数"},
		{"wired=0", "1..32 的整数"},
		{"wired=-1", "1..32 的整数"},
		{"wired=33", "1..32 的整数"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			_, err := parseExitWorkers(tc.raw, chosen, 8)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("got %v; want error containing %q", err, tc.message)
			}
		})
	}
}

func TestExitWorkersRejectedBeforeInterfacesOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// If parameter validation reaches egress.New, this deliberately nonexistent
	// interface would mask the expected, actionable parameter error.
	data := `{"exits":[{"name":"wired","interface":"test-missing-interface"},{"name":"wifi","interface":"test-missing-interface-2"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"check", []string{"check", "-exit-workers", "wired=4"}, "仅用于 fetch"},
		{"login", []string{"login", "-exit", "wifi", "-exit-workers", "wifi=4"}, "仅用于 fetch"},
		{"interfaces", []string{"interfaces", "-exit-workers", "wired=4"}, "仅用于 fetch"},
		{"empty", []string{"fetch", "-url", "https://example.invalid/file", "-out", "unused.zip", "-exit-workers="}, "不能为空"},
		{"unknown", []string{"fetch", "-config", path, "-url", "https://example.invalid/file", "-out", "unused.zip", "-exit-workers", "mobile=4"}, "不在本次下载出口"},
		{"unselected", []string{"fetch", "-config", path, "-exits", "wired", "-url", "https://example.invalid/file", "-out", "unused.zip", "-exit-workers", "wifi=4"}, "不在本次下载出口"},
		{"global_high", []string{"fetch", "-url", "https://example.invalid/file", "-out", "unused.zip", "-workers", "33"}, "-workers 必须是 1..32"},
		{"global_low", []string{"fetch", "-url", "https://example.invalid/file", "-out", "unused.zip", "-workers", "0"}, "-workers 必须是 1..32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want error containing %q", err, tc.want)
			}
		})
	}
}
