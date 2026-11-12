package config

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestLoad_NotFound(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound, 实际 %v", err)
	}
	if cfg != Defaults {
		t.Fatalf("文件不存在时应返回 Defaults, 实际 %+v", cfg)
	}
}

func TestLoad_Malformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	cfg, err := Load(path)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("损坏的 JSON 应返回解析错误, 实际 %v", err)
	}
	if cfg != Defaults {
		t.Fatalf("解析失败时应返回 Defaults, 实际 %+v", cfg)
	}
}

// 回归测试：Windows 记事本 / PowerShell 5 保存的 UTF-8 文件带 BOM，旧实现会报解析失败。
func TestLoad_AcceptsUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte("\xef\xbb\xbf"+`{"remote_addr":"db.internal:5432"}`), 0o600)
	cfg, err := Load(path)
	if err != nil || cfg.RemoteAddr != "db.internal:5432" {
		t.Fatalf("带 BOM 的配置应正常加载: cfg=%+v err=%v", cfg, err)
	}
}

// PowerShell 5 的 > / Out-File 默认写出带 BOM 的 UTF-16LE；BE 也一并支持。
func TestLoad_AcceptsUTF16BOM(t *testing.T) {
	text := `{"remote_addr":"db.internal:5432","socks_user":"alice","socks_password":"密码"}`
	units := utf16.Encode([]rune(text))
	for _, tc := range []struct {
		name  string
		bom   []byte
		order binary.AppendByteOrder
	}{
		{"LE", []byte{0xff, 0xfe}, binary.LittleEndian},
		{"BE", []byte{0xfe, 0xff}, binary.BigEndian},
	} {
		data := tc.bom
		for _, u := range units {
			data = tc.order.AppendUint16(data, u)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(path, data, 0o600)
		cfg, err := Load(path)
		if err != nil || cfg.RemoteAddr != "db.internal:5432" || cfg.SocksPassword != "密码" {
			t.Errorf("UTF-16%s 的配置应正常加载: cfg=%+v err=%v", tc.name, cfg, err)
		}
	}
}

// 回归测试：旧实现对缺字段的配置不做回填，空的 local_addr 会让程序监听 0.0.0.0 的随机端口。
func TestLoad_FillsMissingFieldsWithDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"remote_addr": "db.internal:5432"}`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RemoteAddr != "db.internal:5432" {
		t.Errorf("remote_addr 应保留文件中的值, 实际 %q", cfg.RemoteAddr)
	}
	if cfg.LocalAddr != Defaults.LocalAddr {
		t.Errorf("缺失的 local_addr 应回填默认值 %q, 实际 %q", Defaults.LocalAddr, cfg.LocalAddr)
	}
	if cfg.SocksAddr != Defaults.SocksAddr {
		t.Errorf("缺失的 socks_addr 应回填默认值 %q, 实际 %q", Defaults.SocksAddr, cfg.SocksAddr)
	}
}

func TestLoad_EmptyStringFieldsAreFilled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"local_addr": "", "remote_addr": "", "socks_addr": ""}`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != Defaults {
		t.Fatalf("空字符串字段应回填默认值, 实际 %+v", cfg)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "dir", "config.json") // 父目录不存在，Save 应创建
	want := Config{LocalAddr: ":10808", RemoteAddr: "example.org:22", SocksAddr: "10.0.0.1:1080"}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("往返不一致: 期望 %+v, 实际 %+v", want, got)
	}

	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("配置文件权限应为 0600, 实际 %o", perm)
		}
		dinfo, _ := os.Stat(filepath.Dir(path))
		if perm := dinfo.Mode().Perm(); perm != 0o700 {
			t.Errorf("配置目录权限应为 0700, 实际 %o", perm)
		}
	}

	// 没有残留的临时文件
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("残留临时文件 %s", e.Name())
		}
	}
}

// 不需要认证时，写出的文件不应出现 socks_user / socks_password 字段（与旧版本保持一致）；
// 需要认证时两个字段都要能原样往返。
func TestSaveAndLoad_SocksCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, Defaults); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "socks_user") || strings.Contains(string(data), "socks_password") {
		t.Errorf("不使用认证时不应写出凭据字段:\n%s", data)
	}

	want := Defaults
	want.SocksUser, want.SocksPassword = "alice", "p@ss word 密码"
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("凭据往返不一致: 期望 %+v, 实际 %+v", want, got)
	}

	// 老版本写出的文件（没有凭据字段）读进来就是"不认证"
	os.WriteFile(path, []byte(`{"local_addr":"127.0.0.1:1","remote_addr":"a:2","socks_addr":"b:3"}`), 0o600)
	got, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SocksUser != "" || got.SocksPassword != "" {
		t.Errorf("旧格式配置不应带出凭据: %+v", got)
	}
}

func TestSave_OverwritesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, Defaults); err != nil {
		t.Fatal(err)
	}
	want := Config{LocalAddr: "127.0.0.1:1", RemoteAddr: "a:2", SocksAddr: "b:3"}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, _ := Load(path)
	if got != want {
		t.Fatalf("覆盖保存失败: 期望 %+v, 实际 %+v", want, got)
	}
}

func TestValidate(t *testing.T) {
	valid := []Config{
		Defaults,
		{LocalAddr: ":10808", RemoteAddr: "example.com:22", SocksAddr: "127.0.0.1:1080"},
		{LocalAddr: "0.0.0.0:10808", RemoteAddr: "[::1]:22", SocksAddr: "proxy.local:1080"},
		{LocalAddr: "127.0.0.1:http", RemoteAddr: "example.com:ssh", SocksAddr: "127.0.0.1:socks"},
		{LocalAddr: "127.0.0.1:0", RemoteAddr: "example.com:22", SocksAddr: "127.0.0.1:1080"}, // 监听端口 0 = 随机端口，合法
		// IPv6 zone 是网卡名，可以带空格
		{LocalAddr: "[fe80::1%Ethernet 2]:1", RemoteAddr: "example.com:22", SocksAddr: "[fe80::2%以太网 2]:1080"},
		{LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:22", SocksAddr: "127.0.0.1:1080", SocksUser: "alice", SocksPassword: "secret"},
		{LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:22", SocksAddr: "127.0.0.1:1080", SocksUser: "alice"}, // 空密码合法
		{LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:22", SocksAddr: "127.0.0.1:1080", SocksUser: strings.Repeat("u", 255), SocksPassword: strings.Repeat("p", 255)},
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v 应合法, 实际 %v", c, err)
		}
	}

	invalid := map[string]Config{
		"empty local":           {LocalAddr: "", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080"},
		"empty remote":          {LocalAddr: "127.0.0.1:1", RemoteAddr: "", SocksAddr: "127.0.0.1:1080"},
		"empty socks":           {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: ""},
		"local without port":    {LocalAddr: "127.0.0.1", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080"},
		"remote empty host":     {LocalAddr: "127.0.0.1:1", RemoteAddr: ":80", SocksAddr: "127.0.0.1:1080"},
		"socks empty host":      {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: ":1080"},
		"bad port":              {LocalAddr: "127.0.0.1:99999", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080"},
		"remote port 0":         {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:0", SocksAddr: "127.0.0.1:1080"},
		"socks port 0":          {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:0"},
		"garbage":               {LocalAddr: "127.0.0.1:1", RemoteAddr: "http://example.com", SocksAddr: "127.0.0.1:1080"},
		"password without user": {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080", SocksPassword: "secret"},
		"local host with space": {LocalAddr: "127.0.0.1 :1", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080"},
		"remote host has space": {LocalAddr: "127.0.0.1:1", RemoteAddr: " example.com:80", SocksAddr: "127.0.0.1:1080"},
		"socks host with tab":   {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: "proxy\t.local:1080"},
		"remote ipv6 with zone": {LocalAddr: "127.0.0.1:1", RemoteAddr: "[fe80::1%eth0]:22", SocksAddr: "127.0.0.1:1080"},
		"socks empty zone":      {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: "[fe80::1%]:1080"},
		"socks ipv4 with zone":  {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1%lo:1080"},
		"socks domain percent":  {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: "example.com% 2:1080"},
		"remote host too long":  {LocalAddr: "127.0.0.1:1", RemoteAddr: strings.Repeat("a", 256) + ":80", SocksAddr: "127.0.0.1:1080"},
		"socks host too long":   {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: strings.Repeat("a", 256) + ":1080"},
		"user too long":         {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080", SocksUser: strings.Repeat("u", 256)},
		"user too long utf8":    {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080", SocksUser: strings.Repeat("密", 86)}, // 86 个汉字 = 258 字节
		"password too long":     {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080", SocksUser: "alice", SocksPassword: strings.Repeat("p", 256)},
	}
	for name, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: %+v 应被拒绝", name, c)
		}
	}
}

// 回归测试：Save 用 rename 做原子替换，旧实现直接对着传入的路径做，配置文件是符号链接时
// 链接本身会被换成普通文件，它原本指向的那一份（例如 dotfiles 仓库里的）不再更新。
// 目标文件尚未创建（悬空符号链接）时同样应写进链接指向的位置。
func TestSave_WritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "config.json") // 父目录和目标文件都还不存在
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(filepath.Join("dotfiles", "config.json"), link); err != nil {
		t.Skipf("当前环境无法创建符号链接: %v", err)
	}

	if err := Save(link, Defaults); err != nil {
		t.Fatal(err)
	}
	want := Defaults
	want.RemoteAddr = "git.internal:22"
	if err := Save(link, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("保存后配置文件应仍是符号链接, 实际 mode=%v", info.Mode())
	}
	if got, err := Load(target); err != nil || got != want {
		t.Errorf("新配置应写进链接指向的文件: %+v err=%v", got, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 1 {
		t.Errorf("链接指向的目录里不应留下临时文件, 实际 %d 个条目", len(entries))
	}

	// 父目录本身是悬空符号链接（例如把整个 ~/.config/go-proxy-tunnel 链接到尚未创建的 dotfiles 子目录）
	danglingDir := filepath.Join(dir, "linked-dir")
	realDir := filepath.Join(dir, "repo", "deep", "tunnel")
	if err := os.Symlink(filepath.Join("repo", "deep", "tunnel"), danglingDir); err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(danglingDir, FileName), want); err != nil {
		t.Fatalf("父目录是悬空符号链接时 Save 不应失败: %v", err)
	}
	if got, err := Load(filepath.Join(realDir, FileName)); err != nil || got != want {
		t.Errorf("应写进目录链接指向的位置: %+v err=%v", got, err)
	}

	// 目录符号链接内部的相对路径悬空文件符号链接：.. 应相对于真实目录而非链接名解析
	relTarget := filepath.Join(dir, "repo", "deep", "shared.json")
	relLink := filepath.Join(realDir, "rel.json")
	if err := os.Symlink(filepath.Join("..", "shared.json"), relLink); err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(danglingDir, "rel.json"), want); err != nil {
		t.Fatalf("通过目录符号链接写入相对文件符号链接失败: %v", err)
	}
	if got, err := Load(relTarget); err != nil || got != want {
		t.Errorf("相对符号链接应相对于真实父目录解析: %+v err=%v", got, err)
	}
}

// 配置文件是给人看、给人改的：& < > 不能被转义成 \u0026 之类，否则手动核对密码时认不出来。
func TestSave_DoesNotEscapeHTMLCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := Defaults
	want.SocksUser, want.SocksPassword = "alice", "a&b<c>d"
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"a&b<c>d"`) || !strings.HasSuffix(string(data), "}\n") {
		t.Errorf("配置文件里的字符被转义了或缺少结尾换行:\n%s", data)
	}
	if got, err := Load(path); err != nil || got != want {
		t.Fatalf("往返不一致: 期望 %+v, 实际 %+v, err=%v", want, got, err)
	}
}
