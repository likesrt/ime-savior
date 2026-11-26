//go:build windows

package main

import (
	"context"
	"errors"
	"log"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

const (
	shellPollInterval    = 200 * time.Millisecond
	fallbackPollInterval = 5 * time.Second
)

// pulseResult 是后台脉冲的结果，带回目标 shell 便于状态机判断。
type pulseResult struct {
	shell ShellID
	err   error
}

// Watcher 监听 shell 生命周期，并把触摸键盘脉冲串行化。
type Watcher struct {
	pulse  func(context.Context) error
	logger *log.Logger
	manual chan struct{}
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewWatcher 创建监听器；pulse 会被同一个后台协程串行调用。
func NewWatcher(pulse func(context.Context) error, logger *log.Logger) *Watcher {
	return &Watcher{
		pulse:  pulse,
		logger: logger,
		manual: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// TriggerManual 投递一次手动修复；已有待处理请求时合并，不阻塞菜单回调。
func (w *Watcher) TriggerManual() {
	select {
	case w.manual <- struct{}{}:
	default:
	}
}

// Wait 等待 Run 及其后台协程退出；超时返回 false。
func (w *Watcher) Wait(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.done:
		return true
	case <-timer.C:
		return false
	}
}

// Run 等待首个 shell 后进入监听循环；ctx 取消时先清理后台协程再关闭 done。
func (w *Watcher) Run(ctx context.Context) {
	defer close(w.done)
	workerCtx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		w.wg.Wait()
	}()

	requests := make(chan ShellID, 1)
	results := make(chan pulseResult, 1)
	w.startPulseWorker(workerCtx, requests, results)

	shell, ok := w.waitForShell(ctx)
	if !ok {
		return
	}
	loop := &watchLoop{
		watcher:     w,
		ctx:         ctx,
		requests:    requests,
		results:     results,
		shellEvents: make(chan ShellID, 1),
		exited:      make(chan ShellID, 1),
	}
	loop.attach(shell)
	loop.run()
}

// startPulseWorker 在固定 OS 线程上串行执行 pulse，把结果送回 results。
func (w *Watcher) startPulseWorker(ctx context.Context, requests <-chan ShellID, results chan<- pulseResult) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		// COM 接口有线程亲和性，让所有脉冲都落在同一线程上。
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		for {
			select {
			case <-ctx.Done():
				return
			case shell := <-requests:
				err := w.pulse(ctx)
				select {
				case results <- pulseResult{shell: shell, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
}

// watchLoop 串行处理 shell 变化、手动触发和脉冲结果；字段只在 Run 的协程内访问。
type watchLoop struct {
	watcher       *Watcher
	ctx           context.Context
	requests      chan<- ShellID
	results       <-chan pulseResult
	shellEvents   chan ShellID
	exited        chan ShellID
	tracker       shellTracker
	retry         retryPolicy
	target        ShellID
	pending       ShellID
	pendingManual bool
	watching      ShellID
	waiting       bool
	pulsing       bool
	timer         *time.Timer
	timerC        <-chan time.Time
}

// run 是监听状态机：只在收到事件时唤醒，空闲时不轮询。
func (l *watchLoop) run() {
	for {
		select {
		case <-l.ctx.Done():
			l.stopTimer()
			return
		case shell := <-l.shellEvents:
			l.attach(shell)
		case exitedShell := <-l.exited:
			l.handleExit(exitedShell)
		case <-l.watcher.manual:
			l.handleManual()
		case <-l.timerC:
			l.timerC = nil
			// 定时器和退出事件同时就绪时，先处理退出，避免脉冲已死的 shell。
			select {
			case exitedShell := <-l.exited:
				if sameProcess(exitedShell, l.watching) {
					l.handleExit(exitedShell)
					continue
				}
			default:
			}
			if !l.pulsing && l.target.valid() && sameProcess(l.target, l.watching) {
				l.requests <- l.target
				l.pulsing = true
			}
		case res := <-l.results:
			l.handleResult(res)
		}
	}
}

// attach 为当前 shell 建立退出监听并安排自动脉冲，返回实际采用的 shell。
func (l *watchLoop) attach(shell ShellID) ShellID {
	l.waiting = false
	// 事件可能在投递途中又发生了一次重启，以当前 shell 为准。
	if current, ok := currentShell(); ok {
		shell = current
	}
	if !shell.valid() {
		l.waitForNewShell()
		return ShellID{}
	}
	if !sameProcess(shell, l.watching) {
		l.watching = shell
		l.retry.reset() // 新进程重新获得完整重试预算
		l.watcher.logger.Printf("监听 shell 进程 pid=%d", shell.PID)
		l.watcher.watchShellExit(l.ctx, shell, l.exited)
	} else {
		// 同一进程只是窗口变了，更新标识即可，不重复等进程退出。
		l.watching = shell
	}
	delay, ok := l.tracker.plan(shell, false)
	if !ok {
		return shell
	}
	if l.pulsing {
		// 新 shell 优先；等桌面稳定后再脉冲，不沿用旧 shell 的立即语义。
		l.pending, l.pendingManual = shell, false
		return shell
	}
	l.schedule(shell, delay)
	return shell
}

// handleExit 在 shell 进程退出后撤销旧安排并等待新 shell；过期事件直接忽略。
func (l *watchLoop) handleExit(shell ShellID) {
	if l.pending == shell {
		l.pending, l.pendingManual = ShellID{}, false
	}
	if !sameProcess(shell, l.watching) {
		return
	}
	l.stopTimer()
	l.target = ShellID{}
	l.watching = ShellID{}
	l.waitForNewShell()
}

// waitForNewShell 最多启动一个等待协程，避免连续点击或重复事件累积轮询。
func (l *watchLoop) waitForNewShell() {
	if l.waiting {
		return
	}
	l.waiting = true
	l.watcher.watchAndSend(l.ctx, l.shellEvents)
}

// handleManual 处理托盘点击；没有 shell 时先等桌面就绪，再走正常脉冲流程。
func (l *watchLoop) handleManual() {
	shell, ok := currentShell()
	if !ok {
		l.waitForNewShell()
		return
	}
	if !sameProcess(shell, l.watching) {
		shell = l.attach(shell) // 手动触发时也可能刚换过 shell，先建立监听
		if !shell.valid() {
			return
		}
	}
	if l.pulsing {
		l.pending, l.pendingManual = shell, true
		return
	}
	l.retry.reset() // 用户主动触发，重新获得完整重试预算
	l.schedule(shell, 0)
}

// handleResult 更新跟踪状态；成功或键盘已开视为完成，失败按退避重试。
func (l *watchLoop) handleResult(res pulseResult) {
	l.pulsing = false
	switch {
	case res.err == nil:
		l.watcher.logger.Printf("触摸键盘脉冲完成 shell=%d", res.shell.PID)
		l.finishPulse(res.shell)
	case errors.Is(res.err, errKeyboardAlreadyOpen):
		l.watcher.logger.Printf("触摸键盘已打开，跳过 shell=%d", res.shell.PID)
		l.finishPulse(res.shell)
	default:
		l.watcher.logger.Printf("触摸键盘修复失败 shell=%d: %v", res.shell.PID, res.err)
		l.handleFailure(res.shell)
	}
}

// handleFailure 决定失败后重试当前 shell，还是转向排队中的新 shell。
func (l *watchLoop) handleFailure(shell ShellID) {
	if !sameProcess(shell, l.watching) {
		// 旧 shell 已退出，重试没有意义；只处理可能的新 shell。
		l.schedulePending()
		return
	}
	if l.pending.valid() && l.pending != shell {
		l.schedulePending() // 新 shell 优先，旧 shell 的重试作废
		return
	}
	if l.pendingManual {
		l.schedulePending() // 同一 shell 的手动请求立即重来，并重置预算
		return
	}
	// pending 为空或只是同一 shell 的重复事件；清掉重复项，按退避重试。
	l.pending, l.pendingManual = ShellID{}, false
	if delay, ok := l.retry.next(); ok {
		l.schedule(shell, delay)
	} else {
		l.watcher.logger.Printf("触摸键盘修复放弃 shell=%d", shell.PID)
	}
}

// finishPulse 记录成功或跳过，并处理排队中的下一个 shell。
func (l *watchLoop) finishPulse(shell ShellID) {
	l.tracker.markPulsed(shell)
	l.retry.reset()
	l.schedulePending()
}

// schedulePending 在脉冲结束后处理排队 shell；手动触发保持立即执行。
func (l *watchLoop) schedulePending() {
	if !l.pending.valid() {
		return
	}
	shell, manual := l.pending, l.pendingManual
	l.pending, l.pendingManual = ShellID{}, false
	if manual {
		l.retry.reset() // 排队期间是用户手动触发，按新请求计算重试
	}
	if delay, ok := l.tracker.plan(shell, manual); ok {
		l.schedule(shell, delay)
	}
}

// schedule 设定下一次脉冲的 shell 和延迟；已有计时器则重置。
func (l *watchLoop) schedule(shell ShellID, delay time.Duration) {
	l.target = shell
	if l.timer == nil {
		l.timer = time.NewTimer(delay)
	} else {
		if !l.timer.Stop() {
			select {
			case <-l.timer.C:
			default:
			}
		}
		l.timer.Reset(delay)
	}
	l.timerC = l.timer.C
}

// stopTimer 停止计时器，并让 select 不再监听它。
func (l *watchLoop) stopTimer() {
	if l.timer != nil {
		l.timer.Stop()
		l.timerC = nil
	}
}

// waitForShell 每 200 ms 读取一次 shell，直到拿到有效标识或 ctx 结束。
func (w *Watcher) waitForShell(ctx context.Context) (ShellID, bool) {
	ticker := time.NewTicker(shellPollInterval)
	defer ticker.Stop()
	for {
		if shell, ok := currentShell(); ok {
			return shell, true
		}
		select {
		case <-ctx.Done():
			return ShellID{}, false
		case <-ticker.C:
		}
	}
}

// currentShell 返回当前桌面 shell 窗口及其进程号；读取失败时 ok 为 false。
func currentShell() (ShellID, bool) {
	hwnd := windows.GetShellWindow()
	if hwnd == 0 {
		return ShellID{}, false
	}
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(hwnd, &pid); err != nil || pid == 0 {
		return ShellID{}, false
	}
	return ShellID{HWND: uintptr(hwnd), PID: pid}, true
}

// watchShellExit 等待指定 shell 的进程退出；打开进程失败时退回 5 秒检查。
func (w *Watcher) watchShellExit(ctx context.Context, shell ShellID, out chan<- ShellID) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		proc, err := windows.OpenProcess(windows.SYNCHRONIZE, false, shell.PID)
		if err != nil {
			w.logger.Printf("打开 explorer 进程失败，退回 5 秒检查: %v", err)
			w.pollShell(ctx, shell, out)
			return
		}
		defer windows.CloseHandle(proc)
		w.waitProcess(ctx, proc, shell, out)
	}()
}

// waitProcess 用停止事件加进程句柄阻塞等待，explorer 退出后把旧 shell 发给 out。
func (w *Watcher) waitProcess(ctx context.Context, proc windows.Handle, shell ShellID, out chan<- ShellID) {
	stop, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		w.logger.Printf("创建停止事件失败，退回 5 秒检查: %v", err)
		w.pollShell(ctx, shell, out)
		return
	}
	defer windows.CloseHandle(stop)
	done := make(chan struct{})
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	go func() {
		defer watchWG.Done()
		select {
		case <-ctx.Done():
			_ = windows.SetEvent(stop)
		case <-done:
		}
	}()
	// 先等辅助协程退出，关闭事件句柄，避免关闭正在使用的句柄。
	defer func() {
		close(done)
		watchWG.Wait()
	}()

	event, err := windows.WaitForMultipleObjects([]windows.Handle{proc, stop}, false, windows.INFINITE)
	if err != nil {
		w.logger.Printf("等待 explorer 进程失败，退回 5 秒检查: %v", err)
		w.pollShell(ctx, shell, out)
		return
	}
	if event != 0 {
		return // 停止事件触发，应用正在退出
	}
	select {
	case out <- shell:
	case <-ctx.Done():
	}
}

// pollShell 每 5 秒检查 shell 进程号是否变化；变化时把旧 shell 发给 out。
//
// 仅在无法等待进程句柄时使用；监听循环收到旧 shell 后会重新查找当前 shell。
func (w *Watcher) pollShell(ctx context.Context, old ShellID, out chan<- ShellID) {
	ticker := time.NewTicker(fallbackPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			shell, ok := currentShell()
			if !ok || shell.PID == old.PID {
				continue
			}
			select {
			case out <- old:
			case <-ctx.Done():
			}
			return
		}
	}
}

// watchAndSend 等待桌面重新就绪，把新 shell 投回事件通道，不阻塞监听循环。
func (w *Watcher) watchAndSend(ctx context.Context, out chan<- ShellID) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		shell, ok := w.waitForShell(ctx)
		if !ok {
			return
		}
		select {
		case out <- shell:
		case <-ctx.Done():
		}
	}()
}
