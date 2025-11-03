package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strings"
	"time"

	"github.com/creack/pty"
	"github.com/gdamore/tcell/v2"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

type Terminal struct {
	screen                 tcell.Screen
	inputBuffer            []rune
	cursorPos              int
	cursorVisible          bool
	lastBlink              time.Time
	outputLines            []LineSegment // Храним вывод команд с цветами
	history                []string      // История команд
	historyPos             int           // Позиция в истории
	zshHistory             []string      // История команд из zsh
	completionMatches      []string      // Варианты автодополнения
	completionIndex        int           // Текущий индекс в списке вариантов
	completionScrollOffset int           // Смещение скролла списка вариантов
	ptmx                   *os.File
	cmd                    *exec.Cmd
	inPtyMode              bool
	scrollOffset           int
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

func (t *Terminal) processPtyCommand(args []string) []LineSegment {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return []LineSegment{{Text: fmt.Sprintf("Ошибка PTY: %s", err), Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	t.ptmx = ptmx
	t.cmd = cmd
	t.inPtyMode = true

	// Чтение вывода в фоне
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				break
			}
			output := string(buf[:n])
			t.addColoredOutput(output, tcell.StyleDefault.Foreground(tcell.ColorWhite))
		}
		t.inPtyMode = false
	}()

	return []LineSegment{{Text: "Запущена интерактивная команда...", Style: tcell.StyleDefault.Foreground(tcell.ColorGreen)}}
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

	// Загружаем историю zsh
	zshHistory, err := loadZshHistory()
	if err != nil {
		// В случае ошибки продолжаем работу без истории zsh
		fmt.Printf("Предупреждение: не удалось загрузить историю zsh: %v\n", err)
	} else {
		term.zshHistory = zshHistory
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

// loadZshHistory загружает историю команд из файла ~/.zsh_history
func loadZshHistory() ([]string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	historyPath := homeDir + "/.zsh_history"
	file, err := os.Open(historyPath)
	if err != nil {
		// Если файл не найден, возвращаем пустую историю
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer file.Close()

	var history []string
	scanner := bufio.NewScanner(file)

	// Регулярное выражение для извлечения команд из формата zsh_history
	// Формат: : timestamp:0;command
	re := regexp.MustCompile(`^: \d+:\d+;(.*)$`)

	for scanner.Scan() {
		line := scanner.Text()
		matches := re.FindStringSubmatch(line)
		if len(matches) > 1 {
			command := matches[1]
			if command != "" {
				history = append(history, command)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return history, nil
}
func (t *Terminal) updateCursorBlink() {
	if time.Since(t.lastBlink) > 500*time.Millisecond {
		t.cursorVisible = !t.cursorVisible
		t.lastBlink = time.Now()
	}
}

func (t *Terminal) draw() {
	width, height := t.screen.Size()

	offsetX := 2
	offsetY := 2
	termWidth := width - 4*offsetX
	termHeight := height - 4*offsetY

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

	// Отображаем список вариантов автодополнения, если они есть
	if len(t.completionMatches) > 0 {
		t.drawCompletionList(offsetX, inputY+2, termWidth)
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

	// Пропускаем строки согласно прокрутке
	startIndex := 0
	if t.scrollOffset > 0 && t.scrollOffset < len(t.outputLines) {
		startIndex = t.scrollOffset
	}

	for i := startIndex; i < len(t.outputLines) && currentY < offsetY+availableHeight; i++ {
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
			take := len(runes)
			if width < take {
				take = width
			}
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

// drawCompletionList отображает список вариантов автодополнения
func (t *Terminal) drawCompletionList(offsetX, offsetY, maxWidth int) {
	if len(t.completionMatches) == 0 {
		return
	}

	// Ограничиваем количество отображаемых вариантов
	maxVisible := 10

	// Применяем смещение скролла
	startIndex := t.completionScrollOffset
	if startIndex >= len(t.completionMatches) {
		startIndex = 0
		t.completionScrollOffset = 0
	}

	// Определяем конечный индекс
	endIndex := startIndex + maxVisible
	if endIndex > len(t.completionMatches) {
		endIndex = len(t.completionMatches)
	}

	// Получаем подмножество вариантов для отображения
	matchesToShow := t.completionMatches[startIndex:endIndex]

	// Отображаем каждый вариант
	for i, match := range matchesToShow {
		y := offsetY + i

		// Вычисляем глобальный индекс для определения текущего выбора
		globalIndex := startIndex + i

		// Создаем текст с индикатором текущего выбора
		var text string
		if globalIndex == t.completionIndex {
			text = "> " + match
		} else {
			text = "  " + match
		}

		// Ограничиваем длину текста шириной терминала
		if len([]rune(text)) > maxWidth {
			runes := []rune(text)
			text = string(runes[:maxWidth-3]) + "..."
		}

		// Выбираем стиль в зависимости от того, является ли это текущим выбором
		var style tcell.Style
		if globalIndex == t.completionIndex {
			style = tcell.StyleDefault.
				Foreground(tcell.ColorWhite).
				Background(tcell.ColorBlue)
		} else {
			style = tcell.StyleDefault.
				Foreground(tcell.ColorGray).
				Background(tcell.ColorDefault)
		}

		// Отображаем текст
		t.drawText(offsetX, y, text, style)
	}

	// Если есть еще варианты, отображаем индикатор прокрутки
	if len(t.completionMatches) > maxVisible {
		// Отображаем индикатор прокрутки в правом нижнем углу списка
		scrollIndicator := fmt.Sprintf("[%d/%d]",
			startIndex/maxVisible+1,
			(len(t.completionMatches)+maxVisible-1)/maxVisible)

		indicatorStyle := tcell.StyleDefault.
			Foreground(tcell.ColorYellow).
			Background(tcell.ColorDefault)

		t.drawText(offsetX+maxWidth-len([]rune(scrollIndicator)),
			offsetY+maxVisible-1,
			scrollIndicator,
			indicatorStyle)
	}
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
	// Стили
	titleStyle := tcell.StyleDefault.Foreground(tcell.ColorTeal).Bold(true)
	commandStyle := tcell.StyleDefault.Foreground(tcell.ColorGreen)
	descStyle := tcell.StyleDefault.Foreground(tcell.ColorWhite)
	optionStyle := tcell.StyleDefault.Foreground(tcell.ColorYellow)

	// Сначала формируем весь текст
	var output strings.Builder

	// Заголовок
	output.WriteString("Доступные команды:\n\n")

	// Команды с выравниванием
	commands := []struct {
		cmd  string
		desc string
	}{
		{"exit, quit", "Выйти из терминала"},
		{"clear", "Очистить экран"},
		{"echo <текст>", "Вывести текст"},
		{"pwd", "Показать текущую директорию"},
		{"time", "Показать текущее время"},
		{"date", "Показать текущую дату"},
		{"whoami", "Показать имя текущего пользователя"},
		{"history", "Показать историю команд"},
		{"ls [опции]", "Показать содержимое директории"},
		{"cd <директория>", "Перейти в директорию"},
		{"colors", "Демонстрация цветов"},
		{"help", "Показать это сообщение"},
		{"run <команда>", "Выполнить системную команду"},
		{"<команда>", "Выполнить системную команду напрямую"},
	}

	// Находим максимальную длину команд для выравнивания
	maxLen := 0
	for _, cmd := range commands {
		if len(cmd.cmd) > maxLen {
			maxLen = len(cmd.cmd)
		}
	}

	// Формируем выровненные строки
	for _, cmd := range commands {
		padding := strings.Repeat(" ", maxLen-len(cmd.cmd))
		output.WriteString("  " + cmd.cmd + padding + "  - " + cmd.desc + "\n")
	}

	// Опции для ls
	output.WriteString("\n  Опции для ls:\n")
	options := []struct {
		opt  string
		desc string
	}{
		{"-l", "подробный формат"},
		{"-a", "показать скрытые файлы"},
		{"-1", "по одному файлу на строку"},
	}

	for _, opt := range options {
		output.WriteString("    " + opt.opt + " - " + opt.desc + "\n")
	}

	// Теперь разбиваем на строки и применяем стили
	lines := strings.Split(output.String(), "\n")
	var segments []LineSegment

	for _, line := range lines {
		if strings.Contains(line, "Доступные команды:") {
			segments = append(segments, LineSegment{Text: line, Style: titleStyle})
		} else if strings.Contains(line, "Опции для ls:") {
			segments = append(segments, LineSegment{Text: line, Style: descStyle})
		} else {
			// Разбираем строку на части для раскраски
			segments = append(segments, t.colorizeHelpLine(line, commandStyle, descStyle, optionStyle))
		}
	}

	return segments
}

func (t *Terminal) colorizeHelpLine(line string, cmdStyle, descStyle, optStyle tcell.Style) LineSegment {
	// Простая логика раскраски - если строка начинается с команд, раскрашиваем их
	if strings.HasPrefix(line, "  ") && len(line) > 2 {
		// Ищем разделитель " - "
		if idx := strings.Index(line, " - "); idx != -1 {
			commandPart := line[:idx]
			descPart := line[idx:]

			// Проверяем, является ли это опцией ls (имеет отступ 4 пробела)
			if strings.HasPrefix(line, "    ") && len(line) > 4 {
				// Это опция - раскрашиваем флаг
				if flagIdx := strings.Index(line, " - "); flagIdx != -1 {
					flagPart := line[4:flagIdx]
					restPart := line[flagIdx:]
					coloredLine := flagPart + restPart
					return LineSegment{Text: coloredLine, Style: optStyle}
				}
			} else {
				// Это обычная команда
				coloredLine := commandPart + descPart
				return LineSegment{Text: coloredLine, Style: cmdStyle}
			}
		}
	}

	// По умолчанию - обычный текст
	return LineSegment{Text: line, Style: descStyle}
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
	// Для интерактивных команд используем PTY
	interactiveCommands := map[string]bool{
		"vim": true, "vi": true, "nano": true, "top": true,
		"htop": true, "less": true, "more": true, "man": true,
	}

	if interactiveCommands[args[0]] {
		return t.processPtyCommand(args)
	}

	// Для обычных команд оставляем как было
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")

	output, err := cmd.CombinedOutput()
	if err != nil {
		errorMsg := fmt.Sprintf("Ошибка: %s", err)
		return []LineSegment{{Text: errorMsg, Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	return []LineSegment{{
		Text:  string(output),
		Style: tcell.StyleDefault.Foreground(tcell.ColorWhite),
	}}
}

func (t *Terminal) handleKeyEvent(ev *tcell.EventKey) {
	// Если в режиме PTY, передаем ввод в команду
	if t.inPtyMode && t.ptmx != nil {
		switch ev.Key() {
		case tcell.KeyEscape:
			if ev.Modifiers() == tcell.ModCtrl {
				// Ctrl+C для выхода из PTY режима
				t.cmd.Process.Signal(os.Interrupt)
				t.inPtyMode = false
				return
			}
			t.ptmx.Write([]byte{0x1b}) // ESC
		case tcell.KeyEnter:
			t.ptmx.Write([]byte{'\r'})
		case tcell.KeyBackspace, tcell.KeyBackspace2:
			t.ptmx.Write([]byte{'\b'})
		case tcell.KeyTab:
			t.ptmx.Write([]byte{'\t'})
		case tcell.KeyRune:
			t.ptmx.Write([]byte(string(ev.Rune())))
		}
		return
	}
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
	// Добавьте в switch-case в handleKeyEvent:
	case tcell.KeyPgUp:
		// Если отображается список автодополнения, прокручиваем его
		if len(t.completionMatches) > 0 {
			t.completionScrollOffset = max(0, t.completionScrollOffset-10)
		} else {
			// Иначе прокручиваем основной вывод
			t.scrollOffset += 5
		}
	case tcell.KeyPgDn:
		// Если отображается список автодополнения, прокручиваем его
		if len(t.completionMatches) > 0 {
			maxScroll := len(t.completionMatches) - 10
			if maxScroll < 0 {
				maxScroll = 0
			}
			t.completionScrollOffset = min(t.completionScrollOffset+10, maxScroll)
		} else {
			// Иначе прокручиваем основной вывод
			t.scrollOffset = max(0, t.scrollOffset-5)
		}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if t.cursorPos > 0 && len(t.inputBuffer) > 0 {
			t.inputBuffer = append(t.inputBuffer[:t.cursorPos-1], t.inputBuffer[t.cursorPos:]...)
			t.cursorPos--
			// Обновляем список автодополнения после удаления символа
			t.updateCompletionList()
		}

	case tcell.KeyDelete:
		if t.cursorPos < len(t.inputBuffer) {
			t.inputBuffer = append(t.inputBuffer[:t.cursorPos], t.inputBuffer[t.cursorPos+1:]...)
			// Обновляем список автодополнения после удаления символа
			t.updateCompletionList()
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

	case tcell.KeyTab:
		// Если уже есть совпадения, выполняем циклическое переключение
		if len(t.completionMatches) > 0 {
			t.cycleCompletion()
		} else {
			// Иначе выполняем обычное автодополнение
			t.autoComplete()
		}

	case tcell.KeyRune:
		// При вводе нового символа обновляем список автодополнения
		t.insertRune(ev.Rune())
		t.updateCompletionList()

	default:
		// Игнорируем другие клавиши
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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

// findCompletionMatches находит все совпадения для автодополнения
func (t *Terminal) findCompletionMatches() []string {
	if len(t.inputBuffer) == 0 {
		return []string{}
	}

	currentInput := string(t.inputBuffer)
	var matches []string
	seen := make(map[string]bool) // Для исключения дубликатов

	// Сначала ищем точные префиксы в истории zsh
	for i := len(t.zshHistory) - 1; i >= 0; i-- {
		cmd := t.zshHistory[i]
		if strings.HasPrefix(cmd, currentInput) && cmd != currentInput {
			if !seen[cmd] {
				matches = append(matches, cmd)
				seen[cmd] = true
			}
		}
	}

	// Затем в внутренней истории
	for i := len(t.history) - 1; i >= 0; i-- {
		cmd := t.history[i]
		if strings.HasPrefix(cmd, currentInput) && cmd != currentInput {
			if !seen[cmd] {
				matches = append(matches, cmd)
				seen[cmd] = true
			}
		}
	}

	// Если точных префиксов не найдено, ищем частичные совпадения
	// Сначала в истории zsh
	if len(matches) == 0 {
		for i := len(t.zshHistory) - 1; i >= 0; i-- {
			cmd := t.zshHistory[i]
			if strings.Contains(cmd, currentInput) && cmd != currentInput {
				if !seen[cmd] {
					matches = append(matches, cmd)
					seen[cmd] = true
				}
			}
		}

		// Затем в внутренней истории
		for i := len(t.history) - 1; i >= 0; i-- {
			cmd := t.history[i]
			if strings.Contains(cmd, currentInput) && cmd != currentInput {
				if !seen[cmd] {
					matches = append(matches, cmd)
					seen[cmd] = true
				}
			}
		}
	}

	return matches
}

// autoComplete выполняет автодополнение текущего ввода на основе истории команд
func (t *Terminal) autoComplete() {
	// Находим все совпадения
	matches := t.findCompletionMatches()

	if len(matches) == 0 {
		// Нет совпадений, ничего не делаем
		return
	}

	// Сохраняем совпадения и сбрасываем индекс
	t.completionMatches = matches
	t.completionIndex = 0

	// Применяем первое совпадение
	firstMatch := matches[0]
	t.inputBuffer = []rune(firstMatch)
	t.cursorPos = len(t.inputBuffer)
}

// cycleCompletion выполняет циклическое переключение между вариантами автодополнения
func (t *Terminal) cycleCompletion() {
	if len(t.completionMatches) == 0 {
		// Если нет сохраненных совпадений, пытаемся найти их
		t.autoComplete()
		return
	}

	// Переходим к следующему совпадению (циклически)
	t.completionIndex = (t.completionIndex + 1) % len(t.completionMatches)

	// Применяем текущее совпадение
	currentMatch := t.completionMatches[t.completionIndex]
	t.inputBuffer = []rune(currentMatch)
	t.cursorPos = len(t.inputBuffer)
}

// updateCompletionList обновляет список вариантов автодополнения на основе текущего ввода
func (t *Terminal) updateCompletionList() {
	// Находим все совпадения
	matches := t.findCompletionMatches()

	// Сохраняем совпадения
	t.completionMatches = matches

	// Если есть совпадения, устанавливаем индекс на первое совпадение
	if len(matches) > 0 {
		t.completionIndex = 0
	} else {
		// Если совпадений нет, сбрасываем индекс
		t.completionIndex = 0
	}

	// Сбрасываем смещение скролла
	t.completionScrollOffset = 0
}
