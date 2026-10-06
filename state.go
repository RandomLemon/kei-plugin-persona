package persona

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/RandomLemon/kei/pkg/bot"
)

// TurnPart 是 Turn 中的一段内容，按原消息段顺序排列。
type TurnPart struct {
	Kind string // turnPartText 或 turnPartImage
	Text string // Kind==turnPartText 时有效
	URL  string // Kind==turnPartImage 时有效；抽取不到 URL 时为空串
}

const (
	turnPartText  = "text"
	turnPartImage = "image"
)

// Turn 是一条聊天记录，供历史渲染与计数使用。
type Turn struct {
	At     time.Time
	UserID string
	Name   string
	Text   string
	Self   bool
	IsBot  bool

	Parts []TurnPart // 原消息的分段结构（含图片槽位）；为空表示无图片段，按 Text 渲染
}

// channelState 是单个会话的状态。字段语义见 docs/architecture.md 第 4 章。
type channelState struct {
	mu            sync.Mutex
	key           string
	platform      string
	botID         string
	channelID     string
	channelName   string
	kind          bot.MessageKind // 会话类型：group 或 private
	peerUserID    string          // 私聊对端用户 ID（kind == private 时非空）
	history       []Turn
	replyTimes    []time.Time
	lastReplyAt   time.Time
	awaiting      bool
	firstAt       time.Time
	lastMsgAt     time.Time
	timer         *time.Timer
	inflight      bool
	epoch         uint64
	disabled      bool
	persona       string
	loaded        bool
	pending       *pendingInbound // 懒加载完成前暂存的待决入站消息（有界单槽）
	lastSenderID  string
	lastAddressed bool
	pendingKind   string
	replies       int
	skips         int
	llmErrors     int
	dropped       int

	histMax int
}

// appendHistory 追加一条历史，超出上限时丢弃最旧的。
func (st *channelState) appendHistory(t Turn) {
	st.mu.Lock()
	st.appendHistoryLocked(t)
	st.mu.Unlock()
}

// appendHistoryLocked 同 appendHistory，但要求调用方已持有 st.mu。
func (st *channelState) appendHistoryLocked(t Turn) {
	st.history = append(st.history, t)
	if st.histMax > 0 && len(st.history) > st.histMax {
		n := copy(st.history, st.history[len(st.history)-st.histMax:])
		st.history = st.history[:n]
	}
}

// snapshotHistory 返回最近 max 条历史的副本（时间序）。
func (st *channelState) snapshotHistory(max int) []Turn {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.snapshotHistoryLocked(max)
}

func (st *channelState) snapshotHistoryLocked(max int) []Turn {
	if max > 0 && len(st.history) > max {
		out := make([]Turn, max)
		copy(out, st.history[len(st.history)-max:])
		return out
	}
	out := make([]Turn, len(st.history))
	copy(out, st.history)
	return out
}

// repliesInWindow 返回窗口内本会话的回复数，并顺带裁剪过期时间戳。
func (st *channelState) repliesInWindow(now time.Time, d time.Duration) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	cut := now.Add(-d)
	keep := st.replyTimes[:0]
	for _, t := range st.replyTimes {
		if !t.Before(cut) {
			keep = append(keep, t)
		}
	}
	st.replyTimes = keep
	return len(st.replyTimes)
}

// distinctHumans 返回窗口内不同真人发送者数。
func (st *channelState) distinctHumans(now time.Time, d time.Duration) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	cut := now.Add(-d)
	seen := make(map[string]struct{}, len(st.history))
	count := 0
	for i, t := range st.history {
		if t.Self || t.IsBot || t.At.Before(cut) {
			continue
		}
		k := t.UserID
		if k == "" {
			k = "#" + strconv.Itoa(i)
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		count++
	}
	return count
}

// lastOwnTexts 返回最近 n 条自己发送的文本（由近及远）。
func (st *channelState) lastOwnTexts(n int) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, n)
	for i := len(st.history) - 1; i >= 0 && len(out) < n; i-- {
		if st.history[i].Self {
			out = append(out, st.history[i].Text)
		}
	}
	return out
}

// stopTimer 停止并清空定时器。
func (st *channelState) stopTimer() {
	st.mu.Lock()
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	st.mu.Unlock()
}

// reset 清空历史、覆盖与计数器，并置为开启、递增 epoch。
func (st *channelState) reset() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.history = st.history[:0]
	st.replyTimes = nil
	st.lastReplyAt = time.Time{}
	st.awaiting = false
	st.firstAt = time.Time{}
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	st.disabled = false
	st.persona = ""
	st.pending = nil
	st.epoch++
	st.replies, st.skips, st.llmErrors, st.dropped = 0, 0, 0, 0
}

// info 返回渲染提示词所需的会话元信息。
func (st *channelState) info() (platform, botID, channelName, channelID string, kind bot.MessageKind) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.platform, st.botID, st.channelName, st.channelID, st.kind
}

func (st *channelState) isDisabled() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.disabled
}

func (st *channelState) isLoaded() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.loaded
}

// pendingInbound 是 Storage 懒加载完成前暂存的一条待决入站消息。
//
// 单槽有界：加载完成前到达的多条消息只保留最新一条——更早的已写入 history，
// 仍会随本轮生成交给 LLM，只有真正的最新一条需要补判。
type pendingInbound struct {
	ev    *bot.Event
	text  string
	epoch uint64
}

// deferOrLoaded 原子地处理懒加载状态：未加载则把本条入站消息存入单槽并返回 false，
// 已加载则返回 true（调用方继续走 decide）。
//
// 与 finishLoad 共用 st.mu，因此「暂存」与「置 loaded 并取走暂存」互斥：
// 每条消息要么立即被判决策，要么恰好被补判一次，不会两者皆失。
func (st *channelState) deferOrLoaded(ev *bot.Event, text string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.loaded {
		st.pending = &pendingInbound{ev: ev, text: text, epoch: st.epoch}
		return false
	}
	return true
}

// channelKey 计算会话键：platform:botID:channelID，频道缺失时回落 platform:botID:user:<senderID>。
func channelKey(ev *bot.Event) (key, platform, botID, channelID string) {
	platform, botID = ev.Platform, ev.BotID
	if ev.Channel != nil && ev.Channel.ID != "" {
		channelID = ev.Channel.ID
	}
	if channelID != "" {
		return platform + ":" + botID + ":" + channelID, platform, botID, channelID
	}
	sender := ""
	if ev.Sender != nil {
		sender = ev.Sender.ID
	}
	return platform + ":" + botID + ":user:" + sender, platform, botID, ""
}

// stateFor 返回（必要时创建）会话状态；首次创建时异步触发 Storage 懒加载。
func (p *Plugin) stateFor(ev *bot.Event) *channelState {
	key, platform, botID, channelID := channelKey(ev)

	p.mu.RLock()
	st := p.channels[key]
	p.mu.RUnlock()
	if st != nil {
		return st
	}

	p.mu.Lock()
	if st = p.channels[key]; st != nil {
		p.mu.Unlock()
		return st
	}
	channelName := ""
	peerUserID := ""
	if ev.Channel != nil {
		channelName = ev.Channel.Name
	}
	kind := ev.Message.Kind
	if kind == bot.MessagePrivate {
		peerUserID = ev.Sender.ID
		if channelName == "" {
			channelName = ev.Sender.Name // 私聊的「会话显示名」= 对端显示名
		}
	}
	st = &channelState{
		key: key, platform: platform, botID: botID, channelID: channelID,
		channelName: channelName, kind: kind, peerUserID: peerUserID,
		histMax: p.cfg.contextMaxMessages,
	}
	p.channels[key] = st
	p.evictLocked()
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.restoreState(st)
	}()
	return st
}

// evictLocked 在会话数超限时按最近使用时间淘汰非忙状态。调用方需持有写锁。
func (p *Plugin) evictLocked() {
	max := p.cfg.contextMaxChannels
	if max <= 0 || len(p.channels) <= max {
		return
	}
	type cand struct {
		key string
		at  time.Time
	}
	cands := make([]cand, 0, len(p.channels))
	for k, st := range p.channels {
		st.mu.Lock()
		// 有暂存待判消息的状态视同忙：淘汰它等于丢掉这条消息。
		busy := st.inflight || st.awaiting || st.pending != nil
		at := st.lastMsgAt
		if st.lastReplyAt.After(at) {
			at = st.lastReplyAt
		}
		st.mu.Unlock()
		if !busy {
			cands = append(cands, cand{key: k, at: at})
		}
	}
	for len(p.channels) > max && len(cands) > 0 {
		idx := 0
		for i := range cands {
			if cands[i].at.Before(cands[idx].at) {
				idx = i
			}
		}
		delete(p.channels, cands[idx].key)
		cands = append(cands[:idx], cands[idx+1:]...)
	}
}

// overrideKey 返回会话覆盖的 Storage 键。
func overrideKey(sessionKey string) string {
	return "persona:override:" + sessionKey
}

// overrideValue 是覆盖值持久化后的 JSON 形状。
type overrideValue struct {
	Persona  string `json:"persona"`
	Disabled bool   `json:"disabled"`
}

// restoreState 异步读取会话覆盖；读不到视为无覆盖，完成后补判懒加载期间暂存的消息。
func (p *Plugin) restoreState(st *channelState) {
	var (
		persona  string
		disabled bool
		restored bool
	)
	if p.ctx != nil && p.store != nil {
		ctx, cancel := context.WithTimeout(p.ctx, storageTimeout)
		defer cancel()
		raw, err := p.store.Get(ctx, overrideKey(st.key))
		switch {
		case err == nil:
			var v overrideValue
			if json.Unmarshal(raw, &v) == nil {
				persona, disabled, restored = v.Persona, v.Disabled, true
			} else {
				p.log.Warn("persona: 覆盖数据解析失败", "channel", st.key)
			}
		case errors.Is(err, bot.ErrNotFound):
		default:
			p.log.Warn("persona: 读取覆盖失败", "channel", st.key, "err", err)
		}
	}
	p.finishLoad(st, persona, disabled, restored)
}

// finishLoad 写回覆盖、置 loaded，并在同一临界区内取走懒加载期间暂存的入站消息补判。
//
// 「取走暂存」与 deferOrLoaded 的「暂存」互斥，因此不存在消息被丢弃的窗口；
// 补判同样禁止网络调用，只做判定与布防（见 decide）。
func (p *Plugin) finishLoad(st *channelState, persona string, disabled, restored bool) {
	st.mu.Lock()
	if restored {
		st.persona = persona
		st.disabled = disabled
	}
	st.loaded = true
	pend := st.pending
	st.pending = nil
	if pend != nil && pend.epoch != st.epoch {
		pend = nil // 期间被 /persona reset 代次作废，暂存消息随之丢弃
	}
	st.mu.Unlock()

	if pend == nil || p.ctx == nil || p.ctx.Err() != nil {
		return
	}
	_ = p.decide(st, st.key, pend.ev, pend.text)
}

// saveOverride 写穿透持久化会话覆盖（异步、带超时、失败只 warn）。
//
// 写入经 persist：Stop 会等它落库，且写入后立即关闭不丢数据。
func (p *Plugin) saveOverride(st *channelState) {
	if p.store == nil {
		return
	}
	st.mu.Lock()
	v := overrideValue{Persona: st.persona, Disabled: st.disabled}
	key := overrideKey(st.key)
	st.mu.Unlock()

	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	p.persist("persona: 写入覆盖失败", func(ctx context.Context) error {
		return p.store.Set(ctx, key, data, 0)
	}, "channel", key)
}

// semaphore 是非阻塞的计数信号量。
type semaphore struct{ ch chan struct{} }

func newSemaphore(n int) *semaphore {
	if n < 1 {
		n = 1
	}
	return &semaphore{ch: make(chan struct{}, n)}
}

// TryAcquire 非阻塞获取，失败返回 false。
func (s *semaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release 释放一个名额。
func (s *semaphore) Release() { <-s.ch }

// idRing 是固定容量的消息 ID 环，用于引用寻址判定。
type idRing struct {
	mu   sync.Mutex
	ids  []string
	next int
}

func newIDRing(size int) *idRing {
	if size < 1 {
		size = 1
	}
	return &idRing{ids: make([]string, size)}
}

// Add 写入一个消息 ID。
func (r *idRing) Add(id string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	r.ids[r.next] = id
	r.next = (r.next + 1) % len(r.ids)
	r.mu.Unlock()
}

// Has 判断消息 ID 是否在环内。
func (r *idRing) Has(id string) bool {
	if r == nil || id == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.ids {
		if x == id {
			return true
		}
	}
	return false
}

// runeCount 返回字符串的 rune 数。
func runeCount(s string) int { return utf8.RuneCountInString(s) }
