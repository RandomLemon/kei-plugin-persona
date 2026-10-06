package persona

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestBlockedVisionAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.1.1", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "::ffff:127.0.0.1",
	}
	for _, s := range blocked {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", s, err)
		}
		if !blockedVisionAddr(a) {
			t.Errorf("blockedVisionAddr(%s) = false, want true", s)
		}
	}
	if !blockedVisionAddr(netip.Addr{}) {
		t.Error("blockedVisionAddr(zero) = false, want true")
	}
	for _, s := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", s, err)
		}
		if blockedVisionAddr(a) {
			t.Errorf("blockedVisionAddr(%s) = true, want false", s)
		}
	}
}

func TestDialVisionSafeRejectsLoopback(t *testing.T) {
	if _, err := dialVisionSafe(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("回环地址应被拒绝")
	} else if !strings.Contains(err.Error(), "拒绝访问的图片地址") {
		t.Fatalf("错误文案 = %v", err)
	}
	if _, err := dialVisionSafe(context.Background(), "tcp", "no-such-host.invalid:80"); err == nil {
		t.Fatal("解析失败应返回错误")
	}
}

func TestFetchImageDataURLs(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("vision-payload")...)
	gifBytes := append([]byte("GIF89a"), []byte("vision-payload")...)
	const limit = 4096
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		case "/ok.gif":
			w.Header().Set("Content-Type", "image/gif")
			_, _ = w.Write(gifBytes)
		case "/nomime.png":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(png)
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>nope</html>"))
		case "/big":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(make([]byte, limit+1))
		case "/bad":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	env := newTestEnv(t, nil, func(c map[string]any) { c["llm_vision_max_image_bytes"] = limit })
	env.p.imgClient = srv.Client() // 绕过地址限制：httptest 监听回环

	decode := func(t *testing.T, urlStr string) []byte {
		t.Helper()
		const prefix = "data:image/png;base64,"
		if !strings.HasPrefix(urlStr, prefix) {
			t.Fatalf("data URL = %q, want 前缀 %q", urlStr, prefix)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(urlStr, prefix))
		if err != nil {
			t.Fatalf("base64 解码失败: %v", err)
		}
		return raw
	}

	urls := []string{
		srv.URL + "/ok.png",
		srv.URL + "/nomime.png",
		srv.URL + "/html",
		srv.URL + "/big",
		srv.URL + "/bad",
		"http://127.0.0.1:1/x.jpg", // 连接失败
		srv.URL + "/ok.png",        // 末尾再放一张成功的，验证失败项被跳过而非截断
		srv.URL + "/ok.gif",        // 缺省白名单 = 不过滤任何已识别格式
	}
	got := env.p.fetchImageDataURLs(context.Background(), urls)
	if len(got) != len(urls) {
		t.Fatalf("结果应与入参等长（%d），got %d: %#v", len(urls), len(got), got)
	}
	if raw := decode(t, got[0]); string(raw) != string(png) {
		t.Fatalf("got[0] 解码 = %q, want %q", raw, png)
	}
	if raw := decode(t, got[1]); string(raw) != string(png) {
		t.Fatalf("got[1]（无 image/* Content-Type 但魔数为 PNG）解码 = %q", raw)
	}
	if raw := decode(t, got[6]); string(raw) != string(png) {
		t.Fatalf("got[6]（跳过失败项后仍保位）解码 = %q, want %q", raw, png)
	}
	if !strings.HasPrefix(got[7], "data:image/gif;base64,") {
		t.Fatalf("got[7] = %q, want image/gif data URL", got[7])
	}
	for _, i := range []int{2, 3, 4, 5} {
		if got[i] != "" {
			t.Errorf("got[%d] 应为空串（失败项留位）: %q", i, got[i])
		}
	}

	for _, reason := range []string{"not_image", "too_large", "status", "fetch_error"} {
		if !env.cap.hasAttr("reason", reason) {
			t.Errorf("缺少 reason=%s 的丢弃日志", reason)
		}
	}
}

func TestFetchImageDataURLsFormatWhitelist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(append([]byte("\x89PNG\r\n\x1a\n"), []byte("p")...))
		case "/b.jpeg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("\xff\xd8\xff\xe0jpeg"))
		case "/c.gif":
			w.Header().Set("Content-Type", "image/gif")
			_, _ = w.Write([]byte("GIF89agif"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	env := newTestEnv(t, nil, func(c map[string]any) { c["llm_vision_allowed_formats"] = "PNG,image/gif" })
	env.p.imgClient = srv.Client()

	urls := []string{
		srv.URL + "/a.png", srv.URL + "/b.jpeg", srv.URL + "/c.gif",
	}
	got := env.p.fetchImageDataURLs(context.Background(), urls)
	if len(got) != len(urls) {
		t.Fatalf("结果应与入参等长（%d），got %d: %#v", len(urls), len(got), got)
	}
	if !strings.HasPrefix(got[0], "data:image/png;base64,") {
		t.Errorf("got[0] = %q, want image/png data URL", got[0])
	}
	if got[1] != "" {
		t.Errorf("got[1]（白名单外）应为空串: %q", got[1])
	}
	if !strings.HasPrefix(got[2], "data:image/gif;base64,") {
		t.Errorf("got[2] = %q, want image/gif data URL", got[2])
	}
	if !env.cap.hasAttr("reason", "format") {
		t.Error("缺少 reason=format 的丢弃日志")
	}
}

// TestFetchVisionSlotsWritesBack 锁定槽位下载：等长写回，失败槽位保持空串。
func TestFetchVisionSlotsWritesBack(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("vision-payload")...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok.png" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	env := newTestEnv(t, nil, nil)
	env.p.imgClient = srv.Client() // 绕过地址限制：httptest 监听回环

	slots := [][]string{nil, {"", srv.URL + "/ok.png", srv.URL + "/bad"}}
	got := env.p.fetchVisionSlots(context.Background(), slots)
	if &got[0] != &slots[0] {
		t.Fatal("应原地写回同一切片")
	}
	if got[1][0] != "" {
		t.Errorf("空槽位不应被写入: %q", got[1][0])
	}
	if !strings.HasPrefix(got[1][1], "data:image/png;base64,") {
		t.Errorf("got[1][1] = %q, want image/png data URL", got[1][1])
	}
	if got[1][2] != "" {
		t.Errorf("下载失败的槽位应留空: %q", got[1][2])
	}

	// 无任何非空槽位：不建 ctx、不发请求，原样返回。
	empty := [][]string{{""}, nil}
	if out := env.p.fetchVisionSlots(context.Background(), empty); &out[0] != &empty[0] {
		t.Fatal("空槽位输入应原样返回")
	}
}

func TestSelectVisionSlots(t *testing.T) {
	history := []Turn{
		{Name: "张三", Text: "a", Parts: []TurnPart{
			{Kind: turnPartText, Text: "a"},
			{Kind: turnPartImage, URL: "http://x/1.jpg"},
			{Kind: turnPartImage, URL: "http://x/1b.jpg"},
		}},
		{Name: "李四", Text: "b", Parts: []TurnPart{
			{Kind: turnPartText, Text: "b"},
			{Kind: turnPartImage, URL: "http://x/2.jpg"},
		}},
		{Name: "王五", Text: "c", Parts: []TurnPart{
			{Kind: turnPartText, Text: "c"},
			{Kind: turnPartImage, URL: "http://x/3.jpg"},
		}},
	}

	t.Run("关闭时返回 nil", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		if got := env.p.selectVisionSlots(history); got != nil {
			t.Fatalf("关闭态应返回 nil, got %#v", got)
		}
	})

	t.Run("上限为 0 时返回 nil", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["llm_vision_enabled"] = true
			c["llm_vision_max_images"] = 0
		})
		if got := env.p.selectVisionSlots(history); got != nil {
			t.Fatalf("上限 0 应返回 nil, got %#v", got)
		}
	})

	t.Run("每条消息最多一张且取最后 N 张", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["llm_vision_enabled"] = true
			c["llm_vision_max_images"] = 2
		})
		got := env.p.selectVisionSlots(history)
		if len(got) != len(history) {
			t.Fatalf("应与 trimmed 等长（%d），got %#v", len(history), got)
		}
		if got[0] != nil {
			t.Errorf("张三那条被上限裁掉，应为 nil: %#v", got[0])
		}
		want := [][]string{nil, {"", "http://x/2.jpg"}, {"", "http://x/3.jpg"}}
		for i := 1; i < len(want); i++ {
			if len(got[i]) != len(want[i]) || got[i][0] != "" || got[i][1] != want[i][1] {
				t.Fatalf("got[%d] = %#v, want %#v", i, got[i], want[i])
			}
		}
	})

	t.Run("每条消息只取第一个可用图片段", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["llm_vision_enabled"] = true
			c["llm_vision_max_images"] = 4
		})
		got := env.p.selectVisionSlots(history)
		if got[0][1] != "http://x/1.jpg" || got[0][2] != "" {
			t.Fatalf("张三那条应只命中首个图片段: %#v", got[0])
		}
	})

	t.Run("被文本裁剪丢掉的条目里的图片不发", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["llm_vision_enabled"] = true
			c["llm_history_max_chars"] = 3
		})
		trimmed := env.p.trimHistory(history)
		if len(trimmed) != 1 || trimmed[0].Name != "王五" {
			t.Fatalf("裁剪结果 = %#v", trimmed)
		}
		got := env.p.selectVisionSlots(trimmed)
		if len(got) != 1 || got[0][1] != "http://x/3.jpg" {
			t.Fatalf("got %#v, want 仅王五槽位命中", got)
		}
	})

	t.Run("无图或无可用 URL 时不产生候选", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) { c["llm_vision_enabled"] = true })
		if got := env.p.selectVisionSlots([]Turn{{Name: "甲", Text: "a", Parts: []TurnPart{{Kind: turnPartText, Text: "a"}}}}); got != nil {
			t.Fatalf("无图片段应返回 nil, got %#v", got)
		}
		img := []Turn{{Name: "甲", Text: "a[图片]", Parts: []TurnPart{
			{Kind: turnPartText, Text: "a"},
			{Kind: turnPartImage},
		}}}
		if got := env.p.selectVisionSlots(img); got != nil {
			t.Fatalf("URL 为空的图片段应返回 nil, got %#v", got)
		}
	})
}

// TestVisionDefaultsOffNoImageBlocks 端到端守卫：默认关闭，且生产 imgClient 装了地址限制拨号器。
func TestVisionDefaultsOffNoImageBlocks(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	if env.p.imgClient == nil {
		t.Fatal("imgClient 应非 nil")
	}
	tr, ok := env.p.imgClient.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatalf("imgClient transport 未装地址限制拨号器: %#v", env.p.imgClient.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("imgClient 不应使用环境代理")
	}
	if got := env.p.selectVisionSlots([]Turn{{Name: "甲", Text: "a[图片]", Parts: []TurnPart{
		{Kind: turnPartText, Text: "a"},
		{Kind: turnPartImage, URL: "http://x/1.jpg"},
	}}}); got != nil {
		t.Fatalf("默认配置下应返回 nil, got %#v", got)
	}
}
