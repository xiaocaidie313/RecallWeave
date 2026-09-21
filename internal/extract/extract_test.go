package extract

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTitleOf(t *testing.T) {
	if got := titleOf("第一行\n第二行"); got != "第一行" {
		t.Fatalf("expected first line as title, got %q", got)
	}

	if got := titleOf("  带空格的一行  "); got != "带空格的一行" {
		t.Fatalf("expected trimmed title, got %q", got)
	}

	long := strings.Repeat("记", maxTitleLength+10)
	if got := utf8.RuneCountInString(titleOf(long)); got != maxTitleLength {
		t.Fatalf("expected title truncated to %d runes, got %d", maxTitleLength, got)
	}
}
