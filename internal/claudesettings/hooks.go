package claudesettings

// Hook editing. settings.json belongs to the user and already carries their
// own hooks, so every function here touches only the entries it is asked to
// and leaves the rest of the array, and every other key, exactly as found.

// AddCommandHook ensures a PreToolUse-style hook entry running command exists
// under event with the given matcher. An entry for which isOurs is true is
// updated in place (so a moved binary is repaired) rather than duplicated.
// It reports whether anything changed.
func AddCommandHook(root map[string]any, event, matcher, command string, timeoutSeconds int, isOurs func(cmd string) bool) bool {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	groups, _ := hooks[event].([]any)

	want := map[string]any{"type": "command", "command": command, "timeout": timeoutSeconds}

	for _, g := range groups {
		gm, _ := g.(map[string]any)
		inner, _ := gm["hooks"].([]any)
		for i, h := range inner {
			hm, _ := h.(map[string]any)
			cmd, _ := hm["command"].(string)
			if !isOurs(cmd) {
				continue
			}
			if cmd == command && gm["matcher"] == matcher && numEq(hm["timeout"], timeoutSeconds) {
				return false
			}
			inner[i] = want
			gm["matcher"] = matcher
			root["hooks"] = hooks
			return true
		}
	}

	groups = append(groups, map[string]any{
		"matcher": matcher,
		"hooks":   []any{want},
	})
	hooks[event] = groups
	root["hooks"] = hooks
	return true
}

// RemoveCommandHooks deletes every hook command for which isOurs is true under
// event, pruning any group, event or hooks key that ends up empty. It returns
// how many hook commands were removed.
func RemoveCommandHooks(root map[string]any, event string, isOurs func(cmd string) bool) int {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		return 0
	}
	groups, _ := hooks[event].([]any)
	removed := 0
	var keptGroups []any
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			keptGroups = append(keptGroups, g)
			continue
		}
		inner, _ := gm["hooks"].([]any)
		var keptInner []any
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if cmd, _ := hm["command"].(string); isOurs(cmd) {
				removed++
				continue
			}
			keptInner = append(keptInner, h)
		}
		if len(keptInner) == 0 && len(inner) > 0 {
			continue // the group held only our hook
		}
		if len(inner) > 0 {
			gm["hooks"] = keptInner
		}
		keptGroups = append(keptGroups, gm)
	}
	if removed == 0 {
		return 0
	}
	if len(keptGroups) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = keptGroups
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	return removed
}

// HasCommandHook reports whether a hook command for which isOurs is true is
// present under event.
func HasCommandHook(root map[string]any, event string, isOurs func(cmd string) bool) bool {
	hooks, _ := root["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		inner, _ := gm["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if cmd, _ := hm["command"].(string); isOurs(cmd) {
				return true
			}
		}
	}
	return false
}

// numEq compares a JSON-decoded number (float64) with an int.
func numEq(v any, n int) bool {
	switch x := v.(type) {
	case float64:
		return int(x) == n
	case int:
		return x == n
	}
	return false
}
