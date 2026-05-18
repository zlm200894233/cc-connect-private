package core

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

const (
	terminalScreenshotCellWidth  = 10
	terminalScreenshotCellHeight = 20
	terminalScreenshotPadding    = 12
	terminalScreenshotFontSize   = 14
	terminalScreenshotFontDPI    = 72
)

var (
	terminalScreenshotBackgroundColor = color.RGBA{R: 0x11, G: 0x15, B: 0x20, A: 0xff}
	terminalScreenshotForegroundColor = color.RGBA{R: 0xe5, G: 0xe7, B: 0xeb, A: 0xff}
	terminalScreenshotFontOnce        sync.Once
	terminalScreenshotFonts           []*opentype.Font
	terminalScreenshotFontErr         error
	terminalScreenshotRenderer        = renderTerminalScreenshot
	terminalScreenshotsRenderer       = renderTerminalScreenshots
)

type terminalScreenshotFontFace struct {
	font *opentype.Font
	face font.Face
}

// RenderTerminalScreenshot converts the current terminal screen to a deterministic PNG image attachment.
func RenderTerminalScreenshot(screen *terminalScreen, terminalID string) ImageAttachment {
	attachment, err := renderTerminalScreenshot(screen, terminalID)
	if err == nil {
		return attachment
	}
	slog.Error("terminal screenshot: render failed", "terminal_id", strings.TrimSpace(terminalID), "error", err)
	return terminalScreenshotAttachment(nil, terminalID)
}

func renderTerminalScreenshots(screen *terminalScreen, terminalID string) ([]ImageAttachment, error) {
	if screen == nil {
		attachment, err := renderTerminalScreenshot(nil, terminalID)
		if err != nil {
			return nil, err
		}
		return []ImageAttachment{attachment}, nil
	}

	pages := screen.screenshotPages()
	attachments := make([]ImageAttachment, 0, len(pages))
	multiPage := len(pages) > 1
	for i, page := range pages {
		attachment, err := renderTerminalScreenshot(page, terminalID)
		if err != nil {
			return nil, err
		}
		if multiPage {
			attachment.FileName = terminalScreenshotPageFileName(terminalID, i+1)
		}
		attachments = append(attachments, attachment)
	}
	return attachments, nil
}

func terminalScreenshotPageFileName(terminalID string, page int) string {
	terminalID = strings.TrimSpace(terminalID)
	if terminalID == "" {
		terminalID = "unknown"
	}
	return fmt.Sprintf("terminal-%s-%02d.png", terminalID, page)
}

func renderTerminalScreenshot(screen *terminalScreen, terminalID string) (ImageAttachment, error) {
	if screen == nil {
		screen = newTerminalScreen(0, 0)
	}

	imgW := screen.width*terminalScreenshotCellWidth + terminalScreenshotPadding*2
	imgH := screen.height*terminalScreenshotCellHeight + terminalScreenshotPadding*2
	if imgW <= 0 {
		imgW = terminalScreenshotPadding * 2
	}
	if imgH <= 0 {
		imgH = terminalScreenshotPadding * 2
	}

	img := image.NewRGBA(image.Rect(0, 0, imgW, imgH))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: terminalScreenshotBackgroundColor}, image.Point{}, draw.Src)
	if err := drawTerminalScreenGlyphs(img, screen); err != nil {
		return terminalScreenshotAttachment(nil, terminalID), err
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return terminalScreenshotAttachment(nil, terminalID), fmt.Errorf("encode terminal screenshot PNG: %w", err)
	}

	return terminalScreenshotAttachment(buf.Bytes(), terminalID), nil
}

func terminalScreenshotAttachment(data []byte, terminalID string) ImageAttachment {
	terminalID = strings.TrimSpace(terminalID)
	if terminalID == "" {
		terminalID = "unknown"
	}
	return ImageAttachment{
		MimeType: "image/png",
		FileName: "terminal-" + terminalID + ".png",
		Data:     data,
	}
}

func drawTerminalScreenGlyphs(img draw.Image, screen *terminalScreen) error {
	faces, err := terminalScreenshotFontFaces()
	if err != nil {
		return err
	}
	defer func() {
		for _, face := range faces {
			face.face.Close()
		}
	}()

	metrics := faces[0].face.Metrics()
	ascent := metrics.Ascent.Ceil()
	descent := metrics.Descent.Ceil()
	baselineOffset := (terminalScreenshotCellHeight + ascent - descent) / 2
	drawer := &font.Drawer{
		Dst: img,
		Src: image.NewUniform(terminalScreenshotForegroundColor),
	}
	var glyphBuf sfnt.Buffer

	for y := 0; y < screen.height; y++ {
		for x := 0; x < screen.width; x++ {
			r := screen.charAt(x, y)
			if r == 0 || r == ' ' {
				continue
			}
			x0 := terminalScreenshotPadding + x*terminalScreenshotCellWidth
			y0 := terminalScreenshotPadding + y*terminalScreenshotCellHeight
			drawRune, face := terminalScreenshotDrawableRune(faces, &glyphBuf, r)
			drawer.Face = face
			drawer.Dot = fixed.P(x0+1, y0+baselineOffset)
			drawer.DrawString(string(drawRune))
		}
	}
	return nil
}

func terminalScreenshotFontFaces() ([]terminalScreenshotFontFace, error) {
	fonts, err := terminalScreenshotLoadedFonts()
	if err != nil {
		return nil, err
	}
	faces := make([]terminalScreenshotFontFace, 0, len(fonts))
	for _, f := range fonts {
		face, err := opentype.NewFace(f, &opentype.FaceOptions{
			Size:    terminalScreenshotFontSize,
			DPI:     terminalScreenshotFontDPI,
			Hinting: font.HintingFull,
		})
		if err != nil {
			continue
		}
		faces = append(faces, terminalScreenshotFontFace{font: f, face: face})
	}
	if len(faces) == 0 {
		return nil, fmt.Errorf("create terminal screenshot font face: no usable fonts")
	}
	return faces, nil
}

func terminalScreenshotLoadedFonts() ([]*opentype.Font, error) {
	terminalScreenshotFontOnce.Do(func() {
		primary, err := opentype.Parse(gomono.TTF)
		if err != nil {
			terminalScreenshotFontErr = err
			return
		}
		terminalScreenshotFonts = append(terminalScreenshotFonts, primary)
		terminalScreenshotFonts = append(terminalScreenshotFonts, loadTerminalScreenshotFallbackFonts()...)
	})
	if terminalScreenshotFontErr != nil {
		return nil, fmt.Errorf("parse terminal screenshot font: %w", terminalScreenshotFontErr)
	}
	return terminalScreenshotFonts, nil
}

func terminalScreenshotFaceForRune(faces []terminalScreenshotFontFace, buf *sfnt.Buffer, r rune) font.Face {
	if face := terminalScreenshotFontFaceForRune(faces, buf, r); face != nil {
		return face
	}
	return faces[0].face
}

func terminalScreenshotDrawableRune(faces []terminalScreenshotFontFace, buf *sfnt.Buffer, r rune) (rune, font.Face) {
	if face := terminalScreenshotFontFaceForRune(faces, buf, r); face != nil {
		return r, face
	}
	if fallback, ok := terminalScreenshotRuneFallback(r); ok {
		return fallback, terminalScreenshotFaceForRune(faces, buf, fallback)
	}
	return '?', faces[0].face
}

func terminalScreenshotFontFaceForRune(faces []terminalScreenshotFontFace, buf *sfnt.Buffer, r rune) font.Face {
	for _, face := range faces {
		index, err := face.font.GlyphIndex(buf, r)
		if err == nil && index != 0 {
			return face.face
		}
	}
	return nil
}

func terminalScreenshotRuneFallback(r rune) (rune, bool) {
	switch r {
	case '⎿':
		return '└', true
	case '❯', '›':
		return '>', true
	case '✻':
		return '*', true
	case '●':
		return '*', true
	default:
		return 0, false
	}
}

func loadTerminalScreenshotFallbackFonts() []*opentype.Font {
	paths := terminalScreenshotFallbackFontPaths()
	fonts := make([]*opentype.Font, 0, len(paths))
	for _, path := range paths {
		fonts = append(fonts, loadTerminalScreenshotFontFile(path)...)
	}
	return fonts
}

func loadTerminalScreenshotFontFile(path string) []*opentype.Font {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if f, err := opentype.Parse(data); err == nil {
		return []*opentype.Font{f}
	}
	collection, err := opentype.ParseCollection(data)
	if err != nil {
		return nil
	}
	fonts := make([]*opentype.Font, 0, collection.NumFonts())
	for i := 0; i < collection.NumFonts(); i++ {
		f, err := collection.Font(i)
		if err == nil {
			fonts = append(fonts, f)
		}
	}
	return fonts
}

func terminalScreenshotFallbackFontPaths() []string {
	switch runtime.GOOS {
	case "windows":
		windir := os.Getenv("WINDIR")
		if strings.TrimSpace(windir) == "" {
			windir = `C:\Windows`
		}
		fontsDir := filepath.Join(windir, "Fonts")
		return []string{
			filepath.Join(fontsDir, "CascadiaMono.ttf"),
			filepath.Join(fontsDir, "CascadiaCode.ttf"),
			filepath.Join(fontsDir, "consola.ttf"),
			filepath.Join(fontsDir, "seguisym.ttf"),
			filepath.Join(fontsDir, "seguiemj.ttf"),
			filepath.Join(fontsDir, "msyh.ttc"),
			filepath.Join(fontsDir, "msyhbd.ttc"),
			filepath.Join(fontsDir, "simsun.ttc"),
			filepath.Join(fontsDir, "msyh.ttf"),
			filepath.Join(fontsDir, "simhei.ttf"),
			filepath.Join(fontsDir, "msjh.ttc"),
		}
	case "darwin":
		return []string{
			"/System/Library/Fonts/PingFang.ttc",
			"/System/Library/Fonts/STHeiti Light.ttc",
			"/System/Library/Fonts/STHeiti Medium.ttc",
		}
	default:
		return []string{
			"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
			"/usr/share/fonts/truetype/noto/NotoSansCJK-Regular.ttc",
			"/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
			"/usr/share/fonts/truetype/arphic/uming.ttc",
		}
	}
}
