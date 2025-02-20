package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/spf13/viper"
)

type AddressConfig struct {
	Name          string `mapstructure:"name"`
	RESTEndpoint  string `mapstructure:"rest_endpoint"`
	Address       string `mapstructure:"address"`
	AlertCooldown int    `mapstructure:"alert_cooldown"` // Optional per-address cooldown
	Threshold     struct {
		Denom  string `mapstructure:"denom"`
		Amount string `mapstructure:"amount"`
	} `mapstructure:"threshold"`

	lastAlertTime time.Time // Internal tracking, not from config
}

type MetricConfig struct {
	Name         string `mapstructure:"name"`
	RESTEndpoint string `mapstructure:"rest_endpoint"`
	Metric       string `mapstructure:"metric"`
	Threshold    int    `mapstructure:"threshold"`

	lastAlertTime time.Time // Internal tracking, not from config
}

type Config struct {
	CheckInterval int             `mapstructure:"check_interval"`
	AlertCooldown int             `mapstructure:"alert_cooldown"` // Global cooldown setting
	Metrics       []MetricConfig  `mapstructure:"metrics"`
	Addresses     []AddressConfig `mapstructure:"addresses"`
	Telegram      struct {
		BotToken string `mapstructure:"bot_token"`
		ChatID   int64  `mapstructure:"chat_id"`
	} `mapstructure:"telegram"`
}

type BalanceResponse struct {
	Balances []Balance `json:"balances"`
}

type Balance struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"`
}

func loadConfig(configPath string) (*Config, error) {
	if configPath != "" {
		// If a config path is provided, use it directly
		viper.SetConfigFile(configPath)
	} else {
		// Default behavior: look for config.yaml in the current directory
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
	}

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	// Validate required fields
	if len(config.Addresses) == 0 {
		return nil, fmt.Errorf("at least one address configuration is required")
	}

	// Only validate Telegram config if bot token is provided
	if config.Telegram.BotToken != "" && config.Telegram.ChatID == 0 {
		return nil, fmt.Errorf("telegram chat ID is required when bot token is provided")
	}

	if config.CheckInterval == 0 {
		config.CheckInterval = 600 // Default to 60 seconds if not specified
	}

	// Validate each address configuration
	for i, addr := range config.Addresses {
		if addr.Address == "" {
			return nil, fmt.Errorf("address is required for config #%d", i+1)
		}
		if addr.RESTEndpoint == "" {
			return nil, fmt.Errorf("REST endpoint is required for address %s", addr.Address)
		}
		if addr.Threshold.Denom == "" {
			return nil, fmt.Errorf("threshold denom is required for address %s", addr.Address)
		}
		if addr.Threshold.Amount == "" {
			return nil, fmt.Errorf("threshold amount is required for address %s", addr.Address)
		}
		if addr.Name == "" {
			config.Addresses[i].Name = fmt.Sprintf("Wallet %d", i+1) // Set default name if not provided
		}
	}

	return &config, nil
}

func getBalance(restEndpoint, address string) (*BalanceResponse, error) {
	balanceURL := fmt.Sprintf("%s/cosmos/bank/v1beta1/balances/%s", restEndpoint, address)

	resp, err := http.Get(balanceURL)
	if err != nil {
		return nil, fmt.Errorf("error making request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status code %d: %s", resp.StatusCode, string(body))
	}

	var balanceResp BalanceResponse
	if err := json.Unmarshal(body, &balanceResp); err != nil {
		return nil, fmt.Errorf("error parsing response: %w", err)
	}

	return &balanceResp, nil
}

func getMetricValue(endpoint, metricName string) (float64, error) {
	resp, err := http.Get(endpoint)
	if err != nil {
		return 0, fmt.Errorf("error fetching metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("error reading response: %v", err)
	}

	// Split the response into lines and find the metric
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, metricName+" ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				value, err := strconv.ParseFloat(parts[1], 64)
				if err != nil {
					return 0, fmt.Errorf("error parsing metric value: %v", err)
				}
				return value, nil
			}
		}
	}

	return 0, fmt.Errorf("metric %s not found", metricName)
}

func monitorMetric(metricConfig *MetricConfig, bot *tgbotapi.BotAPI, chatID int64, interval time.Duration, globalCooldown int, wg *sync.WaitGroup) {
	defer wg.Done()

	fmt.Printf("Started monitoring metric %s (%s), you will be alerted if the value exceeds %d\n",
		metricConfig.Name, metricConfig.Metric, metricConfig.Threshold)

	// Initial check
	value, err := getMetricValue(metricConfig.RESTEndpoint, metricConfig.Metric)
	if err != nil {
		fmt.Printf("Error getting metric %s: %v\n", metricConfig.Metric, err)
	} else {
		fmt.Printf("[%s] %s: %.2f (Threshold: %d)\n", metricConfig.Name, metricConfig.Metric, value, metricConfig.Threshold)
		if value >= float64(metricConfig.Threshold) {
			msg := fmt.Sprintf("⚠️ Alert for %s\nMetric `%s` has exceeded threshold: `%.2f` >= `%d`",
				metricConfig.Name, metricConfig.Metric, value, metricConfig.Threshold)

			if bot != nil {
				tgMsg := tgbotapi.NewMessage(chatID, msg)
				_, err := bot.Send(tgMsg)
				if err != nil {
					fmt.Printf("Error sending Telegram message: %v\n", err)
				}
			} else {
				fmt.Println(msg)
			}

			metricConfig.lastAlertTime = time.Now()
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		value, err := getMetricValue(metricConfig.RESTEndpoint, metricConfig.Metric)
		if err != nil {
			fmt.Printf("Error getting metric %s: %v\n", metricConfig.Metric, err)
		} else {
			fmt.Printf("[%s] %s: %.2f (Threshold: %d)\n", metricConfig.Name, metricConfig.Metric, value, metricConfig.Threshold)
			if value >= float64(metricConfig.Threshold) {
				// Check if enough time has passed since the last alert
				if time.Since(metricConfig.lastAlertTime) >= time.Duration(globalCooldown)*time.Second {
					// Format for stdout
					stdoutMsg := fmt.Sprintf("[%s] Alert: %s has exceeded threshold! Expected: <= %d, Actual: %.2f",
						metricConfig.Name, metricConfig.Metric, metricConfig.Threshold, value)

					// Format for Telegram with markdown
					telegramMsg := fmt.Sprintf("🔴 Alert: `%s` has exceeded threshold!\nMetric: `%s`\nCurrent value: %.2f\nThreshold: %d",
						metricConfig.Name, metricConfig.Metric, value, metricConfig.Threshold)

					if bot != nil {
						tgMsg := tgbotapi.NewMessage(chatID, telegramMsg)
						tgMsg.ParseMode = tgbotapi.ModeMarkdown
						_, err := bot.Send(tgMsg)
						if err != nil {
							fmt.Printf("Error sending Telegram message: %v\n", err)
						}
					} else {
						fmt.Println(stdoutMsg)
					}

					metricConfig.lastAlertTime = time.Now()
				}
			}
		}
	}
}

func checkAndNotify(addrConfig *AddressConfig, bot *tgbotapi.BotAPI, chatID int64, globalCooldown int) error {
	balances, err := getBalance(addrConfig.RESTEndpoint, addrConfig.Address)
	if err != nil {
		return fmt.Errorf("error checking %s: %w", addrConfig.Name, err)
	}

	if len(balances.Balances) == 0 {
		return fmt.Errorf("no balances found for %s (%s)", addrConfig.Name, addrConfig.Address)
	}

	thresholdAmount := new(big.Int)
	_, ok := thresholdAmount.SetString(addrConfig.Threshold.Amount, 10)
	if !ok {
		return fmt.Errorf("invalid threshold amount for %s: %s", addrConfig.Name, addrConfig.Threshold.Amount)
	}

	// Find the balance for the specified denomination
	for _, balance := range balances.Balances {
		if balance.Denom == addrConfig.Threshold.Denom {
			currentAmount := new(big.Int)
			_, ok := currentAmount.SetString(balance.Amount, 10)
			if !ok {
				return fmt.Errorf("invalid balance amount for %s: %s", addrConfig.Name, balance.Amount)
			}

			// Always print to stdout
			fmt.Printf("[%s] Balance: %s %s (Threshold: %s %s)\n",
				addrConfig.Name,
				balance.Amount, balance.Denom,
				addrConfig.Threshold.Amount, addrConfig.Threshold.Denom)

			if currentAmount.Cmp(thresholdAmount) < 0 {
				// Check if we're still in cooldown period
				cooldown := globalCooldown
				if addrConfig.AlertCooldown > 0 {
					cooldown = addrConfig.AlertCooldown
				}

				if !addrConfig.lastAlertTime.IsZero() {
					timeSinceLastAlert := time.Since(addrConfig.lastAlertTime)
					if timeSinceLastAlert < time.Duration(cooldown)*time.Second {
						// Still in cooldown, just log to stdout
						fmt.Printf("[%s] Balance still below threshold, but in alert cooldown (%s remaining)\n",
							addrConfig.Name,
							time.Duration(cooldown)*time.Second-timeSinceLastAlert)
						return nil
					}
				}

				// Format for stdout
				stdoutMsg := fmt.Sprintf("[%s] Alert: %s balance is below threshold! Expected: %s %s, Actual: %s %s",
					addrConfig.Name,
					addrConfig.Address,
					addrConfig.Threshold.Amount, addrConfig.Threshold.Denom,
					balance.Amount, balance.Denom)

				// Format for Telegram with markdown
				telegramMsg := fmt.Sprintf("📉 Alert: `%s` balance is below threshold!\nAddress: `%s`\nCurrent balance: %s %s\nThreshold: %s %s",
					addrConfig.Name,
					addrConfig.Address,
					balance.Amount, balance.Denom,
					addrConfig.Threshold.Amount, addrConfig.Threshold.Denom)

				// Only send Telegram message if bot is configured
				if bot != nil {
					msg := tgbotapi.NewMessage(chatID, telegramMsg)
					msg.ParseMode = tgbotapi.ModeMarkdown
					if _, err := bot.Send(msg); err != nil {
						// Log the Telegram error but don't stop monitoring
						fmt.Printf("Warning: Failed to send Telegram message: %v\n", err)
					}
				}
				// Always print to stdout
				fmt.Println(stdoutMsg)

				// Update last alert time
				addrConfig.lastAlertTime = time.Now()
			}
			return nil
		}
	}

	return fmt.Errorf("denomination %s not found in balances for %s", addrConfig.Threshold.Denom, addrConfig.Name)
}

func monitorAddress(addrConfig *AddressConfig, bot *tgbotapi.BotAPI, chatID int64, interval time.Duration, globalCooldown int, wg *sync.WaitGroup) {
	defer wg.Done()

	fmt.Printf("Started monitoring %s (%s), you will be alerted if the balance drops below %s %s\n",
		addrConfig.Name, addrConfig.Address, addrConfig.Threshold.Amount, addrConfig.Threshold.Denom)

	// Initial check
	if err := checkAndNotify(addrConfig, bot, chatID, globalCooldown); err != nil {
		fmt.Printf("Error checking %s: %v\n", addrConfig.Name, err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		if err := checkAndNotify(addrConfig, bot, chatID, globalCooldown); err != nil {
			fmt.Printf("Error checking %s: %v\n", addrConfig.Name, err)
		}
	}
}

func main() {
	// Parse command line flags
	configPath := flag.String("config-path", "", "Path to the config file (default: ./config.yaml)")
	flag.Parse()

	// If a relative path is provided, convert it to absolute
	if *configPath != "" && !filepath.IsAbs(*configPath) {
		abs, err := filepath.Abs(*configPath)
		if err != nil {
			fmt.Printf("Error resolving config path: %v\n", err)
			os.Exit(1)
		}
		*configPath = abs
	}

	config, err := loadConfig(*configPath)
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Initialize Telegram bot only if token is provided
	var bot *tgbotapi.BotAPI
	if config.Telegram.BotToken != "" {
		bot, err = tgbotapi.NewBotAPI(config.Telegram.BotToken)
		if err != nil {
			fmt.Printf("Warning: Failed to initialize Telegram bot: %v\n", err)
			fmt.Println("Continuing in stdout-only mode")
			bot = nil
		} else {
			// Test the connection by sending a startup message
			startupMsg := tgbotapi.NewMessage(config.Telegram.ChatID, "🚀 Monitor started")
			if _, err := bot.Send(startupMsg); err != nil {
				fmt.Printf("Warning: Failed to send test message to Telegram: %v\n", err)
				fmt.Println("Please make sure you have started a chat with the bot and the chat ID is correct")
				fmt.Println("Continuing in stdout-only mode")
				bot = nil
			} else {
				fmt.Println("Telegram notifications enabled and tested successfully")
			}
		}
	} else {
		fmt.Println("Running in stdout-only mode (no Telegram notifications)")
	}

	fmt.Printf("Starting monitor...\n")
	fmt.Printf("Check interval: %d seconds\n", config.CheckInterval)

	fmt.Println("Monitoring addresses:")
	for _, addr := range config.Addresses {
		fmt.Printf("- %s (%s)\n", addr.Name, addr.Address)
	}

	fmt.Println("\nMonitoring metrics:")
	for _, metric := range config.Metrics {
		fmt.Printf("- %s (%s)\n", metric.Name, metric.Metric)
	}

	var wg sync.WaitGroup
	interval := time.Duration(config.CheckInterval) * time.Second

	// Start monitoring metrics
	for i := range config.Metrics {
		wg.Add(1)
		go monitorMetric(&config.Metrics[i], bot, config.Telegram.ChatID, interval, config.AlertCooldown, &wg)
	}

	// Start monitoring each address in parallel
	for i := range config.Addresses {
		wg.Add(1)
		// Pass pointer to address config to allow updating lastAlertTime
		go monitorAddress(&config.Addresses[i], bot, config.Telegram.ChatID, interval, config.AlertCooldown, &wg)
	}

	// Wait for all monitoring goroutines
	wg.Wait()
}
