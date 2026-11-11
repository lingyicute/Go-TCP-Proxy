// Package config 负责配置的默认值、加载、校验和持久化。
package config

import (
	"bytes"
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
	// SocksUser / SocksPassword 是 Socks5 用户名/密码认证（RFC 1929）的凭据，
	// SocksUser 为空表示代理不需要认证。两个字段都是 omitempty：
	// 不用认证时写出的配置文件和以前一模一样，老版本也能原样读回。
	SocksUser     string `json:"socks_user,omitempty"`
	SocksPassword string `json:"socks_password,omitempty"`
}

// MaxSocksCredentialLen 是 RFC 1929 允许的用户名 / 密码最大长度：
// 报文里各用 1 个字节表示长度，按字节计，不是按字符计。
const MaxSocksCredentialLen = 255

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

	// Windows 记事本、PowerShell 5 的 Set-Content -Encoding UTF8 保存时会在文件开头写入 UTF-8 BOM，
	// 而 encoding/json 不认它：手动编辑过的配置不应因此变成"解析失败"。
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	cfg := Defaults
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Defaults, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	cfg.FillDefaults()
	return cfg, nil
}

// Save 把配置写入 path，必要时创建父目录（0700）。
// 通过 os.CreateTemp（0600）写临时文件、fsync、再重命名，保证原子替换且不会以过宽的权限落盘：
// 没有 fsync 的话，rename 之后、数据真正落盘之前断电，部分文件系统会留下一个空的 config.json，
// 下次启动就会在非交互模式下因"解析配置文件失败"而拒绝启动。
func Save(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置: %w", err)
	}
	data = append(data, '\n')

	// 配置文件是符号链接时（dotfiles 仓库里常见的做法）写到它指向的文件：rename 换掉的是目录项，
	// 直接对着 path 做会把链接本身换成一个普通文件，仓库里的那一份从此不再更新。
	path = resolveSymlink(path)

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
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("同步临时文件到磁盘: %w", err)
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

// Validate 检查三个地址都是合法的 host:port，以及认证凭据是否自洽。
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
	if c.SocksUser == "" {
		if c.SocksPassword != "" {
			return errors.New("设置了 Socks5 代理密码却没有用户名")
		}
		return nil
	}
	if err := ValidateSocksUser(c.SocksUser); err != nil {
		return fmt.Errorf("Socks5 代理用户名无效: %w", err)
	}
	if err := ValidateSocksPassword(c.SocksPassword); err != nil {
		return fmt.Errorf("Socks5 代理密码无效: %w", err)
	}
	return nil
}

// resolveSymlink 解开 path 的符号链接链；即使目标文件尚未创建（悬空符号链接），
// 也顺着链接找到最终应写入的路径，避免首次保存把符号链接覆盖成普通文件。
func resolveSymlink(path string) string {
	cur := path
	for range 40 {
		if target, err := filepath.EvalSymlinks(cur); err == nil {
			return target
		}
		target, err := os.Readlink(cur)
		if err != nil {
			return cur
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		cur = filepath.Clean(target)
	}
	return path
}

// ValidateSocksUser 校验 Socks5 用户名：非空且不超过 255 字节。
func ValidateSocksUser(user string) error {
	if user == "" {
		return errors.New("不能为空")
	}
	if len(user) > MaxSocksCredentialLen {
		return fmt.Errorf("长度 %d 字节，超过 RFC 1929 允许的 %d 字节", len(user), MaxSocksCredentialLen)
	}
	return nil
}

// ValidateSocksPassword 校验 Socks5 密码：允许为空（RFC 1929 允许，x/net 也接受），但不能超过 255 字节。
func ValidateSocksPassword(password string) error {
	if len(password) > MaxSocksCredentialLen {
		return fmt.Errorf("长度 %d 字节，超过 RFC 1929 允许的 %d 字节", len(password), MaxSocksCredentialLen)
	}
	return nil
}

// ValidateListenAddr 校验监听地址；允许省略 host（如 ":10808"），也允许端口为 0（由系统随机分配）。
func ValidateListenAddr(addr string) error { return validateAddr(addr, true) }

// ValidateDialAddr 校验用于主动连接的地址；host 与 port 都不能为空，端口也不能是 0。
func ValidateDialAddr(addr string) error { return validateAddr(addr, false) }

func validateAddr(addr string, listen bool) error {
	if addr == "" {
		return errors.New("不能为空")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q 不是合法的 host:port: %w", addr, err)
	}
	if host == "" && !listen {
		return fmt.Errorf("%q 缺少主机名", addr)
	}
	if len(host) > 255 {
		return fmt.Errorf("%q 主机名长度 %d 字节，超过 255 字节上限", addr, len(host))
	}
	if port == "" {
		return fmt.Errorf("%q 缺少端口", addr)
	}
	n, err := net.LookupPort("tcp", port)
	if err != nil {
		return fmt.Errorf("%q 端口无效: %w", addr, err)
	}
	// 监听端口 0 表示让系统随机分配；但谁也连不上端口 0，用作目标/代理地址只会让
	// 之后每一条连接都失败，不如在这里就拦下来。
	if n == 0 && !listen {
		return fmt.Errorf("%q 端口不能为 0", addr)
	}
	return nil
}
