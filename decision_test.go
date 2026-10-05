package persona

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// fastConfig 让随机参与与寻址回复确定性地触发。
func fastConfig(c map[string]any) {
	c["batch_window"] = "1ms"
	c["batch_max_window"] = "5ms"
	c["random_probability"] = 1.0
	c["random_min_participants"] = 1
	c["random_cooldown"] = "0s"
	c["mention_min_interval"] = "0s"
}

func atEvent(channel, user, name, text string) *bot.Event {
	ev := groupEvent(channel, user, name, text)
	ev.Message.Segments = []bot.Segment{
		{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "bot1", bot.KeyUserName: "小助手"}},
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
	}
	return ev
}

func TestFilterReasons(t *testing.T) {
	t.Run("no_sender", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		ev := groupEvent("g1", "u1", "张三", "hi")
		ev.Sender = nil
		_ = env.deliver(ev)
		if !env.cap.has("no_sender") {
			t.Fatal("want no_sender")
		}
	})
	t.Run("not_group", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		ev := groupEvent("g1", "u1", "张三", "hi")
		ev.Message.Kind = bot.MessagePrivate
		_ = env.deliver(ev)
		if !env.cap.has("not_group") {
			t.Fatal("want not_group")
		}
	})
	t.Run("not_private", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		_ = env.deliverPrivate(groupEvent("g1", "u1", "张三", "hi"))
		if !env.cap.has("not_private") {
			t.Fatal("want not_private")
		}
	})
	t.Run("not_allowed", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) { c["group_policy"] = "off" })
		_ = env.deliver(groupEvent("g1", "u1", "张三", "hi"))
		if !env.cap.has("not_allowed") {
			t.Fatal("want not_allowed")
		}
	})
	t.Run("bot_sender", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		ev := groupEvent("g1", "b1", "某机器人", "hi")
		ev.Sender.IsBot = true
		_ = env.deliver(ev)
		if !env.cap.has("bot_sender") {
			t.Fatal("want bot_sender")
		}
	})
	t.Run("command", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		ev := groupEvent("g1", "u1", "张三", "hi")
		ev.Command = &bot.Command{Name: "echo"}
		_ = env.deliver(ev)
		if !env.cap.has("command") {
			t.Fatal("want command")
		}
	})
	t.Run("persona_command_not_in_history", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		st := env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		before := len(st.snapshotHistory(100))
		ev := groupEvent("g1", "u1", "张三", "hi")
		ev.Command = &bot.Command{Name: "persona", Args: []string{"status"}}
		_ = env.deliver(ev)
		if !env.cap.has("command") {
			t.Fatal("want command")
		}
		if after := len(st.snapshotHistory(100)); after != before {
			t.Fatalf("史条数 %d -> %d，/persona 不应进历史", before, after)
		}
	})
	t.Run("empty_text", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", ""))
		if !env.cap.has("empty_text") {
			t.Fatal("want empty_text")
		}
	})
	t.Run("too_short", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "a"))
		if !env.cap.has("too_short") {
			t.Fatal("want too_short")
		}
	})
	t.Run("channel_off", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		env.command("off")
		_ = env.deliver(groupEvent("g1", "u1", "张三", "hi"))
		if !env.cap.has("channel_off") {
			t.Fatal("want channel_off")
		}
	})
	t.Run("loading", func(t *testing.T) {
		gate := make(chan struct{})
		store := &blockingStorage{fakeStorage: newFakeStorage(), gate: gate}
		env := newTestEnvWith(t, nil, nil, store, nil)
		_ = env.deliver(groupEvent("g1", "u1", "张三", "hi"))
		if !env.cap.has("loading") {
			t.Fatal("want loading")
		}
		close(gate)
	})
	t.Run("not_addressed", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) { c["random_enabled"] = false })
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.cap.has("not_addressed") {
			t.Fatal("want not_addressed")
		}
	})
	t.Run("hour_quota", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) { c["random_max_per_hour"] = 0 })
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.cap.has("hour_quota") {
			t.Fatal("want hour_quota")
		}
	})
	t.Run("min_participants", func(t *testing.T) {
		env := newTestEnv(t, nil, nil)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.cap.has("min_participants") {
			t.Fatal("want min_participants")
		}
	})
	t.Run("probability", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["random_min_participants"] = 1
			c["random_probability"] = 0.0
		})
		env.setRand(0)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.cap.has("probability") {
			t.Fatal("want probability")
		}
	})
	t.Run("quiet_hours", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			c["random_quiet_hours"] = "23:00-07:00"
			c["random_timezone"] = "UTC"
		})
		env.fixNow(time.Date(2026, 9, 26, 23, 30, 0, 0, time.UTC))
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.cap.has("quiet_hours") {
			t.Fatal("want quiet_hours")
		}
	})
	t.Run("inflight", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) { c["batch_window"] = "1h" })
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		env.setRand(0)
		env.p.cfg.randomMinParticipants = 0
		env.p.cfg.randomProbability = 1
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好")) // 布防 awaiting
		_ = env.deliver(groupEvent("g1", "u1", "张三", "又来了")) // inflight
		if !env.cap.has("inflight") {
			t.Fatal("want inflight")
		}
	})
}

// TestFirstMessageDeferredUntilLoaded 回归：新会话第一条消息不得因懒加载被丢弃。
//
// 懒加载未完成时 Handler 不能阻塞，只能暂存；加载完成后必须补判一次，
// 否则每次新建会话（首次出现、重启后、被 LRU 淘汰后）的第一条消息都会被静默吞掉。
func TestFirstMessageDeferredUntilLoaded(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		gate := make(chan struct{})
		store := &blockingStorage{fakeStorage: newFakeStorage(), gate: gate}
		env := newTestEnvWith(t, nil, fastConfig, store, nil)

		_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
		if !env.cap.has("loading") {
			t.Fatal("want loading")
		}
		if env.fake.count() != 0 {
			t.Fatal("懒加载完成前不应发送")
		}

		close(gate)
		if !env.waitSends(1, 3*time.Second) {
			t.Fatal("加载完成后应补判首条并回复")
		}
		if sent := env.fake.at(0); sent.Target.ChannelID != "g1" || sent.Target.Kind != bot.MessageGroup {
			t.Fatalf("发送目标错误: %+v", sent.Target)
		}
	})

	t.Run("private", func(t *testing.T) {
		gate := make(chan struct{})
		store := &blockingStorage{fakeStorage: newFakeStorage(), gate: gate}
		env := newTestEnvWith(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["private_policy"] = "open"
		}, store, nil)

		_ = env.deliverPrivate(privateEvent("u1", "张三", "在吗"))
		if env.fake.count() != 0 {
			t.Fatal("懒加载完成前不应发送")
		}

		close(gate)
		if !env.waitSends(1, 3*time.Second) {
			t.Fatal("私聊首条应补判并回复")
		}
		if sent := env.fake.at(0); sent.Target.Kind != bot.MessagePrivate || sent.Target.UserID != "u1" {
			t.Fatalf("发送目标错误: %+v", sent.Target)
		}
	})

	t.Run("disabled_override_wins", func(t *testing.T) {
		gate := make(chan struct{})
		store := &blockingStorage{fakeStorage: newFakeStorage(), gate: gate}
		seedOverride(t, store.fakeStorage, "mock:bot1:g1", overrideValue{Disabled: true})
		env := newTestEnvWith(t, nil, fastConfig, store, nil)

		_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
		close(gate)
		if !env.waitReason("channel_off", 3*time.Second) {
			t.Fatal("补判必须用加载到的 disabled 覆盖")
		}
		if env.fake.count() != 0 {
			t.Fatal("被关闭的会话不应发送")
		}
	})

	t.Run("persona_override_applied", func(t *testing.T) {
		gate := make(chan struct{})
		store := &blockingStorage{fakeStorage: newFakeStorage(), gate: gate}
		seedOverride(t, store.fakeStorage, "mock:bot1:g1", overrideValue{Persona: "tsundere"})
		env := newTestEnvWith(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["debug_prompts"] = true
		}, store, nil)

		_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
		close(gate)
		if !env.waitSends(1, 3*time.Second) {
			t.Fatal("补判应触发回复")
		}
		if body := env.cap.attrOf("persona llm 请求", "body"); !strings.Contains(body, "傲娇") {
			t.Fatalf("补判生成未使用加载到的人格覆盖: %q", body)
		}
	})
}

// seedOverride 往存储写入一条会话覆盖。
func seedOverride(t *testing.T, store *fakeStorage, sessionKey string, v overrideValue) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal override: %v", err)
	}
	if err := store.Set(context.Background(), overrideKey(sessionKey), raw, 0); err != nil {
		t.Fatalf("seed override: %v", err)
	}
}

func TestAddressedAndCooldown(t *testing.T) {
	t.Run("via_at", func(t *testing.T) {
		env := newTestEnv(t, nil, fastConfig)
		env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("被 @ 应触发回复")
		}
		sent := env.fake.at(0)
		if sent.Target.ChannelID != "g1" || sent.Target.Kind != bot.MessageGroup || sent.Target.BotID != "bot1" {
			t.Fatalf("目标错误: %+v", sent.Target)
		}
		if got := plainText(sent.Msg); got != "打球可以啊" {
			t.Fatalf("text = %q", got)
		}
	})
	t.Run("via_reply", func(t *testing.T) {
		env := newTestEnv(t, nil, fastConfig)
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		env.p.replyIDs.Add("mid-1")
		ev := groupEvent("g1", "u1", "张三", "再聊聊")
		ev.Message.Segments = []bot.Segment{
			{Type: bot.SegReply, Data: map[string]any{bot.KeyMessageID: "mid-1"}},
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: "再聊聊"}},
		}
		_ = env.deliver(ev)
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("引用自己应触发回复")
		}
	})
	t.Run("via_keyword", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["trigger_keywords"] = []any{"小助手"}
		})
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "小助手在吗"))
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("关键词应触发回复")
		}
	})
	t.Run("cooldown", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["random_cooldown"] = "90s"
		})
		env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(groupEvent("g1", "u1", "张三", "大家好"))
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("首条应回复")
		}
		_ = env.deliver(groupEvent("g1", "u2", "李四", "又来了"))
		if !env.cap.has("cooldown") {
			t.Fatal("want cooldown")
		}
	})
	// 回归：onebot 下「引用他人 + @他人 + 文本」曾被判为寻址，逐条覆盖三种 At 来源。
	t.Run("at_other_not_addressed", func(t *testing.T) {
		cases := []struct {
			name string
			seg  bot.Segment
		}{
			{"at-other", bot.Segment{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "1139954766"}}},
			{"at-all", bot.Segment{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "all"}}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				env := newTestEnv(t, nil, func(c map[string]any) {
					fastConfig(c)
					c["random_enabled"] = false
					c["self_ids"] = []any{"99999"}
				})
				ev := groupEvent("389372103", "1832783120", "int16@nixos", "可能比manjaro还要臃肿不少")
				ev.Message.Segments = []bot.Segment{
					{Type: bot.SegReply, Data: map[string]any{bot.KeyMessageID: "1585012348"}},
					tc.seg,
					{Type: bot.SegText, Data: map[string]any{bot.KeyText: "可能比manjaro还要臃肿不少"}},
				}
				env.waitLoaded(ev)
				_ = env.deliver(ev)
				if !env.cap.has("not_addressed") {
					t.Fatal("want not_addressed：非寻址消息不应进入寻址路径")
				}
				if env.fake.count() != 0 {
					t.Fatalf("不应发送, count=%d", env.fake.count())
				}
			})
		}
	})
	// 回归：self_ids 写成 YAML 裸数字时不得被丢成空列表（空 = 任意 At 均算寻址）。
	t.Run("self_ids_unquoted_still_matches", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["self_ids"] = []any{99999}
		})
		ev := groupEvent("g1", "u1", "张三", "在吗")
		ev.Message.Segments = []bot.Segment{
			{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "99999"}},
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: "在吗"}},
		}
		env.waitLoaded(ev)
		_ = env.deliver(ev)
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("@本人应触发回复")
		}
	})
}

func TestReplyMentionSender(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["reply_mention_sender"] = true
	})
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u7", "老王", "在吗"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("应回复")
	}
	msg := env.fake.at(0).Msg
	if len(msg.Segments) != 2 || msg.Segments[0].Type != bot.SegAt {
		t.Fatalf("应前置 At 段: %+v", msg.Segments)
	}
	if got := msg.Segments[0].Data[bot.KeyUserID]; got != "u7" {
		t.Fatalf("At 目标 = %v, want u7", got)
	}
}

func TestGenerateSkipReasons(t *testing.T) {
	cases := []struct {
		name string
		llm  func() string
		want string
	}{
		{"skipped_by_llm", func() string { return "[SKIP]" }, "skipped_by_llm"},
		{"empty_reply", func() string { return "   " }, "empty_reply"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, llmJSON(tc.llm()), fastConfig)
			env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
			_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
			if !env.waitReason(tc.want, 2*time.Second) {
				t.Fatalf("want %s", tc.want)
			}
			if env.fake.count() != 0 {
				t.Fatal("不应发送任何消息")
			}
		})
	}
}

func TestDuplicateReply(t *testing.T) {
	env := newTestEnv(t, llmJSON("你好"), fastConfig)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("首次应回复")
	}
	_ = env.deliver(atEvent("g1", "u1", "张三", "再说一次"))
	if !env.waitReason("duplicate_reply", 2*time.Second) {
		t.Fatal("want duplicate_reply")
	}
	if env.fake.count() != 1 {
		t.Fatalf("重复回复不应发送, count=%d", env.fake.count())
	}
}

func TestLLMError(t *testing.T) {
	bad := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) }
	env := newTestEnv(t, http.HandlerFunc(bad), fastConfig)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
	if !env.waitReason("llm_error", 2*time.Second) {
		t.Fatal("want llm_error")
	}
	if env.fake.count() != 0 {
		t.Fatal("LLM 失败不应发送")
	}
}

func TestSendError(t *testing.T) {
	env := newTestEnvWith(t, nil, fastConfig, nil, &failingAPI{})
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
	if !env.waitReason("send_error", 2*time.Second) {
		t.Fatal("want send_error")
	}
}

func TestSemaphoreFull(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["limits_max_concurrent"] = 1
	})
	// 直接占满唯一的并发名额，模拟其它会话正在生成。
	evB := atEvent("gB", "u1", "张三", "hi")
	env.waitLoaded(evB)
	if !env.p.sem.TryAcquire() {
		t.Fatal("名额应为空")
	}
	_ = env.deliver(evB)
	if !env.waitReason("semaphore_full", 2*time.Second) {
		t.Fatal("want semaphore_full")
	}
	env.p.sem.Release()
	if env.fake.count() != 0 {
		t.Fatal("名额占满时不应发送")
	}
}

func TestGenerateStale(t *testing.T) {
	env := newTestEnv(t, nil, fastConfig)
	ev := atEvent("g1", "u1", "张三", "hi")
	st := env.waitLoaded(ev)
	st.mu.Lock()
	st.epoch = 9
	st.mu.Unlock()
	env.p.wg.Add(1)
	env.p.generate(st, 1, nil)
	if !env.cap.has("stale") {
		t.Fatal("want stale")
	}
	if env.fake.count() != 0 {
		t.Fatal("stale 不应发送")
	}
}

func TestStatusAndPersonaCommands(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	env.waitLoaded(groupEvent("g1", "u1", "张三", "hi"))

	if got := env.command("status"); got != "persona: 开 · persona=default(default) · 历史 0 条 · 近 1 小时回复 0/6 · 上次回复 从未 · llm 错误 0 · 已跳 0" {
		t.Fatalf("status = %q", got)
	}
	if got := env.command("switch"); got != "persona: persona=default 来源=default" {
		t.Fatalf("persona = %q", got)
	}
	if got := env.command("switch", "tsundere"); got != "persona: persona=tsundere" {
		t.Fatalf("persona set = %q", got)
	}
	if got := env.command("switch"); got != "persona: persona=tsundere 来源=override" {
		t.Fatalf("persona after set = %q", got)
	}
	if got := env.command("switch", "nope"); got != "persona: 未找到人格 nope" {
		t.Fatalf("persona unknown = %q", got)
	}
	if got := env.command("bogus"); got != personaUsage() {
		t.Fatalf("unknown = %q", got)
	}
	// 覆盖应写穿透到 Storage。
	key := "persona:override:mock:bot1:g1"
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, ok := env.store.value(key); ok && v != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	v, ok := env.store.value(key)
	if !ok || !containsStr(v, "tsundere") {
		t.Fatalf("override 未持久化: %q ok=%v", v, ok)
	}
}

func TestResetCommand(t *testing.T) {
	env := newTestEnv(t, nil, fastConfig)
	env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
	_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("应回复")
	}
	env.command("reset")
	st := env.p.stateFor(groupEvent("g1", "u1", "张三", "hi"))
	if n := len(st.snapshotHistory(100)); n != 0 {
		t.Fatalf("reset 后历史应为空, got %d", n)
	}
	if st.isDisabled() {
		t.Fatal("reset 后应为开启")
	}
}

func TestStopNoSend(t *testing.T) {
	env := newTestEnv(t, nil, fastConfig)
	ev := atEvent("g1", "u1", "张三", "hi")
	env.waitLoaded(ev)
	if err := env.p.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_ = env.deliver(ev)
	time.Sleep(200 * time.Millisecond)
	if env.fake.count() != 0 {
		t.Fatalf("Stop 后不应发送, count=%d", env.fake.count())
	}
}

func TestStopIsIdempotent(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	if err := env.p.Stop(t.Context()); err != nil {
		t.Fatalf("首次 Stop: %v", err)
	}
	if err := env.p.Stop(t.Context()); err != nil {
		t.Fatalf("二次 Stop: %v", err)
	}
}

func TestGroupPolicyFilter(t *testing.T) {
	t.Run("whitelist", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["group_policy"] = "whitelist"
			c["group_list"] = []any{"g1"}
		})
		env.waitLoaded(atEvent("g1", "u1", "张三", "hi"))
		_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("白名单内应回复")
		}
		_ = env.deliver(atEvent("g2", "u1", "张三", "在吗"))
		if !env.cap.has("not_allowed") {
			t.Fatal("want not_allowed")
		}
		if env.fake.count() != 1 {
			t.Fatalf("白名单外不应发送, count=%d", env.fake.count())
		}
	})
	t.Run("blacklist", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["group_policy"] = "blacklist"
			c["group_list"] = []any{"g1"}
		})
		_ = env.deliver(atEvent("g1", "u1", "张三", "在吗"))
		if !env.cap.has("not_allowed") {
			t.Fatal("黑名单内应拒绝")
		}
		env.waitLoaded(atEvent("g2", "u1", "张三", "hi"))
		_ = env.deliver(atEvent("g2", "u1", "张三", "在吗"))
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("黑名单外应回复")
		}
	})
}

func TestPolicyRejectSkipsStateAndHistory(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) { c["group_policy"] = "whitelist" })
		_ = env.deliver(groupEvent("g1", "u1", "张三", "hi"))
		if !env.cap.has("not_allowed") {
			t.Fatal("want not_allowed")
		}
		if n := channelCount(env.p); n != 0 {
			t.Fatalf("被拒会话不应建状态, channels=%d", n)
		}
	})
	t.Run("private", func(t *testing.T) {
		env := newTestEnv(t, nil, nil) // private_policy 默认 off
		_ = env.deliverPrivate(privateEvent("u1", "张三", "在吗"))
		if !env.cap.has("not_allowed") {
			t.Fatal("want not_allowed")
		}
		if n := channelCount(env.p); n != 0 {
			t.Fatalf("被拒会话不应建状态, channels=%d", n)
		}
	})
}

func channelCount(p *Plugin) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.channels)
}

func TestPrivatePolicyDefaultOff(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	_ = env.deliverPrivate(privateEvent("u1", "张三", "在吗"))
	if !env.cap.has("not_allowed") {
		t.Fatal("want not_allowed")
	}
	if env.fake.count() != 0 {
		t.Fatalf("默认关闭私聊不应发送, count=%d", env.fake.count())
	}
}

func TestPrivateReply(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["private_policy"] = "open"
	})
	env.waitLoaded(privateEvent("u1", "张三", "hi"))
	_ = env.deliverPrivate(privateEvent("u1", "张三", "在吗"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("私聊应回复")
	}
	sent := env.fake.at(0)
	if sent.Target.Kind != bot.MessagePrivate {
		t.Fatalf("Kind = %q, want private", sent.Target.Kind)
	}
	if sent.Target.UserID != "u1" {
		t.Fatalf("UserID = %q, want u1", sent.Target.UserID)
	}
	if sent.Target.ChannelID != "" {
		t.Fatalf("ChannelID = %q, want empty", sent.Target.ChannelID)
	}
	if sent.Target.BotID != "bot1" {
		t.Fatalf("BotID = %q, want bot1", sent.Target.BotID)
	}
	if got := plainText(sent.Msg); got != "打球可以啊" {
		t.Fatalf("text = %q", got)
	}
}

func TestPrivateSkipsRandomPath(t *testing.T) {
	t.Run("random_enabled=false", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["private_policy"] = "open"
			c["random_enabled"] = false
		})
		env.waitLoaded(privateEvent("u1", "张三", "hi"))
		_ = env.deliverPrivate(privateEvent("u1", "张三", "在吗"))
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("私聊不依赖 random_enabled")
		}
	})
	t.Run("probability 0", func(t *testing.T) {
		env := newTestEnv(t, nil, func(c map[string]any) {
			fastConfig(c)
			c["private_policy"] = "open"
			c["mention_reply_probability"] = 0.0
		})
		env.setRand(0)
		env.waitLoaded(privateEvent("u1", "张三", "hi"))
		_ = env.deliverPrivate(privateEvent("u1", "张三", "在吗"))
		if !env.cap.has("probability") {
			t.Fatal("want probability")
		}
		if env.fake.count() != 0 {
			t.Fatalf("概率未命中不应发送, count=%d", env.fake.count())
		}
	})
}

func TestPrivateAlwaysAddressedCooldown(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["private_policy"] = "open"
		c["mention_min_interval"] = "90s"
	})
	env.waitLoaded(privateEvent("u1", "张三", "hi"))
	_ = env.deliverPrivate(privateEvent("u1", "张三", "在吗"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("首条应回复")
	}
	_ = env.deliverPrivate(privateEvent("u1", "张三", "再说一次"))
	if !env.cap.has("cooldown") {
		t.Fatal("want cooldown")
	}
	if env.fake.count() != 1 {
		t.Fatalf("冷却期内不应发送, count=%d", env.fake.count())
	}
}

func TestPrivateWhitelist(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["private_policy"] = "whitelist"
		c["private_list"] = []any{"u1"}
	})
	env.waitLoaded(privateEvent("u1", "张三", "hi"))
	_ = env.deliverPrivate(privateEvent("u1", "张三", "在吗"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("白名单内应回复")
	}
	_ = env.deliverPrivate(privateEvent("u2", "李四", "在吗"))
	if !env.cap.has("not_allowed") {
		t.Fatal("want not_allowed")
	}
	if env.fake.count() != 1 {
		t.Fatalf("白名单外不应发送, count=%d", env.fake.count())
	}
}

func TestPrivateReplyOmitsAtSegment(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) {
		fastConfig(c)
		c["private_policy"] = "open"
		c["reply_mention_sender"] = true
	})
	env.waitLoaded(privateEvent("u1", "张三", "hi"))
	_ = env.deliverPrivate(privateEvent("u1", "张三", "在吗"))
	if !env.waitSends(1, 2*time.Second) {
		t.Fatal("应回复")
	}
	msg := env.fake.at(0).Msg
	if msg.Kind != bot.MessagePrivate {
		t.Fatalf("Kind = %q, want private", msg.Kind)
	}
	if len(msg.Segments) != 1 || msg.Segments[0].Type != bot.SegText {
		t.Fatalf("私聊不应插入 At 段: %+v", msg.Segments)
	}
}

func TestPrivateRuleRegistered(t *testing.T) {
	env := newTestEnv(t, nil, nil)
	rule := env.reg.rule("persona:private")
	if rule == nil {
		t.Fatal("persona:private 未注册")
	}
	if rule.EventType != bot.EventMessage || rule.Kind != bot.MessagePrivate {
		t.Fatalf("private 规则过滤错误: type=%q kind=%q", rule.EventType, rule.Kind)
	}
	group := env.reg.rule("persona:group")
	if group == nil || group.Kind != bot.MessageGroup {
		t.Fatalf("persona:group 规则异常: %+v", group)
	}
}

func TestPrivatePeersHaveDistinctKeys(t *testing.T) {
	env := newTestEnv(t, nil, func(c map[string]any) { c["private_policy"] = "open" })
	st1 := env.waitLoaded(privateEvent("u1", "张三", "hi"))
	st2 := env.waitLoaded(privateEvent("u2", "李四", "hi"))
	if st1.key != "mock:bot1:user:u1" || st2.key != "mock:bot1:user:u2" {
		t.Fatalf("会话键 = %q / %q", st1.key, st2.key)
	}
	if overrideKey(st1.key) == overrideKey(st2.key) {
		t.Fatalf("私聊对端覆盖键碰撞: %q", overrideKey(st1.key))
	}
	if got := overrideKey(st1.key); got != "persona:override:mock:bot1:user:u1" {
		t.Fatalf("覆盖键 = %q", got)
	}
	if st1.kind != bot.MessagePrivate || st1.peerUserID != "u1" {
		t.Fatalf("状态类型错误: kind=%q peer=%q", st1.kind, st1.peerUserID)
	}
}

// TestVisionDecisionRequestShape 走完整链路，断言发往 LLM 的请求体形状。
func TestVisionDecisionRequestShape(t *testing.T) {
	imageEvent := func(channel, user, name string) *bot.Event {
		ev := atEvent(channel, user, name, "看这张图")
		ev.Message.Segments = append(ev.Message.Segments,
			bot.Segment{Type: bot.SegImage, Data: map[string]any{bot.KeyURL: "http://x/a.jpg"}})
		return ev
	}
	run := func(t *testing.T, enabled bool) map[string]any {
		t.Helper()
		got := make(chan map[string]any, 1)
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			select {
			case got <- body:
			default:
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"看到了"}}]}`))
		})
		env := newTestEnv(t, handler, func(c map[string]any) {
			fastConfig(c)
			c["llm_vision_enabled"] = enabled
		})
		env.waitLoaded(imageEvent("g1", "u1", "张三"))
		_ = env.deliver(imageEvent("g1", "u1", "张三"))
		if !env.waitSends(1, 2*time.Second) {
			t.Fatal("应触发一次回复")
		}
		select {
		case body := <-got:
			return body
		case <-time.After(time.Second):
			t.Fatal("未捕获请求体")
			return nil
		}
	}

	userContent := func(t *testing.T, body map[string]any) any {
		t.Helper()
		msgs, ok := body["messages"].([]any)
		if !ok || len(msgs) != 2 {
			t.Fatalf("messages = %#v", body["messages"])
		}
		user, _ := msgs[1].(map[string]any)
		return user["content"]
	}

	t.Run("关闭态为仅含 text 块的数组且不含 image_url", func(t *testing.T) {
		body := run(t, false)
		content := userContent(t, body)
		parts, ok := content.([]any)
		if !ok || len(parts) != 1 {
			t.Fatalf("关闭态 content 应为长度 1 的数组, got %#v", content)
		}
		p0, _ := parts[0].(map[string]any)
		if p0["type"] != "text" {
			t.Fatalf("首个块 = %#v", p0)
		}
		if s, _ := p0["text"].(string); !strings.Contains(s, "看这张图") {
			t.Fatalf("text 块 = %#v", p0["text"])
		}
		if raw, ok := p0["image_url"]; ok {
			t.Fatalf("关闭态不应有 image_url: %#v", raw)
		}
	})

	t.Run("开启态含 image_url 块", func(t *testing.T) {
		body := run(t, true)
		content := userContent(t, body)
		parts, ok := content.([]any)
		if !ok || len(parts) != 2 {
			t.Fatalf("开启态 content 应为长度 2 的数组, got %#v", content)
		}
		p1, _ := parts[1].(map[string]any)
		if p1["type"] != "image_url" {
			t.Fatalf("第二块 = %#v", p1)
		}
		img, _ := p1["image_url"].(map[string]any)
		if img["url"] != "http://x/a.jpg" {
			t.Fatalf("image_url.url = %#v", p1["image_url"])
		}
	})
}
