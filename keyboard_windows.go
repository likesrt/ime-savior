//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// errKeyboardAlreadyOpen 表示调用前触摸键盘就已经处于可见状态，本次不做开合。
var errKeyboardAlreadyOpen = errors.New("触摸键盘已经打开")

const (
	coinitSTA    = 0x2  // COINIT_APARTMENTTHREADED，触摸键盘接口要求 STA
	clsctxAll    = 0x17 // CLSCTX_ALL
	sFalse       = 0x1  // S_FALSE：COM 已在本线程初始化，仍需配对 CoUninitialize
	pollInterval = 50 * time.Millisecond
	pollTimeout  = 5 * time.Second
	holdDuration = 500 * time.Millisecond
	tabTipDelay  = 500 * time.Millisecond
)

var (
	// IFrameworkInputPane：查询触摸键盘位置（可见性）。
	clsidFrameworkInputPane = windows.GUID{Data1: 0xD5120AA3, Data2: 0x46BA, Data3: 0x44C5, Data4: [8]byte{0x82, 0x2D, 0xCA, 0x80, 0x92, 0xC1, 0xFC, 0x72}}
	iidFrameworkInputPane   = windows.GUID{Data1: 0x5752238B, Data2: 0x24F0, Data3: 0x495A, Data4: [8]byte{0x82, 0xF1, 0x2F, 0xD5, 0x93, 0x05, 0x67, 0x96}}
	// ITipInvocation：切换触摸键盘开合。
	clsidTipInvocation = windows.GUID{Data1: 0x4CE576FA, Data2: 0x83DC, Data3: 0x4F88, Data4: [8]byte{0x95, 0x1C, 0x9D, 0x07, 0x82, 0xB4, 0xE3, 0x76}}
	iidTipInvocation   = windows.GUID{Data1: 0x37C994E7, Data2: 0x432B, Data3: 0x4834, Data4: [8]byte{0xA2, 0xF7, 0xDC, 0xE1, 0xF1, 0x3B, 0x83, 0x4B}}

	procCoCreateInstance = windows.NewLazySystemDLL("ole32.dll").NewProc("CoCreateInstance")
)

// keyboardRect 对应 Win32 RECT，用于接收 IFrameworkInputPane.Location 的输出。
type keyboardRect struct {
	Left, Top, Right, Bottom int32
}

// pulseKeyboard 打开并关闭一次触摸键盘，用于确认开合链路可用。
//
// 参数 ctx 控制所有等待；它取消时返回 ctx.Err()，不会无限阻塞。
// 若调用前键盘已可见，返回 errKeyboardAlreadyOpen（不计为故障）。
// 返回 nil 表示完成一次“可见→隐藏”的脉冲；其余错误带步骤名与 HRESULT 十六进制。
// 副作用：锁定当前 OS 线程并在其上初始化/反初始化 COM STA，可能启动 TabTip.exe。
func pulseKeyboard(ctx context.Context) error {
	// COM 接口有线程亲和性，必须锁定同一线程完成创建与调用。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := comInit(); err != nil {
		return err
	}
	defer windows.CoUninitialize()

	pane, err := createCOM(&clsidFrameworkInputPane, &iidFrameworkInputPane)
	if err != nil {
		return fmt.Errorf("创建 IFrameworkInputPane 失败: %w", err)
	}
	defer releaseCOM(pane)

	visible, err := keyboardVisible(pane)
	if err != nil {
		return fmt.Errorf("读取触摸键盘可见性失败: %w", err)
	}
	if visible {
		return errKeyboardAlreadyOpen
	}
	return togglePulse(ctx, pane)
}

// comInit 在当前线程初始化 COM STA 环境。
//
// 参数无。成功或 S_FALSE（本线程已初始化）返回 nil，调用方必须配对 CoUninitialize；
// 其他 HRESULT 返回带十六进制码的错误。只在锁定的 OS 线程上调用。
func comInit() error {
	err := windows.CoInitializeEx(0, coinitSTA)
	if err == nil {
		return nil
	}
	if errno, ok := err.(syscall.Errno); ok {
		if uint32(errno) == sFalse {
			return nil
		}
		return fmt.Errorf("CoInitializeEx 失败: HRESULT 0x%08X", uint32(errno))
	}
	return fmt.Errorf("CoInitializeEx 失败: %w", err)
}

// createCOM 通过 ole32!CoCreateInstance 创建指定 CLSID/IID 的 COM 对象。
//
// clsid、iid 不能为空。成功返回接口指针，调用方负责 releaseCOM；
// 失败返回带 HRESULT 十六进制码的错误。不处理封送，仅限本线程使用。
func createCOM(clsid, iid *windows.GUID) (unsafe.Pointer, error) {
	var obj unsafe.Pointer
	r1, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(clsid)),
		0,
		clsctxAll,
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(&obj)),
	)
	if hr := uint32(r1); hr != 0 {
		return nil, fmt.Errorf("CoCreateInstance 失败: HRESULT 0x%08X", hr)
	}
	if obj == nil {
		return nil, errors.New("CoCreateInstance 返回空接口指针")
	}
	return obj, nil
}

// comVtblFn 读取 COM 接口 vtable 指定槽位的函数指针。
//
// obj 为有效接口指针，index 从 0 开始（IUnknown 占 0-2）。
// 返回值只用于 SyscallN，不校验槽位是否越界。
func comVtblFn(obj unsafe.Pointer, index uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	return *(*uintptr)(unsafe.Add(vtbl, index*unsafe.Sizeof(uintptr(0))))
}

// releaseCOM 调用 COM 接口 vtable 索引 2 的 Release 释放引用。
//
// obj 为接口指针，nil 时安全跳过；无返回值，失败也无从上报。
func releaseCOM(obj unsafe.Pointer) {
	if obj == nil {
		return
	}
	_, _, _ = syscall.SyscallN(comVtblFn(obj, 2), uintptr(obj))
}

// keyboardVisible 查询 IFrameworkInputPane.Location，判断键盘矩形是否非空。
//
// pane 为有效的 IFrameworkInputPane 指针。返回 (可见性, 错误)；
// Location 返回非零 HRESULT 时错误带十六进制码。可见即 right>left 且 bottom>top。
func keyboardVisible(pane unsafe.Pointer) (bool, error) {
	var rect keyboardRect
	r1, _, _ := syscall.SyscallN(comVtblFn(pane, 6), uintptr(pane), uintptr(unsafe.Pointer(&rect)))
	if hr := uint32(r1); hr != 0 {
		return false, fmt.Errorf("IFrameworkInputPane.Location 失败: HRESULT 0x%08X", hr)
	}
	return rect.Right > rect.Left && rect.Bottom > rect.Top, nil
}

// togglePulse 创建 ITipInvocation（必要时兜底启动 TabTip.exe），打开键盘确认后关闭。
//
// ctx 控制等待；pane 已确认不可见。若键盘是用户自己打开的，返回 errKeyboardAlreadyOpen；
// 若是兜底启动 TabTip 打开的，则保持片刻后由本次调用关闭。
// 返回 nil 表示键盘经历了可见再隐藏；失败时带步骤名，创建失败时附带 HRESULT。
func togglePulse(ctx context.Context, pane unsafe.Pointer) error {
	inv, launched, err := acquireTipInvocation(ctx)
	if err != nil {
		return err
	}
	defer releaseCOM(inv)

	visible, err := keyboardVisible(pane)
	if err != nil {
		return fmt.Errorf("读取触摸键盘可见性失败: %w", err)
	}
	if visible {
		if !launched {
			// 用户可能在两次检查之间自己打开了键盘；不要把它当成我们打开的再关掉。
			return errKeyboardAlreadyOpen
		}
		if err := sleepCtx(ctx, holdDuration); err != nil {
			return err
		}
		return closeKeyboard(ctx, inv, pane)
	}
	if err := toggleTip(inv); err != nil {
		return fmt.Errorf("打开触摸键盘失败: %w", err)
	}
	if err := waitKeyboard(ctx, pane, true, pollTimeout); err != nil {
		return fmt.Errorf("等待触摸键盘显示失败: %w", err)
	}
	if err := sleepCtx(ctx, holdDuration); err != nil {
		return err
	}
	return closeKeyboard(ctx, inv, pane)
}

// acquireTipInvocation 创建 ITipInvocation；失败时启动 TabTip.exe 并重试一次。
//
// 返回 launched 表示 TabTip 可能是本次启动的，调用方需要负责关闭它打开的键盘。
// ctx 取消时返回其错误；两次创建都失败时返回带原因的错误。
func acquireTipInvocation(ctx context.Context) (unsafe.Pointer, bool, error) {
	inv, err := createCOM(&clsidTipInvocation, &iidTipInvocation)
	if err == nil {
		return inv, false, nil
	}
	if ferr := launchTabTip(); ferr != nil {
		return nil, false, fmt.Errorf("创建 ITipInvocation 失败: %w；启动 TabTip.exe 也失败: %v", err, ferr)
	}
	if serr := sleepCtx(ctx, tabTipDelay); serr != nil {
		return nil, true, serr
	}
	inv, err = createCOM(&clsidTipInvocation, &iidTipInvocation)
	if err != nil {
		return nil, true, fmt.Errorf("启动 TabTip.exe 后创建 ITipInvocation 仍失败: %w", err)
	}
	return inv, true, nil
}

// closeKeyboard 在键盘可见时切换关闭，并等待其隐藏。
//
// inv、pane 必须有效；ctx 控制等待。返回读取可见性、切换或等待超时的错误。
func closeKeyboard(ctx context.Context, inv unsafe.Pointer, pane unsafe.Pointer) error {
	visible, err := keyboardVisible(pane)
	if err != nil {
		return fmt.Errorf("读取触摸键盘可见性失败: %w", err)
	}
	if visible {
		if err := toggleTip(inv); err != nil {
			return fmt.Errorf("关闭触摸键盘失败: %w", err)
		}
	}
	if err := waitKeyboard(ctx, pane, false, pollTimeout); err != nil {
		return fmt.Errorf("等待触摸键盘隐藏失败: %w", err)
	}
	return nil
}

// toggleTip 调用 ITipInvocation.Toggle 切换触摸键盘开合。
//
// inv 为有效的 ITipInvocation 指针；目标 HWND 取桌面窗口（按接口要求）。
// 返回 nil 表示调用成功，否则错误带 HRESULT 十六进制码。
func toggleTip(inv unsafe.Pointer) error {
	r1, _, _ := syscall.SyscallN(comVtblFn(inv, 3), uintptr(inv), uintptr(windows.GetDesktopWindow()))
	if hr := uint32(r1); hr != 0 {
		return fmt.Errorf("ITipInvocation.Toggle 失败: HRESULT 0x%08X", hr)
	}
	return nil
}

// waitKeyboard 轮询等待键盘可见性变为 want。
//
// ctx 取消立即返回其错误；pane 读取失败立即返回；超过 timeout 返回超时错误。
// 每 pollInterval 检查一次，首次检查不等待。
func waitKeyboard(ctx context.Context, pane unsafe.Pointer, want bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		visible, err := keyboardVisible(pane)
		if err != nil {
			return err
		}
		if visible == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待超过 %s", timeout)
		}
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return err
		}
	}
}

// sleepCtx 等待 d，或在 ctx 结束时提前返回。
//
// ctx 取消返回 ctx.Err()，正常等待返回 nil。用于所有可取消的延时。
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// launchTabTip 用绝对路径启动系统自带 TabTip.exe，作为 ITipInvocation 不可用时的兜底。
//
// 依次尝试 CommonProgramW6432、CommonProgramFiles，先确认文件存在再启动；
// 不经过 shell，也不等待进程退出。启动失败或路径缺失时返回错误。
func launchTabTip() error {
	base := os.Getenv("CommonProgramW6432")
	if base == "" {
		base = os.Getenv("CommonProgramFiles")
	}
	if base == "" {
		return errors.New("未找到 CommonProgramW6432/CommonProgramFiles 环境变量")
	}
	path := filepath.Join(base, "microsoft shared", "ink", "TabTip.exe")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("TabTip.exe 不可用: %w", err)
	}
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 TabTip.exe 失败: %w", err)
	}
	// 不 Wait：键盘程序需要持续运行；Release 失败不影响已经启动的进程。
	_ = cmd.Process.Release()
	return nil
}
