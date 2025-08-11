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

// Build information, populated during build
var (
	BuildVersion = "dev"
	BuildCommit  = "none"
	BuildTime    = "unknown"
)

type AddressItem struct {
	Name          string `mapstructure:"name"`
	Address       string `mapstructure:"address"`
	AlertCooldown int    `mapstructure:"alert_cooldown"` // Optional per-address cooldown
	Threshold     struct {
		Denom  string `mapstructure:"denom"`
		Amount string `mapstructure:"amount"`
	} `mapstructure:"threshold"`

	lastAlertTime time.Time // Internal tracking, not from config
}

type AddressConfig struct {
	Name         string        `mapstructure:"name"`
	RESTEndpoint string        `mapstructure:"rest_endpoint"`
	Addresses    []AddressItem `mapstructure:"addresses"`
}

type MetricItem struct {
	Name      string `mapstructure:"name"`
	Metric    string `mapstructure:"metric"`
	Threshold int    `mapstructure:"threshold"`

	lastAlertTime time.Time // Internal tracking, not from config
}

type MetricConfig struct {
	Name         string       `mapstructure:"name"`
	RESTEndpoint string       `mapstructure:"rest_endpoint"`
	Metrics      []MetricItem `mapstructure:"metrics"`
}

type HealthItem struct {
	Name          string    `mapstructure:"name"`
	Endpoint      string    `mapstructure:"endpoint"`
	lastAlertTime time.Time // Internal tracking, not from config
}

type HealthConfig struct {
	Name      string       `mapstructure:"name"`
	Endpoints []HealthItem `mapstructure:"endpoints"`
}

type Config struct {
	CheckInterval int             `mapstructure:"check_interval"`
	AlertCooldown int             `mapstructure:"alert_cooldown"` // Global cooldown setting
	Metrics       []MetricConfig  `mapstructure:"metrics"`
	Addresses     []AddressConfig `mapstructure:"addresses"`
	Health        []HealthConfig  `mapstructure:"health"`
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

type HealthResponse struct {
	JSONRPC string `json:"jsonrpc"`
	Result  struct {
		IsHealthy bool   `json:"isHealthy"`
		Error     string `json:"error"`
	} `json:"result"`
	ID int `json:"id"`
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

	// Only validate Telegram config if bot token is provided
	if config.Telegram.BotToken != "" && config.Telegram.ChatID == 0 {
		return nil, fmt.Errorf("telegram chat ID is required when bot token is provided")
	}

	if config.CheckInterval == 0 {
		config.CheckInterval = 600 // Default to 600 seconds if not specified
	}

	// Validate each address configuration if any are provided
	for i, addrGroup := range config.Addresses {
		if addrGroup.RESTEndpoint == "" {
			return nil, fmt.Errorf("REST endpoint is required for address group #%d", i+1)
		}
		if addrGroup.Name == "" {
			config.Addresses[i].Name = fmt.Sprintf("Address Group %d", i+1) // Set default name if not provided
		}

		// Validate each address within the group
		for j, addr := range addrGroup.Addresses {
			if addr.Address == "" {
				return nil, fmt.Errorf("address is required for address item #%d in group '%s'", j+1, addrGroup.Name)
			}
			if addr.Threshold.Denom == "" {
				return nil, fmt.Errorf("threshold denom is required for address '%s' in group '%s'", addr.Address, addrGroup.Name)
			}
			if addr.Threshold.Amount == "" {
				return nil, fmt.Errorf("threshold amount is required for address '%s' in group '%s'", addr.Address, addrGroup.Name)
			}
			if addr.Name == "" {
				config.Addresses[i].Addresses[j].Name = fmt.Sprintf("Wallet %d", j+1) // Set default name if not provided
			}
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

func checkHealth(endpoint string) (*HealthResponse, error) {
	resp, err := http.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("error making request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health endpoint returned status code %d: %s", resp.StatusCode, string(body))
	}

	var healthResp HealthResponse
	if err := json.Unmarshal(body, &healthResp); err != nil {
		return nil, fmt.Errorf("error parsing health response: %w", err)
	}

	return &healthResp, nil
}

func monitorMetric(metricConfig *MetricConfig, bot *tgbotapi.BotAPI, chatID int64, interval time.Duration, globalCooldown int, wg *sync.WaitGroup) {
	defer wg.Done()

	fmt.Printf("Started monitoring metrics group '%s' with %d metrics\n",
		metricConfig.Name, len(metricConfig.Metrics))

	// Initial check for each metric
	for i := range metricConfig.Metrics {
		metricItem := &metricConfig.Metrics[i]
		value, err := getMetricValue(metricConfig.RESTEndpoint, metricItem.Metric)
		if err != nil {
			fmt.Printf("Error getting metric %s: %v\n", metricItem.Metric, err)
		} else {
			// Use metric name if provided, otherwise use the metric identifier
			displayName := metricItem.Metric
			if metricItem.Name != "" {
				displayName = metricItem.Name
			}

			fmt.Printf("[%s] %s (%s): %.2f (Threshold: %d)\n",
				metricConfig.Name, displayName, metricItem.Metric, value, metricItem.Threshold)

			if value >= float64(metricItem.Threshold) {
				// Format for stdout
				stdoutMsg := fmt.Sprintf("[%s] %s (%s) is above threshold, expected: %d, got: %.2f",
					metricConfig.Name, displayName, metricItem.Metric, metricItem.Threshold, value)

				telegramMsg := fmt.Sprintf("🔴 Alert: [%s] %s `%s` is above threshold\nExpected: %d\nGot: %.2f",
					metricConfig.Name, displayName, metricItem.Metric, metricItem.Threshold, value)

				fmt.Println(telegramMsg)

				if bot != nil {
					tgMsg := tgbotapi.NewMessage(chatID, telegramMsg)
					tgMsg.ParseMode = tgbotapi.ModeMarkdown
					_, err := bot.Send(tgMsg)
					if err != nil {
						fmt.Printf("Error sending Telegram message (%s): %v\n", telegramMsg, err)
					}
				} else {
					fmt.Println(stdoutMsg)
				}

				metricItem.lastAlertTime = time.Now()
			}
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		for i := range metricConfig.Metrics {
			metricItem := &metricConfig.Metrics[i]
			value, err := getMetricValue(metricConfig.RESTEndpoint, metricItem.Metric)
			if err != nil {
				fmt.Printf("Error getting metric %s: %v\n", metricItem.Metric, err)
			} else {
				// Use metric name if provided, otherwise use the metric identifier
				displayName := metricItem.Metric
				if metricItem.Name != "" {
					displayName = metricItem.Name
				}

				fmt.Printf("[%s] %s (%s): %.2f (Threshold: %d)\n",
					metricConfig.Name, displayName, metricItem.Metric, value, metricItem.Threshold)

				if value >= float64(metricItem.Threshold) {
					// Check if enough time has passed since the last alert
					if time.Since(metricItem.lastAlertTime) >= time.Duration(globalCooldown)*time.Second {
						// Format for stdout
						stdoutMsg := fmt.Sprintf("[%s] %s `%s` is above threshold, expected: %d, got: %.2f",
							metricConfig.Name, displayName, metricItem.Metric, metricItem.Threshold, value)

						telegramMsg := fmt.Sprintf("🔴 Alert: [%s] %s `%s` is above threshold\nExpected: %d\nGot: %.2f",
							metricConfig.Name, displayName, metricItem.Metric, metricItem.Threshold, value)

						fmt.Println(telegramMsg)

						if bot != nil {
							tgMsg := tgbotapi.NewMessage(chatID, telegramMsg)
							tgMsg.ParseMode = tgbotapi.ModeMarkdown
							_, err := bot.Send(tgMsg)
							if err != nil {
								fmt.Printf("Error sending Telegram message (%s): %v\n", telegramMsg, err)
							}
						} else {
							fmt.Println(stdoutMsg)
						}

						metricItem.lastAlertTime = time.Now()
					}
				}
			}
		}
	}
}

func checkAndNotify(addrGroupConfig *AddressConfig, addrItem *AddressItem, bot *tgbotapi.BotAPI, chatID int64, globalCooldown int) error {
	balances, err := getBalance(addrGroupConfig.RESTEndpoint, addrItem.Address)
	if err != nil {
		return fmt.Errorf("error checking %s: %w", addrItem.Name, err)
	}

	if len(balances.Balances) == 0 {
		return fmt.Errorf("no balances found for %s (%s)", addrItem.Name, addrItem.Address)
	}

	thresholdAmount := new(big.Int)
	_, ok := thresholdAmount.SetString(addrItem.Threshold.Amount, 10)
	if !ok {
		return fmt.Errorf("invalid threshold amount for %s: %s", addrItem.Name, addrItem.Threshold.Amount)
	}

	// Find the balance for the specified denomination
	for _, balance := range balances.Balances {
		if balance.Denom == addrItem.Threshold.Denom {
			currentAmount := new(big.Int)
			_, ok := currentAmount.SetString(balance.Amount, 10)
			if !ok {
				return fmt.Errorf("invalid balance amount for %s: %s", addrItem.Name, balance.Amount)
			}

			// Always print to stdout
			fmt.Printf("[%s] %s Balance: %s %s (Threshold: %s %s)\n",
				addrGroupConfig.Name,
				addrItem.Name,
				balance.Amount, balance.Denom,
				addrItem.Threshold.Amount, addrItem.Threshold.Denom)

			if currentAmount.Cmp(thresholdAmount) < 0 {
				// Check if we're still in cooldown period
				cooldown := globalCooldown
				if addrItem.AlertCooldown > 0 {
					cooldown = addrItem.AlertCooldown
				}

				if !addrItem.lastAlertTime.IsZero() {
					timeSinceLastAlert := time.Since(addrItem.lastAlertTime)
					if timeSinceLastAlert < time.Duration(cooldown)*time.Second {
						// Still in cooldown, just log to stdout
						fmt.Printf("[%s] %s Balance still below threshold, but in alert cooldown (%s remaining)\n",
							addrGroupConfig.Name,
							addrItem.Name,
							time.Duration(cooldown)*time.Second-timeSinceLastAlert)
						return nil
					}
				}

				// Format for stdout
				stdoutMsg := fmt.Sprintf("[%s] %s balance is below threshold! Expected: %s %s, Actual: %s %s",
					addrGroupConfig.Name,
					addrItem.Name,
					addrItem.Threshold.Amount, addrItem.Threshold.Denom,
					balance.Amount, balance.Denom)

				// Format for Telegram with markdown
				// Escape special characters in strings to avoid Markdown parsing issues

				telegramMsg := fmt.Sprintf("📉 Alert: [%s] `%s` balance is below threshold!\nAddress: `%s`\nCurrent balance: %s %s\nThreshold: %s %s",
					addrGroupConfig.Name,
					addrItem.Name,
					addrItem.Address,
					balance.Amount, balance.Denom,
					addrItem.Threshold.Amount, addrItem.Threshold.Denom)

				fmt.Println(telegramMsg)

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
				addrItem.lastAlertTime = time.Now()
			}
			return nil
		}
	}

	return fmt.Errorf("denomination %s not found in balances for %s", addrItem.Threshold.Denom, addrItem.Name)
}

func checkAndNotifyHealth(healthConfig *HealthConfig, healthItem *HealthItem, bot *tgbotapi.BotAPI, chatID int64, globalCooldown int) error {
	healthResp, err := checkHealth(healthItem.Endpoint)
	if err != nil {
		// Check if we're still in cooldown period
		if !healthItem.lastAlertTime.IsZero() {
			timeSinceLastAlert := time.Since(healthItem.lastAlertTime)
			if timeSinceLastAlert < time.Duration(globalCooldown)*time.Second {
				// Still in cooldown, just log to stdout
				fmt.Printf("[%s] %s health check failed, but in alert cooldown (%s remaining)\n",
					healthConfig.Name,
					healthItem.Name,
					time.Duration(globalCooldown)*time.Second-timeSinceLastAlert)
				return nil
			}
		}

		// Format for stdout
		stdoutMsg := fmt.Sprintf("[%s] %s health check failed: %v",
			healthConfig.Name,
			healthItem.Name, err)

		// Format for Telegram with markdown
		telegramMsg := fmt.Sprintf("🚨 Alert: [%s] `%s` health check failed!\nEndpoint: `%s`\nError: %v",
			healthConfig.Name,
			healthItem.Name,
			healthItem.Endpoint, err)

		fmt.Println(telegramMsg)

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
		healthItem.lastAlertTime = time.Now()
		return nil
	}

	// Always print to stdout
	fmt.Printf("[%s] %s Health: %v (Endpoint: %s)\n",
		healthConfig.Name,
		healthItem.Name,
		healthResp.Result.IsHealthy,
		healthItem.Endpoint)

	// Check if health is not true
	if !healthResp.Result.IsHealthy {
		// Check if we're still in cooldown period
		if !healthItem.lastAlertTime.IsZero() {
			timeSinceLastAlert := time.Since(healthItem.lastAlertTime)
			if timeSinceLastAlert < time.Duration(globalCooldown)*time.Second {
				// Still in cooldown, just log to stdout
				fmt.Printf("[%s] %s health is unhealthy, but in alert cooldown (%s remaining)\n",
					healthConfig.Name,
					healthItem.Name,
					time.Duration(globalCooldown)*time.Second-timeSinceLastAlert)
				return nil
			}
		}

		// Format for stdout
		stdoutMsg := fmt.Sprintf("[%s] %s health is unhealthy! isHealthy: %v, error: %s",
			healthConfig.Name,
			healthItem.Name,
			healthResp.Result.IsHealthy,
			healthResp.Result.Error)

		// Format for Telegram with markdown
		telegramMsg := fmt.Sprintf("⚠️ Alert: [%s] `%s` health is unhealthy!\nEndpoint: `%s`\nIsHealthy: %v\nError: %s",
			healthConfig.Name,
			healthItem.Name,
			healthItem.Endpoint,
			healthResp.Result.IsHealthy,
			healthResp.Result.Error)

		fmt.Println(telegramMsg)

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
		healthItem.lastAlertTime = time.Now()
	}

	return nil
}

func monitorAddressGroup(addrGroupConfig *AddressConfig, bot *tgbotapi.BotAPI, chatID int64, interval time.Duration, globalCooldown int, wg *sync.WaitGroup) {
	defer wg.Done()

	fmt.Printf("Started monitoring address group '%s' with %d addresses\n",
		addrGroupConfig.Name, len(addrGroupConfig.Addresses))

	// Initial check for each address
	for i := range addrGroupConfig.Addresses {
		addrItem := &addrGroupConfig.Addresses[i]
		if err := checkAndNotify(addrGroupConfig, addrItem, bot, chatID, globalCooldown); err != nil {
			fmt.Printf("Error checking %s: %v\n", addrItem.Name, err)
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		for i := range addrGroupConfig.Addresses {
			addrItem := &addrGroupConfig.Addresses[i]
			if err := checkAndNotify(addrGroupConfig, addrItem, bot, chatID, globalCooldown); err != nil {
				fmt.Printf("Error checking %s: %v\n", addrItem.Name, err)
			}
		}
	}
}

func monitorHealth(healthConfig *HealthConfig, bot *tgbotapi.BotAPI, chatID int64, interval time.Duration, globalCooldown int, wg *sync.WaitGroup) {
	defer wg.Done()

	fmt.Printf("Started monitoring health group '%s' with %d health endpoints\n",
		healthConfig.Name, len(healthConfig.Endpoints))

	// Initial check for each health endpoint
	for i := range healthConfig.Endpoints {
		healthItem := &healthConfig.Endpoints[i]
		if err := checkAndNotifyHealth(healthConfig, healthItem, bot, chatID, globalCooldown); err != nil {
			fmt.Printf("Error checking health endpoint %s: %v\n", healthItem.Name, err)
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		for i := range healthConfig.Endpoints {
			healthItem := &healthConfig.Endpoints[i]
			if err := checkAndNotifyHealth(healthConfig, healthItem, bot, chatID, globalCooldown); err != nil {
				fmt.Printf("Error checking health endpoint %s: %v\n", healthItem.Name, err)
			}
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

	// Only show addresses section if we have addresses to monitor
	if len(config.Addresses) > 0 {
		fmt.Println("\nMonitoring addresses:")
		for _, addrGroup := range config.Addresses {
			fmt.Printf("- %s (endpoint: %s)\n", addrGroup.Name, addrGroup.RESTEndpoint)
			for _, addr := range addrGroup.Addresses {
				fmt.Printf("  • %s (%s), threshold: %s %s\n",
					addr.Name, addr.Address, addr.Threshold.Amount, addr.Threshold.Denom)
			}
		}
	}

	// Only show metrics section if we have metrics to monitor
	if len(config.Metrics) > 0 {
		fmt.Println("\nMonitoring metrics:")
		for _, metricGroup := range config.Metrics {
			fmt.Printf("- %s (endpoint: %s)\n", metricGroup.Name, metricGroup.RESTEndpoint)
			for _, metric := range metricGroup.Metrics {
				displayName := metric.Metric
				if metric.Name != "" {
					displayName = metric.Name
				}
				fmt.Printf("  • %s (%s), threshold: %d\n", displayName, metric.Metric, metric.Threshold)
			}
		}
	}

	// Only show health section if we have health endpoints to monitor
	if len(config.Health) > 0 {
		fmt.Println("\nMonitoring health endpoints:")
		for _, healthGroup := range config.Health {
			fmt.Printf("- %s\n", healthGroup.Name)
			for _, health := range healthGroup.Endpoints {
				fmt.Printf("  • %s (%s)\n", health.Name, health.Endpoint)
			}
		}
	}

	// Exit if there's nothing to monitor
	if len(config.Addresses) == 0 && len(config.Metrics) == 0 && len(config.Health) == 0 {
		fmt.Println("\nError: No addresses, metrics, or health endpoints configured to monitor. Please add at least one address, metric, or health endpoint to your config.")
		os.Exit(1)
	}

	var wg sync.WaitGroup
	interval := time.Duration(config.CheckInterval) * time.Second

	// Start monitoring metrics
	for i := range config.Metrics {
		wg.Add(1)
		go monitorMetric(&config.Metrics[i], bot, config.Telegram.ChatID, interval, config.AlertCooldown, &wg)
	}

	// Start monitoring each address group in parallel
	for i := range config.Addresses {
		wg.Add(1)
		// Pass pointer to address group config to allow updating lastAlertTime for each address
		go monitorAddressGroup(&config.Addresses[i], bot, config.Telegram.ChatID, interval, config.AlertCooldown, &wg)
	}

	// Start monitoring health groups in parallel
	for i := range config.Health {
		wg.Add(1)
		// Pass pointer to health group config to allow updating lastAlertTime for each health item
		go monitorHealth(&config.Health[i], bot, config.Telegram.ChatID, interval, config.AlertCooldown, &wg)
	}

	// Wait for all monitoring goroutines
	wg.Wait()
}
