package delivery

import "testing"

func TestDeliveryDefaultsAndExistingGroupCompatibility(t *testing.T) {
	task, err := NormalizeTaskMode("")
	if err != nil || task != Direct {
		t.Fatalf("task default=%q %v", task, err)
	}
	group, err := NormalizeNewGroupMode("")
	if err != nil || group != Direct {
		t.Fatalf("new group default=%q %v", group, err)
	}
	legacy, err := ResolveStoredGroupMode("")
	if err != nil || legacy != Combined {
		t.Fatalf("old group shape changed: %q %v", legacy, err)
	}
	if _, err := NormalizeTaskMode(Combined); err == nil {
		t.Fatal("single task accepted an unnecessary group mode")
	}
	for _, invalid := range []string{"auto", "unknown", "synthesize ", "DIRECT"} {
		if _, err := NormalizeTaskMode(invalid); err == nil {
			t.Fatalf("task accepted %q", invalid)
		}
		if _, err := NormalizeNewGroupMode(invalid); err == nil {
			t.Fatalf("group accepted %q", invalid)
		}
	}
}
