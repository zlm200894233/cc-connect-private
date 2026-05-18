package core

import (
	"strings"
	"testing"
)

func TestTerminalScreenCapturesPlainText(t *testing.T) {
	screen := newTerminalScreen(20, 4)
	screen.ingest("hello world\nfoo bar\nthird")

	if got := strings.TrimRight(screen.lines()[0], " "); got != "hello world" {
		t.Fatalf("expected first line hello world, got %q", got)
	}
	if got := strings.TrimRight(screen.lines()[1], " "); got != "foo bar" {
		t.Fatalf("expected second line foo bar, got %q", got)
	}
	if got := strings.TrimRight(screen.lines()[2], " "); got != "third" {
		t.Fatalf("expected third line third, got %q", got)
	}
}

func TestTerminalScreenHandlesCarriageReturnRedraw(t *testing.T) {
	screen := newTerminalScreen(12, 2)
	screen.ingest("abcde\rxy")

	if got := strings.TrimRight(screen.lines()[0], " "); got != "xycde" {
		t.Fatalf("expected carriage return redraw to overwrite from start, got %q", got)
	}
}

func TestTerminalScreenHandlesClearScreenAndCursorHome(t *testing.T) {
	screen := newTerminalScreen(10, 3)
	screen.ingest("hello\nworld\nagain")
	screen.ingest("\x1b[2Jnext")

	if got := strings.TrimRight(screen.lines()[0], " "); got != "next" {
		t.Fatalf("expected first line next after clear screen, got %q", got)
	}
	if got := strings.TrimRight(screen.lines()[1], " "); got != "" {
		t.Fatalf("expected second line to be cleared, got %q", got)
	}
	if got := strings.TrimRight(screen.lines()[2], " "); got != "" {
		t.Fatalf("expected third line to be cleared, got %q", got)
	}
	if screen.cursorX != 4 || screen.cursorY != 0 {
		t.Fatalf("expected cursor home+advance after writing next, got (%d,%d)", screen.cursorX, screen.cursorY)
	}
}

func TestTerminalScreenKeepsTableColumns(t *testing.T) {
	screen := newTerminalScreen(24, 3)
	screen.ingest("Name   |   Age\nAlice  |   10\nBob    |    9")

	if got := strings.TrimRight(screen.lines()[0], " "); got != "Name   |   Age" {
		t.Fatalf("expected table header preserved, got %q", got)
	}
	if got := strings.TrimRight(screen.lines()[1], " "); got != "Alice  |   10" {
		t.Fatalf("expected table row spacing preserved, got %q", got)
	}
	if got := strings.TrimRight(screen.lines()[2], " "); got != "Bob    |    9" {
		t.Fatalf("expected table row spacing preserved, got %q", got)
	}
}

func TestTerminalScreenSkipsOSCSequences(t *testing.T) {
	screen := newTerminalScreen(20, 2)
	screen.ingest("pre\x1b]0;title\x07mid\x1b]0;other\x1b\\post")

	if got := strings.TrimRight(screen.lines()[0], " "); got != "premidpost" {
		t.Fatalf("expected OSC payloads skipped, got %q", got)
	}
}

func TestTerminalScreenIgnoresPrivateCSISequences(t *testing.T) {
	screen := newTerminalScreen(20, 2)
	screen.ingest("pre\x1b[?25hpost")

	if got := strings.TrimRight(screen.lines()[0], " "); got != "prepost" {
		t.Fatalf("expected private CSI ignored without corrupting text, got %q", got)
	}
}

func TestTerminalScreenRecoversFromGarbageCSISequence(t *testing.T) {
	screen := newTerminalScreen(20, 2)
	screen.ingest("pre\x1b[12345678901234567890123456789012345678901234567890123456789012345post")

	if got := strings.TrimRight(screen.lines()[0], " "); got != "prepost" {
		t.Fatalf("expected garbage CSI skipped and later text preserved, got %q", got)
	}
}

func TestTerminalScreenHandlesCursorMovement(t *testing.T) {
	screen := newTerminalScreen(12, 4)
	screen.ingest("abcd\x1b[2DXY")
	screen.ingest("\x1b[2Bdown")
	screen.ingest("\x1b[1Aup")
	screen.ingest("\x1b[10Cz")
	screen.ingest("\x1b[2;3Hh")
	screen.ingest("\x1b[3;4ff")

	if got := strings.TrimRight(screen.lines()[0], " "); got != "abXY" {
		t.Fatalf("expected C/D movement to preserve first line, got %q", got)
	}
	if got := screen.charAt(2, 1); got != 'h' {
		t.Fatalf("expected H to move cursor to row 2 column 3, got %q", got)
	}
	if got := screen.charAt(3, 2); got != 'f' {
		t.Fatalf("expected f to move cursor to row 3 column 4, got %q", got)
	}
	if got := screen.charAt(11, 1); got != 'z' {
		t.Fatalf("expected A/C movement to place z at row 2 last column, got %q", got)
	}
}

func TestTerminalScreenTabAdvancesToNextEightColumnStop(t *testing.T) {
	screen := newTerminalScreen(20, 2)
	screen.ingest("ab\tcd")

	if got := strings.TrimRight(screen.lines()[0], " "); got != "ab      cd" {
		t.Fatalf("expected tab to advance to next 8-column stop, got %q", got)
	}
}

func TestTerminalScreenTreatsCJKRunesAsWideCells(t *testing.T) {
	screen := newTerminalScreen(8, 2)
	screen.ingest("中A")

	if got := screen.charAt(0, 0); got != '中' {
		t.Fatalf("cell 0 = %q, want 中", got)
	}
	if got := screen.charAt(1, 0); got != ' ' {
		t.Fatalf("cell 1 = %q, want wide-character spacer", got)
	}
	if got := screen.charAt(2, 0); got != 'A' {
		t.Fatalf("cell 2 = %q, want A after wide CJK rune", got)
	}
	if screen.cursorX != 3 {
		t.Fatalf("cursorX = %d, want 3 after 中A", screen.cursorX)
	}
}

func TestTerminalScreenKeepsScrollbackAfterOverflow(t *testing.T) {
	screen := newTerminalScreen(8, 3)
	screen.ingest("line1\nline2\nline3\nline4")

	full := screen.fullLines()
	trimmed := make([]string, 0, len(full))
	for _, line := range full {
		trimmed = append(trimmed, strings.TrimRight(line, " "))
	}
	want := []string{"line1", "line2", "line3", "line4"}
	if strings.Join(trimmed, "|") != strings.Join(want, "|") {
		t.Fatalf("full lines = %#v, want %#v", trimmed, want)
	}
}

func TestTerminalScreenLinesRemainVisibleViewportOnly(t *testing.T) {
	screen := newTerminalScreen(8, 3)
	screen.ingest("line1\nline2\nline3\nline4")

	visible := screen.lines()
	trimmed := []string{
		strings.TrimRight(visible[0], " "),
		strings.TrimRight(visible[1], " "),
		strings.TrimRight(visible[2], " "),
	}
	want := []string{"line2", "line3", "line4"}
	if strings.Join(trimmed, "|") != strings.Join(want, "|") {
		t.Fatalf("visible lines = %#v, want %#v", trimmed, want)
	}
}

func TestTerminalScreenCloneCopiesScrollback(t *testing.T) {
	screen := newTerminalScreen(8, 3)
	screen.ingest("line1\nline2\nline3\nline4")
	clone := screen.clone()

	screen.clearScreen()
	if got := strings.TrimRight(clone.fullLines()[0], " "); got != "line1" {
		t.Fatalf("clone lost scrollback after source clear, got %q", got)
	}
}

func TestTerminalScreenClearScreenClearsScrollback(t *testing.T) {
	screen := newTerminalScreen(8, 3)
	screen.ingest("line1\nline2\nline3\nline4")
	screen.clearScreen()

	for _, line := range screen.fullLines() {
		if got := strings.TrimRight(line, " "); got != "" {
			t.Fatalf("expected clear screen to clear scrollback and viewport, got line %q", got)
		}
	}
}

func TestTerminalScreenScreenshotPagesAreNotCapped(t *testing.T) {
	screen := newTerminalScreen(8, 2)
	for i := 1; i <= 25; i++ {
		if i > 1 {
			screen.ingest("\n")
		}
		screen.ingest("x")
	}

	pages := screen.screenshotPages()
	if len(pages) != 13 {
		t.Fatalf("expected 13 pages for 25 lines at height 2, got %d", len(pages))
	}
}

func TestTerminalScreenScreenshotPagesOrderedOldestToNewest(t *testing.T) {
	screen := newTerminalScreen(8, 2)
	screen.ingest("line1\nline2\nline3\nline4")

	pages := screen.screenshotPages()
	if len(pages) != 2 {
		t.Fatalf("expected 2 pages, got %d", len(pages))
	}
	if got := strings.TrimRight(pages[0].lines()[0], " "); got != "line1" {
		t.Fatalf("first page first line = %q, want line1", got)
	}
	if got := strings.TrimRight(pages[1].lines()[1], " "); got != "line4" {
		t.Fatalf("second page second line = %q, want line4", got)
	}
}
