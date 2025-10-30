package main

import (
    "bytes"
    "fmt"
    "io"
    "os"
    "os/exec"
    "os/user"
    "regexp"
    "strings"
    "time"

    "github.com/gdamore/tcell/v2"
    "golang.org/x/text/encoding/charmap"
    "golang.org/x/text/transform"
)

type Terminal struct {
	screen        tcell.Screen
	inputBuffer   []rune
	cursorPos     int
	cursorVisible bool
	lastBlink     time.Time
	outputLines   []LineSegment // Храним вывод команд с цветами
	history       []string      // История команд
	historyPos    int           // Позиция в истории
}

// LineSegment представляет сегмент текста с определенным стилем
type LineSegment struct {
	Text  string
	Style tcell.Style
}

// ANSI цвета для преобразования
var ansiColors = map[int]tcell.Color{
	30: tcell.ColorBlack,       // black
	31: tcell.ColorRed,         // red
	32: tcell.ColorGreen,       // green
	33: tcell.ColorYellow,      // yellow
	34: tcell.ColorBlue,        // blue
	35: tcell.ColorDarkMagenta, // magenta
	36: tcell.ColorTeal,        // cyan
	37: tcell.ColorWhite,       // white
	90: tcell.ColorGray,        // bright black
	91: tcell.ColorRed,         // bright red
	92: tcell.ColorGreen,       // bright green
	93: tcell.ColorYellow,      // bright yellow
	94: tcell.ColorBlue,        // bright blue
	95: tcell.ColorDarkMagenta, // bright magenta
	96: tcell.ColorTeal,        // bright cyan
	97: tcell.ColorWhite,       // bright white
}

var ansiBgColors = map[int]tcell.Color{
	40:  tcell.ColorBlack,
	41:  tcell.ColorRed,
	42:  tcell.ColorGreen,
	43:  tcell.ColorYellow,
	44:  tcell.ColorBlue,
	45:  tcell.ColorDarkMagenta,
	46:  tcell.ColorTeal,
	47:  tcell.ColorWhite,
	100: tcell.ColorGray,
	101: tcell.ColorRed,
	102: tcell.ColorGreen,
	103: tcell.ColorYellow,
	104: tcell.ColorBlue,
	105: tcell.ColorDarkMagenta,
	106: tcell.ColorTeal,
	107: tcell.ColorWhite,
}

func main() {
	os.Setenv("LANG", "en_US.UTF-8")
	os.Setenv("LC_ALL", "en_US.UTF-8")
	// Инициализация экрана
	s, err := tcell.NewScreen()
	if err != nil {
		panic(err)
	}
	if err := s.Init(); err != nil {
		panic(err)
	}
	defer s.Fini()

	// Создаем терминал
	term := &Terminal{
		screen:        s,
		inputBuffer:   make([]rune, 0),
		cursorPos:     0,
		cursorVisible: true,
		lastBlink:     time.Now(),
		outputLines:   []LineSegment{},
		history:       []string{},
		historyPos:    0,
	}

	// Устанавливаем темный стиль
	defStyle := tcell.StyleDefault.
		Foreground(tcell.ColorWhite).
		Background(tcell.ColorDefault)
	s.SetStyle(defStyle)
	s.Clear()

	// Главный цикл
	for {
		// Обновляем мигание курсора
		term.updateCursorBlink()

		// Рисуем состояние
		term.draw()

		// Показываем изменения
		s.Show()

		// Обработка событий с таймаутом для плавного мигания
		select {
		case <-time.After(50 * time.Millisecond):
			continue
		default:
		}

		// Обработка событий ввода
		if s.HasPendingEvent() {
			ev := s.PollEvent()
			switch ev := ev.(type) {
			case *tcell.EventResize:
				s.Sync()
			case *tcell.EventKey:
				term.handleKeyEvent(ev)
			}
		}
	}
}
func decodeWindows1251(data []byte) string {
    // Пробуем декодировать из Windows-1251 (часто используется в Windows)
    reader := transform.NewReader(bytes.NewReader(data), charmap.Windows1251.NewDecoder())
    decoded, err := io.ReadAll(reader)
    if err != nil {
        // Если не получается, возвращаем как есть
        return string(data)
    }
    return string(decoded)
}
func (t *Terminal) updateCursorBlink() {
	if time.Since(t.lastBlink) > 500*time.Millisecond {
		t.cursorVisible = !t.cursorVisible
		t.lastBlink = time.Now()
	}
}

func (t *Terminal) draw() {
    width, height := t.screen.Size()

    offsetX := width / 4
    offsetY := height / 4
    termWidth := width - 2*offsetX
    termHeight := height - 2*offsetY

    t.screen.Clear()
    t.drawTerminalArea(offsetX, offsetY, termWidth, termHeight)

    inputY := offsetY + 1
    inputLine := "> " + string(t.inputBuffer)

    t.drawOutput(offsetX, inputY+1, termWidth, termHeight-2)

    // Рисуем текст
    t.drawText(offsetX, inputY, inputLine, tcell.StyleDefault.
        Foreground(tcell.ColorWhite).
        Background(tcell.ColorDefault))

    // Курсор - правильное вычисление позиции для кириллицы
    prefix := "> "
    cursorX := offsetX + len([]rune(prefix)) + t.cursorPos // Используем руны для префикса
    
    if t.cursorVisible {
        t.drawCursor(cursorX, inputY)
    }
}

func (t *Terminal) drawTerminalArea(x, y, width, height int) {
	style := tcell.StyleDefault.
		Foreground(tcell.ColorWhite).
		Background(tcell.ColorDefault)

	for i := 0; i < width; i++ {
		for j := 0; j < height; j++ {
			t.screen.SetContent(x+i, y+j, ' ', nil, style)
		}
	}
}

func (t *Terminal) drawOutput(offsetX, offsetY, width, height int) {
	availableHeight := height
	currentY := offsetY

	for i := 0; i < len(t.outputLines) && currentY < offsetY+availableHeight; i++ {
		segment := t.outputLines[i]
		text := segment.Text
		runes := []rune(text)
		
		// Если строка пустая, просто переходим на следующую строку
		if len(runes) == 0 {
			currentY++
			continue
		}

		// Разбиваем длинные строки на несколько строк
		for len(runes) > 0 && currentY < offsetY+availableHeight {
			// Берем столько символов, сколько влезает в ширину
			take := min(len(runes), width)
			line := string(runes[:take])
			t.drawText(offsetX, currentY, line, segment.Style)
			
			// Убираем обработанную часть
			runes = runes[take:]
			currentY++
		}
	}
}

// Вспомогательная функция для min
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (t *Terminal) drawText(x, y int, text string, style tcell.Style) {
    runes := []rune(text) // Правильно преобразуем в руны
    for i, r := range runes {
        t.screen.SetContent(x+i, y, r, nil, style)
    }
}

func (t *Terminal) drawCursor(x, y int) {
    style := tcell.StyleDefault.
        Foreground(tcell.ColorBlack).
        Background(tcell.ColorWhite)
    // Используем пробел для курсора вместо символа
    t.screen.SetContent(x, y, ' ', nil, style)
}

// parseANSI преобразует строку с ANSI кодами в сегменты с правильными стилями
func parseANSI(text string, baseStyle tcell.Style) []LineSegment {
	segments := []LineSegment{}
	currentStyle := baseStyle

	// Регулярное выражение для поиска ANSI escape последовательностей
	re := regexp.MustCompile(`\033\[([\d;]*)m`)
	matches := re.FindAllStringSubmatchIndex(text, -1)

	if len(matches) == 0 {
		// Нет ANSI кодов - возвращаем весь текст как один сегмент
		return []LineSegment{{Text: text, Style: baseStyle}}
	}

	lastIndex := 0
	for _, match := range matches {
		// Добавляем текст до ANSI кода
		if match[0] > lastIndex {
			segments = append(segments, LineSegment{
				Text:  text[lastIndex:match[0]],
				Style: currentStyle,
			})
		}

		// Обрабатываем ANSI код
		codeStr := text[match[2]:match[3]]
		if codeStr == "" {
			// Reset
			currentStyle = baseStyle
		} else {
			codes := parseANSICodes(codeStr)
			currentStyle = applyANSICodes(codes, baseStyle)
		}

		lastIndex = match[1]
	}

	// Добавляем оставшийся текст
	if lastIndex < len(text) {
		segments = append(segments, LineSegment{
			Text:  text[lastIndex:],
			Style: currentStyle,
		})
	}

	return segments
}

func parseANSICodes(codeStr string) []int {
	parts := strings.Split(codeStr, ";")
	codes := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			codes = append(codes, 0)
		} else {
			var code int
			fmt.Sscanf(part, "%d", &code)
			codes = append(codes, code)
		}
	}
	return codes
}

func applyANSICodes(codes []int, baseStyle tcell.Style) tcell.Style {
	style := baseStyle
	fgColor := tcell.ColorDefault
	bgColor := tcell.ColorDefault
	bold := false
	underline := false
	italic := false

	i := 0
	for i < len(codes) {
		code := codes[i]
		switch {
		case code == 0:
			// Reset
			style = baseStyle
			fgColor = tcell.ColorDefault
			bgColor = tcell.ColorDefault
			bold = false
			underline = false
			italic = false

		case code == 1:
			bold = true
		case code == 3:
			italic = true
		case code == 4:
			underline = true
		case code == 22:
			bold = false
		case code == 23:
			italic = false
		case code == 24:
			underline = false

		case code >= 30 && code <= 37:
			fgColor = ansiColors[code]
		case code >= 90 && code <= 97:
			fgColor = ansiColors[code]

		case code >= 40 && code <= 47:
			bgColor = ansiBgColors[code]
		case code >= 100 && code <= 107:
			bgColor = ansiBgColors[code]

		case code == 38 && i+2 < len(codes) && codes[i+1] == 5:
			// 256 colors - упрощенная поддержка
			fgColor = tcell.PaletteColor(codes[i+2])
			i += 2
		case code == 48 && i+2 < len(codes) && codes[i+1] == 5:
			// 256 colors background
			bgColor = tcell.PaletteColor(codes[i+2])
			i += 2
		}
		i++
	}

	// Применяем цвета
	if fgColor != tcell.ColorDefault {
		style = style.Foreground(fgColor)
	}
	if bgColor != tcell.ColorDefault {
		style = style.Background(bgColor)
	}

	// Применяем атрибуты
	if bold {
		style = style.Bold(true)
	}
	if underline {
		style = style.Underline(true)
	}
	if italic {
		style = style.Italic(true)
	}

	return style
}

func (t *Terminal) addColoredOutput(text string, baseStyle tcell.Style) {
	segments := parseANSI(text, baseStyle)
	for _, segment := range segments {
		// Разбиваем на строки если есть переносы
		lines := strings.Split(segment.Text, "\n")
		for i, line := range lines {
			if i > 0 {
				// Добавляем явный перенос строки между частями
				t.outputLines = append(t.outputLines, LineSegment{Text: "\n", Style: segment.Style})
			}
			t.outputLines = append(t.outputLines, LineSegment{Text: line, Style: segment.Style})
		}
	}
}

func (t *Terminal) executeCommand(cmd string) {
	// Добавляем команду и ее вывод в НАЧАЛО вывода (чтобы сдвинуть старый вывод вниз)
	// Но сначала добавляем текущую команду
	commandSegment := LineSegment{
		Text:  "> " + cmd,
		Style: tcell.StyleDefault.Foreground(tcell.ColorGray).Background(tcell.ColorDefault),
	}

	// Создаем новый слайс и добавляем команду ПЕРВОЙ
	newOutput := []LineSegment{commandSegment}

	// Обрабатываем команду и получаем вывод
	resultSegments := t.processCommand(cmd)

	// Добавляем результат команды после самой команды
	newOutput = append(newOutput, resultSegments...)

	// Добавляем весь старый вывод ПОСЛЕ новой команды и ее результата
	newOutput = append(newOutput, t.outputLines...)

	// Заменяем старый вывод на новый
	t.outputLines = newOutput

	// Очищаем ввод и обновляем историю
	t.inputBuffer = make([]rune, 0)
	t.cursorPos = 0
	t.history = append(t.history, cmd)
	t.historyPos = len(t.history)
}
func (t *Terminal) processCommand(cmd string) []LineSegment {
	args := strings.Fields(cmd)
	if len(args) == 0 {
		return []LineSegment{}
	}

	var segments []LineSegment

	switch args[0] {
	case "exit", "quit":
		t.screen.Fini()
		os.Exit(0)
	case "clear":
		t.outputLines = []LineSegment{}
		return []LineSegment{}
	case "echo":
		if len(args) > 1 {
			echoText := strings.Join(args[1:], " ")
			segments = parseANSI(echoText, tcell.StyleDefault.
				Foreground(tcell.ColorWhite).
				Background(tcell.ColorDefault))
		}
	case "pwd":
		dir, _ := os.Getwd()
		segments = parseANSI(dir, tcell.StyleDefault.
			Foreground(tcell.ColorGreen).
			Background(tcell.ColorDefault))
	case "time":
		currentTime := time.Now().Format("15:04:05")
		segments = parseANSI(currentTime, tcell.StyleDefault.
			Foreground(tcell.ColorYellow).
			Background(tcell.ColorDefault))
	case "colors":
		segments = t.processColorDemo()
	case "help":
		segments = t.processHelpCommand()
	case "history":
		segments = t.processHistoryCommand()
	case "cd":
		segments = t.processCdCommand(args)
	case "ls":
		segments = t.processLsCommand(args)
	case "date":
		segments = t.processDateCommand()
	case "whoami":
		segments = t.processWhoamiCommand()
	case "run":
		if len(args) > 1 {
			segments = t.processSystemCommand(args[1:])
		} else {
			segments = parseANSI("Usage: run <command> [args...]", tcell.StyleDefault.
				Foreground(tcell.ColorRed).
				Background(tcell.ColorDefault))
		}
	default:
		segments = t.processSystemCommand(args)
	}

	return segments
}
func (t *Terminal) processLsCommand(args []string) []LineSegment {
	dir := "."
	longFormat := false
	showHidden := false
	onePerLine := false

	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "-l" {
			longFormat = true
		} else if arg == "-a" {
			showHidden = true
		} else if arg == "-1" {
			onePerLine = true
		} else if arg == "-la" || arg == "-al" {
			longFormat = true
			showHidden = true
		} else if !strings.HasPrefix(arg, "-") {
			dir = arg
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return []LineSegment{{Text: "Error reading directory", Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	var validEntries []os.DirEntry
	for _, entry := range entries {
		if !showHidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		validEntries = append(validEntries, entry)
	}

	// Для -1 или -l - каждый элемент на отдельной строке
	if onePerLine || longFormat {
		var result []LineSegment
		for _, entry := range validEntries { // Убрали i, так как он не используется
			var line string
			if longFormat {
				info, err := entry.Info()
				if err != nil {
					continue
				}
				fileType := "-"
				if entry.IsDir() {
					fileType = "d"
				}
				line = fmt.Sprintf("%s %8d %s %s", fileType, info.Size(), info.ModTime().Format("Jan 02 15:04"), entry.Name())
			} else {
				line = entry.Name()
			}
			
			// Каждая строка - отдельный сегмент
			result = append(result, LineSegment{Text: line, Style: tcell.StyleDefault.Foreground(tcell.ColorWhite)})
		}
		return result
	} else {
		// Обычный ls - все в одну строку
		var names []string
		for _, entry := range validEntries {
			names = append(names, entry.Name())
		}
		combined := strings.Join(names, "  ")
		return []LineSegment{{Text: combined, Style: tcell.StyleDefault.Foreground(tcell.ColorWhite)}}
	}
}

func (t *Terminal) processColorDemo() []LineSegment {
	colors := []struct {
		name  string
		color tcell.Color
		code  string
	}{
		{"Black", tcell.ColorBlack, "30"},
		{"Red", tcell.ColorRed, "31"},
		{"Green", tcell.ColorGreen, "32"},
		{"Yellow", tcell.ColorYellow, "33"},
		{"Blue", tcell.ColorBlue, "34"},
		{"Magenta", tcell.ColorDarkMagenta, "35"},
		{"Cyan", tcell.ColorTeal, "36"},
		{"White", tcell.ColorWhite, "37"},
		{"Gray", tcell.ColorGray, "90"},
		{"Bright Red", tcell.ColorRed, "91"},
		{"Bright Green", tcell.ColorGreen, "92"},
		{"Bright Yellow", tcell.ColorYellow, "93"},
		{"Bright Blue", tcell.ColorBlue, "94"},
		{"Bright Magenta", tcell.ColorDarkMagenta, "95"},
		{"Bright Cyan", tcell.ColorTeal, "96"},
		{"Bright White", tcell.ColorWhite, "97"},
	}

	var segments []LineSegment
	for _, c := range colors {
		demo := fmt.Sprintf("\033[%sm%s\033[0m - %s", c.code, c.name, c.code)
		segments = append(segments, parseANSI(demo, tcell.StyleDefault)...)
	}
	return segments
}

func (t *Terminal) processHelpCommand() []LineSegment {
	helpText := `Доступные команды:
  exit, quit    - Выйти из терминала
  clear         - Очистить экран
  echo <текст>  - Вывести текст
  pwd           - Показать текущую директорию
  time          - Показать текущее время
  date          - Показать текущую дату
  whoami        - Показать имя текущего пользователя
  history       - Показать историю команд
  ls [опции]    - Показать содержимое директории
                -l: подробный формат
                -a: показать скрытые файлы
                -1: по одному файлу на строку
  cd <директория> - Перейти в директорию
  colors        - Демонстрация цветов
  help          - Показать это сообщение
  run <команда> - Выполнить системную команду
  <команда>     - Выполнить системную команду напрямую`

	return []LineSegment{{
		Text:  helpText,
		Style: tcell.StyleDefault.Foreground(tcell.ColorWhite),
	}}
}

func (t *Terminal) processHistoryCommand() []LineSegment {
	var segments []LineSegment

	// Отображаем историю команд с номерами
	for i, cmd := range t.history {
		historyLine := fmt.Sprintf("%d: %s", i+1, cmd)
		segments = append(segments, LineSegment{
			Text:  historyLine,
			Style: tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorDefault),
		})
	}

	return segments
}

func (t *Terminal) processCdCommand(args []string) []LineSegment {
	if len(args) < 2 {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			errorMsg := fmt.Sprintf("Ошибка: %s", err)
			return []LineSegment{{Text: errorMsg, Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
		}
		args = []string{"cd", homeDir}
	}

	err := os.Chdir(args[1])
	if err != nil {
		errorMsg := fmt.Sprintf("Ошибка: %s", err)
		return []LineSegment{{Text: errorMsg, Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	return []LineSegment{}
}



func (t *Terminal) processDateCommand() []LineSegment {
	// Получаем текущую дату и время
	currentTime := time.Now()

	// Форматируем дату и время
	// Формат: день недели, месяц, день, год, время
	dateText := currentTime.Format("Mon Jan 2 15:04:05 MST 2006")

	return parseANSI(dateText, tcell.StyleDefault.
		Foreground(tcell.ColorYellow).
		Background(tcell.ColorDefault))
}

func (t *Terminal) processWhoamiCommand() []LineSegment {
	// Получаем информацию о текущем пользователе
	currentUser, err := user.Current()
	if err != nil {
		errorMsg := fmt.Sprintf("\033[31mError: %s\033[0m", err)
		return parseANSI(errorMsg, tcell.StyleDefault)
	}

	return parseANSI(currentUser.Username, tcell.StyleDefault.
		Foreground(tcell.ColorGreen).
		Background(tcell.ColorDefault))
}

func (t *Terminal) processSystemCommand(args []string) []LineSegment {
	cmd := exec.Command(args[0], args[1:]...)
	
	// Устанавливаем UTF-8 кодировку для вывода
	cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	
	output, err := cmd.CombinedOutput()

	if err != nil {
		errorMsg := fmt.Sprintf("Ошибка: %s", err)
		return []LineSegment{{Text: errorMsg, Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	} else {
		// Вывод как есть - предполагаем что tcell поддерживает UTF-8
		return []LineSegment{{
			Text:  string(output),
			Style: tcell.StyleDefault.Foreground(tcell.ColorWhite),
		}}
	}
}

func (t *Terminal) handleKeyEvent(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEscape, tcell.KeyCtrlC:
		t.screen.Fini()
		os.Exit(0)

	case tcell.KeyEnter:
		cmd := string(t.inputBuffer)
		if cmd != "" {
			t.executeCommand(cmd)
		}

	case tcell.KeyUp:
		if t.historyPos > 0 {
			t.historyPos--
			t.inputBuffer = []rune(t.history[t.historyPos])
			t.cursorPos = len(t.inputBuffer)
		}

	case tcell.KeyDown:
		if t.historyPos < len(t.history)-1 {
			t.historyPos++
			t.inputBuffer = []rune(t.history[t.historyPos])
			t.cursorPos = len(t.inputBuffer)
		} else if t.historyPos == len(t.history)-1 {
			t.historyPos = len(t.history)
			t.inputBuffer = make([]rune, 0)
			t.cursorPos = 0
		}

	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if t.cursorPos > 0 && len(t.inputBuffer) > 0 {
			t.inputBuffer = append(t.inputBuffer[:t.cursorPos-1], t.inputBuffer[t.cursorPos:]...)
			t.cursorPos--
		}

	case tcell.KeyDelete:
		if t.cursorPos < len(t.inputBuffer) {
			t.inputBuffer = append(t.inputBuffer[:t.cursorPos], t.inputBuffer[t.cursorPos+1:]...)
		}

	case tcell.KeyLeft:
		if t.cursorPos > 0 {
			t.cursorPos--
		}

	case tcell.KeyRight:
		if t.cursorPos < len(t.inputBuffer) {
			t.cursorPos++
		}

	case tcell.KeyHome:
		t.cursorPos = 0

	case tcell.KeyEnd:
		t.cursorPos = len(t.inputBuffer)

	case tcell.KeyRune:
		t.insertRune(ev.Rune())

	default:
		// Игнорируем другие клавиши
	}
}

func (t *Terminal) insertRune(r rune) {
    // Вставляем руну правильно
    if t.cursorPos == len(t.inputBuffer) {
        t.inputBuffer = append(t.inputBuffer, r)
    } else {
        t.inputBuffer = append(t.inputBuffer[:t.cursorPos], append([]rune{r}, t.inputBuffer[t.cursorPos:]...)...)
    }
    t.cursorPos++
}
