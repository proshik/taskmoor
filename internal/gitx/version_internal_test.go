package gitx

import "testing"

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][3]int{
		"git version 2.50.1 (Apple Git-155)\n": {2, 50, 1},
		"git version 2.45.0\n":                 {2, 45, 0},
		"git version 2.44.0.windows.1":         {2, 44, 0},
		"git version 3.0.rc1":                  {3, 0, 0},
	} {
		if got, ok := parseVersion(in); !ok || got != want {
			t.Errorf("%q: %v %v", in, got, ok)
		}
	}
	if _, ok := parseVersion("nonsense"); ok {
		t.Error("parsed nonsense")
	}
}
