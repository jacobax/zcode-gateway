package main

import (
	"testing"
	"time"
)

func TestNextRelayStep(t *testing.T) {
	const wait = 5 * time.Second
	cases := []struct {
		name       string
		verdict    upstreamVerdict
		done       bool
		budget     time.Duration
		wantStep   relayStep
		wantBudget time.Duration
	}{
		{"响应已写出", upstreamVerdict{}, true, 20 * time.Second, relayDone, 20 * time.Second},
		{"频控且预算够 → 原地等", upstreamVerdict{Retry: true, Wait: wait}, false, 20 * time.Second, relayWait, 15 * time.Second},
		{"频控但预算不够 → 换号", upstreamVerdict{Retry: true, Wait: wait}, false, 3 * time.Second, relaySwitch, 3 * time.Second},
		{"频控没有等待时长 → 换号", upstreamVerdict{Retry: true}, false, 20 * time.Second, relaySwitch, 20 * time.Second},
		{"预算刚好够 → 等完清零", upstreamVerdict{Retry: true, Wait: wait}, false, wait, relayWait, 0},
		{"不可重试的响应", upstreamVerdict{Retry: false}, false, 20 * time.Second, relayRetry, 20 * time.Second},
		{"转发失败（空 verdict）", upstreamVerdict{}, false, 20 * time.Second, relayRetry, 20 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step, budget := nextRelayStep(tc.verdict, tc.done, tc.budget)
			if step != tc.wantStep || budget != tc.wantBudget {
				t.Fatalf("step=%v budget=%v want %v/%v", step, budget, tc.wantStep, tc.wantBudget)
			}
		})
	}
}

func TestParseCLIArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    cliAction
		wantArg string
	}{
		{"无参数 → 正常启动", nil, cliRun, ""},
		{"空切片 → 正常启动", []string{}, cliRun, ""},
		{"--version", []string{"--version"}, cliVersion, ""},
		{"-v", []string{"-v"}, cliVersion, ""},
		{"version", []string{"version"}, cliVersion, ""},
		{"--help", []string{"--help"}, cliHelp, ""},
		{"-h", []string{"-h"}, cliHelp, ""},
		{"未知参数 → 拒绝启动", []string{"--bogus"}, cliInvalid, "--bogus"},
		{"只看第一个参数", []string{"--version", "--bogus"}, cliVersion, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, arg := parseCLIArgs(tc.args)
			if action != tc.want || arg != tc.wantArg {
				t.Fatalf("action=%v arg=%q want %v/%q", action, arg, tc.want, tc.wantArg)
			}
		})
	}
}
