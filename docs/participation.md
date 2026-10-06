# participation.md — 接话决策模型（第 7 章）

本文覆盖第 7 章：插件最核心的行为——判断「何时接话」。所有参数名与默认值以 [`configuration.md`](configuration.md) §10.1 键表为唯一权威。

决策分三段：**过滤**（7.1）、**寻址与随机参与**（7.2-7.3）、**批处理窗口与生成**（7.4-7.5）。完整伪代码见 7.6，reason token 词表见 7.7，群聊/私聊名单策略见 7.8。

## 7.1 事件过滤

Handler 收到群消息或私聊消息后，按固定顺序过滤。**所有进入 Handler 的消息都先写入历史**（含被下述条件过滤的），例外有三：`/persona` 命令与其它命令消息（命令不入历史）、被名单策略拒绝的会话消息（在记历史前就返回，不建会话状态，见 §7.8）。

| 顺序 | 条件 | 未通过时 reason |
| --- | --- | --- |
| 1 | 会话类型不匹配（群 Handler：`ev.Message == nil` 或 `Kind != bot.MessageGroup`；私聊 Handler：`ev.Message == nil` 或 `Kind != bot.MessagePrivate`） | `not_group` / `not_private` |
| 2 | 无发送者（`ev.Sender == nil`） | `no_sender` |
| 3 | `/persona` 命令（`ev.Command != nil && ev.Command.Name == "persona"`）——**无条件**，且不进历史 | `command` |
| 4 | 名单策略拒绝（群聊按 `ev.Channel.ID`，私聊按 `ev.Sender.ID`；模式见 §7.8）——**在建立会话状态与记历史之前**返回 | `not_allowed` |
| — | 记历史：`ev.Command == nil` 时把本消息写入历史；命令与被拒会话一律不进历史 | — |
| 5 | 会话被 `/persona off` 关闭 | `channel_off` |
| 6 | Storage 懒加载未完成（本条**暂存**进 `pending` 单槽，加载完成后补判一次，见 §7.6） | `loading` |
| 7 | 机器人发送者且 `ignore_bots=true` | `bot_sender` |
| 8 | 命令消息且 `respond_to_commands=false` | `command` |
| 9 | 文本为空 | `empty_text` |
| 10 | 文本短于 `trigger_min_chars`（按 rune 计） | `too_short` |

要点：

- `/persona` 命令在第 3 步就被无条件丢弃，原因见 [`architecture.md`](architecture.md) 第 6 章：`OnCommand("persona", ...)` 与消息规则会被**同时执行**。
- 名单策略在第 4 步判定，早于 `stateFor`：被拒会话不建 `channelState`、不触发 Storage 懒加载、不进历史。诊断时用日志字段 `channel=<会话键>` 配合 `/persona policy` 输出区分拒绝原因。
- 过滤顺序固定，日志 reason 取决于**第一个**未通过的条件。
- 第 6 步（Storage 懒加载未完成）**不丢消息**：本条被暂存进 `channelState.pending` 单槽、按 `loading` 记日志，`Storage.Get` 返回后由 `finishLoad` 补判一次（判定依据是加载后的 `disabled`/`persona` 真值，见 [`architecture.md`](architecture.md) §4.5）。因此首次出现的会话、进程重启后、被 LRU 淘汰后的**第一条消息都会得到回复**；加载窗口内连发多条时只补判最新一条，更早的已进历史。
- `not_group`/`not_private`/`no_sender` 在第 3 步之前，因此它们是「理论上不会发生」的护栏（规则已按 `WithKind` 限定会话类型，但 Handler 仍自检）。

## 7.2 寻址判定

```text
addressed = 私聊 || mention || reply_to_self || keyword
```

- **私聊**：`ev.Message.Kind == bot.MessagePrivate` 一律视为寻址（私聊里对方开口就是对你说话），因此私聊**必回**（受 `mention_min_interval` 与 `mention_reply_probability` 约束，见 §7.3）。
- **mention**：`ev.Message.Segments` 中存在 `bot.SegAt`，且满足——
  - `self_ids` 为空 → 任意指向具体用户的 `At` 视为寻址（`@全体成员` 除外，见下）；
  - `self_ids` 非空 → `segment.Data[bot.KeyUserID]` 命中 `self_ids` 之一。
  - `@全体成员` 在 OneBot 下上报为 `qq="all"`（契约见 [`architecture.md`](architecture.md) 第 6 章），**任何情况下都不算寻址**：@全体不等于 @机器人。
- **reply_to_self**：存在 `bot.SegReply`，其 `segment.Data[bot.KeyMessageID]` 命中「最近发送消息 ID 环」。该环容量常量 `128`，每次 `BotAPI.Send` 成功把 `SendResult.MessageID` 入环。
- **keyword**：文本对 `trigger_keywords` 做**大小写不敏感子串匹配**（`strings.Contains(strings.ToLower(text), strings.ToLower(kw))`）。`trigger_keywords` 为空表示关闭。

寻址判定只需要文本与消息段，不需要网络，因此可在 Handler 内完成。

**列表键的取值口径**：`self_ids`、`trigger_keywords`、`group_list`、`private_list`、`llm_vision_allowed_formats` 五个列表键经 `readStringList` 读取，兼容 `["123"]`、`[123]`（YAML 裸数字）、`123`（裸标量）、`"123,456"`（逗号分隔）四种写法，元素一律按 YAML 语义转成字符串后比对，不做类型断言丢弃。原因见 [`configuration.md`](configuration.md) §10.4（`llm_vision_allowed_formats` 另按该节注归一：去 `image/` 前缀、转小写、`jpg`→`jpeg`）。

## 7.3 随机参与

仅当**非寻址**时进入随机路径。以下条件**依次**判定，全过才排程：

| 顺序 | 条件 | 未通过时 reason |
| --- | --- | --- |
| 1 | `random_enabled = true` | `not_addressed` |
| 2 | 非 `inflight` 且非 `awaiting` | `inflight` |
| 3 | 不在 `random_quiet_hours`（按 `random_timezone` 解释，格式 `HH:MM-HH:MM`，支持跨零点） | `quiet_hours` |
| 4 | 距 `lastReplyAt` ≥ `random_cooldown` | `cooldown` |
| 5 | 近 1 小时本会话回复数 < `random_max_per_hour` | `hour_quota` |
| 6 | `random_activity_window` 内不同真人发送者数 ≥ `random_min_participants` | `min_participants` |
| 7 | `rand.Float64() < random_probability` | `probability` |

寻址路径（`addressed == true`）单独判定，**不受**小时配额与静默时段限制：

| 顺序 | 条件 | 未通过时 reason |
| --- | --- | --- |
| 1 | 距 `lastReplyAt` ≥ `mention_min_interval` | `cooldown` |
| 2 | `rand.Float64() < mention_reply_probability`（默认 `1.0`，即被 @ 必回） | `probability` |

说明：

- 私聊不进入随机路径：私聊消息恒为寻址（§7.2），只走下面的寻址路径，**不受** `random_enabled`、小时配额、静默时段、`min_participants`、`random_cooldown` 限制。
- `random_max_per_hour = 0` 时第 5 条恒不通过，等价于**关闭随机插话**（但寻址路径仍工作）。
- 第 6 条的「不同真人」由 `channelState.distinctHumans(now, random_activity_window)` 统计，只计 `Self == false && IsBot == false` 的 `Turn`，按 `UserID` 去重（`UserID` 为空时按下标计一条）。
- 静默时段语义：`23:00-07:00` 表示跨零点（23:00 起、次日 07:00 止）；不可跨零点时按同日区间。

## 7.4 批处理窗口（像人一样先看完再说话）

判定通过后**不立即生成**，而是在本会话布防一个 `time.AfterFunc` 定时器；窗口内到达的新消息并入**同一轮**生成，让 LLM 看到完整上下文再决定。

三个字段决定算法：`awaiting`（是否已布防）、`firstAt`（本轮首次判定时间）、`lastMsgAt`（本轮最近一条消息时间）。

**确定性算法**

1. 布防（`schedule`）：置 `awaiting = true`、`pendingKind = kind`、`lastAddressed`、`lastSenderID`；`firstAt` 为零则置 `now`；更新 `lastMsgAt = now`。延迟 `delay = min(batch_window, firstAt + batch_max_window - now)`，`delay < 0` 取 0。若已有定时器先 `Stop`，重新布防并捕获当前 `epoch`。
2. 回调（`onBatch`）：
   - `epoch` 已变或 `!awaiting` → 静默返回；
   - 若 `now - lastMsgAt < batch_window` **且** `now - firstAt < batch_max_window` → 重新布防（延迟 `min(batch_window - (now - lastMsgAt), firstAt + batch_max_window - now)`），返回；
   - 否则开始生成：置 `awaiting = false`、`firstAt` 归零；若 `inflight` 已为真则放弃（已有在途生成）；否则置 `inflight = true`、快照历史、启动生成协程。
3. 生成完成（成功或失败）后置 `inflight = false`。

`batch_window`（默认 `2.5s`）是「说话前的静默等待」；`batch_max_window`（默认 `8s`）是「自首次判定起最长等待」上限，防止消息持续刷屏导致永不触发。

## 7.5 生成与发送

- **信号量**：全局 `limits_max_concurrent`（默认 2），`TryAcquire` 非阻塞获取；失败记 `semaphore_full`，丢弃本轮，不排队。获取成功 `defer Release`。
- **epoch 机制**：生成协程捕获发起时的 `epoch`。`/persona off`、`/persona switch`、`/persona reset` 会递增 `epoch`；回调或生成协程发现 `st.epoch != captured` 即丢弃结果，reason `stale`。这保证「刚被关掉或刚换人格的会话，旧在途结果不落地」。
- **发送**：按会话类型分派，**不用** `TargetFromEvent`：
  - 群聊：`message.Group(segs...)` + `bot.Target{Platform, BotID, ChannelID, Kind: bot.MessageGroup}`（不填 `UserID`）；
  - 私聊：`message.Private(segs...)` + `bot.Target{Platform, BotID, UserID: 对端用户 ID, Kind: bot.MessagePrivate}`（不填 `ChannelID`）。
  最后调用 `p.api.Send(p.ctx, target, msg)`。需要 `send_message` 权限（已在 `Metadata` 声明）。
- **At 段仅群聊**：`reply_mention_sender=true` 且本轮触发是寻址消息时，在文本段前插入 `message.At(发送者 ID)`；私聊不插入（@ 自己无意义）。
- **成功后**：回写历史（`Self=true` 的 `Turn`）、把 `MessageID` 记入 ID 环、更新 `lastReplyAt`、`replyTimes` 追加、`replies++`。
- **失败**：只记 `Warn`（核心自带重试），**不重排、不重发**，reason `send_error`。

## 7.6 决策伪代码

```text
onGroupMessage(ev):                              # persona:group（WithKind=MessageGroup）
  if ev.Sender == nil:                            return log(decision="skip", reason="no_sender")
  if ev.Message == nil || ev.Message.Kind != group: return log(decision="skip", reason="not_group")
  return handleChat(ev)

onPrivateMessage(ev):                            # persona:private（WithKind=MessagePrivate）
  if ev.Sender == nil:                            return log(decision="skip", reason="no_sender")
  if ev.Message == nil || ev.Message.Kind != private: return log(decision="skip", reason="not_private")
  return handleChat(ev)

handleChat(ev):
  # 入站：本函数必须在毫秒级返回，禁止任何网络调用或阻塞等待
  key, _, _, channelID = channelKey(ev)           # 群聊 platform:botID:channelID；私聊 platform:botID:user:<senderID>
  if ev.Command != nil && ev.Command.Name == "persona":
                                                  return log(decision="skip", reason="command")   # 无条件，且不进历史
  if ev.Message.Kind == private:
    if !policyAllowsPrivate(ev.Sender.ID):         return log(decision="skip", reason="not_allowed")  # 早于 stateFor
  else:
    if !policyAllowsGroup(channelID):              return log(decision="skip", reason="not_allowed")  # 早于 stateFor
  text, parts = renderMessage(ev.Message)         # 扁平文本 + 分段结构（图片段在原位置）
  st   = stateFor(ev)                             # 首次出现时异步触发 Storage 懒加载
  if ev.Command == nil:                           st.appendHistory(userTurn(ev, text, parts))    # 命令一律不进历史

  if !st.deferOrLoaded(ev, text):                 # 懒加载未完成：暂存本条（有界单槽），
                                                  return log(decision="skip", reason="loading")   # 加载完成后由 finishLoad 补判
  return decide(st, ev, text)                     # 以下为 decide()：已加载后的完整判定链

decide(st, ev, text):
  if st.disabled:                                 return log(decision="skip", reason="channel_off")
  if ev.Sender.IsBot && ignore_bots:               return log(decision="skip", reason="bot_sender")
  if ev.Command != nil && !respond_to_commands:    return log(decision="skip", reason="command")
  if text == "":                                   return log(decision="skip", reason="empty_text")
  if runeCount(text) < trigger_min_chars:          return log(decision="skip", reason="too_short")

  addressed = ev.Message.Kind == private ||
              hasMention(ev, self_ids) || isReplyToSelf(ev) || hasKeyword(text, trigger_keywords)
  now = p.now()
  if addressed:
    if now - st.lastReplyAt < mention_min_interval:  return log(decision="skip", reason="cooldown")
    if p.randFloat() >= mention_reply_probability:   return log(decision="skip", reason="probability")
    return schedule(st, "addressed")
  if !random_enabled:                                      return log(decision="skip", reason="not_addressed")
  if st.inflight || st.awaiting:                           return log(decision="skip", reason="inflight")
  if inQuietHours(now, random_quiet_hours, random_timezone): return log(decision="skip", reason="quiet_hours")
  if now - st.lastReplyAt < random_cooldown:               return log(decision="skip", reason="cooldown")
  if st.repliesInWindow(now, 1h) >= random_max_per_hour:   return log(decision="skip", reason="hour_quota")
  if st.distinctHumans(now, random_activity_window) < random_min_participants:
                                                          return log(decision="skip", reason="min_participants")
  if p.randFloat() >= random_probability:                  return log(decision="skip", reason="probability")
  return schedule(st, "random")

schedule(st, kind):
  # 布防批处理窗口：窗口内到达的新消息并入同一轮生成
  st.mu.Lock(); defer st.mu.Unlock()
  now = p.now()
  st.awaiting = true
  st.pendingKind = kind
  st.lastAddressed = (kind == "addressed")
  st.lastSenderID = currentSenderID
  if st.firstAt.IsZero(): st.firstAt = now
  st.lastMsgAt = now
  delay = min(batch_window, st.firstAt + batch_max_window - now)
  if delay < 0: delay = 0
  if st.timer != nil: st.timer.Stop()
  epoch = st.epoch
  st.timer = time.AfterFunc(delay, func(){ p.onBatch(st, epoch) })
  return log(decision="reply", kind=kind)

onBatch(st, epoch):
  st.mu.Lock()
  if st.epoch != epoch: { st.mu.Unlock(); return }          # epoch 已变，静默丢弃
  if !st.awaiting:      { st.mu.Unlock(); return }          # 已被本轮处理
  now = p.now()
  if now - st.lastMsgAt < batch_window && now - st.firstAt < batch_max_window:
    delay = min(batch_window - (now - st.lastMsgAt), st.firstAt + batch_max_window - now)
    st.timer = time.AfterFunc(delay, func(){ p.onBatch(st, epoch) })
    st.mu.Unlock(); return
  st.awaiting = false
  st.firstAt  = zeroTime
  if st.inflight { st.mu.Unlock(); return }                 # 已有在途生成
  st.inflight = true
  history = st.snapshotHistory(context_max_messages)
  st.mu.Unlock()
  p.wg.Add(1)
  go p.generate(st, epoch, history)

generate(st, epoch, history):
  defer p.wg.Done()
  defer func(){ st.mu.Lock(); st.inflight = false; st.mu.Unlock() }()

  if !p.sem.TryAcquire():                       return log(decision="skip", reason="semaphore_full")
  defer p.sem.Release()

  st.mu.Lock()
  if st.epoch != epoch: { st.mu.Unlock(); return log(decision="skip", reason="stale") }
  personaName = resolvePersona(st)              # 覆盖 > bindings > default_persona
  st.mu.Unlock()

  trimmed = p.trimHistory(history)                  # 与文本裁剪同一结果，图片槽位与之一一对应
  slots   = p.fetchVisionSlots(p.ctx, p.selectVisionSlots(trimmed))  # 关闭视觉或无图时为 nil；失败槽位留空
  req = completionRequest{
    System:      renderSystemPrompt(personaName, st, history),
    User:        renderUserContent(st.kind, trimmed, slots),  # 首块头 + 每条历史自己的块，图片在原位置
    Temperature: personaTemperature(personaName),   # 默认 llm_temperature
    MaxTokens:   personaMaxTokens(personaName),     # 默认 llm_max_tokens
  }
  content, err = p.completer.Complete(p.ctx, req)   # 内部：attempt 派生自 p.ctx、独立超时、429/5xx 退避重试
  if err != nil:            return log(decision="skip", reason="llm_error", err=err)
  if p.ctx.Err() != nil:    return

  # 回复清洗管线（8.7）
  reply = trimSpace(content)
  if reply == "":                                     return log(decision="skip", reason="empty_reply")
  if contains(reply, effectiveSkipToken(personaName)): return log(decision="skip", reason="skipped_by_llm")
  reply = stripWrappingQuotes(reply)                  # " “ ” 「 」 『 』 ' 成对剥一层
  reply = collapseWhitespace(reply)                   # 换行 → 空格，连续空格 → 一个
  reply = truncateRunes(reply, reply_max_chars)       # 硬截断，不加省略号
  if reply == "":                                     return log(decision="skip", reason="empty_reply")
  if reply_dedupe && isDuplicateOfLastOwn(st, reply, 3):
                                                      return log(decision="skip", reason="duplicate_reply")

  st.mu.Lock()
  if st.epoch != epoch: { st.mu.Unlock(); return log(decision="skip", reason="stale") }
  segs = [message.Text(reply)]
  if reply_mention_sender && st.lastAddressed && st.kind == group:
      segs = prepend(message.At(st.lastSenderID), segs)   # At 段仅群聊
  if st.kind == private:
    msg    = message.Private(segs...)
    target = bot.Target{Platform: st.platform, BotID: st.botID, UserID: st.peerUserID, Kind: bot.MessagePrivate}
  else:
    msg    = message.Group(segs...)
    target = bot.Target{Platform: st.platform, BotID: st.botID, ChannelID: st.channelID, Kind: bot.MessageGroup}
  res, err = p.api.Send(p.ctx, target, msg)           # 需要 send_message 权限
  if err != nil:
    st.mu.Unlock()
    return log(decision="skip", reason="send_error", err=err)
  st.lastReplyAt = p.now()
  st.replyTimes = append(st.replyTimes, st.lastReplyAt)
  st.appendHistory(Turn{At: st.lastReplyAt, Name: personaDisplayName(personaName), Text: reply, Self: true})
  p.rememberReplyID(res.MessageID)                    # 入方向 128 条 ID 环
  st.replies++
  st.mu.Unlock()
  return log(decision="reply", kind=st.pendingKind)
```

`log(...)` 的输出契约：`decision=reply` 行含 `kind=addressed|random`；`decision=skip` 行必须含 `reason="<token>"`，`channel=<key>`、`sender=<id>` 为固定字段；`debug_prompts=true` 时由 LLM 客户端以 `persona llm 请求` 输出请求体、以 `persona llm 响应` 输出响应体（`body` 各截断 2048 字符，见 [`llm.md`](llm.md) §9.5）。

### 状态机

```text
               判定通过
   idle ──────────────────▶ awaiting ──(窗口结束)──▶ inflight ──(完成/失败)──▶ idle
     ▲                        │                          │
     │                        │ /persona off / reset:    │ epoch 变化: 丢弃结果
     └────────────────────────┴── disabled 或 epoch 变化 ─┘   （reason=stale）

   disabled: 会话被 /persona off 关闭，所有入站直接 channel_off；/persona on 恢复。
   epoch 变化: 定时器回调丢弃（epoch != captured），生成协程丢弃结果（stale）。
```

## 7.7 决策日志与 reason 词表

始终以 `Debug` 级别输出每条群消息与私聊消息的决策行：

```text
decision=reply|skip reason=<token> channel=<key> sender=<id>
```

**reason token 全集固定为 24 个**（实现、日志与 [`testing.md`](testing.md) §11.2 必须一致）：

| token | 语义 |
| --- | --- |
| `not_group` | 非群聊消息（群 Handler 护栏） |
| `not_private` | 非私聊消息（私聊 Handler 护栏） |
| `no_sender` | 无发送者（护栏） |
| `not_allowed` | 被名单策略拒绝（`off` 或白/黑名单未放行，见 §7.8） |
| `bot_sender` | 机器人发送者且 `ignore_bots=true` |
| `command` | 命令消息（`/persona` 无条件丢弃；其它命令视 `respond_to_commands`） |
| `too_short` | 文本短于 `trigger_min_chars` |
| `empty_text` | 文本为空 |
| `channel_off` | 会话被 `/persona off` 关闭 |
| `loading` | Storage 懒加载未完成（本条已暂存 `pending` 单槽，加载完成后补判） |
| `not_addressed` | 非寻址且 `random_enabled=false` |
| `probability` | 概率判定未通过（寻址或随机） |
| `min_participants` | 活跃窗口内不同真人不足 |
| `cooldown` | 距上次回复不足最小间隔 |
| `hour_quota` | 近 1 小时回复数达上限 |
| `quiet_hours` | 处于静默时段 |
| `inflight` | 已有在途生成或已布防 |
| `semaphore_full` | 全局并发信号量已满 |
| `skipped_by_llm` | LLM 返回了 skip token |
| `empty_reply` | 回复为空（清洗前或清洗后） |
| `duplicate_reply` | 与最近 3 条自己的发言重复 |
| `llm_error` | LLM 调用失败 |
| `send_error` | 发送失败 |
| `stale` | `epoch` 变化，结果被丢弃 |

`debug_prompts=true` 时额外以 Debug 输出 LLM 请求体与响应体（见 [`llm.md`](llm.md) §9.5）。

## 7.8 会话名单策略

插件级（非每会话）的放行策略：群聊与私聊**各一套**「模式 + 单列表」，在 §7.1 第 4 步判定，先于会话状态建立。

**匹配字段**

| 作用域 | 匹配对象 | 说明 |
| --- | --- | --- |
| `group` | `ev.Channel.ID` | 群聊按频道 ID 放行/拒绝，与 `group_list` 比较 |
| `private` | `ev.Sender.ID` | 私聊按发送者 ID 放行/拒绝，与 `private_list` 比较 |

**四值模式语义**

| 模式 | 语义 |
| --- | --- |
| `off` | 该类会话全部不参与（一刀切拒绝，名单被忽略） |
| `open` | 全部参与，不过滤（一刀切放行，名单被忽略） |
| `whitelist` | 仅名单内的 ID 参与 |
| `blacklist` | 名单**外**的 ID 参与 |

**空名单语义**（组合起来容易误配，逐条列清）：

- `off` → 全部拒绝；`open` → 全部放行（两者都与名单无关）。
- `whitelist` + 空名单 → **全部拒绝**（还没往名单里加人）。
- `blacklist` + 空名单 → **全部放行**（黑名单为空 = 没有要挡的人）。

**配置默认**（[`configuration.md`](configuration.md) §10.1）：`group_policy=open`、`group_list=[]`、`private_policy=off`、`private_list=[]`。即升级后群聊行为不变，私聊默认关闭；要启用私聊须显式配置或运行期切换。

**运行期切换（`/persona` 子命令，仅管理员，不做 LLM 调用、不进历史）**

| 命令 | 行为 | 输出（逐字） |
| --- | --- | --- |
| `/persona policy` | 打印两侧模式与名单条数 | `persona: group=open(0) · private=off(0)` |
| `/persona policy group <mode>` | 设置群聊模式 | `persona: group=whitelist(0)` |
| `/persona policy private <mode>` | 设置私聊模式 | `persona: private=blacklist(1)` |
| `/persona list` | 打印两侧名单 | `persona: group=[g1 g2] private=[]` |
| `/persona list group` | 打印单侧名单 | `persona: group=[g1 g2]` |
| `/persona list group add g9` | 追加 ID（已存在则幂等） | `persona: group=[g1 g2 g9]` |
| `/persona list group del g9` | 移除 ID（不存在则幂等） | `persona: group=[g1 g2]` |

- `<mode>` 必须是 `off`/`open`/`whitelist`/`blacklist` 之一，作用域必须是 `group`/`private`；参数非法或缺参回 `/persona` 用法文本（[`persona.md`](persona.md) §8.4）。
- ID 以**单空格**分隔；空名单渲染为 `[]`。
- 只有真正发生变更时才写穿透；重复设置同一模式、`add` 已存在、`del` 不存在都不产生 `Storage.Set`。

**持久化**：键 `persona:policy`，值 JSON 形状见 [`architecture.md`](architecture.md) §4.4。`Start` 阶段同步读一次（1s 超时，失败只 `Warn` 并保留配置默认值）；写穿透异步（1s 超时，失败只 `Warn`）。逐字段覆盖：模式为空或非法不覆盖，列表为 `null`/缺键不覆盖、为 `[]` 则覆盖为空名单。

**下限保护**：`allowByMode` 对未知模式字符串返回「放行」。配置层与 Storage 层都已过滤非法模式，这个分支只在坏数据绕过两层校验时兜底——宁可多说话，也不要因为一条坏数据把机器人永久静默。
