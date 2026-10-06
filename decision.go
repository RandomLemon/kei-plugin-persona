package persona

import (
	"context"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
	"github.com/RandomLemon/kei/pkg/message"
)

// handleGroupMessage 处理群消息。必须毫秒级返回，禁止网络调用或阻塞等待。
func (p *Plugin) handleGroupMessage(ctx context.Context, ev *bot.Event, r bot.Reply) error {
	if ev.Sender == nil {
		return p.skipLog("", "no_sender", "")
	}
	if ev.Message == nil || ev.Message.Kind != bot.MessageGroup {
		return p.skipLog("", "not_group", "")
	}
	return p.handleChat(ctx, ev, r)
}

// handlePrivateMessage 处理私聊消息。必须毫秒级返回，禁止网络调用或阻塞等待。
func (p *Plugin) handlePrivateMessage(ctx context.Context, ev *bot.Event, r bot.Reply) error {
	if ev.Sender == nil {
		return p.skipLog("", "no_sender", "")
	}
	if ev.Message == nil || ev.Message.Kind != bot.MessagePrivate {
		return p.skipLog("", "not_private", "")
	}
	return p.handleChat(ctx, ev, r)
}

// handleChat 是群聊与私聊共用的入站处理：过滤 → 记历史 → 判决策 → 布防。
func (p *Plugin) handleChat(ctx context.Context, ev *bot.Event, r bot.Reply) error {
	key, _, _, channelID := channelKey(ev)
	if ev.Command != nil && ev.Command.Name == "persona" {
		return p.skipLog(key, "command", ev.Sender.ID)
	}
	// 名单策略在建立会话状态之前判定：被拒的会话不建状态、不进历史、不触发懒加载。
	if ev.Message.Kind == bot.MessagePrivate {
		if !p.policyAllowsPrivate(ev.Sender.ID) {
			return p.skipLog(key, "not_allowed", ev.Sender.ID)
		}
	} else if !p.policyAllowsGroup(channelID) {
		return p.skipLog(key, "not_allowed", ev.Sender.ID)
	}
	text, parts := renderMessage(ev.Message)
	st := p.stateFor(ev)
	if ev.Command == nil {
		st.appendHistory(p.userTurn(ev, text, parts))
	}
	// 懒加载未完成：暂存本条（有界单槽），加载完成后由 restoreState 补判，
	// 保证新会话的第一条消息不被静默丢弃；加载期间到达的更新消息会覆盖单槽。
	if !st.deferOrLoaded(ev, text) {
		return p.skipLog(key, "loading", ev.Sender.ID)
	}
	return p.decide(st, key, ev, text)
}

// decide 是「会话已加载」后的完整判定链：过滤 → 寻址 → 概率 → 布防。
//
// 必须毫秒级返回：只做内存判定与定时器布防，禁止任何网络调用或阻塞等待。
// 除入站 Handler 外，restoreState 补判懒加载期间暂存的消息时也走这里。
func (p *Plugin) decide(st *channelState, key string, ev *bot.Event, text string) error {
	if st.isDisabled() {
		return p.skipLog(key, "channel_off", ev.Sender.ID)
	}
	if ev.Sender.IsBot && p.cfg.ignoreBots {
		return p.skipLog(key, "bot_sender", ev.Sender.ID)
	}
	if ev.Command != nil && !p.cfg.respondToCommands {
		return p.skipLog(key, "command", ev.Sender.ID)
	}
	if text == "" {
		return p.skipLog(key, "empty_text", ev.Sender.ID)
	}
	if runeCount(text) < p.cfg.triggerMinChars {
		return p.skipLog(key, "too_short", ev.Sender.ID)
	}

	now := p.now()
	st.mu.Lock()
	last := st.lastReplyAt
	st.mu.Unlock()

	if ev.Message.Kind == bot.MessagePrivate || p.isAddressed(ev, text) {
		if now.Sub(last) < p.cfg.mentionMinInterval {
			return p.skipLog(key, "cooldown", ev.Sender.ID)
		}
		if p.randFloat() >= p.cfg.mentionReplyProbability {
			return p.skipLog(key, "probability", ev.Sender.ID)
		}
		return p.schedule(st, "addressed", ev)
	}

	if !p.cfg.randomEnabled {
		return p.skipLog(key, "not_addressed", ev.Sender.ID)
	}
	st.mu.Lock()
	busy := st.inflight || st.awaiting
	st.mu.Unlock()
	if busy {
		return p.skipLog(key, "inflight", ev.Sender.ID)
	}
	if p.inQuietHours(now) {
		return p.skipLog(key, "quiet_hours", ev.Sender.ID)
	}
	if now.Sub(last) < p.cfg.randomCooldown {
		return p.skipLog(key, "cooldown", ev.Sender.ID)
	}
	if st.repliesInWindow(now, time.Hour) >= p.cfg.randomMaxPerHour {
		return p.skipLog(key, "hour_quota", ev.Sender.ID)
	}
	if st.distinctHumans(now, p.cfg.randomActivityWindow) < p.cfg.randomMinParticipants {
		return p.skipLog(key, "min_participants", ev.Sender.ID)
	}
	if p.randFloat() >= p.cfg.randomProbability {
		return p.skipLog(key, "probability", ev.Sender.ID)
	}
	return p.schedule(st, "random", ev)
}

// userTurn 由事件构造一条入站历史。
func (p *Plugin) userTurn(ev *bot.Event, text string, parts []TurnPart) Turn {
	at := ev.Time
	if at.IsZero() {
		at = p.now()
	}
	t := Turn{At: at, Text: text, Parts: parts}
	if ev.Sender != nil {
		t.UserID = ev.Sender.ID
		t.Name = ev.Sender.Name
		t.IsBot = ev.Sender.IsBot
	}
	return t
}

// isAddressed 判定本条消息是否寻址本机器人。
func (p *Plugin) isAddressed(ev *bot.Event, text string) bool {
	return hasMention(ev.Message, p.cfg.selfIDs) ||
		p.isReplyToSelf(ev.Message) ||
		hasKeyword(text, p.cfg.triggerKeywords)
}

// atAllSentinel 是 OneBot 等平台表示「@全体成员」的 At 目标值。
//
// 它不是一个真实用户 ID，任何情况下都不应算作「@机器人」；否则 @全体 会触发
// 回复，且与 self_ids 是否配置无关（见 participation.md §7.2）。
const atAllSentinel = "all"

func hasMention(msg *bot.Message, selfIDs []string) bool {
	if msg == nil {
		return false
	}
	for _, seg := range msg.Segments {
		if seg.Type != bot.SegAt {
			continue
		}
		id := strOf(seg.Data[bot.KeyUserID])
		if id == atAllSentinel {
			continue
		}
		if len(selfIDs) == 0 {
			return true
		}
		for _, s := range selfIDs {
			if s == id {
				return true
			}
		}
	}
	return false
}

func (p *Plugin) isReplyToSelf(msg *bot.Message) bool {
	if msg == nil {
		return false
	}
	for _, seg := range msg.Segments {
		if seg.Type != bot.SegReply {
			continue
		}
		if p.replyIDs.Has(strOf(seg.Data[bot.KeyMessageID])) {
			return true
		}
	}
	return false
}

func hasKeyword(text string, keywords []string) bool {
	if len(keywords) == 0 {
		return false
	}
	low := strings.ToLower(text)
	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		if strings.Contains(low, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// inQuietHours 判断当前是否处于静默时段（支持跨零点）。
func (p *Plugin) inQuietHours(now time.Time) bool {
	spec := p.cfg.randomQuietHours
	if spec == "" {
		return false
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return false
	}
	start, ok1 := parseHM(parts[0])
	end, ok2 := parseHM(parts[1])
	if !ok1 || !ok2 || start == end {
		return false
	}
	t := now.In(p.cfg.randomTimezone)
	cur := t.Hour()*60 + t.Minute()
	if start < end {
		return cur >= start && cur < end
	}
	return cur >= start || cur < end
}

// parseHM 解析 HH:MM 为当日分钟数。
func parseHM(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// schedule 布防批处理窗口。
func (p *Plugin) schedule(st *channelState, kind string, ev *bot.Event) error {
	st.mu.Lock()
	now := p.now()
	st.awaiting = true
	st.pendingKind = kind
	st.lastAddressed = kind == "addressed"
	if ev.Sender != nil {
		st.lastSenderID = ev.Sender.ID
	}
	if st.firstAt.IsZero() {
		st.firstAt = now
	}
	st.lastMsgAt = now
	delay := p.cfg.batchWindow
	if m := st.firstAt.Add(p.cfg.batchMaxWindow).Sub(now); m < delay {
		delay = m
	}
	if delay < 0 {
		delay = 0
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	epoch := st.epoch
	key := st.key
	st.timer = time.AfterFunc(delay, func() { p.onBatch(st, epoch) })
	st.mu.Unlock()

	p.log.Debug("persona 决策", "decision", "reply", "kind", kind, "channel", key)
	return nil
}

// onBatch 定时器回调：窗口未结束则重新布防，否则启动生成。
func (p *Plugin) onBatch(st *channelState, epoch uint64) {
	st.mu.Lock()
	if st.epoch != epoch || !st.awaiting {
		st.mu.Unlock()
		return
	}
	now := p.now()
	if now.Sub(st.lastMsgAt) < p.cfg.batchWindow && now.Sub(st.firstAt) < p.cfg.batchMaxWindow {
		delay := p.cfg.batchWindow - now.Sub(st.lastMsgAt)
		if m := st.firstAt.Add(p.cfg.batchMaxWindow).Sub(now); m < delay {
			delay = m
		}
		if delay < 0 {
			delay = 0
		}
		st.timer = time.AfterFunc(delay, func() { p.onBatch(st, epoch) })
		st.mu.Unlock()
		return
	}
	st.awaiting = false
	st.firstAt = time.Time{}
	if st.inflight {
		st.mu.Unlock()
		return
	}
	st.inflight = true
	history := st.snapshotHistoryLocked(p.cfg.contextMaxMessages)
	st.mu.Unlock()

	p.wg.Add(1)
	go p.generate(st, epoch, history)
}

// generate 在后台协程内完成 LLM 调用、清洗与发送。
func (p *Plugin) generate(st *channelState, epoch uint64, history []Turn) {
	defer p.wg.Done()
	defer func() {
		st.mu.Lock()
		st.inflight = false
		st.mu.Unlock()
	}()

	if !p.sem.TryAcquire() {
		p.bumpDropped(st)
		_ = p.skipLog(st.key, "semaphore_full", st.senderID())
		return
	}
	defer p.sem.Release()

	st.mu.Lock()
	if st.epoch != epoch {
		st.mu.Unlock()
		_ = p.skipLog(st.key, "stale", st.senderID())
		return
	}
	pendingKind := st.pendingKind
	sender := st.lastSenderID
	chatKind := st.kind
	st.mu.Unlock()

	if p.ctx == nil {
		return
	}
	personaName, _ := p.resolvePersona(st)
	pf := p.cfg.personas[personaName]

	trimmed := p.trimHistory(history)
	slots := p.fetchVisionSlots(p.ctx, p.selectVisionSlots(trimmed))
	req := completionRequest{
		System:      p.renderSystemPrompt(personaName, st, history),
		User:        p.renderUserContent(chatKind, trimmed, slots),
		Temperature: pf.Temperature,
		MaxTokens:   pf.MaxTokens,
	}
	content, err := p.completer.Complete(p.ctx, req)
	if err != nil {
		p.bumpLLMError(st)
		p.log.Warn("persona: LLM 调用失败", "channel", st.key, "err", err)
		_ = p.skipLog(st.key, "llm_error", sender)
		return
	}
	if p.ctx.Err() != nil {
		return
	}

	reply, reason := p.cleanReply(content, pf.SkipToken, st)
	if reason != "" {
		p.bumpSkip(st)
		_ = p.skipLog(st.key, reason, sender)
		return
	}

	st.mu.Lock()
	if st.epoch != epoch {
		st.mu.Unlock()
		_ = p.skipLog(st.key, "stale", sender)
		return
	}
	platform, botID, channelID := st.platform, st.botID, st.channelID
	peer := st.peerUserID
	mention := p.cfg.replyMentionSender && st.lastAddressed && chatKind == bot.MessageGroup
	st.mu.Unlock()

	segs := make([]bot.Segment, 0, 2)
	if mention && sender != "" {
		segs = append(segs, message.At(sender))
	}
	segs = append(segs, message.Text(reply))

	var msg *bot.Message
	var target bot.Target
	if chatKind == bot.MessagePrivate {
		msg = message.Private(segs...)
		target = bot.Target{Platform: platform, BotID: botID, UserID: peer, Kind: bot.MessagePrivate}
	} else {
		msg = message.Group(segs...)
		target = bot.Target{Platform: platform, BotID: botID, ChannelID: channelID, Kind: bot.MessageGroup}
	}

	res, err := p.api.Send(p.ctx, target, msg)
	if err != nil {
		p.log.Warn("persona: 发送失败", "channel", st.key, "err", err)
		_ = p.skipLog(st.key, "send_error", sender)
		return
	}

	now := p.now()
	st.mu.Lock()
	st.lastReplyAt = now
	st.replyTimes = append(st.replyTimes, now)
	st.appendHistoryLocked(Turn{At: now, Name: p.personaDisplayName(personaName), Text: reply, Self: true})
	st.replies++
	st.mu.Unlock()
	if res != nil {
		p.replyIDs.Add(res.MessageID)
	}
	p.log.Debug("persona 决策", "decision", "reply", "kind", pendingKind, "channel", st.key)
}

// skipLog 输出决策日志的 skip 行。
func (p *Plugin) skipLog(key, reason, sender string) error {
	p.log.Debug("persona 决策", "decision", "skip", "reason", reason, "channel", key, "sender", sender)
	return nil
}

func (st *channelState) senderID() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lastSenderID
}

func (p *Plugin) bumpSkip(st *channelState) {
	st.mu.Lock()
	st.skips++
	st.mu.Unlock()
	p.statSkips.Add(1)
}

func (p *Plugin) bumpLLMError(st *channelState) {
	st.mu.Lock()
	st.llmErrors++
	st.mu.Unlock()
	p.statLLMErrors.Add(1)
}

func (p *Plugin) bumpDropped(st *channelState) {
	st.mu.Lock()
	st.dropped++
	st.mu.Unlock()
}
