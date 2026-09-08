package main

import (
	"bufio"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/lingyicute/Go-TCP-Proxy/internal/config"
)

func TestParseFlags(t *testing.T) {
	cases := []struct {
		args        []string
		wantErr     bool
		interactive bool
	}{
		{nil, false, true},
		{[]string{"-y"}, false, false},
		{[]string{"-r", "a:1"}, false, false},
		{[]string{"-l", ":1", "-r", "a:1", "-s", "b:1", "--no-save"}, false, false},
		{[]string{"-shutdown-timeout", "1s", "-dial-timeout", "2s"}, false, true},
		{[]string{"positional"}, true, false},
		{[]string{"-unknown"}, true, false},
	}
	for _, tc := range cases {
		o, err := parseFlags(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("%v: err=%v, wantErr=%v", tc.args, err, tc.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		got := !o.yes && o.listen == "" && o.remote == "" && o.socks == ""
		if got != tc.interactive {
			t.Errorf("%v: interactive=%v, want %v", tc.args, got, tc.interactive)
		}
	}

	o, err := parseFlags([]string{"-shutdown-timeout", "1s", "-dial-timeout", "2s"})
	if err != nil || o.shutdownTimeout != time.Second || o.dialTimeout != 2*time.Second {
		t.Errorf("duration flags 解析错误: %+v err=%v", o, err)
	}
	if _, err := parseFlags([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h 应返回 flag.ErrHelp, 实际 %v", err)
	}
}

// 回归测试：旧实现每次提问都新建 bufio.Reader，管道输入的第二行会被吞掉并报 EOF。
func TestPrompt_ReadsConsecutiveLinesFromOneReader(t *testing.T) {
	stdinEOF = false
	r := bufio.NewReader(strings.NewReader("first\n  second  \n\nlast-without-newline"))
	if got := prompt(r, "a", "d1"); got != "first" {
		t.Errorf("第 1 行: %q", got)
	}
	if got := prompt(r, "b", "d2"); got != "second" {
		t.Errorf("第 2 行应去掉首尾空白: %q", got)
	}
	if got := prompt(r, "c", "d3"); got != "d3" {
		t.Errorf("空行应返回默认值: %q", got)
	}
	if got := prompt(r, "d", "d4"); got != "last-without-newline" {
		t.Errorf("无换行的最后一行也应被读取: %q", got)
	}
	if got := prompt(r, "e", "d5"); got != "d5" || !stdinEOF {
		t.Errorf("EOF 后应返回默认值并标记 stdinEOF: %q %v", got, stdinEOF)
	}
}

func TestPromptAddr_RetriesUntilValid(t *testing.T) {
	stdinEOF = false
	r := bufio.NewReader(strings.NewReader("nonsense\n:99999\nexample.com:22\n"))
	got := promptAddr(r, "x", "default:1", config.ValidateDialAddr)
	if got != "example.com:22" {
		t.Errorf("应跳过两个非法输入, 实际 %q", got)
	}
}

func TestPromptAddr_StopsRetryingOnEOF(t *testing.T) {
	stdinEOF = false
	r := bufio.NewReader(strings.NewReader(""))
	done := make(chan string, 1)
	go func() { done <- promptAddr(r, "x", "invalid-default", config.ValidateDialAddr) }()
	select {
	case got := <-done:
		if got != "invalid-default" {
			t.Errorf("EOF 时应原样返回默认值交给后续校验, 实际 %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stdin 结束且默认值非法时 promptAddr 死循环")
	}
}
