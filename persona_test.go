package persona

import (
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func TestRenderTextAndFallbacks(t *testing.T) {
	seg := func(tp bot.SegmentType, data map[string]any) bot.Segment {
		return bot.Segment{Type: tp, Data: data}
	}
	cases := []struct {
		name string
		msg  *bot.Message
		want string
	}{
		{"文本拼接", &bot.Message{Segments: []bot.Segment{
			seg(bot.SegText, map[string]any{bot.KeyText: "你好"}),
			seg(bot.SegMarkdown, map[string]any{bot.KeyText: "世界"}),
		}}, "你好世界"},
		{"At 前置", &bot.Message{Segments: []bot.Segment{
			seg(bot.SegAt, map[string]any{bot.KeyUserID: "u9", bot.KeyUserName: "老王"}),
			seg(bot.SegText, map[string]any{bot.KeyText: "在吗"}),
		}}, "@老王 在吗"},
		{"图片回落", &bot.Message{Segments: []bot.Segment{seg(bot.SegImage, map[string]any{bot.KeyURL: "http://x"})}}, "[图片]"},
		{"表情回落", &bot.Message{Segments: []bot.Segment{seg(bot.SegFace, map[string]any{bot.KeyFaceID: "1"})}}, "[表情]"},
		{"文件回落", &bot.Message{Segments: []bot.Segment{seg(bot.SegFile, map[string]any{bot.KeyURL: "http://x"})}}, "[文件]"},
		{"卡片回落", &bot.Message{Segments: []bot.Segment{seg(bot.SegCard, map[string]any{bot.KeyCard: "{}"})}}, "[卡片]"},
		{"引用回落", &bot.Message{Segments: []bot.Segment{seg(bot.SegReply, map[string]any{bot.KeyMessageID: "x"})}}, "[引用]"},
		{"未知回落", &bot.Message{Segments: []bot.Segment{seg(bot.SegmentType("weird"), nil)}}, "[消息]"},
		{"空消息", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderText(tc.msg); got != tc.want {
				t.Fatalf("renderText = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderHistoryBlock(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	history := []Turn{
		{Name: "张三", Text: "今晚谁去打球"},
		{Name: "李四", Text: "我可能不行"},
		{Name: "小傲娇", Text: "打球可以啊", Self: true},
	}
	got := env.p.renderHistoryBlock(bot.MessageGroup, history)
	want := "[群聊记录]\n张三: 今晚谁去打球\n李四: 我可能不行\n小傲娇: 打球可以啊\n\n"
	if got != want {
		t.Fatalf("renderHistoryBlock =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderHistoryBlockPrivate(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	history := []Turn{
		{Name: "张三", Text: "在吗"},
		{Name: "小傲娇", Text: "在", Self: true},
	}
	got := env.p.renderHistoryBlock(bot.MessagePrivate, history)
	want := "[私聊记录]\n张三: 在吗\n小傲娇: 在\n\n"
	if got != want {
		t.Fatalf("renderHistoryBlock(private) =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderHistoryBlockTrimsOldest(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) { c["llm_history_max_chars"] = 6 })
	history := []Turn{
		{Name: "张三", Text: "aaaa"},
		{Name: "李四", Text: "bbbb"},
		{Name: "我", Text: "ccc"},
	}
	got := env.p.renderHistoryBlock(bot.MessageGroup, history)
	if got != "[群聊记录]\n我: ccc\n\n" {
		t.Fatalf("裁剪后 = %q", got)
	}
}

// TestExtractImageURLs 覆盖图片 URL 的可用性判定：KeyURL 优先，KeyFile 仅 http(s)。
func TestExtractImageURLs(t *testing.T) {
	seg := func(data map[string]any) bot.Segment { return bot.Segment{Type: bot.SegImage, Data: data} }
	cases := []struct {
		name string
		msg  *bot.Message
		want []string
	}{
		{"nil 消息", nil, nil},
		{"无图片段", &bot.Message{Segments: []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hi"}}}}, nil},
		{"KeyURL 命中", &bot.Message{Segments: []bot.Segment{seg(map[string]any{bot.KeyURL: "http://x/a.jpg"})}},
			[]string{"http://x/a.jpg"}},
		{"KeyFile 为 http URL 命中", &bot.Message{Segments: []bot.Segment{seg(map[string]any{bot.KeyFile: "https://x/b.png"})}},
			[]string{"https://x/b.png"}},
		{"KeyFile 为本地路径丢弃", &bot.Message{Segments: []bot.Segment{seg(map[string]any{bot.KeyFile: "/tmp/x.png"})}}, nil},
		{"KeyFile 为平台文件 ID 丢弃", &bot.Message{Segments: []bot.Segment{seg(map[string]any{bot.KeyFile: "file_abc"})}}, nil},
		{"KeyURL 优先于 KeyFile", &bot.Message{Segments: []bot.Segment{
			seg(map[string]any{bot.KeyURL: "http://x/a.jpg", bot.KeyFile: "/tmp/b.png"}),
		}}, []string{"http://x/a.jpg"}},
		{"多图按序", &bot.Message{Segments: []bot.Segment{
			seg(map[string]any{bot.KeyURL: "http://x/1.jpg"}),
			{Type: bot.SegImage, Data: map[string]any{}},
			seg(map[string]any{bot.KeyURL: "http://x/2.jpg"}),
		}}, []string{"http://x/1.jpg", "http://x/2.jpg"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractImageURLs(tc.msg)
			if len(got) != len(tc.want) {
				t.Fatalf("extractImageURLs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("extractImageURLs[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestRenderUserContentVisionOff 锁定关闭态：单个 text 块，内容与 renderHistoryBlock 逐字相同。
func TestRenderUserContentVisionOff(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	history := []Turn{
		{Name: "张三", Text: "今晚谁去打球", ImageURLs: []string{"http://x/1.jpg"}},
		{Name: "李四", Text: "我可能不行"},
	}
	parts := env.p.renderUserContent(bot.MessageGroup, history)
	if len(parts) != 1 || parts[0].Type != "text" {
		t.Fatalf("关闭态应只有 text 块: %#v", parts)
	}
	if parts[0].Text != env.p.renderHistoryBlock(bot.MessageGroup, history) {
		t.Fatalf("text 块应与 renderHistoryBlock 逐字相同: %q", parts[0].Text)
	}
	if parts[0].ImageURL != nil {
		t.Fatal("text 块不应带 image_url")
	}
}

// TestRenderUserContentVisionOn 锁定开关态：text 块 + 按时间序的最后 N 张图。
func TestRenderUserContentVisionOn(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		c["llm_vision_enabled"] = true
		c["llm_vision_max_images"] = 2
	})
	history := []Turn{
		{Name: "张三", Text: "a", ImageURLs: []string{"http://x/1.jpg", "http://x/1b.jpg"}},
		{Name: "李四", Text: "b", ImageURLs: []string{"http://x/2.jpg"}},
		{Name: "王五", Text: "c", ImageURLs: []string{"http://x/3.jpg"}},
	}
	parts := env.p.renderUserContent(bot.MessageGroup, history)
	if len(parts) != 3 {
		t.Fatalf("应为 text + 2 张图: %#v", parts)
	}
	if parts[0].Type != "text" || parts[0].Text != "[群聊记录]\n张三: a\n李四: b\n王五: c\n\n" {
		t.Fatalf("text 块 = %#v", parts[0])
	}
	// 每条消息最多 1 张（visionMaxImagesPerTurn），故候选为 1.jpg/2.jpg/3.jpg，取最后 2 张。
	want := []string{"http://x/2.jpg", "http://x/3.jpg"}
	for i, w := range want {
		p := parts[i+1]
		if p.Type != "image_url" || p.ImageURL == nil || p.ImageURL.URL != w {
			t.Fatalf("图片块[%d] = %#v, want %q", i, p, w)
		}
	}
}

// TestRenderUserContentVisionZero 锁定 llm_vision_max_images=0 等价于不附加。
func TestRenderUserContentVisionZero(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		c["llm_vision_enabled"] = true
		c["llm_vision_max_images"] = 0
	})
	history := []Turn{{Name: "张三", Text: "a", ImageURLs: []string{"http://x/1.jpg"}}}
	if parts := env.p.renderUserContent(bot.MessageGroup, history); len(parts) != 1 {
		t.Fatalf("上限 0 时不应附加图片: %#v", parts)
	}
}

// TestRenderUserContentVisionTrimmed 锁定图片取自与文本块同一裁剪结果。
func TestRenderUserContentVisionTrimmed(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		c["llm_vision_enabled"] = true
		c["llm_history_max_chars"] = 3
	})
	history := []Turn{
		{Name: "甲", Text: "aaa", ImageURLs: []string{"http://x/old.jpg"}},
		{Name: "乙", Text: "bbb", ImageURLs: []string{"http://x/new.jpg"}},
	}
	parts := env.p.renderUserContent(bot.MessageGroup, history)
	if parts[0].Text != "[群聊记录]\n乙: bbb\n\n" {
		t.Fatalf("text 块 = %q", parts[0].Text)
	}
	if len(parts) != 2 || parts[1].ImageURL.URL != "http://x/new.jpg" {
		t.Fatalf("被裁掉的条目里的图片不应发送: %#v", parts)
	}
}

func TestRenderSystemPrompt(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) { c["random_timezone"] = "UTC" })
	ev := groupEvent("g1", "u1", "张三", "hi")
	st := env.waitLoaded(ev)
	st.appendHistory(Turn{At: time.Now(), UserID: "u1", Name: "张三", Text: "hi"})
	env.fixNow(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))

	env.p.cfg.personaTemplate = "A{{persona}}|B{{persona_name}}|C{{channel_name}}|D{{channel_id}}|E{{platform}}|F{{bot_name}}|G{{now}}|H{{last_sender}}|I{{max_chars}}|J{{skip_token}}|K{{unknown}}|L{{chat_kind}}"
	got := env.p.renderSystemPrompt("tsundere", st, st.snapshotHistory(20))
	want := "A傲娇|Btsundere|C群g1|Dg1|Emock|Fbot1|G2026-09-26 12:00|H张三|I200|J[SKIP]|K{{unknown}}|L群聊"
	if got != want {
		t.Fatalf("renderSystemPrompt =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderSystemPromptPrivate(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	ev := privateEvent("u1", "张三", "在吗")
	st := env.waitLoaded(ev)
	st.appendHistory(Turn{At: time.Now(), UserID: "u1", Name: "张三", Text: "在吗"})

	env.p.cfg.personaTemplate = "A{{chat_kind}}|B{{channel_name}}|C{{channel_id}}|D{{platform}}|E{{bot_name}}|F{{last_sender}}"
	got := env.p.renderSystemPrompt("default", st, st.snapshotHistory(20))
	want := "A私聊|B张三|C|Dmock|Ebot1|F张三"
	if got != want {
		t.Fatalf("renderSystemPrompt(private) =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderSystemPromptDefaultTemplate(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	ev := groupEvent("g1", "u1", "张三", "hi")
	st := env.waitLoaded(ev)
	got := env.p.renderSystemPrompt("default", st, nil)
	if !containsStr(got, "你正在一个群聊里聊天。") || !containsStr(got, "默认人格") {
		t.Fatalf("默认模板渲染异常:\n%s", got)
	}
	if containsStr(got, "{{") {
		t.Fatalf("存在未替换占位符:\n%s", got)
	}
}

func TestCleanReply(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	ev := groupEvent("g1", "u1", "张三", "hi")
	st := env.waitLoaded(ev)

	cases := []struct {
		in     string
		want   string
		reason string
	}{
		{`  你好  `, "你好", ""},
		{`"你好"`, "你好", ""},
		{"「你好」", "你好", ""},
		{"『你好』", "你好", ""},
		{"行吧\n那我去", "行吧 那我去", ""},
		{"好   的", "好 的", ""},
		{"[SKIP]", "", "skipped_by_llm"},
		{"   ", "", "empty_reply"},
	}
	for _, tc := range cases {
		got, reason := env.p.cleanReply(tc.in, "[SKIP]", st)
		if got != tc.want || reason != tc.reason {
			t.Errorf("cleanReply(%q) = (%q,%q), want (%q,%q)", tc.in, got, reason, tc.want, tc.reason)
		}
	}

	// reply_max_chars 硬截断（不加省略号）。
	env2 := newTestEnv(t, nil, func(c map[string]any) { c["reply_max_chars"] = 5 })
	st2 := env2.waitLoaded(ev)
	if got, reason := env2.p.cleanReply("123456789", "[SKIP]", st2); got != "12345" || reason != "" {
		t.Errorf("截断 = (%q,%q), want (12345,\"\")", got, reason)
	}
}

func TestCleanReplyDedupe(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	ev := groupEvent("g1", "u1", "张三", "hi")
	st := env.waitLoaded(ev)
	st.appendHistory(Turn{Text: "你好", Self: true})
	if _, reason := env.p.cleanReply("你好", "[SKIP]", st); reason != "duplicate_reply" {
		t.Fatalf("reason = %q, want duplicate_reply", reason)
	}
}

func TestMatchBindingPrecedence(t *testing.T) {
	cfg, err := loadConfig(bot.NewConfig(map[string]any{
		"personas":    map[string]any{"default": "p", "tsundere": "t", "deadpan": "d"},
		"llm_api_key": "k",
		"llm_model":   "m",
		"bindings": []any{
			map[string]any{"channel_id": "g1", "persona": "tsundere"},
			map[string]any{"platform": "feishu", "channel_id": "g1", "persona": "deadpan"},
			map[string]any{"platform": "feishu", "bot_id": "feishu-main", "channel_id": "g2", "persona": "deadpan"},
		},
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	cases := []struct {
		platform, botID, channelID, want string
		ok                               bool
	}{
		{"onebot", "b", "g1", "tsundere", true},
		{"feishu", "b", "g1", "deadpan", true},
		{"feishu", "feishu-main", "g2", "deadpan", true},
		{"onebot", "b", "g9", "", false},
	}
	for _, tc := range cases {
		got, ok := cfg.matchBinding(tc.platform, tc.botID, tc.channelID)
		if ok != tc.ok || got != tc.want {
			t.Errorf("matchBinding(%s,%s,%s) = (%q,%v), want (%q,%v)",
				tc.platform, tc.botID, tc.channelID, got, ok, tc.want, tc.ok)
		}
	}
}

func TestResolvePersonaPriority(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		c["bindings"] = []any{map[string]any{"channel_id": "g1", "persona": "tsundere"}}
	})
	ev := groupEvent("g1", "u1", "张三", "hi")
	st := env.waitLoaded(ev)

	if name, src := env.p.resolvePersona(st); name != "tsundere" || src != "binding" {
		t.Fatalf("binding: (%q,%q)", name, src)
	}
	st.mu.Lock()
	st.persona = "default"
	st.mu.Unlock()
	if name, src := env.p.resolvePersona(st); name != "default" || src != "override" {
		t.Fatalf("override: (%q,%q)", name, src)
	}
	ev2 := groupEvent("g9", "u1", "张三", "hi")
	st2 := env.waitLoaded(ev2)
	if name, src := env.p.resolvePersona(st2); name != "default" || src != "default" {
		t.Fatalf("default: (%q,%q)", name, src)
	}
}

func TestInQuietHours(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		c["random_quiet_hours"] = "23:00-07:00"
		c["random_timezone"] = "UTC"
	})
	at := func(h, m int) time.Time { return time.Date(2026, 9, 26, h, m, 0, 0, time.UTC) }
	cases := []struct {
		h, m int
		want bool
	}{
		{23, 30, true},
		{0, 0, true},
		{6, 59, true},
		{7, 0, false},
		{12, 0, false},
		{22, 59, false},
	}
	for _, tc := range cases {
		if got := env.p.inQuietHours(at(tc.h, tc.m)); got != tc.want {
			t.Errorf("%02d:%02d = %v, want %v", tc.h, tc.m, got, tc.want)
		}
	}
}

func TestHasMentionSelfIDs(t *testing.T) {
	msg := &bot.Message{Segments: []bot.Segment{
		{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "other"}},
	}}
	if !hasMention(msg, nil) {
		t.Error("self_ids 为空时任意 At 都应算寻址")
	}
	if hasMention(msg, []string{"me"}) {
		t.Error("self_ids 非空且未命中不应算寻址")
	}
	if !hasMention(msg, []string{"other"}) {
		t.Error("self_ids 命中应算寻址")
	}
}

// TestHasMentionAtAll 锁定：@全体成员（qq="all"）任何情况下都不算寻址。
func TestHasMentionAtAll(t *testing.T) {
	msg := func(segs ...bot.Segment) *bot.Message { return &bot.Message{Segments: segs} }
	all := bot.Segment{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "all"}}

	if hasMention(msg(all), nil) {
		t.Error("self_ids 为空时 @全体 也不应算寻址")
	}
	if hasMention(msg(all), []string{"all"}) {
		t.Error("即使 self_ids 显式写了 all 也不应算寻址（all 不是真实用户 ID）")
	}
	// 同一消息里 @全体 + @本人：仍应因后者判定为寻址。
	me := bot.Segment{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "me"}}
	if !hasMention(msg(all, me), []string{"me"}) {
		t.Error("@全体 与 @本人 同时出现时应判定为寻址")
	}
	// @全体 在前不应短路掉后续段的判定。
	if !hasMention(msg(all, me), nil) {
		t.Error("@全体 不应短路后续 At 段的判定")
	}
}

// TestSelfIDsUnquotedYAML 锁定：self_ids 写成 YAML 裸数字时不会被静默丢弃。
//
// 回归场景：self_ids 被丢成空列表会落入「空 = 任意 At 均算寻址」，
// 表现为 @任何人都触发 kind=addressed 回复。
func TestSelfIDsUnquotedYAML(t *testing.T) {
	cases := []struct {
		name string
		raw  any
	}{
		{"quoted-list", []any{"123456789"}},
		{"unquoted-list", []any{123456789}},
		{"bare-scalar", 123456789},
		{"csv-string", "123456789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(bot.NewConfig(map[string]any{
				"personas":  map[string]any{"default": map[string]any{"prompt": "p"}},
				"llm_model": "m",
				"self_ids":  tc.raw,
			}))
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if len(cfg.selfIDs) != 1 || cfg.selfIDs[0] != "123456789" {
				t.Fatalf("selfIDs = %#v, want [123456789]", cfg.selfIDs)
			}
		})
	}
}

// TestListKeysUnquotedYAML 锁定名单键同样兼容裸数字写法。
func TestListKeysUnquotedYAML(t *testing.T) {
	cfg, err := loadConfig(bot.NewConfig(map[string]any{
		"personas":     map[string]any{"default": map[string]any{"prompt": "p"}},
		"llm_model":    "m",
		"group_policy": "whitelist",
		"group_list":   []any{389372103},
		"private_list": "123, 456",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.groupList) != 1 || cfg.groupList[0] != "389372103" {
		t.Fatalf("groupList = %#v, want [389372103]", cfg.groupList)
	}
	if len(cfg.privateList) != 2 || cfg.privateList[0] != "123" || cfg.privateList[1] != "456" {
		t.Fatalf("privateList = %#v, want [123 456]", cfg.privateList)
	}
	if !allowByMode(cfg.groupPolicy, cfg.groupList, "389372103") {
		t.Error("裸数字群号应命中白名单")
	}
}

func TestHasKeyword(t *testing.T) {
	if !hasKeyword("小助手在吗", []string{"小助手"}) {
		t.Error("关键词子串匹配失败")
	}
	if !hasKeyword("Hello BOT", []string{"bot"}) {
		t.Error("关键词应大小写不敏感")
	}
	if hasKeyword("无关", nil) {
		t.Error("关键词为空不应匹配")
	}
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
