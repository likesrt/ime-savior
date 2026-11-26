package main

import (
	"testing"
	"time"
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
