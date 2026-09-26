package discord

import "strings"

const (
	customCancel = "cancel"
	customExpand = "expand"
	customPick   = "pick"
	customAdd    = "add:"
)

type actionKind int

const (
	actionNone actionKind = iota
	actionCancel
	actionExpand
	actionAdd
	actionPick
)

type action struct {
	kind  actionKind
	value string
}

// parseAction maps a component custom_id plus select values to an action.
func parseAction(customID string, values []string) action {
	switch customID {
	case customCancel:
		return action{kind: actionCancel}
	case customExpand:
		return action{kind: actionExpand}
	case customPick:
		if len(values) > 0 {
			return action{kind: actionPick, value: values[0]}
		}
		return action{kind: actionPick}
	}
	if rest, ok := strings.CutPrefix(customID, customAdd); ok && rest != "" {
		return action{kind: actionAdd, value: rest}
	}
	return action{}
}
