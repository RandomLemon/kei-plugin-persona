package persona

import (
	"context"
	"fmt"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func personaUsage() string {
	return "用法: /persona status | switch [name] | on | off | reset | policy [group|private mode] | list [group|private [add|del id]]"
}

// handleCommand 处理 /persona 管理命令。
func (p *Plugin) handleCommand(ctx context.Context, ev *bot.Event, r bot.Reply) error {
	if ev.Command == nil {
		return nil
	}
	st := p.stateFor(ev)
	args := ev.Command.Args
	if len(args) == 0 {
		return r.Text(personaUsage()).Send(ctx)
	}
	switch args[0] {
	case "status":
		return r.Text(p.statusLine(st)).Send(ctx)
	case "switch":
		if len(args) == 1 {
			name, src := p.resolvePersona(st)
			return r.Text(fmt.Sprintf("persona: persona=%s 来源=%s", name, src)).Send(ctx)
		}
		name := args[1]
		if _, ok := p.cfg.personas[name]; !ok {
			return r.Text("persona: 未找到人格 " + name).Send(ctx)
		}
		st.mu.Lock()
		st.persona = name
		st.epoch++
		st.mu.Unlock()
		p.saveOverride(st)
		return r.Text("persona: persona=" + name).Send(ctx)
	case "on":
		st.mu.Lock()
		st.disabled = false
		st.mu.Unlock()
		p.saveOverride(st)
		return r.Text("persona: 已开启").Send(ctx)
	case "off":
		st.mu.Lock()
		st.disabled = true
		st.epoch++
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		st.mu.Unlock()
		p.saveOverride(st)
		return r.Text("persona: 已关闭").Send(ctx)
	case "reset":
		st.reset()
		p.saveOverride(st)
		return r.Text("persona: 已重置").Send(ctx)
	case "policy":
		if len(args) == 1 {
			return r.Text(p.policyReport()).Send(ctx)
		}
		if len(args) != 3 || !p.setPolicyMode(args[1], args[2]) {
			return r.Text(personaUsage()).Send(ctx)
		}
		return r.Text(p.policyScopeReport(args[1])).Send(ctx)
	case "list":
		switch {
		case len(args) == 1:
			return r.Text(p.listReport()).Send(ctx)
		case len(args) == 2 && (args[1] == scopeGroup || args[1] == scopePrivate):
			return r.Text(p.listScopeReport(args[1])).Send(ctx)
		case len(args) == 4 && (args[1] == scopeGroup || args[1] == scopePrivate) && args[3] != "":
			if args[2] == "add" {
				p.addPolicyID(args[1], args[3])
			} else if args[2] == "del" {
				p.delPolicyID(args[1], args[3])
			} else {
				return r.Text(personaUsage()).Send(ctx)
			}
			return r.Text(p.listScopeReport(args[1])).Send(ctx)
		default:
			return r.Text(personaUsage()).Send(ctx)
		}
	default:
		return r.Text(personaUsage()).Send(ctx)
	}
}

// statusLine 生成 /persona status 的固定格式一行。
func (p *Plugin) statusLine(st *channelState) string {
	now := p.now()
	st.mu.Lock()
	disabled := st.disabled
	hist := len(st.history)
	last := st.lastReplyAt
	st.mu.Unlock()

	name, src := p.resolvePersona(st)
	window := st.repliesInWindow(now, time.Hour)

	state := "开"
	if disabled {
		state = "关"
	}
	lastStr := "从未"
	if !last.IsZero() {
		d := now.Sub(last)
		if d < 0 {
			d = 0
		}
		lastStr = d.Round(time.Second).String() + " 前"
	}
	return fmt.Sprintf(
		"persona: %s · persona=%s(%s) · 历史 %d 条 · 近 1 小时回复 %d/%d · 上次回复 %s · llm 错误 %d · 已跳 %d",
		state, name, src, hist, window, p.cfg.randomMaxPerHour, lastStr,
		p.statLLMErrors.Load(), p.statSkips.Load(),
	)
}
