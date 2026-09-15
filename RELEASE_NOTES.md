# wirectl download 0.2.3

修复 aMule 中文文件名和百分号编码链接导致下载任务误报 `unknown` 的问题，并加入 Homebrew 安装说明。

- 使用 UTF-8 环境读取 aMule 队列，保留包含中文文件名的状态行。
- 正确提取编码文件链接的 eD2k hash；旧任务在引擎报告匹配 hash 时自动恢复，无需重复添加。
- Homebrew 安装：`brew install k0ngk0ng/tap/wire-download`，自动安装共享的 `wirectl` 和配套下载引擎。
- 支持 Homebrew 标准目录中的独立 Bash/Zsh/Fish 补全，以及 `brew services` 后台运行。

提供 macOS arm64、Linux arm64 和 Linux amd64 安装包；当前不提供 macOS Intel（amd64）安装包。每个包包含 wirectl、下载插件及 aria2/aMule 引擎，无需额外安装下载或搜索依赖。macOS 要求 13+，Linux 要求 glibc 2.36+。可选浏览器登录需要桌面端已有 Chromium 浏览器；远端传入会话需要 SSH 客户端。

发布流程以三个平台的原生构建、单元测试、实际终端操作、HTTP 登录下载、BT 搜索选中后的真实 peer 传输与重启恢复、电驴搜索及真实 peer 传输作为门槛。公共网站可用性会变化，网站连接失败会在搜索界面显示。

`wire-download` 公开仓库的 GitHub Release 提供安装包、SHA256SUMS 和对应源码归档，任何人均可下载；`wirectl` 源仓库保持私有，但发行包已包含运行所需的 CLI。源码归档无需下载安装。升级前停止 daemon；安装脚本拒绝覆盖现有引擎目录，建议安装至新目录后切换。完整命令和限制见 README.md。
