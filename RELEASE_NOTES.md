# wirectl download 0.3.0

新增 Twitter/X 和 YouTube 视频下载，直接把网站链接传给 `wirectl download`。

- X 一条推文的每个视频各建一个任务，支持 Twitter/X 域名去重和独立暂停、恢复、删除。
- YouTube 支持单视频、Shorts 和短链接；不自动展开播放列表或频道。
- 默认下载最佳可用画质，分离的音视频轨道无损合并为 MKV；单文件视频保留原容器。
- 视频任务在 daemon 中运行，关闭终端不影响下载；暂停保留分片，恢复和重启时重新解析地址。
- 错误任务可以用 `resume` 重试。网站登录复用已有 `login` 会话，临时 Cookie 文件不会写进任务数据库。
- 三个平台的发行包新增 yt-dlp 2026.08.19、FFmpeg/ffprobe 9.0.2、Deno 2.9.7，无需另外安装 Python 或视频工具。

提供 macOS arm64（macOS 13+）、Linux arm64 和 Linux amd64（glibc 2.36+）安装包。
升级前停止 daemon，升级后重新启动；原下载状态和文件保留。
网站可用性、登录、地区限制和限流可能影响下载；不支持直播、尚未开始的视频或自动展开播放列表。

发布验证包含任务拆分、去重、暂停恢复、Cookie 隔离，以及实际 yt-dlp HTTP 下载和 ffmpeg 音视频合并；
保留 HTTP 登录、BT/eMule 真实协议、重启恢复和终端操作的现有验收。
