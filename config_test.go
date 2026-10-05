package persona

import (
	"context"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(bot.NewConfig(map[string]any{
		"personas":    map[string]any{"default": "普通群友"},
		"llm_api_key": "k",
		"llm_model":   "m",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"default_persona", cfg.defaultPersona, "default"},
		{"llm_base_url", cfg.llmBaseURL, defaultLLMBaseURL},
		{"llm_temperature", cfg.llmTemperature, 0.8},
		{"llm_max_tokens", cfg.llmMaxTokens, 200},
		{"llm_timeout", cfg.llmTimeout, 20 * time.Second},
		{"llm_max_retries", cfg.llmMaxRetries, 1},
		{"llm_skip_token", cfg.llmSkipToken, "[SKIP]"},
		{"llm_history_max_chars", cfg.llmHistoryMaxChars, 4000},
		{"trigger_min_chars", cfg.triggerMinChars, 2},
		{"ignore_bots", cfg.ignoreBots, true},
		{"respond_to_commands", cfg.respondToCommands, false},
		{"group_policy", cfg.groupPolicy, "open"},
		{"private_policy", cfg.privatePolicy, "off"},
		{"mention_reply_probability", cfg.mentionReplyProbability, 1.0},
		{"mention_min_interval", cfg.mentionMinInterval, 10 * time.Second},
		{"random_enabled", cfg.randomEnabled, true},
		{"random_probability", cfg.randomProbability, 0.12},
		{"random_cooldown", cfg.randomCooldown, 90 * time.Second},
		{"random_max_per_hour", cfg.randomMaxPerHour, 6},
		{"random_min_participants", cfg.randomMinParticipants, 2},
		{"random_activity_window", cfg.randomActivityWindow, 5 * time.Minute},
		{"batch_window", cfg.batchWindow, 2500 * time.Millisecond},
		{"batch_max_window", cfg.batchMaxWindow, 8 * time.Second},
		{"context_max_messages", cfg.contextMaxMessages, 20},
		{"context_max_channels", cfg.contextMaxChannels, 512},
		{"reply_max_chars", cfg.replyMaxChars, 200},
		{"reply_mention_sender", cfg.replyMentionSender, false},
		{"reply_dedupe", cfg.replyDedupe, true},
		{"limits_max_concurrent", cfg.limitsMaxConcurrent, 2},
		{"debug_prompts", cfg.debugPrompts, false},
		{"llm_vision_enabled", cfg.llmVisionEnabled, false},
		{"llm_vision_max_images", cfg.llmVisionMaxImages, 4},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	pf := cfg.personas["default"]
	if pf.DisplayName != "default" || pf.Temperature != 0.8 || pf.MaxTokens != 200 || pf.SkipToken != "[SKIP]" {
		t.Errorf("人格默认回落错误: %+v", pf)
	}
	if len(cfg.groupList) != 0 || len(cfg.privateList) != 0 {
		t.Errorf("名单默认应为空: group=%v private=%v", cfg.groupList, cfg.privateList)
	}
	if cfg.randomTimezone == nil {
		t.Error("randomTimezone 未解析")
	}
}

func TestLoadConfigAPIKeyOptional(t *testing.T) {
	cfg, err := loadConfig(bot.NewConfig(map[string]any{
		"personas":  map[string]any{"default": "普通群友"},
		"llm_model": "m",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.llmAPIKey != "" {
		t.Fatalf("llmAPIKey = %q, want empty", cfg.llmAPIKey)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"personas":     map[string]any{"default": "p"},
			"llm_base_url": "http://example.test/v1",
			"llm_api_key":  "k",
			"llm_model":    "m",
		}
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"default_persona 未定义", func(c map[string]any) { c["default_persona"] = "cat" },
			"persona: 配置错误 default_persona=cat: 未在 personas 中定义"},
		{"personas 为空", func(c map[string]any) { c["personas"] = map[string]any{} },
			"persona: 配置错误 personas=: 必须是非空对象"},
		{"random_probability 越界", func(c map[string]any) { c["random_probability"] = 1.5 },
			"persona: 配置错误 random_probability=1.5: 必须是 0..1 之间的小数"},
		{"quiet_hours 格式", func(c map[string]any) { c["random_quiet_hours"] = "23:00" },
			"persona: 配置错误 random_quiet_hours=23:00: 必须是 HH:MM-HH:MM 格式"},
		{"context_max_messages 越界", func(c map[string]any) { c["context_max_messages"] = 0 },
			"persona: 配置错误 context_max_messages=0: 必须 >= 1"},
		{"prompt 为空", func(c map[string]any) {
			c["personas"] = map[string]any{"default": map[string]any{"prompt": ""}}
		}, "persona: 配置错误 personas.default.prompt=: prompt 不能为空"},
		{"bindings channel_id 为空", func(c map[string]any) {
			c["bindings"] = []any{map[string]any{"persona": "default"}}
		}, "persona: 配置错误 bindings.channel_id=: channel_id 不能为空"},
		{"bindings persona 未定义", func(c map[string]any) {
			c["bindings"] = []any{map[string]any{"channel_id": "g1", "persona": "nope"}}
		}, "persona: 配置错误 bindings.persona=nope: 未在 personas 中定义"},
		{"时区非法", func(c map[string]any) { c["random_timezone"] = "No/SuchZone" },
			"persona: 配置错误 random_timezone=No/SuchZone: 不是合法时区"},
		{"llm_max_tokens 越界", func(c map[string]any) { c["llm_max_tokens"] = 0 },
			"persona: 配置错误 llm_max_tokens=0: 必须 >= 1"},
		{"random_min_participants 越界", func(c map[string]any) { c["random_min_participants"] = 0 },
			"persona: 配置错误 random_min_participants=0: 必须 >= 1"},
		{"llm_timeout 为负", func(c map[string]any) { c["llm_timeout"] = -1 },
			"persona: 配置错误 llm_timeout=-1s: 必须 >= 0"},
		{"persona temperature 类型错误", func(c map[string]any) {
			c["personas"] = map[string]any{"default": map[string]any{"prompt": "p", "temperature": "hot"}}
		}, "persona: 配置错误 personas.default.temperature=hot: 必须 >= 0"},
		{"persona max_tokens 类型错误", func(c map[string]any) {
			c["personas"] = map[string]any{"default": map[string]any{"prompt": "p", "max_tokens": "many"}}
		}, "persona: 配置错误 personas.default.max_tokens=many: 必须 >= 1"},
		{"group_policy 非法", func(c map[string]any) { c["group_policy"] = "all" },
			"persona: 配置错误 group_policy=all: 必须是 off|open|whitelist|blacklist 之一"},
		{"private_policy 非法", func(c map[string]any) { c["private_policy"] = "on" },
			"persona: 配置错误 private_policy=on: 必须是 off|open|whitelist|blacklist 之一"},
		{"llm_vision_max_images 为负", func(c map[string]any) { c["llm_vision_max_images"] = -1 },
			"persona: 配置错误 llm_vision_max_images=-1: 必须 >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)
			gotErr := func() string {
				_, err := loadConfig(bot.NewConfig(cfg))
				if err == nil {
					return ""
				}
				return err.Error()
			}()
			if gotErr != tc.want {
				t.Fatalf("错误 = %q, want %q", gotErr, tc.want)
			}
		})
	}
}

func TestSetupRequiresNetworkPermission(t *testing.T) {
	p := &Plugin{}
	pc := bot.PluginContext{
		Name:   "persona",
		Config: bot.NewConfig(baseConfigMap("http://x")),
	}
	ctx := bot.WithPluginContext(context.Background(), pc)
	err := p.Setup(ctx, &fakeRegistrar{})
	if err == nil || err.Error() != "persona: 需要 network 权限" {
		t.Fatalf("err = %v, want 需要 network 权限", err)
	}
}

func TestSetupRequiresPluginContext(t *testing.T) {
	p := &Plugin{}
	if err := p.Setup(context.Background(), &fakeRegistrar{}); err == nil {
		t.Fatal("缺少 PluginContext 应报错")
	}
}
