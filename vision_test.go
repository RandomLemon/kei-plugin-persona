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
	if len(got) != 4 {
		t.Fatalf("应保留 4 张成功图片, got %d: %#v", len(got), got)
	}
	if raw := decode(t, got[0]); string(raw) != string(png) {
		t.Fatalf("got[0] 解码 = %q, want %q", raw, png)
	}
	if raw := decode(t, got[1]); string(raw) != string(png) {
		t.Fatalf("got[1]（无 image/* Content-Type 但魔数为 PNG）解码 = %q", raw)
	}
	if raw := decode(t, got[2]); string(raw) != string(png) {
		t.Fatalf("got[2] 解码 = %q, want %q", raw, png)
	}
	if !strings.HasPrefix(got[3], "data:image/gif;base64,") {
		t.Fatalf("got[3] = %q, want image/gif data URL", got[3])
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

	got := env.p.fetchImageDataURLs(context.Background(), []string{
		srv.URL + "/a.png", srv.URL + "/b.jpeg", srv.URL + "/c.gif",
	})
	if len(got) != 2 {
		t.Fatalf("白名单内应保留 2 张, got %d: %#v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "data:image/png;base64,") {
		t.Errorf("got[0] = %q, want image/png data URL", got[0])
	}
	if !strings.HasPrefix(got[1], "data:image/gif;base64,") {
		t.Errorf("got[1] = %q, want image/gif data URL", got[1])
	}
	if !env.cap.hasAttr("reason", "format") {
		t.Error("缺少 reason=format 的丢弃日志")
	}
}

func TestSelectVisionURLs(t *testing.T) {
	history := []Turn{
		{Name: "张三", Text: "a", ImageURLs: []string{"http://x/1.jpg", "http://x/1b.jpg"}},
		{Name: "李四", Text: "b", ImageURLs: []string{"http://x/2.jpg"}},
		{Name: "王五", Text: "c", ImageURLs: []string{"http://x/3.jpg"}},
	}

	t.Run("关闭时返回 nil", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		if got := env.p.selectVisionURLs(history); got != nil {
			t.Fatalf("关闭态应返回 nil, got %#v", got)
		}
	})

	t.Run("上限为 0 时返回 nil", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["llm_vision_enabled"] = true
			c["llm_vision_max_images"] = 0
		})
		if got := env.p.selectVisionURLs(history); got != nil {
			t.Fatalf("上限 0 应返回 nil, got %#v", got)
		}
	})

	t.Run("每条消息最多一张且取最后 N 张", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["llm_vision_enabled"] = true
			c["llm_vision_max_images"] = 2
		})
		want := []string{"http://x/2.jpg", "http://x/3.jpg"}
		got := env.p.selectVisionURLs(history)
		if len(got) != len(want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("被文本裁剪丢掉的条目里的图片不发", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["llm_vision_enabled"] = true
			c["llm_history_max_chars"] = 3
		})
		got := env.p.selectVisionURLs(history)
		if len(got) != 1 || got[0] != "http://x/3.jpg" {
			t.Fatalf("got %#v, want [http://x/3.jpg]", got)
		}
	})

	t.Run("imgs 无 URL 时不产生候选", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) { c["llm_vision_enabled"] = true })
		if got := env.p.selectVisionURLs([]Turn{{Name: "甲", Text: "a"}}); len(got) != 0 {
			t.Fatalf("got %#v, want empty", got)
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
	if got := env.p.selectVisionURLs([]Turn{{Name: "甲", Text: "a", ImageURLs: []string{"http://x/1.jpg"}}}); got != nil {
		t.Fatalf("默认配置下应返回 nil, got %#v", got)
	}
}
