# Gwifi 双出口下载原型

面向山东科技大学 Gwifi 网页认证、Windows 有线 + Wi-Fi 场景的 Go 命令行程序。
将同一文件的 HTTP Range 分片分配给不同出口下载。**多个账号不会自行变成多个出口**：两个账号必须分别通过两个能独立认证的校园网连接出网。

## 连接示例

```text
Windows 以太网 → EW1200G PRO → 校园有线网 → 账号 A
Windows WLAN   → 学校 Gwifi 无线网络       → 账号 B
                         ↓
                两路下载分片，合成同一文件
```

WLAN 必须直连学校的无线网络。连接 EW1200G PRO 发出的 Wi-Fi 会继续共用它的校园出口。Go 命令行程序不改变路由器、无线连接、系统默认路由或代理设置；可选的 Mihomo 集成会修改其分流配置，详见下文。

锐捷原厂资料说明 EW1200G PRO 为单 WAN 接入。这个原型将现有路由器保留为有线出口，第二出口由电脑的无线网卡提供。它不是 PPPoE 多拨程序，也不加速任意软件的一条普通 TCP 连接。

## 使用

源码仓库不包含 `bin\gwifi.exe`。首次使用请在 Windows 上安装 [Go 1.26 或更高版本](https://go.dev/doc/install)，将 Go 的 `bin` 目录加入 PATH，重新打开 PowerShell，在项目目录构建：

```powershell
go version
.\scripts\build.ps1
```

构建脚本会先运行测试和静态检查，再生成 `bin\gwifi.exe`。已有构建好的可执行文件时，运行它不需要安装 Go。

先手动把电脑 Wi-Fi 连接到学校 Gwifi，保留当前网线，再创建本机配置：

```powershell
if (!(Test-Path .\config.local.json)) { Copy-Item .\config.example.json .\config.local.json }
.\bin\gwifi.exe interfaces
.\bin\gwifi.exe check
```

仅首次复制配置，已有 `config.local.json` 时直接编辑，保留原配置。`interface` 必须与 `interfaces` 输出的名称一致。`source_ip` 留空时选该网卡唯一可用 IPv4；多地址网卡需要明确指定。配置不含账号密码。

每个出口独立维护 HTTP 会话。示例有线入口不携带固定 `wlanuserip`，程序每次从页面重新获取终端参数。无线 `portal_url` 留空时，通过 Microsoft HTTP 连通性检测页的重定向发现认证页；已能联网则报告无需重复登录。如果没有 HTTP 重定向或仅有 JavaScript 跳转，需要将**该出口当前真实的认证页面地址**填入对应 `portal_url`。不要把有线网卡的终端 IP/登录链接复制给无线网卡。

本机交互登录：

```powershell
.\bin\gwifi.exe login -exit wifi
# 只有有线出口也需要登录时，再执行：
.\bin\gwifi.exe login -exit wired
.\bin\gwifi.exe check
```

密码不回显，也不写入配置或日志；仅在明确执行 `login` 时提交。登录接口返回成功后仍应运行 `check` 并测试下载。原型只适配已经读取的 Gwifi `loginForm` 协议；不同入口若使用另一版登录页，需要单独适配。

换账号前，可以退出指定出口当前使用的校园网会话：

```powershell
# 只退出 Wi-Fi，然后手动输入另一个账号登录
.\bin\gwifi.exe logout -exit wifi
.\bin\gwifi.exe login -exit wifi
.\bin\gwifi.exe check

# 如果需要退出有线，则单独执行这一条
.\bin\gwifi.exe logout -exit wired
```

`logout` 必须用 `-exit` 明确指定一个出口，不接受双出口列表或下载参数。它通过该出口获取当前在线会话，发送一次退出请求，不需要输入账号密码。有线接在路由器后面时，退出的是路由器 WAN 对应的校园网会话，共用它的其他设备也会失去该路认证。

程序只在退出接口报告成功、且公网连通性检测被重定向到真实 Gwifi 登录表单时，才报告已退出并需要重新认证；接口接受但状态未确认时会明确提示。网络错误或登录表单本身不能证明退出成功。若当前已经被要求重新认证，则直接报告无需退出，不重复发请求。退出过程不会自动重试、自动重新登录或确认换绑，也不切换 Mihomo 模式。

退出功能访问 `portal_url` 的同源首页；配置留空时使用 `http://100.100.100.2/`。本校首页会跳转到 `/gportal/web/index`，已在线时显示“下线”页面，其中 `formData` 表单提供当前会话的隐藏字段 `si`。程序按该页面的 `offline()` 脚本，仅将 `si` 作为普通表单提交到 `/gportal/Web/logoutAction`，会话值不显示或保存。缺失、空值或重复的 `si` 会在提交前报错。

`/gportal/web/login` 在已联网时也可能显示登录表单，其中的 `sign`、`iv` 等登录字段无法代替退出需要的 `si`；旧版因此会收到 `session id is empty`。当前版本已改为读取真实下线表单。开发验证包括模拟接口测试和实际门户的只读会话获取，不主动退出当前校园网会话。

若认证接口返回 **124（设备换绑确认）**，并提供有效的同源确认链接，程序会显示服务器的剩余次数提示，并询问：

```text
确认换绑会消耗剩余换绑次数。
是否确认换绑至当前设备？[y/N]:
```

输入 `y` 或 `Y` 后，程序通过当前出口发送一次换绑确认；输入 `n`、`N`、直接回车、输入结束或 Ctrl+C 都会取消，取消不会被报告为登录成功。其他输入会重新询问。确认请求失败时不会自动重试，请先运行 `check` 核实；确认接口报告成功后，程序会检查当前出口是否联网。若尚未联网，按提示运行 `check` / `login`，不会自动再次提交密码。程序不会自动执行其他“踢下设备”、改密码或额外确认操作。

此门户实测会拒绝 Go 默认 User-Agent；认证及入口发现请求使用 Windows 桌面浏览器 User-Agent，与当前电脑的终端类别一致。

使用可直接下载、支持 Range 的 HTTPS 文件链接：

```powershell
.\bin\gwifi.exe fetch -exits wired,wifi -workers 8 -url 'https://mirrors.aliyun.com/golang/go1.27.2.windows-amd64.zip' -out '.\go-dual-8.zip'
```

上面是已验证支持 Range 的实际下载地址，文件约 75.3 MiB；可以直接复制运行。输出文件或对应 `.part` 已存在时请换一个输出名。默认每出口最多 2 个并发连接、每片最多 4 MiB，可用 `-workers 1..32`、`-chunk-mib 1..64` 调整。`-workers 8` 表示**每个出口**最多 8 个连接，双出口合计最多 16 个，并非两个出口平分 8 个。

有线与 Wi-Fi 的速度上限不同时，可分别设置连接数；未列出的出口沿用 `-workers`：

```powershell
.\bin\gwifi.exe fetch -exits wired,wifi -exit-workers 'wired=24,wifi=4' -url 'https://mirrors.aliyun.com/golang/go1.27.2.windows-amd64.zip' -out '.\go-dual-24-4.zip'
```

`-exit-workers` 只用于 `fetch`，只接受本次选中的出口，每个值为 1..32。`wired=24,wifi=4` 是 2026-10-19 在开发测试电脑和上述下载源上实测有效的配置，其他网络或服务器的最优值可能不同。程序启动时显示每出口实际采用的并发上限。终端显示总体平均速度、每出口累计下载量和最近至少 1 秒的速率；最后不足 1 秒的更新只显示累计量。下载进度达到 100% 后，只有出现“下载完成”且最终文件发布成功才代表成功。

各出口同时探测文件信息。下载时，各连接从共享队列领取分片；接近队列末尾时自动缩小尚未分配的分片，减少单个慢连接持有大块尾部数据造成的等待。已领取的分片不会抢占或重复下载。

## 验证能否叠加

选同一个足够大的、两个出口都能访问的直接下载链接，使用不同输出文件依次测试：

```powershell
.\bin\gwifi.exe fetch -exits wired -workers 8 -url 'https://mirrors.aliyun.com/golang/go1.27.2.windows-amd64.zip' -out '.\go-wired-8.zip'
.\bin\gwifi.exe fetch -exits wifi -workers 8 -url 'https://mirrors.aliyun.com/golang/go1.27.2.windows-amd64.zip' -out '.\go-wifi-8.zip'
.\bin\gwifi.exe fetch -exits wired,wifi -workers 8 -url 'https://mirrors.aliyun.com/golang/go1.27.2.windows-amd64.zip' -out '.\go-combined-8.zip'
```

两路各自有流量且合计速度稳定超过单路，才是有效证据。页面显示不同终端 IP、连接两块网卡、或登录接口成功，都不能证明两份独立带宽。以有线 100 Mbps、Wi-Fi 50 Mbps 的套餐为例，理论合计为 **150 Mbps，即 18.75 MB/s**；实际套餐上限需按自己的账号确认，文件下载速度还受协议开销、校园共享上联、无线信号及下载服务器约束。目标带宽不等于已经实测达到的速度。约 75 MiB 的文件适合快速对比，连接建立和收尾时间会影响它的全程平均速度；验证持续速度应再用更大的文件。

## 边界

- 固定 Windows `IP_UNICAST_IF` 和对应本地 IPv4，独立 DNS 查询也走选定网卡；不使用环境 HTTP/SOCKS 代理。配置中的 `dns` 留空时读取该网卡实际配置的 IPv4 DNS，也可指定该出口可达的 `DNS地址:53`。若透明代理、VPN、Mihomo TUN 或驱动过滤仍截获流量，需要结合实际网卡流量检查路径；不能只凭软件统计判断物理链路。
- Go 下载器为 IPv4 原型，不自建 TUN、SOCKS 代理、单连接隧道汇聚或后台重登录。其他软件下载可使用下方已有 Mihomo 的集成方案。
- 下载源必须对每个出口返回正确的 `206` 和 `Content-Range`，并具有共同的强 ETag，或满足保守校验条件的 Last-Modified。弱/缺失验证器、不同版本、不同最终重定向 URL、HTML 认证页会报错。部分网盘、需浏览器登录/自定义请求头的地址和每次变化的签名链接暂不适用。
- Ctrl+C 取消；错误或取消保留 `.part`。v1 **不自动续传**，再次下载需改输出名或自行处理旧 `.part`。不覆盖已存在文件。
- 完成时用硬链接原子发布，适合本机 NTFS；不支持硬链接的目标盘会保留完整 `.part` 并报告失败。暂不支持空文件。
- 认证请求沿用现有门户的 AES 封装；这不等于为原 HTTP 门户增加 TLS。

## 构建与测试

```powershell
.\scripts\build.ps1
```

脚本使用 PATH 中的全局 Go，并在开始时显示实际可执行文件路径和版本。工具目录、依赖和编译缓存沿用全局 `go env` 配置，输出 `bin\gwifi.exe`。当前依赖要求 Go 1.26 或更高版本；首次使用新的全局依赖缓存时需要联网下载。

Go SDK、`GOPATH` 和编译缓存可使用全局默认值或自己配置的目录，无需安装项目内工具链，也无需手动设置 `GOROOT`。更改 PATH 后需重启终端及编辑器，再检查：

```powershell
where.exe go
go version
go env GOROOT GOPATH GOBIN GOMODCACHE GOCACHE
```

本地测试用模拟服务器验证认证编码、换绑确认与取消、确认请求不重试、错误分支、分片合并、文件一致性、取消和不覆盖行为。**模拟测试不能替代校园网账号登录与双路带宽实测。**

### 2026-10-18 初期验证记录

- `go test ./...`、`go vet ./...`、Windows 可执行文件构建通过。
- 在真实 Windows socket 上设置并读取 `IP_UNICAST_IF` 的测试通过。
- 使用正式模块绑定以太网后，实际校园登录页解析成功，动态表单参数及 IV 可读取；没有提交登录请求。
- `gwifi check -exit wired` 的有线连通性检查通过。
- 无线网卡当时未连接。真实账号认证、双出口同时在线、实际带宽叠加尚未验证。

### 2026-10-19 性能优化与实测

以下记录来自开发测试电脑，仅代表当时网络、账号套餐和下载源条件。测试时有线 LAN 协商为 1 Gbps，无网卡错误或丢弃包计数，Windows TCP 接收窗口自动调节为 normal；未改动系统网络参数或路由器。对同一阿里云 Go ZIP 地址进行限时 Range 吞吐对照，发现增加有线连接数能明显提高吞吐。以下单路稳态值取 12 秒测试中第 3 秒之后的数据：

| 有线连接数 | 有线稳态吞吐 |
| --- | --- |
| 8 | 5.90 MB/s，47.2 Mbps |
| 16 | 10.55 MB/s，84.4 Mbps |
| 24 | 11.93 MB/s，95.4 Mbps |

双路 `wired=24,wifi=4` 的 15 秒循环 Range 吞吐测试，全程平均 **18.33 MB/s（146.6 Mbps）**，没有请求失败。这是下载到内存的网络吞吐测试，不是完整文件下载的平均速度，也不是长期带宽保证。单凭此测试不能区分服务器单连接限制与线路上单连接吞吐不足的具体原因。

随后使用优化后的正式程序完整下载同一个 79,008,081 字节 ZIP：下载日志最终平均 **14.68 MB/s（117.5 Mbps）**；包含进程启动和结束的计时为 5.575 秒、14.17 MB/s。SHA256 与已核实的官方值 `1314008898bd40df77af4b014f777f08873dbdfbcd3d92308728ee03304fe04f` 一致。有线与 WLAN 接收计数分别增加约 46.31 / 32.90 MiB，确认两块物理网卡实际承载流量。小文件的建连与尾部等待仍会拉低全程平均速度。

优化包含分出口设置 1..32 并发、并行元信息探测、缩小尾部分片、每出口连接池容量 32 和近期分出口速度显示。完整 `go test ./...`、`go vet ./...`、Windows 构建通过；新增测试覆盖并发上限、分片不重叠且不遗漏、文件内容一致、极小文件和取消行为。上述性能数据是开发期间的实测记录，原始本机诊断日志不随源码仓库发布。

## 依据

- [EW1200G PRO 官方配置指南](https://reyee.ruijie.com/en-global/support/documents/slide_ruijie-reyee-rg-ew1200g-pro-wi-fi-router-reyeeos-1-380-configuration-guide/)
- [Windows IP_UNICAST_IF](https://learn.microsoft.com/en-us/windows/win32/winsock/ipproto-ip-socket-options)
- [HTTP Range / RFC 9110](https://www.rfc-editor.org/rfc/rfc9110.html#section-14)
- 认证适配依据：用户提供的校园网登录页及页面内 `/gportal/Web/loginAction` JavaScript。项目不保存真实页面签名、账号、密码或 Cookie。

## 让其他软件使用双路

此功能需要先安装并运行 Clash Verge Rev，启用其 Mihomo TUN，并准备可用的 Node.js、Python 和 PyYAML。`scripts/gwifi-mihomo.js` 为已有配置添加网卡出站与负载均衡，`scripts/system-download.ps1` 提供安装、切换和回退。不启动第二个 TUN，不要求逐个软件设置 HTTP 代理。首次使用先执行下方的 `prepare` 和 `install`，再使用双路开关。

集成脚本默认网卡名称为 `以太网` 和 `WLAN`，与 Go 程序的 `config.local.json` 分开配置。若自己的网卡名称不同，先按 `gwifi interfaces` 输出修改 `scripts/gwifi-mihomo.js` 中三个出站的 `interface-name`，再安装集成。该脚本中的校园网直连保护规则也针对本文学校网络；其他网络需先核对适配。

集成安装完成后，日常使用直接双击项目根目录的 **`开启双路.cmd`**、**`关闭双路.cmd`** 或 **`查看双路状态.cmd`**。关闭双路会把总开关切回原 `DIRECT`，不会关闭 TUN 或海外代理。脚本使用自身路径定位项目，从其他目录打开也能工作；操作结果会保留在窗口中，按任意键关闭。也可以给这些文件创建桌面快捷方式；不要单独移动脚本文件。

在 Clash Verge 的代理页面，也可以直接切换 **`GWIFI-DOWNLOAD`**：选择 **`GWIFI-Dual`** 开启双路，选择 **`DIRECT`** 关闭分流。开启前仍需两块网卡各自联网，单路掉线检测存在下述检查间隔。

开关针对新建立的连接，不会强行断开已有下载；需要让正在下载的任务采用新模式时，暂停后重新开始下载。

这与 `gwifi fetch` 的 Range 分片不同：**全软件模式只把不同连接分配给不同网卡，不会拆分一条 TCP/QUIC 连接**。浏览器单连接下载仍受单路上限约束；支持多连接的下载软件、多任务同时下载才有机会利用两路总容量。海外流量保留现有代理路径，不能据此保证所有软件下载达到 150 Mbps。

原来指向 `DIRECT` 的公网规则会转到 `GWIFI-DOWNLOAD`；当前策略组若选中 `DIRECT`，安装时会迁移到该总开关。其他代理选择保持。校园认证页、局域网与原显式 IPv6 规则保持直连处理。新出站采用 `ipv4-prefer`，不会人为禁止 IPv6，但每块网卡的 IPv6 可达性仍取决于网络，未验证 IPv6 双路性能。

`GWIFI-Dual` 使用两份有线节点与一份无线节点，以约 2:1 分配**新连接数量**。这不是按下载字节数或实时带宽调度，也不会套用 `fetch` 的 24:4 并发设置。每 15 秒进行 HTTPS 204 健康检查，跳过检测为不可用的出口；掉线到检测完成之间仍可能失败，已有连接不能迁移。

```powershell
# 只检查配置是否兼容，不安装
.\scripts\system-download.ps1 -Action prepare

# 安装并先使用有线；已有有效安装不会重复覆盖
.\scripts\system-download.ps1 -Action install

# 保持网线，连接学校 Gwifi 并分别认证后，启用双路
.\bin\gwifi.exe check
.\scripts\system-download.ps1 -Action dual
.\scripts\system-download.ps1 -Action status

# 切回仅有线，或关闭分流并恢复原 DIRECT 行为
.\scripts\system-download.ps1 -Action wired
.\scripts\system-download.ps1 -Action off

# 完整恢复安装前配置（检测到配置漂移时会拒绝覆盖）
.\scripts\system-download.ps1 -Action uninstall
```

保持 Clash Verge 的 **规则模式** 和 **TUN 模式**。订阅更新或切换后，若某个国内/下载策略组仍选中原来的 `DIRECT`，可在该组选择新增的 `GWIFI-DOWNLOAD`。原来的 `DIRECT` 选项仍保留，供单独退出分流。`GWIFI-DOWNLOAD` 内的 `GWIFI-Dual` 为双路，`GWIFI-Wired-A` 为有线，`GWIFI-WiFi` 为无线，`DIRECT` 为原直连。

安装器依赖本机 Node.js、Python/PyYAML 和已有 Mihomo 内核。默认查找 `%APPDATA%\io.github.clash-verge-rev.clash-verge-rev`，只自动接入空的全局脚本，遇到已有自定义全局脚本会停止以保留其内容。订阅级脚本在全局脚本之后运行；若它会覆写规则，可能需要手动合并。先通过 `prepare` 检查当前配置的兼容性。

全局 `profiles/Script.js` 的改动会保留，随后通过本地命名管道热加载到当前内核。**API 热加载不更新 Verge 的内存配置**；安装后在 Clash Verge 对当前订阅执行一次“重新激活配置”，让界面、生成配置与内核同步。普通订阅重新生成会重新执行全局脚本。原脚本、运行配置及策略选择备份在 Clash Verge 配置目录的 `gwifi-backup`，备份包含原有私人订阅信息，请留在本机；终端输出和项目准备记录不输出订阅密钥。

该脚本不重写 `sub-rules`，不修改代理节点的上游连接，不保证绕过其他 VPN、独立虚拟机网络或软件自行绑定的网卡。全软件模式的实际覆盖与速度要通过目标软件和物理网卡流量确认。

### 2026-10-19 全软件集成验证

- 开发测试电脑上的 Mihomo `v1.19.32` 对实际订阅合并后的候选配置校验通过；10 项 Node 测试覆盖规则改写、原代理保留、内网保护、重复执行与名称冲突等行为。
- 隔离实例关闭 TUN、仅监听本机测试端口，确认无线断开时检测为不可用，双路组仍能通过有线返回正确的 `206` 与 1 MiB 内容；测试实例已关闭。
- 实际接入原有 TUN 后，不指定代理或网卡的 6 个普通 TCP Range 请求全部成功，共 6 MiB。实际连接记录观察到 `Tun → GWIFI-DOWNLOAD → GWIFI-Dual → GWIFI-Wired-A`，证明普通连接走到了新分流策略。
- 保留规则模式及已开启的 TUN，8 个原策略选择保持，4 个原 DIRECT 选择迁移至总开关，无其他选择变化。完整卸载恢复及重新安装也已实际执行成功；该次验证结束时选中 `GWIFI-Dual`。
- **当次集成验证时 WLAN 已断开，仅验证了有线和无线不可用时的处理。尚未实测 Steam、网盘等具体软件的双路速度，不能把之前 `fetch`/Range 压测的 146.6 Mbps 当作全软件模式成绩。** Wi-Fi 连接学校网络并通过认证后，周期检查会使其重新参与新连接。

参考：[Mihomo DIRECT 出站](https://wiki.metacubex.one/config/proxies/direct/)、[负载均衡策略](https://wiki.metacubex.one/config/proxy-groups/load-balance/)、[Clash Verge 全局脚本](https://www.clashverge.dev/guide/script.html)。
