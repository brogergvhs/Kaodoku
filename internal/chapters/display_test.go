package chapters

import "testing"

func TestDisplayName(t *testing.T) {
	cases := []struct {
		label, title, name, volume string
	}{
		{"1", "Chapter 1: Vol.1 File 1: Detection and Subsequent Actions", "File 1: Detection and Subsequent Actions", "Vol. 1"},
		{"6", "Chapter 6: Vol.2 File 6: Fresh Start", "File 6: Fresh Start", "Vol. 2"},
		{"9", "Chapter 9 : File: 09: Killtacular", "File: 09: Killtacular", ""},
		{"9.5", "Chapter 9.5: File 9.5", "File 9.5", ""},
		{"1", "Chapter 1", "", ""},
		{"7", "Chapter 7.", "", ""},
		{"9", "Chapter 09", "", ""}, // leading zero still repeats the label
		{"12", "The Promise", "The Promise", ""},
		{"3", "Vol. 2 Something", "Something", "Vol. 2"},
		{"3", "Vol.1 Chapter 3: Epilogue", "Epilogue", "Vol. 1"},
		{"9", "Chapter 9.5", "Chapter 9.5", ""}, // different number: not ours to strip
		{"1", "", "", ""},
		{"Extra", "Chapter 2: Bonus", "Chapter 2: Bonus", ""}, // non-numeric label never matches
		{"2", "ch.2 - Night Raid", "Night Raid", ""},
		{"1", "Chapter 1, The Beginning", "The Beginning", ""},
		{"12", "Chapter 12：Fullwidth Colon", "Fullwidth Colon", ""},
		{"10", "Chapter 10a: Split Chapter", "Chapter 10a: Split Chapter", ""}, // attached suffix: not ours
		{"22.5", "Chapter 22.5b Extra", "Chapter 22.5b Extra", ""},
		{"9", "Chapter 9. Story", "Story", ""},
		{"2", "Vol. 2, Something", "Something", "Vol. 2"},
	}
	for _, c := range cases {
		got := DisplayName(c.label, c.title)
		if got.Name != c.name || got.Volume != c.volume {
			t.Errorf("DisplayName(%q, %q) = (%q, %q), want (%q, %q)",
				c.label, c.title, got.Name, got.Volume, c.name, c.volume)
		}
	}
}
