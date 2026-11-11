package main

import (
	"bufio"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lingyicute/Go-TCP-Proxy/internal/config"
)

// useStdin 把全局 stdin 换成给定内容（并当作非终端处理），在测试结束后恢复。
func useStdin(t *testing.T, content string) {
	t.Helper()
	old, oldEOF, oldTTY := stdin, stdinEOF, stdinIsTerminal
	stdin, stdinEOF, stdinIsTerminal = bufio.NewReader(strings.NewReader(content)), false, false
	t.Cleanup(func() { stdin, stdinEOF, stdinIsTerminal = old, oldEOF, oldTTY })
}

// 测试不能受调用者环境的影响：平时就靠 TCP_PROXY_SOCKS_PASSWORD 使用本工具的人，shell 里多半
// 一直导出着它，而 resolveConfig 会把它当作密码读进来——断言密码的用例于是全都对不上。
// 需要这个变量的用例自己用 t.Setenv 设置。
func TestMain(m *testing.M) {
	os.Unsetenv(passwordEnv)
	os.Exit(m.Run())
}

func mustParse(t *testing.T, args ...string) *options {
	t.Helper()
	o, err := parseFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// 回归测试：拿不到用户配置目录时（容器 / 服务里没有 $HOME、%AppData%），
// 旧实现直接退出，哪怕地址已经通过 -l/-r/-s 全部给出且 --no-save。
func TestResolveConfig_MissingConfigDirIsNotFatal(t *testing.T) {
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "home"} {
		t.Setenv(k, "")
	}
	if _, err := config.DefaultPath(); err == nil {
		t.Skip("当前平台清空环境变量后仍能得到配置目录，跳过")
	}

	cfg, path, err := resolveConfig(mustParse(t, "-l", "127.0.0.1:0", "-r", "db.internal:5432", "-s", "127.0.0.1:1080", "--no-save"))
	if err != nil {
		t.Fatalf("地址全部来自命令行时不应因缺少配置目录而失败: %v", err)
	}
	if path != "" {
		t.Errorf("没有配置目录时不应返回保存路径, 实际 %q", path)
	}
	if cfg.RemoteAddr != "db.internal:5432" {
		t.Errorf("remote = %q", cfg.RemoteAddr)
	}

	// 没有配置目录、也没给 -r：仍然要像"配置文件不存在"一样拒绝占位符默认值
	if _, _, err := resolveConfig(mustParse(t, "-y")); err == nil {
		t.Fatal("既无配置目录又无 -r 时应报错，而不是静默转发到 example.com:80")
	}
}

// 回归测试：标准输入立刻 EOF（没有 TTY）且没有配置文件时，旧实现会把占位符
// example.com:80 当作真实目标启动服务，还把它写进配置文件变成以后的默认值。
func TestResolveConfig_StdinEOFWithoutConfigRejectsPlaceholder(t *testing.T) {
	useStdin(t, "")
	missing := filepath.Join(t.TempDir(), "missing.json")
	_, _, err := resolveConfig(mustParse(t, "-c", missing))
	if err == nil {
		t.Fatal("stdin EOF 且无配置文件时应报错")
	}
	if !strings.Contains(err.Error(), "-r") {
		t.Errorf("错误信息应提示使用 -r, 实际: %v", err)
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Error("失败路径不应创建配置文件")
	}
}

// 回归测试：配置文件损坏时交互模式用默认值兜底；若标准输入又立刻 EOF，旧实现只在"文件不存在"时
// 拒绝占位符，于是照样把 example.com:80 当作真实目标，还会用默认值覆盖掉那份损坏的配置。
func TestResolveConfig_StdinEOFWithMalformedConfigRejectsPlaceholder(t *testing.T) {
	useStdin(t, "")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveConfig(mustParse(t, "-c", path)); err == nil {
		t.Fatal("stdin EOF 且配置文件损坏时应报错，而不是采用占位符 example.com:80")
	}
}

// 有已保存的配置时，stdin EOF 只是"一路回车"：沿用保存的值即可。
func TestResolveConfig_StdinEOFWithSavedConfigUsesIt(t *testing.T) {
	useStdin(t, "")
	path := filepath.Join(t.TempDir(), "config.json")
	saved := config.Config{LocalAddr: "127.0.0.1:18080", RemoteAddr: "git.internal:22", SocksAddr: "10.0.0.1:1080"}
	if err := config.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	cfg, gotPath, err := resolveConfig(mustParse(t, "-c", path))
	if err != nil {
		t.Fatal(err)
	}
	if cfg != saved || gotPath != path {
		t.Errorf("应沿用已保存的配置: cfg=%+v path=%q", cfg, gotPath)
	}
}

// 管道里显式给出了远程地址：即使之后 stdin 结束、也没有配置文件，也应正常接受。
func TestResolveConfig_PipedRemoteWithoutConfig(t *testing.T) {
	useStdin(t, "\nssh.internal:22") // 监听地址回车用默认；远程地址给出后输入结束
	cfg, _, err := resolveConfig(mustParse(t, "-c", filepath.Join(t.TempDir(), "missing.json")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RemoteAddr != "ssh.internal:22" || cfg.LocalAddr != config.Defaults.LocalAddr {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestResolveConfig_NonInteractiveWithoutConfigRequiresRemote(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	if _, _, err := resolveConfig(mustParse(t, "-c", missing, "-y")); err == nil {
		t.Fatal("-y 且无配置文件、无 -r 时应报错")
	}
	cfg, path, err := resolveConfig(mustParse(t, "-c", missing, "-r", "a.internal:1"))
	if err != nil {
		t.Fatal(err)
	}
	if path != missing || cfg.RemoteAddr != "a.internal:1" {
		t.Errorf("cfg=%+v path=%q", cfg, path)
	}
}

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
		{[]string{"-socks-user", "alice", "-socks-password", "pw"}, false, true}, // 凭据参数不改变交互与否
		{[]string{"-y", "-socks-user", "alice"}, false, false},
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

	o, err := parseFlags([]string{"-shutdown-timeout", "1s", "-dial-timeout", "2s", "-idle-timeout", "3m"})
	if err != nil || o.shutdownTimeout != time.Second || o.dialTimeout != 2*time.Second || o.idleTimeout != 3*time.Minute {
		t.Errorf("duration flags 解析错误: %+v err=%v", o, err)
	}
	if o, err := parseFlags(nil); err != nil || o.idleTimeout != 0 {
		t.Errorf("-idle-timeout 默认应为 0（不限制）: %+v err=%v", o, err)
	}
	if o, err := parseFlags([]string{"-socks-user", "alice", "-socks-password", "p w"}); err != nil || o.socksUser != "alice" || o.socksPassword != "p w" {
		t.Errorf("凭据参数解析错误: %+v err=%v", o, err)
	}
	if _, err := parseFlags([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h 应返回 flag.ErrHelp, 实际 %v", err)
	}
}

// ---------- Socks5 认证：交互式 / 参数 / 环境变量 ----------

func TestResolveConfig_InteractiveAuthFlow(t *testing.T) {
	t.Setenv(passwordEnv, "")
	// 监听地址回车；远程地址；代理地址回车；需要认证 y；用户名；密码（首尾空格要保留）
	useStdin(t, "\nssh.internal:22\n\ny\nalice\n  s3cret 密码  \n")
	cfg, _, err := resolveConfig(mustParse(t, "-c", filepath.Join(t.TempDir(), "missing.json")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocksUser != "alice" || cfg.SocksPassword != "  s3cret 密码  " {
		t.Errorf("凭据应来自交互输入且密码不去空白: user=%q password=%q", cfg.SocksUser, cfg.SocksPassword)
	}
	if cfg.RemoteAddr != "ssh.internal:22" {
		t.Errorf("remote = %q", cfg.RemoteAddr)
	}
}

// 用户名为空会被反复追问；标准输入结束后交给 Validate 报错，而不是死循环或带着空用户名启动。
func TestResolveConfig_InteractiveAuthRequiresUsername(t *testing.T) {
	t.Setenv(passwordEnv, "")
	useStdin(t, "\nssh.internal:22\n\ny\n\n\n")
	_, _, err := resolveConfig(mustParse(t, "-c", filepath.Join(t.TempDir(), "missing.json")))
	if err == nil || !strings.Contains(err.Error(), "用户名") {
		t.Fatalf("回答需要认证却不给用户名时应报错, 实际 %v", err)
	}
}

func TestResolveConfig_InteractiveDeclineClearsSavedCredentials(t *testing.T) {
	t.Setenv(passwordEnv, "")
	path := filepath.Join(t.TempDir(), "config.json")
	saved := config.Config{LocalAddr: "127.0.0.1:18080", RemoteAddr: "git.internal:22", SocksAddr: "10.0.0.1:1080", SocksUser: "alice", SocksPassword: "old"}
	if err := config.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	useStdin(t, "\n\n\nn\n")
	cfg, _, err := resolveConfig(mustParse(t, "-c", path))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocksUser != "" || cfg.SocksPassword != "" {
		t.Errorf("回答\"否\"应清除已保存的凭据, 实际 user=%q password=%q", cfg.SocksUser, cfg.SocksPassword)
	}
}

// 一路回车（以及 stdin 直接 EOF）：有保存的用户名时默认"需要认证"，用户名、密码都沿用保存的值。
func TestResolveConfig_InteractiveEnterKeepsSavedCredentials(t *testing.T) {
	t.Setenv(passwordEnv, "")
	path := filepath.Join(t.TempDir(), "config.json")
	saved := config.Config{LocalAddr: "127.0.0.1:18080", RemoteAddr: "git.internal:22", SocksAddr: "10.0.0.1:1080", SocksUser: "alice", SocksPassword: "old"}
	if err := config.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"\n\n\n\n\n\n", ""} {
		useStdin(t, input)
		cfg, _, err := resolveConfig(mustParse(t, "-c", path))
		if err != nil {
			t.Fatal(err)
		}
		if cfg != saved {
			t.Errorf("输入 %q 时应沿用保存的凭据: %+v", input, cfg)
		}
	}
}

func TestResolveConfig_PasswordFromEnvAndFlagPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	saved := config.Config{LocalAddr: "127.0.0.1:18080", RemoteAddr: "git.internal:22", SocksAddr: "10.0.0.1:1080", SocksUser: "alice", SocksPassword: "fromfile"}
	if err := config.Save(path, saved); err != nil {
		t.Fatal(err)
	}

	t.Setenv(passwordEnv, "fromenv")
	cfg, _, err := resolveConfig(mustParse(t, "-y", "-c", path, "-socks-user", "bob"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocksUser != "bob" || cfg.SocksPassword != "fromenv" {
		t.Errorf("环境变量应覆盖配置文件中的密码: user=%q password=%q", cfg.SocksUser, cfg.SocksPassword)
	}

	cfg, _, err = resolveConfig(mustParse(t, "-y", "-c", path, "-socks-password", "fromflag"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocksUser != "alice" || cfg.SocksPassword != "fromflag" {
		t.Errorf("命令行参数应优先于环境变量: user=%q password=%q", cfg.SocksUser, cfg.SocksPassword)
	}

	t.Setenv(passwordEnv, "fromenv")
	cfg, _, err = resolveConfig(mustParse(t, "-y", "-c", path, "-socks-password", ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocksUser != "alice" || cfg.SocksPassword != "" {
		t.Errorf("显式给出空 -socks-password 应覆盖环境变量与配置文件: user=%q password=%q", cfg.SocksUser, cfg.SocksPassword)
	}

	cfg, _, err = resolveConfig(mustParse(t, "-y", "-c", path, "-socks-user", ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocksUser != "" || cfg.SocksPassword != "" {
		t.Errorf("显式给出空 -socks-user 应清除已保存的凭据: user=%q password=%q", cfg.SocksUser, cfg.SocksPassword)
	}

	t.Setenv(passwordEnv, "")
	cfg, _, err = resolveConfig(mustParse(t, "-y", "-c", path))
	if err != nil {
		t.Fatal(err)
	}
	if cfg != saved {
		t.Errorf("没有参数和环境变量时应沿用配置文件: %+v", cfg)
	}
}

// 环境变量里的密码常年导出在 shell 里：这个实例没有配置用户名时应忽略它，而不是拒绝启动；
// 交互模式下仍把它当作密码的默认值，供回答"需要认证"之后沿用。
func TestResolveConfig_EnvPasswordWithoutUserIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	saved := config.Config{LocalAddr: "127.0.0.1:18080", RemoteAddr: "git.internal:22", SocksAddr: "10.0.0.1:1080"}
	if err := config.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	t.Setenv(passwordEnv, "fromenv")

	cfg, _, err := resolveConfig(mustParse(t, "-y", "-c", path))
	if err != nil {
		t.Fatalf("没有用户名时环境变量里的密码不应导致启动失败: %v", err)
	}
	if cfg != saved {
		t.Errorf("没有用户名时应忽略环境变量里的密码: %+v", cfg)
	}

	useStdin(t, "\n\n\ny\nalice\n\n")
	cfg, _, err = resolveConfig(mustParse(t, "-c", path))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SocksUser != "alice" || cfg.SocksPassword != "fromenv" {
		t.Errorf("交互模式下应沿用环境变量里的密码: user=%q password=%q", cfg.SocksUser, cfg.SocksPassword)
	}
}

func TestResolveConfig_PasswordWithoutUserIsRejected(t *testing.T) {
	t.Setenv(passwordEnv, "")
	missing := filepath.Join(t.TempDir(), "missing.json")
	_, _, err := resolveConfig(mustParse(t, "-c", missing, "-r", "a.internal:1", "-socks-password", "x"))
	if err == nil || !strings.Contains(err.Error(), "用户名") {
		t.Fatalf("只给密码不给用户名应报错, 实际 %v", err)
	}
}

func TestPromptYesNo(t *testing.T) {
	cases := []struct {
		input      string
		defaultYes bool
		want       bool
	}{
		{"y\n", false, true},
		{"YES\n", false, true},
		{"是\n", false, true},
		{"n\n", true, false},
		{"否\n", true, false},
		{"\n", true, true},
		{"\n", false, false},
		{"", true, true},            // 直接 EOF 用默认值
		{"maybe\nn\n", true, false}, // 无法识别的回答会被追问
		{"garbage", false, false},   // 最后一行不可识别且已 EOF：用默认值，不死循环
	}
	for _, tc := range cases {
		useStdin(t, tc.input)
		if got := promptYesNo(stdin, "q", tc.defaultYes); got != tc.want {
			t.Errorf("输入 %q 默认 %v: 期望 %v, 实际 %v", tc.input, tc.defaultYes, tc.want, got)
		}
	}
}

func TestPromptPassword_NonTerminal(t *testing.T) {
	cases := []struct{ input, saved, want string }{
		{"  spaces kept  \n", "", "  spaces kept  "},
		{"crlf\r\n", "", "crlf"},
		{"\n", "saved", "saved"},
		{"", "saved", "saved"}, // EOF 沿用保存的值
		{"\n", "", ""},         // 没有保存的值时空输入就是空密码
		{"new\n", "saved", "new"},
	}
	for _, tc := range cases {
		useStdin(t, tc.input)
		if got := promptPassword(stdin, "p", tc.saved); got != tc.want {
			t.Errorf("输入 %q 保存值 %q: 期望 %q, 实际 %q", tc.input, tc.saved, tc.want, got)
		}
	}
}

// 回归测试：在终端的密码提问处按 Ctrl+D（空行上的 EOF）应当和其它提问一样沿用现有的值；
// 之前这里会以"无法读取输入: EOF"直接退出。
func TestPromptPassword_TerminalEOFKeepsSaved(t *testing.T) {
	useStdin(t, "")
	oldRead := readPassword
	t.Cleanup(func() { readPassword = oldRead })
	stdinIsTerminal, readPassword = true, func(int) ([]byte, error) { return nil, io.EOF }

	if got := promptPassword(stdin, "p", "saved"); got != "saved" {
		t.Errorf("期望沿用保存的密码, 实际 %q", got)
	}
	if !stdinEOF {
		t.Error("终端密码输入遇到 EOF 后应置位 stdinEOF")
	}
	if got := promptPassword(stdin, "p", ""); got != "" {
		t.Errorf("没有保存的密码时期望得到空密码, 实际 %q", got)
	}
}

func TestPromptPassword_RetriesWhenTooLongAndReadsBuffered(t *testing.T) {
	useStdin(t, strings.Repeat("p", 256)+"\nvalid-pw\n")
	if got := promptPassword(stdin, "p", ""); got != "valid-pw" {
		t.Errorf("超长密码后应重问并读取下一行, 实际 %q", got)
	}

	// 终端粘贴多行时，前面的提问已把密码行预读进 bufio.Reader：应从缓冲区读取而非调用 readPassword 阻塞
	useStdin(t, "alice\npasted-pw\n")
	oldRead := readPassword
	t.Cleanup(func() { readPassword = oldRead })
	stdinIsTerminal = true
	readPassword = func(int) ([]byte, error) {
		t.Fatal("bufio.Reader 已有预读数据时不应调用 readPassword")
		return nil, nil
	}
	if u := prompt(stdin, "u", ""); u != "alice" {
		t.Fatalf("u = %q", u)
	}
	if got := promptPassword(stdin, "p", ""); got != "pasted-pw" {
		t.Errorf("应读到已预读进缓冲区的粘贴密码, 实际 %q", got)
	}
}

func TestVersionString(t *testing.T) {
	got := versionString()
	for _, want := range []string{"tcp-proxy ", runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("versionString() = %q, 应包含 %q", got, want)
		}
	}
	if strings.Contains(got, "tcp-proxy (devel)") {
		t.Errorf("versionString() = %q, 不应原样输出 (devel)", got)
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

func TestPromptValid_RetriesUntilValid(t *testing.T) {
	stdinEOF = false
	r := bufio.NewReader(strings.NewReader("nonsense\n:99999\nexample.com:22\n"))
	got := promptValid(r, "x", "default:1", config.ValidateDialAddr)
	if got != "example.com:22" {
		t.Errorf("应跳过两个非法输入, 实际 %q", got)
	}
}

func TestPromptValid_StopsRetryingOnEOF(t *testing.T) {
	stdinEOF = false
	r := bufio.NewReader(strings.NewReader(""))
	done := make(chan string, 1)
	go func() { done <- promptValid(r, "x", "invalid-default", config.ValidateDialAddr) }()
	select {
	case got := <-done:
		if got != "invalid-default" {
			t.Errorf("EOF 时应原样返回默认值交给后续校验, 实际 %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stdin 结束且默认值非法时 promptValid 死循环")
	}
}

// 回归测试：旧实现每次启动都无条件重写配置文件，哪怕什么都没改——手动编辑时的排版被抹掉，
// 只读的配置文件每次启动都报一条保存失败的警告。
func TestSaveConfig_LeavesUnchangedFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// 手写的紧凑排版，还省略了取默认值的 socks_addr：config.Save 写出来的内容一定和它不同。
	const handWritten = `{"local_addr":"127.0.0.1:18080","remote_addr":"git.internal:22"}`
	if err := os.WriteFile(path, []byte(handWritten), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // WriteFile 的权限受 umask 影响，显式设一遍
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	saveConfig(path, cfg)
	if got, _ := os.ReadFile(path); string(got) != handWritten {
		t.Errorf("配置没有变化时不应重写文件, 实际内容:\n%s", got)
	}
	// 手写的文件多半是 0644，里面可能有密码：不重写也要守住 config.Save 一直保证的 0600。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("配置没有变化时也应把权限收紧到 0600, 实际 %o", perm)
		}
	}

	cfg.RemoteAddr = "db.internal:5432"
	saveConfig(path, cfg)
	if got, err := config.Load(path); err != nil || got != cfg {
		t.Errorf("配置变化后应写回文件: %+v err=%v", got, err)
	}

	missing := filepath.Join(t.TempDir(), "sub", "config.json")
	saveConfig(missing, cfg)
	if got, err := config.Load(missing); err != nil || got != cfg {
		t.Errorf("配置文件不存在时应照常新建: %+v err=%v", got, err)
	}
}

// 回归测试：Socks5 代理地址与监听地址相同时监听照样成功，但每条连接都会再向自己拨一条，
// 一条客户端连接就能耗尽文件描述符。只有确定是同一个 IP 和端口时才拦下。
func TestSocksIsSelf(t *testing.T) {
	v4 := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1080}
	v6 := &net.TCPAddr{IP: net.IPv6loopback, Port: 1080}
	wild := &net.TCPAddr{IP: net.IPv6unspecified, Port: 1080}
	cases := []struct {
		listen net.Addr
		socks  string
		want   bool
	}{
		{v4, "127.0.0.1:1080", true},
		{v4, "127.0.0.1:socks", true}, // 服务名端口，socks = 1080
		{v4, "0.0.0.0:1080", true},    // 拨号时通配地址就是本机回环
		{v4, "[::ffff:127.0.0.1]:1080", true},
		{v6, "[::1]:1080", true},
		{v6, "[::]:1080", true},
		{v4, "127.0.0.1:1081", false},
		{v4, "127.0.0.2:1080", false},
		{v4, "[::1]:1080", false}, // 另一个地址族上可以是别的进程
		{v6, "127.0.0.1:1080", false},
		{v4, "localhost:1080", false},   // 域名可能解析到别处，不猜
		{wild, "127.0.0.1:1080", false}, // 通配监听与具体地址上的另一个进程可以共存（BSD / macOS）
		{v4, "not an address", false},
		{&net.UnixAddr{Name: "/tmp/x", Net: "unix"}, "127.0.0.1:1080", false},
	}
	for _, tc := range cases {
		if got := socksIsSelf(tc.listen, tc.socks); got != tc.want {
			t.Errorf("socksIsSelf(%v, %q) = %v, 期望 %v", tc.listen, tc.socks, got, tc.want)
		}
	}
}
