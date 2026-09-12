package tui

import (
	"math"
	"strings"
	"testing"
)

func TestProgressBarBoundaries(t *testing.T) {
	for percent, want := range map[float64]int{0: 0, 50: 5, 100: 10, -1: 0, 101: 10} {
		bar := progressBar(percent)
		if strings.Count(bar, "█") != want || strings.Count(bar, "█")+strings.Count(bar, "░") != 10 {
			t.Fatal(percent, bar)
		}
	}
	if strings.Contains(progressBar(math.NaN()), "NaN") {
		t.Fatal("non-finite progress rendered")
	}
}
func TestTerminalTextCannotInjectControls(t *testing.T) {
	s := Clean("name\x1b]52;c;secret\a\n\r\u202e")
	if strings.ContainsAny(s, "\x1b\a\n\r\u202e") {
		t.Fatal(s)
	}
	if got := cell("下载文件", 5); got != "下载 " {
		t.Fatal(got)
	}
}
