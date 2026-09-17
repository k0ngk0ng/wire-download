# wire-download

`wire-download` 是项目和发行包名称；用户命令是 `wirectl download`，对应的插件
可直接运行 `wirectl-download`。它支持 HTTP/HTTPS、ed2k、BitTorrent 和 magnet。
CLI 通过私有 Unix socket 控制独立 daemon；关闭终端或退出仪表盘不会中断下载。

CLI 入口与库位于 [`wirectl`](https://github.com/k0ngk0ng/wirectl) 仓库，该仓库需要相应访问权限。这里提供
`wirectl-download` 插件，其他仓库可独立提供 `wirectl-xxx`，不需要修改下载器。

## 安装与使用

### Homebrew

Apple Silicon macOS 和 Linux amd64/arm64 可以直接安装：

```sh
brew install k0ngk0ng/tap/wire-download
wirectl download init
wirectl download daemon start
```

Formula 自动安装 `wirectl` 主程序，并附带 aria2/aMule 引擎；可以与
`wire-connect` 一起安装。macOS 要求 13+，Linux 要求 glibc 2.36+。
首次安装后运行 `init`；已有配置时跳过，继续使用原状态目录。

升级前停止 daemon，升级后重新启动：

```sh
wirectl download daemon stop
brew update
brew upgrade k0ngk0ng/tap/wirectl k0ngk0ng/tap/wire-download
wirectl download daemon start
```

也可以交给 Homebrew 管理后台服务：首次 `init` 后用
`brew services start k0ngk0ng/tap/wire-download` 启动。使用这种方式时，升级前后分别用
`brew services stop k0ngk0ng/tap/wire-download` 和
`brew services start k0ngk0ng/tap/wire-download`，不要再同时运行 `daemon start`。

如果原来手动安装在 `~/.local/bin`，先用 `type -a wirectl wirectl-download` 检查路径，
把 Homebrew 的 `bin` 放在 `PATH` 前面；原下载文件和状态目录继续保留。
可用 `"$(brew --prefix)/bin/wirectl" download version` 明确检查 Homebrew 安装的版本。

Bash、Zsh 和 Fish 的 `wirectl-download` 补全安装到 Homebrew 的标准目录，
不会覆盖 `wire-connect` 的补全文件。启用 Homebrew 的 shell 补全后即可使用。
`wirectl download` 的手动加载方式见下方「Shell 补全」。

[Homebrew Tap](https://github.com/k0ngk0ng/homebrew-tap) 每小时检查官方稳定版 Release，
核对 GitHub asset digest 和独立 `SHA256SUMS`，通过三个平台的安装测试后更新 Formula。

### 手动安装发行包

每个发行归档都是一个自包含的 macOS 或 Linux 目标包，包含主入口、下载插件、aria2
和无 GUI 的 aMule 引擎；不需要另装引擎。安装脚本不访问网络，也不调用系统包管理器。
发布工作流分别构建 `darwin-arm64`、`linux-arm64` 和 `linux-amd64`，选择与你的系统和
CPU 匹配的归档即可。当前不提供 macOS Intel（amd64）归档。它们是按目标分别打包的
原生归档，不是一个跨架构文件。macOS 原生引擎的最低构建目标为 macOS 13；Linux
引擎构建和验收环境是 Debian 12 / glibc 2.36，其他发行版需要自行验证。

```sh
tar -xzf wire-download-<version>-<os>-<arch>.tar.gz
cd wire-download-<version>-<os>-<arch>
sh install.sh "$HOME/.local"
export PATH="$HOME/.local/bin:$PATH"

wirectl download init
wirectl download daemon start
wirectl download 'https://example.com/archive.zip'
wirectl download 'http://example.com/archive.zip'
wirectl download 'magnet:?xt=urn:btih:...'
wirectl download ./example.torrent
wirectl download 'ed2k://|file|example.bin|12345|0123456789abcdef0123456789abcdef|/'
wirectl download watch
```

发行包同时安装 `wirectl` 和 `wirectl-download`。通常使用 `wirectl download`；如果
只想直接调用插件，也可以使用同样的参数运行 `wirectl-download`。

链接必须加引号，避免 shell 解释 `&`、`|` 等字符。示例 hash 是占位符。
也支持 HTTP(S) 文件及 torrent URL。
交互终端提交任务后自动进入实时仪表盘，每秒刷新进度条、百分比、下载速度，
并显示选中任务的已下载大小与总大小。按 `q` 返回终端，下载继续在后台进行。
只提交任务用 `wirectl download add --detach '<链接>'`；重定向输出时也只提交任务。

```sh
wirectl download list                  # 状态表
wirectl download list --json           # 机器可读状态、引擎健康信息
wirectl download pause <id>
wirectl download resume <id>
wirectl download remove <id>
wirectl download daemon status
wirectl download daemon stop
wirectl download doctor
```

仪表盘：方向键 / `j`、`k` 选择任务，`p` 暂停，`r` 恢复，`d` 删除，`q` 退出。
删除需要 `y` 确认。**删除 ed2k 未完成任务会删除 aMule 的临时分片**；已完成文件保留。
CLI 的 `remove` 是直接执行，适合脚本调用。

## Shell 补全

生成补全脚本不需要初始化配置或启动 daemon。脚本同时支持 `wirectl download` 和
`wirectl-download`，补全子命令、选项及固定选项值。

在当前 shell 中加载：

```bash
# Bash
source <(wirectl download completion bash)
```

```zsh
# Zsh（先初始化补全系统）
autoload -Uz compinit && compinit
source <(wirectl download completion zsh)
```

```fish
# Fish
wirectl download completion fish | source
```

持久启用时，把对应加载命令放入 `~/.bashrc`、`~/.zshrc` 或
`~/.config/fish/config.fish`；Zsh 已初始化 `compinit` 时无需重复添加初始化命令。

## 搜索与选择下载

先启动 daemon，再搜索关键词：

```sh
wirectl download search ubuntu
wirectl download search --type ed2k --ed2k-mode global '关键词'
wirectl download search --type magnet ubuntu
wirectl download search --type torrent --source nyaa sintel
wirectl download search --type bt --json ubuntu
```

交互终端显示每个来源的进度、结果数和错误；方向键 / `j`、`k` 选择结果，
Enter / `d` 下载，`q` 退出并取消尚未完成的搜索。已加入的下载任务继续运行。
搜索本身不会自动下载。非交互模式将进度写入 stderr，最终结果写入 stdout；
`--json` 适合脚本。所有选项放在关键词之前。

BT 与磁力使用相同的网站索引；`--type bt` 同时接受种子文件和磁力，
`--type magnet` 只返回磁力，`--type torrent` 只返回有种子下载地址的结果。
按 info hash 合并重复结果，保留来源及可用的种子地址；电驴按文件哈希和大小去重。
种子数和来源数来自索引或电驴网络，未提供时显示为未知/0，并不保证可下载。

默认查询 Nyaa、Anime Tosho、动漫花园、BTDig 和 LinuxTracker；来源侧重不同内容，
可按需启停。Internet Archive API 为可选来源，默认关闭。
网站可能临时不可达、限制请求或改版；失败会按来源显示，其他成功结果仍可使用。
不自动绕过网站验证码。支持添加 RSS 和 Torznab 索引，搜索无需额外运行索引服务；
若使用自己的 Torznab 服务，请提供已有服务地址。

```sh
wirectl download search sources list
wirectl download search sources disable nyaa
wirectl download search sources enable nyaa
wirectl download search sources add --id myrss --name 'My RSS' --type rss --url 'https://example.org/rss?q={query}'
wirectl download search sources add --id myindex --name 'My Index' --type torznab --url 'https://example.org/api' --api-key-env MY_INDEX_KEY
wirectl download search sources remove myrss
```

来源配置存于状态目录 `search-sources.json`，权限 0600。API key 从指定环境变量读取，
不需要写入命令参数；来源列表不返回 key。通过 CLI 修改立即影响新搜索。
`--source` 接受逗号分隔的来源 ID，`--limit` 为 1–200，`--timeout` 为 5s–180s。

电驴支持 `--ed2k-mode server|global|kad|all`，分别查询当前服务器、服务器列表、
Kad，或依次查询 global 和 Kad；默认 all。全局查询在设定的时间窗口内持续收集，
进度按该时间窗口显示，不把 aMule 瞬时进度值当成全部服务器已完成的保证。同一个 aMule 引擎一次只进行一个搜索，
后续搜索等待前一个完成或取消。需要对应网络已连接；Kad 刚启动可能需要引导时间。
BT 的 DHT 用于通过已知哈希寻找 peer，关键词检索来自网站索引。

结果保存在 daemon 内存中，最多 16 个搜索会话，后续搜索会淘汰旧的已完成会话；
重启 daemon 后不保留。每个搜索最多保留 1000 个去重结果，超过显示上限会标记截断。
可使用结果中的稳定 ID 再次查看或提交下载：

```sh
wirectl download search results --json <search-id>
wirectl download search download <search-id> <result-id>
wirectl download search cancel <search-id>
```

## 网站登录（可选）

公开 HTTP(S) 地址不需要登录。需要网站 cookie 的下载可以先为该网站保存一个会话：

```sh
wirectl download login https://files.example.com
# 在打开的浏览器中完成登录，然后回到终端按 Enter
wirectl download 'https://files.example.com/private/archive.zip'
wirectl download logout https://files.example.com
```

登录只接受 HTTP(S) 网站地址，不能在地址中嵌入用户名或密码。程序会启动独立的
Chromium 配置目录，只收集目标网站域名的 cookie；会话文件保存在状态目录的
`auth/` 下并使用 0600 权限。下载由 daemon 的本地认证代理完成，浏览器 cookie 不会
写入 aria2 的 session 文件。`logout` 删除下载器保存的会话，不会注销网站或清除独立浏览器配置中的登录。当前自动登录支持 Cookie 会话，不提取 localStorage/Bearer token，也不自动处理 HTTP Basic 登录。

浏览器登录是可选功能，但执行这条命令需要已安装的 Chrome、Chromium、Edge 等
Chromium 系浏览器和图形桌面。自动发现失败时，用 `--browser` 指定可执行文件：

```sh
wirectl download login https://files.example.com --browser /path/to/chromium
```

可以在有图形桌面的机器上登录，再通过 SSH 把会话交给远端 daemon：

```sh
wirectl download login https://files.example.com --remote user@download-host
```

这会在本地打开浏览器，确认后通过 `ssh` 的标准输入执行远端
`wirectl download login --import`。远端需要已安装 `wirectl`、`wirectl-download` 并
能在默认状态目录中运行；远端不需要安装浏览器。不要把会话 JSON 写入 shell 参数或
日志。

## 配置和默认网络

默认状态目录是 `$XDG_STATE_HOME/wirectl/download`，未设置时使用
`~/.local/state/wirectl/download`。可设置 `WIRECTL_DOWNLOAD_HOME`，或把
`--data-dir /absolute/path` 放在下载子命令之前：

```sh
wirectl download --data-dir /srv/wire-download init --downloads /srv/downloads
wirectl download --data-dir /srv/wire-download daemon run
```

`init` 创建权限为 0600 的 `config.json`，不会覆盖已有配置。
修改配置后重启 daemon。不要手动修改运行中的引擎配置。

`wirectl download doctor` 显示实际状态目录（`State`）和下载目录。查看配置时可用
以下命令隐藏连接密钥（需要 `jq`）；指定了 `--data-dir` 时改用对应目录：

```sh
jq 'del(.secret)' "${WIRECTL_DOWNLOAD_HOME:-${XDG_STATE_HOME:-$HOME/.local/state}/wirectl/download}/config.json"
```

daemon 运行时，可以查看或更新 eMule 服务器列表：

```sh
wirectl download emule servers list
wirectl download emule servers update
```

| 配置 | 默认值 / 含义 |
| --- | --- |
| `downloads` | `~/Downloads` |
| `max_downloads` | aria2 同时下载 5 个任务 |
| `download_limit` | `0`，不限速 |
| `upload_limit` | `1M`，每个引擎各自的总上传上限 |
| `seed_ratio` | BT 做种分享率 1.0；`0` 表示不按分享率停止 |
| `bt_port` | 16881 TCP/UDP |
| `ed2k_port` | 14662 TCP；aMule 另使用 TCP 端口 + 3 的 UDP 服务端口 |
| `kad_port` | 14672 UDP |
| `aria2_port` | 16800，仅回环 RPC |
| `amule_ec_port` | 14712，仅回环 EC |

BT 默认开启 DHT、IPv6 DHT、PEX、局域网发现，并补充 opentrackr、stealth.si、
torrent.eu.org、tamersunion 的公共 tracker。DHT 默认引导地址为
`dht.transmissionbt.com:6881`。私有 torrent 是否允许 DHT/PEX 由 aria2 按 torrent 标记处理。
支持 v1 和含 v1 info hash 的 hybrid torrent；aria2 不支持纯 BitTorrent v2 / btmh magnet。

eMule 默认启用 ED2K 和 Kad。初次启动通过 HTTPS 获取 eMule Security / gruk / Shortypower 的
`server.met`，并发获取、验证结构后按 IP/端口去重合并，再原子写入。离线时使用 2026-09-12 获取的内置服务器列表，
并提供同来源的 Kad `nodes.dat` 初始节点。已有引擎节点状态不会被覆盖。
公共服务器、tracker 和节点可用性会随时间变化，配置中可替换地址。

```sh
wirectl download daemon stop
wirectl download emule servers update
wirectl download daemon start
```

要获得 eMule High ID 和更好的入站连接，请在防火墙/路由器允许上述 P2P 端口。
控制端口无需映射到公网。默认不开启 UPnP。

## 后台运行与恢复

`daemon start` 脱离终端运行；`daemon run` 保持前台，供 systemd/launchd 管理。
daemon 使用文件锁防止同一个状态目录运行多个实例。引擎退出时 daemon 报错退出，
系统服务配置会自动重启。`daemon start` 自身不承担系统服务的开机启动职责。

Linux 用户服务：把 `deploy/wirectl-download.service` 放到
`~/.config/systemd/user/`，然后执行：

```sh
systemctl --user daemon-reload
systemctl --user enable --now wirectl-download
```

模板默认安装位置为 `~/.local/bin`。需要退出登录后继续运行时，由系统管理员启用
该用户的 linger。macOS 使用 `deploy/io.github.k0ngk0ng.wirectl.download.plist`，
先把可执行文件占位符替换成绝对路径，再放到 `~/Library/LaunchAgents/`，执行
`launchctl bootstrap gui/$(id -u) <plist 的绝对路径>`。

任务数据库为 `jobs.json`，每次提交/操作原子写入并同步磁盘。aria2 session 和 DHT 状态、
aMule identity、Kad 状态及未完成分片分别保存在状态目录的 `aria2/` 和 `amule/`。
恢复时不会盲目重新提交所有任务。引擎暂时失联保留最后观测状态；找不到的任务标记
`unknown`，不会被当成下载完成。aMule 完成状态通过已共享的完整文件 hash 确认。
aMule 文本接口仅提供百分比，其已下载字节数根据链接大小估算。

正常停止会保存引擎状态。进程被强制杀死或机器断电时，恢复范围取决于引擎最近的
落盘记录；不要同时移动/删除临时目录。升级前停止服务并备份配置和状态。
安装脚本拒绝直接覆盖已有引擎 bundle，支持先安装到新目录再切换服务路径。

日志在状态目录 `logs/` 以及 `amule/logfile`。启动失败时错误会给出日志路径。
`doctor` 检查配置和引擎路径，`list --json` 显示控制连接健康状态；连接正常不代表
公共网络上一定存在目标文件的可用源。

## 开发与验证

需要 Go 1.23+。跨仓库开发时，可把有权限访问的 `wirectl` 库放在当前仓库的同级目录；下载项目已 vendor 依赖，公开仓库的测试和发布构建无需访问该私有源仓库。
CLI 库模块版本为 `github.com/k0ngk0ng/wirectl v0.1.0`，本地 replace 仅用于跨仓库开发。

```sh
make check       # race tests + vet；有同级 wirectl 源码时一并检查
make build       # bin/wirectl 和 bin/wirectl-download
```

Go 构建缓存和测试临时目录都位于仓库 `.cache/`。
真实协议测试使用本机生成的数据、本机 tracker 和两个 aria2 peer，逐字节核对内容：

```sh
WIRECTL_TEST_ARIA2=/absolute/path/to/aria2c make test
```

不配置真实引擎时，相关测试会明确 skip，不能据此宣称协议已完成实测。
发行包必须包含所有引擎，不能把仅 Go 二进制的开发构建当作自包含发行包。

在目标系统上执行 `VERSION=0.2.1 make release` 构建完整候选包。
构建机需要 Go、Python 3、C/C++ 工具链、make、curl、tar；Linux 还需要
OpenSSL/zlib 开发文件和 patchelf。引擎源代码下载均校验固定 SHA256。
`dist/` 生成安装归档、单独的对应源码归档和校验和；终端用户只需安装归档。
源码归档用于审阅、重建和履行第三方许可要求，不是安装依赖。

将候选包安装到测试目录后，运行以下验收（全部测试数据写入本仓库）：

```sh
python3 scripts/test-install.py /absolute/path/to/test-install-prefix
python3 scripts/test-tui.py /absolute/path/to/test-install-prefix
python3 scripts/test-bt-recovery.py /absolute/path/to/test-install-prefix
python3 scripts/test-bt-recovery.py /absolute/path/to/test-install-prefix --search --only http-torrent --only magnet
python3 scripts/test-ed2k.py /absolute/path/to/test-install-prefix --address <本机非回环IPv4> --search
```

这些验收在精简 PATH 下启动安装包中的引擎，验证 HTTP 登录下载和重启续传、
真实终端进度与键盘控制，以及本地 torrent、HTTP torrent 和 magnet 的暂停重启恢复。
BT 恢复保留用户看到的任务 ID；引擎内部的元数据子任务 ID 可以变化。
ED2K 实际传输使用 `scripts/test-ed2k.py`；Release 工作流在三个目标平台运行全部验收。

## 第三方组件

aria2（GPL-2.0-or-later）、aMule（GPL-2.0-or-later）、wxWidgets（wxWindows Library
Licence）、Crypto++ 和 Boost（Boost Software License）作为独立引擎和静态构建依赖。
Go CLI 使用 `golang.org/x/term`、`golang.org/x/sys`、`golang.org/x/net`（BSD-3-Clause）和 `gorilla/websocket`（BSD-2-Clause）。
打包时附带各组件许可证和对应源码/构建说明；不要省略 GPL/LGPL 对源码分发的要求。

## GitHub Release

[`k0ngk0ng/wire-download`](https://github.com/k0ngk0ng/wire-download) 是公开仓库，源码和
[GitHub Releases](https://github.com/k0ngk0ng/wire-download/releases) 可直接访问；同级
`wirectl` 源仓库保持私有。下载器推送 `v*` 标签后，GitHub Actions 原生构建
macOS arm64 和 Linux amd64、arm64 安装包，并运行真实协议及安装恢复测试。
三个平台全部成功后，公开发布安装包、SHA256SUMS 和对应源码归档。发行包已包含
`wirectl` CLI 和下载引擎，使用者无需访问 `wirectl` 源仓库。

Linux 发布构建需要启用 Debian `deb-src` 软件源，以下载随包运行库的精确对应源码；
工作流和 `scripts/linux/Dockerfile` 已配置。源码归档供审查和重建使用，不是安装依赖。

### BitTorrent tracker 管理

```sh
wirectl download bt trackers list
wirectl download bt trackers add udp://tracker.example.org:1337/announce
wirectl download bt trackers remove udp://tracker.example.org:1337/announce
```

修改后重启 daemon 生效。
