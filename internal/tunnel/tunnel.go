// Package tunnel 实现核心转发逻辑：接受本地 TCP 连接，经 SOCKS5 代理连接远程目标，
// 然后在两者之间做全双工搬运。
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
)

// 默认参数，可通过 Options 覆盖。
const (
	DefaultDialTimeout     = 10 * time.Second
	DefaultShutdownTimeout = 5 * time.Second

	// Accept 出错后的退避区间，与 net/http.Server.Serve 保持一致。
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = 1 * time.Second
)

// Options 描述一条隧道。
type Options struct {
	// RemoteAddr 是经由代理连接的最终目标（host:port）。
	RemoteAddr string
	// SocksAddr 是 SOCKS5 代理地址（host:port）。
	SocksAddr string
	// DialTimeout 限制"连接代理 + SOCKS5 握手"的总耗时；<= 0 使用 DefaultDialTimeout。
	DialTimeout time.Duration
	// ShutdownTimeout 是 Shutdown 等待现有连接自然结束的时长；<= 0 使用 DefaultShutdownTimeout。
	ShutdownTimeout time.Duration
	// IdleTimeout 是一条会话（两个方向合计）允许的最长无数据时间，超过后整条会话被关闭；
	// <= 0 表示不限制。
	//
	// 半关闭传播有一个固有的盲区：客户端"说完了"之后无论是半关闭还是彻底 Close，
	// 本端看到的都只是 EOF，只能把 FIN 转发给远端然后等它回话。远端若既不回复也不关闭，
	// 这条会话（连同占用的代理连接）就会一直留到远端自己超时为止。IdleTimeout 给这种
	// 情况兜底。启用后转发改走用户态缓冲区（放弃 Linux 上的 splice 零拷贝），
	// 以便在每次读写前刷新超时。
	IdleTimeout time.Duration
	// Logger 为 nil 时使用 log.Default()。
	Logger *log.Logger
}

// Server 在一个 listener 上服务多个连接。零值不可用，请通过 New 创建。
type Server struct {
	opts   Options
	logger *log.Logger
	socks  socksDialer

	// dialCtx 是所有出站拨号的父 context；强制停机时取消它，
	// 让还卡在"连接代理 / SOCKS5 握手"阶段的 goroutine 立刻结束。
	dialCtx    context.Context
	cancelDial context.CancelFunc

	mu       sync.Mutex
	sessions map[*session]struct{} // 活跃会话，Shutdown 超时后用于强制关闭
	closed   bool

	wg sync.WaitGroup
}

// session 是一条客户端连接及其对应的远程连接。
// 两端都记录下来，强制停机时才能把卡在 remote.Read 上的搬运 goroutine 也叫醒。
type session struct {
	client *net.TCPConn

	// 空闲计时，仅在启用 IdleTimeout 时使用。两个方向共用：任一方向有数据流动，整条会话就算活跃。
	//
	// lastActive 存的是"自 start 起经过的单调时钟时长"，而不是 time.Now().UnixNano() 这样的
	// 墙上时钟：墙上时钟会被 NTP 校时、休眠唤醒、手动改时间改变，往前跳会让刚刚还活跃的会话
	// 被误判为空闲，往后跳会让空闲会话多活一段时间。start 带有单调时钟读数，start.Add(d) 得到的
	// 时刻也带，time.Since 与 SetDeadline 对这类时刻用的都是单调时钟，与墙上时钟的跳变无关。
	start      time.Time
	lastActive atomic.Int64 // 自 start 起的纳秒数（单调时钟）
	idleLogged atomic.Bool  // 两个方向可能同时判定空闲，日志只记一次

	mu     sync.Mutex
	remote *net.TCPConn // 拨号成功后设置
	closed bool         // closeAll 已被调用
}

// touch 记录会话刚刚活跃过。
func (ss *session) touch() { ss.lastActive.Store(int64(time.Since(ss.start))) }

// activeAt 返回最近一次活跃的时刻（带单调时钟读数）。
func (ss *session) activeAt() time.Time { return ss.start.Add(time.Duration(ss.lastActive.Load())) }

// setRemote 记录远程连接；如果会话已经被强制关闭则返回 false，调用方应关闭 r。
func (ss *session) setRemote(r *net.TCPConn) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return false
	}
	ss.remote = r
	return true
}

// closeAll 关闭会话两端的连接。
func (ss *session) closeAll() {
	ss.mu.Lock()
	ss.closed = true
	r := ss.remote
	ss.mu.Unlock()

	ss.client.Close()
	if r != nil {
		r.Close()
	}
}

// socksDialer 是 *socks.Dialer 中我们真正需要的那部分：
// 在一条已经连到代理的连接上完成 SOCKS5 握手。
// 自己拨号可以拿到 *net.TCPConn（从而支持 CloseWrite 半关闭），
// 而 proxy.Dialer.DialContext 返回的包装类型不暴露该方法。
type socksDialer interface {
	DialWithConn(ctx context.Context, c net.Conn, network, address string) (net.Addr, error)
}

// New 校验参数并创建 Server。
func New(opts Options) (*Server, error) {
	if _, _, err := net.SplitHostPort(opts.RemoteAddr); err != nil {
		return nil, fmt.Errorf("远程目标地址无效 %q: %w", opts.RemoteAddr, err)
	}
	if _, _, err := net.SplitHostPort(opts.SocksAddr); err != nil {
		return nil, fmt.Errorf("Socks5 代理地址无效 %q: %w", opts.SocksAddr, err)
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = DefaultDialTimeout
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = DefaultShutdownTimeout
	}
	if opts.IdleTimeout < 0 {
		opts.IdleTimeout = 0
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	d, err := proxy.SOCKS5("tcp", opts.SocksAddr, nil, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("创建 Socks5 拨号器失败: %w", err)
	}
	sd, ok := d.(socksDialer)
	if !ok {
		return nil, errors.New("golang.org/x/net/proxy 返回的拨号器不支持 DialWithConn")
	}

	dialCtx, cancelDial := context.WithCancel(context.Background())
	return &Server{
		opts:       opts,
		logger:     logger,
		socks:      sd,
		dialCtx:    dialCtx,
		cancelDial: cancelDial,
		sessions:   make(map[*session]struct{}),
	}, nil
}

// Serve 在 ln 上循环 Accept，直到 ln 被关闭（通常由 Shutdown 触发）。
// 临时性错误（例如 EMFILE）只记录日志并短暂退避，不会终止循环。
// ln 被关闭后返回 nil；Serve 返回时并不代表所有连接都已结束。
func (s *Server) Serve(ln net.Listener) error {
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			if delay == 0 {
				delay = acceptRetryMin
			} else {
				delay = min(delay*2, acceptRetryMax)
			}
			s.logger.Printf("Accept 出错，%v 后重试: %v", delay, err)
			time.Sleep(delay)
			continue
		}
		delay = 0

		tcp, ok := conn.(*net.TCPConn)
		if !ok {
			// Serve 只支持 TCP listener；其他类型直接拒绝而不是崩溃。
			s.logger.Printf("忽略非 TCP 连接 %T", conn)
			conn.Close()
			continue
		}
		ss, ok := s.track(tcp)
		if !ok {
			// Shutdown 已经开始，Accept 与 Close 之间的窗口里进来的连接直接关闭。
			tcp.Close()
			continue
		}
		go func() {
			defer s.wg.Done()
			defer s.untrack(ss)
			s.handle(ss)
		}()
	}
}

// Shutdown 等待现有连接自然结束；超过 ShutdownTimeout 或 ctx 结束后，
// 强制关闭所有剩余连接。调用方应先关闭 listener。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	// closed 置位后不会再有新会话、也就不会再有新的拨号：无论走优雅还是强制路径，
	// 返回前都释放 dialCtx（强制路径下面会提前显式取消，CancelFunc 可重复调用）。
	defer s.cancelDial()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(s.opts.ShutdownTimeout)
	defer timer.Stop()

	var err error
	select {
	case <-done:
		return nil
	case <-timer.C:
		err = fmt.Errorf("等待 %v 后仍有连接未结束，已强制关闭", s.opts.ShutdownTimeout)
	case <-ctx.Done():
		err = fmt.Errorf("停机被中断，已强制关闭剩余连接: %w", ctx.Err())
	}

	s.cancelDial()
	s.mu.Lock()
	for ss := range s.sessions {
		ss.closeAll()
	}
	s.mu.Unlock()
	<-done
	return err
}

// ActiveConns 返回当前活跃的客户端连接数。
func (s *Server) ActiveConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// track 为连接创建会话、登记到活跃集合并增加 WaitGroup 计数；Shutdown 已开始时返回 false。
// 与 Shutdown 共用同一把锁，避免 wg.Add 与 wg.Wait 之间的竞态。
func (s *Server) track(c *net.TCPConn) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	ss := &session{client: c, start: time.Now()}
	s.sessions[ss] = struct{}{}
	s.wg.Add(1)
	return ss, true
}

func (s *Server) untrack(ss *session) {
	s.mu.Lock()
	delete(s.sessions, ss)
	s.mu.Unlock()
}

// handle 服务一条客户端连接：经代理连远程，然后双向搬运。
func (s *Server) handle(ss *session) {
	client := ss.client
	defer client.Close()

	peer := client.RemoteAddr()
	s.logger.Printf("客户端 %s 已连接，正在通过代理连接 %s", peer, s.opts.RemoteAddr)

	ctx, cancel := context.WithTimeout(s.dialCtx, s.opts.DialTimeout)
	remote, err := s.dial(ctx)
	cancel()
	if err != nil {
		if s.dialCtx.Err() == nil { // 因强制停机而中断的拨号不算错误
			s.logger.Printf("错误：通过代理连接 %s 失败: %v", s.opts.RemoteAddr, err)
		}
		return
	}
	defer remote.Close()
	if !ss.setRemote(remote) {
		// 拨号期间已被强制停机
		return
	}
	s.logger.Printf("客户端 %s 已通过代理连接到 %s", peer, s.opts.RemoteAddr)

	ss.touch() // 空闲计时从隧道打通开始
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.pipe(ss, remote, client, "客户端 -> 远程")
	}()
	go func() {
		defer wg.Done()
		s.pipe(ss, client, remote, "远程 -> 客户端")
	}()
	wg.Wait()

	s.logger.Printf("客户端 %s 的连接已关闭", peer)
}

// dial 自己建立到代理的 TCP 连接，再委托 x/net 完成 SOCKS5 握手。
// 整个过程受 ctx 的超时约束。
func (s *Server) dial(ctx context.Context) (*net.TCPConn, error) {
	var nd net.Dialer
	raw, err := nd.DialContext(ctx, "tcp", s.opts.SocksAddr)
	if err != nil {
		return nil, fmt.Errorf("连接代理 %s: %w", s.opts.SocksAddr, err)
	}
	tcp := raw.(*net.TCPConn)
	// DialWithConn 内部会根据 ctx 的 deadline 设置读写超时，握手结束后清除。
	if _, err := s.socks.DialWithConn(ctx, tcp, "tcp", s.opts.RemoteAddr); err != nil {
		tcp.Close()
		return nil, fmt.Errorf("SOCKS5 握手: %w", err)
	}
	return tcp, nil
}

// pipe 把 src 的数据搬到 dst，直到 src 读到 EOF 或任一端出错。
//
//   - src 正常 EOF：只关闭 dst 的写方向（发送 FIN），对端仍然可以把剩余的响应发回来。
//     这是正确的 TCP 半关闭传播，而不是粗暴地 Close 掉整条连接。
//   - 出错（对端复位、本端被强制关闭、整条会话空闲超时等）：这条隧道已经没救了，
//     直接关掉两头，让另一个方向的搬运 goroutine 也能立刻从阻塞的 Read 中退出。
func (s *Server) pipe(ss *session, dst, src *net.TCPConn, direction string) {
	var err error
	if s.opts.IdleTimeout > 0 {
		err = copyWithIdleTimeout(ss, dst, src, s.opts.IdleTimeout)
	} else {
		_, err = io.Copy(dst, src) // Linux 上 dst 是 *net.TCPConn 时走 splice 零拷贝
	}
	if err == nil {
		if err := dst.CloseWrite(); err != nil && !isBenignError(err) {
			s.logger.Printf("半关闭 %s 失败: %v", dst.RemoteAddr(), err)
		}
		return
	}
	switch {
	case errors.Is(err, errIdleTimeout):
		if ss.idleLogged.CompareAndSwap(false, true) {
			s.logger.Printf("客户端 %s 的连接已空闲超过 %v，关闭", ss.client.RemoteAddr(), s.opts.IdleTimeout)
		}
	case !isBenignError(err):
		s.logger.Printf("数据流 %s (%s) 出错: %v", direction, src.RemoteAddr(), err)
	}
	dst.Close()
	src.Close()
}

// errIdleTimeout 表示整条会话在 IdleTimeout 内没有任何数据流动。
var errIdleTimeout = errors.New("会话空闲超时")

// copyWithIdleTimeout 是 io.Copy 的带空闲超时版本，用的是 net.Conn 文档推荐的做法：
// 每次读写前把 deadline 重设为 "最近活跃时刻 + idle"。
//
// 空闲以整条会话为单位计算——两个方向任何一方搬运了数据都会刷新 ss.lastActive，
// 所以"客户端一直在上传而服务端一直沉默"这类单向活跃的连接不会被误杀；
// 某个方向的 deadline 到期时，若发现会话其实刚刚活跃过（另一方向有数据），
// 就续期重试，只有两个方向都静默超过 idle 才返回 errIdleTimeout。
func copyWithIdleTimeout(ss *session, dst, src *net.TCPConn, idle time.Duration) error {
	// stillActive 在 deadline 到期后判断是否只是本方向静默：是则续期重试，否则整条会话确实空闲。
	stillActive := func(err error) bool {
		return errors.Is(err, os.ErrDeadlineExceeded) && time.Since(ss.activeAt()) < idle
	}
	buf := make([]byte, 32*1024)
	for {
		if err := src.SetReadDeadline(ss.activeAt().Add(idle)); err != nil {
			return err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			ss.touch()
		}
		// 写端同样受 idle 约束：对端 idle 时间内一个字节都没收走，也算空闲。
		// Write 超时会返回已写入的字节数，续期后从断点继续，不会重复或丢失数据。
		for off := 0; off < n; {
			if err := dst.SetWriteDeadline(ss.activeAt().Add(idle)); err != nil {
				return err
			}
			w, werr := dst.Write(buf[off:n])
			off += w
			if w > 0 {
				ss.touch()
			}
			if werr == nil || stillActive(werr) {
				continue
			}
			if errors.Is(werr, os.ErrDeadlineExceeded) {
				return errIdleTimeout
			}
			return werr
		}
		switch {
		case rerr == nil: // 继续搬下一块
		case errors.Is(rerr, io.EOF):
			return nil
		case stillActive(rerr): // 只是本方向静默，续期后重试
		case errors.Is(rerr, os.ErrDeadlineExceeded):
			return errIdleTimeout
		default:
			return rerr
		}
	}
}

// isBenignError 判断是否为连接正常/被动关闭时产生的常规错误，这类错误不值得记录：
// 本端已关闭（net.ErrClosed）、对端复位（ECONNRESET）、对端已关闭仍写入（EPIPE）等。
func isBenignError(err error) bool {
	if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return isConnClosedErrno(errno)
	}
	return false
}
