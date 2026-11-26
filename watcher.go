package main

import "time"

// ShellID 标识当前 Windows shell 窗口及其进程，用于判断 explorer 是否重启。
type ShellID struct {
	HWND uintptr
	PID  uint32
}

// valid 报告 shell 标识是否完整；启动早期可能只拿到窗口或进程号。
func (s ShellID) valid() bool {
	return s.HWND != 0 && s.PID != 0
}

const (
	settleDelay      = 2 * time.Second
	maxPulseAttempts = 3
	retryBaseDelay   = 5 * time.Second
	retryMaxDelay    = 30 * time.Second
)

// shellTracker 记录已经处理过的 shell，避免同一个 shell 重复脉冲。
type shellTracker struct {
	completed ShellID
}

// plan 决定是否为 shell 安排脉冲；manual 为 true 时跳过已完成检查。
func (t *shellTracker) plan(shell ShellID, manual bool) (time.Duration, bool) {
	if !shell.valid() {
		return 0, false
	}
	if manual {
		return 0, true
	}
	if shell == t.completed {
		return 0, false
	}
	return settleDelay, true
}

// markPulsed 记录 shell 的脉冲已经完成。
func (t *shellTracker) markPulsed(shell ShellID) {
	t.completed = shell
}

// sameProcess 报告两个 shell 标识是否指向同一个进程；PID 为零表示无效。
func sameProcess(a, b ShellID) bool {
	return a.PID != 0 && a.PID == b.PID
}

// retryPolicy 计算失败重试的退避时间，并限制总次数。
type retryPolicy struct {
	attempts int
}

// reset 在成功或跳过之后清空重试计数。
func (p *retryPolicy) reset() {
	p.attempts = 0
}

// next 返回下一次重试的等待时间；超过上限时返回 false。
func (p *retryPolicy) next() (time.Duration, bool) {
	if p.attempts >= maxPulseAttempts {
		return 0, false
	}
	delay := retryBaseDelay << p.attempts
	if delay > retryMaxDelay {
		delay = retryMaxDelay
	}
	p.attempts++
	return delay, true
}
