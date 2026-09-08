// Package config 负责配置的默认值、加载、校验和持久化。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
)

// AppName 是配置目录名。历史上沿用了旧的 module 名，改名会让老用户的配置"失踪"，因此保持不变。
const AppName = "go-proxy-tunnel"

// FileName 是配置文件名。
const FileName = "config.json"

// Config 是持久化到磁盘的配置。
type Config struct {
	LocalAddr  string `json:"local_addr"`
	RemoteAddr string `json:"remote_addr"`
	SocksAddr  string `json:"socks_addr"`
}

// Defaults 是出厂默认值。
var Defaults = Config{
	LocalAddr:  "127.0.0.1:10808",
	RemoteAddr: "example.com:80",
	SocksAddr:  "127.0.0.1:1080",
}

// ErrNotFound 表示配置文件不存在。
var ErrNotFound = errors.New("配置文件不存在")

// DefaultPath 返回当前用户的默认配置文件路径。
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法获取用户配置目录: %w", err)
	}
	return filepath.Join(dir, AppName, FileName), nil
}

// Load 读取 path 处的配置。文件不存在时返回 Defaults 和 ErrNotFound；
// 文件存在但缺少的字段用 Defaults 回填，避免出现空地址。
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Defaults, ErrNotFound
		}
		return Defaults, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}

	cfg := Defaults
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Defaults, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	cfg.FillDefaults()
	return cfg, nil
}

// Save 把配置写入 path，必要时创建父目录（0700）。
// 通过 os.CreateTemp（0600）写临时文件再重命名，保证原子替换且不会以过宽的权限落盘。
func Save(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录 %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // 成功 rename 之后这里会因为文件不存在而静默失败

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("保存配置文件到 %s: %w", path, err)
	}
	return nil
}

// FillDefaults 用 Defaults 回填空字段。
func (c *Config) FillDefaults() {
	if c.LocalAddr == "" {
		c.LocalAddr = Defaults.LocalAddr
	}
	if c.RemoteAddr == "" {
		c.RemoteAddr = Defaults.RemoteAddr
	}
	if c.SocksAddr == "" {
		c.SocksAddr = Defaults.SocksAddr
	}
}

// Validate 检查三个地址都是合法的 host:port。
// 本地监听地址允许 host 为空（等价于监听所有接口，例如 ":10808"），
// 但远程目标与代理地址必须有 host。
func (c Config) Validate() error {
	if err := ValidateListenAddr(c.LocalAddr); err != nil {
		return fmt.Errorf("本地监听地址无效: %w", err)
	}
	if err := ValidateDialAddr(c.RemoteAddr); err != nil {
		return fmt.Errorf("远程目标地址无效: %w", err)
	}
	if err := ValidateDialAddr(c.SocksAddr); err != nil {
		return fmt.Errorf("Socks5 代理地址无效: %w", err)
	}
	return nil
}

// ValidateListenAddr 校验监听地址；允许省略 host（如 ":10808"）。
func ValidateListenAddr(addr string) error { return validateAddr(addr, true) }

// ValidateDialAddr 校验用于主动连接的地址；host 与 port 都不能为空。
func ValidateDialAddr(addr string) error { return validateAddr(addr, false) }

func validateAddr(addr string, allowEmptyHost bool) error {
	if addr == "" {
		return errors.New("不能为空")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q 不是合法的 host:port: %w", addr, err)
	}
	if host == "" && !allowEmptyHost {
		return fmt.Errorf("%q 缺少主机名", addr)
	}
	if port == "" {
		return fmt.Errorf("%q 缺少端口", addr)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("%q 端口无效: %w", addr, err)
	}
	return nil
}
