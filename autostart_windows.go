//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/egoist/mygo"
	"golang.org/x/sys/windows"
)

// autostartEnabled 报告登录计划任务是否已注册。
func autostartEnabled() bool {
	_, err := runSchtasks("/Query", "/TN", autostartTaskName)
	return err == nil
}

// setAutostart 创建或删除登录任务，并清理旧版 Run 项。
//
// 创建按当前进程权限选择运行级别：已提权用 HighestAvailable，否则用
// LeastPrivilege。删除时任务已不存在视为成功；Run 项清理失败不影响结果。
func setAutostart(enable bool) error {
	if enable {
		if err := createAutostartTask(); err != nil {
			return err
		}
	} else if err := deleteAutostartTask(); err != nil {
		return err
	}
	// 旧版本用 Run 项；留着会重复启动，而且提权时无法在登录时启动。
	_ = mygo.App.SetOpenAtLogin(false)
	return nil
}

// createAutostartTask 生成任务 XML 并注册；任务指向当前 exe 的绝对路径。
func createAutostartTask() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	user, err := currentUserID()
	if err != nil {
		return err
	}
	runLevel := runLevelLeast
	if isElevated() {
		runLevel = runLevelHighest
	}
	path, err := writeTaskFile(buildTaskXML(user, exe, runLevel))
	if err != nil {
		return err
	}
	defer os.Remove(path)
	_, err = runSchtasks("/Create", "/TN", autostartTaskName, "/XML", path, "/F")
	return err
}

// deleteAutostartTask 删除登录任务；任务已经不存在时视成功。
func deleteAutostartTask() error {
	if _, err := runSchtasks("/Delete", "/TN", autostartTaskName, "/F"); err != nil {
		if !autostartEnabled() {
			return nil
		}
		return err
	}
	return nil
}

// writeTaskFile 把任务定义以 UTF-16LE 写入临时文件，返回路径供 schtasks 读取。
func writeTaskFile(xmlText string) (string, error) {
	file, err := os.CreateTemp("", "imesavior-task-*.xml")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := file.Write(encodeUTF16LE(xmlText)); err != nil {
		file.Close()
		os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// currentUserID 返回当前进程用户的 SID，供登录触发器和任务主体使用。
func currentUserID() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

// isElevated 报告当前进程是否已提权，决定任务用最高权限还是普通权限。
func isElevated() bool {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

// runSchtasks 调用系统目录下的 schtasks.exe，避免 PATH 劫持和额外窗口。
func runSchtasks(args ...string) ([]byte, error) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		return nil, errors.New("未找到 SystemRoot 环境变量")
	}
	exe := filepath.Join(root, "System32", "schtasks.exe")
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	if err != nil {
		action := ""
		if len(args) > 0 {
			action = args[0]
		}
		return out, fmt.Errorf("schtasks %s 失败: %v: %s", action, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
