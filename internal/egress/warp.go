// WARP 出口：让引擎取源站时从 Cloudflare WARP 出网，源站看不到宿主机 IP。
//
// ## 为什么不调系统 WARP 客户端
//
// 引擎要能跑在任何普通 VPS/容器里：不装内核模块、不要 root、不要 TUN 设备。这里用
// **用户态 WireGuard**（`wireguard-go` + gvisor netstack）实现——WireGuard 跑在进程内，
// 网络栈也跑在进程内，出站 TCP 直接从 netstack 拨出去，宿主机只需要放行
// **出站 UDP 2408**（WARP 的标准端口）。
//
// ## 身份从哪来
//
// `warp-plus` 的 `LoadOrCreateIdentity` 走 Cloudflare 的注册接口（HTTPS，无需账号），
// 拿到 WARP 分配的 v4/v6 内网地址与对端公钥。身份持久化到磁盘：**复用同一身份**，
// 避免每次重启都注册新设备（接口有频控，且设备列表会越堆越长）。
//
// ## 两个容易踩的坑（都实测过）
//
//  1. 身份里的 WireGuard 密钥是 **base64**，而 `wireguard-go` 的 `IpcSet` 只认 **hex**；
//  2. `endpoint` 必须是**数字 IP:port**（新版 `netip.ParseAddr` 不认域名），而 identity
//     里的 `Endpoint.V4` 形如 `162.159.192.4:0`（带一个占位端口），要拆开再拼。
package egress

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/bepass-org/warp-plus/warp"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// warpHandshakeTimeout 等待 WireGuard 握手完成的上限。
// 握手失败通常意味着出站 UDP 被封（此时必须**启动失败**，而不是悄悄退回直连——
// 那会把宿主机 IP 暴露给源站，正好违反这个模式的目的）。
const warpHandshakeTimeout = 20 * time.Second

// WarpEgress WARP 出口（持有用户态 WireGuard 设备与 netstack 拨号器）。
type WarpEgress struct {
	// DialContext 在 WARP 内建立 TCP 连接（含 DNS 解析）。
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	device      *device.Device
}

// HTTPClient 返回一个所有出站流量都经 WARP 的 HTTP 客户端。
func (w *WarpEgress) HTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext:         w.DialContext,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
		// WARP 在用户态做 TLS 之上的 TCP，代理环境变量与 HTTP/2 复用都不适用。
		Proxy:             nil,
		ForceAttemptHTTP2: false,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// Close 关闭 WireGuard 设备。
func (w *WarpEgress) Close() {
	if w.device != nil {
		w.device.Close()
	}
}

// StartWarpEgress 注册/加载 WARP 身份并建立用户态 WireGuard 连接。
//
// @param identityDir 身份持久化目录（`warp-identity.json` 落在其下）。
// @return 握手成功的出口；失败时返回错误（调用方应据此拒绝启动）。
func StartWarpEgress(identityDir string, log *slog.Logger) (*WarpEgress, error) {
	identity, err := warp.LoadOrCreateIdentity(log, identityDir, "")
	if err != nil {
		return nil, fmt.Errorf("获取 WARP 身份失败（需要 HTTPS 出网）：%w", err)
	}
	if len(identity.Config.Peers) == 0 {
		return nil, fmt.Errorf("WARP 身份里没有对端（注册返回异常）")
	}

	privKey, err := base64KeyToHex(identity.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("WARP 私钥格式异常：%w", err)
	}
	peerKey, err := base64KeyToHex(identity.Config.Peers[0].PublicKey)
	if err != nil {
		return nil, fmt.Errorf("WARP 对端公钥格式异常：%w", err)
	}

	var addrs []netip.Addr
	if v4 := identity.Config.Interface.Addresses.V4; v4 != "" {
		if addr, err := netip.ParseAddr(strings.TrimSpace(v4)); err == nil {
			addrs = append(addrs, addr)
		}
	}
	if v6 := identity.Config.Interface.Addresses.V6; v6 != "" {
		if addr, err := netip.ParseAddr(strings.TrimSpace(v6)); err == nil {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("WARP 身份里没有可用的接口地址")
	}

	tunDevice, netstackNet, err := netstack.CreateNetTUN(
		addrs,
		[]netip.Addr{netip.MustParseAddr("1.1.1.1")},
		1280,
	)
	if err != nil {
		return nil, fmt.Errorf("创建用户态网络栈失败：%w", err)
	}

	wgDevice := device.NewDevice(tunDevice, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "wg: "))

	endpoint := identity.Config.Peers[0].Endpoint
	hostPart := strings.SplitN(endpoint.V4, ":", 2)[0]
	if hostPart == "" {
		wgDevice.Close()
		return nil, fmt.Errorf("WARP 对端地址为空")
	}

	if err := wgDevice.IpcSet(fmt.Sprintf(`private_key=%s
public_key=%s
allowed_ip=0.0.0.0/0
allowed_ip=::/0
`, privKey, peerKey)); err != nil {
		wgDevice.Close()
		return nil, fmt.Errorf("配置 WireGuard 失败：%w", err)
	}

	// 依次尝试常见 WARP 端口：不同网络放行的端口不一样（2408 最常见）。
	ports := endpoint.Ports
	if len(ports) == 0 {
		ports = []uint16{2408, 500, 1701, 4500}
	}
	var lastErr error
	for _, port := range ports {
		target := net.JoinHostPort(hostPart, strconv.Itoa(int(port)))
		if err := wgDevice.IpcSet("endpoint=" + target + "\n"); err != nil {
			lastErr = err
			continue
		}
		if err := wgDevice.Up(); err != nil {
			lastErr = err
			continue
		}
		if waitForHandshake(wgDevice, warpHandshakeTimeout) {
			log.Info("WARP 出口已就绪", "endpoint", target)
			return &WarpEgress{DialContext: netstackNet.DialContext, device: wgDevice}, nil
		}
		lastErr = fmt.Errorf("端口 %d 握手超时", port)
		log.Warn("WARP 端口握手失败，尝试下一个", "port", port)
	}

	wgDevice.Close()
	return nil, fmt.Errorf("WARP 握手失败（多半是出站 UDP 被封）：%v", lastErr)
}

// waitForHandshake 轮询 IpcGet，直到出现非零 last_handshake_time。
func waitForHandshake(wgDevice *device.Device, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := wgDevice.IpcGet()
		if err == nil {
			for _, line := range strings.Split(state, "\n") {
				if strings.HasPrefix(line, "last_handshake_time_sec=") {
					if !strings.HasSuffix(line, "=0") {
						return true
					}
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// base64KeyToHex WireGuard 密钥在 WARP identity 里是 base64，wireguard-go 只认 hex。
func base64KeyToHex(value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("密钥长度应为 32 字节，实际 %d", len(raw))
	}
	return hex.EncodeToString(raw), nil
}
