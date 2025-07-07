package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/net/proxy"
)

// Config 结构体用于定义配置项，并与JSON文件对应
type Config struct {
	LocalAddr string `json:"local_addr"`
	RemoteAddr string `json:"remote_addr"`
	SocksAddr string `json:"socks_addr"`
}

// 全局变量，定义程序名称和默认配置
const appName = "go-proxy-tunnel"

var factoryDefaults = Config{
	LocalAddr: "127.0.0.1:10808",
	RemoteAddr: "example.com:80",
	SocksAddr: "127.0.0.1:1080",
}

// ---- 网络处理核心----
func handleConnection(clientConn net.Conn, remoteAddr string, dialer proxy.Dialer, wg *sync.WaitGroup) {
	// 通知 main 函数，此连接的处理已结束
	defer wg.Done()
	defer clientConn.Close()

	log.Printf("客户端 %s 已连接，准备通过代理连接到 %s", clientConn.RemoteAddr(), remoteAddr)

	remoteConn, err := dialer.Dial("tcp", remoteAddr)
	if err != nil {
		log.Printf("错误：通过代理连接到 %s 失败: %v", remoteAddr, err)
		return
	}
	defer remoteConn.Close()
	log.Printf("已通过代理成功连接到 %s", remoteAddr)

	var copyWg sync.WaitGroup
	copyWg.Add(2)

	// Goroutine 1: 从客户端复制到远程
	go func() {
		defer copyWg.Done()
		defer remoteConn.Close() // 加速另一个goroutine的退出
		if _, err := io.Copy(remoteConn, clientConn); err != nil {
			// 忽略“连接已关闭”的常规错误，只记录意外错误
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("从客户端 %s 到远程的数据流错误: %v", clientConn.RemoteAddr(), err)
			}
		}
	}()

	// Goroutine 2: 从远程复制到客户端
	go func() {
		defer copyWg.Done()
		defer clientConn.Close() // 加速另一个goroutine的退出
		if _, err := io.Copy(clientConn, remoteConn); err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Printf("从远程到客户端 %s 的数据流错误: %v", clientConn.RemoteAddr(), err)
			}
		}
	}()

	copyWg.Wait()
	log.Printf("客户端 %s 的连接已关闭", clientConn.RemoteAddr())
}

// ---- 交互输入，与之前版本相同 ----
func readInput(prompt, defaultValue string) string {
	fmt.Printf("%s [%s]: ", prompt, defaultValue)
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		log.Fatalf("错误：无法读取您的输入: %v", err)
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultValue
	}
	return input
}

// ---- 配置加载和保存的函数 ----

func getConfigPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法获取用户配置目录: %w", err)
	}
	return filepath.Join(configDir, appName, "config.json"), nil
}

func loadConfig(path string) Config {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Println("未找到配置文件，将使用默认值。")
		return factoryDefaults
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("配置文件解析失败 (%v)，将使用默认值。", err)
		return factoryDefaults
	}

	log.Println("已成功从", path, "加载配置。")
	return cfg
}

// saveConfig (已应用建议 1)
func saveConfig(path string, cfg *Config) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("错误：无法序列化配置: %v", err)
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		log.Printf("错误：无法创建配置目录 %s: %v", filepath.Dir(path), err)
		return
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Printf("错误：无法保存配置文件到 %s: %v", path, err)
	} else {
		log.Println("配置已成功保存到", path)
	}
}

// main 函数 (已应用建议 3)
func main() {
	// 1. 加载配置
	fmt.Println()
	configPath, err := getConfigPath()
	if err != nil {
		log.Fatalf("初始化配置失败: %v", err)
	}
	currentConfig := loadConfig(configPath)
	fmt.Println()

	// 2. 交互式获取配置
	fmt.Println(" -----       Go SOCKS5 TCP 代理转发工具       -----")
	fmt.Println()
	fmt.Println(" 请根据提示输入配置信息，直接按回车将使用上次保存的值。")
	fmt.Println()
	currentConfig.LocalAddr = readInput(" 请输入本地监听地址和端口", currentConfig.LocalAddr)
	currentConfig.RemoteAddr = readInput(" 请输入远程目标服务地址和端口", currentConfig.RemoteAddr)
	currentConfig.SocksAddr = readInput(" 请输入 SOCKS5 代理地址和端口", currentConfig.SocksAddr)

	// 3. 保存最终配置
	fmt.Println()
	saveConfig(configPath, &currentConfig)
	fmt.Println()
	fmt.Println(" -------------------------------------------------")
	fmt.Println()
	log.Println("配置确认，准备启动服务...")

	// 4. 创建 SOCKS5 代理拨号器
	dialer, err := proxy.SOCKS5("tcp", currentConfig.SocksAddr, nil, proxy.Direct)
	if err != nil {
		log.Fatalf("错误：无法创建 SOCKS5 代理拨号器: %v", err)
	}

	// 5. 启动监听
	listener, err := net.Listen("tcp", currentConfig.LocalAddr)
	if err != nil {
		log.Fatalf("错误：无法监听本地端口 %s: %v", currentConfig.LocalAddr, err)
	}
	// 保留 defer listener.Close() 作为一种保障，以防程序因 panic 等意外情况退出
	defer listener.Close()
	log.Printf("服务已在 %s 成功启动，现在可以连接此端口了。", currentConfig.LocalAddr)

	// 使用 WaitGroup 追踪所有活跃的连接
	var wgConnections sync.WaitGroup

	// 6. 将 listener.Accept() 放入单独的 goroutine，以实现非阻塞监听
	go func() {
		for {
			clientConn, err := listener.Accept()
			if err != nil {
				// 当 listener 被关闭时，Accept会返回错误，此时可以安全退出goroutine
				if !strings.Contains(err.Error(), "use of closed network connection") {
					log.Printf("Accept 循环遇到未知错误: %v", err)
				}
				break // 退出循环，结束此 goroutine
			}
			// 每接受一个新连接，WaitGroup 计数器加 1
			wgConnections.Add(1)
			go handleConnection(clientConn, currentConfig.RemoteAddr, dialer, &wgConnections)
		}
	}()

	// 7. 阻塞主 goroutine，直到收到系统信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// 8. 执行优雅停机流程
	fmt.Println()
	log.Println("收到关闭信号，正在停止服务...")

	// 首先，停止接受新连接。这将导致 Accept() 循环出错并退出。
	log.Println("正在停止接受新连接...")
	listener.Close()

	// 然后，等待所有已建立的连接处理完成
	log.Println("等待现有连接处理完成...")
	wgConnections.Wait()

	log.Println("所有连接均已关闭，服务成功退出。")
}