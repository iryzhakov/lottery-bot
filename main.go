// v7
// выриант с весами для участников, чтобы чаще выпадали те, кто реже выпадал

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB
var appDir string
var envAdmins map[int64]bool

var pendingInputs = make(map[int64]struct {
	ChatID int64
	UserID int64
})

func getAppDir() string {
	ex, err := os.Executable()
	if err != nil {
		log.Fatal("Failed to get executable path:", err)
	}
	dir := filepath.Dir(ex)

	// Если запускаем через `go run`, бинарник находится во временной папке.
	// Проверяем сначала папку с исполняемым файлом, потом текущую рабочую директорию.
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		return dir
	}

	cwd, err := os.Getwd()
	if err == nil {
		if _, err := os.Stat(filepath.Join(cwd, ".env")); err == nil {
			return cwd
		}
	}

	return dir
}

func initDB() {
	var err error
	// dbPath := filepath.Join(appDir, "lottery.db")
	dbPath := "/data/lottery.db"
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		log.Fatal(err)
	}

	createTableQuery := `
    CREATE TABLE IF NOT EXISTS participants (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        chat_id INTEGER NOT NULL,
        user_id INTEGER NOT NULL,
        username TEXT NOT NULL,
        attempts INTEGER NOT NULL DEFAULT 0,
        UNIQUE(chat_id, user_id)
    );

    CREATE TABLE IF NOT EXISTS results (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        chat_id INTEGER NOT NULL,
        user_id INTEGER NOT NULL,
        username TEXT NOT NULL,
        date TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );

    CREATE TABLE IF NOT EXISTS pidor_time (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        chat_id INTEGER NOT NULL,
        last_pidor_time TIMESTAMP,
        last_pidor_username TEXT,
        UNIQUE(chat_id)
    );
	CREATE TABLE IF NOT EXISTS admins (
    user_id INTEGER PRIMARY KEY);`

	_, err = db.Exec(createTableQuery)
	if err != nil {
		log.Fatal(err)
	}

	// Check and alter existing tables if necessary
	alterTableQueries := []string{
		"ALTER TABLE participants ADD COLUMN chat_id INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE results ADD COLUMN chat_id INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE pidor_time ADD COLUMN chat_id INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE participants ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE participants ADD COLUMN weight INTEGER NOT NULL DEFAULT 1",
		"ALTER TABLE participants ADD COLUMN is_paused INTEGER NOT NULL DEFAULT 0",
	}

	for _, query := range alterTableQueries {
		_, err = db.Exec(query)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			log.Fatal(err)
		}
	}

	//убираем нулевые и отрицательные веса, если были
	_, err = db.Exec("UPDATE participants SET weight = 1 WHERE weight IS NULL OR weight <= 0")
	if err != nil {
		log.Println("Failed to normalize weights:", err)
	}

	// Initialize the pidor_time table if empty
	var count int
	db.QueryRow("SELECT COUNT(*) FROM pidor_time").Scan(&count)
	if count == 0 {
		db.Exec("INSERT INTO pidor_time (chat_id, last_pidor_time, last_pidor_username) VALUES (0, datetime('now', '-2 hours'), '')")
	}
}

func registerParticipant(chatID int64, userID int64, username string) string {
	var existingUserID int64
	var attempts int
	err := db.QueryRow("SELECT user_id, attempts FROM participants WHERE chat_id = ? AND user_id = ?", chatID, userID).Scan(&existingUserID, &attempts)
	if err != nil && err != sql.ErrNoRows {
		log.Fatal(err)
	}
	if existingUserID != 0 {
		attempts++
		if attempts >= 3 {
			return "похоже, ты не пидор, а дебил"
		}
		_, err = db.Exec("UPDATE participants SET attempts = ? WHERE chat_id = ? AND user_id = ?", attempts, chatID, userID)
		if err != nil {
			log.Fatal(err)
		}
		return "Ты уже в игре, не жми сюда больше!"
	}

	stmt, err := db.Prepare("INSERT INTO participants (chat_id, user_id, username, attempts) VALUES (?, ?, ?, 0)")
	if err != nil {
		log.Fatal(err)
	}
	_, err = stmt.Exec(chatID, userID, username)
	if err != nil {
		log.Println("User already registered:", username)
		return "Вы уже зарегистрированы!"
	} else {
		log.Printf("Registered participant: %s (%d) in chat %d", username, userID, chatID)

		// Ensure there is a pidor_time entry for this chat
		_, err = db.Exec("INSERT OR IGNORE INTO pidor_time (chat_id, last_pidor_time, last_pidor_username) VALUES (?, datetime('now', '-25 hours'), '')", chatID)
		if err != nil {
			log.Fatal(err)
		}

		return "Вы успешно зарегистрированы!"
	}
}

func getDisplayName(user *tgbotapi.User) string {
	if user.UserName != "" {
		return user.UserName
	}
	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if name != "" {
		return name
	}
	return fmt.Sprintf("%d", user.ID)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func formatMention(userID int64, username string) string {
	if username == "" {
		return fmt.Sprintf("[user](tg://user?id=%d)", userID)
	}
	if strings.HasPrefix(username, "@") {
		return username
	}
	if strings.Contains(username, " ") || isNumeric(username) {
		return fmt.Sprintf("[%s](tg://user?id=%d)", username, userID)
	}
	return "@" + username
}

// func getRandomParticipant(chatID int64) (int64, string) {
// 	rows, err := db.Query("SELECT user_id, username FROM participants WHERE chat_id = ? ORDER BY RANDOM() LIMIT 1", chatID)
// 	if err != nil {
// 		log.Fatal(err)
// 	}
// 	defer rows.Close()

// 	var userID int64
// 	var username string
// 	if rows.Next() {
// 		err = rows.Scan(&userID, &username)
// 		if err != nil {
// 			log.Fatal(err)
// 		}
// 	}
// 	return userID, username
// }
// выше код для рандома, ниже с весами, чтобы чаще выпадали те, кто реже выпадал

func getWeightedRandomParticipant(chatID int64) (int64, string) {
	rows, err := db.Query(`
	SELECT user_id, username, weight 
	FROM participants 
	WHERE chat_id = ? AND is_paused = 0
`, chatID)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	type User struct {
		ID       int64
		Username string
		Weight   int
	}

	var users []User
	totalWeight := 0

	for rows.Next() {
		var u User
		err := rows.Scan(&u.ID, &u.Username, &u.Weight)
		if err != nil {
			log.Fatal(err)
		}
		users = append(users, u)
		totalWeight += u.Weight
	}

	if totalWeight == 0 || len(users) == 0 {
		return 0, ""
	}

	r := rand.Intn(totalWeight)

	current := 0
	for _, u := range users {
		current += u.Weight
		if r < current {
			return u.ID, u.Username
		}
	}

	return 0, ""
}

func recordResult(chatID int64, userID int64, username string) {
	stmt, err := db.Prepare("INSERT INTO results (chat_id, user_id, username) VALUES (?, ?, ?)")
	if err != nil {
		log.Fatal(err)
	}
	_, err = stmt.Exec(chatID, userID, username)
	if err != nil {
		log.Fatal(err)
	} else {
		log.Printf("Recorded result: %s (%d) in chat %d", username, userID, chatID)
	}
}

func getStatistics(chatID int64) string {
	rows, err := db.Query("SELECT username, COUNT(*) as count FROM results WHERE chat_id = ? GROUP BY user_id, username ORDER BY count DESC", chatID)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var stats string
	for rows.Next() {
		var username string
		var count int
		err = rows.Scan(&username, &count)
		if err != nil {
			log.Fatal(err)
		}
		stats += fmt.Sprintf("%s: %d\n", username, count)
	}
	return stats
}

func getParticipants(chatID int64) string {
	rows, err := db.Query("SELECT username FROM participants WHERE chat_id = ?", chatID)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	var users string
	for rows.Next() {
		var username string
		err = rows.Scan(&username)
		if err != nil {
			log.Fatal(err)
		}
		users += formatMention(0, username) + "\n"
	}
	if users == "" {
		return "Нет зарегистрированных участников."
	}
	return "Зарегистрированные участники:\n" + users
}

func clearStatisticsAndResetTime(chatID int64) {
	_, err := db.Exec("DELETE FROM results WHERE chat_id = ?", chatID)
	if err != nil {
		log.Fatal(err)
	}
	// обнуляем к хуям время последнего запуска на рандомную дату в прошлом, чтобы при первом запуске после очистки сразу можно было выбрать пидора дня
	_, err = db.Exec("UPDATE pidor_time SET last_pidor_time = '1990-01-01 00:00:00', last_pidor_username = '' WHERE chat_id = ?", chatID)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Statistics and time reset for chat %d", chatID)
}

func resetPidorTimer(chatID int64) {
	_, err := db.Exec(`
		UPDATE pidor_time 
		SET last_pidor_time = '1990-01-01 00:00:00', last_pidor_username = '' 
		WHERE chat_id = ?
	`, chatID)

	if err != nil {
		log.Println("Failed to reset timer:", err)
	}
}

func deleteParticipant(chatID int64, userID int64) {
	_, err := db.Exec("DELETE FROM participants WHERE chat_id = ? AND user_id = ?", chatID, userID)
	if err != nil {
		log.Fatal(err)
	}
}

func deleteAllParticipants(chatID int64) {
	_, err := db.Exec("DELETE FROM participants WHERE chat_id = ?", chatID)
	if err != nil {
		log.Fatal(err)
	}
}

func getLastPidor(chatID int64) (time.Time, string) {
	var lastTime time.Time
	var lastUsername sql.NullString
	err := db.QueryRow("SELECT last_pidor_time, last_pidor_username FROM pidor_time WHERE chat_id = ?", chatID).Scan(&lastTime, &lastUsername)
	if err != nil {
		log.Fatal(err)
	}
	if lastUsername.Valid {
		return lastTime, lastUsername.String
	}
	return lastTime, ""
}

func updateLastPidor(chatID int64, userID int64, username string) {
	stmt, err := db.Prepare("UPDATE pidor_time SET last_pidor_time = datetime('now'), last_pidor_username = ? WHERE chat_id = ?")
	if err != nil {
		log.Fatal(err)
	}
	_, err = stmt.Exec(username, chatID)
	if err != nil {
		log.Fatal(err)
	}
}

func getCommands(isPrivate bool, isAdminUser bool) string {
	if isPrivate && isAdminUser {
		return `Админ-команды:
/admin - Админ панель
/add_user CHAT_ID USER_ID username - Добавить участника
/del_user CHAT_ID USER_ID - Удалить участника
/del_all CHAT_ID - Удалить всех участников
/clear CHAT_ID - Сбросить статистику
/setadmin USER_ID - Добавить админа
/deladmin - Удалить админа`
	}

	return `Доступные команды:
/start - Показать команды
/pidor - Выбрать пидорка
/pidor_stat - Показать статистику
/show_users - Показать участников`
}

func setBotCommands(token string) {
	setCommandsForScope := func(scope map[string]string, commands []map[string]string) {
		url := fmt.Sprintf("https://api.telegram.org/bot%s/setMyCommands", token)

		payload := map[string]interface{}{
			"scope":    scope,
			"commands": commands,
		}

		jsonData, err := json.Marshal(payload)
		if err != nil {
			log.Println("Failed to marshal commands:", err)
			return
		}

		resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
		if err != nil {
			log.Println("Failed to set bot commands:", err)
			return
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			log.Printf("Warning: Failed to set bot commands. Status: %d, Response: %s\n", resp.StatusCode, string(body))
		}
	}

	groupCommands := []map[string]string{
		{"command": "start", "description": "Показать команды"},
		{"command": "pidor", "description": "Выбрать пидорка"},
		{"command": "pidor_stat", "description": "Показать статистику"},
		{"command": "show_users", "description": "Показать участников"},
	}

	privateCommands := []map[string]string{
		{"command": "start", "description": "Показать команды"},
		{"command": "admin", "description": "Админ панель"},
		{"command": "add_user", "description": "Добавить участника"},
		{"command": "del_user", "description": "Удалить участника"},
		{"command": "del_all", "description": "Удалить всех участников"},
		{"command": "clear", "description": "Сбросить статистику"},
		{"command": "setadmin", "description": "Добавить админа"},
		{"command": "deladmin", "description": "Удалить админа"},
	}

	setCommandsForScope(map[string]string{"type": "all_group_chats"}, groupCommands)
	setCommandsForScope(map[string]string{"type": "all_private_chats"}, privateCommands)

	log.Println("Bot commands set successfully")
}

func parseAdminIDs(s string) map[int64]bool {
	admins := make(map[int64]bool)

	parts := strings.Split(s, ",")
	for _, p := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil {
			log.Println("Invalid ADMIN_ID:", p)
			continue
		}
		admins[id] = true
	}

	return admins
}

func isAdmin(userID int64) bool {
	var exists int
	err := db.QueryRow("SELECT 1 FROM admins WHERE user_id = ?", userID).Scan(&exists)
	return err == nil
}

func addAdmin(userID int64) {
	_, err := db.Exec("INSERT OR IGNORE INTO admins (user_id) VALUES (?)", userID)
	if err != nil {
		log.Println("Failed to add admin:", err)
	}
}

func delAdmin(userID int64) {
	_, err := db.Exec("DELETE FROM admins WHERE user_id = ?", userID)
	if err != nil {
		log.Println("Failed to delete admin:", err)
	}
}

func handleAdmin(update tgbotapi.Update, bot *tgbotapi.BotAPI) {
	text := update.Message.Text
	chatID := update.Message.Chat.ID

	// показать список
	if strings.HasPrefix(text, "/admin") {
		parts := strings.Split(text, " ")
		if len(parts) < 2 {
			bot.Send(tgbotapi.NewMessage(chatID, "Формат: /admin CHAT_ID"))
			return
		}

		targetChatID, _ := strconv.ParseInt(parts[1], 10, 64)

		rows, err := db.Query("SELECT username, weight FROM participants WHERE chat_id = ?", targetChatID)
		if err != nil {
			log.Println(err)
			return
		}
		defer rows.Close()

		msg := "Участники:\n"

		for rows.Next() {
			var username string
			var weight int
			rows.Scan(&username, &weight)

			msg += fmt.Sprintf("@%s — %d\n", username, weight)
		}

		bot.Send(tgbotapi.NewMessage(chatID, msg))
	}

	// изменить вес
	if strings.HasPrefix(text, "/set_weight") {
		// /set_weight CHAT_ID username weight
		parts := strings.Split(text, " ")
		if len(parts) < 4 {
			bot.Send(tgbotapi.NewMessage(chatID, "Формат: /set_weight CHAT_ID username weight"))
			return
		}

		targetChatID, _ := strconv.ParseInt(parts[1], 10, 64)
		username := parts[2]
		weight, _ := strconv.Atoi(parts[3])

		_, err := db.Exec("UPDATE participants SET weight = ? WHERE chat_id = ? AND username = ?", weight, targetChatID, username)
		if err != nil {
			log.Println(err)
			return
		}

		bot.Send(tgbotapi.NewMessage(chatID, "Готово 😈"))
	}
}

func showChats(bot *tgbotapi.BotAPI, chatID int64) { // Показать список чатов с зарегистрированными участниками
	rows, err := db.Query("SELECT DISTINCT chat_id FROM participants")
	if err != nil {
		log.Println(err)
		return
	}
	defer rows.Close()

	var buttons [][]tgbotapi.InlineKeyboardButton

	for rows.Next() {
		var cID int64
		rows.Scan(&cID)

		btn := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("Чат %d", cID),
			fmt.Sprintf("chat_%d", cID),
		)

		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(btn))
	}

	msg := tgbotapi.NewMessage(chatID, "Выбери чат:")
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(buttons...)

	bot.Send(msg)
}

func showAdmins(bot *tgbotapi.BotAPI, chatID int64, currentAdminID int64) {
	rows, err := db.Query("SELECT user_id FROM admins")
	if err != nil {
		log.Println(err)
		return
	}
	defer rows.Close()

	var buttons [][]tgbotapi.InlineKeyboardButton
	for rows.Next() {
		var userID int64
		rows.Scan(&userID)

		if envAdmins[userID] || userID == currentAdminID {
			continue
		}

		btn := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("Админ %d", userID),
			fmt.Sprintf("deladmin_%d", userID),
		)
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(btn))
	}

	if len(buttons) == 0 {
		bot.Send(tgbotapi.NewMessage(chatID, "Нет удаляемых админов."))
		return
	}

	msg := tgbotapi.NewMessage(chatID, "Выбери админа для удаления:")
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(buttons...)
	bot.Send(msg)
}

func showUsers(bot *tgbotapi.BotAPI, chatID int64, targetChatID int64) { // Показать участников выбранного чата
	rows, err := db.Query(`
	SELECT user_id, username, weight, is_paused 
	FROM participants 
	WHERE chat_id = ?
`, targetChatID)
	if err != nil {
		log.Println(err)
		return
	}
	defer rows.Close()

	var buttons [][]tgbotapi.InlineKeyboardButton

	for rows.Next() {
		var userID int64
		var username string
		var weight int
		var isPaused int

		rows.Scan(&userID, &username, &weight, &isPaused)

		status := "▶️ active"
		if isPaused == 1 {
			status = "⏸ paused"
		}

		btn := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("%s (вес: %d, %s)", formatMention(userID, username), weight, status),
			fmt.Sprintf("user_%d_%d", targetChatID, userID),
		)

		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(btn))
	}

	clearStatsBtn := tgbotapi.NewInlineKeyboardButtonData(
		"🧹 Сбросить статистику",
		fmt.Sprintf("clear_stats_%d", targetChatID),
	)

	resetTimerBtn := tgbotapi.NewInlineKeyboardButtonData(
		"⏱ Обнулить таймер",
		fmt.Sprintf("reset_timer_%d", targetChatID),
	)

	backBtn := tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "goback")

	buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(clearStatsBtn))
	buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(resetTimerBtn))
	buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(backBtn))

	msg := tgbotapi.NewMessage(chatID, "Выбери участника:")
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(buttons...)

	bot.Send(msg)
}

func showUserControls(bot *tgbotapi.BotAPI, chatID int64, targetChatID int64, userID int64) { // Показать кнопки управления весом для выбранного участника
	var username string
	var weight int

	var isPaused int

	err := db.QueryRow(
		"SELECT username, weight, is_paused FROM participants WHERE chat_id = ? AND user_id = ?",
		targetChatID, userID,
	).Scan(&username, &weight, &isPaused)

	if err != nil {
		log.Println(err)
		return
	}

	statusText := "▶️ Активен"
	pauseButtonText := "⏸ Пауза"
	pauseAction := "pause"

	if isPaused == 1 {
		statusText = "⏸ На паузе"
		pauseButtonText = "▶️ Старт"
		pauseAction = "start"
	}

	text := fmt.Sprintf(
		"%s\nID: %d\nВес: %d\nСтатус: %s",
		formatMention(userID, username),
		userID,
		weight,
		statusText,
	)

	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("+10", fmt.Sprintf("weight_%d_%d_10", targetChatID, userID)),
			tgbotapi.NewInlineKeyboardButtonData("+1", fmt.Sprintf("weight_%d_%d_1", targetChatID, userID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("-10", fmt.Sprintf("weight_%d_%d_-10", targetChatID, userID)),
			tgbotapi.NewInlineKeyboardButtonData("-1", fmt.Sprintf("weight_%d_%d_-1", targetChatID, userID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("Сброс", fmt.Sprintf("weight_%d_%d_reset", targetChatID, userID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData(
				pauseButtonText,
				fmt.Sprintf("status_%d_%d_%s", targetChatID, userID, pauseAction),
			),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("Введи значение", fmt.Sprintf("weight_%d_%d_add", targetChatID, userID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", fmt.Sprintf("back_%d", targetChatID)),
		},
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(buttons...)

	bot.Send(msg)
}

func setPendingInput(adminID int64, chatID int64, userID int64) { // Сохранить состояние ожидания ввода веса для конкретного участника
	pendingInputs[adminID] = struct {
		ChatID int64
		UserID int64
	}{
		ChatID: chatID,
		UserID: userID,
	}
}

func getRandomNo() string {
	phrases := []string{
		"Сережа, блядь",
		"Хули ты сюда лезешь?",
		"Я, блядь, просто чувствую, что это Сережа",
		"Похоже, сегодня ты хочешь стать пидором?",
		"Руки, нахуй, прочь от этой кнопки, чувак",
		"Ты кто по жизни? Сережа что ли?",
	}
	return phrases[rand.Intn(len(phrases))]
}

func isPrivateChat(message *tgbotapi.Message) bool {
	return message.Chat.Type == "private"
}

func isGroupChat(message *tgbotapi.Message) bool {
	return message.Chat.IsGroup() || message.Chat.IsSuperGroup()
}

// авторегистрация
func autoRegisterParticipant(chatID int64, user *tgbotapi.User) {
	if user == nil || user.IsBot {
		return
	}

	username := getDisplayName(user)

	_, err := db.Exec(`
		INSERT OR IGNORE INTO participants (chat_id, user_id, username, attempts, weight, is_paused)
VALUES (?, ?, ?, 0, 1, 0)
	`, chatID, user.ID, username)

	if err != nil {
		log.Println("Failed to auto-register user:", err)
		return
	}

	_, err = db.Exec(`
		INSERT OR IGNORE INTO pidor_time (chat_id, last_pidor_time, last_pidor_username)
		VALUES (?, datetime('now', '-25 hours'), '')
	`, chatID)

	if err != nil {
		log.Println("Failed to init pidor_time:", err)
	}
}

// ручная регистрация
func addParticipantByAdmin(targetChatID int64, userID int64, username string) string {
	username = strings.TrimSpace(strings.TrimPrefix(username, "@"))

	if username == "" {
		username = fmt.Sprintf("%d", userID)
	}

	_, err := db.Exec(`
		INSERT OR IGNORE INTO participants (chat_id, user_id, username, attempts, weight, is_paused)
		VALUES (?, ?, ?, 0, 1, 0)
	`, targetChatID, userID, username)

	if err != nil {
		log.Println("Failed to add participant:", err)
		return "Не смог добавить участника."
	}

	_, err = db.Exec(`
		INSERT OR IGNORE INTO pidor_time (chat_id, last_pidor_time, last_pidor_username)
		VALUES (?, datetime('now', '-25 hours'), '')
	`, targetChatID)

	if err != nil {
		log.Println("Failed to init pidor_time:", err)
	}

	return "Участник добавлен 😈"
}

// для одного сообщения от бота с редактированием обновлений
func editUserControls(bot *tgbotapi.BotAPI, callback *tgbotapi.CallbackQuery, targetChatID int64, userID int64) {
	var username string
	var weight int
	var isPaused int

	err := db.QueryRow(
		"SELECT username, weight, is_paused FROM participants WHERE chat_id = ? AND user_id = ?",
		targetChatID, userID,
	).Scan(&username, &weight, &isPaused)

	if err != nil {
		log.Println(err)
		return
	}

	statusText := "▶️ Активен"
	pauseButtonText := "⏸ Пауза"
	pauseAction := "pause"

	if isPaused == 1 {
		statusText = "⏸ На паузе"
		pauseButtonText = "▶️ Старт"
		pauseAction = "start"
	}

	text := fmt.Sprintf(
		"%s\nID: %d\nВес: %d\nСтатус: %s",
		formatMention(userID, username),
		userID,
		weight,
		statusText,
	)

	buttons := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("+10", fmt.Sprintf("weight_%d_%d_10", targetChatID, userID)),
			tgbotapi.NewInlineKeyboardButtonData("+1", fmt.Sprintf("weight_%d_%d_1", targetChatID, userID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("-10", fmt.Sprintf("weight_%d_%d_-10", targetChatID, userID)),
			tgbotapi.NewInlineKeyboardButtonData("-1", fmt.Sprintf("weight_%d_%d_-1", targetChatID, userID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("Сброс", fmt.Sprintf("weight_%d_%d_reset", targetChatID, userID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData(
				pauseButtonText,
				fmt.Sprintf("status_%d_%d_%s", targetChatID, userID, pauseAction),
			),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("Введи значение", fmt.Sprintf("weight_%d_%d_add", targetChatID, userID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", fmt.Sprintf("back_%d", targetChatID)),
		},
	}

	edit := tgbotapi.NewEditMessageTextAndMarkup(
		callback.Message.Chat.ID,
		callback.Message.MessageID,
		text,
		tgbotapi.NewInlineKeyboardMarkup(buttons...),
	)

	bot.Send(edit)
}

func main() {
	appDir = getAppDir()
	envPath := filepath.Join(appDir, ".env")

	// err := godotenv.Load(envPath)
	// if err != nil {
	// 	log.Fatal("Failed to load .env file at:", envPath, "error:", err)
	// }
	err := godotenv.Load(envPath)
	if err != nil {
		log.Println("No .env file found, using environment variables")
	}

	token := os.Getenv("BOT_TOKEN")
	// adminStr := os.Getenv("ADMIN_ID")
	adminStr := os.Getenv("ADMIN_IDS")

	if adminStr == "" {
		log.Fatal("ADMIN_IDS not found in .env")
	}

	envAdmins = parseAdminIDs(adminStr)
	if token == "" {
		log.Fatal("BOT_TOKEN not found in .env file at:", envPath)
	}

	// adminID, err := strconv.ParseInt(adminStr, 10, 64)
	// if err != nil {
	// 	log.Fatal("Invalid ADMIN_ID in .env:", err)
	// }

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Panic("Failed to create bot API:", err)
	}
	setBotCommands(token)

	bot.Debug = true
	log.Printf("Authorized on account %s", bot.Self.UserName)

	initDB()

	// Add initial admins from .env to database
	for adminID := range envAdmins {
		addAdmin(adminID)
	}

	// rand.Seed(time.Now().UnixNano())

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	for update := range updates {

		if update.CallbackQuery != nil {

			cb := tgbotapi.NewCallback(update.CallbackQuery.ID, "")
			bot.Request(cb)

			data := update.CallbackQuery.Data
			if !isAdmin(update.CallbackQuery.From.ID) {
				bot.Send(tgbotapi.NewMessage(update.CallbackQuery.Message.Chat.ID, getRandomNo()))
				continue
			}
			if data == "goback" {
				cb := tgbotapi.NewCallback(update.CallbackQuery.ID, "")
				bot.Request(cb)

				showChats(bot, update.CallbackQuery.Message.Chat.ID)
			}

			if strings.HasPrefix(data, "chat_") {
				parts := strings.Split(data, "_")
				targetChatID, _ := strconv.ParseInt(parts[1], 10, 64)

				showUsers(bot, update.CallbackQuery.Message.Chat.ID, targetChatID)
			}
			// if strings.HasPrefix(data, "chat_") {
			// 	bot.Send(tgbotapi.NewMessage(
			// 		update.CallbackQuery.Message.Chat.ID,
			// 		"Чат выбран: "+data,
			// 	))
			// }

			if strings.HasPrefix(data, "user_") {
				parts := strings.Split(data, "_")

				targetChatID, _ := strconv.ParseInt(parts[1], 10, 64)
				userID, _ := strconv.ParseInt(parts[2], 10, 64)

				editUserControls(bot, update.CallbackQuery, targetChatID, userID)
			}

			if strings.HasPrefix(data, "back_") {
				parts := strings.Split(data, "_")

				targetChatID, _ := strconv.ParseInt(parts[1], 10, 64)

				showUsers(bot, update.CallbackQuery.Message.Chat.ID, targetChatID)
			}

			if strings.HasPrefix(data, "deladmin_") {
				parts := strings.Split(data, "_")
				adminID, _ := strconv.ParseInt(parts[1], 10, 64)

				if !isAdmin(update.CallbackQuery.From.ID) {
					bot.Send(tgbotapi.NewMessage(update.CallbackQuery.Message.Chat.ID, getRandomNo()))
					continue
				}

				if envAdmins[adminID] || adminID == update.CallbackQuery.From.ID {
					bot.Send(tgbotapi.NewMessage(update.CallbackQuery.Message.Chat.ID, "Нельзя удалить этого админа."))
					continue
				}

				delAdmin(adminID)
				bot.Send(tgbotapi.NewMessage(update.CallbackQuery.Message.Chat.ID, "Админ удален 😈"))
			}

			if strings.HasPrefix(data, "clear_stats_") {
				parts := strings.Split(data, "_")
				targetChatID, _ := strconv.ParseInt(parts[2], 10, 64)

				clearStatisticsAndResetTime(targetChatID)

				bot.Send(tgbotapi.NewMessage(
					update.CallbackQuery.Message.Chat.ID,
					fmt.Sprintf("Статистика сброшена для чата %d", targetChatID),
				))

				showUsers(bot, update.CallbackQuery.Message.Chat.ID, targetChatID)
				continue
			}

			if strings.HasPrefix(data, "reset_timer_") {
				parts := strings.Split(data, "_")
				targetChatID, _ := strconv.ParseInt(parts[2], 10, 64)

				resetPidorTimer(targetChatID)

				bot.Send(tgbotapi.NewMessage(
					update.CallbackQuery.Message.Chat.ID,
					fmt.Sprintf("Таймер обнулён для чата %d", targetChatID),
				))

				showUsers(bot, update.CallbackQuery.Message.Chat.ID, targetChatID)
				continue
			}

			if strings.HasPrefix(data, "status_") {
				parts := strings.Split(data, "_")

				targetChatID, _ := strconv.ParseInt(parts[1], 10, 64)
				userID, _ := strconv.ParseInt(parts[2], 10, 64)
				action := parts[3]

				isPaused := 0
				if action == "pause" {
					isPaused = 1
				}

				_, err := db.Exec(`
		UPDATE participants 
		SET is_paused = ? 
		WHERE chat_id = ? AND user_id = ?
	`, isPaused, targetChatID, userID)

				if err != nil {
					log.Println(err)
					continue
				}

				editUserControls(bot, update.CallbackQuery, targetChatID, userID)
				continue
			}

			if strings.HasPrefix(data, "weight_") {
				parts := strings.Split(data, "_")

				targetChatID, _ := strconv.ParseInt(parts[1], 10, 64)
				userID, _ := strconv.ParseInt(parts[2], 10, 64)

				action := parts[3]

				if action == "add" {

					// 👉 сохраняем состояние (что ждём ввод)
					setPendingInput(update.CallbackQuery.From.ID, targetChatID, userID)
					bot.Send(tgbotapi.NewMessage(
						update.CallbackQuery.Message.Chat.ID,
						"Введи новый вес числом:",
					))
					continue
				}

				if action == "reset" {
					db.Exec("UPDATE participants SET weight = 1 WHERE chat_id = ? AND user_id = ?", targetChatID, userID)
				} else {
					delta, _ := strconv.Atoi(action)

					db.Exec(`
			UPDATE participants 
			SET weight = MAX(1, weight + ?) 
			WHERE chat_id = ? AND user_id = ?
		`, delta, targetChatID, userID)
				}

				// 🔥 после изменения — обновляем меню
				editUserControls(bot, update.CallbackQuery, targetChatID, userID)
			}

			continue
		}

		if update.Message == nil {
			continue
		}

		chatID := update.Message.Chat.ID
		log.Printf("Received message in chat %d: %s", chatID, update.Message.Text)

		userID := update.Message.From.ID
		if isGroupChat(update.Message) {
			autoRegisterParticipant(chatID, update.Message.From)
		}
		if pending, ok := pendingInputs[userID]; ok {

			value, err := strconv.Atoi(update.Message.Text)
			if err != nil {
				bot.Send(tgbotapi.NewMessage(chatID, "Введи нормальное число"))
				continue
			}
			_, err = db.Exec(
				"UPDATE participants SET weight = ? WHERE chat_id = ? AND user_id = ?",
				value, pending.ChatID, pending.UserID,
			)
			if err != nil {
				log.Println(err)
				continue
			}
			delete(pendingInputs, userID)
			bot.Send(tgbotapi.NewMessage(chatID, "Done"))
			showUsers(bot, chatID, pending.ChatID)
			continue
		}

		switch update.Message.Command() {

		case "admin":
			if !isPrivateChat(update.Message) {
				continue
			}

			if !isAdmin(update.Message.From.ID) {
				bot.Send(tgbotapi.NewMessage(chatID, getRandomNo()))
				continue
			}

			showChats(bot, chatID)
			continue

		case "add_user":
			if !isPrivateChat(update.Message) {
				continue
			}

			if !isAdmin(update.Message.From.ID) {
				bot.Send(tgbotapi.NewMessage(chatID, getRandomNo()))
				continue
			}

			parts := strings.Split(update.Message.Text, " ")
			if len(parts) < 4 {
				bot.Send(tgbotapi.NewMessage(chatID, "Формат: /add_user CHAT_ID USER_ID username"))
				continue
			}

			targetChatID, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				bot.Send(tgbotapi.NewMessage(chatID, "Неверный CHAT_ID"))
				continue
			}

			targetUserID, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				bot.Send(tgbotapi.NewMessage(chatID, "Неверный USER_ID"))
				continue
			}

			username := parts[3]

			response := addParticipantByAdmin(targetChatID, targetUserID, username)
			bot.Send(tgbotapi.NewMessage(chatID, response))
			continue

		case "del_user":
			if !isPrivateChat(update.Message) {
				continue
			}

			if !isAdmin(update.Message.From.ID) {
				bot.Send(tgbotapi.NewMessage(chatID, getRandomNo()))
				continue
			}

			parts := strings.Split(update.Message.Text, " ")
			if len(parts) < 3 {
				bot.Send(tgbotapi.NewMessage(chatID, "Формат: /del_user CHAT_ID USER_ID"))
				continue
			}

			targetChatID, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				bot.Send(tgbotapi.NewMessage(chatID, "Неверный CHAT_ID"))
				continue
			}

			targetUserID, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				bot.Send(tgbotapi.NewMessage(chatID, "Неверный USER_ID"))
				continue
			}

			deleteParticipant(targetChatID, targetUserID)
			bot.Send(tgbotapi.NewMessage(chatID, "Участник удалён."))
			continue

		case "del_all":
			if !isPrivateChat(update.Message) {
				continue
			}

			if !isAdmin(update.Message.From.ID) {
				bot.Send(tgbotapi.NewMessage(chatID, getRandomNo()))
				continue
			}

			parts := strings.Split(update.Message.Text, " ")
			if len(parts) < 2 {
				bot.Send(tgbotapi.NewMessage(chatID, "Формат: /del_all CHAT_ID"))
				continue
			}

			targetChatID, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				bot.Send(tgbotapi.NewMessage(chatID, "Неверный CHAT_ID"))
				continue
			}

			deleteAllParticipants(targetChatID)
			bot.Send(tgbotapi.NewMessage(chatID, "Все участники удалены."))
			continue

		case "clear":
			if !isPrivateChat(update.Message) {
				continue
			}

			if !isAdmin(update.Message.From.ID) {
				bot.Send(tgbotapi.NewMessage(chatID, getRandomNo()))
				continue
			}

			parts := strings.Split(update.Message.Text, " ")
			if len(parts) < 2 {
				bot.Send(tgbotapi.NewMessage(chatID, "Формат: /clear CHAT_ID"))
				continue
			}

			targetChatID, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				bot.Send(tgbotapi.NewMessage(chatID, "Неверный CHAT_ID"))
				continue
			}

			clearStatisticsAndResetTime(targetChatID)
			bot.Send(tgbotapi.NewMessage(chatID, "Статистика сброшена!"))
			continue

		case "setadmin":
			if !isPrivateChat(update.Message) {
				continue
			}

			if !isAdmin(update.Message.From.ID) {
				bot.Send(tgbotapi.NewMessage(chatID, getRandomNo()))
				continue
			}

			parts := strings.Split(update.Message.Text, " ")
			if len(parts) < 2 {
				bot.Send(tgbotapi.NewMessage(chatID, "Формат: /setadmin USER_ID"))
				continue
			}

			id, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				bot.Send(tgbotapi.NewMessage(chatID, "Неверный ID"))
				continue
			}

			addAdmin(id)
			bot.Send(tgbotapi.NewMessage(chatID, "Админ добавлен 😈"))
			continue

		case "deladmin":
			if !isPrivateChat(update.Message) {
				continue
			}

			if !isAdmin(update.Message.From.ID) {
				bot.Send(tgbotapi.NewMessage(chatID, getRandomNo()))
				continue
			}

			showAdmins(bot, chatID, update.Message.From.ID)
			continue

		case "set_weight":
			if !isPrivateChat(update.Message) {
				continue
			}

			if !isAdmin(update.Message.From.ID) {
				bot.Send(tgbotapi.NewMessage(chatID, getRandomNo()))
				continue
			}

			parts := strings.Split(update.Message.Text, " ")
			if len(parts) < 4 {
				bot.Send(tgbotapi.NewMessage(chatID, "Формат: /set_weight CHAT_ID username weight"))
				continue
			}

			targetChatID, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				bot.Send(tgbotapi.NewMessage(chatID, "Неверный CHAT_ID"))
				continue
			}

			username := strings.TrimPrefix(parts[2], "@")

			weight, err := strconv.Atoi(parts[3])
			if err != nil {
				bot.Send(tgbotapi.NewMessage(chatID, "Неверный вес"))
				continue
			}

			if weight < 1 {
				weight = 1
			}

			_, err = db.Exec(
				"UPDATE participants SET weight = ? WHERE chat_id = ? AND username = ?",
				weight,
				targetChatID,
				username,
			)

			if err != nil {
				log.Println(err)
				continue
			}

			bot.Send(tgbotapi.NewMessage(chatID, "Готово 😈"))
			continue
		}

		switch update.Message.Command() {

		case "start":
			msg := tgbotapi.NewMessage(chatID, getCommands(isPrivateChat(update.Message), isAdmin(update.Message.From.ID)))
			bot.Send(msg)

		case "pidor":
			if !isGroupChat(update.Message) {
				bot.Send(tgbotapi.NewMessage(chatID, "Розыгрыш работает только в группе."))
				continue
			}

			lastPidorTime, lastPidorUsername := getLastPidor(chatID)

			loc, err := time.LoadLocation("Europe/Warsaw")
			if err != nil {
				log.Fatal(err)
			}

			currentTime := time.Now().In(loc)
			lastPidorTimeInLoc := lastPidorTime.In(loc)

			currentDay := currentTime.Format("2006-01-02")
			lastPidorDay := lastPidorTimeInLoc.Format("2006-01-02")

			if currentDay == lastPidorDay {
				msg := tgbotapi.NewMessage(chatID,
					fmt.Sprintf("🔥🔥🔥 Сегодня пидор дня: %s!\nСледующий розыгрыш будет доступен завтра.",
						lastPidorUsername,
					),
				)

				msg.ParseMode = "Markdown"

				bot.Send(msg)

			} else {
				phrases := []string{
					"Ищу пидора дня...",
					"Советуюсь с коллегами...",
					"Проверяю базу данных...",
					"Анализирую участников...",
					"Определяю судьбу...",
					"Ищем самого мутного типа…",
					"Сча вычислим, кто тут косячит…",
					"Копаемся в ваших грешках…",
					"Проверяем, кто сегодня накосячил…",
					"Сканируем чат на долбоебизм…",
					"Кто-то сегодня попал, чувствуем…",
					"Сча найдём главного виновника…",
					"Анализируем уровень кринжа…",
					"Определяем, кто сегодня отличился по-плохому…",
					"Проверяем, у кого сегодня минус карма…",
					"Сча система выдаст самого подозрительного…",
					"Кто сегодня словил неудачу, подождите…",
					"Собираем доказательства против вас…",
					"Перебираем список косяков…",
					"Сча выберем самого ‘удачливого’…",
					"Кто-то из вас сейчас огребёт…",
					"Проверяем, кто нарывается…",
					"Секундочку, почти вычислили…",
					"Сча рандом решит вашу судьбу…",
					"Готовьтесь, сейчас будет больно…",
				}

				for i := 0; i < 5; i++ {
					phrase := phrases[rand.Intn(len(phrases))]
					msg := tgbotapi.NewMessage(chatID, phrase)
					bot.Send(msg)
					time.Sleep(1 * time.Second)
				}

				userID, username := getWeightedRandomParticipant(chatID)

				if userID != 0 {
					recordResult(chatID, userID, username)
					updateLastPidor(chatID, userID, formatMention(userID, username))

					msg := tgbotapi.NewMessage(chatID,
						fmt.Sprintf("🔥 Сегодня пидор дня 🎉: %s!", formatMention(userID, username)),
					)

					msg.ParseMode = "Markdown"

					bot.Send(msg)

				} else {
					msg := tgbotapi.NewMessage(chatID, "Нет зарегистрированных участников.")
					bot.Send(msg)
				}
			}

		case "pidor_stat":
			if !isGroupChat(update.Message) {
				continue
			}

			stats := getStatistics(chatID)
			msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🔥 Статистика пидоров дня 🎉:\n%s", stats))
			bot.Send(msg)

		case "show_users":
			if !isGroupChat(update.Message) {
				continue
			}

			response := getParticipants(chatID)
			msg := tgbotapi.NewMessage(chatID, response)
			bot.Send(msg)
		}
	}
}
