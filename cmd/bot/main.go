package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
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
	commandQueue  chan tgbotapi.Update
}

func NewBot(token string, dbPath string, checkInterval time.Duration) (*Bot, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}

	// Enable debugging to see all API requests and responses
	bot.Debug = true

	// Log bot information
	log.Printf("Authorized on account %s (ID: %d)", bot.Self.UserName, bot.Self.ID)

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
		commandQueue:  make(chan tgbotapi.Update, 100), // Buffer for 100 pending commands
	}, nil
}

func (b *Bot) Start() {
	log.Println("Starting bot...")

	// Setup goroutines with appropriate error handling
	go b.checkReleases()
	go b.keepAlive()
	go b.processCommands() // Start the command processor

	// Configure exponential backoff for reconnection attempts
	backoffTime := 10 * time.Second
	maxBackoff := 5 * time.Minute
	ticker := time.NewTicker(10 * time.Minute)

	for {
		select {
		case <-b.done:
			log.Println("Stopping bot due to shutdown signal")
			return
		case <-ticker.C:
			log.Printf("Bot is running, command queue length: %d", len(b.commandQueue))
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

	// Remove any existing webhook to ensure we're using long polling
	_, err := b.bot.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: true})
	if err != nil {
		log.Printf("Warning: failed to remove webhook: %v", err)
	} else {
		log.Println("Successfully removed webhook, using GetUpdatesChan")
	}

	updates := b.bot.GetUpdatesChan(u)
	log.Println("Update channel established")

	// Create a channel to watch for the done signal
	done := make(chan struct{})
	go func() {
		<-b.done
		close(done)
	}()

	for update := range updates {
		log.Printf("Received update: ID=%d, UpdateID=%d", update.UpdateID, update.UpdateID)

		// Check if we should stop
		select {
		case <-done:
			return fmt.Errorf("bot is shutting down")
		default:
			// Continue processing
		}

		// Just validate the Message isn't nil and log it
		if update.Message != nil {
			log.Printf("Enqueueing message: ID=%d, From=%s, Text: %s",
				update.Message.MessageID,
				update.Message.From.UserName,
				update.Message.Text)

			if update.Message.IsCommand() {
				log.Printf("Received command: /%s from chat ID %d",
					update.Message.Command(), update.Message.Chat.ID)
			}

			// Queue the update for processing in a separate goroutine
			select {
			case b.commandQueue <- update:
				log.Printf("Command from %s queued for processing", update.Message.From.UserName)
			default:
				// If queue is full, process immediately to prevent deadlock
				log.Printf("Command queue full, processing command from %s immediately",
					update.Message.From.UserName)
				b.addSubscriber(update.Message.Chat.ID)
				if update.Message.IsCommand() {
					b.handleCommand(update.Message)
				}
			}
		} else {
			log.Println("Received update with nil Message, skipping")
		}
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
	case "ping", "start":
		b.handlePingCommand(chatID)
	default:
		b.sendMessage(chatID, "Available commands:\n/ping - Check if bot is alive\n/list - Show monitored repositories\n/latest <repo_url> - Show latest release\n/add <repo_url> - Add repository to monitor")
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

func (b *Bot) handlePingCommand(chatID int64) {
	log.Printf("Handling ping command from chat %d", chatID)
	b.sendMessage(chatID, "I'm alive! Bot is working correctly.")
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

				// Create a context with timeout for the entire check cycle
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()

				// Create a context for database operations
				dbCtx, dbCancel := context.WithTimeout(ctx, 10*time.Second)
				defer dbCancel()

				log.Println("Starting release check cycle")
				startTime := time.Now()

				rows, err := b.db.QueryContext(dbCtx, "SELECT url, latest_release FROM repositories")
				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) {
						log.Printf("Database query timed out in checkReleases")
						return
					}
					log.Printf("Error fetching repositories: %v", err)
					return
				}
				defer rows.Close()

				// Process each repository with a safety limit to prevent too many concurrent checks
				var repoList []struct {
					URL           string
					LatestRelease string
				}

				for rows.Next() {
					var url, latestRelease string
					if err := rows.Scan(&url, &latestRelease); err != nil {
						log.Printf("Error scanning row: %v", err)
						continue
					}
					repoList = append(repoList, struct {
						URL           string
						LatestRelease string
					}{URL: url, LatestRelease: latestRelease})
				}

				if len(repoList) == 0 {
					log.Println("No repositories to check")
					return
				}

				log.Printf("Found %d repositories to check", len(repoList))

				// Use a wait group to track completion of all checks
				var wg sync.WaitGroup
				// Limit concurrent checks to avoid overwhelming GitHub API
				semaphore := make(chan struct{}, 2) // Allow 2 concurrent checks

				for _, repo := range repoList {
					// Check for context cancellation
					select {
					case <-ctx.Done():
						log.Printf("Check cycle timed out after %v", time.Since(startTime))
						return
					default:
						// Continue processing
					}

					wg.Add(1)
					semaphore <- struct{}{} // Acquire semaphore
					go func(url, latestRelease string) {
						defer wg.Done()
						defer func() { <-semaphore }() // Release semaphore

						// Create a separate context for each repository check
						checkCtx, checkCancel := context.WithTimeout(ctx, 45*time.Second)
						defer checkCancel()

						// Create a done channel to track completion
						done := make(chan struct{})

						go func() {
							b.checkRepository(url, latestRelease)
							close(done)
						}()

						// Wait for either completion or timeout
						select {
						case <-done:
							// Check completed normally
						case <-checkCtx.Done():
							log.Printf("Repository check for %s timed out", url)
						}
					}(repo.URL, repo.LatestRelease)
				}

				// Wait for all checks to complete or timeout
				waitCh := make(chan struct{})
				go func() {
					wg.Wait()
					close(waitCh)
				}()

				// Wait with timeout for all checks to complete
				select {
				case <-waitCh:
					log.Printf("All repository checks completed in %v", time.Since(startTime))
				case <-ctx.Done():
					log.Printf("Some repository checks did not complete before timeout after %v", time.Since(startTime))
				}
			}()
		case <-b.done:
			log.Println("checkReleases routine received stop signal")
			return
		}
	}
}

func (b *Bot) checkRepository(repoURL, latestRelease string) {
	// Create a context with timeout for the entire operation
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Add timeout for HTTP operations
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
		},
	}

	apiURL := strings.Replace(repoURL, "https://github.com/", "https://api.github.com/repos/", 1)
	apiURL = strings.Replace(apiURL, "/releases", "/releases/latest", 1)

	// Create a request with the context
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		log.Printf("Error creating request for %s: %v", repoURL, err)
		return
	}

	// Add user agent to avoid GitHub API limitations
	req.Header.Set("User-Agent", "TelegramReleaseBot/1.0")

	// Send the request with context
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("Timeout fetching release for %s", repoURL)
		} else {
			log.Printf("Error fetching release for %s: %v", repoURL, err)
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		// Handle specific error codes better
		if resp.StatusCode == 403 {
			log.Printf("Rate limit exceeded for %s (403), will retry next cycle", repoURL)
		} else if resp.StatusCode == 404 {
			log.Printf("Repository or release not found for %s (404)", repoURL)
		} else {
			log.Printf("Non-200 status for %s: %d, will retry next cycle", repoURL, resp.StatusCode)
		}
		return
	}

	// Use limited reader to prevent extremely large responses
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // Limit to 1MB
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("Timeout reading response body for %s", repoURL)
		} else {
			log.Printf("Error reading response body for %s: %v", repoURL, err)
		}
		return
	}

	var release Release
	if err := json.Unmarshal(bodyBytes, &release); err != nil {
		log.Printf("Error decoding release for %s: %v", repoURL, err)
		return
	}

	// Check for context timeout
	select {
	case <-ctx.Done():
		log.Printf("Operation timed out for %s: %v", repoURL, ctx.Err())
		return
	default:
		// Continue processing
	}

	if release.TagName != latestRelease {
		log.Printf("Found new release for %s: %s (was: %s)", repoURL, release.TagName, latestRelease)

		// Create a separate goroutine for notifications to avoid blocking
		go func(r Release) {
			log.Printf("Starting notification for %s in background", repoURL)
			b.notifySubscribers(repoURL, &r)
		}(release)

		// Add context timeout for database operations
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dbCancel()

		// Use context in database operation
		_, err = b.db.ExecContext(dbCtx, "UPDATE repositories SET latest_release = ? WHERE url = ?", release.TagName, repoURL)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				log.Printf("Database update timed out for %s", repoURL)
			} else {
				log.Printf("Error updating latest release: %v", err)
			}
		} else {
			log.Printf("New release: %s, %s", release.TagName, repoURL)
		}
	}
}

func (b *Bot) notifySubscribers(repoURL string, release *Release) {
	// Create context with timeout for the entire operation
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create context for database operation
	dbCtx, dbCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dbCancel()

	// Use context in database query
	rows, err := b.db.QueryContext(dbCtx, "SELECT chat_id FROM subscribers")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("Database query timed out in notifySubscribers")
			return
		}
		log.Printf("Error fetching subscribers: %v", err)
		return
	}
	defer rows.Close()

	message := fmt.Sprintf("New release for %s\nVersion: %s\nName: %s\nPublished: %s\nDetails: %s",
		repoURL, release.TagName, release.Name, release.PublishedAt.Format(time.RFC822), release.HTMLURL)

	// Create a wait group to track notification completion
	var wg sync.WaitGroup
	// Limit concurrent notifications to avoid overwhelming Telegram API
	semaphore := make(chan struct{}, 3) // Allow 3 concurrent notifications

	for rows.Next() {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			log.Printf("Notification operation timed out for %s", repoURL)
			return
		default:
			// Continue processing
		}

		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			log.Printf("Error scanning subscriber: %v", err)
			continue
		}

		// Use a goroutine for each notification but limit concurrency
		wg.Add(1)
		semaphore <- struct{}{} // Acquire semaphore
		go func(id int64) {
			defer wg.Done()
			defer func() { <-semaphore }() // Release semaphore

			// Create a timeout specifically for this notification
			msgCtx, msgCancel := context.WithTimeout(ctx, 15*time.Second)
			defer msgCancel()

			// Create a done channel to track completion
			done := make(chan struct{})

			go func() {
				b.sendMessage(id, message)
				close(done)
			}()

			// Wait for either completion or timeout
			select {
			case <-done:
				// Message sent successfully or failed with retry
			case <-msgCtx.Done():
				log.Printf("Notification to %d timed out", id)
			}
		}(chatID)
	}

	// Wait for all notifications to complete or timeout
	waitCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitCh)
	}()

	// Wait with timeout for all notifications to complete
	select {
	case <-waitCh:
		log.Printf("All notifications sent for %s", repoURL)
	case <-ctx.Done():
		log.Printf("Some notifications for %s may not have completed before timeout", repoURL)
	}
}

func (b *Bot) sendMessage(chatID int64, text string) {
	log.Printf("Sending message to chat %d: %s", chatID, text)
	msg := tgbotapi.NewMessage(chatID, text)

	// We'll use the original bot but with retry logic

	// Add retry logic for send failures
	var sendErr error
	maxRetries := 3
	for i := 0; i < maxRetries; i++ {
		_, sendErr = b.bot.Send(msg)
		if sendErr == nil {
			log.Printf("Message successfully sent to %d", chatID)
			return
		}
		log.Printf("Error sending message to %d (attempt %d/%d): %v",
			chatID, i+1, maxRetries, sendErr)

		// Wait before retrying
		if i < maxRetries-1 {
			time.Sleep(time.Duration(2*(i+1)) * time.Second) // Increase backoff with each retry
		}
	}

	// If we get here, all retries failed
	log.Printf("Failed to send message to %d after %d attempts: %v",
		chatID, maxRetries, sendErr)
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

// processCommands handles commands from the command queue in a separate goroutine
func (b *Bot) processCommands() {
	log.Println("Command processor started")
	//ticker := time.NewTicker(10 * time.Second)
	//defer ticker.Stop()
	for {
		//log.Printf("Command queue length: %d", len(b.commandQueue))
		select {
		//case <-ticker.C:
		//	log.Printf("Bot is running, command queue length: %d", len(b.commandQueue))
		case update := <-b.commandQueue:
			// Process the command with full error recovery
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan struct{})
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("PANIC in command processing: %v", r)
					}
				}()

				if update.Message == nil {
					return
				}

				chatID := update.Message.Chat.ID

				// Add subscriber regardless of command
				b.addSubscriber(chatID)

				if update.Message.IsCommand() {
					command := update.Message.Command()
					log.Printf("Processing command: /%s from chat ID %d", command, chatID)

					// Process the command with timing information
					start := time.Now()
					b.handleCommand(update.Message)
					elapsed := time.Since(start)

					log.Printf("Command /%s processed in %v", command, elapsed)
				}

				select {
				case <-done:
				case <-ctx.Done():
					log.Printf("Command processing timed out for update: %+v", update)
				}
			}()

		case <-b.done:
			log.Println("Command processor shutting down")
			return
		}
	}
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
