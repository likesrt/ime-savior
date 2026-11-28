package main

import (
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"strings"
	"unicode/utf16"
)

const (
	autostartTaskName = "ImeSavior"
	autostartDelay    = "PT5S"

	runLevelHighest = "HighestAvailable"
	runLevelLeast   = "LeastPrivilege"
)

// autostartTaskXML 是登录任务的 Task Scheduler 1.2 定义。
//
// 四个 %s 依次是：登录触发器用户、任务主体用户、运行级别、exe 绝对路径；
// 调用方必须先用 escapeXML 转义。HighestAvailable 让管理员账户以提权令牌
// 启动且不弹 UAC，InteractiveToken 保证任务运行在用户会话里，托盘图标才
// 可见；PT0S 关闭默认的三天运行上限。
const autostartTaskXML = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Author>ImeSavior</Author>
    <Description>登录后打开并关闭一次触摸键盘，修复输入法首击键异常</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <StartBoundary>2020-01-01T00:00:00</StartBoundary>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
      <Delay>` + autostartDelay + `</Delay>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>%s</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
    </Exec>
  </Actions>
</Task>
`

// buildTaskXML 生成登录任务定义；userID 为 SID 或 DOMAIN\User，exe 为绝对路径。
//
// 含空格的 exe 路径会加双引号，这是 Task Scheduler 对 Program/script 的要求。
func buildTaskXML(userID, exe, runLevel string) string {
	user := escapeXML(userID)
	command := escapeXML(exe)
	if strings.ContainsRune(exe, ' ') {
		command = `"` + command + `"`
	}
	return fmt.Sprintf(autostartTaskXML, user, user, escapeXML(runLevel), command)
}

// escapeXML 转义 XML 文本节点里的特殊字符，避免路径破坏任务定义。
func escapeXML(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// encodeUTF16LE 生成带 BOM 的 UTF-16LE 字节；schtasks /XML 要求 Unicode 文件。
func encodeUTF16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2+len(units)*2)
	out[0], out[1] = 0xFF, 0xFE
	for i, u := range units {
		binary.LittleEndian.PutUint16(out[2+i*2:], u)
	}
	return out
}
