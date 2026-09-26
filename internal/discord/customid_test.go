package discord

import "testing"

func TestParseAction(t *testing.T) {
	cases := []struct {
		customID string
		values   []string
		want     action
	}{
		{customID: customCancel, want: action{kind: actionCancel}},
		{customID: customExpand, want: action{kind: actionExpand}},
		{customID: customPick, values: []string{"grp-1", "grp-2"}, want: action{kind: actionPick, value: "grp-1"}},
		{customID: customAdd + "grp-1", want: action{kind: actionAdd, value: "grp-1"}},
		{customID: customLastFM, want: action{kind: actionLastFMAdd}},
		{customID: customPick, want: action{kind: actionPick}},
		{customID: customAdd, want: action{kind: actionNone}},
		{customID: "unknown", want: action{kind: actionNone}},
	}
	for _, tc := range cases {
		got := parseAction(tc.customID, tc.values)
		if got != tc.want {
			t.Errorf("parseAction(%q, %v) = %+v, want %+v", tc.customID, tc.values, got, tc.want)
		}
	}
}
