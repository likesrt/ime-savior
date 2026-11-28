//go:build windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

const (
	appName     = "ImeSavior"
	logMaxBytes = 128 * 1024
	logBackups  = 1
)

// main 启动单实例托盘应用；程序没有窗口，退出由托盘菜单触发。
func main() {
	// 先固定应用名，让单实例锁不随 exe 文件名变化。
	mygo.App.SetName(appName)
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}
	logger, closeLog := setupLogger()
	defer closeLog()
	mygo.App.OnWindowAllClosed(func() {}) // 没有窗口时也保持事件循环
	mygo.App.OnSecondInstance(func([]string, string) {
		logger.Printf("已有实例在运行，忽略重复启动")
	})
	mygo.App.WhenReady(func() { startApp(logger) })
	if err := mygo.App.Run(); err != nil {
		logger.Printf("应用退出: %v", err)
	}
}

// startApp 创建托盘、菜单和监听器；它运行在 MyGo 主线程。
func startApp(logger *log.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	watcher := NewWatcher(pulseKeyboard, logger)
	menu := newTrayMenu(watcher, logger)
	tray, err := mygo.NewTray(mygo.TrayOptions{
		ID: "imesavior", Icon: trayIcon(), ToolTip: "输入法救星", Menu: menu,
	})
	if err != nil {
		logger.Printf("创建托盘失败: %v", err)
		cancel()
		mygo.App.Quit()
		return
	}
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		cancel()
		if !watcher.Wait(2 * time.Second) {
			logger.Printf("监听器未在 2 秒内退出")
		}
		tray.Destroy()
	})
	go watcher.Run(ctx)
	logger.Printf("已启动托盘")
}

// newTrayMenu 生成托盘菜单；立即修复投递任务，自启失败时回滚勾选。
func newTrayMenu(watcher *Watcher, logger *log.Logger) *mygo.Menu {
	return mygo.NewMenu([]*mygo.MenuItem{
		{Label: "立即修复", Click: func(*mygo.MenuItem, *mygo.Window) {
			watcher.TriggerManual()
		}},
		{Label: "开机自启", Type: mygo.MenuItemCheckbox, Checked: autostartEnabled(),
			Click: func(item *mygo.MenuItem, _ *mygo.Window) {
				if err := setAutostart(item.Checked); err != nil {
					logger.Printf("设置开机自启失败: %v", err)
					item.SetChecked(!item.Checked)
					mygo.Dialog.Error("设置开机自启失败",
						"如果提示拒绝访问，请右键 ImeSavior.exe 选择“以管理员身份运行”，再勾选一次。\n\n"+err.Error())
					return
				}
				if item.Checked {
					logger.Printf("已启用开机自启任务")
				} else {
					logger.Printf("已关闭开机自启任务")
				}
			}},
		mygo.Separator(),
		{Label: "退出", Click: func(*mygo.MenuItem, *mygo.Window) {
			mygo.App.Quit()
		}},
	})
}

// setupLogger 把日志写到用户缓存目录；失败时退化为丢弃，不影响主流程。
func setupLogger() (*log.Logger, func()) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return log.New(io.Discard, "", 0), func() {}
	}
	dir = filepath.Join(dir, appName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return log.New(io.Discard, "", 0), func() {}
	}
	writer, err := newRotatingWriter(filepath.Join(dir, appName+".log"), logMaxBytes, logBackups)
	if err != nil {
		return log.New(io.Discard, "", 0), func() {}
	}
	return log.New(writer, "", log.LstdFlags), func() { _ = writer.Close() }
}

// rotatingWriter 是按大小轮转的单文件写入器，固定上限，避免日志无限增长。
type rotatingWriter struct {
	mu      sync.Mutex
	path    string
	max     int64
	backups int
	file    *os.File
	size    int64
}

// newRotatingWriter 打开或创建日志文件，并从现有大小继续计数。
func newRotatingWriter(path string, max int64, backups int) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, max: max, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open 以追加方式打开文件并记录当前大小。
func (w *rotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	w.file, w.size = file, info.Size()
	return nil
}

// Write 在写入前检查上限，超限先轮转；并发调用由互斥锁串行化。
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size+int64(len(p)) > w.max {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate 关闭当前文件，把旧文件依次后移，再开新文件。
func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	for i := w.backups; i >= 1; i-- {
		src, dst := w.path, fmt.Sprintf("%s.%d", w.path, i)
		if i > 1 {
			src = fmt.Sprintf("%s.%d", w.path, i-1)
		}
		_ = os.Remove(dst)
		_ = os.Rename(src, dst)
	}
	return w.open()
}

// Close 关闭日志文件；重复调用是安全的。
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// trayIcon 在内存中画一个 32×32 的键盘图标，避免额外资源文件。
func trayIcon() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	bg := color.NRGBA{R: 79, G: 124, B: 255, A: 255}
	drawRoundedRect(img, 2, 2, 30, 30, 7, bg)
	drawKeyboardKeys(img)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

// drawRoundedRect 填充一个圆角矩形；半径外的像素保持透明。
func drawRoundedRect(img *image.NRGBA, x0, y0, x1, y1, r int, c color.NRGBA) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if insideRounded(x, y, x0, y0, x1, y1, r) {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}

// insideRounded 判断像素是否落在圆角矩形内。
func insideRounded(x, y, x0, y0, x1, y1, r int) bool {
	if x < x0 || x >= x1 || y < y0 || y >= y1 {
		return false
	}
	if x >= x0+r && x < x1-r {
		return true
	}
	if y >= y0+r && y < y1-r {
		return true
	}
	cx, cy := x0+r, y0+r
	if x < cx && y < cy {
		return (cx-x)*(cx-x)+(cy-y)*(cy-y) <= r*r
	}
	cx, cy = x1-r-1, y0+r
	if x > cx && y < cy {
		return (x-cx)*(x-cx)+(cy-y)*(cy-y) <= r*r
	}
	cx, cy = x0+r, y1-r-1
	if x < cx && y > cy {
		return (cx-x)*(cx-x)+(y-cy)*(y-cy) <= r*r
	}
	cx, cy = x1-r-1, y1-r-1
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r
}

// drawKeyboardKeys 在图标上画三排白色键位和一条空格键。
func drawKeyboardKeys(img *image.NRGBA) {
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 230}
	for row := 0; row < 3; row++ {
		for col := 0; col < 4; col++ {
			x, y := 8+col*4, 10+row*4
			draw.Draw(img, image.Rect(x, y, x+2, y+2), image.NewUniform(white), image.Point{}, draw.Src)
		}
	}
	draw.Draw(img, image.Rect(11, 22, 21, 24), image.NewUniform(white), image.Point{}, draw.Src)
}
