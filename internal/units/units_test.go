package units

import "testing"

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"8G": 8 << 30, "512M": 512 << 20, "1.5G": 3 << 29, "26Gi": 26 << 30, "100": 100, "": 0} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseSize("lots"); err == nil {
		t.Error("ParseSize(lots) should fail")
	}
	if FormatSize(26<<30) != "26G" || FormatSize(3<<29) != "1.5G" {
		t.Errorf("FormatSize: %s %s", FormatSize(26<<30), FormatSize(3<<29))
	}
}

func TestCPUList(t *testing.T) {
	c, err := ParseCPUList("0-14,16-30")
	if err != nil || len(c) != 30 || c[15] != 16 {
		t.Fatalf("ParseCPUList: %v %v", c, err)
	}
	if s := FormatCPUList(c); s != "0-14,16-30" {
		t.Errorf("FormatCPUList = %q", s)
	}
	if s := FormatCPUList([]int{1, 3, 4, 5, 9}); s != "1,3-5,9" {
		t.Errorf("FormatCPUList = %q", s)
	}
}

func TestParseDuration(t *testing.T) {
	d, err := ParseDuration("14d")
	if err != nil || d.Hours() != 14*24 {
		t.Errorf("ParseDuration(14d) = %v, %v", d, err)
	}
}
