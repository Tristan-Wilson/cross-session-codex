package bridge

import (
	"testing"

	"github.com/google/uuid"
)

func TestMessagingDefaultsAcceptRepliesAcrossClasses(t *testing.T) {
	isolatedState(t)
	a, b := uuid.NewString(), uuid.NewString()
	info := cli(t, "start", "--thread", a, "--name", "default-peer", "--delivery", "manual")
	t.Cleanup(func() { _ = Stop(a) })
	if str(info, "inbound") != "accept" || str(info, "permission_class") != "bypass" {
		t.Fatalf("unexpected messaging defaults: %v", info)
	}
	cli(t, "start", "--thread", b, "--name", "prompting-peer", "--delivery", "manual", "--permission-class", "prompting")
	t.Cleanup(func() { _ = Stop(b) })
	cli(t, "send", "prompting-peer", "--thread", a, "--body", "default ping")
	page := cli(t, "wait", "--thread", b, "--timeout", "5")
	rows := page["messages"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["from_mode"] != "bypass" {
		t.Fatalf("default wire class was not bypass: %v", page)
	}
	cli(t, "reply", str(rows[0].(map[string]any), "id"), "--thread", b, "--body", "prompting reply")
	page = cli(t, "wait", "--thread", a, "--timeout", "5")
	rows = page["messages"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["from_mode"] != "prompting" || number(page, "held", 0) != 0 {
		t.Fatalf("default receiver held a cross-class reply: %v", page)
	}
}

func TestHooksPreserveMessagingClassAndAdmission(t *testing.T) {
	isolatedState(t)
	thread := uuid.NewString()
	cli(t, "start", "--thread", thread, "--name", "hook-peer", "--delivery", "manual")
	t.Cleanup(func() { _ = Stop(thread) })
	for _, settings := range []struct{ inbound, permission string }{{"accept", "bypass"}, {"parity", "prompting"}, {"hold", "bypass"}} {
		cli(t, "start", "--thread", thread, "--inbound", settings.inbound, "--permission-class", settings.permission)
		for _, event := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop", "Interrupt"} {
			for _, mode := range []string{"", "default", "auto", "acceptEdits", "dontAsk", "plan", "bypassPermissions"} {
				_, err := handleHook(Object{"session_id": thread, "hook_event_name": event, "permission_mode": mode})
				must(t, err)
				info, err := RPC(thread, "info", nil)
				must(t, err)
				if str(info, "inbound") != settings.inbound || str(info, "permission_class") != settings.permission {
					t.Fatalf("%s/%s overwrote messaging settings: %v", event, mode, info)
				}
			}
		}
		must(t, Stop(thread))
		info := cli(t, "start", "--thread", thread)
		if str(info, "inbound") != settings.inbound || str(info, "permission_class") != settings.permission {
			t.Fatalf("restart overwrote explicit messaging settings: %v", info)
		}
	}
}

func TestMCPMessagingClassConfiguration(t *testing.T) {
	isolatedState(t)
	thread := uuid.NewString()
	info, err := callTool("enable_messaging", Object{"thread_id": thread, "name": "mcp-peer", "delivery": "manual", "permission_class": "prompting", "inbound": "parity"})
	must(t, err)
	t.Cleanup(func() { _ = Stop(thread) })
	if str(info, "permission_class") != "prompting" || str(info, "inbound") != "parity" {
		t.Fatalf("MCP settings were not applied: %v", info)
	}
	if _, err = callTool("enable_messaging", Object{"thread_id": thread, "permission_class": "auto"}); err == nil {
		t.Fatal("accepted a Codex mode as a messaging class")
	}
}
