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
	"strings"
	"syscall"
	"time"

	"github.com/lingyicute/Go-TCP-Proxy/internal/config"
	"github.com/lingyicute/Go-TCP-Proxy/internal/tunnel"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

// stdin 只创建一次并在整个进程内复用：bufio.Reader 会预读多余的字节，
// 每次提问都新建 reader 会把管道里后续几行输入吞掉。
var stdin = bufio.NewReader(os.Stdin)

type options struct {
	configPath      string
	listen          string
	remote          string
	socks           string
	yes             bool
	noSave          bool
	dialTimeout     time.Duration
	shutdownTimeout time.Duration
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
	fs.BoolVar(&o.yes, "yes", false, "跳过交互式提问，直接使用配置文件/命令行参数中的值")
	fs.BoolVar(&o.yes, "y", false, "同 -yes")
	fs.BoolVar(&o.noSave, "no-save", false, "本次运行不把配置写回配置文件")
	fs.DurationVar(&o.dialTimeout, "dial-timeout", tunnel.DefaultDialTimeout, "连接代理并完成 Socks5 握手的超时时间")
	fs.DurationVar(&o.shutdownTimeout, "shutdown-timeout", tunnel.DefaultShutdownTimeout, "退出时等待现有连接结束的最长时间")
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
		fmt.Printf("tcp-proxy %s\n", version)
		return nil
	}

	// 1. 定位并加载配置
	configPath := opts.configPath
	if configPath == "" {
		if configPath, err = config.DefaultPath(); err != nil {
			return err
		}
	}
	interactive := !opts.yes && opts.listen == "" && opts.remote == "" && opts.socks == ""

	cfg, loadErr := config.Load(configPath)
	switch {
	case loadErr == nil:
		log.Printf("已从 %s 加载配置。", configPath)
	case errors.Is(loadErr, config.ErrNotFound):
		log.Println("未找到配置文件，将使用默认值。")
	case interactive:
		// 交互模式下用户会逐项确认，损坏的配置文件用默认值兜底即可。
		log.Printf("%v，将使用默认值。", loadErr)
	default:
		// 非交互模式没有人能纠正错误值，直接失败比静默使用默认值更安全。
		return loadErr
	}

	// 2. 命令行参数覆盖配置文件
	if opts.listen != "" {
		cfg.LocalAddr = opts.listen
	}
	if opts.remote != "" {
		cfg.RemoteAddr = opts.remote
	}
	if opts.socks != "" {
		cfg.SocksAddr = opts.socks
	}

	// 3. 交互式确认
	if interactive {
		printBanner()
		cfg.LocalAddr = promptAddr(stdin, " 请输入本地监听地址和端口", cfg.LocalAddr, config.ValidateListenAddr)
		cfg.RemoteAddr = promptAddr(stdin, " 请输入远程目标服务地址和端口", cfg.RemoteAddr, config.ValidateDialAddr)
		cfg.SocksAddr = promptAddr(stdin, " 请输入 Socks5 代理地址和端口", cfg.SocksAddr, config.ValidateDialAddr)
		fmt.Println()
	} else if errors.Is(loadErr, config.ErrNotFound) && opts.remote == "" {
		// 默认的 example.com:80 只是占位符，非交互模式下不应被静默采用。
		return errors.New("未找到配置文件，非交互模式下必须通过 -r 指定远程目标地址")
	}

	// 4. 校验
	if err := cfg.Validate(); err != nil {
		return err
	}

	// 5. 先启动监听，成功之后再把配置落盘——避免把错误的地址记成下次的默认值
	srv, err := tunnel.New(tunnel.Options{
		RemoteAddr:      cfg.RemoteAddr,
		SocksAddr:       cfg.SocksAddr,
		DialTimeout:     opts.dialTimeout,
		ShutdownTimeout: opts.shutdownTimeout,
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.LocalAddr)
	if err != nil {
		return fmt.Errorf("无法监听本地地址 %s: %w", cfg.LocalAddr, err)
	}
	defer ln.Close()

	if !opts.noSave {
		if err := config.Save(configPath, cfg); err != nil {
			log.Printf("警告：%v", err)
		} else {
			log.Printf("配置已保存到 %s", configPath)
		}
	}

	log.Printf("服务已启动：%s  ──▶  Socks5 %s  ──▶  %s", ln.Addr(), cfg.SocksAddr, cfg.RemoteAddr)
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

func printBanner() {
	fmt.Println()
	fmt.Println(" -----       Socks5 TCP 代理工具 By 梨       -----")
	fmt.Println()
	fmt.Println(" 请根据提示输入配置信息，直接按回车将使用方括号中的值。")
	fmt.Println()
}

// stdinEOF 记录标准输入是否已经结束：只提示一次，并且不再重复提问（否则会死循环）。
var stdinEOF bool

// prompt 打印提示并读取一行输入；空行或标准输入已结束时返回 defaultValue。
func prompt(r *bufio.Reader, label, defaultValue string) string {
	fmt.Printf("%s [%s]: ", label, defaultValue)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		log.Fatalf("错误：无法读取输入: %v", err)
	}
	if errors.Is(err, io.EOF) {
		// 最后一行缺少换行符时 ReadString 会同时返回数据和 EOF；管道/重定向结束时也会走到这里。
		fmt.Println()
		if !stdinEOF {
			stdinEOF = true
			log.Println("标准输入已结束，剩余项将使用方括号中的值（非交互场景建议改用 -y 或 -l/-r/-s 参数）。")
		}
	}
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return defaultValue
}

// promptAddr 反复提问直到输入通过 validate；标准输入结束后不再重试，交给最终的 Validate 报错。
func promptAddr(r *bufio.Reader, label, defaultValue string, validate func(string) error) string {
	for {
		v := prompt(r, label, defaultValue)
		err := validate(v)
		if err == nil || stdinEOF {
			return v
		}
		fmt.Printf(" ✗ %v，请重新输入。\n", err)
	}
}

// warnIfPublic 在监听地址不是回环地址时给出提示，避免用户无意中把入口暴露到局域网。
func warnIfPublic(addr net.Addr) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp.IP.IsLoopback() {
		return
	}
	log.Printf("提示：当前监听在非回环地址 %s，同一网络中的其它设备也可以使用此代理，请确认网络环境可信。", addr)
}
