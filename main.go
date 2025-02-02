package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	probing "github.com/prometheus-community/pro-bing"
)

type DeviceState struct {
	Key   string      `json:"key"`
	Value interface{} `json:"value"`
}

type DeviceData struct {
	States []DeviceState `json:"states"`
}

type DevicesData struct {
	Devices map[string]DeviceData `json:"devices"`
}

func main() {
	// Load environment variables
	scanInterval, err := strconv.Atoi(os.Getenv("SCAN_INTERVAL_MINUTES"))
	if err != nil {
		log.Fatalf("Error parsing SCAN_INTERVAL_MINUTES: %v", err)
	}
	scanAttempts, err := strconv.Atoi(os.Getenv("SCAN_ATTEMPTS"))
	if err != nil {
		log.Fatalf("Error parsing SCAN_ATTEMPTS: %v", err)
	}
	targetDeviceIP := os.Getenv("TARGET_DEVICE_IP")
	deviceID := os.Getenv("DEVICE_ID")
	mqttLogin := os.Getenv("MQTT_LOGIN")
	mqttPassword := os.Getenv("MQTT_PASSWORD")
	telegramBotToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	telegramUserID, err := strconv.ParseInt(os.Getenv("TELEGRAM_USER_ID"), 10, 64)
	if err != nil {
		log.Fatalf("Error parsing TELEGRAM_USER_ID: %v", err)
	}

	// Create a context that can be canceled with os signals
	ctx, cancel := context.WithCancel(context.Background())
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Initialize MQTT client
	mqttClient := initMQTTClient(mqttLogin, mqttPassword)
	defer mqttClient.Disconnect(250)

	// Initialize Telegram bot
	bot, err := tgbotapi.NewBotAPI(telegramBotToken)
	if err != nil {
		log.Fatalf("Error initializing Telegram bot: %v", err)
	}

	// Device state tracking
	deviceMissing := false
	missingAttempts := 0
	var mu sync.Mutex

	// Start network scanning in a goroutine
	go func() {
		ticker := time.NewTicker(time.Duration(scanInterval) * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				deviceFound := pingDevice(targetDeviceIP)
				mu.Lock()
				if deviceFound {
					log.Printf("Device with IP '%s' found on the network\n", targetDeviceIP)
					deviceMissing = false
					missingAttempts = 0
				} else {
					log.Printf("Device with IP '%s' not found on the network\n", targetDeviceIP)
					if !deviceMissing {
						missingAttempts++
						if missingAttempts >= scanAttempts {
							log.Printf("Device with IP '%s' missing for %d attempts. Sending MQTT and Telegram messages.\n", targetDeviceIP, missingAttempts)
							sendRequests(ctx, mqttClient, bot, mqttLogin, deviceID, telegramUserID)
							deviceMissing = true
							missingAttempts = 0
						}
					}
				}
				mu.Unlock()
			case <-ctx.Done():
				log.Println("Network scanner stopped.")
				return
			}
		}
	}()

	// Block until a signal is received
	<-sigChan
	log.Println("Shutting down...")
	cancel()
}

func initMQTTClient(mqttLogin, mqttPassword string) mqtt.Client {
	// Get system cert pool
	certPool, err := x509.SystemCertPool()
	if err != nil {
		log.Fatalf("Error getting system cert pool: %v", err)
	}

	// Configure TLS
	tlsConfig := &tls.Config{
		RootCAs:            certPool,
		InsecureSkipVerify: false, // Set to true if you don't want to verify the server certificate
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker("mqtts://mqtt-partners.iot.sberdevices.ru:8883")
	opts.SetClientID("device-watch-client")
	opts.SetUsername(mqttLogin)
	opts.SetPassword(mqttPassword)
	opts.SetTLSConfig(tlsConfig)
	opts.SetAutoReconnect(true)

	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("Error connecting to MQTT server: %v", token.Error())
	}
	log.Println("Connected to MQTT server")

	return client
}

func pingDevice(ipAddress string) bool {
	pinger, err := probing.NewPinger(ipAddress)
	if runtime.GOOS == "windows" {
		pinger.SetPrivileged(true)
	}
	if err != nil {
		log.Printf("Error during ping: %v", err)
		return false
	}
	pinger.Count = 1
	pinger.Timeout = time.Second * 1

	err = pinger.Run()
	if err != nil {
		log.Printf("Error during ping: %v", err)
		return false
	}

	stats := pinger.Statistics()
	return stats.PacketsRecv > 0
}

func sendRequests(ctx context.Context, mqttClient mqtt.Client, bot *tgbotapi.BotAPI, mqttLogin, deviceID string, telegramUserID int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	// Send MQTT request
	go func() {
		defer wg.Done()
		mqttPayload := createMQTTData(deviceID)
		mqttTopic := fmt.Sprintf("sberdevices/v1/%s/up/status", mqttLogin)

		payload, err := json.Marshal(mqttPayload)
		if err != nil {
			log.Printf("Error marshaling MQTT JSON: %v", err)
			return
		}

		token := mqttClient.Publish(mqttTopic, 0, false, payload)
		token.Wait()
		if token.Error() != nil {
			log.Printf("Error publishing MQTT message: %v", token.Error())
		}
		log.Println("MQTT message sent")
	}()

	// Send Telegram message
	go func() {
		defer wg.Done()
		telegramMessage := tgbotapi.NewMessage(telegramUserID, "DeviceWatch: device not found, turned the lights off")
		if _, err := bot.Send(telegramMessage); err != nil {
			log.Printf("Error sending Telegram message: %v", err)
		}
		log.Println("Telegram message sent")
	}()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		wg.Wait() // Wait for both goroutines to complete
	}()
	<-ctx.Done()
}

func createMQTTData(deviceID string) DevicesData {
	data := DevicesData{
		Devices: map[string]DeviceData{
			deviceID: {
				States: []DeviceState{
					{
						Key: "online",
						Value: map[string]interface{}{
							"type":       "BOOL",
							"bool_value": true,
						},
					},
					{
						Key: "button_event",
						Value: map[string]interface{}{
							"type":       "ENUM",
							"enum_value": "click",
						},
					},
				},
			},
		},
	}
	return data
}
