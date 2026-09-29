package transform

import "testing"

func TestParseModelSuffixes(t *testing.T) {
	tests := []struct {
		model       string
		wantBase    string
		wantEffort  string
		wantNoThink bool
		wantMaxMsgs int
	}{
		// Plain names pass through untouched.
		{"deepseek-flash", "deepseek-flash", "", false, 0},
		{"deepseek-v4-pro", "deepseek-v4-pro", "", false, 0},

		// Effort and thinking toggle, as before.
		{"deepseek-flash:max", "deepseek-flash", "max", false, 0},
		{"deepseek-flash:nothink", "deepseek-flash", "", true, 0},
		{"deepseek-flash:low:nothink", "deepseek-flash", "low", true, 0},
		{"deepseek-v4-pro:high:nothink", "deepseek-v4-pro", "high", true, 0},

		// History cap: only the explicit "mm" form.
		{"deepseek-flash:mm120", "deepseek-flash", "", false, 120},
		{"deepseek-flash:mm200", "deepseek-flash", "", false, 200},
		{"deepseek-flash:mm300", "deepseek-flash", "", false, 300},

		// Zero means "no cap", a valid override.
		{"deepseek-flash:mm0", "deepseek-flash", "", false, 0},

		// Order independence: every permutation means the same thing.
		{"deepseek-flash:low:nothink:mm200", "deepseek-flash", "low", true, 200},
		{"deepseek-flash:mm200:low:nothink", "deepseek-flash", "low", true, 200},
		{"deepseek-flash:nothink:mm200:low", "deepseek-flash", "low", true, 200},
		{"deepseek-flash:mm200:nothink:low", "deepseek-flash", "low", true, 200},
		{"deepseek-flash:low:mm200", "deepseek-flash", "low", false, 200},
		{"deepseek-flash:mm200:low", "deepseek-flash", "low", false, 200},

		// A bare number is NOT a cap: it is ambiguous, so the whole name is
		// rejected and routed upstream as-is.
		{"deepseek-flash:120", "deepseek-flash:120", "", false, 0},
		{"deepseek-flash:low:nothink:42", "deepseek-flash:low:nothink:42", "", false, 0},

		// An unrecognized suffix is left intact so the name routes as-is.
		{"deepseek-flash:banana", "deepseek-flash:banana", "", false, 0},
		{"deepseek-flash:max:banana", "deepseek-flash:max:banana", "", false, 0},
		{"ds-v4:flash:low:nothink:mm42", "ds-v4:flash:low:nothink:mm42", "", false, 0},

		// "mm" with no digits, or a non-numeric body, is not a cap token.
		{"deepseek-flash:mm", "deepseek-flash:mm", "", false, 0},
		{"deepseek-flash:mmx", "deepseek-flash:mmx", "", false, 0},
		{"deepseek-flash:m200", "deepseek-flash:m200", "", false, 0},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			base, effort, noThink, maxMessages := ParseModelSuffixes(tc.model)
			if base != tc.wantBase {
				t.Errorf("base = %q, want %q", base, tc.wantBase)
			}
			if effort != tc.wantEffort {
				t.Errorf("effort = %q, want %q", effort, tc.wantEffort)
			}
			if noThink != tc.wantNoThink {
				t.Errorf("noThink = %v, want %v", noThink, tc.wantNoThink)
			}
			if maxMessages != tc.wantMaxMsgs {
				t.Errorf("maxMessages = %d, want %d", maxMessages, tc.wantMaxMsgs)
			}
		})
	}
}

// TestParseModelSuffixesOrderIndependent pins the core contract: for a set of
// suffixes, every permutation yields the same parsed result.
func TestParseModelSuffixesOrderIndependent(t *testing.T) {
	perms := [][]string{
		{"low", "nothink", "mm200"},
		{"low", "mm200", "nothink"},
		{"nothink", "low", "mm200"},
		{"nothink", "mm200", "low"},
		{"mm200", "low", "nothink"},
		{"mm200", "nothink", "low"},
	}

	for _, p := range perms {
		model := "deepseek-flash:" + p[0] + ":" + p[1] + ":" + p[2]
		base, effort, noThink, maxMessages := ParseModelSuffixes(model)
		if base != "deepseek-flash" || effort != "low" || !noThink || maxMessages != 200 {
			t.Errorf("%s -> base=%q effort=%q noThink=%v max=%d, want deepseek-flash/low/true/200",
				model, base, effort, noThink, maxMessages)
		}
	}
}
