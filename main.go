package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strings"
	// "syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gdamore/tcell/v2"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

type Terminal struct {
	screen               tcell.Screen
	inputBuffer          []rune
	cursorPos            int
	cursorVisible        bool
	lastBlink            time.Time
	outputLines          []LineSegment // Храним вывод команд с цветами
	history              []string      // История команд
	historyPos           int           // Позиция в истории
	zshHistory           []string      // История команд из zsh
	completionSuggestion string        // Текст подсказки (серая часть)
	suggestionStyle      tcell.Style
	ptmx                 *os.File
	cmd                  *exec.Cmd
	inPtyMode            bool
	scrollOffset         int
	sudoPrompt           string            // Приглашение ввода пароля для sudo
	aliases              map[string]string // Алиасы команд
	envVars              map[string]string // Переменные окружения
	ptyClosed            chan struct{}     // Канал для сигнализации о закрытии PTY
}

// parseArgs разбирает команду на аргументы с учетом кавычек
func (t *Terminal) parseArgs(input string) []string {
	var args []string
	var current strings.Builder
	inQuotes := false
	quoteChar := rune(0)

	for _, r := range input {
		switch {
		case r == '"' || r == '\'':
			if !inQuotes {
				// Начало кавычек
				inQuotes = true
				quoteChar = r
			} else if quoteChar == r {
				// Конец кавычек
				inQuotes = false
				quoteChar = 0
			} else {
				// Кавычка внутри других кавычек
				current.WriteRune(r)
			}
		case r == ' ' || r == '\t':
			if inQuotes {
				// Пробел внутри кавычек
				current.WriteRune(r)
			} else {
				// Пробел вне кавычек - конец аргумента
				if current.Len() > 0 {
					args = append(args, current.String())
					current.Reset()
				}
			}
		default:
			current.WriteRune(r)
		}
	}

	// Добавляем последний аргумент
	if current.Len() > 0 {
		args = append(args, current.String())
	}

	return args
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

// executeSimpleCommand выполняет простые команды с выводом результата
func (t *Terminal) executeSimpleCommand(args []string) []LineSegment {
	log.Printf("🔧 Выполнение простой команды: %v", args)

	cmd := exec.Command(args[0], args[1:]...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return []LineSegment{{Text: fmt.Sprintf("Ошибка: %s\n%s", err, string(output)), Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	text := string(output)
	if text == "" {
		text = "[Команда выполнена без вывода]"
	}
	return []LineSegment{{Text: text, Style: tcell.StyleDefault.Foreground(tcell.ColorWhite)}}
}

// executeInteractiveCommand выполняет интерактивные команды через pipes
func (t *Terminal) executeInteractiveCommand(args []string) []LineSegment {
	log.Printf("🔄 Запуск интерактивной команды: %v", args)

	if len(args) == 0 {
		return []LineSegment{{Text: "Ошибка: нет команды", Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	// Создаем команду
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")

	// Создаем pipes для stdin, stdout, stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("❌ Ошибка создания stdin pipe: %v", err)
		return []LineSegment{{Text: fmt.Sprintf("Ошибка stdin: %s", err), Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("❌ Ошибка создания stdout pipe: %v", err)
		return []LineSegment{{Text: fmt.Sprintf("Ошибка stdout: %s", err), Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("❌ Ошибка создания stderr pipe: %v", err)
		return []LineSegment{{Text: fmt.Sprintf("Ошибка stderr: %s", err), Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	// Запускаем команду
	if err := cmd.Start(); err != nil {
		log.Printf("❌ Ошибка запуска команды: %v", err)
		return []LineSegment{{Text: fmt.Sprintf("Ошибка запуска: %s", err), Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	log.Printf("✅ Команда запущена, PID: %d", cmd.Process.Pid)

	// Сохраняем состояние
	t.cmd = cmd
	t.inPtyMode = true

	// Обработка stdout
	// В executeInteractiveCommand обновите обработку stdout:
	go func() {
		defer stdout.Close()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			text := scanner.Text()
			log.Printf("📨 STDOUT: %s", text)

			// 🔴 ОБНАРУЖЕНИЕ SUDO PROMPT
			if strings.Contains(text, "[sudo] password for") ||
				strings.Contains(text, "Password:") ||
				strings.Contains(text, "Пароль:") {
				t.sudoPrompt = text
				log.Printf("🔐 Обнаружен sudo prompt: %s", text)
			}

			t.addColoredOutputAtBeginning(text+"\n", tcell.StyleDefault.Foreground(tcell.ColorWhite))
		}
		if err := scanner.Err(); err != nil {
			log.Printf("❌ Ошибка чтения stdout: %v", err)
		}
	}()

	// Обработка stderr
	go func() {
		defer stderr.Close()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			text := scanner.Text()
			log.Printf("📨 STDERR: %s", text)
			t.addColoredOutputAtBeginning(text+"\n", tcell.StyleDefault.Foreground(tcell.ColorRed))
		}
		if err := scanner.Err(); err != nil {
			log.Printf("❌ Ошибка чтения stderr: %v", err)
		}
	}()

	// Сохраняем stdin для использования в handleKeyEvent
	t.ptmx = stdin.(*os.File)

	// Ожидание завершения в отдельной горутине
	go func() {
		err := cmd.Wait()
		log.Printf("🔚 Команда завершена, ошибка: %v", err)
		t.inPtyMode = false
		t.ptmx = nil
		t.cmd = nil
		if err == nil {
			t.addColoredOutputAtBeginning("\n[Команда завершена успешно]\n", tcell.StyleDefault.Foreground(tcell.ColorGreen))
		} else {
			t.addColoredOutputAtBeginning(fmt.Sprintf("\n[Команда завершена с ошибкой: %v]\n", err), tcell.StyleDefault.Foreground(tcell.ColorYellow))
		}
	}()

	return []LineSegment{}
}

// addColoredOutputAtBeginning добавляет вывод в НАЧАЛО outputLines
// addColoredOutputAtBeginning добавляет вывод в НАЧАЛО outputLines с правильным порядком строк
// addColoredOutputAtBeginning добавляет вывод в НАЧАЛО outputLines (как в оригинале)
func (t *Terminal) addColoredOutputAtBeginning(text string, baseStyle tcell.Style) {
	segments := parseANSI(text, baseStyle)

	// Создаем новый слайс и добавляем новые сегменты ПЕРВЫМИ
	newOutput := []LineSegment{}

	// Добавляем новые сегменты
	for _, segment := range segments {
		// Разбиваем на строки если есть переносы
		lines := strings.Split(segment.Text, "\n")
		for i, line := range lines {
			if i > 0 {
				// Добавляем явный перенос строки между частями
				newOutput = append(newOutput, LineSegment{Text: "\n", Style: segment.Style})
			}
			newOutput = append(newOutput, LineSegment{Text: line, Style: segment.Style})
		}
	}

	// Добавляем весь старый вывод ПОСЛЕ новых сегментов
	newOutput = append(newOutput, t.outputLines...)

	// Заменяем старый вывод на новый
	t.outputLines = newOutput
}
func (t *Terminal) processPtyCommand(args []string) []LineSegment {
	log.Printf("🎯 Обработка команды: %v", args)

	if len(args) == 0 {
		return []LineSegment{{Text: "Ошибка: нет команды", Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	// Определяем тип команды
	command := args[0]

	// Интерактивные команды выполняем через pipes
	interactiveCommands := map[string]bool{
		"vim": true, "nano": true, "htop": true, "top": true,
		"less": true, "more": true, "man": true, "cat": true,
		"python": true, "python3": true, "bash": true, "sh": true,
		"zsh": true, "fish": true,
	}

	if interactiveCommands[command] {
		return t.executeInteractiveCommand(args)
	}

	// Простые команды выполняем напрямую
	return t.executeSimpleCommand(args)
}

// executeWithRealTTY использует настоящий PTY для команд, которым это нужно
func (t *Terminal) executeWithRealTTY(args []string) []LineSegment {
	log.Printf("🔧 Запуск с настоящим TTY: %v", args)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")

	width, height := t.screen.Size()

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(height),
		Cols: uint16(width),
	})
	if err != nil {
		return []LineSegment{{Text: fmt.Sprintf("Ошибка TTY: %s", err), Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	t.ptmx = ptmx
	t.cmd = cmd
	t.inPtyMode = true

	// Обработка вывода
	go func() {
		defer func() {
			ptmx.Close()
			t.inPtyMode = false
			t.ptmx = nil
			t.cmd = nil
		}()

		buffer := make([]byte, 1024)
		for {
			n, err := ptmx.Read(buffer)
			if err != nil {
				break
			}
			if n > 0 {
				output := string(buffer[:n])
				t.addColoredOutputAtBeginning(output, tcell.StyleDefault.Foreground(tcell.ColorWhite))
			}
		}
	}()

	return []LineSegment{}
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

// handleSudoInput обрабатывает ввод пароля для sudo
func (t *Terminal) handleSudoInput(ev *tcell.EventKey) {
	log.Printf("🔐 Обработка sudo ввода: %v", ev.Key())

	switch ev.Key() {
	case tcell.KeyEnter:
		// Отправляем Enter в PTY (подтверждаем пароль или пустой пароль)
		t.ptmx.Write([]byte{'\n'})
		log.Printf("↵ Enter отправлен в sudo")
		t.sudoPrompt = "" // Сбрасываем приглашение после ввода

	case tcell.KeyBackspace, tcell.KeyBackspace2:
		// Backspace для редактирования пароля
		t.ptmx.Write([]byte{'\b'})
		log.Printf("⌫ Backspace в sudo")

	case tcell.KeyRune:
		// Передаем символы пароля
		t.ptmx.Write([]byte(string(ev.Rune())))
		log.Printf("📝 Символ пароля отправлен")

	case tcell.KeyCtrlC:
		// Ctrl+C для отмены sudo
		t.ptmx.Write([]byte{0x03})
		log.Printf("🚫 Ctrl+C - отмена sudo")
		t.sudoPrompt = ""

	default:
		log.Printf("❓ Необработанная клавиша в sudo: %v", ev.Key())
	}
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

// saveAliases сохраняет алиасы в файл ~/.termgo_aliases
func (t *Terminal) saveAliases() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	aliasesPath := homeDir + "/.termgo_aliases"
	file, err := os.Create(aliasesPath)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)

	// Записываем алиасы в формате alias_name=command
	for alias, command := range t.aliases {
		line := fmt.Sprintf("%s=%s\n", alias, command)
		_, err := writer.WriteString(line)
		if err != nil {
			return err
		}
	}

	return writer.Flush()
}

// loadZshAliases загружает алиасы из файла ~/.zshrc
func loadZshAliases() (map[string]string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	zshrcPath := homeDir + "/.zshrc"
	file, err := os.Open(zshrcPath)
	if err != nil {
		// Если файл не найден, возвращаем пустую карту алиасов
		if os.IsNotExist(err) {
			return make(map[string]string), nil
		}
		return nil, err
	}
	defer file.Close()

	aliases := make(map[string]string)
	scanner := bufio.NewScanner(file)

	// Регулярное выражение для извлечения алиасов из формата zshrc
	// Формат: alias имя=команда или alias имя="команда" или alias имя='команда'
	re := regexp.MustCompile(`^alias\s+([^=]+)=(.*)$`)

	for scanner.Scan() {
		line := scanner.Text()
		// Пропускаем пустые строки и комментарии
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		matches := re.FindStringSubmatch(line)
		if len(matches) > 2 {
			alias := matches[1]
			command := matches[2]

			// Убираем кавычки если есть
			command = strings.Trim(command, "\"'")

			aliases[alias] = command
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return aliases, nil
}

// loadAliases загружает алиасы из файла ~/.termgo_aliases
func loadAliases() (map[string]string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	aliasesPath := homeDir + "/.termgo_aliases"
	file, err := os.Open(aliasesPath)
	if err != nil {
		// Если файл не найден, возвращаем пустую карту алиасов
		if os.IsNotExist(err) {
			return make(map[string]string), nil
		}
		return nil, err
	}
	defer file.Close()

	aliases := make(map[string]string)
	scanner := bufio.NewScanner(file)

	// Формат: alias_name=command
	re := regexp.MustCompile(`^([^=]+)=(.*)$`)

	for scanner.Scan() {
		line := scanner.Text()
		// Пропускаем пустые строки и комментарии
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		matches := re.FindStringSubmatch(line)
		if len(matches) > 2 {
			alias := matches[1]
			command := matches[2]
			aliases[alias] = command
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return aliases, nil
}

func main() {
	// Инициализация логирования
	logFile, err := os.OpenFile("terminal.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal("Не удалось открыть файл лога:", err)
	}
	defer logFile.Close()
	log.SetOutput(logFile)

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
		screen:               s,
		inputBuffer:          make([]rune, 0),
		cursorPos:            0,
		cursorVisible:        true,
		lastBlink:            time.Now(),
		outputLines:          []LineSegment{},
		history:              []string{},
		historyPos:           0,
		aliases:              make(map[string]string),
		envVars:              make(map[string]string),
		completionSuggestion: "",
		suggestionStyle:      tcell.StyleDefault.Foreground(tcell.ColorGray),
	}

	// Загружаем историю zsh
	zshHistory, err := loadZshHistory()
	if err != nil {
		// В случае ошибки продолжаем работу без истории zsh
		fmt.Printf("Предупреждение: не удалось загрузить историю zsh: %v\n", err)
	} else {
		term.zshHistory = zshHistory
	}

	// Загружаем алиасы из .zshrc
	zshAliases, err := loadZshAliases()
	if err != nil {
		// В случае ошибки продолжаем работу без алиасов из .zshrc
		fmt.Printf("Предупреждение: не удалось загрузить алиасы из .zshrc: %v\n", err)
	} else {
		// Копируем алиасы из .zshrc в терминал
		for alias, command := range zshAliases {
			term.aliases[alias] = command
		}
	}

	// Загружаем алиасы из .termgo_aliases (они будут иметь приоритет)
	aliases, err := loadAliases()
	if err != nil {
		// В случае ошибки продолжаем работу без алиасов из .termgo_aliases
		fmt.Printf("Предупреждение: не удалось загрузить алиасы из .termgo_aliases: %v\n", err)
	} else {
		// Копируем алиасы из .termgo_aliases в терминал (они перезапишут алиасы из .zshrc)
		for alias, command := range aliases {
			term.aliases[alias] = command
		}
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

// updateCompletionSuggestion ищет наиболее подходящую подсказку из истории
func (t *Terminal) updateCompletionSuggestion() {
	if len(t.inputBuffer) == 0 {
		t.completionSuggestion = ""
		return
	}

	currentInput := string(t.inputBuffer)
	t.completionSuggestion = t.findBestSuggestion(currentInput)
}

// findBestSuggestion находит лучшую подсказку из истории
func (t *Terminal) findBestSuggestion(currentInput string) string {
	var bestMatch string
	var bestScore int

	// Сначала ищем в обычной истории (более высший приоритет)
	for i := len(t.history) - 1; i >= 0; i-- {
		cmd := t.history[i]
		if score := t.calculateMatchScore(cmd, currentInput); score > bestScore {
			bestMatch = cmd
			bestScore = score
		}
	}

	// Затем в zsh истории
	for i := len(t.zshHistory) - 1; i >= 0; i-- {
		cmd := t.zshHistory[i]
		if score := t.calculateMatchScore(cmd, currentInput); score > bestScore {
			bestMatch = cmd
			bestScore = score
		}
	}

	if bestMatch != "" {
		return bestMatch[len(currentInput):] // Возвращаем только дополняющую часть
	}
	return ""
}

// calculateMatchScore вычисляет релевантность совпадения
func (t *Terminal) calculateMatchScore(cmd, currentInput string) int {
	// Точное совпадение по префиксу - самый высокий приоритет
	if strings.HasPrefix(cmd, currentInput) && cmd != currentInput {
		return 1000 + len(cmd) // Более длинные команды имеют больший вес
	}

	// Частичное совпадение - низкий приоритет
	if strings.Contains(cmd, currentInput) && cmd != currentInput {
		return 100 + len(cmd)
	}

	return 0
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

	// Получаем текущую директорию
	currentDir, _ := os.Getwd()

	// 🔴 ОСОБЫЙ ПРОМПТ ДЛЯ SUDO
	var prompt string
	if t.sudoPrompt != "" {
		prompt = "[SUDO PASSWORD] "
		// Скрываем ввод для пароля
		inputLine := prompt + strings.Repeat("*", len(t.inputBuffer))
		t.drawText(offsetX, offsetY+1, inputLine, tcell.StyleDefault.
			Foreground(tcell.ColorYellow).Background(tcell.ColorDefault))
	} else {
		prompt = currentDir + " $ "

		// ОСНОВНОЙ ТЕКСТ ВВОДА (белый)
		inputText := prompt + string(t.inputBuffer)
		t.drawText(offsetX, offsetY+1, inputText, tcell.StyleDefault.
			Foreground(tcell.ColorWhite).Background(tcell.ColorDefault))

		// ПОДСКАЗКА АВТОДОПОЛНЕНИЯ (серый)
		if t.completionSuggestion != "" {
			suggestionX := offsetX + len([]rune(prompt)) + len(t.inputBuffer)
			t.drawText(suggestionX, offsetY+1, t.completionSuggestion, t.suggestionStyle)
		}
	}

	inputY := offsetY + 1
	t.drawOutput(offsetX, inputY+1, termWidth, termHeight-2)

	// 🔴 ОТОБРАЖЕНИЕ SUDO PROMPT
	if t.sudoPrompt != "" {
		t.drawText(offsetX, inputY, t.sudoPrompt, tcell.StyleDefault.
			Foreground(tcell.ColorRed).Background(tcell.ColorDefault))
	}

	// Курсор
	prefix := prompt
	cursorX := offsetX + len([]rune(prefix)) + t.cursorPos

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

	// Пропускаем первые scrollOffset строк
	skippedLines := 0
	lineIndex := 0

	// Сначала пропускаем нужное количество строк
	for lineIndex < len(t.outputLines) && skippedLines < t.scrollOffset {
		segment := t.outputLines[lineIndex]
		text := segment.Text

		// Пропускаем полностью пустые строки
		if strings.TrimSpace(text) == "" {
			lineIndex++
			continue
		}

		// Разбиваем на строки по переносам
		lines := strings.Split(text, "\n")
		skippedLines += len(lines)
		lineIndex++
	}

	// Если пропустили больше строк, чем нужно, корректируем
	if skippedLines > t.scrollOffset {
		// Нужно отобразить часть последней пропущенной строки
		segment := t.outputLines[lineIndex-1]
		text := segment.Text
		lines := strings.Split(text, "\n")
		linesToSkip := skippedLines - t.scrollOffset
		if linesToSkip < len(lines) {
			// Отображаем оставшиеся строки из последнего сегмента
			for i := linesToSkip; i < len(lines); i++ {
				line := lines[i]
				if currentY >= offsetY+availableHeight {
					break
				}

				runes := []rune(line)
				for len(runes) > 0 && currentY < offsetY+availableHeight {
					take := min(len(runes), width)
					chunk := string(runes[:take])

					// Рисуем только непустые чанки
					if strings.TrimSpace(chunk) != "" {
						t.drawText(offsetX, currentY, chunk, segment.Style)
					}

					currentY++
					runes = runes[take:]
				}
			}
		}
	}

	// Отображаем оставшиеся строки
	for lineIndex < len(t.outputLines) && currentY < offsetY+availableHeight {
		segment := t.outputLines[lineIndex]
		text := segment.Text

		// Пропускаем полностью пустые строки
		if strings.TrimSpace(text) == "" {
			lineIndex++
			continue
		}

		// Разбиваем на строки по переносам
		lines := strings.Split(text, "\n")

		for _, line := range lines {
			if currentY >= offsetY+availableHeight {
				break
			}

			runes := []rune(line)
			for len(runes) > 0 && currentY < offsetY+availableHeight {
				take := min(len(runes), width)
				chunk := string(runes[:take])

				// Рисуем только непустые чанки
				if strings.TrimSpace(chunk) != "" {
					t.drawText(offsetX, currentY, chunk, segment.Style)
				}

				currentY++
				runes = runes[take:]
			}
		}
		lineIndex++
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

// // drawCompletionList отображает список вариантов автодополнения
// func (t *Terminal) drawCompletionList(offsetX, offsetY, maxWidth int) {
// 	if len(t.completionMatches) == 0 {
// 		return
// 	}
//
// 	// Ограничиваем количество отображаемых вариантов
// 	maxVisible := 10
//
// 	// Применяем смещение скролла
// 	startIndex := t.completionScrollOffset
// 	if startIndex >= len(t.completionMatches) {
// 		startIndex = 0
// 		t.completionScrollOffset = 0
// 	}
//
// 	// Определяем конечный индекс
// 	endIndex := startIndex + maxVisible
// 	if endIndex > len(t.completionMatches) {
// 		endIndex = len(t.completionMatches)
// 	}
//
// 	// Получаем подмножество вариантов для отображения
// 	matchesToShow := t.completionMatches[startIndex:endIndex]
//
// 	// Рисуем темный фон
// 	backgroundStyle := tcell.StyleDefault.
// 		Foreground(tcell.ColorWhite).
// 		Background(tcell.ColorBlack)
//
// 	// Получаем размеры экрана
// 	screenWidth, screenHeight := t.screen.Size()
//
// 	// Рисуем фон
// 	visibleHeight := len(matchesToShow)
// 	for i := 0; i < visibleHeight && offsetY+i < screenHeight-1; i++ {
// 		for j := 0; j < maxWidth && offsetX+j < screenWidth-1; j++ {
// 			// Фон
// 			t.screen.SetContent(offsetX+j, offsetY+i, ' ', nil, backgroundStyle)
// 		}
// 	}
//
// 	// Отображаем каждый вариант
// 	for i, match := range matchesToShow {
// 		y := offsetY + i
// 		x := offsetX + 1
//
// 		// Создаем текст с индикатором текущего выбора
// 		var text string
// 		if startIndex+i == t.completionIndex {
// 			text = "> " + match
// 		} else {
// 			text = "  " + match
// 		}
//
// 		// Ограничиваем длину текста шириной терминала
// 		if len([]rune(text)) > maxWidth-2 { // -2 для учета отступа
// 			runes := []rune(text)
// 			text = string(runes[:maxWidth-5]) + "..." // -5 для учета отступа и "..."
// 		}
//
// 		// Выбираем стиль в зависимости от того, является ли это текущим выбором
// 		var style tcell.Style
// 		if startIndex+i == t.completionIndex {
// 			style = tcell.StyleDefault.
// 				Foreground(tcell.ColorBlack).
// 				Background(tcell.ColorGray)
// 		} else {
// 			style = tcell.StyleDefault.
// 				Foreground(tcell.ColorGray).
// 				Background(tcell.ColorBlack)
// 		}
//
// 		// Отображаем текст
// 		t.drawText(x, y, text, style)
// 	}
//
// 	// Если есть еще варианты, отображаем индикатор прокрутки
// 	if len(t.completionMatches) > maxVisible {
// 		// Отображаем индикатор прокрутки в правом нижнем углу списка
// 		scrollIndicator := fmt.Sprintf("[%d/%d]",
// 			startIndex/maxVisible+1,
// 			(len(t.completionMatches)+maxVisible-1)/maxVisible)
//
// 		indicatorStyle := tcell.StyleDefault.
// 			Foreground(tcell.ColorYellow).
// 			Background(tcell.ColorBlack)
//
// 		t.drawText(offsetX+maxWidth-len([]rune(scrollIndicator))-1, // -1 для учета отступа
// 			offsetY+len(matchesToShow)-1, // -1 для учета отступа
// 			scrollIndicator,
// 			indicatorStyle)
// 	}
// }

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

	// 🔴 ПРОСТО ДОБАВЛЯЕМ В КОНЕЦ - ДЛЯ ИНТЕРАКТИВНЫХ КОМАНД
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

func (t *Terminal) expandAliases(cmd string) string {
	// Разбиваем команду на аргументы
	args := t.parseArgs(cmd)
	if len(args) == 0 {
		return cmd
	}

	// Проверяем, является ли первое слово алиасом
	if aliasCmd, exists := t.aliases[args[0]]; exists {
		// Заменяем алиас на команду
		if len(args) > 1 {
			// Если есть дополнительные аргументы, добавляем их к команде
			// Объединяем аргументы обратно в строку
			var cmdBuilder strings.Builder
			cmdBuilder.WriteString(aliasCmd)
			for _, arg := range args[1:] {
				cmdBuilder.WriteString(" ")
				// Добавляем кавычки вокруг аргументов, содержащих пробелы
				if strings.Contains(arg, " ") {
					cmdBuilder.WriteString("\"")
					cmdBuilder.WriteString(arg)
					cmdBuilder.WriteString("\"")
				} else {
					cmdBuilder.WriteString(arg)
				}
			}
			return cmdBuilder.String()
		}
		return aliasCmd
	}

	return cmd
}

func (t *Terminal) executeCommand(cmd string) {
	// Раскрываем алиасы в команде
	expandedCmd := t.expandAliases(cmd)

	// Добавляем команду и ее вывод в НАЧАЛО вывода (чтобы сдвинуть старый вывод вниз)
	// Но сначала добавляем текущую команду
	commandSegment := LineSegment{
		Text:  "> " + cmd,
		Style: tcell.StyleDefault.Foreground(tcell.ColorGray).Background(tcell.ColorDefault),
	}

	// Создаем новый слайс и добавляем команду ПЕРВОЙ
	newOutput := []LineSegment{commandSegment}

	// Обрабатываем команду и получаем вывод
	resultSegments := t.processCommand(expandedCmd)

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

	// Очищаем список автодополнения
	// t.completionMatches = make([]string, 0)
	// t.completionIndex = 0
	// t.completionScrollOffset = 0
}

func (t *Terminal) processCommand(cmd string) []LineSegment {
	args := t.parseArgs(cmd)
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
	case "alias":
		segments = t.processAliasCommand(args)
	case "unalias":
		segments = t.processUnaliasCommand(args)
	case "export":
		segments = t.processExportCommand(args)
	case "env":
		segments = t.processEnvCommand()
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
		{"alias [имя[=команда]]", "Определить или показать алиасы"},
		{"unalias <имя>", "Удалить алиас"},
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

func (t *Terminal) processAliasCommand(args []string) []LineSegment {
	// Если нет аргументов, выводим список всех алиасов
	if len(args) <= 1 {
		if len(t.aliases) == 0 {
			return []LineSegment{{Text: "Алиасы не определены. Используйте 'alias имя=команда' для создания алиаса.", Style: tcell.StyleDefault.Foreground(tcell.ColorWhite)}}
		}

		var segments []LineSegment
		for alias, command := range t.aliases {
			line := fmt.Sprintf("%s='%s'", alias, command)
			segments = append(segments, LineSegment{Text: line, Style: tcell.StyleDefault.Foreground(tcell.ColorWhite)})
		}
		return segments
	}

	// Проверяем формат аргумента
	arg := args[1]
	parts := strings.SplitN(arg, "=", 2)
	if len(parts) != 2 {
		return []LineSegment{{Text: "Неправильный формат. Используйте: alias имя='команда'", Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	alias := parts[0]
	command := strings.Trim(parts[1], "'\"") // Убираем кавычки если есть

	// Добавляем или обновляем алиас
	t.aliases[alias] = command

	// Сохраняем алиасы в файл
	err := t.saveAliases()
	if err != nil {
		return []LineSegment{{Text: fmt.Sprintf("Ошибка сохранения алиаса: %s", err), Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	return []LineSegment{{Text: fmt.Sprintf("Алиас '%s' установлен как '%s'", alias, command), Style: tcell.StyleDefault.Foreground(tcell.ColorGreen)}}
}

func (t *Terminal) processUnaliasCommand(args []string) []LineSegment {
	if len(args) <= 1 {
		return []LineSegment{{Text: "Используйте: unalias имя_алиаса", Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	alias := args[1]

	// Проверяем, существует ли алиас
	if _, exists := t.aliases[alias]; !exists {
		return []LineSegment{{Text: fmt.Sprintf("Алиас '%s' не найден", alias), Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	// Удаляем алиас
	delete(t.aliases, alias)

	// Сохраняем алиасы в файл
	err := t.saveAliases()
	if err != nil {
		return []LineSegment{{Text: fmt.Sprintf("Ошибка сохранения алиасов: %s", err), Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	return []LineSegment{{Text: fmt.Sprintf("Алиас '%s' удален", alias), Style: tcell.StyleDefault.Foreground(tcell.ColorGreen)}}
}

func (t *Terminal) processExportCommand(args []string) []LineSegment {
	if len(args) <= 1 {
		return []LineSegment{{Text: "Используйте: export ИМЯ=значение", Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	// Разбираем аргумент на имя и значение
	parts := strings.SplitN(args[1], "=", 2)
	if len(parts) != 2 {
		return []LineSegment{{Text: "Неправильный формат. Используйте: export ИМЯ=значение", Style: tcell.StyleDefault.Foreground(tcell.ColorRed)}}
	}

	name := parts[0]
	value := parts[1]

	// Убираем кавычки если есть
	value = strings.Trim(value, "'\"")

	// Устанавливаем переменную окружения
	t.envVars[name] = value

	return []LineSegment{{Text: fmt.Sprintf("Переменная окружения '%s' установлена как '%s'", name, value), Style: tcell.StyleDefault.Foreground(tcell.ColorGreen)}}
}

func (t *Terminal) processEnvCommand() []LineSegment {
	var segments []LineSegment

	// Отображаем все переменные окружения
	for name, value := range t.envVars {
		line := fmt.Sprintf("%s=%s", name, value)
		segments = append(segments, LineSegment{Text: line, Style: tcell.StyleDefault.Foreground(tcell.ColorWhite)})
	}

	return segments
}

func (t *Terminal) processSystemCommand(args []string) []LineSegment {
	// Проверяем базовые команды которые должны работать без PTY
	switch args[0] {
	case "cd", "export", "alias", "unalias":
		// Эти команды обрабатываем напрямую
		return t.processCommand(strings.Join(args, " "))
	default:
		// Все остальные через PTY
		return t.processPtyCommand(args)
	}
}

// expandEnvVars заменяет переменные окружения в строке на их значения
func (t *Terminal) expandEnvVars(input string) string {
	// Заменяем переменные вида $ИМЯ или ${ИМЯ}
	re := regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)|\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	return re.ReplaceAllStringFunc(input, func(match string) string {
		// Извлекаем имя переменной
		var varName string
		if match[1] == '{' {
			// Формат ${ИМЯ}
			varName = match[2 : len(match)-1]
		} else {
			// Формат $ИМЯ
			varName = match[1:]
		}

		// Проверяем в наших переменных
		if value, exists := t.envVars[varName]; exists {
			return value
		}

		// Проверяем в системных переменных
		if value := os.Getenv(varName); value != "" {
			return value
		}

		// Если переменная не найдена, возвращаем оригинальную строку
		return match
	})
}

func (t *Terminal) handleKeyEvent(ev *tcell.EventKey) {
	// 🔴 АВАРИЙНЫЙ ВЫХОД ИЗ ЛЮБОГО РЕЖИМА
	if ev.Key() == tcell.KeyCtrlQ {
		log.Printf("🚨 Аварийный выход по Ctrl+Q")
		if t.inPtyMode && t.cmd != nil && t.cmd.Process != nil {
			log.Printf("⚡ Принудительное завершение процесса %d", t.cmd.Process.Pid)
			t.cmd.Process.Kill()
			t.inPtyMode = false
			t.ptmx = nil
			t.cmd = nil
		}
		return
	}

	if ev.Key() == tcell.KeyCtrlC && ev.Modifiers()&tcell.ModCtrl != 0 {
		log.Printf("🚨 Глобальный Ctrl+C")
		if t.inPtyMode && t.cmd != nil && t.cmd.Process != nil {
			t.cmd.Process.Signal(os.Interrupt)
		}
		return
	}

	// 🔴 ПРОСТАЯ логика для PTY режима
	if t.inPtyMode && t.ptmx != nil {
		log.Printf("⌨️  PTY режим - клавиша: %v, Rune: %q, Modifiers: %v", ev.Key(), ev.Rune(), ev.Modifiers())

		// 🔴 ПЕРВЫЕ ПРОВЕРЯЕМ SUDO
		if t.sudoPrompt != "" {
			t.handleSudoInput(ev)
			return
		}

		// 🔴 ОБРАБОТКА КОМБИНАЦИЙ С ALT ПЕРВОЙ
		if ev.Modifiers()&tcell.ModAlt != 0 {
			switch ev.Key() {
			case tcell.KeyF4:
				t.ptmx.Write([]byte{0x1b, 'O', 'S'}) // Alt+F4
				log.Printf("🔑 Отправлен Alt+F4")
				return
			}
		}

		switch ev.Key() {
		case tcell.KeyRune:
			t.ptmx.Write([]byte(string(ev.Rune())))

		case tcell.KeyEnter:
			t.ptmx.Write([]byte{'\n'})

		case tcell.KeyBackspace, tcell.KeyBackspace2:
			t.ptmx.Write([]byte{'\b'})

		case tcell.KeyTab:
			t.ptmx.Write([]byte{'\t'})

		case tcell.KeyEscape:
			t.ptmx.Write([]byte{0x1b})

		case tcell.KeyCtrlC:
			t.ptmx.Write([]byte{0x03}) // Ctrl+C

		case tcell.KeyCtrlD:
			t.ptmx.Write([]byte{0x04}) // Ctrl+D (EOF)

		case tcell.KeyCtrlZ:
			t.ptmx.Write([]byte{0x1a}) // Ctrl+Z (suspend)

		// 🔴 ФУНКЦИОНАЛЬНЫЕ КЛАВИШИ
		case tcell.KeyF1:
			t.ptmx.Write([]byte{0x1b, 'O', 'P'}) // F1
		case tcell.KeyF2:
			t.ptmx.Write([]byte{0x1b, 'O', 'Q'}) // F2
		case tcell.KeyF3:
			t.ptmx.Write([]byte{0x1b, 'O', 'R'}) // F3
		case tcell.KeyF4:
			t.ptmx.Write([]byte{0x1b, 'O', 'S'}) // F4
		case tcell.KeyF5:
			t.ptmx.Write([]byte{0x1b, '[', '1', '5', '~'}) // F5
		case tcell.KeyF6:
			t.ptmx.Write([]byte{0x1b, '[', '1', '7', '~'}) // F6
		case tcell.KeyF7:
			t.ptmx.Write([]byte{0x1b, '[', '1', '8', '~'}) // F7
		case tcell.KeyF8:
			t.ptmx.Write([]byte{0x1b, '[', '1', '9', '~'}) // F8
		case tcell.KeyF9:
			t.ptmx.Write([]byte{0x1b, '[', '2', '0', '~'}) // F9
		case tcell.KeyF10:
			t.ptmx.Write([]byte{0x1b, '[', '2', '1', '~'}) // F10
		case tcell.KeyF11:
			t.ptmx.Write([]byte{0x1b, '[', '2', '3', '~'}) // F11
		case tcell.KeyF12:
			t.ptmx.Write([]byte{0x1b, '[', '2', '4', '~'}) // F12

		default:
			log.Printf("❓ Необработанная клавиша в PTY: %v", ev.Key())
		}
		return
	}

	// Обработка клавиш в НЕ-PTY режиме
	switch ev.Key() {
	case tcell.KeyCtrlC, tcell.KeyCtrlQ:
		t.screen.Fini()
		os.Exit(0)

	case tcell.KeyEscape:
		// Отмена операций: очистка ввода и подсказки
		t.inputBuffer = make([]rune, 0)
		t.cursorPos = 0
		t.completionSuggestion = ""

	case tcell.KeyEnter:
		cmd := string(t.inputBuffer)
		if cmd != "" {
			t.executeCommand(cmd)
		}
		t.completionSuggestion = "" // Сбрасываем подсказку после выполнения

	case tcell.KeyUp:
		if ev.Modifiers() == tcell.ModCtrl {
			// Ctrl+стрелка вверх - прокрутка вывода вверх
			t.scrollOffset += 1
		} else {
			// Обычная стрелка вверх - навигация по истории
			if t.historyPos > 0 {
				t.historyPos--
				t.inputBuffer = []rune(t.history[t.historyPos])
				t.cursorPos = len(t.inputBuffer)
				t.updateCompletionSuggestion() // Обновляем подсказку для истории
			}
		}

	case tcell.KeyDown:
		if ev.Modifiers() == tcell.ModCtrl {
			// Ctrl+стрелка вниз - прокрутка вывода вниз
			t.scrollOffset = max(0, t.scrollOffset-1)
		} else {
			// Обычная стрелка вниз - навигация по истории
			if t.historyPos < len(t.history)-1 {
				t.historyPos++
				t.inputBuffer = []rune(t.history[t.historyPos])
				t.cursorPos = len(t.inputBuffer)
				t.updateCompletionSuggestion() // Обновляем подсказку для истории
			} else if t.historyPos == len(t.history)-1 {
				t.historyPos = len(t.history)
				t.inputBuffer = make([]rune, 0)
				t.cursorPos = 0
				t.completionSuggestion = "" // Сбрасываем подсказку
			}
		}

	case tcell.KeyPgUp:
		// Прокрутка основного вывода вверх
		t.scrollOffset += 5

	case tcell.KeyPgDn:
		// Прокрутка основного вывода вниз
		t.scrollOffset = max(0, t.scrollOffset-5)

	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if t.cursorPos > 0 && len(t.inputBuffer) > 0 {
			t.inputBuffer = append(t.inputBuffer[:t.cursorPos-1], t.inputBuffer[t.cursorPos:]...)
			t.cursorPos--
			t.updateCompletionSuggestion() // Обновляем подсказку!
		}

	case tcell.KeyDelete:
		if t.cursorPos < len(t.inputBuffer) {
			t.inputBuffer = append(t.inputBuffer[:t.cursorPos], t.inputBuffer[t.cursorPos+1:]...)
			t.updateCompletionSuggestion() // Обновляем подсказку!
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
		// ПРИНЯТИЕ ПОДСКАЗКИ
		if t.completionSuggestion != "" {
			t.inputBuffer = append(t.inputBuffer, []rune(t.completionSuggestion)...)
			t.cursorPos = len(t.inputBuffer)
			t.completionSuggestion = "" // Сбрасываем после принятия
		}
		// Если подсказки нет - ничего не делаем

	case tcell.KeyRune:
		// При вводе нового символа обновляем подсказку
		t.insertRune(ev.Rune())
		t.updateCompletionSuggestion()

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
	if t.cursorPos == len(t.inputBuffer) {
		t.inputBuffer = append(t.inputBuffer, r)
	} else {
		t.inputBuffer = append(t.inputBuffer[:t.cursorPos], append([]rune{r}, t.inputBuffer[t.cursorPos:]...)...)
	}
	t.cursorPos++
	// updateCompletionSuggestion теперь вызывается в handleKeyEvent
}

// // findCompletionMatches находит все совпадения для автодополнения
// func (t *Terminal) findCompletionMatches() []string {
// 	if len(t.inputBuffer) == 0 {
// 		return []string{}
// 	}
//
// 	currentInput := string(t.inputBuffer)
// 	var matches []string
// 	seen := make(map[string]bool) // Для исключения дубликатов
//
// 	// Сначала ищем точные префиксы в истории zsh
// 	for i := len(t.zshHistory) - 1; i >= 0; i-- {
// 		cmd := t.zshHistory[i]
// 		if strings.HasPrefix(cmd, currentInput) && cmd != currentInput {
// 			if !seen[cmd] {
// 				matches = append(matches, cmd)
// 				seen[cmd] = true
// 			}
// 		}
// 	}
//
// 	// Затем в внутренней истории
// 	for i := len(t.history) - 1; i >= 0; i-- {
// 		cmd := t.history[i]
// 		if strings.HasPrefix(cmd, currentInput) && cmd != currentInput {
// 			if !seen[cmd] {
// 				matches = append(matches, cmd)
// 				seen[cmd] = true
// 			}
// 		}
// 	}
//
// 	// Если точных префиксов не найдено, ищем частичные совпадения
// 	// Сначала в истории zsh
// 	if len(matches) == 0 {
// 		for i := len(t.zshHistory) - 1; i >= 0; i-- {
// 			cmd := t.zshHistory[i]
// 			if strings.Contains(cmd, currentInput) && cmd != currentInput {
// 				if !seen[cmd] {
// 					matches = append(matches, cmd)
// 					seen[cmd] = true
// 				}
// 			}
// 		}
//
// 		// Затем в внутренней истории
// 		for i := len(t.history) - 1; i >= 0; i-- {
// 			cmd := t.history[i]
// 			if strings.Contains(cmd, currentInput) && cmd != currentInput {
// 				if !seen[cmd] {
// 					matches = append(matches, cmd)
// 					seen[cmd] = true
// 				}
// 			}
// 		}
// 	}
//
// 	return matches
// }

// autoComplete выполняет автодополнение текущего ввода на основе истории команд
// .

// cycleCompletion выполняет циклическое переключение между вариантами автодополнения
// func (t *Terminal) cycleCompletion() {
// 	if len(t.completionMatches) == 0 {
// 		// Если нет сохраненных совпадений, пытаемся найти их
// 		t.autoComplete()
// 		return
// 	}
//
// 	// Переходим к следующему совпадению (циклически)
// 	t.completionIndex = (t.completionIndex + 1) % len(t.completionMatches)
//
// 	// Применяем текущее совпадение
// 	currentMatch := t.completionMatches[t.completionIndex]
// 	t.inputBuffer = []rune(currentMatch)
// 	t.cursorPos = len(t.inputBuffer)
// }

// updateCompletionList обновляет список вариантов автодополнения на основе текущего ввода
// func (t *Terminal) updateCompletionList() {
// 	// Находим все совпадения
// 	matches := t.findCompletionMatches()
//
// 	// Сохраняем совпадения
// 	t.completionMatches = matches
//
// 	// Если есть совпадения, устанавливаем индекс на первое совпадение
// 	if len(matches) > 0 {
// 		t.completionIndex = 0
// 	} else {
// 		// Если совпадений нет, сбрасываем индекс
// 		t.completionIndex = 0
// 	}
//
// 	// Сбрасываем смещение скролла
// 	t.completionScrollOffset = 0
// }
