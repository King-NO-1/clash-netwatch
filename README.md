# Clash NetWatch

按当前 WiFi 自动开关 Clash Verge 的**系统代理**与**虚拟网卡（TUN）**。

- 连上 **MyPhone-5G** → 开系统代理 + 进 TUN
- 连上任何其它网络 / 断开 → 立即关系统代理 + 退 TUN

三个 Go 文件（`netwatch.go` 守护主体 + `tray.go` 托盘通知 + `autoconnect.go` 自动连热点），产物 2.6MB。
代理/TUN 判定是事件驱动（不轮询）；候选热点另有一条 15s 的只读轮询兜底。
常驻内存：开托盘 14.7MB、关托盘 9.0MB（`config.json` 里 `show_tray` 控制）。

> 文中的 `MyPhone-5G` / `Campus-WiFi` 是示例网络名。实际使用时按自己的热点改配置，源码里的内置禁止名单也要一并改掉。

## 快速开始

环境：Windows + Clash Verge Rev + Go 1.25+。零第三方依赖。

```powershell
git clone <仓库地址>
cd clash-netwatch
cp config.example.json config.json
# 编辑 config.json：target_ssid 改成你的热点名，never_proxy_ssids 填绝不能走代理的网络
go build -trimpath -ldflags "-s -w -H=windowsgui" -o netwatch.exe .
.\netwatch.exe -scan       # 只读：确认候选热点、profile 和匹配判定
.\install-task.ps1         # 注册开机自启 + 心跳保活
.\netwatch.exe -status     # 查看当前状态
```

`auto_connect` 依赖目标热点**已经手动连过一次**——Windows 的 `WlanConnect` 必须带已保存的 profile，不会凭 SSID 猜密码。

---

## 一、实现思路

```
NetworkProfile 事件（EvtSubscribe）  ┐
IP 地址表变更（NotifyAddrChange）    ├─▶ 唤醒（channel 容量 1，天然合并）─▶ 单个 worker
定时校准（60s，兜底）                ┘                                      │
15s 候选轮询（只在非目标网络时）─────┘                                      ▼
                                            WLAN API 取 SSID（~2ms，不 spawn 进程）
                                                                            │
                                              ┌─────────────────────────────┴──────────────┐
                                              ▼                                            ▼
                                    目标匹配 → 系统代理（注册表）              不匹配 → 先看候选
                                              │                                            │
                                              └─▶ TUN（内核管道 PATCH /configs）   有目标热点 → WlanConnect 切过去
                                                                                          无 → 关代理 + 关 TUN
```

### 为什么是这两类事件

| 事件源 | 覆盖场景 | 本机状态 |
|---|---|---|
| `EvtSubscribe` → `Microsoft-Windows-NetworkProfile/Operational`，EventID 10000/10001 | 网络配置文件连接/断开，SSID 变化最直接的信号 | 通道已启用，普通用户可订阅（无需管理员） |
| `NotifyAddrChange`（iphlpapi） | 接口拿到/失去地址、路由变化，也覆盖事件通道被停用的情况 | 始终可用 |
| 15s 候选轮询（仅非目标网络时） | 热点后开、目标网络重新出现在候选里（此时没有任何网络事件） | 只读 WLAN 缓存，~1ms，不发射探测 |
| 60s 定时校准 | 进程启动时正在切换、休眠错过事件、被外部改动 | 一次判定约 7ms，开销可忽略 |

两个事件源互为备份：任意一个漏了，另一个会在毫秒级补上；候选轮询负责"网络没变但可选网络变了"这一类。

### 为什么不上 netsh

`netsh wlan show interfaces` 实测 140–165ms，光取 SSID 就吃掉 1/3 的延迟预算，还要 spawn 一个进程。改用 `WlanQueryInterface`（wlanapi.dll）直接读，**1–3ms**，零进程开销。

### 执行层

- **系统代理**：直接写 `HKCU\...\Internet Settings` 的 `ProxyEnable` / `ProxyServer`，再 `InternetSetOption` 广播变更。
- **TUN**：内核（verge-mihomo）在命名管道 `\\.\pipe\verge-mihomo-production-<hash>` 上开了 HTTP 控制器，`PATCH /configs {"tun":{...}}` 热开关，不重启内核、不需要管理员。
- **verge.yaml 回写**：把 `enable_tun_mode` / `enable_system_proxy` 同步成实际状态，Verge 重启或重载内核时不会翻回去。

---

## 二、延迟实测与 500ms 预算

ON 路径（事件驱动，实测日志）：

```
状态不一致（网络事件，事件→判定 102ms）：ssid="MyPhone" 目标=ON tun=false ...
切换 ON via "网络事件" 完成：代理 43ms + TUN 923ms = 969ms（事件→完成 1078ms）
```

分阶段归因（多次实测区间）：

| 阶段 | 耗时 | 说明 |
|---|---|---|
| Windows 事件投递 → worker 拿到 | **79–179ms** | 主要是 Windows 自己的事件排队；我方唤醒 + 读 SSID + 读内核状态约 10ms |
| 系统代理启停 | **39–170ms** | 注册表写入本身 <1ms，耗时在 `InternetSetOption` 向所有进程广播 |
| TUN 关闭 | **157–205ms** | mihomo 销毁 Wintun 会话 |
| TUN 开启 | **923–1485ms** | mihomo 重建 Wintun 适配器 + 装路由，`PATCH` 响应前就等在这里 |

**结论：**

- **关闭（离开目标网络）全程 305–414ms**，满足 500ms。
- **开启时系统代理 250ms 左右生效**，满足 500ms——所以 ON 的顺序是「先代理，后 TUN」：流量 250ms 内就走代理，TUN 晚 1.2s 补上，只影响不走系统代理的程序（游戏、部分命令行工具）。
- **TUN 本身的开启压不进 500ms**：1.0–1.5s 全部消耗在 mihomo 内部（`PATCH /configs` 阻塞返回），客户端无论怎么优化都改变不了；这是 Wintun 适配器创建 + 路由表安装的固有成本。

如果必须让"进入 TUN"也在 500ms 内，只有一条路：TUN 常开，只切系统代理（此时切换 ≈ 250ms）。代价是不符合"退出 TUN 模式"的要求，所以没有默认这样做。

其它影响因素：

- **事件触发机制**：WiFi 关联/认证本身要几百毫秒到几秒，`事件→判定` 只能覆盖系统认为"已连上"之后的部分。想更早介入只能监听 802.11 关联（`WlanRegisterNotification`，仍在同一量级）。
- **并发与竞态**：所有判定串行在一个 worker 里，事件通过容量 1 的 channel 合并，不存在并发改状态；自己开关 TUN 产生的 IP 变更事件会在下一轮被 `inSync` 判掉，不会形成开关循环。
- **系统权限**：读 SSID、写 `HKCU`、访问内核管道全部用户态完成，不需要管理员；只有写 `verge.yaml` 需要该文件可写（在用户目录下，满足）。

---

## 三、判定规则

```
归一化（去空格/连字符/点、转小写）后比较：
  完全相等                                                  → 目标网络
  fuzzy_match=true 且短的一方 ≥6 字节、长度差 ≤4、且是前缀   → 目标网络
  其它                                                      → 非目标网络
```

`MyPhone-5G` 归一化成 `myphone-5g`；本机实际 SSID 是 `MyPhone`（`myphone`），靠前缀容错命中。这不是笔误兜底，而是刻意保留：手机热点改名、路由广播带 `-5G`/`Plus` 后缀都能覆盖。想要严格相等就把 `fuzzy_match` 设成 `false`。

前缀匹配要求「够像」（短的一方 ≥6 字节且长度差 ≤4），因为自动连接会**真的把 WiFi 切过去**：早先的规则只要一方是另一方前缀就算命中，一个叫「MyPhone」的邻居热点也能骗过判定。收紧后 `myphone` ↔ `myphone-5g` 仍然命中，`MyPhone` 不会。

**断开不是"未知"**：明确读不到 SSID 且网卡未连接 → 判定为"非目标网络" → 关闭。只有 wlanapi 报错才算未知，此时不动任何状态。

---

## 四、自动连接目标热点

Windows 自己的「自动连接」只在**尚未连接**时按 profile 优先级挑网络。电脑已经连在 Campus-WiFi 上时，即使手机热点就在候选里也不会跳过去——这是系统策略，不是配置问题。

NetWatch 补上这一层：**候选列表里出现目标 SSID（含模糊匹配）时，直接把 WiFi 切过去**。

### 触发时机

| 触发 | 说明 | 实测延迟 |
|---|---|---|
| 网络事件 | 连上别的网络 / 断开时立即检查候选 | 事件 → 发起 `WlanConnect` 约 0.1–1.5s |
| 15s 候选轮询 | 手机热点后开、电脑一直连着校园网时没有任何网络事件，靠这条兜底 | ≤15s |
| 托盘「立即连接目标热点」/ `-connect` | 手动触发，绕过冷却 | 立即 |

### 怎么连

1. `WlanGetAvailableNetworkList` 读 Windows 维护的候选缓存（只读，不发射探测请求，~1ms）
2. 用与代理判定完全相同的 `matches()` 找目标 SSID，挑信号最强的一条
3. `WlanConnect` + 该网络**已保存的 profile** 发起连接；调用立即返回，认证与 DHCP 由 Windows 完成
4. 连上后走原有链路：NetworkProfile 事件 → `matches=true` → 开系统代理 + TUN

连接自己 profile 列表里的网络是普通用户权限，**不需要管理员**。

### 状态归位优先于自动连接

`reconcile` 的顺序是固定的：**先按当前 SSID 把代理/TUN 归位，再尝试自动连热点**。这是 2026-11-25 事故换来的教训——之前反过来做，候选里一直有MyPhone时每轮都「发起连接→return」，代理挂在 Campus-WiFi 上 85 秒没关。现在只要当前网络不是目标，代理与 TUN 立刻关（实测事件→完成 0.5 秒内），连接热点的动作排在后面。

代价是从校园网切回热点时会先关再开，中间有几秒空窗。**这是对的**：一号需求是"非目标立即关"，不能为了不断流而违背它。

### 防抖与退避

`WlanConnect` 是异步的——返回 0 只代表"请求发出去了"，不代表连上了。不能拿返回值当成功。

| 情况 | 行为 |
|---|---|
| 发起连接后 12 秒内 | 验证窗口：不重发。窗口到期仍未连上目标 → 算一次失败 |
| 连上目标后观察 3 秒 | 稳定才清失败退避，防关联中的暂态 SSID 把冷却刷掉 |
| 连接失败 / 超时 | 60s → 120s → 240s … 最长 10 分钟退避，日志记 WARN |
| 没看到候选 | 最多每 20 秒踢一次 `WlanScan` 刷新列表，不做持续射频扫描 |
| 托盘「暂停自动切换」 | 连热点一并暂停（暂停 = 自动行为全部交给用户） |

### 前提：目标网络必须手动连过一次

`WlanConnect` 必须带已保存的 profile，Windows 不会凭 SSID 猜密码。候选里看见目标但没有 profile 时，日志与 `-scan` 会明确提示「需手动连一次」；连过一次生成 profile 后就能一直自动切。

### 实测

```
11:44:38 手动连到 Campus-WiFi
11:44:41 当前网络 Campus-WiFi（代理/TUN 已关）
11:44:42.953 检测到目标热点，自动连接（网络事件）：MyPhone（信号 91%）
11:44:44 当前网络 MyPhone
```

收紧过滤后（去掉候选列表里过期的"已连接"标志、允许在关联/认证中直接抢回）：

```
11:48:58 手动连到 Campus-WiFi
11:48:59.308 检测到目标热点，自动连接（网络事件）：MyPhone（信号 96%）
11:49:02 当前网络 MyPhone，TUN 全程 true
```

---

## 五、稳定性设计

| 关注点 | 做法 |
|---|---|
| 单实例 | `CreateMutexW("Local\ClashNetWatch")`；拿不到互斥体时**拒绝启动**（不冒双实例互杀的风险） |
| 常驻保活 | 四个触发点：登录后 15s、网络「已连接」事件、睡眠唤醒、**每分钟心跳**；实测强杀后 56 秒被心跳拉回 |
| 重复事件去重 | 事件合并 + `inSync()` 先比对再动作；一致时零写入、零日志 |
| 事件风暴 | channel 容量 1 + 进入时 drain，一次网络切换只触发一轮判定 |
| 自己触发自己 | 开关 TUN 会产生 IP 变更事件，下一轮因状态已一致被丢弃，不会自激 |
| 换网抖动 | 只有"断开且 SSID 为空"时等 250ms 复读一次，避免换网中途先关后开 |
| 自动连接防抖 | 状态归位优先；发起后 12s 验证窗口（不当成已连上）；稳定连上 3s 才清退避；失败 60s→10min 指数退避；暂停期间不连 |
| TUN 瞬时失败 | 写操作超时放宽到 9s（`PATCH tun.enable` 重建 Wintun 实测 3.4–6.4s，3s 会误判失败），失败后重试并回读 `enable` 确认，避免推迟到下一轮 60s 校准 |
| 部分失败回滚 | 代理开了但 TUN 失败 → 回滚代理并**检查回滚结果**，不留半开状态 |
| 内核未起 | 只关不开；目标网络下若内核中途消失（Verge 换代），顺手清掉注册表里残留的系统代理，让流量直连而不是灌进死端口；内核恢复后下一轮校准自动把代理+TUN 一起开回来。这次清理不弹通知（后台纠错），且因提前返回不改动通知基准，不会误报"已开启" |
| 外部改动 | 60s 校准同时充当自愈，被 Verge 或用户改掉会拉回 |
| 事件源失效 | 两类事件互为备份；`NotifyAddrChange` 报错时记 ERROR 并 5 秒后重试，不会静默失效 |
| 资源清理 | WLAN/管道/注册表句柄均在返回前关闭；管道读超时 3s、写超时 9s（超时关句柄打断），响应体上限 8MB |
| 长期运行 | 日志 512KB 轮转（留一份 `.1`）；每轮只创建有界短生命周期 goroutine |

### 永不代理硬约束（Campus-WiFi）

**只要当前连着 Campus-WiFi，系统代理和 TUN 必须关闭——没有任何开关能绕过。** 这是比"目标网络才开"更高优先级的绝对规则，用三道防线保证：

| 防线 | 机制 |
|---|---|
| 判定层 | `effectiveWant = matches(ssid) && !isNeverProxy(ssid)`。即使把 `target_ssid` 改成 Campus-WiFi、或 `fuzzy_match` 误判命中，只要命中禁止名单就一律判 OFF。所有决策点（reconcile / -status / -refresh）统一走这个入口 |
| 暂停层 | 暂停只暂停"开代理/开 TUN/自动连热点"，**绝不暂停"关"**。暂停期间切到 Campus-WiFi，代理照样立刻断开（旧代码在这里直接 return，会让代理一直挂着） |
| 看门狗层 | 独立 goroutine 每 2 秒巡检：禁止网络下只要 `ProxyEnable=1` 就强制写 0 并回写 verge.yaml。不受暂停、自动连接、内核可达性影响——内核挂掉时它也只写注册表，照样能清掉代理 |

禁止名单 = 硬编码 `Campus-WiFi`（含 `Campus-WiFi 5G` 等前缀变体）+ 配置 `never_proxy_ssids` 追加项。配置只能加，删不掉内置项。

实测（2026-11-25）：
- `target_ssid` 改成 Campus-WiFi（fuzzy 命中）→ 判定仍 OFF、代理/TUN 全关、一致 true
- Campus-WiFi 下手动把 `ProxyEnable` 写回 1 → 看门狗 2 秒内强制关（日志 `看门狗：... 强制关闭`）
- 暂停 30 分钟期间写回代理 → 看门狗依然强制关，暂停挡不住

---

## 六、配置

`config.json`（与 exe 同目录，改完重启进程生效）：

| 字段 | 默认 | 说明 |
|---|---|---|
| `target_ssid` | `"MyPhone-5G"` | 唯一目标网络 |
| `fuzzy_match` | `true` | 前缀容错；`false` 为严格相等 |
| `auto_connect` | `true` | 候选里出现目标热点时自动切过去 |
| `auto_connect_seconds` | `15` | 候选轮询间隔（最小 5） |
| `auto_connect_min_signal` | `0` | 只连信号 ≥ 该值的候选（0-100，0 不限） |
| `proxy_port` | `0` | 0 = 读 verge.yaml 的 `verge_mixed_port` |
| `sync_verge_config` | `true` | 回写 verge.yaml |
| `reconcile_seconds` | `60` | 定时校准周期（最小 10） |
| `never_proxy_ssids` | `["Campus-WiFi"]` | 永不代理的网络（追加项；内置 Campus-WiFi 删不掉） |
| `log_max_bytes` | `524288` | 日志轮转阈值 |

---

## 七、使用与验证

```powershell
.\netwatch.exe -status     # 当前 SSID、目标、内核、TUN/代理实际状态、是否一致、自动连热点、守护进程在不在
.\netwatch.exe -scan       # 只读：列出 WiFi 候选、profile、此刻自动连接会怎么做（不改任何状态）
.\netwatch.exe -connect    # 立即尝试连接目标热点（等价于托盘菜单「立即连接目标热点」）
.\netwatch.exe -once       # 立即校准一次（要求常驻实例没在跑，否则拒绝执行）
.\netwatch.exe -refresh    # 刷新 Clash Verge 界面状态（重启 GUI 让它重新读盘）
.\netwatch.exe -refresh -soft   # 只同步配置与内核状态，不重启 GUI
.\install-task.ps1         # 注册/更新计划任务（登录 / 网络连接 / 唤醒 / 每分钟心跳）
```

`-connect` 的退出码：已在目标网络、已发起连接 → `0`；读不到候选 / 缺 profile / `WlanConnect` 报错 → `1`，方便脚本判断。

### 怎么确认它真的生效了

Verge 的界面不会因为我们改状态而刷新，所以本进程自己给出反馈，三条路：

**1. 右下角通知（最直接）**

只在**网络或运行态相对上次通知真的变了**时弹一条 Windows 通知：

```
NetWatch
已关闭代理与 TUN · 未连接网络 · 152ms
```

什么情况弹、什么情况不弹（这是核心规则，已通过全流程测试验证）：

| 场景 | 弹通知？ | 原因 |
|---|---|---|
| 断开 WiFi | 弹「已关闭」 | 网络变了 |
| 连回 WiFi | 弹「已开启」 | 网络变了 |
| 换到别的网络 | 弹「已关闭」 | 目标变了 |
| 外部关 TUN，守护进程拉回 | **不弹** | 网络和目标都没变，是后台纠错 |
| 定时校准发现状态漂了，纠正 | **不弹** | 同上 |
| 启动时状态已在目标值 | **不弹** | 无变化 |
| 启动时状态不对（config 改过） | 弹 | 运行态和基准不同 |
| 两次切换间隔 < 3 秒 | 只弹第一次 | 限流防刷屏 |
| 左键点击托盘图标 | 弹当前状态 | 1.5 秒内只响应一次 |

通知文本在 `apply` 成功完成后才生成，所以**通知内容与真实状态一致**——说"已开启"就是真的开了，不会虚报。

没看到通知多半是被系统通知设置/专注助手拦了——`netwatch.log` 里会留一行 `已弹出通知：...`，能区分"没发"和"没显示"。


**2. 托盘图标（常驻）**

任务栏通知区有一个 NetWatch 图标（通用应用图标）：

- 鼠标悬停 → `NetWatch · MyPhone · 代理开｜10:56:12 切换开`
- 左键点击 → 弹出当前状态
- 右键 → **立即连接目标热点** / 暂停自动切换 30 分钟 / 刷新 Clash Verge 界面 / 打开日志 / 退出

想手动接管（比如临时在校园网也开着代理）就用"暂停"，暂停期间只更新图标提示，不动任何状态；到期自动恢复，暂停状态落盘，进程被心跳拉起也不会丢。`-status` 会显示剩余时间。

**3. `-status`（随时对账）**

```powershell
.\netwatch.exe -status
```

```
当前网络 : "MyPhone"
目标     : ON（"MyPhone-5G"）
TUN      : true
系统代理 : true server=127.0.0.1:7897
是否一致 : true
守护进程 : 运行中
```

这四项是直接读内核 API 和注册表来的，跟任何界面无关，是最权威的状态。

**构建**（改代码后）：

```powershell
go build -trimpath -ldflags "-s -w -H=windowsgui" -o netwatch.exe .
```

注意：程序正在运行时直接构建会生成 `netwatch.exe~` 而替换失败，先停任务再编译：
`Stop-ScheduledTask ClashNetWatch; Get-Process netwatch | Stop-Process -Force`

**验证生效**：

1. `-status` 看 `是否一致 : true`，且 TUN / 系统代理均为 `true`。
2. 手动在 Verge 里关掉 TUN，几秒内 `netwatch.log` 出现 `状态不一致（网络事件，事件→判定 XXms）` 与 `切换 ON ...`。
3. 手机关热点：几秒内出现 `切换 OFF`，`TUN=false`、`代理=false`，网络恢复直连。重新开热点：反向恢复。
4. 不想动手机就临时把 `target_ssid` 改成不存在的名字跑 `-once`（会关），再改回来跑一次（会开）。
5. 保活：`Stop-Process -Name netwatch -Force` 杀掉进程，等 1 分钟，`-status` 的「守护进程」应为「运行中」（实测 56 秒恢复）。
6. 自动连热点：`-scan` 确认候选里有目标且带 profile；在 WiFi 列表手动连到 Campus-WiFi，1–3 秒内应自动切回MyPhone，日志出现「检测到目标热点，自动连接」，期间代理与 TUN 不断。

---

## 八、刷新 Verge 界面状态

### 为什么需要

Verge 的两个开关显示的是**它自己进程内存里的配置**（启动时从 `verge.yaml` 读一次，之后只跟着用户点击更新）。NetWatch 是通过内核 API 改 TUN、通过注册表改系统代理，Verge 感知不到，界面就会停在旧值。Tauri 没有对外开放"重载配置"的接口（端口 53876 不是 CDP，HTTP 无响应），唯一能让界面重新初始化的办法是重启 GUI 进程。

### 会重新加载什么

重启进程等于把下面这些全部重新初始化：

| 层 | 内容 |
|---|---|
| 应用配置 | `verge.yaml`：`enable_tun_mode`、`enable_system_proxy`、`verge_mixed_port`、主题、语言等 |
| 运行时内核配置 | 由 `verge.yaml` + 当前 profile 重新生成 `config.yaml`，并据此启动或接管内核 |
| 内核真实状态 | `/configs` 的 `tun.enable`、混合端口；`/version` 版本号 |
| 订阅与 profile | `profiles.yaml`、当前选中的订阅、providers |
| 系统代理 | 注册表 `ProxyEnable`/`ProxyServer` 的实际值 → 「系统代理」开关的显示 |
| 托盘菜单 | TUN / 系统代理 / 规则模式的勾选状态 |
| 界面组件 | 设置页两个开关、首页运行时卡片、代理组与延迟 |

### 触发方式

- 手动：`netwatch.exe -refresh`（默认硬刷新，重启 GUI）
- 只同步不重启：`netwatch.exe -refresh -soft`
- 被动提示：`-status` 在 `是否一致 : 否` 时会追加一行提示这条命令
- **不做自动触发**：重启会让内核重新接管（实测有时沿用、有时换新内核），有 2~8 秒代理中断，不能挂在网络切换路径上

### 加载状态提示

Verge 没有可注入的进度通道，所以进度只在自己这一侧显示，分三段：

```
[1/3] 配置已同步：verge.yaml tun=true 代理=true（守护进程在跑，状态由它维护）
[2/3] 已终止旧进程 pid=32348（119ms）
[3/3] 已重启：pid=32348 → 17340，启动日志 239ms，内核 v1.19.29（内核沿用；总 347ms）
```

判定"启动完成"用的两个信号：**Verge 写出新的启动日志**（`logs/*.log` 的 mtime 越过基线）+ **内核可达**。不能用"进程存在"当判据——内核挂在 `clash-verge-service` 下，GUI 被杀时它还活着，会立刻造成"已就绪"的假象。

注意：`enable_silent_start: true` 时重启后**只在托盘出现**，不弹窗。要看界面得点一下托盘图标。

### 异常处理

| 情况 | 行为 | 退出码 |
|---|---|---|
| Verge 没在运行 | 跳过重启，提示"界面不存在，无需刷新" | 0 |
| 找到进程但读不到 exe 路径 | 提示手动重启 | 1 |
| `TerminateProcess` 失败（权限等） | 提示先手动退出 Verge 再重试 | 1 |
| `CreateProcess` 失败 | 提示手动打开 Verge | 1 |
| 20 秒内未确认启动 | 打印 日志/内核 两个标记的真实状态，让用户自己确认 | 1 |
| 刷新后状态仍不一致 | 守护进程没在跑时自动校正一次；仍不一致则报错并写日志 | 1 |
| 守护进程正在跑 | 不抢着 apply，只同步文件 + 重启 GUI（避免双写） | — |

### 完成后的反馈

输出刷新前后的对照 + 耗时 + 一致性，并写一行到 `netwatch.log`：

```
刷新前   : 内核 tun=true，注册表 代理=true；verge.yaml tun=true 代理=true
[3/3] 已重启：pid=32348 → 17340，启动日志 239ms，内核 v1.19.29（内核沿用；总 347ms）
刷新后   : 内核 tun=true，注册表 代理=true；verge.yaml tun=true 代理=true
结果     : 一致（总耗时 361ms）
```

---

## 九、已知限制

- 只认 WiFi。USB 网络共享 / 有线连接读不到 SSID，会被判为"非目标网络"并关闭代理。
- **自动连接要求目标网络有已保存 profile**：Windows 不允许凭 SSID 猜密码，从没连过的热点必须手动连一次。
- 目标热点只是广播存在但连不上（改了密码 / 走远了）时，每次验证窗口到期后才按 60s→10min 退避重试，不会连发。
- Windows 的候选缓存有延迟：热点刚开最多要等一次扫描（≤20s）；连过一次、缓存热的时候是 1–3s。
- 切回目标热点时代理/TUN 会先关再开（状态归位优先），中间有 1–4 秒空窗；这是"非目标立即关"的必然代价。
- `-status` 里的「冷却中 / 最近一次判定」这类瞬时信息只有在该进程自己做过判定时才有内容（`-status` 是独立进程），要看实时判定请用 `-scan`。
- `config.json` 只在进程启动时读一次，改完要重启守护进程（托盘菜单没有重载入口）。
- 内核管道是 Verge 内部实现，Verge 大版本换了传输方式就要跟着改（管道名按前缀 `verge-mihomo` 动态发现，secret 从 `config.yaml` 读）。
- 网络在 1 秒内反复抖动时，事件合并会牺牲一点实时性换取不抖动。
- `legacy/` 是上一版 Python 实现（轮询式 3s 周期），仅保留供回滚参考，不参与运行。
