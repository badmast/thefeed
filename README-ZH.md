# thefeed

基于 DNS 的 Telegram 频道与公开 X 账号阅读器。专为只有 DNS 查询能通的网络环境而设计。

[English](README.md) | [فارسی](README-FA.md) | 简体中文 | [Русский](README-RU.md)

## 下载

- **最新版本** —— 各平台的服务端 / 客户端二进制以及 Android APK。选一个能访问的镜像即可：[GitLab](https://gitlab.com/sartoopjj/thefeed/-/releases) / [GitHub](https://github.com/sartoopjj/thefeed/releases/latest)。
- **服务端一键安装**(Linux + systemd) —— 选一个能通的镜像：
  ```bash
  # GitHub 镜像
  sudo bash -c "$(curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh)"

  # GitLab 镜像（GitHub 账号暂不可用时使用）
  sudo bash -c "$(curl -Ls https://gitlab.com/sartoopjj/thefeed/-/raw/main/scripts/install.sh)" -- --gitlab
  ```
- **Android APK**(Android 7.0+):2017 年以后的设备装 `arm64-v8a`，更老的 32 位机型装 `armeabi-v7a`。
- **iOS**(iOS 14+):App Store 版本计划中。源码在 [ios/](ios/) 目录，参见下文 [iOS 开发](#ios-开发)。

可用于测试的公开配置：[@thefeedconfig](https://t.me/thefeedconfig)。

## 截图

<table align="center">
<tr>
<td align="center"><img src="docs/screenshots/feed-list.jpg" width="170" alt="Main feed"><br><sub>主信息流</sub></td>
<td align="center"><img src="docs/screenshots/feed-post.jpg" width="170" alt="Reading a post"><br><sub>阅读消息</sub></td>
<td align="center"><img src="docs/screenshots/telemirror.jpg" width="170" alt="Telemirror"><br><sub>Telemirror</sub></td>
</tr>
<tr>
<td align="center"><img src="docs/screenshots/scanner.jpg" width="170" alt="Resolver scanner"><br><sub>解析器扫描</sub></td>
<td align="center"><img src="docs/screenshots/resolver-bank.jpg" width="170" alt="Resolver bank"><br><sub>解析器池</sub></td>
<td align="center"><img src="docs/screenshots/settings.jpg" width="170" alt="Settings"><br><sub>设置</sub></td>
</tr>
</table>

## 工作原理

```
                                  Encrypted DNS TXT
   ┌──────────────┐  feed meta + small media   ┌──────────────────┐    MTProto      ┌──────────┐
   │              │ ─────────────────────────▸ │      Server      │ ─────────────▸  │ Telegram │
   │    Client    │ ◂───────────────────────── │  (DNS auth +     │ ◂─────────────  │   API    │
   │  (Web UI)    │                            │   media relays)  │    RSS / HTTP   ┌──────────┐
   │              │  large media (fast relay)  │                  │ ─────────────▸  │  Nitter  │
   │              │ ◂───── api.github.com ◂──  │                  │ ◂─────────────  │ (X feed) │
   └──────────────┘     (uploaded by server)   └──────────────────┘                 └──────────┘
```

**服务端**(部署在墙外)：
- 连接 Telegram，读取指定频道的消息
- 通过 RSS 兼容镜像抓取公开 X 帖子(无需登录)
- 以加密 DNS TXT 响应分发信息流元数据和小媒体
- **媒体中继** —— 同一个文件支持多条投递路径：
  - **DNS 中继**(慢，抗封锁) 将字节切片打包到多个 DNS 块中
  - **GitHub 中继**(快，默认关闭) 把字节上传到仓库，客户端走普通 HTTPS 拉取；适合 DNS 装不下的大文件
  - 未来新增的中继可以并存，不会破坏老客户端
- 响应携带随机填充(抗 DPI)
- **多域名**(`--extra-domains` / `THEFEED_EXTRA_DOMAINS`):服务器在主域名之外还可在多个子域名上响应 feed 查询，客户端把块请求分散到所有域名上(分担负载,且某个域名被封时仍可用)。主域名对中继路径保持不变。导入 URI 用 `d=` 字段携带子域名。
- **私信通道**(`--chat-domains` / `THEFEED_CHAT_DOMAINS`):可选的存储转发私信功能,服务于同一服务器的用户之间,运行在专用子域名上 — 见「私信通道」一节
- 会话持久化 —— 登录一次后长期生效
- 无 Telegram 模式(`--no-telegram`) —— 无凭据也能读公开频道
- 所有数据统一存放在一个目录

**客户端**(部署在墙内)：
- 浏览器端 Web UI，原生支持 RTL/波斯语(VazirMatn 字体)
- 通过解析器池发送加密的 DNS TXT 查询
- **解析器池**：跨所有配置共享的 DNS 解析器池，可通过扫描、导入或手动添加，并自动打分
- **内置初始配置**：导入窗口自带几个现成配置（含解析器预设），一键即可导入
- **解析器评分**：按成功率 + 延迟对每个解析器持续打分，优先用健康的解析器；低分的可以清掉
- **散射模式(Scatter)**：把同一个 DNS 请求同时发给多个解析器，谁先回就用谁(默认 2 路并发)
- **中继感知的媒体下载** —— 当 manifest 标注有快速中继时自动选用，失败重试，回退到慢通道(DNS)前会先询问。每次下载都校验哈希和大小
- 向频道和私聊发送消息(需服务端开启 `--allow-manage` 且已登录 Telegram)
- 频道管理：当 `--allow-manage` 开启时，可通过管理命令远程添加/删除频道
- **按频道自动更新**：可以钉住特定频道做后台定时刷新，按 profile 存档
- 消息走 deflate 压缩，传输效率更高
- Web UI 支持密码保护(客户端的 `--password` 参数)
- 新消息提示(频道列表的 NEW 徽章 + 聊天内的分隔条)、下次抓取的倒计时
- 频道类型徽章(私有/公开/X) 用不同颜色区分
- 媒体类型识别(`[IMAGE]`、`[VIDEO]` 等) 并直接渲染
- 浏览器内实时查看 DNS 查询日志

## 协议

所有通信都用 AES-256 加密，承载在标准 DNS TXT 查询/响应上，配合可变填充和解析器评分，让流量看起来与正常 DNS 活动无异。消息数据在加密前会先经过 deflate 压缩。

## 图片与文件下载

带图片、文件、GIF、音频或视频的消息可以在服务端缓存，再通过同一条加密 DNS 通道下载。

服务端对每个媒体文件去重(按上游 id 和内容哈希)，把字节推送到所有已启用的中继上，并在消息文本里加一个小的元数据头：

```
[IMAGE]<size>:<flags>:<dnsCh>:<dnsBlk>:<crc32>[:<filename>]
optional caption
```

`<flags>` 是逗号分隔的位列表，表示每个中继的可用性(`1`=可用，`0`=不可用)。槽 0 是 DNS，槽 1 是 GitHub 中继；未来新中继向后追加。老客户端会忽略不认识的槽位。

每个 DNS 缓存文件的第 0 块都以 16 字节协议头开头：4 字节 CRC32(对应解压后内容)、1 字节版本、1 字节压缩算法、10 字节预留。客户端在交付任何字节之前都会先校验 CRC，然后按压缩字节解压。下载在客户端(IndexedDB，7 天)和本地 thefeed-client 服务(`<dataDir>/media-cache/`，7 天) 都有缓存。并发下载有上限，多余的点击会排队。

### 媒体中继

每个中继都是独立的 —— 同一个文件可以同时通过 DNS、GitHub 和未来的中继提供。客户端按消息 manifest 上声明的中继选择最快的那条；失败时重试，要回退到慢通道前会先询问。每次下载都校验哈希和大小。

目前已有两种中继：

- **DNS 中继**(慢，默认关闭)。字节拆分到 DNS 块中投递，可在被审查的网络中存活。默认上限 100 KB。
- **GitHub 中继**(快，默认关闭)。字节上传到仓库，客户端走普通 HTTPS 拉取。需要带 `contents:write` 权限的 PAT。文件落到 `<repo>/<sanitised-domain>/<size>_<crc32>`，方便多个部署共用一个仓库。默认上限 15 MB。

服务端参数 / 环境变量：

| 参数                          | 环境变量                              | 默认值      | 说明                                |
|-------------------------------|--------------------------------------|-------------|-------------------------------------|
| `--dns-media-enabled`         | `THEFEED_DNS_MEDIA_ENABLED`          | `false`     | DNS 中继开关                        |
| `--dns-media-max-size`        | `THEFEED_DNS_MEDIA_MAX_SIZE_KB`      | `100` (KB)  | 单文件上限                          |
| `--dns-media-cache-ttl`       | `THEFEED_DNS_MEDIA_CACHE_TTL_MIN`    | `600` (min) | TTL                                 |
| `--dns-media-compression`     | `THEFEED_DNS_MEDIA_COMPRESSION`      | `gzip`      | `none`、`gzip` 或 `deflate`         |
| `--github-relay-enabled`      | `THEFEED_GITHUB_RELAY_ENABLED`       | `false`     | GitHub 中继开关                     |
| `--github-relay-token`        | `THEFEED_GITHUB_RELAY_TOKEN`         | —           | PAT，需 `contents:write` 权限       |
| `--github-relay-repo`         | `THEFEED_GITHUB_RELAY_REPO`          | —           | `owner/repo`                        |
| `--github-relay-branch`       | `THEFEED_GITHUB_RELAY_BRANCH`        | `main`      | 提交中继对象到哪个分支              |
| `--github-relay-max-size`     | `THEFEED_GITHUB_RELAY_MAX_SIZE_KB`   | `15360` (KB)| 单文件上限                          |
| `--github-relay-ttl`          | `THEFEED_GITHUB_RELAY_TTL_MIN`       | `600` (min) | 孤儿对象在下一轮刷新时清理          |

每小时的 DNS 报告会带上 `totalMediaQueries` 和一个 `mediaCache` 块(条数、字节数、命中、未命中、淘汰)。

## 私信通道

可选的、独立的存储转发私信功能,用于同一服务器的用户之间通信(与 Telegram 完全无关)。为服务器配置一个或多个专用子域名(必须不同于 feed 域名)即可启用:

```bash
thefeed-server ... --chat-domains c.example.com
# 或: THEFEED_CHAT_DOMAINS=c.example.com
```

- **端到端加密**:只有双方能读取消息 — 服务器仅存储不透明数据块,并在不读取内容的情况下验证发送者。联系人名称永远不离开设备。
- **身份**:客户端在本地生成恢复码;你的地址是由它派生的 20 个字符。通过其他渠道把地址告诉对方即可被联系;同一恢复码可在任何服务器上使用。
- **失败即关闭的安全模型**:只有当配置中固定了服务器公钥(`sk=`)且聊天能力数据通过签名验证时,客户端才会启用私信。 feed 元数据中有一个签名比特位标明服务器是否有私信功能,因此没有密钥的客户端会提示「此服务器有私信 — 请用其密钥重新导入配置」而不是静默失败,且无私信的服务器上客户端绝不浪费一次探测。
- **保留域名但关闭**:`--chat-enabled=false`(或 `THEFEED_CHAT_ENABLED=0`)保留域名配置但对外宣告私信已停用,客户端显示「此服务器已停用私信」。
- **滥用限制**(自动告知客户端):`--chat-send-per-hour`(30)、`--chat-inbox-cap`(50)、`--chat-per-pair-cap`(10)、`--chat-max-msg-bytes`(500)。未送达的消息在 `--chat-ttl-hours`(72)后过期。
- **账户默认永久保留**(`--chat-account-ttl-days 0`)以保证报告准确;繁忙的服务器可设置天数以回收闲置账户。`--chat-max-accounts`(0 = 无限)限制账户总数。
- **持久性与吞吐的权衡**(`--chat-sync-seconds`,默认 1):消息库每 N 秒刷盘一次,因此崩溃时最多丢失约 N 秒内刚收到的消息(无妨——聊天端到端加密,发送方会重发)。设为 `0` 则每条消息都 fsync(严格持久,吞吐更低)。
- 在客户端界面从侧边栏打开**私信**。✓ = 已存储到服务器,✓✓ = 对方已取走。两台设备上的安全表情一致即表示会话安全。

每小时 DNS 报告包含 `totalChatQueries` 和 `chat` 块(账户数、消息数、注册数、会话数)。

### 运维报告(TUI)

服务器把每个每小时报告作为一行 JSON 追加到 `<data-dir>/dns_hourly.jsonl`(按大小轮转,保留若干备份)。服务器二进制自身用 `--report` 从该文件渲染终端仪表板,不在网络上提供任何服务,只读取数据目录:

```bash
thefeed-server --report                       # 读取 ./data/dns_hourly.jsonl 和 ./data/chat.db
thefeed-server --data-dir /srv/thefeed --report
thefeed-server --report --report-refresh 5s   # 实时,每 5 秒刷新
```

它显示总查询/频道抓取/元数据/媒体/私信查询、带条形图的每频道均值、各域名总计、每报告火花线、按小时查询量以及私信统计(包括来自 `chat.db` 的实时账户数)— 与 `scripts/thefeed_log_report.py` 相同的聚合,绘制在终端中。

## 赞助

如果想支持作者，可以通过以下网络发送任意金额的 USDT 或 USDC：

- Polygon
- BNB Chain

钱包地址：
`0xe73f022f668c57cce79feccd875ac7332311013a`

感谢支持 ❤️

## 链接
- 作者的 telegram 频道：[@networkti](https://t.me/networkti)
- 公开 TheFeed 配置：[@thefeedconfig](https://t.me/thefeedconfig)
- TheFeed 服务端搭建指南：[@networkti](https://t.me/networkti/25)
- 用 SlipGate 搭建 TheFeed 服务端：[@networkti](https://t.me/networkti/200)
- 路线图 / 任务面板：[GitHub project](https://github.com/users/sartoopjj/projects/1/views/1)

## 服务器快速安装

安装脚本可以从 GitHub 或 GitLab 镜像拉取二进制。
默认自动探测(先试 GitHub，失败回退到 GitLab)；GitHub 账号不可用时可以传 `--gitlab` 强制走 GitLab。

```bash
# GitHub 镜像
sudo bash -c "$(curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh)"

# GitLab 镜像
sudo bash -c "$(curl -Ls https://gitlab.com/sartoopjj/thefeed/-/raw/main/scripts/install.sh)" -- --gitlab
```

或者手动：

```bash
# 在你的服务器上(Linux + systemd)
curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh -o install.sh
sudo bash install.sh                # 自动：先 GitHub，再回退到 GitLab
sudo bash install.sh --gitlab       # 强制走 GitLab 镜像
sudo bash install.sh --source github
```

脚本会：
1. 从 GitHub 拉取最新发行版的二进制
2. 询问你的域名、口令、Telegram 频道和 X 账号
3. 询问是否使用 Telegram 登录(建议选 **不用** —— 公开频道无需登录就能读)
4. 如果走 Telegram 模式：询问 API 凭据并完成登录
5. 配置 systemd 服务

**更新**：再次运行上面的一键安装命令即可。

**安装指定版本**(用 `--version v0.9.2` 回滚，用 `--pre` 装最新预发布，用 `--list` 查看最近发行版)：

```bash
curl -Ls https://raw.githubusercontent.com/sartoopjj/thefeed/main/scripts/install.sh | sudo bash -s -- --version v0.9.2
```

短选项：`-v <tag>` 等同于 `--version <tag>`，旧的位置参数 `sudo bash install.sh v1.0.0` 也仍然有效。

**重新登录** / **卸载** / **查看配置**：把上面命令尾部的参数换成 `--login` / `--uninstall` / `--config` 即可。`--config` 会打印导入 URI（域名、密钥、服务器公钥 `sk=` 和引导解析器）。


> **提示：** 服务端需要接收来自外部 53 端口的包。直接监听 `:53` 需要 root 权限，建议让程序监听一个非特权端口(`:5300`)，再把 53 端口转发过去。
>
> 把 `eth0` 换成你实际的网卡名(用 `ip a` 查看)：
> ```bash
> sudo iptables -I INPUT -p udp --dport 5300 -j ACCEPT
> sudo iptables -t nat -I PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
> sudo ip6tables -I INPUT -p udp --dport 5300 -j ACCEPT
> sudo ip6tables -t nat -I PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
> ```
>
> 让规则在重启后仍生效：
> ```bash
> sudo apt install iptables-persistent   # Debian/Ubuntu
> sudo netfilter-persistent save
> ```


**出问题时**可以快速移除转发：`sudo iptables -t nat -D PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300`、`sudo iptables -D INPUT -p udp --dport 5300 -j ACCEPT`、`sudo netfilter-persistent save`，恢复原状。

## Docker 部署(服务端)

用 Docker 跑服务端 —— 无需 Go 工具链。

### 快速开始(公开频道，无 Telegram 登录)

```bash
# 1. 配置环境变量
cp .env.example .env
nano .env   # 设置 THEFEED_DOMAIN 和 THEFEED_KEY

# 2. 准备数据目录和频道列表
mkdir -p data
cp configs/channels.txt data/
cp configs/x_accounts.txt data/   # 可选

# 3. 构建并启动
docker compose up -d

# 4. 把外部 DNS 流量重定向到容器
#    把 eth0 换成你的网卡(用 ip a 查看)
sudo iptables -t nat -I PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
sudo iptables -I INPUT -p udp --dport 5300 -j ACCEPT
sudo ip6tables -t nat -I PREROUTING -i eth0 -p udp --dport 53 -j REDIRECT --to-ports 5300
sudo ip6tables -I INPUT -p udp --dport 5300 -j ACCEPT

# 让 iptables 规则在重启后仍生效
sudo apt install -y iptables-persistent
sudo netfilter-persistent save

# 5. 查看日志
docker compose logs -f
```

> **提示：** 容器监听的是 5300(不是 53)，避免和 `systemd-resolved` 冲突。
> `iptables PREROUTING` 规则只会把**外部**的 DNS 流量(53 端口) 重定向到容器，
> 服务器本地的 DNS 解析照常工作，不受影响。

### 启用 Telegram(一次性交互登录)

```bash
# 1. 配置环境变量(在 .env 中解开 Telegram 相关变量的注释)
cp .env.example .env
nano .env

# 2. 一次性登录(交互式 —— 按提示输入验证码)
docker compose run -it --rm server \
  --login-only --data-dir /data \
  --domain YOUR_DOMAIN --key YOUR_KEY \
  --api-id YOUR_API_ID --api-hash YOUR_HASH \
  --phone YOUR_PHONE

# 3. 编辑 docker-compose.yml：移除 --no-telegram，加入 Telegram 相关参数
# 4. 启动服务
docker compose up -d
# 5. 配置 iptables 转发(同上面快速开始的第 4 步)
```

### Docker 细节

| 项目 | 值 |
|------|-------|
| 基础镜像 | `alpine:3.21`(总共约 23 MB) |
| 构建 | 多阶段(`golang:1.26-alpine` → `alpine`) |
| 用户 | `thefeed`(UID 1000，非 root) |
| 容器端口 | `:5300/udp`(宿主 `:5300/udp` + iptables 把 `:53` 转过来) |
| 数据 | `./data` 卷(频道、会话、缓存) |
| 配置 | `.env` 文件(被 gitignore) |

```bash
# 代码改动后重新构建
docker compose build

# 停止
docker compose down
```

### 53 端口与服务安全

容器监听 **5300** 端口(不是 53)，避免和宿主上的 `systemd-resolved` 或其他 DNS 服务冲突。配置前用 `ss -ulnp | grep ':53 '` 看下谁在占 53 端口(预期只有 systemd-resolved 在 127.0.0.53)，配置后用 `dig +short google.com @127.0.0.53` 确认本地 DNS 仍能正常解析，`iptables -t nat -L PREROUTING -n | grep 5300` 确认转发规则已生效。

## 手动安装

### 前置条件

- Go 1.26+
- 一个域名，NS 记录指向你的服务器
- 从 https://my.telegram.org 拿到的 Telegram API 凭据(只有读取私有频道才需要)

### 服务端

```bash
# 构建
make build-server

# 首次运行：登录 Telegram 并保存会话
./build/thefeed-server \
  --login-only \
  --data-dir ./data \
  --domain t.example.com \
  --key "your-secret-passphrase" \
  --api-id 12345 \
  --api-hash "your-api-hash" \
  --phone "+1234567890"

# 正常运行(从数据目录读取已保存的会话)
./build/thefeed-server \
  --data-dir ./data \
  --domain t.example.com \
  --key "your-secret-passphrase" \
  --api-id 12345 \
  --api-hash "your-api-hash" \
  --phone "+1234567890" \
  --listen ":53"
```

所有数据文件(会话、频道、x 账号) 都存在 `--data-dir` 目录里(默认 `./data`)。

环境变量：`THEFEED_DOMAIN`、`THEFEED_KEY`、`THEFEED_MSG_LIMIT`、`THEFEED_FETCH_INTERVAL`、`THEFEED_ALLOW_MANAGE`(设为 `0` 可以强制关掉，即使二进制带了对应的 flag)、`THEFEED_X_RSS_INSTANCES`、`TELEGRAM_API_ID`、`TELEGRAM_API_HASH`、`TELEGRAM_PHONE`、`TELEGRAM_PASSWORD`

#### 服务端参数

| 参数 | 默认值 | 说明 |
|------|---------|-------------|
| `--data-dir` | `./data` | 频道、会话、配置的数据目录 |
| `--domain` | | DNS 域名(必填) |
| `--key` | | 加密口令(必填) |
| `--channels` | `{data-dir}/channels.txt` | 频道文件路径 |
| `--x-accounts` | `{data-dir}/x_accounts.txt` | X 账号文件路径 |
| `--x-rss-instances` | `https://nitter.net,http://nitter.net` | 逗号分隔的 X RSS 源 |
| `--api-id` | | Telegram API ID(必填) |
| `--api-hash` | | Telegram API Hash(必填) |
| `--phone` | | Telegram 手机号(必填) |
| `--session` | `{data-dir}/session.json` | Telegram 会话文件路径 |
| `--login-only` | `false` | 完成登录并保存会话后立即退出 |
| `--no-telegram` | `false` | 不登录 Telegram(仅公开频道) |
| `--listen` | `:5300` | DNS 监听地址 |
| `--padding` | `32` | 最大随机填充字节(0 = 关闭) |
| `--msg-limit` | `15` | 每个 Telegram 频道单次抓取的最大消息数 |
| `--fetch-interval` | `10` | 抓取轮询间隔，分钟(最小 3) |
| `--allow-manage` | `false` | 允许远程发消息 / 管理频道(默认关闭) |
| `--debug` | `false` | 打印每条解码后的 DNS 查询 |
| `--dns-media-enabled` | `false` | 通过 DNS 提供媒体(慢中继) |
| `--dns-media-max-size` | `100` | DNS 中继单文件上限，KB(0 = 不限) |
| `--dns-media-cache-ttl` | `600` | DNS 中继 TTL，分钟 |
| `--dns-media-compression` | `gzip` | DNS 中继压缩方式：`none`、`gzip` 或 `deflate` |
| `--github-relay-enabled` | `false` | 启用 GitHub 快速中继 |
| `--github-relay-token` | | 带 `contents:write` 的 PAT(或 `THEFEED_GITHUB_RELAY_TOKEN`) |
| `--github-relay-repo` | | 中继使用的 `owner/repo` |
| `--github-relay-branch` | `main` | 提交中继对象的分支 |
| `--github-relay-max-size` | `15360` | GitHub 中继单文件上限，KB |
| `--github-relay-ttl` | `600` | GitHub 中继 TTL，分钟(孤儿在下一轮清理) |
| `--version` | | 显示版本后退出 |

### 客户端

```bash
# 构建
make build-client

# 运行(自动在浏览器打开 Web UI)
./build/thefeed-client

# 自定义数据目录和端口
./build/thefeed-client --data-dir ./mydata --port 9090

# 启用远程管理
./build/thefeed-client --password "your-secret"
```

首次运行时，客户端会在当前目录旁创建 `./thefeeddata/`。打开浏览器访问 `http://127.0.0.1:8080`，在 Settings 页面配置域名和口令。DNS 解析器在共享的 Resolver Bank(从侧栏进入) 里管理，所有 profile 共用。

所有配置、缓存和数据文件都存在数据目录里。

#### 客户端参数

| 参数 | 默认值 | 说明 |
|------|---------|-------------|
| `--data-dir` | `./thefeeddata` | 配置、缓存的数据目录 |
| `--port` | `8080` | Web UI 端口 |
| `--password` | | Web UI 密码(为空表示无认证) |
| `--version` | | 显示版本后退出 |

**并发请求(scatter)** 以及其他所有 profile 选项(解析器、限速、查询模式、超时) 都在 Web UI 的 profile 编辑器里配置，不通过 CLI 参数。

#### macOS(.app / .dmg)

每个发行版都附带一个通用的 `thefeed-macos-<version>.dmg`，里面打包了客户端，拖动即可安装为 `Thefeed.app`。同一个二进制在 Intel 和 Apple Silicon 上都能跑。应用会启动本地 Web UI 并打开浏览器；数据持久化到 `~/Library/Application Support/Thefeed`。运行后会在菜单栏(屏幕右上角) 出现一个 "Thefeed" 图标，里面有 **Open Thefeed** 和 **Quit Thefeed** —— 想干净地停掉服务请用菜单退出。子进程日志写到 `~/Library/Application Support/Thefeed/launcher.log`，可用于排错。

DMG 没有签名，首次启动需要：

```bash
# A) 在 Finder 中右键 → 打开(一次性确认即可)
# B) 用终端清掉隔离属性
xattr -dr com.apple.quarantine /Applications/Thefeed.app
```

在 macOS 上本地构建：

```bash
make mac-dmg
# → build/Thefeed.app  +  build/thefeed-macos-<version>.dmg
```

#### Android(Termux)

```bash
# 从 F-Droid 安装 Termux
pkg update && pkg install curl

# 下载 Android 二进制
curl -Lo thefeed-client https://github.com/sartoopjj/thefeed/releases/latest/download/thefeed-client-android-arm64
chmod +x thefeed-client
./thefeed-client
# 浏览器打开：http://127.0.0.1:8080
```

#### Android(原生 APK)

最新发行版资源里有两个 APK:`thefeed-android-<version>-arm64-v8a.apk`(现代 64 位手机，2017 年后基本都是) 和 `thefeed-android-<version>-armeabi-v7a.apk`(老式 32 位手机)。装错版本时 Android 可能会装上去但内置的原生二进制跑不起来；除非你确定是 32 位设备，否则装 `arm64-v8a`。

原生应用会在前台/后台 service 中运行客户端二进制，并在应用内 WebView 里打开本地 Web UI。首次启动时会自动申请省电豁免，避免后台被系统杀掉。源码在 `android/`，本地构建步骤：

```bash
# 1) 项目根目录构建 Android 二进制
make build-android-arm64

# 2) 拷贝到 Android 应用资源目录(文件名必须一致)
cp build/thefeed-client-android-arm64 android/app/src/main/assets/thefeed-client

# 3) 构建并安装 debug APK
cd android && gradle wrapper --gradle-version 8.10.2 && ./gradlew assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

### Web 界面

浏览器界面包含：
- **频道侧栏**(左侧)：按类型(公开/X/私有) 分组的频道列表，带徽章
- **消息面板**(右侧)：原生 RTL/波斯语渲染(VazirMatn 字体)
- **发送面板**：当 Telegram 已连接时，可向频道和私聊发送消息
- **新消息徽章**：哪些频道有新消息一目了然
- **下次抓取倒计时**：到下一次自动刷新的时间
- **媒体识别**：高亮 `[IMAGE]`、`[VIDEO]`、`[DOCUMENT]` 等标签
- **消息搜索**：在当前频道内搜索，高亮匹配并提供上一条/下一条导航
- **导出消息**：把当前频道最近 N 条消息导出到剪贴板
- **日志面板**(底部)：实时 DNS 查询日志
- **设置弹窗**：配置域名、口令、解析器、查询模式、限速、并发(scatter)、超时、调试模式
- **可用解析器**：在设置里查看当前活跃/健康的解析器列表
- **背景图**：为消息面板设置自定义背景图 URL(本地保存)
- **DNS 查询超时**：在 profile 编辑器中按 profile 配置查询超时(默认 15s)
- **按 profile 缓存**：浏览器端 1 小时缓存，重新打开时数据立即可见
- **解析器扫描**：扫描 IP 段或 CIDR，发现可用的 DNS 解析器

### 解析器扫描

Web UI 内置了解析器扫描功能(侧栏的 🔍 图标)，可以扫一段 IP 找到能联通你 thefeed 服务端的 DNS 服务器。特性：

- **灵活目标**：可以输入单个 IP、CIDR(例如 `5.1.0.0/16`) 或域名 —— 一行一个
- **默认 CIDR 预设**：一键加载内置的精选 CIDR 列表
- **清空目标**：一键清空扫描器的 CIDR/IP 列表
- **按 profile 工作**：选定哪个 profile 的域名和口令用来探测
- **可配置**：设置并发数(默认 50)、超时(默认 15s)、最大扫描 IP 数
- **/24 扩展**：发现可用解析器后，自动扫描同一个 /24 子网内的相邻 IP
- **暂停 / 继续 / 停止**：长时间扫描的全套控制(暂停会真的停止派发新探测)
- **响应时间**：结果按延迟排序，最快的解析器排在前面
- **可选结果**：用复选框选择要应用或复制的解析器
- **应用结果**：直接从扫描器把结果追加或覆盖到当前 profile 的解析器列表
- **复制**：单 IP 复制、批量复制、复制全部
- **新扫描**：扫完后一键重置 UI 开始下一轮
- **调试日志**：开启调试模式后，每条探测的请求/响应都会写入日志
- **profile 编辑快捷入口**：从 profile 编辑页直接点 "Find Resolvers" 按钮进扫描器

## 开发

```bash
make test        # 带 race detector 跑测试
make build       # 构建两个二进制
make build-all   # 跨平台交叉编译(含 Android)
make upx         # 用 UPX 压缩 Linux/Windows/Android 二进制
make vet         # go vet
make fmt         # 格式化代码
make clean       # 清理构建产物
```

## iOS 开发

把 Go 客户端通过 gomobile 打包成 xcframework，由 `ios/` 下的 SwiftUI 应用消费。服务进程跑在 `127.0.0.1:<random-port>`；仅前台(iOS 不允许长时间后台服务)。

macOS 上的依赖：Xcode 15+、Go 1.26+、gomobile。

```
go install golang.org/x/mobile/cmd/gomobile@latest
gomobile init
```

常用目标：

```
make ios-bind            # 构建 Mobile.xcframework(iOS 真机 + 模拟器)
make ios-bind-catalyst   # 同时包含 Mac Catalyst 切片
make ios-build           # 为模拟器构建 app
make ios-test            # 在模拟器跑单元测试
make ios-list-sims       # 列出可用的模拟器
```

可通过 `IOS_SIM_NAME='iPhone 16'` 覆盖默认的模拟器。

执行 `make ios-bind` 后，在 Xcode 里打开 `ios/Thefeed.xcodeproj` 即可运行。

## 发布流程(GitHub Actions)

推送以 `v` 开头的 tag 会触发 CI 构建并发布 GitHub Release。

- 稳定版 tag 示例：`v1.4.0`
- 预发布 tag 示例：`v1.4.0-rc1`、`v1.4.0-beta.2`

规则：
- 如果 tag 包含 `-`，自动标记为**预发布**。

发行版包含：
- 当前所有目标平台的服务端/客户端二进制
- 原生 Android APK(64 位，推荐):`thefeed-android-<version>-arm64-v8a.apk`
- 原生 Android APK(32 位，老设备):`thefeed-android-<version>-armeabi-v7a.apk`

## DNS 记录配置

你的域名上需要配置 **两条 DNS 记录**。假设服务器 IP 是 `203.0.113.10`，想用 `example.com`:

### 1. NS 服务器的 A 记录

| 类型 | 名称 | 值 |
|------|------|-------|
| A | `ns.example.com` | `203.0.113.10` |

把一个主机名指向服务器 IP。

### 2. 隧道子域的 NS 记录

| 类型 | 名称 | 值 |
|------|------|-------|
| NS | `t.example.com` | `ns.example.com` |

把 `t.example.com`(及其子域) 的 DNS 查询全部委派给你的服务器。


## channels.txt 格式

```
# 以 # 开头的是注释
@VahidOnline
```

## x_accounts.txt 格式

```
# 以 # 开头的是注释
Vahid
```

## X 抓取的安全考量

- X 抓取只用 RSS/XML。
- 实例 URL 会校验(必须是 `http`/`https`、仅主机名、不带 path/query/fragment)。
- 响应体大小有上限，并强制请求超时。
- 当某个镜像返回 `403` 或失败时，服务端会自动尝试下一个配置的实例。
- 建议：用 `--x-rss-instances`(或 `THEFEED_X_RSS_INSTANCES`) 设置你自己信任的镜像列表。

## 安全说明

### 双层访问控制

**加密口令(`--key`)**：服务端和客户端都需要。拿到这个口令的人可以读所有频道的消息(含私有频道)。你可以分享给信任的朋友让他们一起读。

**远程管理(服务端的 `--allow-manage`)**：开启后，拿到加密口令的人还能发消息和管理频道。默认关闭，只在信任的服务器上开启。

**客户端 Web 密码(`--password`)**：用 HTTP Basic Auth 保护所有 Web UI 端点。这只是**本地保护**，不影响 DNS 层的访问。

### 安全特性

- 全链路端到端加密(AES-256)
- 客户端与服务端都需要预共享口令
- 每个查询独立 —— 链路上不保留会话状态
- 双向随机填充，防止流量分析
- 写操作由服务端 `--allow-manage` 控制
- Telegram 二步验证密码通过交互式输入(永远不写入命令行参数)
- 会话文件的权限受限(0600)

> **⚠️ 警告：** 如果你把口令公开分享，**任何人**都可以用这个口令跑自己的客户端读你的所有消息，无法阻止。
> 客户端的 `--password` 只保护**你自己机器上**的 Web UI —— 它不能阻止别人用你的口令。**绝对不要公开分享口令。**

## 服务管理

```bash
# install.sh 执行完后
systemctl status thefeed-server
systemctl restart thefeed-server
journalctl -u thefeed-server -f

# 更新频道列表
sudo vi /opt/thefeed/data/channels.txt
sudo systemctl restart thefeed-server

# 更新二进制
sudo bash scripts/install.sh
```

## 许可证

MIT

---

<div align="center">

**For FREE IRAN** <img src="internal/web/static/lion-sun.svg" alt="Lion-and-Sun" height="20">

*Everyone deserves free access to information*

</div>
