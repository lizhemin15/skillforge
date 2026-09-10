package docgen

import (
	"os"
	"testing"
)

func TestCNUppercaseMoney(t *testing.T) {
	cases := map[int]string{
		0:      "零元整",
		5:      "伍元整",
		10:     "壹拾元整",
		100:    "壹佰元整",
		1000:   "壹仟元整",
		10000:  "壹万元整",
		500:    "伍佰元整",
		85000:  "捌万伍仟元整",
		563400: "伍拾陆万叁仟肆佰元整",
		960500: "玖拾陆万零伍佰元整",
		100050: "壹拾万零伍拾元整",
		10005:  "壹万零伍元整",
		123456: "壹拾贰万叁仟肆佰伍拾陆元整",
	}
	for n, want := range cases {
		got := cnUppercaseMoney(n)
		if got != want {
			t.Errorf("cnUppercaseMoney(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestReviseDocxTotalsFromFile(t *testing.T) {
	src, err := os.ReadFile("/tmp/press2.docx")
	if err != nil {
		t.Skipf("no press2.docx: %v", err)
	}
	out, err := ReviseDocxTotals(src)
	if err != nil {
		t.Fatalf("ReviseDocxTotals: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("empty output")
	}
	t.Logf("revised bytes=%d (orig=%d)", len(out), len(src))
}
