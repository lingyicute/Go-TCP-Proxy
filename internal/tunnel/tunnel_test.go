package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- 测试辅助：一个极简但行为正确的 SOCKS5 服务器 ----------

// socksServer 实现 RFC 1928 的 CONNECT，只支持无认证方式。
// 转发阶段正确传播半关闭，以便验证被测代码也做到了这一点。
type socksServer struct {
	ln       net.Listener
	requests atomic.Int32
	// hang 为 true 时，接受 TCP 连接后不做任何回应——用于测试握手超时。
	hang bool
}

func newSocksServer(t *testing.T) *socksServer {
	return newSocksServerWith(t, false)
}

func newSocksServerWith(t *testing.T, hang bool) *socksServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socksServer{ln: ln, hang: hang}
	t.Cleanup(func() { ln.Close() })
	go s.serve()
	return s
}

func (s *socksServer) addr() string { return s.ln.Addr().String() }

func (s *socksServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *socksServer) handle(c net.Conn) {
	defer c.Close()
	s.requests.Add(1)
	if s.hang {
		time.Sleep(time.Hour)
		return
	}
	buf := make([]byte, 262)
	// 方法协商
	if _, err := io.ReadFull(c, buf[:2]); err != nil || buf[0] != 5 {
		return
	}
	if _, err := io.ReadFull(c, buf[:int(buf[1])]); err != nil {
		return
	}
	c.Write([]byte{5, 0})
	// CONNECT 请求
	if _, err := io.ReadFull(c, buf[:4]); err != nil || buf[1] != 1 {
		return
	}
	var host string
	switch buf[3] {
	case 1:
		io.ReadFull(c, buf[:4])
		host = net.IP(buf[:4]).String()
	case 3:
		io.ReadFull(c, buf[:1])
		n := int(buf[0])
		io.ReadFull(c, buf[:n])
		host = string(buf[:n])
	case 4:
		io.ReadFull(c, buf[:16])
		host = net.IP(buf[:16]).String()
	default:
		return
	}
	io.ReadFull(c, buf[:2])
	port := binary.BigEndian.Uint16(buf[:2])

	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // connection refused
		return
	}
	defer target.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(target, c)
		target.(*net.TCPConn).CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(c, target)
		c.(*net.TCPConn).CloseWrite()
		done <- struct{}{}
	}()
	<-done
	<-done
}

// echoServer 原样回显收到的数据。
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// silentServer 读到 EOF 后既不回复也不关闭，模拟一个"挂住"的远端。
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(io.Discard, c)
				<-t.Context().Done()
				c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// sinkServer 读到客户端的 EOF 之后才回复一行汇总，模拟依赖半关闭语义的协议
// （HTTP/1.0、`nc -N`、部分 RPC 客户端等）。
func sinkServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				n, _ := io.Copy(io.Discard, c)
				fmt.Fprintf(c, "server got %d bytes\n", n)
			}()
		}
	}()
	return ln.Addr().String()
}

// startTunnel 启动被测 Server，返回本地监听地址。
func startTunnel(t *testing.T, opts Options) (*Server, net.Listener) {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = log.New(testWriter{t}, "", 0)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		ln.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return srv, ln
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func dialTCP(t *testing.T, addr string) *net.TCPConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c.(*net.TCPConn)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

// ---------- 测试 ----------

func TestNew_ValidatesAddresses(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"empty remote", Options{RemoteAddr: "", SocksAddr: "127.0.0.1:1080"}},
		{"remote without port", Options{RemoteAddr: "example.com", SocksAddr: "127.0.0.1:1080"}},
		{"empty socks", Options{RemoteAddr: "example.com:80", SocksAddr: ""}},
		{"garbage socks", Options{RemoteAddr: "example.com:80", SocksAddr: "not an address"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Fatalf("期望返回错误，实际 nil")
			}
		})
	}
	if _, err := New(Options{RemoteAddr: "example.com:80", SocksAddr: "127.0.0.1:1080"}); err != nil {
		t.Fatalf("合法参数不应报错: %v", err)
	}
}

func TestBidirectionalEcho(t *testing.T) {
	socks := newSocksServer(t)
	echo := echoServer(t)
	_, ln := startTunnel(t, Options{RemoteAddr: echo, SocksAddr: socks.addr()})

	c := dialTCP(t, ln.Addr().String())
	payload := strings.Repeat("hello, tunnel! ", 10000) // ~150 KB，超过单个 socket 缓冲区
	go func() {
		c.Write([]byte(payload))
		c.CloseWrite()
	}()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("回显数据不一致: 期望 %d 字节, 实际 %d 字节", len(payload), len(got))
	}
	if n := socks.requests.Load(); n != 1 {
		t.Fatalf("期望经过 SOCKS 服务器 1 次, 实际 %d 次", n)
	}
}

// 回归测试：旧实现在客户端半关闭后立即 Close 远程连接，导致服务端最后的响应丢失。
func TestHalfCloseIsPropagated(t *testing.T) {
	socks := newSocksServer(t)
	sink := sinkServer(t)
	_, ln := startTunnel(t, Options{RemoteAddr: sink, SocksAddr: socks.addr()})

	c := dialTCP(t, ln.Addr().String())
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil { // 客户端：我发完了，等你回复
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if want := "server got 5 bytes\n"; string(got) != want {
		t.Fatalf("半关闭后的响应丢失: 期望 %q, 实际 %q", want, got)
	}
}

// 反方向的半关闭：服务端先 FIN，客户端仍然能继续发送并被服务端读到。
func TestHalfCloseFromRemoteSide(t *testing.T) {
	socks := newSocksServer(t)

	gotFromClient := make(chan string, 1)
	ln0, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln0.Close() })
	go func() {
		c, err := ln0.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte("greeting\n"))
		c.(*net.TCPConn).CloseWrite() // 服务端：我说完了，但还在听
		b, _ := io.ReadAll(c)
		gotFromClient <- string(b)
	}()

	_, ln := startTunnel(t, Options{RemoteAddr: ln0.Addr().String(), SocksAddr: socks.addr()})
	c := dialTCP(t, ln.Addr().String())
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	greeting, err := io.ReadAll(c) // 读到 EOF，说明服务端的 FIN 被传过来了
	if err != nil || string(greeting) != "greeting\n" {
		t.Fatalf("期望收到 greeting, 实际 %q (err=%v)", greeting, err)
	}
	// 服务端已经半关闭，客户端这一侧仍应可写
	if _, err := c.Write([]byte("late reply")); err != nil {
		t.Fatalf("服务端半关闭后客户端应仍可写: %v", err)
	}
	c.Close()

	select {
	case got := <-gotFromClient:
		if got != "late reply" {
			t.Fatalf("服务端应收到 late reply, 实际 %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("服务端没有收到客户端在半关闭之后发送的数据")
	}
}

func TestDialFailureClosesClient(t *testing.T) {
	socks := newSocksServer(t)
	// 远程目标：一个已经关闭的端口
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()

	srv, ln := startTunnel(t, Options{RemoteAddr: deadAddr, SocksAddr: socks.addr()})
	c := dialTCP(t, ln.Addr().String())
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("代理连不上远程时应关闭客户端连接 (EOF), 实际 err=%v", err)
	}
	waitFor(t, "连接计数归零", func() bool { return srv.ActiveConns() == 0 })
}

// 回归测试：旧实现的拨号没有超时，SOCKS 服务器只接受 TCP 不回应时 goroutine 永久挂起。
func TestDialTimeout(t *testing.T) {
	socks := newSocksServerWith(t, true) // 只接受 TCP，不回应握手

	srv, ln := startTunnel(t, Options{
		RemoteAddr:  "example.com:80",
		SocksAddr:   socks.addr(),
		DialTimeout: 200 * time.Millisecond,
	})
	c := dialTCP(t, ln.Addr().String())
	start := time.Now()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := c.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("握手超时后应关闭客户端连接 (EOF), 实际 err=%v", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("超时用时过长: %v", elapsed)
	}
	waitFor(t, "连接计数归零", func() bool { return srv.ActiveConns() == 0 })
}

// 回归测试：旧实现在 Accept 返回任意错误时退出循环，进程从此不再服务新连接。
func TestServeSurvivesTransientAcceptErrors(t *testing.T) {
	socks := newSocksServer(t)
	echo := echoServer(t)
	srv, err := New(Options{RemoteAddr: echo, SocksAddr: socks.addr(), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fl := &flakyListener{Listener: real}
	fl.failures.Store(3)
	go srv.Serve(fl)
	t.Cleanup(func() {
		real.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	c := dialTCP(t, real.Addr().String())
	c.Write([]byte("ping"))
	c.CloseWrite()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, _ := io.ReadAll(c)
	if string(got) != "ping" {
		t.Fatalf("Accept 出现临时错误后应继续服务, 实际收到 %q", got)
	}
	if n := fl.failures.Load(); n != 0 {
		t.Fatalf("预期的临时错误没有全部被消费掉, 剩余 %d", n)
	}
}

// flakyListener 在前 failures 次 Accept 调用返回临时性错误。
// failures 在 Serve goroutine 里递减、在测试 goroutine 里读取，二者之间没有同步，用原子量。
type flakyListener struct {
	net.Listener
	failures atomic.Int32
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures.Load() > 0 {
		l.failures.Add(-1)
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: errors.New("too many open files")}
	}
	return l.Listener.Accept()
}

func TestServeReturnsWhenListenerClosed(t *testing.T) {
	socks := newSocksServer(t)
	srv, err := New(Options{RemoteAddr: "example.com:80", SocksAddr: socks.addr()})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	ln.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("listener 关闭后 Serve 应返回 nil, 实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener 关闭后 Serve 没有返回")
	}
}

func TestShutdownWaitsForActiveConnections(t *testing.T) {
	socks := newSocksServer(t)
	echo := echoServer(t)
	srv, ln := startTunnel(t, Options{RemoteAddr: echo, SocksAddr: socks.addr(), ShutdownTimeout: 5 * time.Second})

	c := dialTCP(t, ln.Addr().String())
	waitFor(t, "连接建立", func() bool { return srv.ActiveConns() == 1 })
	ln.Close()

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown(context.Background()) }()

	select {
	case <-shutdownDone:
		t.Fatal("仍有活跃连接时 Shutdown 不应立即返回")
	case <-time.After(200 * time.Millisecond):
	}

	// 连接仍然可用
	c.Write([]byte("still alive"))
	buf := make([]byte, 11)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "still alive" {
		t.Fatalf("Shutdown 等待期间现有连接应继续工作: %q err=%v", buf, err)
	}

	c.Close()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("连接自然结束后 Shutdown 应返回 nil, 实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("最后一条连接关闭后 Shutdown 没有返回")
	}
}

// 回归测试：旧实现的优雅退出没有兜底，一条空闲连接就能让进程永远无法退出。
func TestShutdownForcesCloseAfterTimeout(t *testing.T) {
	socks := newSocksServer(t)
	echo := echoServer(t)
	srv, ln := startTunnel(t, Options{RemoteAddr: echo, SocksAddr: socks.addr(), ShutdownTimeout: 300 * time.Millisecond})

	c := dialTCP(t, ln.Addr().String()) // 空闲长连接，永不主动关闭
	waitFor(t, "连接建立", func() bool { return srv.ActiveConns() == 1 })
	ln.Close()

	start := time.Now()
	err := srv.Shutdown(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("超时强制关闭时 Shutdown 应返回一个说明性错误")
	}
	if elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("Shutdown 应在 ShutdownTimeout 附近返回, 实际用时 %v", elapsed)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("强制关闭后客户端应读到 EOF, 实际 err=%v", err)
	}
	if n := srv.ActiveConns(); n != 0 {
		t.Fatalf("强制关闭后仍有 %d 条活跃连接", n)
	}
}

// 第二次 Ctrl+C 通过取消 ctx 来实现"立即退出"。
func TestShutdownHonorsContextCancellation(t *testing.T) {
	socks := newSocksServer(t)
	echo := echoServer(t)
	srv, ln := startTunnel(t, Options{RemoteAddr: echo, SocksAddr: socks.addr(), ShutdownTimeout: time.Hour})

	dialTCP(t, ln.Addr().String())
	waitFor(t, "连接建立", func() bool { return srv.ActiveConns() == 1 })
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("期望错误链中包含 context.Canceled, 实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Shutdown 没有及时返回")
	}
}

// 强制停机时，卡在 SOCKS5 握手阶段的连接也必须被打断，否则 Shutdown 会一直等到 DialTimeout。
func TestShutdownInterruptsPendingDials(t *testing.T) {
	socks := newSocksServerWith(t, true) // 只接受 TCP，不回应握手
	srv, ln := startTunnel(t, Options{
		RemoteAddr:      "example.com:80",
		SocksAddr:       socks.addr(),
		DialTimeout:     time.Hour,
		ShutdownTimeout: 200 * time.Millisecond,
	})
	dialTCP(t, ln.Addr().String())
	waitFor(t, "连接进入握手阶段", func() bool { return socks.requests.Load() == 1 })
	ln.Close()

	start := time.Now()
	srv.Shutdown(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Shutdown 应打断进行中的拨号, 实际用时 %v", elapsed)
	}
	if n := srv.ActiveConns(); n != 0 {
		t.Fatalf("强制关闭后仍有 %d 条活跃连接", n)
	}
}

// 远端在收到 FIN 后一直沉默：强制停机必须把两端都关掉，否则"远程 -> 客户端"方向会永远卡在 Read。
func TestShutdownForcesCloseWhenRemoteIsSilent(t *testing.T) {
	socks := newSocksServer(t)
	silent := silentServer(t)
	srv, ln := startTunnel(t, Options{RemoteAddr: silent, SocksAddr: socks.addr(), ShutdownTimeout: 200 * time.Millisecond})

	c := dialTCP(t, ln.Addr().String())
	c.Write([]byte("hello"))
	c.CloseWrite() // 客户端说完了；远端既不回复也不关闭
	waitFor(t, "连接建立", func() bool { return srv.ActiveConns() == 1 })
	ln.Close()

	start := time.Now()
	srv.Shutdown(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("远端沉默时 Shutdown 应在超时后强制结束, 实际用时 %v", elapsed)
	}
	if n := srv.ActiveConns(); n != 0 {
		t.Fatalf("强制关闭后仍有 %d 条活跃连接", n)
	}
}

// 客户端异常断开（RST）时，即使远端沉默，会话也应被清理，而不是泄漏 goroutine。
func TestClientResetTearsDownSession(t *testing.T) {
	socks := newSocksServer(t)
	silent := silentServer(t)
	srv, ln := startTunnel(t, Options{RemoteAddr: silent, SocksAddr: socks.addr()})

	c := dialTCP(t, ln.Addr().String())
	c.Write([]byte("hello"))
	waitFor(t, "连接建立", func() bool { return srv.ActiveConns() == 1 })
	c.SetLinger(0) // 让 Close 发送 RST 而不是 FIN
	c.Close()
	waitFor(t, "会话被清理", func() bool { return srv.ActiveConns() == 0 })
}

func TestConnectionsAcceptedDuringShutdownAreRejected(t *testing.T) {
	socks := newSocksServer(t)
	echo := echoServer(t)
	srv, ln := startTunnel(t, Options{RemoteAddr: echo, SocksAddr: socks.addr(), ShutdownTimeout: time.Second})

	// 先标记 closed（不关 listener，模拟 Accept 与 Shutdown 的竞争窗口）
	srv.mu.Lock()
	srv.closed = true
	srv.mu.Unlock()

	c := dialTCP(t, ln.Addr().String())
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("停机期间新连接应被直接关闭, 实际 err=%v", err)
	}
	if n := srv.ActiveConns(); n != 0 {
		t.Fatalf("被拒绝的连接不应计入活跃连接, 实际 %d", n)
	}
}

func TestIsBenignError(t *testing.T) {
	benign := []error{
		nil,
		io.EOF,
		net.ErrClosed,
		&net.OpError{Op: "read", Err: net.ErrClosed},
		fmt.Errorf("wrapped: %w", net.ErrClosed),
	}
	for _, err := range benign {
		if !isBenignError(err) {
			t.Errorf("%v 应被视为常规错误", err)
		}
	}
	notBenign := []error{
		errors.New("something unexpected"),
		&net.OpError{Op: "read", Err: errors.New("i/o timeout")},
	}
	for _, err := range notBenign {
		if isBenignError(err) {
			t.Errorf("%v 不应被视为常规错误", err)
		}
	}
}

// ---------- IdleTimeout ----------

// 半关闭传播的固有盲区：客户端正常 Close()（FIN，而非 RST）之后远端一直沉默，
// 本端只能等远端回话或关闭。不启用 IdleTimeout 时这条会话会一直留着（由远端自己的
// 超时兜底），启用后应在 idle 附近被整体关闭。
func TestIdleTimeoutReapsSessionAfterClientCloses(t *testing.T) {
	socks := newSocksServer(t)
	silent := silentServer(t)
	srv, ln := startTunnel(t, Options{RemoteAddr: silent, SocksAddr: socks.addr(), IdleTimeout: 300 * time.Millisecond})

	c := dialTCP(t, ln.Addr().String())
	// 计时从最后一次有数据之前开始：会话最早也要到"hello 被搬运的时刻 + idle"才会被回收，
	// 因此 elapsed 不可能小于 idle——这样断言不会因为 CI 机器慢而误报。
	start := time.Now()
	c.Write([]byte("hello"))
	waitFor(t, "连接建立", func() bool { return srv.ActiveConns() == 1 })
	c.Close() // 正常 FIN；远端既不回复也不关闭

	waitFor(t, "空闲会话被回收", func() bool { return srv.ActiveConns() == 0 })
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("会话在 %v 后就被关闭，早于 IdleTimeout：FIN 本身不应触发关闭", elapsed)
	}
}

// 不关闭、只是不说话的客户端同样应被回收，并且收到 EOF。
func TestIdleTimeoutClosesQuietConnection(t *testing.T) {
	socks := newSocksServer(t)
	silent := silentServer(t)
	srv, ln := startTunnel(t, Options{RemoteAddr: silent, SocksAddr: socks.addr(), IdleTimeout: 300 * time.Millisecond})

	c := dialTCP(t, ln.Addr().String())
	c.Write([]byte("hello"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("空闲超时后客户端应读到 EOF, 实际 err=%v", err)
	}
	waitFor(t, "连接计数归零", func() bool { return srv.ActiveConns() == 0 })
}

// 空闲必须按整条会话计算：客户端持续上传、服务端始终沉默时，"远程 -> 客户端"方向
// 虽然一直没有数据，也不能把会话判成空闲。
func TestIdleTimeoutIsPerSessionNotPerDirection(t *testing.T) {
	socks := newSocksServer(t)
	silent := silentServer(t)
	// idle 取写入间隔的 10 倍：-race 下的 CI 机器偶尔会把一次 50ms 的 Sleep 拖到一两百毫秒，
	// 留足余量，避免把调度抖动误报成"被误杀"。
	srv, ln := startTunnel(t, Options{RemoteAddr: silent, SocksAddr: socks.addr(), IdleTimeout: 500 * time.Millisecond})

	c := dialTCP(t, ln.Addr().String())
	waitFor(t, "连接建立", func() bool { return srv.ActiveConns() == 1 })
	// 持续 1.2s、每 50ms 写一次：远超 IdleTimeout，但会话始终有数据流动
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := c.Write([]byte("tick")); err != nil {
			t.Fatalf("单向活跃的连接被误判为空闲: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := srv.ActiveConns(); n != 1 {
		t.Fatalf("单向活跃期间会话应保持, 实际 ActiveConns=%d", n)
	}
	// 停止写入后才应被回收
	waitFor(t, "停止活动后会话被回收", func() bool { return srv.ActiveConns() == 0 })
}

// 启用 IdleTimeout 后转发不再走 io.Copy，半关闭传播和大流量搬运必须保持正确。
func TestIdleTimeoutPreservesHalfCloseAndData(t *testing.T) {
	socks := newSocksServer(t)

	t.Run("half-close", func(t *testing.T) {
		sink := sinkServer(t)
		_, ln := startTunnel(t, Options{RemoteAddr: sink, SocksAddr: socks.addr(), IdleTimeout: time.Second})
		c := dialTCP(t, ln.Addr().String())
		c.Write([]byte("hello"))
		c.CloseWrite()
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		got, err := io.ReadAll(c)
		if err != nil || string(got) != "server got 5 bytes\n" {
			t.Fatalf("半关闭后的响应丢失: %q err=%v", got, err)
		}
	})

	t.Run("echo-150KB", func(t *testing.T) {
		echo := echoServer(t)
		_, ln := startTunnel(t, Options{RemoteAddr: echo, SocksAddr: socks.addr(), IdleTimeout: time.Second})
		c := dialTCP(t, ln.Addr().String())
		payload := strings.Repeat("hello, tunnel! ", 10000)
		go func() {
			c.Write([]byte(payload))
			c.CloseWrite()
		}()
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		got, err := io.ReadAll(c)
		if err != nil || string(got) != payload {
			t.Fatalf("回显数据不一致: 期望 %d 字节, 实际 %d 字节, err=%v", len(payload), len(got), err)
		}
	})
}

// floodServer 连接建立后不停地写，直到连接被关闭，模拟一个只管往外推数据的远端。
func floodServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				context.AfterFunc(t.Context(), func() { c.Close() })
				buf := make([]byte, 64<<10)
				for {
					if _, err := c.Write(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// 对端一直不收数据（接收窗口满）也算空闲：写端同样受 IdleTimeout 约束。
//
// 这个场景必须由写端的 deadline 兜底：客户端先半关闭，"客户端 -> 远程"方向正常结束退出，
// 之后整条会话只剩"远程 -> 客户端"这一个 goroutine，而它卡在 Write 上——没有任何一方在
// Read，读端的 deadline 帮不上忙。（如果客户端不半关闭，"客户端 -> 远程"方向的读超时会先
// 把会话收掉，测不到写端。）远端用无限流而不是固定大小的数据，是为了保证 Write 一定会
// 因为缓冲区满而阻塞：loopback 上内核的收发缓冲区能吞下好几 MB。
func TestIdleTimeoutCoversStalledWrites(t *testing.T) {
	socks := newSocksServer(t)
	flood := floodServer(t)
	srv, ln := startTunnel(t, Options{RemoteAddr: flood, SocksAddr: socks.addr(), IdleTimeout: 300 * time.Millisecond})

	c := dialTCP(t, ln.Addr().String())
	waitFor(t, "连接建立", func() bool { return srv.ActiveConns() == 1 })
	if err := c.CloseWrite(); err != nil { // 客户端说完了；之后一个字节都不读
		t.Fatal(err)
	}
	// 写超时后若有部分写入会续期一次，所以最坏要 2 个 idle 才会判定空闲；waitFor 给了 3s。
	waitFor(t, "写端卡住的会话被回收", func() bool { return srv.ActiveConns() == 0 })
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.Copy(io.Discard, c); err != nil && !isBenignError(err) {
		t.Fatalf("会话被回收后客户端应读到 EOF 或连接复位, 实际 %v", err)
	}
}

// 空闲计时必须用单调时钟：墙上时钟（time.Now().UnixNano()、time.Unix(0, n)）会随 NTP 校时、
// 休眠唤醒、手动改时间而跳变。带单调读数的 time.Time 在 String() 里会多出 " m=±…" 一段，
// 这是标准库暴露该信息的唯一途径。
func TestSessionActiveAtIsMonotonic(t *testing.T) {
	ss := &session{start: time.Now()}
	ss.touch()
	if got := ss.activeAt().String(); !strings.Contains(got, " m=") {
		t.Fatalf("activeAt() 应带单调时钟读数, 实际 %s", got)
	}
	if d := time.Since(ss.activeAt()); d < 0 || d > time.Second {
		t.Fatalf("activeAt() 应约等于现在, 实际相差 %v", d)
	}
}
