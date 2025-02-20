# Cosmos Balance Monitor

A Go program that monitors multiple Cosmos account balances across different chains and provides alerts through stdout or Telegram when balances fall below specified thresholds.

## Features

- Monitor multiple addresses simultaneously
- Support for different chains and cosmos compatible REST endpoints
- Monitor Prometheus metrics with threshold alerts
- Individual threshold settings for each address and metric
- Parallel monitoring with efficient resource usage
- Flexible output options (stdout or Telegram)
- YAML-based configuration

## Configuration

Create a `config.yaml` file in the same directory as the program:

```yaml
check_interval: 600                        # Global check interval in seconds (default: 10 minutes)
alert_cooldown: 3600                       # Global alert cooldown in seconds (default: 1 hour). When an alert is triggered, the agent will wait for the cooldown to expire before sending another alert.

metrics:
  - name: "Sequencer Wallet"               # Human-readable name for the metric alert
    rest_endpoint: "http://localhost:2112/metrics" # Prometheus metrics endpoint
    metric: "rollapp_consecutive_failed_da_submissions" # Metric name to monitor
    threshold: 10                          # Alert when metric exceeds this value

addresses:
  - name: "Sequencer Wallet"               # Human-readable name for the address
    rest_endpoint: "https://api-dymension.rollapp.network" # not a real endpoint, just an example
    address: "dym1dshqzh897jpamh67nqph47h5sqgphgcl7lull2" # Cosmos address to monitor
    threshold:
      denom: "adym"                        # denomination to check
      amount: "1000000000000000000"        # minimum amount
    alert_cooldown: 7200                   # Optional: override global cooldown for this address (2 hours)
  - name: "Celestia Wallet"                # Human-readable name for the address
    rest_endpoint: "https://api-mocha.pops.one" # not a real endpoint, just an example
    address: "celestia179njue5pgfw578eg2w660h5evzh58t366pt0k8" # Cosmos address to monitor
    threshold:
      denom: "utia"                        # denomination to check
      amount: "1000000000000000000"        # minimum amount
    alert_cooldown: 7200                   # Optional: override global cooldown for this address (2 hours)

telegram:
  bot_token: ""                            # Leave empty to use stdout only
  chat_id: 0                               # Required only if bot_token is provided
```

## Telegram Setup (Optional)

1. Create a new bot using [@BotFather](https://t.me/botfather) on Telegram
2. Copy the bot token and add it to your config.yaml
3. To get your chat ID:
   - For personal chat: Send a message to [@userinfobot](https://t.me/userinfobot)
   - For group chat: Add [@userinfobot](https://t.me/userinfobot) to your group

## Usage

```bash
# Build the program
go build

# Run the program
./observability-agent
```
