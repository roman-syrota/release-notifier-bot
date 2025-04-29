package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/mattn/go-sqlite3"
)

type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
}

type Bot struct {
	bot           *tgbotapi.BotAPI
	db            *sql.DB
	checkInterval time.Duration
}

func NewBot(token string, dbPath string, checkInterval time.Duration) (*Bot, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}

	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=5000", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	// Restrict to one connection to avoid SQLite locks
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS repositories (
			url TEXT PRIMARY KEY,
			latest_release TEXT
		);
		CREATE TABLE IF NOT EXISTS subscribers (
			chat_id INTEGER PRIMARY KEY
		);
	`)
	if err != nil {
		return nil, err
	}

	return &Bot{
		bot:           bot,
		db:            db,
		checkInterval: checkInterval,
	}, nil
}

func (b *Bot) Start() {
	go b.checkReleases()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.bot.GetUpdatesChan(u)
	for update := range updates {
		if update.Message == nil {
			continue
		}

		chatID := update.Message.Chat.ID
		b.addSubscriber(chatID)

		if update.Message.IsCommand() {
			b.handleCommand(update.Message)
		}
	}
}

func (b *Bot) addSubscriber(chatID int64) {
	_, err := b.db.Exec("INSERT OR IGNORE INTO subscribers (chat_id) VALUES (?)", chatID)
	if err != nil {
		log.Printf("Error adding subscriber: %v", err)
	}
}

func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	command := msg.Command()
	args := msg.CommandArguments()

	switch command {
	case "list":
		b.handleListCommand(chatID)
	case "latest":
		b.handleLatestCommand(chatID, args)
	case "add":
		b.handleAddCommand(chatID, args)
	default:
		b.sendMessage(chatID, "Available commands:\n/list - Show monitored repositories\n/latest <repo_url> - Show latest release\n/add <repo_url> - Add repository to monitor")
	}
}

func (b *Bot) handleListCommand(chatID int64) {
	rows, err := b.db.Query("SELECT url FROM repositories")
	if err != nil {
		b.sendMessage(chatID, "Error fetching repositories")
		return
	}
	defer rows.Close()

	var repos []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			log.Printf("Error scanning row: %v", err)
			continue
		}
		repos = append(repos, url)
	}

	if len(repos) == 0 {
		b.sendMessage(chatID, "No repositories are being monitored")
		return
	}

	b.sendMessage(chatID, "Monitored repositories:\n"+strings.Join(repos, "\n"))
}

func (b *Bot) handleLatestCommand(chatID int64, repoURL string) {
	if repoURL == "" {
		b.sendMessage(chatID, "Please provide a repository URL")
		return
	}

	var latestRelease string
	err := b.db.QueryRow("SELECT latest_release FROM repositories WHERE url = ?", repoURL).Scan(&latestRelease)
	if err == sql.ErrNoRows {
		b.sendMessage(chatID, "Repository not found")
		return
	} else if err != nil {
		b.sendMessage(chatID, "Error fetching latest release")
		return
	}

	if latestRelease == "" {
		b.sendMessage(chatID, "No releases found for this repository")
		return
	}

	b.sendMessage(chatID, fmt.Sprintf("Latest release for %s: %s", repoURL, latestRelease))
}

func (b *Bot) handleAddCommand(chatID int64, repoURL string) {
	if !strings.HasPrefix(repoURL, "https://github.com/") || !strings.Contains(repoURL, "/releases") {
		b.sendMessage(chatID, "Invalid GitHub releases URL")
		return
	}

	_, err := b.db.Exec("INSERT OR IGNORE INTO repositories (url, latest_release) VALUES (?, '')", repoURL)
	if err != nil {
		b.sendMessage(chatID, "Error adding repository")
		return
	}

	b.sendMessage(chatID, fmt.Sprintf("Added repository: %s", repoURL))
}

func (b *Bot) checkReleases() {
	ticker := time.NewTicker(b.checkInterval)
	for range ticker.C {
		rows, err := b.db.Query("SELECT url, latest_release FROM repositories")
		if err != nil {
			log.Printf("Error fetching repositories: %v", err)
			continue
		}

		for rows.Next() {
			var url, latestRelease string
			if err := rows.Scan(&url, &latestRelease); err != nil {
				log.Printf("Error scanning row: %v", err)
				continue
			}

			b.checkRepository(url, latestRelease)
		}
		rows.Close()
	}
}

func (b *Bot) checkRepository(repoURL, latestRelease string) {
	apiURL := strings.Replace(repoURL, "https://github.com/", "https://api.github.com/repos/", 1)
	apiURL = strings.Replace(apiURL, "/releases", "/releases/latest", 1)

	resp, err := http.Get(apiURL)
	if err != nil {
		log.Printf("Error fetching release for %s: %v", repoURL, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("Non-200 status for %s: %d", repoURL, resp.StatusCode)
		return
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		log.Printf("Error decoding release for %s: %v", repoURL, err)
		return
	}

	if release.TagName != latestRelease {
		b.notifySubscribers(repoURL, &release)
		_, err = b.db.Exec("UPDATE repositories SET latest_release = ? WHERE url = ?", release.TagName, repoURL)
		if err != nil {
			log.Printf("Error updating latest release: %v", err)
		}
	}
}

func (b *Bot) notifySubscribers(repoURL string, release *Release) {
	rows, err := b.db.Query("SELECT chat_id FROM subscribers")
	if err != nil {
		log.Printf("Error fetching subscribers: %v", err)
		return
	}
	defer rows.Close()

	message := fmt.Sprintf("New release for %s\nVersion: %s\nName: %s\nPublished: %s\nDetails: %s",
		repoURL, release.TagName, release.Name, release.PublishedAt.Format(time.RFC822), release.HTMLURL)

	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			log.Printf("Error scanning subscriber: %v", err)
			continue
		}
		b.sendMessage(chatID, message)
	}
}

func (b *Bot) sendMessage(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := b.bot.Send(msg)
	if err != nil {
		log.Printf("Error sending message to %d: %v", chatID, err)
	}
}

func main() {
	token := "YOUR_TELEGRAM_BOT_TOKEN" // Replace with your bot token
	dbPath := "bot.db"
	checkInterval := 5 * time.Minute

	bot, err := NewBot(token, dbPath, checkInterval)
	if err != nil {
		log.Fatalf("Error creating bot: %v", err)
	}

	// Initialize with default repositories
	defaultRepos := []string{
		"https://github.com/bnb-chain/bsc/releases",
		"https://github.com/tronprotocol/java-tron/releases",
	}
	for _, repo := range defaultRepos {
		_, err := bot.db.Exec("INSERT OR IGNORE INTO repositories (url, latest_release) VALUES (?, '')", repo)
		if err != nil {
			log.Printf("Error adding default repository %s: %v", repo, err)
		}
	}

	log.Println("Bot started")
	bot.Start()
}
