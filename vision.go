package persona

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// visionFetchTimeout 是一批图片下载共用的时间上限。
const visionFetchTimeout = 10 * time.Second

// newVisionClient 由注入的 HTTP 客户端派生图片下载客户端。
//
// transport 优先克隆注入客户端的 *http.Transport，否则克隆 http.DefaultTransport；
// 两种情况下都替换 DialContext 为地址限制拨号器，并清空 Proxy（否则经代理可绕过限制）。
// 不设 Client.Timeout：超时由每次请求的 ctx 控制（visionFetchTimeout）。
func newVisionClient(base *http.Client) *http.Client {
	tr := &http.Transport{}
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = t.Clone()
	}
	if base != nil {
		if t, ok := base.Transport.(*http.Transport); ok && t != nil {
			tr = t.Clone()
		}
	}
	tr.Proxy = nil
	tr.DialContext = dialVisionSafe
	return &http.Client{Transport: tr}
}

// blockedVisionAddr 判定地址是否禁止访问：回环、私网、链路本地单播/组播、
// 接口本地组播、组播、未指定、非法地址一律拒绝。
func blockedVisionAddr(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified()
}

// dialVisionSafe 解析主机名并逐个校验解析结果，只拨号到通过校验的地址。
//
// 校验发生在拨号时刻（而非 URL 解析时刻），因此重定向目标同样受限。
func dialVisionSafe(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("persona: 图片地址非法: %w", err)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("persona: 图片地址解析失败: %w", err)
	}
	if len(addrs) == 0 {
		return nil, errors.New("persona: 图片地址无解析结果")
	}
	for _, a := range addrs {
		if blockedVisionAddr(a) {
			return nil, fmt.Errorf("persona: 拒绝访问的图片地址 %s", a.Unmap())
		}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, net.JoinHostPort(addrs[0].Unmap().String(), port))
}

// dataURL 把图片字节编码为 data URL。
func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// mediaType 取 MIME 的类型部分（去掉 "; charset=..." 等参数）。
func mediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// imageFormat 归一化图片格式名：去参数与 "image/" 前缀、转小写、jpg 归一到 jpeg。
// 配置项与响应 MIME 都走这一条归一，故 jpeg/JPEG/image/jpeg/jpg 等价。
func imageFormat(v string) string {
	f := strings.ToLower(mediaType(strings.TrimSpace(v)))
	f = strings.TrimPrefix(f, "image/")
	if f == "jpg" {
		f = "jpeg"
	}
	return f
}

// formatSet 把配置中的格式名归一为集合；归一后为空时返回 nil（nil 表示不过滤）。
func formatSet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, v := range list {
		if f := imageFormat(v); f != "" {
			out[f] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// debugSkipImage 记录一张被丢弃的图片；attrs 含 reason、host，fetch_error 时含 err。
func (p *Plugin) debugSkipImage(raw, reason string, err error) {
	attrs := []any{"reason", reason, "host", hostOf(raw)}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	p.log.Debug("persona 图片跳过", attrs...)
}

// fetchImageDataURLs 按序下载图片并返回 base64 data URL；失败/超限/非图片/格式不在白名单的图丢弃，
// 保序返回其余。urls 为空时直接返回 nil（不建 ctx、不发请求）。
func (p *Plugin) fetchImageDataURLs(ctx context.Context, urls []string) []string {
	if len(urls) == 0 {
		return nil
	}
	fctx, cancel := context.WithTimeout(ctx, visionFetchTimeout)
	defer cancel()

	out := make([]string, 0, len(urls))
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p.debugSkipImage(raw, "bad_url", nil)
			continue
		}
		req, err := http.NewRequestWithContext(fctx, http.MethodGet, raw, nil)
		if err != nil {
			p.debugSkipImage(raw, "bad_url", err)
			continue
		}
		resp, err := p.imgClient.Do(req)
		if err != nil {
			p.debugSkipImage(raw, "fetch_error", err)
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, int64(p.cfg.llmVisionMaxImageBytes)+1))
		_ = resp.Body.Close()
		if rerr != nil {
			p.debugSkipImage(raw, "fetch_error", rerr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			p.debugSkipImage(raw, "status", nil)
			continue
		}
		if int64(len(data)) > int64(p.cfg.llmVisionMaxImageBytes) {
			p.debugSkipImage(raw, "too_large", nil)
			continue
		}
		mime := mediaType(resp.Header.Get("Content-Type"))
		if !strings.HasPrefix(mime, "image/") {
			mime = mediaType(http.DetectContentType(data))
		}
		if !strings.HasPrefix(mime, "image/") {
			p.debugSkipImage(raw, "not_image", nil)
			continue
		}
		if p.cfg.llmVisionAllowedFormats != nil && !p.cfg.llmVisionAllowedFormats[imageFormat(mime)] {
			p.debugSkipImage(raw, "format", nil)
			continue
		}
		out = append(out, dataURL(mime, data))
	}
	return out
}
