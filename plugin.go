package persona

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

const (
	// replyIDRingSize 是「最近发送消息 ID 环」的容量，用于引用寻址判定。
	replyIDRingSize = 128
	// storageTimeout 是单次 Storage 读写的超时。
	storageTimeout = time.Second
)

// Plugin 是 persona 插件的实例状态。
//
// 实例在 init() 时构造（此时无配置、无依赖），运行期依赖只能在 Setup/Start
// 阶段装配。全部可变状态都挂在本结构上，禁止包级可变状态。
type Plugin struct {
	cfg   *config
	api   bot.BotAPI
	log   *slog.Logger
	store bot.Storage
	// imgClient 是图片下载客户端（派生自 PluginContext.HTTPClient 的 transport，
	// 装了地址限制拨号器）；单测可整体替换为桩。
	imgClient *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// writeMu 保护 closing；写穿透与 Stop 用它串行化「是否已进入关闭」的判定。
	writeMu sync.Mutex
	// closing 为真表示 Stop 已开始，此后不再接受新的写穿透。
	closing bool
	// writeWG 跟踪在途的写穿透协程：Stop 先等它们结束再取消 p.ctx，
	// 否则派生自 p.ctx 的写入会被当场取消（持久化后端上会丢数据）。
	writeWG sync.WaitGroup

	mu       sync.RWMutex
	channels map[string]*channelState

	policyMu sync.RWMutex
	policy   policyState

	sem      *semaphore
	replyIDs *idRing

	// 注入缝：单测替换为桩。
	now       func() time.Time
	randFloat func() float64
	completer completer

	statSkips     atomic.Int64
	statLLMErrors atomic.Int64
}

// Setup 读取 PluginContext、校验配置并注册规则。
//
// 配置非法或缺少 network 权限时返回错误，kei 会阻止启动。
func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	pc, ok := bot.PluginContextFrom(ctx)
	if !ok {
		return errors.New("persona: 缺少 PluginContext")
	}
	return p.setup(pc, reg)
}

// setup 是 Setup 的可测内核：直接接收已解析的 PluginContext。
func (p *Plugin) setup(pc bot.PluginContext, reg bot.Registrar) error {
	if pc.HTTPClient == nil {
		return errors.New("persona: 需要 network 权限")
	}
	cfg, err := loadConfig(pc.Config)
	if err != nil {
		return err
	}
	log := pc.Logger
	if log == nil {
		log = slog.Default()
	}
	p.cfg = cfg
	p.api = pc.Bot
	p.log = log
	p.store = pc.Storage
	p.imgClient = newVisionClient(pc.HTTPClient)
	p.now = time.Now
	p.randFloat = rand.Float64
	p.channels = make(map[string]*channelState)
	p.policy = policyState{
		groupMode:   cfg.groupPolicy,
		groupList:   cfg.groupList,
		privateMode: cfg.privatePolicy,
		privateList: cfg.privateList,
	}
	p.sem = newSemaphore(cfg.limitsMaxConcurrent)
	p.replyIDs = newIDRing(replyIDRingSize)
	p.completer = newOpenAIClient(cfg, pc.HTTPClient, log)

	reg.OnEvent(bot.EventMessage, p.handleGroupMessage,
		bot.WithKind(bot.MessageGroup), bot.WithPriority(0), bot.WithID("persona:group"))

	reg.OnEvent(bot.EventMessage, p.handlePrivateMessage,
		bot.WithKind(bot.MessagePrivate), bot.WithPriority(0), bot.WithID("persona:private"))

	reg.OnCommand("persona", p.handleCommand,
		bot.WithAdmin(), bot.WithPriority(100), bot.WithID("persona:admin"))
	return nil
}

// Start 保存插件级 ctx、同步加载名单策略并启动后台能力。
//
// kei 传给 Start 的 ctx 是「阶段上下文」：阶段函数返回后立即被 cancel
// （见 internal/pluginmgr/manager.go 的 run）。因此这里用 context.WithoutCancel
// 摘掉它的取消与超时、只保留值，再由 Stop 经 p.cancel 终止全部后台工作；
// 直接用阶段 ctx 会让所有异步链路（懒加载、写穿透、LLM、发送）当场失效。
func (p *Plugin) Start(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(context.WithoutCancel(ctx))
	p.loadPolicy(ctx)
	return nil
}

// persist 异步执行一次写穿透：带 storageTimeout 超时，失败只 warn。
//
// Stop 之后拒绝新写入；在途写入由 writeWG 跟踪，Stop 会先等它们结束再取消
// p.ctx——顺序反了的话，派生自 p.ctx 的写入 ctx 会被当场取消，持久化后端
// （sqlite/mysql）上最后一次覆盖或策略变更就丢了。
func (p *Plugin) persist(op string, fn func(ctx context.Context) error, attrs ...any) {
	if p.ctx == nil || p.store == nil {
		return
	}
	p.writeMu.Lock()
	if p.closing {
		p.writeMu.Unlock()
		return
	}
	p.writeWG.Add(1)
	p.writeMu.Unlock()

	go func() {
		defer p.writeWG.Done()
		ctx, cancel := context.WithTimeout(p.ctx, storageTimeout)
		defer cancel()
		if err := fn(ctx); err != nil {
			p.log.Warn(op, append(attrs, "err", err)...)
		}
	}()
}

// Stop 标记关闭、等待在途写穿透结束，再取消插件级 ctx、停止全部定时器并
// 等待后台协程退出；幂等。
func (p *Plugin) Stop(ctx context.Context) error {
	// 先拒绝新写入并等在途写入落库，再取消 p.ctx：写穿透的 ctx 派生自 p.ctx，
	// 先取消会让持久化后端上的最后一次写入被丢弃（见 persist）。每次写入自带
	// storageTimeout，故这里的等待有界。
	p.writeMu.Lock()
	p.closing = true
	p.writeMu.Unlock()
	p.writeWG.Wait()

	if p.cancel != nil {
		p.cancel()
	}
	p.mu.RLock()
	states := make([]*channelState, 0, len(p.channels))
	for _, st := range p.channels {
		states = append(states, st)
	}
	p.mu.RUnlock()
	for _, st := range states {
		st.stopTimer()
	}
	p.wg.Wait()
	return nil
}
