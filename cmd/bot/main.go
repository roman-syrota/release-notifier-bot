package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
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
	done          chan struct{}
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
		done:          make(chan struct{}),
	}, nil
}

func (b *Bot) Start() {
	log.Println("Starting bot...")

	// Setup goroutines with appropriate error handling
	go b.checkReleases()
	go b.keepAlive()

	// Configure exponential backoff for reconnection attempts
	backoffTime := 10 * time.Second
	maxBackoff := 5 * time.Minute

	for {
		select {
		case <-b.done:
			log.Println("Stopping bot due to shutdown signal")
			return
		default:
		}

		log.Println("Starting bot update loop...")
		if err := b.processUpdates(); err != nil {
			log.Printf("Error in update loop: %v. Reconnecting in %v...", err, backoffTime)

			// Sleep with cancellation support
			select {
			case <-time.After(backoffTime):
				// Increase backoff time for next attempt, capped at maxBackoff
				backoffTime = time.Duration(float64(backoffTime) * 1.5)
				if backoffTime > maxBackoff {
					backoffTime = maxBackoff
				}
			case <-b.done:
				log.Println("Stopping bot reconnection due to shutdown signal")
				return
			}
			continue
		}

		// If we get here, the update loop exited without error - reset backoff
		log.Println("Update loop exited unexpectedly, restarting...")
		backoffTime = 10 * time.Second
	}
}

func (b *Bot) processUpdates() error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.bot.GetUpdatesChan(u)

	// Create a channel to watch for the done signal
	done := make(chan struct{})
	go func() {
		<-b.done
		close(done)
	}()

	for update := range updates {
		// Check if we should stop
		select {
		case <-done:
			return fmt.Errorf("bot is shutting down")
		default:
			// Continue processing
		}

		// Use recover to prevent panics from breaking the bot
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Recovered from panic in update processing: %v", r)
				}
			}()

			if update.Message == nil {
				return
			}

			chatID := update.Message.Chat.ID
			b.addSubscriber(chatID)

			if update.Message.IsCommand() {
				b.handleCommand(update.Message)
			}
		}()
	}

	// If we get here, the updates channel was closed
	return fmt.Errorf("updates channel closed")
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
	// Add panic recovery
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered from panic in checkReleases: %v", r)
			// Restart the goroutine
			go b.checkReleases()
		}
	}()

	ticker := time.NewTicker(b.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Handle each check cycle in a recovery function
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("Recovered from panic in release check cycle: %v", r)
					}
				}()

				rows, err := b.db.Query("SELECT url, latest_release FROM repositories")
				if err != nil {
					log.Printf("Error fetching repositories: %v", err)
					return
				}
				defer rows.Close()

				for rows.Next() {
					var url, latestRelease string
					if err := rows.Scan(&url, &latestRelease); err != nil {
						log.Printf("Error scanning row: %v", err)
						continue
					}

					b.checkRepository(url, latestRelease)
				}
			}()
		case <-b.done:
			log.Println("checkReleases routine received stop signal")
			return
		}
	}
}

func (b *Bot) checkRepository(repoURL, latestRelease string) {
	// Add timeout for HTTP operations
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	apiURL := strings.Replace(repoURL, "https://github.com/", "https://api.github.com/repos/", 1)
	apiURL = strings.Replace(apiURL, "/releases", "/releases/latest", 1)

	resp, err := client.Get(apiURL)
	if err != nil {
		log.Printf("Error fetching release for %s: %v", repoURL, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("Non-200 status for %s: %d, will retry next cycle", repoURL, resp.StatusCode)
		return
	}

	// Set explicit timeout for reading the response body
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading response body for %s: %v", repoURL, err)
		return
	}

	var release Release
	if err := json.Unmarshal(bodyBytes, &release); err != nil {
		log.Printf("Error decoding release for %s: %v", repoURL, err)
		return
	}

	if release.TagName != latestRelease {
		log.Printf("Found new release for %s: %s (was: %s)", repoURL, release.TagName, latestRelease)
		b.notifySubscribers(repoURL, &release)
		_, err = b.db.Exec("UPDATE repositories SET latest_release = ? WHERE url = ?", release.TagName, repoURL)
		if err != nil {
			log.Printf("Error updating latest release: %v", err)
		} else {
			log.Printf("New release: %s, %s", release.TagName, repoURL)
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

// keepAlive sends periodic requests to Telegram to keep the connection alive
func (b *Bot) keepAlive() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Println("Sending keepalive request to Telegram...")
			_, err := b.bot.GetMe()
			if err != nil {
				log.Printf("Keepalive request failed: %v", err)
			} else {
				log.Println("Keepalive request successful")
			}
		case <-b.done:
			log.Println("keepAlive routine received stop signal")
			return
		}
	}
}

// Stop signals all goroutines to stop and closes the database
func (b *Bot) Stop() {
	close(b.done)
	if b.db != nil {
		b.db.Close()
	}
	log.Println("Bot stopped")
}

func main() {
	// Load environment variables from .env file
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, proceeding with environment variables")
	}
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable not set")
	}
	dbPath := "bot.db"
	checkInterval := 5 * time.Minute

	bot, err := NewBot(token, dbPath, checkInterval)
	if err != nil {
		log.Fatalf("Error creating bot: %v", err)
	}
	defer bot.Stop()

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

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the bot in a goroutine
	go bot.Start()

	// Wait for termination signal
	sig := <-sigChan
	log.Printf("Received signal %v, shutting down...", sig)

	// bot.Stop() will be called by defer
}
