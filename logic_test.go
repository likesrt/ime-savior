package main

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestShellTrackerSameShell(t *testing.T) {
	var tracker shellTracker
	shell := ShellID{HWND: 100, PID: 200}

	delay, ok := tracker.plan(shell, false)
	if !ok || delay != settleDelay {
		t.Fatalf("首次 plan = (%v, %v)，期望 (%v, true)", delay, ok, settleDelay)
	}
	tracker.markPulsed(shell)

	if delay, ok := tracker.plan(shell, false); ok || delay != 0 {
		t.Fatalf("同一 shell 重复 plan = (%v, %v)，期望 (0, false)", delay, ok)
	}
}

func TestShellTrackerNewShell(t *testing.T) {
	var tracker shellTracker
	oldShell := ShellID{HWND: 100, PID: 200}
	newShell := ShellID{HWND: 300, PID: 400}

	if _, ok := tracker.plan(oldShell, false); !ok {
		t.Fatal("旧 shell 首次 plan 应返回 true")
	}
	tracker.markPulsed(oldShell)

	if delay, ok := tracker.plan(newShell, false); !ok || delay != settleDelay {
		t.Fatalf("新 shell plan = (%v, %v)，期望 (%v, true)", delay, ok, settleDelay)
	}
}

func TestShellTrackerManualBypassesCompleted(t *testing.T) {
	var tracker shellTracker
	shell := ShellID{HWND: 100, PID: 200}
	tracker.markPulsed(shell)

	if delay, ok := tracker.plan(shell, true); !ok || delay != 0 {
		t.Fatalf("手动 plan = (%v, %v)，期望 (0, true)", delay, ok)
	}
}

func TestShellTrackerRejectsInvalidShell(t *testing.T) {
	var tracker shellTracker
	cases := []ShellID{
		{},
		{HWND: 100},
		{PID: 200},
	}
	for _, shell := range cases {
		if _, ok := tracker.plan(shell, false); ok {
			t.Fatalf("无效 shell %+v 不应安排脉冲", shell)
		}
		if _, ok := tracker.plan(shell, true); ok {
			t.Fatalf("无效 shell %+v 手动触发也应拒绝", shell)
		}
	}
}

func TestSameProcess(t *testing.T) {
	current := ShellID{HWND: 1, PID: 10}
	cases := []struct {
		name  string
		left  ShellID
		right ShellID
		want  bool
	}{
		{"同一 PID 不同窗口", current, ShellID{HWND: 2, PID: 10}, true},
		{"不同 PID", current, ShellID{HWND: 1, PID: 20}, false},
		{"右侧 PID 为零", current, ShellID{HWND: 1}, false},
		{"左侧 PID 为零", ShellID{HWND: 1}, current, false},
	}
	for _, tc := range cases {
		if got := sameProcess(tc.left, tc.right); got != tc.want {
			t.Fatalf("%s: sameProcess = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

func TestRetryPolicyBackoff(t *testing.T) {
	var policy retryPolicy
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}

	for i, expected := range want {
		delay, ok := policy.next()
		if !ok || delay != expected {
			t.Fatalf("第 %d 次 next = (%v, %v)，期望 (%v, true)", i+1, delay, ok, expected)
		}
	}
	if delay, ok := policy.next(); ok || delay != 0 {
		t.Fatalf("超过上限 next = (%v, %v)，期望 (0, false)", delay, ok)
	}

	policy.reset()
	if delay, ok := policy.next(); !ok || delay != retryBaseDelay {
		t.Fatalf("reset 后 next = (%v, %v)，期望 (%v, true)", delay, ok, retryBaseDelay)
	}
}

// TestBuildTaskXML 检查任务定义的关键设置、路径转义和 XML 合法性。
func TestBuildTaskXML(t *testing.T) {
	got := buildTaskXML("S-1-5-21-1000", `C:\Program Files\A & B\ImeSavior.exe`, runLevelHighest)

	for _, want := range []string{
		`<UserId>S-1-5-21-1000</UserId>`,
		`<LogonType>InteractiveToken</LogonType>`,
		`<RunLevel>HighestAvailable</RunLevel>`,
		`<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>`,
		`<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
		`<Command>"C:\Program Files\A &amp; B\ImeSavior.exe"</Command>`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("任务 XML 缺少 %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "<UserId>"); n != 2 {
		t.Fatalf("UserId 出现 %d 次，期望 2 次", n)
	}
	if err := checkXML(got); err != nil {
		t.Fatalf("任务 XML 不合法: %v", err)
	}
}

// TestBuildTaskXMLRunLevel 检查普通权限运行级别和无空格路径的写法。
func TestBuildTaskXMLRunLevel(t *testing.T) {
	got := buildTaskXML("S-1-5-21-1000", `C:\ImeSavior.exe`, runLevelLeast)
	if !strings.Contains(got, `<RunLevel>LeastPrivilege</RunLevel>`) {
		t.Fatalf("任务 XML 未使用 LeastPrivilege:\n%s", got)
	}
	if !strings.Contains(got, `<Command>C:\ImeSavior.exe</Command>`) {
		t.Fatalf("无空格路径不应加引号:\n%s", got)
	}
}

// TestEncodeUTF16LE 检查 BOM、字节序和往返解码。
func TestEncodeUTF16LE(t *testing.T) {
	const text = "A中"
	got := encodeUTF16LE(text)
	want := []byte{0xFF, 0xFE, 0x41, 0x00, 0x2D, 0x4E}
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeUTF16LE = % X，期望 % X", got, want)
	}

	units := make([]uint16, 0, (len(got)-2)/2)
	for i := 2; i < len(got); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(got[i:]))
	}
	if decoded := string(utf16.Decode(units)); decoded != text {
		t.Fatalf("解码 = %q，期望 %q", decoded, text)
	}
}

// checkXML 用宽松的字符集阅读器检查 XML 是否合法；输入字符串本身是 UTF-8。
func checkXML(s string) error {
	decoder := xml.NewDecoder(strings.NewReader(s))
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
