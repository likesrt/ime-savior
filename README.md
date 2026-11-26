# ImeSavior

Windows 11 上修复第三方输入法「首击键被判成英文」的托盘工具。开机登录后、explorer 重启后，或手动点「立即修复」时，它会打开再关闭一次触摸键盘，让输入法重新接管第一个按键。

## 问题

- 中文输入法下，复制粘贴、切换输入框或长时间打字后，第一个键经常被系统当成英文；有时连续几个键都这样。五笔等形码输入法没有字母概念，也一样中招。
- 微软拼音、搜狗、小狼毫等都能复现，换输入法无效；从 Windows 11 23H2 到 Canary Build 27729 都存在，微软没有正式补丁。
- 已知规避是 explorer 启动后开合一次触摸键盘，但每次登录和 explorer 重启后都要重做。

## 功能

- 托盘常驻，没有主窗口；菜单只有「立即修复」「开机自启」「退出」。
- 开机登录后自动修复一次。
- explorer 重启后自动修复。
- 复制粘贴后首击键又变英文时，点「立即修复」。
- 普通用户权限，不提权；不修改输入法注册表；不联网。

## 下载

从 [Releases](https://github.com/likesrt/ime-savior/releases/latest) 下载最新的 `ImeSavior.exe`。

## 使用

1. 把 `ImeSavior.exe` 放到固定目录，不要从临时目录或压缩包里直接运行。
2. 运行后右键通知区域图标；需要开机自动修复就勾选「开机自启」。
3. 复制粘贴后首击键又变成英文时，点「立即修复」。

日志在 `%LOCALAPPDATA%\ImeSavior\ImeSavior.log`，128 KB 轮转，保留 1 份备份。

极少数情况下，兜底启动的 TabTip 可能来不及关闭；手动关闭触摸键盘即可。

## 构建

```sh
mkdir -p dist
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-H windowsgui -s -w" -o dist/ImeSavior.exe .
```

产物是 `dist/ImeSavior.exe`，Windows 11 x64，GUI 子系统，无控制台窗口。`dist/` 不会进入 git，发布时作为 GitHub Release 附件上传。

## 验证

Linux 上已通过：

- `go test ./...`
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./...`
- 上述交叉编译

Windows 真机上的托盘、COM 开合、explorer 重启、开机自启和复制粘贴后的修复效果尚未验证，需要在 Windows 11 上实际跑一遍。

## 实现

- MyGo 原生托盘，不启动 WebView。
- 触摸键盘通过 COM 的 `ITipInvocation` / `IFrameworkInputPane` 开合；调用放在锁定线程，对象和 COM 环境成对释放。
- explorer 监听用进程句柄和停止事件，正常空闲不轮询；拿不到句柄才退回 5 秒检查。
- 单实例用 MyGo 的 `RequestSingleInstanceLock`，开机自启用当前用户 Run 项。
- 可见性判断依赖 `IFrameworkInputPane.Location`；不同 Windows 版本行为可能不同。

## 许可证

Copyright (C) 2026 likesrt

本项目与原项目相同，采用 GNU General Public License v3.0，完整文本见 [LICENSE](LICENSE)。

本项目是 [cs0tony/ime-savior](https://github.com/cs0tony/ime-savior) 的 Go/MyGo 重写；原项目同样采用 GPL-3.0。

## 参考

- 原项目：https://github.com/cs0tony/ime-savior
- 原 PR：https://github.com/cs0tony/ime-savior/pull/1
- 微软问答：https://learn.microsoft.com/zh-cn/answers/questions/3971899/windows11-24h2
- 小狼毫议题：https://github.com/rime/weasel/issues/1519
- 联想知识库：https://iknow.lenovo.com.cn/detail/427726
