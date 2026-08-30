package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// egressProxy 是浏览器**唯一的出网口**：Chromium 以 --proxy-server 指向它，页面的
// 每一个请求——首个 URL、每一次重定向、每一张图片、每一次 fetch——都从这里过。
//
// 它存在是为了关掉两个洞，而这两个洞都不是「再加一次检查」能补的：
//
//  1. **DNS rebinding**。此前 checkURL 解析一次主机名、看 IP 不是私网就放行，然后
//     **Chromium 自己再解析一次**。两次解析之间的窗口正是 rebinding 要的：同一个
//     名字第二次回 127.0.0.1，浏览器连过去，而检查早已通过。这里的做法是解析一次、
//     校验那一次的结果、再亲自拨号到刚校验过的那个 IP——没有第二次解析，就没有可
//     被替换的那一刻。
//  2. **只看第一个 URL**。页面 302 进内网、img 指向 169.254.169.254、JS fetch 一个
//     私网地址，此前一条都不经过检查。它们全都是经过这个代理的请求。
//
// 它**不做 TLS 中间人**：https 走 CONNECT，代理校验目标、拨到校验过的 IP，然后原样
// 转发字节。看不见明文，也不需要看见——要判断的是「连去哪」，不是「说了什么」。
type egressProxy struct {
	ln     net.Listener
	srv    *http.Server
	logger *slog.Logger

	// allowPrivateHosts 是部署显式打开的开发用开关：允许连回环/私网。它**只**放宽
	// 「哪些地址可以连」，不放宽拨号钉住——否则开发模式下的浏览器与被排查的那个
	// 浏览器行为不同，而那正是最不该出现分歧的地方。
	allowPrivateHosts bool

	// resolve 与 dial 是给测试用的接缝：rebinding 与重定向靠编排解析结果来复现，
	// 真机上无法稳定制造。生产路径就是 net.DefaultResolver 与 net.Dialer。
	resolve func(ctx context.Context, host string) ([]net.IP, error)
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)

	closeOnce sync.Once
	closeErr  error
}

// egressProxyConfig 是启动一个出口代理需要的全部东西。
type egressProxyConfig struct {
	AllowPrivateHosts bool
	Logger            *slog.Logger
}

// proxyDialTimeout 是代理向上游拨号的上限。它与页面加载超时无关：拨不通就该早点
// 告诉浏览器，而不是让一个标签页悬在那里。
const proxyDialTimeout = 15 * time.Second

// startEgressProxy 在回环上起一个代理并开始服务。端口随机：它只服务本机的
// Chromium，固定端口只会制造冲突。
func startEgressProxy(cfg egressProxyConfig) (*egressProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the browser egress proxy: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	p := &egressProxy{
		ln:                ln,
		logger:            logger,
		allowPrivateHosts: cfg.AllowPrivateHosts,
		resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: proxyDialTimeout}
			return d.DialContext(ctx, network, addr)
		},
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := p.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("browser egress proxy stopped", "component", "browser", "error", err)
		}
	}()
	return p, nil
}

// Addr 是代理的 host:port。
func (p *egressProxy) Addr() string { return p.ln.Addr().String() }

// URL 是 Chromium --proxy-server 要的形式。
func (p *egressProxy) URL() string { return "http://" + p.Addr() }

// Close 停掉代理。重复调用安全（关闭路径可能走多次）。
func (p *egressProxy) Close() error {
	p.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.closeErr = p.srv.Shutdown(ctx)
	})
	return p.closeErr
}

// ServeHTTP 分派两种代理请求：CONNECT 隧道（https 与 ws）与绝对 URI 的普通请求。
func (p *egressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.serveConnect(w, r)
		return
	}
	p.serveForward(w, r)
}

// checkedAddr 把 host[:port] 解析、校验，返回**要拨的那个地址**。
//
// 返回的是 ip:port 而不是 host:port，这正是钉住：调用方拿到的地址里不再有名字，
// 因此不可能被解析第二次。
func (p *egressProxy) checkedAddr(ctx context.Context, hostport, defaultPort string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, defaultPort
	}
	if host == "" {
		return "", errors.New("no host in the request")
	}
	// 字面 IP 不需要解析——也不该解析：交给解析器只会多一次可被换掉的机会。
	if literal := net.ParseIP(host); literal != nil {
		if err := p.allowIP(host, literal); err != nil {
			return "", err
		}
		return net.JoinHostPort(literal.String(), port), nil
	}
	ips, err := p.resolve(ctx, host)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("resolve %s: no addresses", host)
	}
	// 全部答案都必须可连：一个名字同时给出公网与私网地址，是 rebinding 最省事的
	// 写法，挑其中「好的那个」用等于替攻击者做了选择。
	for _, ip := range ips {
		if err := p.allowIP(host, ip); err != nil {
			return "", err
		}
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}

// allowIP 是地址策略本身：回环、私网、链路本地（含云元数据 169.254.169.254）、
// 未指定地址与多播一律拒绝，除非部署显式打开 allowPrivateHosts。
func (p *egressProxy) allowIP(host string, ip net.IP) error {
	if p.allowPrivateHosts {
		return nil
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsUnspecified(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return fmt.Errorf("host %s resolves to the non-public address %s", host, ip)
	default:
		return nil
	}
}

// refuse 把一次拒绝告诉浏览器。403 而不是静默丢弃：一个挂住的标签页看起来像网络
// 慢，而这是一条策略决定，页面上应该看得出来。
func (p *egressProxy) refuse(w http.ResponseWriter, target string, err error) {
	p.logger.Warn("browser egress refused",
		"component", "browser",
		"target", target,
		"error", err,
		"consequence", "the page sees a 403 from its own browser, not a slow network")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, "blocked by the agent browser egress policy: "+err.Error()+"\n")
}

// serveConnect 处理 https/ws 的隧道：校验目标 → 拨到校验过的 IP → 双向拷贝字节。
func (p *egressProxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	addr, err := p.checkedAddr(r.Context(), r.Host, "443")
	if err != nil {
		p.refuse(w, r.Host, err)
		return
	}
	upstream, err := p.dial(r.Context(), "tcp", addr)
	if err != nil {
		// 502 而不是 403：连不上与被策略拒绝是两回事，而 403 会让人去查策略。
		// 真机排查时我就在这上面绕了一圈——错误里写着 Forbidden，实际是拨号失败。
		p.unreachable(w, r.Host, err)
		return
	}
	defer func() { _ = upstream.Close() }()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		// 编程错误而不是运行时状况：net/http 的 ResponseWriter 一直可劫持。
		p.refuse(w, r.Host, errors.New("this server cannot hijack connections"))
		return
	}
	// 接管连接时**必须**接着用 Hijack 给的那个 bufio.ReadWriter 读，而不是裸 net.Conn。
	//
	// 客户端常常不等 200 就把 TLS ClientHello 跟在 CONNECT 后面一起发出来，那几个
	// 字节此刻已经躺在 net/http 的 bufio.Reader 里。从裸连接读，就再也拿不到它们：
	// 隧道少了握手的开头，对端一直等，最后 "unexpected EOF"——真实网络上的跨协议
	// 重定向那一跳实测到的正是这个。
	//
	// 也别去 Peek 一份「先补发」：Peek **不消费**缓冲，紧接着的 io.Copy 会把同一段
	// 字节再发一遍。我第一版就是这么写的，8KB 的早发数据一下就把重复暴露出来了。
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		p.logger.Warn("hijack proxy connection", "component", "browser", "error", err)
		return
	}
	defer func() { _ = client.Close() }()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// 两个方向都拷完才收工，而不是「谁先结束就拆」：一端读完不代表另一端已经把该发
	// 的发完，先拆会把还在路上的字节截掉。各自结束时半关闭，好让对端看到 EOF。
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, buffered)
		halfClose(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		halfClose(client)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// halfClose 关掉写方向（TCP 的 FIN），让对端读到 EOF 而不是被整条连接的关闭截断。
// 不支持半关闭的连接类型（测试里的假连接）退回整体关闭。
func halfClose(conn net.Conn) {
	if closer, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
		return
	}
	_ = conn.Close()
}

// unreachable 回 502：目标是允许的，只是这一刻连不上（DNS 通了、TCP 没通）。
//
// 与 refuse 的 403 分开，是因为两者的下一步完全不同：403 去看策略与配置，502 去看
// 网络与对端。把后者说成前者，会把人送去查一个根本没问题的地方。
func (p *egressProxy) unreachable(w http.ResponseWriter, target string, err error) {
	p.logger.Warn("browser egress could not reach the target",
		"component", "browser", "target", target, "error", err)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(w, "the agent browser could not reach "+target+": "+err.Error()+"\n")
}

// serveForward 处理明文 http：同样先校验、再钉住地址转发一次。
//
// 它自己不跟随重定向——浏览器会把 302 的下一跳作为**新的一次请求**再送进来，于是
// 每一跳都各自受检。
func (p *egressProxy) serveForward(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.Host == "" {
		p.refuse(w, r.Host, errors.New("proxy requests must carry an absolute URI"))
		return
	}
	defaultPort := "80"
	if strings.EqualFold(r.URL.Scheme, "https") {
		defaultPort = "443"
	}
	addr, err := p.checkedAddr(r.Context(), r.URL.Host, defaultPort)
	if err != nil {
		p.refuse(w, r.URL.Host, err)
		return
	}

	transport := &http.Transport{
		DisableKeepAlives: true,
		// 拨号忽略调用方给的地址，改用**已校验的那个**：钉住落到实处就是这一行。
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return p.dial(ctx, network, addr)
		},
	}
	outbound := r.Clone(r.Context())
	outbound.RequestURI = ""
	resp, err := transport.RoundTrip(outbound)
	if err != nil {
		// 同上：转发失败是连不上，不是策略拒绝。
		p.unreachable(w, r.URL.Host, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		p.logger.Warn("copy proxied response", "component", "browser", "target", r.URL.Host, "error", err)
	}
}
