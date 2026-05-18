package core

import "strings"

const (
	defaultTerminalScreenWidth  = 120
	defaultTerminalScreenHeight = 40
)

type terminalEscapeState uint8

const (
	escapeStateText terminalEscapeState = iota
	escapeStateEscape
	escapeStateCSI
	escapeStateOSC
	escapeStateOSCEscape
)

type terminalScreen struct {
	width       int
	height      int
	cursorX     int
	cursorY     int
	cells       []rune
	scrollback  []string
	escapeState terminalEscapeState
	csiBuffer   []rune
}

func newTerminalScreen(width, height int) *terminalScreen {
	if width <= 0 {
		width = defaultTerminalScreenWidth
	}
	if height <= 0 {
		height = defaultTerminalScreenHeight
	}

	cells := make([]rune, width*height)
	for i := range cells {
		cells[i] = ' '
	}

	return &terminalScreen{
		width:  width,
		height: height,
		cells:  cells,
	}
}

func (s *terminalScreen) ingest(content string) {
	for _, r := range content {
		s.processRune(r)
	}
}

func (s *terminalScreen) processRune(r rune) {
	switch s.escapeState {
	case escapeStateEscape:
		s.handleAfterEscape(r)
		return
	case escapeStateCSI:
		s.handleCSI(r)
		return
	case escapeStateOSC:
		s.handleOSC(r)
		return
	case escapeStateOSCEscape:
		s.handleOSCEscape(r)
		return
	}

	if r == '\x1b' {
		s.escapeState = escapeStateEscape
		return
	}

	s.handleTextRune(r)
}

func (s *terminalScreen) handleAfterEscape(r rune) {
	s.escapeState = escapeStateText
	switch r {
	case '[':
		s.escapeState = escapeStateCSI
		s.csiBuffer = s.csiBuffer[:0]
	case ']':
		s.escapeState = escapeStateOSC
	}
	// Ignore unsupported single-escape sequences safely.
}

func (s *terminalScreen) handleOSC(r rune) {
	switch r {
	case '\a':
		s.escapeState = escapeStateText
	case '\x1b':
		s.escapeState = escapeStateOSCEscape
	}
}

func (s *terminalScreen) handleOSCEscape(r rune) {
	if r == '\\' {
		s.escapeState = escapeStateText
		return
	}
	s.escapeState = escapeStateOSC
}

func (s *terminalScreen) handleCSI(r rune) {
	// ANSI CSI final bytes are ASCII 0x40..0x7e.
	if r >= 0x40 && r <= 0x7e {
		s.applyCSISequence(string(append(s.csiBuffer, r)))
		s.escapeState = escapeStateText
		s.csiBuffer = nil
		return
	}

	s.csiBuffer = append(s.csiBuffer, r)
	if len(s.csiBuffer) > 64 {
		// Guard against runaway garbage/unsupported sequences.
		s.escapeState = escapeStateText
		s.csiBuffer = nil
	}
}

func (s *terminalScreen) handleTextRune(r rune) {
	switch r {
	case '\r':
		s.cursorX = 0
		return
	case '\n':
		s.newLine()
		return
	case '\b':
		if s.cursorX > 0 {
			s.cursorX--
		}
		return
	case '\t':
		s.writeTab()
		return
	}

	if r < 0x20 || r == 0x7f {
		return
	}
	s.writeRune(r)
}

func (s *terminalScreen) writeTab() {
	spaces := 8 - (s.cursorX % 8)
	if spaces == 0 {
		spaces = 8
	}
	for i := 0; i < spaces; i++ {
		s.writeRune(' ')
	}
}

func (s *terminalScreen) writeRune(r rune) {
	if s.width <= 0 || s.height <= 0 {
		return
	}
	if s.cursorX < 0 {
		s.cursorX = 0
	}
	if s.cursorY < 0 {
		s.cursorY = 0
	}
	if s.cursorY >= s.height {
		s.scrollUpToBottom()
	}

	cellWidth := terminalRuneWidth(r)
	if cellWidth == 2 && s.cursorX == s.width-1 {
		s.newLine()
	}

	if s.cursorY >= 0 && s.cursorY < s.height && s.cursorX >= 0 && s.cursorX < s.width {
		index := s.cursorY*s.width + s.cursorX
		s.cells[index] = r
		if cellWidth == 2 && s.cursorX+1 < s.width {
			s.cells[index+1] = ' '
		}
	}

	s.cursorX += cellWidth
	if s.cursorX >= s.width {
		s.newLine()
	}
}

func terminalRuneWidth(r rune) int {
	if isWideTerminalRune(r) {
		return 2
	}
	return 1
}

func isWideTerminalRune(r rune) bool {
	return r >= 0x1100 && (r <= 0x115f ||
		r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1f64f) ||
		(r >= 0x1f900 && r <= 0x1f9ff))
}

func (s *terminalScreen) applyCSISequence(seq string) {
	rawParams := seq[:len(seq)-1]
	cmd := seq[len(seq)-1]
	params := parseANSICSIParams(rawParams)

	switch cmd {
	case 'A':
		lineOffset := atLeast1(params)
		s.cursorY -= lineOffset
	case 'B':
		lineOffset := atLeast1(params)
		s.cursorY += lineOffset
	case 'C':
		offset := atLeast1(params)
		s.cursorX += offset
	case 'D':
		offset := atLeast1(params)
		s.cursorX -= offset
	case 'H', 'f':
		targetRow := 1
		targetCol := 1
		if len(params) > 0 {
			targetRow = params[0]
		}
		if len(params) > 1 {
			targetCol = params[1]
		}
		if targetRow < 1 {
			targetRow = 1
		}
		if targetCol < 1 {
			targetCol = 1
		}
		s.cursorY = targetRow - 1
		s.cursorX = targetCol - 1
	case 'J':
		// Support all common clear-screen forms in a simple form for now.
		s.clearScreen()
	case 'K':
		clearMode := 0
		if len(params) > 0 {
			clearMode = params[0]
		}
		s.clearLine(clearMode)
	}

	s.clampCursor()
}

func (s *terminalScreen) clampCursor() {
	if s.width <= 0 || s.height <= 0 {
		s.cursorX = 0
		s.cursorY = 0
		return
	}
	if s.cursorX < 0 {
		s.cursorX = 0
	}
	if s.cursorX >= s.width {
		s.cursorX = s.width - 1
	}
	if s.cursorY < 0 {
		s.cursorY = 0
	}
	if s.cursorY >= s.height {
		s.cursorY = s.height - 1
	}
}

func (s *terminalScreen) newLine() {
	s.cursorY++
	s.cursorX = 0
	if s.cursorY >= s.height {
		s.scrollUpToBottom()
		s.cursorY = s.height - 1
	}
}

func (s *terminalScreen) scrollUpToBottom() {
	if s.height <= 1 || s.width <= 0 {
		s.appendScrollbackLine(s.lineString(0))
		for i := range s.cells {
			s.cells[i] = ' '
		}
		return
	}
	s.appendScrollbackLine(s.lineString(0))
	copy(s.cells, s.cells[s.width:])
	for i := (s.height - 1) * s.width; i < len(s.cells); i++ {
		s.cells[i] = ' '
	}
}

func (s *terminalScreen) appendScrollbackLine(line string) {
	if s.height <= 0 {
		return
	}
	s.scrollback = append(s.scrollback, line)
}

func (s *terminalScreen) lineString(y int) string {
	if y < 0 || y >= s.height || s.width <= 0 {
		return ""
	}
	start := y * s.width
	end := start + s.width
	return string(s.cells[start:end])
}

func (s *terminalScreen) clearLine(mode int) {
	switch mode {
	case 1:
		for x := 0; x <= s.cursorX && x < s.width; x++ {
			s.cells[s.cursorY*s.width+x] = ' '
		}
	case 2:
		for x := 0; x < s.width; x++ {
			s.cells[s.cursorY*s.width+x] = ' '
		}
	default:
		for x := s.cursorX; x < s.width; x++ {
			s.cells[s.cursorY*s.width+x] = ' '
		}
	}
}

func (s *terminalScreen) clearScreen() {
	for i := range s.cells {
		s.cells[i] = ' '
	}
	s.scrollback = nil
	s.cursorX = 0
	s.cursorY = 0
}

func parseANSICSIParams(raw string) []int {
	if strings.TrimSpace(raw) == "" {
		return []int{}
	}
	parts := strings.Split(raw, ";")
	params := make([]int, 0, len(parts))
	for _, part := range parts {
		v, ok := parseInt(part)
		if ok {
			params = append(params, v)
		}
	}
	return params
}

func parseInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	value := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		value = value*10 + int(s[i]-'0')
	}
	return value, true
}

func atLeast1(params []int) int {
	if len(params) == 0 || params[0] <= 0 {
		return 1
	}
	return params[0]
}

func (s *terminalScreen) lines() []string {
	out := make([]string, s.height)
	for y := 0; y < s.height; y++ {
		out[y] = s.lineString(y)
	}
	return out
}

func (s *terminalScreen) fullLines() []string {
	out := make([]string, 0, len(s.scrollback)+s.height)
	out = append(out, s.scrollback...)
	out = append(out, s.lines()...)
	return out
}

func (s *terminalScreen) screenshotPages() []*terminalScreen {
	if s == nil {
		return []*terminalScreen{newTerminalScreen(0, 0)}
	}
	lines := trimBlankTerminalLines(s.fullLines())
	if len(lines) == 0 {
		return []*terminalScreen{newTerminalScreen(s.width, 1)}
	}
	pageHeight := s.height
	if pageHeight <= 0 {
		pageHeight = defaultTerminalScreenHeight
	}
	pages := make([]*terminalScreen, 0, (len(lines)+pageHeight-1)/pageHeight)
	for start := 0; start < len(lines); start += pageHeight {
		end := start + pageHeight
		if end > len(lines) {
			end = len(lines)
		}
		page := newTerminalScreen(s.width, end-start)
		for y, line := range lines[start:end] {
			page.setLine(y, line)
		}
		pages = append(pages, page)
	}
	return pages
}

func trimBlankTerminalLines(lines []string) []string {
	start := 0
	for start < len(lines) && strings.TrimRight(lines[start], " ") == "" {
		start++
	}
	end := len(lines)
	for end > start && strings.TrimRight(lines[end-1], " ") == "" {
		end--
	}
	return lines[start:end]
}

func (s *terminalScreen) setLine(y int, line string) {
	if y < 0 || y >= s.height || s.width <= 0 {
		return
	}
	start := y * s.width
	end := start + s.width
	for i := start; i < end; i++ {
		s.cells[i] = ' '
	}
	for x, r := range []rune(line) {
		if x >= s.width {
			break
		}
		s.cells[start+x] = r
	}
}

func (s *terminalScreen) text() string {
	return strings.Join(s.lines(), "\n")
}

func (s *terminalScreen) clone() *terminalScreen {
	if s == nil {
		return nil
	}
	cp := &terminalScreen{
		width:       s.width,
		height:      s.height,
		cursorX:     s.cursorX,
		cursorY:     s.cursorY,
		escapeState: s.escapeState,
	}
	cp.cells = append([]rune(nil), s.cells...)
	cp.scrollback = append([]string(nil), s.scrollback...)
	cp.csiBuffer = append([]rune(nil), s.csiBuffer...)
	return cp
}

func (s *terminalScreen) charAt(x, y int) rune {
	if x < 0 || y < 0 || x >= s.width || y >= s.height {
		return 0
	}
	return s.cells[y*s.width+x]
}
