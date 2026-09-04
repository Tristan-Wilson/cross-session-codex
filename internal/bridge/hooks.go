package bridge

import "os"

func handleHook(event Object) (Object, error) {
	if event["agent_id"] != nil {
		return Object{}, nil
	}
	thread, err := canonicalThread(str(event, "session_id"))
	if err != nil {
		return nil, err
	}
	switch str(event, "hook_event_name") {
	case "SessionStart":
		// Hooks run in the shared server, not the owning UI process. They may
		// update an existing registration but must never create an unowned one.
		if _, err = RPC(thread, "info", nil); err != nil {
			return Object{}, nil
		}
		options := Object{}
		if cwd := str(event, "cwd"); cwd != "" {
			options["cwd"] = cwd
		}
		// The advertised messaging class is configured separately from Codex's
		// approval mode. A hook must not overwrite the user's messaging choice.
		state, err := RPC(thread, "configure", options)
		if err != nil {
			return Object{"systemMessage": "Cross-session messaging: " + err.Error()}, nil
		}
		exe, _ := os.Executable()
		return Object{"hookSpecificOutput": Object{"hookEventName": "SessionStart", "additionalContext": "Cross-session peer " + str(state, "name") + " is enabled. " + inboxNotice(exe, thread)}}, nil
	case "SessionEnd":
		_, _ = RPC(thread, "stop", nil)
	}
	return Object{}, nil
}
