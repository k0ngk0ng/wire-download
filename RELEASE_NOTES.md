# wirectl download 0.3.2

修复升级客户端后、旧 daemon 仍在运行时，普通下载返回 `daemon HTTP 404` 的兼容性回归。

- HTTP(S)、torrent、magnet、ed2k 恢复使用兼容旧 daemon 的任务接口，原命令无需改变。
- 只有 X/Twitter、YouTube 视频使用批量视频接口；旧 daemon 不支持时明确提示升级并执行 `wirectl download daemon restart`，不会把网页 HTML 当视频下载。
- 新增 `daemon restart`：等待状态保存、旧进程清理并释放锁后再启动；未运行时启动，停止失败时保留错误并中止启动。
- daemon API 错误保留接口路径和 HTTP 状态，服务端具体错误继续原样显示。
- 更新 Bash/Zsh/Fish 补全和使用说明。

新增旧/新 daemon 接口兼容、视频多任务与部分失败、禁止 HTML 回退、错误保留及不重复提交的回归测试。
安装验收直接执行 restart，检查进程更换、暂停/完成/删除状态、HTTP 登录和续传保持正确。

提供 macOS arm64、Linux arm64 和 Linux amd64 完整安装包，继续内置 aria2、aMule、yt-dlp、FFmpeg 和 Deno。
