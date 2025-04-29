# GitHub Release Monitor Telegram Bot

This is a Telegram bot written in Go that monitors specified GitHub repositories for new releases and notifies users via Telegram. It supports commands to manage the list of monitored repositories and retrieve release information.

## Features
- **Release Monitoring**: Periodically checks GitHub repositories for new releases and sends notifications with release details (version, name, publication date, and link).
- **Commands**:
  - `/list`: Displays the list of monitored repositories.
  - `/latest <repo_url>`: Shows the latest release version for a specified repository.
  - `/add <repo_url>`: Adds a new GitHub repository (releases page URL) to the monitoring list.
- **Persistent Storage**: Uses SQLite to store repository URLs, latest release versions, and subscriber chat IDs.
- **Default Repositories**: Pre-configured to monitor:
  - [BNB Chain BSC](https://github.com/bnb-chain/bsc/releases)
  - [Tron Protocol Java-Tron](https://github.com/tronprotocol/java-tron/releases)

## Prerequisites
- Go 1.24 or later
- A Telegram Bot Token (obtained from [BotFather](https://t.me/BotFather))
- GitHub API access (no authentication required for public repositories)

## Installation
1. **Clone the Repository**:
   ```bash
   git clone <repository-url>
   cd <repository-directory>
   ```

2. **Configure the Bot Token**:
   In `main.go`, replace `"YOUR_TELEGRAM_BOT_TOKEN"` with your actual Telegram bot token:
   ```go
   token := "your-telegram-bot-token"
   ```

3. **Build and Run**:
   ```bash
   go build
   ./<binary-name>
   ```
   Alternatively, run directly:
   ```bash
   go run main.go
   ```

## Usage
1. **Start the Bot**:
   After running the bot, it will create a SQLite database (`bot.db`) and start monitoring the default repositories every 5 minutes.

2. **Interact with the Bot**:
   - Start a chat with your bot on Telegram.
   - Use the following commands:
     - `/list`: View all monitored repositories.
     - `/latest https://github.com/bnb-chain/bsc/releases`: Check the latest release for the specified repository.
     - `/add https://github.com/owner/repo/releases`: Add a new repository to monitor.
   - The bot will automatically notify you when a new release is detected in any monitored repository.

## Database
The bot uses a SQLite database (`bot.db`) with two tables:
- `repositories`: Stores repository URLs and their latest release versions.
- `subscribers`: Stores chat IDs of users who interact with the bot.

## Customization
- **Check Interval**: Modify the `checkInterval` in `main.go` to change how often the bot checks for new releases (default: 5 minutes).
  ```go
  checkInterval := 5 * time.Minute
  ```
- **Default Repositories**: Update the `defaultRepos` slice in `main.go` to change the initial repositories.
  ```go
  defaultRepos := []string{
      "https://github.com/bnb-chain/bsc/releases",
      "https://github.com/tronprotocol/java-tron/releases",
  }
  ```

## Dependencies
- [go-telegram-bot-api](https://github.com/go-telegram-bot-api/telegram-bot-api): Telegram Bot API for Go.
- [go-sqlite3](https://github.com/mattn/go-sqlite3): SQLite driver for Go.

## Troubleshooting
- **Dependency Errors**: Ensure all dependencies are installed using `go get` and `go mod tidy`.
- **Bot Not Responding**: Verify the Telegram bot token and ensure the bot is running.
- **GitHub API Issues**: Check for rate limits or network connectivity issues when fetching release data.

## License
This project is licensed under the MIT License.