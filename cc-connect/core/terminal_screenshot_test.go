package core

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/image/font/gofont/gomono"
)

func TestRenderTerminalScreenshotProducesPNG(t *testing.T) {
	screen := newTerminalScreen(10, 4)
	screen.ingest("hello")

	attachment := RenderTerminalScreenshot(screen, "term_0001")
	if attachment.MimeType != "image/png" {
		t.Fatalf("expected MIME image/png, got %q", attachment.MimeType)
	}
	if attachment.FileName != "terminal-term_0001.png" {
		t.Fatalf("expected filename terminal-term_0001.png, got %q", attachment.FileName)
	}
	if len(attachment.Data) == 0 {
		t.Fatalf("expected non-empty PNG bytes")
	}

	img, err := png.Decode(bytes.NewReader(attachment.Data))
	if err != nil {
		t.Fatalf("decode PNG failed: %v", err)
	}

	bounds := img.Bounds()
	expectedW := screen.width*terminalScreenshotCellWidth + terminalScreenshotPadding*2
	expectedH := screen.height*terminalScreenshotCellHeight + terminalScreenshotPadding*2
	if bounds.Dx() != expectedW || bounds.Dy() != expectedH {
		t.Fatalf("unexpected screenshot dimensions: got %dx%d, want %dx%d", bounds.Dx(), bounds.Dy(), expectedW, expectedH)
	}

	hasForeground := false
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if !terminalScreenshotIsBackground(img.At(x, y)) {
				hasForeground = true
				break
			}
		}
		if hasForeground {
			break
		}
	}
	if !hasForeground {
		t.Fatalf("expected screenshot to contain non-background pixels")
	}
}

func TestRenderTerminalScreenshotIncludesVisibleText(t *testing.T) {
	screen := newTerminalScreen(6, 3)
	screen.ingest("A B\n  C")

	attachment := RenderTerminalScreenshot(screen, "term_visible")
	img, err := png.Decode(bytes.NewReader(attachment.Data))
	if err != nil {
		t.Fatalf("decode PNG failed: %v", err)
	}

	if !terminalScreenshotCellHasInk(img, 0, 0) {
		t.Fatalf("expected cell (0,0) containing A to render visible pixels")
	}
	if !terminalScreenshotCellHasInk(img, 2, 1) {
		t.Fatalf("expected cell (2,1) containing C to render visible pixels")
	}
	if terminalScreenshotCellHasInk(img, 1, 0) {
		t.Fatalf("expected space cell (1,0) to remain background")
	}
}

func TestRenderTerminalScreenshotDrawsDistinctGlyphShapes(t *testing.T) {
	screen := newTerminalScreen(3, 1)
	screen.ingest("AB")

	attachment := RenderTerminalScreenshot(screen, "term_glyphs")
	img, err := png.Decode(bytes.NewReader(attachment.Data))
	if err != nil {
		t.Fatalf("decode PNG failed: %v", err)
	}

	patternA := terminalScreenshotCellPattern(img, 0, 0)
	patternB := terminalScreenshotCellPattern(img, 1, 0)
	if bytes.Equal(patternA, patternB) {
		t.Fatalf("expected A and B cells to have distinct glyph shapes")
	}
}

func TestRenderTerminalScreenshotDrawsDistinctCJKGlyphShapes(t *testing.T) {
	screen := newTerminalScreen(6, 1)
	screen.ingest("中文")

	attachment := RenderTerminalScreenshot(screen, "term_cjk_glyphs")
	img, err := png.Decode(bytes.NewReader(attachment.Data))
	if err != nil {
		t.Fatalf("decode PNG failed: %v", err)
	}

	patternZhong := terminalScreenshotCellPattern(img, 0, 0)
	patternWen := terminalScreenshotCellPattern(img, 2, 0)
	if bytes.Equal(patternZhong, patternWen) {
		t.Fatalf("expected 中 and 文 to render as distinct glyph shapes, got identical missing-glyph cells")
	}
}

func TestRenderTerminalScreenshotBlankIDUsesUnknownFilename(t *testing.T) {
	attachment, err := renderTerminalScreenshot(newTerminalScreen(1, 1), "   ")
	if err != nil {
		t.Fatalf("render screenshot: %v", err)
	}
	if attachment.FileName != "terminal-unknown.png" {
		t.Fatalf("filename = %q, want terminal-unknown.png", attachment.FileName)
	}
	if attachment.MimeType != "image/png" {
		t.Fatalf("MIME = %q, want image/png", attachment.MimeType)
	}
}

func TestRenderTerminalScreenshotNilScreenRendersDefaultPNG(t *testing.T) {
	attachment, err := renderTerminalScreenshot(nil, "nil_screen")
	if err != nil {
		t.Fatalf("render screenshot: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(attachment.Data))
	if err != nil {
		t.Fatalf("decode PNG failed: %v", err)
	}

	bounds := img.Bounds()
	expectedW := defaultTerminalScreenWidth*terminalScreenshotCellWidth + terminalScreenshotPadding*2
	expectedH := defaultTerminalScreenHeight*terminalScreenshotCellHeight + terminalScreenshotPadding*2
	if bounds.Dx() != expectedW || bounds.Dy() != expectedH {
		t.Fatalf("unexpected nil-screen dimensions: got %dx%d, want %dx%d", bounds.Dx(), bounds.Dy(), expectedW, expectedH)
	}
}

func TestRenderTerminalScreenshotsTrimsBlankRows(t *testing.T) {
	screen := newTerminalScreen(10, 8)
	screen.ingest("\x1b[6;1Hbottom")

	attachments, err := renderTerminalScreenshots(screen, "term_trim")
	if err != nil {
		t.Fatalf("render screenshots: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("expected one screenshot, got %d", len(attachments))
	}
	img, err := png.Decode(bytes.NewReader(attachments[0].Data))
	if err != nil {
		t.Fatalf("decode PNG failed: %v", err)
	}
	wantH := terminalScreenshotCellHeight + terminalScreenshotPadding*2
	if img.Bounds().Dy() != wantH {
		t.Fatalf("screenshot height = %d, want trimmed one-row height %d", img.Bounds().Dy(), wantH)
	}
	if !terminalScreenshotCellHasInk(img, 0, 0) {
		t.Fatalf("expected trimmed screenshot to move bottom text to first rendered row")
	}
}

func TestRenderTerminalScreenshotsNoScrollbackReturnsOnePNG(t *testing.T) {
	screen := newTerminalScreen(8, 3)
	screen.ingest("hello")

	attachments, err := renderTerminalScreenshots(screen, "term_single")
	if err != nil {
		t.Fatalf("render screenshots: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("expected one screenshot, got %d", len(attachments))
	}
	if attachments[0].FileName != "terminal-term_single.png" {
		t.Fatalf("filename = %q, want terminal-term_single.png", attachments[0].FileName)
	}
}

func TestRenderTerminalScreenshotsWithScrollbackReturnsMultiplePNGs(t *testing.T) {
	screen := newTerminalScreen(8, 2)
	screen.ingest("line1\nline2\nline3\nline4")

	attachments, err := renderTerminalScreenshots(screen, "term_multi")
	if err != nil {
		t.Fatalf("render screenshots: %v", err)
	}
	if len(attachments) != 2 {
		t.Fatalf("expected two screenshots, got %d", len(attachments))
	}
	for i, attachment := range attachments {
		if attachment.MimeType != "image/png" {
			t.Fatalf("attachment %d MIME = %q, want image/png", i, attachment.MimeType)
		}
		if len(attachment.Data) == 0 {
			t.Fatalf("attachment %d has empty data", i)
		}
		if _, err := png.Decode(bytes.NewReader(attachment.Data)); err != nil {
			t.Fatalf("attachment %d decode PNG: %v", i, err)
		}
	}
}

func TestRenderTerminalScreenshotsUsesPageFilenamesForMultiplePages(t *testing.T) {
	screen := newTerminalScreen(8, 2)
	screen.ingest("line1\nline2\nline3\nline4")

	attachments, err := renderTerminalScreenshots(screen, "term_pages")
	if err != nil {
		t.Fatalf("render screenshots: %v", err)
	}
	want := []string{"terminal-term_pages-01.png", "terminal-term_pages-02.png"}
	for i, attachment := range attachments {
		if attachment.FileName != want[i] {
			t.Fatalf("attachment %d filename = %q, want %q", i, attachment.FileName, want[i])
		}
	}
}

func TestRenderTerminalScreenshotsNilScreenReturnsOnePNG(t *testing.T) {
	attachments, err := renderTerminalScreenshots(nil, "nil_multi")
	if err != nil {
		t.Fatalf("render screenshots: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("expected one nil-screen screenshot, got %d", len(attachments))
	}
	if attachments[0].FileName != "terminal-nil_multi.png" {
		t.Fatalf("filename = %q, want terminal-nil_multi.png", attachments[0].FileName)
	}
}

func TestLoadTerminalScreenshotFontFileAcceptsTTF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gomono.ttf")
	if err := os.WriteFile(path, gomono.TTF, 0o600); err != nil {
		t.Fatalf("write temp font: %v", err)
	}

	fonts := loadTerminalScreenshotFontFile(path)
	if len(fonts) != 1 {
		t.Fatalf("loaded fonts = %d, want one TTF font", len(fonts))
	}
}

func TestTerminalScreenshotRuneFallbacksAreVisibleASCII(t *testing.T) {
	cases := map[rune]rune{
		'⎿': '└',
		'❯': '>',
		'✻': '*',
	}
	for input, want := range cases {
		if got, ok := terminalScreenshotRuneFallback(input); !ok || got != want {
			t.Fatalf("fallback for %q = (%q, %v), want (%q, true)", input, got, ok, want)
		}
	}
}

func TestRenderTerminalScreenshotCJKRuneRendersInk(t *testing.T) {
	screen := newTerminalScreen(4, 2)
	screen.ingest("中")

	attachment, err := renderTerminalScreenshot(screen, "cjk")
	if err != nil {
		t.Fatalf("render screenshot: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(attachment.Data))
	if err != nil {
		t.Fatalf("decode PNG failed: %v", err)
	}
	if !terminalScreenshotCellHasInk(img, 0, 0) {
		t.Fatalf("expected CJK/replacement glyph to render ink in cell (0,0)")
	}
}

func terminalScreenshotCellHasInk(img image.Image, x, y int) bool {
	x0 := terminalScreenshotPadding + x*terminalScreenshotCellWidth
	y0 := terminalScreenshotPadding + y*terminalScreenshotCellHeight
	for py := y0; py < y0+terminalScreenshotCellHeight; py++ {
		for px := x0; px < x0+terminalScreenshotCellWidth; px++ {
			if !terminalScreenshotIsBackground(img.At(px, py)) {
				return true
			}
		}
	}
	return false
}

func terminalScreenshotCellPattern(img image.Image, x, y int) []byte {
	pattern := make([]byte, 0, terminalScreenshotCellWidth*terminalScreenshotCellHeight*4)
	x0 := terminalScreenshotPadding + x*terminalScreenshotCellWidth
	y0 := terminalScreenshotPadding + y*terminalScreenshotCellHeight
	for py := y0; py < y0+terminalScreenshotCellHeight; py++ {
		for px := x0; px < x0+terminalScreenshotCellWidth; px++ {
			rgba := color.RGBAModel.Convert(img.At(px, py)).(color.RGBA)
			pattern = append(pattern, rgba.R, rgba.G, rgba.B, rgba.A)
		}
	}
	return pattern
}

func terminalScreenshotIsBackground(c color.Color) bool {
	rgba := color.RGBAModel.Convert(c).(color.RGBA)
	return rgba.R == terminalScreenshotBackgroundColor.R && rgba.G == terminalScreenshotBackgroundColor.G && rgba.B == terminalScreenshotBackgroundColor.B && rgba.A == terminalScreenshotBackgroundColor.A
}
