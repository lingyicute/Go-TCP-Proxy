// tcp-proxy：把本地端口收到的每一条 TCP 连接经由 Socks5 代理送往远程目标。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/lingyicute/Go-TCP-Proxy/internal/config"
	"github.com/lingyicute/Go-TCP-Proxy/internal/tunnel"
)

// passwordEnv 是非交互场景下传递 Socks5 密码的环境变量：命令行参数会出现在 ps / 任务管理器里，
// 环境变量只有同一用户（和 root）能看到，是 PGPASSWORD、MYSQL_PWD 这类工具的惯例做法。
const passwordEnv = "TCP_PROXY_SOCKS_PASSWORD"

// version 由构建时通过 -ldflags "-X main.version=..." 注入；没有注入时 versionString 会
// 退而使用 Go 自己嵌入二进制的构建信息。
var version = "dev"

// versionString 返回 -version 打印的内容：版本号之外还带上编译它的 Go 版本和目标平台。
// 静态二进制把整个标准库打包在内，标准库的安全修复只能靠换工具链重新编译拿到，
// 所以"这个二进制是哪个 Go 编的"是排查问题和判断是否该升级时最先要知道的信息。
func versionString() string {
	v := version
	if v == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			switch {
			case bi.Main.Version != "" && bi.Main.Version != "(devel)":
				v = bi.Main.Version // go install …@version 装出来的二进制带模块版本号
			default:
				var rev, dirty string
				for _, kv := range bi.Settings {
					switch kv.Key {
					case "vcs.revision":
						rev = kv.Value
					case "vcs.modified":
						if kv.Value == "true" {
							dirty = "-dirty"
						}
					}
				}
				if len(rev) >= 7 { // 本地 go build 出来的二进制带 git 提交号
					v = "dev-" + rev[:7] + dirty
				}
			}
		}
	}
	return fmt.Sprintf("tcp-proxy %s (%s %s/%s)", v, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// stdin 只创建一次并在整个进程内复用：bufio.Reader 会预读多余的字节，
// 每次提问都新建 reader 会把管道里后续几行输入吞掉。
var stdin = bufio.NewReader(os.Stdin)

// stdinIsTerminal 决定密码提问是否关闭回显：只有真正的终端才有回显可关；
// 管道 / 重定向走和其它问题一样的按行读取。测试里会把它置为 false。
var stdinIsTerminal = term.IsTerminal(int(os.Stdin.Fd()))

type options struct {
	configPath      string
	listen          string
	remote          string
	socks           string
	socksUser       string
	socksPassword   string
	yes             bool
	noSave          bool
	dialTimeout     time.Duration
	shutdownTimeout time.Duration
	idleTimeout     time.Duration
	showVersion     bool
}

func parseFlags(args []string) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet("tcp-proxy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fs.StringVar(&o.configPath, "config", "", "配置文件路径（默认：系统用户配置目录下的 "+config.AppName+"/"+config.FileName+"）")
	fs.StringVar(&o.configPath, "c", "", "同 -config")
	fs.StringVar(&o.listen, "listen", "", "本地监听地址，例如 127.0.0.1:10808")
	fs.StringVar(&o.listen, "l", "", "同 -listen")
	fs.StringVar(&o.remote, "remote", "", "远程目标地址，例如 example.com:22")
	fs.StringVar(&o.remote, "r", "", "同 -remote")
	fs.StringVar(&o.socks, "socks", "", "Socks5 代理地址，例如 127.0.0.1:1080")
	fs.StringVar(&o.socks, "s", "", "同 -socks")
	fs.StringVar(&o.socksUser, "socks-user", "", "Socks5 代理用户名（代理需要用户名/密码认证时填写）")
	fs.StringVar(&o.socksPassword, "socks-password", "", "Socks5 代理密码；也可以用环境变量 "+passwordEnv+" 传入，避免出现在命令行里")
	fs.BoolVar(&o.yes, "yes", false, "跳过交互式提问，直接使用配置文件/命令行参数中的值")
	fs.BoolVar(&o.yes, "y", false, "同 -yes")
	fs.BoolVar(&o.noSave, "no-save", false, "本次运行不把配置写回配置文件")
	fs.DurationVar(&o.dialTimeout, "dial-timeout", tunnel.DefaultDialTimeout, "连接代理并完成 Socks5 握手的超时时间")
	fs.DurationVar(&o.shutdownTimeout, "shutdown-timeout", tunnel.DefaultShutdownTimeout, "退出时等待现有连接结束的最长时间")
	fs.DurationVar(&o.idleTimeout, "idle-timeout", 0, "一条连接两个方向都没有数据流动超过该时长就关闭它（0 表示不限制）")
	fs.BoolVar(&o.showVersion, "version", false, "显示版本号并退出")

	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "用法: tcp-proxy [参数]\n\n")
		fmt.Fprintf(w, "不带任何地址参数运行时进入交互模式，逐项询问并记住你的选择。\n")
		fmt.Fprintf(w, "给出 -l/-r/-s 任意一个或 -y 时进入非交互模式，适合脚本、systemd、Docker。\n\n")
		fmt.Fprintf(w, "参数:\n")
		fs.PrintDefaults()
		fmt.Fprintf(w, "\n示例:\n")
		fmt.Fprintf(w, "  tcp-proxy                                    # 交互式配置\n")
		fmt.Fprintf(w, "  tcp-proxy -r your-server:22                  # 其余项沿用上次的配置\n")
		fmt.Fprintf(w, "  tcp-proxy -l :10808 -r db.internal:5432 -s 127.0.0.1:1080 --no-save\n")
		fmt.Fprintf(w, "  tcp-proxy -y -socks-user alice -socks-password '...'   # 代理需要认证；密码也可放在环境变量 %s 里\n", passwordEnv)
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return nil, fmt.Errorf("多余的参数: %s", strings.Join(fs.Args(), " "))
	}
	return o, nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatalf("错误：%v", err)
	}
}

func run(args []string) error {
	opts, err := parseFlags(args)
	if err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Println(versionString())
		return nil
	}

	cfg, configPath, err := resolveConfig(opts)
	if err != nil {
		return err
	}

	// 先启动监听，成功之后再把配置落盘——避免把错误的地址记成下次的默认值
	srv, err := tunnel.New(tunnel.Options{
		RemoteAddr:      cfg.RemoteAddr,
		SocksAddr:       cfg.SocksAddr,
		SocksUser:       cfg.SocksUser,
		SocksPassword:   cfg.SocksPassword,
		DialTimeout:     opts.dialTimeout,
		ShutdownTimeout: opts.shutdownTimeout,
		IdleTimeout:     opts.idleTimeout,
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.LocalAddr)
	if err != nil {
		return fmt.Errorf("无法监听本地地址 %s: %w", cfg.LocalAddr, err)
	}
	defer ln.Close()

	if !opts.noSave && configPath != "" {
		if err := config.Save(configPath, cfg); err != nil {
			log.Printf("警告：%v", err)
		} else {
			log.Printf("配置已保存到 %s", configPath)
		}
	}

	socksDesc := cfg.SocksAddr
	if cfg.SocksUser != "" {
		socksDesc += "（用户名 " + cfg.SocksUser + "）"
	}
	log.Printf("服务已启动：%s  ──▶  Socks5 %s  ──▶  %s", ln.Addr(), socksDesc, cfg.RemoteAddr)
	warnIfPublic(ln.Addr())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	// 6. 等待退出信号；第二次信号会中断等待、立即强制关闭
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-serveErr:
		if err == nil {
			err = errors.New("监听器已关闭")
		}
		return fmt.Errorf("监听循环意外退出: %w", err)
	case sig := <-sigCh:
		fmt.Println()
		log.Printf("收到 %v，停止接受新连接；最多等待 %v 让现有连接结束（再按一次 Ctrl+C 立即退出）...",
			sig, opts.shutdownTimeout)
	}
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("%v", err)
	} else {
		log.Println("所有连接均已关闭。")
	}
	log.Println("服务已退出。")
	return nil
}

// resolveConfig 完成配置的定位、加载、命令行覆盖、交互式确认与校验，
// 返回最终配置以及监听成功后应写回的路径；路径为空表示本次没有可用的配置文件。
func resolveConfig(opts *options) (config.Config, string, error) {
	interactive := !opts.yes && opts.listen == "" && opts.remote == "" && opts.socks == ""

	// 1. 定位配置文件
	configPath := opts.configPath
	if configPath == "" {
		var err error
		if configPath, err = config.DefaultPath(); err != nil {
			// 拿不到用户配置目录（典型场景：FROM scratch 的容器、DynamicUser 的 systemd 服务，
			// 里面没有 $HOME / %AppData%）并不致命：本次运行不读也不写配置文件，
			// 地址全部来自命令行或交互输入即可。之前这里直接报错退出，
			// 导致 `-l/-r/-s --no-save` 这种完全不需要配置文件的用法也无法启动。
			log.Printf("警告：%v，本次不读写配置文件。", err)
			configPath = ""
		}
	}

	// 2. 加载
	cfg, loadErr := config.Defaults, config.ErrNotFound
	if configPath != "" {
		cfg, loadErr = config.Load(configPath)
	}
	switch {
	case loadErr == nil:
		log.Printf("已从 %s 加载配置。", configPath)
	case errors.Is(loadErr, config.ErrNotFound):
		if configPath != "" {
			log.Println("未找到配置文件，将使用默认值。")
		}
	case interactive:
		// 交互模式下用户会逐项确认，损坏的配置文件用默认值兜底即可。
		log.Printf("%v，将使用默认值。", loadErr)
	default:
		// 非交互模式没有人能纠正错误值，直接失败比静默使用默认值更安全。
		return cfg, "", loadErr
	}

	// 3. 命令行参数覆盖配置文件（密码还可以来自环境变量，优先级：参数 > 环境变量 > 配置文件）
	if opts.listen != "" {
		cfg.LocalAddr = opts.listen
	}
	if opts.remote != "" {
		cfg.RemoteAddr = opts.remote
	}
	if opts.socks != "" {
		cfg.SocksAddr = opts.socks
	}
	if opts.socksUser != "" {
		cfg.SocksUser = opts.socksUser
	}
	if opts.socksPassword == "" {
		opts.socksPassword = os.Getenv(passwordEnv)
	}
	if opts.socksPassword != "" {
		cfg.SocksPassword = opts.socksPassword
	}

	// 4. 交互式确认
	if interactive {
		printBanner()
		cfg.LocalAddr = promptValid(stdin, " 请输入本地监听地址和端口", cfg.LocalAddr, config.ValidateListenAddr)
		cfg.RemoteAddr = promptValid(stdin, " 请输入远程目标服务地址和端口", cfg.RemoteAddr, config.ValidateDialAddr)
		cfg.SocksAddr = promptValid(stdin, " 请输入 Socks5 代理地址和端口", cfg.SocksAddr, config.ValidateDialAddr)
		// 认证用一个是/否问题开头：不需要认证的多数用户只多按一次回车；
		// 回答"否"同时清掉以前保存的凭据，这也是交互模式下取消认证的方式。
		if promptYesNo(stdin, " Socks5 代理是否需要用户名/密码认证", cfg.SocksUser != "") {
			cfg.SocksUser = promptValid(stdin, " 请输入 Socks5 代理用户名", cfg.SocksUser, config.ValidateSocksUser)
			if cfg.SocksUser == "" {
				// 只有标准输入提前结束才会走到这里：明确要求认证却没有用户名，
				// 不能静默降级成"不认证"启动。
				return cfg, "", errors.New("已选择需要认证，但没有提供 Socks5 代理用户名")
			}
			cfg.SocksPassword = promptPassword(stdin, " 请输入 Socks5 代理密码", cfg.SocksPassword)
		} else {
			cfg.SocksUser, cfg.SocksPassword = "", ""
		}
		fmt.Println()
		if stdinEOF && errors.Is(loadErr, config.ErrNotFound) && cfg.RemoteAddr == config.Defaults.RemoteAddr {
			// 标准输入已经结束（没有 TTY 的容器 / 服务），又没有保存过的配置可以沿用：
			// 此时"默认值"只剩占位符 example.com:80。之前这里会静默地把流量转发到
			// example.com:80，并且把它写进配置文件变成以后的默认值。
			return cfg, "", errors.New("标准输入已结束且未找到配置文件，无法确定远程目标地址；" +
				"请通过 -r 指定（非交互场景建议使用 -y 或 -l/-r/-s 参数）")
		}
	} else if errors.Is(loadErr, config.ErrNotFound) && opts.remote == "" {
		// 默认的 example.com:80 只是占位符，非交互模式下不应被静默采用。
		return cfg, "", errors.New("未找到配置文件，非交互模式下必须通过 -r 指定远程目标地址")
	}

	// 5. 校验
	if err := cfg.Validate(); err != nil {
		return cfg, "", err
	}
	return cfg, configPath, nil
}

func printBanner() {
	fmt.Println()
	fmt.Println(" -----       Socks5 TCP 代理工具 By 梨       -----")
	fmt.Println()
	fmt.Println(" 请根据提示输入配置信息，直接按回车将使用方括号中的值。")
	fmt.Println()
}

// stdinEOF 记录标准输入是否已经结束：只提示一次，并且不再重复提问（否则会死循环）。
var stdinEOF bool

// readLine 读取一行输入，只去掉行尾的换行符。标准输入结束时（管道 / 重定向读完、没有 TTY 的容器）
// 只提示一次并置位 stdinEOF；最后一行缺少换行符时 ReadString 会同时返回数据和 EOF，数据照常返回。
func readLine(r *bufio.Reader) string {
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		log.Fatalf("错误：无法读取输入: %v", err)
	}
	if errors.Is(err, io.EOF) {
		fmt.Println()
		if !stdinEOF {
			stdinEOF = true
			log.Println("标准输入已结束，剩余项将使用方括号中的值（非交互场景建议改用 -y 或 -l/-r/-s 参数）。")
		}
	}
	return strings.TrimRight(line, "\r\n")
}

// prompt 打印提示并读取一行输入；空行或标准输入已结束时返回 defaultValue。
func prompt(r *bufio.Reader, label, defaultValue string) string {
	fmt.Printf("%s [%s]: ", label, defaultValue)
	if line := strings.TrimSpace(readLine(r)); line != "" {
		return line
	}
	return defaultValue
}

// promptValid 反复提问直到输入通过 validate；标准输入结束后不再重试，交给最终的 Validate 报错。
func promptValid(r *bufio.Reader, label, defaultValue string, validate func(string) error) string {
	for {
		v := prompt(r, label, defaultValue)
		err := validate(v)
		if err == nil || stdinEOF {
			return v
		}
		fmt.Printf(" ✗ %v，请重新输入。\n", err)
	}
}

// promptYesNo 提一个是/否问题；空行或标准输入已结束时返回 defaultYes，其它输入反复询问。
func promptYesNo(r *bufio.Reader, label string, defaultYes bool) bool {
	hint := "y/N"
	if defaultYes {
		hint = "Y/n"
	}
	for {
		fmt.Printf("%s？[%s]: ", label, hint)
		switch strings.ToLower(strings.TrimSpace(readLine(r))) {
		case "":
			return defaultYes
		case "y", "yes", "是":
			return true
		case "n", "no", "否":
			return false
		}
		if stdinEOF {
			return defaultYes
		}
		fmt.Println(" ✗ 请输入 y 或 n。")
	}
}

// promptPassword 读取密码：在终端上关闭回显；空输入沿用 saved（saved 也可能为空——空密码是合法的）。
// 密码不做 TrimSpace：首尾的空格也是密码的一部分。
func promptPassword(r *bufio.Reader, label, saved string) string {
	if saved != "" {
		fmt.Printf("%s [直接回车保留当前密码]: ", label)
	} else {
		fmt.Printf("%s: ", label)
	}
	var line string
	if stdinIsTerminal {
		// term.ReadPassword 直接按字节读文件描述符、读到换行为止，不会越过本行预读，
		// 所以和复用 bufio.Reader 的其它问题可以交替使用。
		// 关闭回显后用户敲的回车不会显示出来，读完补一个换行，后面的输出才不会接在提示符后面。
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			log.Fatalf("错误：无法读取输入: %v", err)
		}
		line = string(b)
	} else {
		line = readLine(r)
	}
	if line == "" {
		return saved
	}
	return line
}

// warnIfPublic 在监听地址不是回环地址时给出提示，避免用户无意中把入口暴露到局域网。
func warnIfPublic(addr net.Addr) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP.IsLoopback() {
		return
	}
	log.Printf("提示：当前监听在非回环地址 %s，同一网络中的其它设备也可以使用此代理，请确认网络环境可信。", addr)
}
