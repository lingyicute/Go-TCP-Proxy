package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v 应合法, 实际 %v", c, err)
		}
	}

	invalid := map[string]Config{
		"empty local":        {LocalAddr: "", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080"},
		"empty remote":       {LocalAddr: "127.0.0.1:1", RemoteAddr: "", SocksAddr: "127.0.0.1:1080"},
		"empty socks":        {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: ""},
		"local without port": {LocalAddr: "127.0.0.1", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080"},
		"remote empty host":  {LocalAddr: "127.0.0.1:1", RemoteAddr: ":80", SocksAddr: "127.0.0.1:1080"},
		"socks empty host":   {LocalAddr: "127.0.0.1:1", RemoteAddr: "example.com:80", SocksAddr: ":1080"},
		"bad port":           {LocalAddr: "127.0.0.1:99999", RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080"},
		"garbage":            {LocalAddr: "127.0.0.1:1", RemoteAddr: "http://example.com", SocksAddr: "127.0.0.1:1080"},
	}
	for name, c := range invalid {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: %+v 应被拒绝", name, c)
		}
	}
}
