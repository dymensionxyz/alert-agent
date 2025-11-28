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

type KaspaAddressItem struct {
	Name          string `mapstructure:"name"`
	Address       string `mapstructure:"address"`
	AlertCooldown int    `mapstructure:"alert_cooldown"` // Optional per-address cooldown
	Threshold     string `mapstructure:"threshold"`      // Threshold amount in sompi

	lastAlertTime       time.Time   // Internal tracking, not from config
	isUnhealthy         bool        // Track if currently in unhealthy state
	recoveryMonitorStop chan bool   // Channel to stop recovery monitoring
	recoveryMonitorMu   *sync.Mutex // Pointer to avoid copy issues
}

type AddressConfig struct {
	Name         string        `mapstructure:"name"`
	RESTEndpoint string        `mapstructure:"rest_endpoint"`
	Addresses    []AddressItem `mapstructure:"addresses"`
}

type KaspaAddressConfig struct {
	Name         string             `mapstructure:"name"`
	RESTEndpoint string             `mapstructure:"rest_endpoint"`
	Addresses    []KaspaAddressItem `mapstructure:"addresses"`
}

type MetricItem struct {
	Name      string `mapstructure:"name"`
	Metric    string `mapstructure:"metric"`
	Threshold int    `mapstructure:"threshold"`

	lastAlertTime       time.Time   // Internal tracking, not from config
	isUnhealthy         bool        // Track if currently in unhealthy state
	recoveryMonitorStop chan bool   // Channel to stop recovery monitoring
	recoveryMonitorMu   *sync.Mutex // Pointer to avoid copy issues
}

type MetricConfig struct {
	Name         string       `mapstructure:"name"`
	RESTEndpoint string       `mapstructure:"rest_endpoint"`
	Metrics      []MetricItem `mapstructure:"metrics"`
}

type HealthItem struct {
	Name                string      `mapstructure:"name"`
	Endpoint            string      `mapstructure:"endpoint"`
	lastAlertTime       time.Time   // Internal tracking, not from config
	isUnhealthy         bool        // Track if currently in unhealthy state
	recoveryMonitorStop chan bool   // Channel to stop recovery monitoring
	recoveryMonitorMu   *sync.Mutex // Pointer to avoid copy issues
}

type HealthConfig struct {
	Name      string       `mapstructure:"name"`
	Endpoints []HealthItem `mapstructure:"endpoints"`
}

type KaspaValidatorItem struct {
	Name          string `mapstructure:"name"`
	Endpoint      string `mapstructure:"endpoint"`
	AlertCooldown int    `mapstructure:"alert_cooldown"` // Optional per-validator cooldown

	lastAlertTime       time.Time   // Internal tracking, not from config
	isUnhealthy         bool        // Track if currently in unhealthy state
	recoveryMonitorStop chan bool   // Channel to stop recovery monitoring
	recoveryMonitorMu   *sync.Mutex // Pointer to avoid copy issues
}

type KaspaValidatorConfig struct {
	Name       string               `mapstructure:"name"`
	Validators []KaspaValidatorItem `mapstructure:"validators"`
}

type Config struct {
	CheckInterval   int                    `mapstructure:"check_interval"`
	AlertCooldown   int                    `mapstructure:"alert_cooldown"` // Global cooldown setting
	Metrics         []MetricConfig         `mapstructure:"metrics"`
	Addresses       []AddressConfig        `mapstructure:"addresses"`
	KaspaAddresses  []KaspaAddressConfig   `mapstructure:"kaspa_addresses"`
	KaspaValidators []KaspaValidatorConfig `mapstructure:"kaspa_validators"`
	Health          []HealthConfig         `mapstructure:"health"`
	Telegram        struct {
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

type KaspaBalanceResponse struct {
	Address string `json:"address"`
	Balance int64  `json:"balance"`
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

	// Validate each Kaspa address configuration if any are provided
	for i, kaspaGroup := range config.KaspaAddresses {
		if kaspaGroup.RESTEndpoint == "" {
			return nil, fmt.Errorf("REST endpoint is required for Kaspa address group #%d", i+1)
		}
		if kaspaGroup.Name == "" {
			config.KaspaAddresses[i].Name = fmt.Sprintf("Kaspa Address Group %d", i+1) // Set default name if not provided
		}

		// Validate each Kaspa address within the group
		for j, addr := range kaspaGroup.Addresses {
			if addr.Address == "" {
				return nil, fmt.Errorf("address is required for Kaspa address item #%d in group '%s'", j+1, kaspaGroup.Name)
			}
			if addr.Threshold == "" {
				return nil, fmt.Errorf("threshold is required for Kaspa address '%s' in group '%s'", addr.Address, kaspaGroup.Name)
			}
			if addr.Name == "" {
				config.KaspaAddresses[i].Addresses[j].Name = fmt.Sprintf("Kaspa Wallet %d", j+1) // Set default name if not provided
			}
		}
	}

	// Initialize mutexes for metrics
	for i := range config.Metrics {
		for j := range config.Metrics[i].Metrics {
			config.Metrics[i].Metrics[j].recoveryMonitorMu = &sync.Mutex{}
		}
	}

	// Initialize mutexes for health endpoints
	for i := range config.Health {
		for j := range config.Health[i].Endpoints {
			config.Health[i].Endpoints[j].recoveryMonitorMu = &sync.Mutex{}
		}
	}

	// Validate each Kaspa validator configuration if any are provided
	for i, validatorGroup := range config.KaspaValidators {
		if validatorGroup.Name == "" {
			config.KaspaValidators[i].Name = fmt.Sprintf("Kaspa Validator Group %d", i+1) // Set default name if not provided
		}

		// Validate each validator within the group
		for j, validator := range validatorGroup.Validators {
			if validator.Endpoint == "" {
				return nil, fmt.Errorf("endpoint is required for Kaspa validator item #%d in group '%s'", j+1, validatorGroup.Name)
			}
			if validator.Name == "" {
				config.KaspaValidators[i].Validators[j].Name = fmt.Sprintf("Kaspa Validator %d", j+1) // Set default name if not provided
			}
			// Initialize mutex for recovery monitoring
			config.KaspaValidators[i].Validators[j].recoveryMonitorMu = &sync.Mutex{}
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

func getKaspaBalance(restEndpoint, address string) (*KaspaBalanceResponse, error) {
	balanceURL := fmt.Sprintf("%s/addresses/%s/balance", restEndpoint, address)

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

	var balanceResp KaspaBalanceResponse
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

// pingKaspaValidator sends a GET request to the health endpoint and expects 200 OK
func pingKaspaValidator(endpoint string) error {
	resp, err := http.Get(endpoint)
	if err != nil {
		return fmt.Errorf("error making request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("validator health check returned status code %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func monitorMetricRecovery(metricConfig *MetricConfig, metricItem *MetricItem, bot *tgbotapi.BotAPI, chatID int64) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			value, err := getMetricValue(metricConfig.RESTEndpoint, metricItem.Metric)
			if err != nil {
				fmt.Printf("[Recovery Monitor] Error getting metric %s: %v\n", metricItem.Metric, err)
				continue
			}

			// Check if metric has recovered (below threshold)
			if value < float64(metricItem.Threshold) {
				metricItem.recoveryMonitorMu.Lock()
				if metricItem.isUnhealthy {
					// Metric has recovered
					metricItem.isUnhealthy = false

					displayName := metricItem.Metric
					if metricItem.Name != "" {
						displayName = metricItem.Name
					}

					stdoutMsg := fmt.Sprintf("[%s] %s (%s) has recovered! Current value: %.2f (Threshold: %d)",
						metricConfig.Name, displayName, metricItem.Metric, value, metricItem.Threshold)

					telegramMsg := fmt.Sprintf("✅ Recovery: [%s] %s `%s` has recovered!\nCurrent value: %.2f\nThreshold: %d",
						metricConfig.Name, displayName, metricItem.Metric, value, metricItem.Threshold)

					fmt.Println(telegramMsg)

					if bot != nil {
						tgMsg := tgbotapi.NewMessage(chatID, telegramMsg)
						tgMsg.ParseMode = tgbotapi.ModeMarkdown
						_, err := bot.Send(tgMsg)
						if err != nil {
							fmt.Printf("Error sending Telegram recovery message: %v\n", err)
						}
					} else {
						fmt.Println(stdoutMsg)
					}

					// Stop the recovery monitor
					metricItem.recoveryMonitorMu.Unlock()
					return
				}
				metricItem.recoveryMonitorMu.Unlock()
			}
		case <-metricItem.recoveryMonitorStop:
			return
		}
	}
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

				// Start recovery monitoring if not already started
				metricItem.recoveryMonitorMu.Lock()
				if !metricItem.isUnhealthy {
					metricItem.isUnhealthy = true
					metricItem.recoveryMonitorStop = make(chan bool)
					go monitorMetricRecovery(metricConfig, metricItem, bot, chatID)
				}
				metricItem.recoveryMonitorMu.Unlock()
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

					// Start recovery monitoring if not already started
					metricItem.recoveryMonitorMu.Lock()
					if !metricItem.isUnhealthy {
						metricItem.isUnhealthy = true
						metricItem.recoveryMonitorStop = make(chan bool)
						go monitorMetricRecovery(metricConfig, metricItem, bot, chatID)
					}
					metricItem.recoveryMonitorMu.Unlock()
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

func monitorHealthRecovery(healthConfig *HealthConfig, healthItem *HealthItem, bot *tgbotapi.BotAPI, chatID int64) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			healthResp, err := checkHealth(healthItem.Endpoint)

			// Check if health has recovered (no error and isHealthy is true)
			if err == nil && healthResp.Result.IsHealthy {
				healthItem.recoveryMonitorMu.Lock()
				if healthItem.isUnhealthy {
					// Health has recovered
					healthItem.isUnhealthy = false

					stdoutMsg := fmt.Sprintf("[%s] %s has recovered! Health is now: %v",
						healthConfig.Name, healthItem.Name, healthResp.Result.IsHealthy)

					telegramMsg := fmt.Sprintf("✅ Recovery: [%s] `%s` has recovered!\nEndpoint: `%s`\nHealth is now: %v",
						healthConfig.Name, healthItem.Name, healthItem.Endpoint, healthResp.Result.IsHealthy)

					fmt.Println(telegramMsg)

					if bot != nil {
						tgMsg := tgbotapi.NewMessage(chatID, telegramMsg)
						tgMsg.ParseMode = tgbotapi.ModeMarkdown
						_, sendErr := bot.Send(tgMsg)
						if sendErr != nil {
							fmt.Printf("Error sending Telegram recovery message: %v\n", sendErr)
						}
					} else {
						fmt.Println(stdoutMsg)
					}

					// Stop the recovery monitor
					healthItem.recoveryMonitorMu.Unlock()
					return
				}
				healthItem.recoveryMonitorMu.Unlock()
			}
		case <-healthItem.recoveryMonitorStop:
			return
		}
	}
}

func monitorKaspaValidatorRecovery(validatorConfig *KaspaValidatorConfig, validatorItem *KaspaValidatorItem, bot *tgbotapi.BotAPI, chatID int64) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := pingKaspaValidator(validatorItem.Endpoint)

			// Check if validator has recovered (no error means healthy)
			if err == nil {
				validatorItem.recoveryMonitorMu.Lock()
				if validatorItem.isUnhealthy {
					// Validator has recovered
					validatorItem.isUnhealthy = false

					stdoutMsg := fmt.Sprintf("[%s] %s has recovered! Validator is now responding",
						validatorConfig.Name, validatorItem.Name)

					telegramMsg := fmt.Sprintf("✅ Recovery: [%s] `%s` has recovered!\nEndpoint: `%s`\nValidator is now responding to ping",
						validatorConfig.Name, validatorItem.Name, validatorItem.Endpoint)

					fmt.Println(telegramMsg)

					if bot != nil {
						tgMsg := tgbotapi.NewMessage(chatID, telegramMsg)
						tgMsg.ParseMode = tgbotapi.ModeMarkdown
						_, sendErr := bot.Send(tgMsg)
						if sendErr != nil {
							fmt.Printf("Error sending Telegram recovery message: %v\n", sendErr)
						}
					} else {
						fmt.Println(stdoutMsg)
					}

					// Stop the recovery monitor
					validatorItem.recoveryMonitorMu.Unlock()
					return
				}
				validatorItem.recoveryMonitorMu.Unlock()
			}
		case <-validatorItem.recoveryMonitorStop:
			return
		}
	}
}

func checkAndNotifyKaspaValidator(validatorConfig *KaspaValidatorConfig, validatorItem *KaspaValidatorItem, bot *tgbotapi.BotAPI, chatID int64, globalCooldown int) error {
	err := pingKaspaValidator(validatorItem.Endpoint)
	if err != nil {
		// Check if we're still in cooldown period
		cooldown := globalCooldown
		if validatorItem.AlertCooldown > 0 {
			cooldown = validatorItem.AlertCooldown
		}

		if !validatorItem.lastAlertTime.IsZero() {
			timeSinceLastAlert := time.Since(validatorItem.lastAlertTime)
			if timeSinceLastAlert < time.Duration(cooldown)*time.Second {
				// Still in cooldown, just log to stdout
				fmt.Printf("[%s] %s validator ping failed, but in alert cooldown (%s remaining)\n",
					validatorConfig.Name,
					validatorItem.Name,
					time.Duration(cooldown)*time.Second-timeSinceLastAlert)
				return nil
			}
		}

		// Format for stdout
		stdoutMsg := fmt.Sprintf("[%s] %s validator ping failed: %v",
			validatorConfig.Name,
			validatorItem.Name, err)

		// Format for Telegram with markdown
		telegramMsg := fmt.Sprintf("🚨 Alert: [%s] `%s` Kaspa validator is unavailable!\nEndpoint: `%s`\nError: %v",
			validatorConfig.Name,
			validatorItem.Name,
			validatorItem.Endpoint, err)

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
		validatorItem.lastAlertTime = time.Now()

		// Start recovery monitoring if not already started
		validatorItem.recoveryMonitorMu.Lock()
		if !validatorItem.isUnhealthy {
			validatorItem.isUnhealthy = true
			validatorItem.recoveryMonitorStop = make(chan bool)
			go monitorKaspaValidatorRecovery(validatorConfig, validatorItem, bot, chatID)
		}
		validatorItem.recoveryMonitorMu.Unlock()

		return nil
	}

	// Always print to stdout when healthy
	fmt.Printf("[%s] %s Kaspa Validator: OK (Endpoint: %s)\n",
		validatorConfig.Name,
		validatorItem.Name,
		validatorItem.Endpoint)

	return nil
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

		// Start recovery monitoring if not already started
		healthItem.recoveryMonitorMu.Lock()
		if !healthItem.isUnhealthy {
			healthItem.isUnhealthy = true
			healthItem.recoveryMonitorStop = make(chan bool)
			go monitorHealthRecovery(healthConfig, healthItem, bot, chatID)
		}
		healthItem.recoveryMonitorMu.Unlock()

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

		// Start recovery monitoring if not already started
		healthItem.recoveryMonitorMu.Lock()
		if !healthItem.isUnhealthy {
			healthItem.isUnhealthy = true
			healthItem.recoveryMonitorStop = make(chan bool)
			go monitorHealthRecovery(healthConfig, healthItem, bot, chatID)
		}
		healthItem.recoveryMonitorMu.Unlock()
	}

	return nil
}

func checkAndNotifyKaspa(kaspaGroupConfig *KaspaAddressConfig, kaspaItem *KaspaAddressItem, bot *tgbotapi.BotAPI, chatID int64, globalCooldown int) error {
	balanceResp, err := getKaspaBalance(kaspaGroupConfig.RESTEndpoint, kaspaItem.Address)
	if err != nil {
		return fmt.Errorf("error checking %s: %w", kaspaItem.Name, err)
	}

	thresholdAmount := new(big.Int)
	_, ok := thresholdAmount.SetString(kaspaItem.Threshold, 10)
	if !ok {
		return fmt.Errorf("invalid threshold amount for %s: %s", kaspaItem.Name, kaspaItem.Threshold)
	}

	currentAmount := big.NewInt(balanceResp.Balance)

	// Always print to stdout
	fmt.Printf("[%s] %s Kaspa Balance: %d sompi (Threshold: %s sompi)\n",
		kaspaGroupConfig.Name,
		kaspaItem.Name,
		balanceResp.Balance,
		kaspaItem.Threshold)

	if currentAmount.Cmp(thresholdAmount) < 0 {
		// Check if we're still in cooldown period
		cooldown := globalCooldown
		if kaspaItem.AlertCooldown > 0 {
			cooldown = kaspaItem.AlertCooldown
		}

		if !kaspaItem.lastAlertTime.IsZero() {
			timeSinceLastAlert := time.Since(kaspaItem.lastAlertTime)
			if timeSinceLastAlert < time.Duration(cooldown)*time.Second {
				// Still in cooldown, just log to stdout
				fmt.Printf("[%s] %s Kaspa balance still below threshold, but in alert cooldown (%s remaining)\n",
					kaspaGroupConfig.Name,
					kaspaItem.Name,
					time.Duration(cooldown)*time.Second-timeSinceLastAlert)
				return nil
			}
		}

		// Format for stdout
		stdoutMsg := fmt.Sprintf("[%s] %s Kaspa balance is below threshold! Expected: %s sompi, Actual: %d sompi",
			kaspaGroupConfig.Name,
			kaspaItem.Name,
			kaspaItem.Threshold,
			balanceResp.Balance)

		// Format for Telegram with markdown
		telegramMsg := fmt.Sprintf("📉 Alert: [%s] `%s` Kaspa balance is below threshold!\nAddress: `%s`\nCurrent balance: %d sompi\nThreshold: %s sompi",
			kaspaGroupConfig.Name,
			kaspaItem.Name,
			kaspaItem.Address,
			balanceResp.Balance,
			kaspaItem.Threshold)

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
		kaspaItem.lastAlertTime = time.Now()
	}

	return nil
}

func monitorKaspaAddressGroup(kaspaGroupConfig *KaspaAddressConfig, bot *tgbotapi.BotAPI, chatID int64, interval time.Duration, globalCooldown int, wg *sync.WaitGroup) {
	defer wg.Done()

	fmt.Printf("Started monitoring Kaspa address group '%s' with %d addresses\n",
		kaspaGroupConfig.Name, len(kaspaGroupConfig.Addresses))

	// Initial check for each address
	for i := range kaspaGroupConfig.Addresses {
		kaspaItem := &kaspaGroupConfig.Addresses[i]
		if err := checkAndNotifyKaspa(kaspaGroupConfig, kaspaItem, bot, chatID, globalCooldown); err != nil {
			fmt.Printf("Error checking %s: %v\n", kaspaItem.Name, err)
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		for i := range kaspaGroupConfig.Addresses {
			kaspaItem := &kaspaGroupConfig.Addresses[i]
			if err := checkAndNotifyKaspa(kaspaGroupConfig, kaspaItem, bot, chatID, globalCooldown); err != nil {
				fmt.Printf("Error checking %s: %v\n", kaspaItem.Name, err)
			}
		}
	}
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

func monitorKaspaValidators(validatorConfig *KaspaValidatorConfig, bot *tgbotapi.BotAPI, chatID int64, interval time.Duration, globalCooldown int, wg *sync.WaitGroup) {
	defer wg.Done()

	fmt.Printf("Started monitoring Kaspa validator group '%s' with %d validators\n",
		validatorConfig.Name, len(validatorConfig.Validators))

	// Initial check for each validator
	for i := range validatorConfig.Validators {
		validatorItem := &validatorConfig.Validators[i]
		if err := checkAndNotifyKaspaValidator(validatorConfig, validatorItem, bot, chatID, globalCooldown); err != nil {
			fmt.Printf("Error checking Kaspa validator %s: %v\n", validatorItem.Name, err)
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		for i := range validatorConfig.Validators {
			validatorItem := &validatorConfig.Validators[i]
			if err := checkAndNotifyKaspaValidator(validatorConfig, validatorItem, bot, chatID, globalCooldown); err != nil {
				fmt.Printf("Error checking Kaspa validator %s: %v\n", validatorItem.Name, err)
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

	// Only show Kaspa addresses section if we have Kaspa addresses to monitor
	if len(config.KaspaAddresses) > 0 {
		fmt.Println("\nMonitoring Kaspa addresses:")
		for _, kaspaGroup := range config.KaspaAddresses {
			fmt.Printf("- %s (endpoint: %s)\n", kaspaGroup.Name, kaspaGroup.RESTEndpoint)
			for _, addr := range kaspaGroup.Addresses {
				fmt.Printf("  • %s (%s), threshold: %s sompi\n",
					addr.Name, addr.Address, addr.Threshold)
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

	// Only show Kaspa validators section if we have validators to monitor
	if len(config.KaspaValidators) > 0 {
		fmt.Println("\nMonitoring Kaspa validators:")
		for _, validatorGroup := range config.KaspaValidators {
			fmt.Printf("- %s\n", validatorGroup.Name)
			for _, validator := range validatorGroup.Validators {
				fmt.Printf("  • %s (%s)\n", validator.Name, validator.Endpoint)
			}
		}
	}

	// Exit if there's nothing to monitor
	if len(config.Addresses) == 0 && len(config.KaspaAddresses) == 0 && len(config.Metrics) == 0 && len(config.Health) == 0 && len(config.KaspaValidators) == 0 {
		fmt.Println("\nError: No addresses, Kaspa addresses, metrics, health endpoints, or Kaspa validators configured to monitor. Please add at least one to your config.")
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

	// Start monitoring each Kaspa address group in parallel
	for i := range config.KaspaAddresses {
		wg.Add(1)
		// Pass pointer to Kaspa address group config to allow updating lastAlertTime for each address
		go monitorKaspaAddressGroup(&config.KaspaAddresses[i], bot, config.Telegram.ChatID, interval, config.AlertCooldown, &wg)
	}

	// Start monitoring health groups in parallel
	for i := range config.Health {
		wg.Add(1)
		// Pass pointer to health group config to allow updating lastAlertTime for each health item
		go monitorHealth(&config.Health[i], bot, config.Telegram.ChatID, interval, config.AlertCooldown, &wg)
	}

	// Start monitoring Kaspa validator groups in parallel
	for i := range config.KaspaValidators {
		wg.Add(1)
		// Pass pointer to Kaspa validator group config to allow updating lastAlertTime for each validator
		go monitorKaspaValidators(&config.KaspaValidators[i], bot, config.Telegram.ChatID, interval, config.AlertCooldown, &wg)
	}

	// Wait for all monitoring goroutines
	wg.Wait()
}
