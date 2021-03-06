package telegrambot

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/HouzuoGuo/laitos/daemon/common"
	"github.com/HouzuoGuo/laitos/inet"
	"github.com/HouzuoGuo/laitos/misc"
	"github.com/HouzuoGuo/laitos/testingstub"
	"github.com/HouzuoGuo/laitos/toolbox"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	ChatTypePrivate   = "private" // Name of the private chat type
	PollIntervalSec   = 5         // Poll for incoming messages every three seconds
	APICallTimeoutSec = 30        // Outgoing API calls are constrained by this timeout
	CommandTimeoutSec = 30        // Command execution is constrained by this timeout
)

var DurationStats = misc.NewStats() // DurationStats stores statistics of duration of all chat conversations served.

// Telegram API entity - user
type APIUser struct {
	ID        uint64 `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	UserName  string `json:"username"`
}

// Telegram API entity - chat
type APIChat struct {
	ID        uint64 `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	UserName  string `json:"username"`
	Type      string `json:"type"`
}

// Telegram API entity - message
type APIMessage struct {
	ID        uint64  `json:"message_id"`
	From      APIUser `json:"from"`
	Chat      APIChat `json:"chat"`
	Timestamp int64   `json:"date"`
	Text      string  `json:"text"`
}

// Telegram API entity - one bot update
type APIUpdate struct {
	ID      uint64     `json:"update_id"`
	Message APIMessage `json:"message"`
}

// Telegram API entity - getUpdates response
type APIUpdates struct {
	OK      bool        `json:"ok"`
	Updates []APIUpdate `json:"result"`
}

// Process feature commands from incoming telegram messages, reply to the chats with command results.
type Daemon struct {
	AuthorizationToken string                   `json:"AuthorizationToken"` // Telegram bot API auth token
	RateLimit          int                      `json:"RateLimit"`          // rateLimit determines how many messages may be processed per chat at a regular interval
	Processor          *common.CommandProcessor `json:"-"`                  // Feature command processor

	messageOffset uint64          // Process chat messages arrived after this point
	userRateLimit *misc.RateLimit // Prevent user from flooding bot with new messages
	loopIsRunning int32           // Value is 1 only when message loop is running
	stop          chan bool       // Signal message loop to stop
	logger        misc.Logger
}

func (bot *Daemon) Initialise() error {
	bot.logger = misc.Logger{ComponentName: "telegrambot", ComponentID: ""}
	if bot.Processor == nil || bot.Processor.IsEmpty() {
		return fmt.Errorf("telegrambot.Initialise: command processor and its filters must be configured")
	}
	bot.Processor.SetLogger(bot.logger)
	if errs := bot.Processor.IsSaneForInternet(); len(errs) > 0 {
		return fmt.Errorf("telegrambot.Initialise: %+v", errs)
	}
	if bot.AuthorizationToken == "" {
		return errors.New("telegrambot.Initialise: AuthorizationToken must not be empty")
	}
	if bot.RateLimit < 1 {
		return errors.New("telegrambot.Initialise: RateLimit must be greater than 0")
	}
	// Configure rate limit
	bot.userRateLimit = &misc.RateLimit{
		UnitSecs: PollIntervalSec,
		MaxCount: bot.RateLimit,
		Logger:   bot.logger,
	}
	bot.userRateLimit.Initialise()
	return nil
}

// Send a text reply to the telegram chat.
func (bot *Daemon) ReplyTo(chatID uint64, text string) error {
	resp, err := inet.DoHTTP(inet.HTTPRequest{
		Method:     http.MethodPost,
		TimeoutSec: APICallTimeoutSec,
		Body: strings.NewReader(url.Values{
			"chat_id": []string{strconv.FormatUint(chatID, 10)},
			"text":    []string{text},
		}.Encode()),
	}, "https://api.telegram.org/bot%s/sendMessage", bot.AuthorizationToken)
	if err != nil || resp.StatusCode/200 != 1 {
		return fmt.Errorf("telegrambot.ReplyTo: failed to reply to %d - HTTP %d - %v %s", chatID, resp.StatusCode, err, string(resp.Body))
	}
	return nil
}

// Process incoming chat messages and reply command results to chat initiators.
func (bot *Daemon) ProcessMessages(updates APIUpdates) {
	for _, ding := range updates.Updates {
		// Put processing duration (including API time) into statistics
		beginTimeNano := time.Now().UnixNano()
		if bot.messageOffset <= ding.ID {
			bot.messageOffset = ding.ID + 1
		}
		// Apply rate limit to the user
		origin := ding.Message.From.UserName
		if origin == "" {
			origin = ding.Message.Chat.UserName
		}
		if !bot.userRateLimit.Add(origin, true) {
			if err := bot.ReplyTo(ding.Message.Chat.ID, "rate limited"); err != nil {
				bot.logger.Warningf("ProcessMessages", origin, err, "failed to send message reply")
			}
			continue
		}
		// Do not process messages that arrived prior to server startup
		if ding.Message.Timestamp < misc.StartupTime.Unix() {
			bot.logger.Warningf("ProcessMessages", origin, nil, "ignore message from \"%s\" that arrived before server started up", ding.Message.Chat.UserName)
			continue
		}
		// Do not process non-private chats
		if ding.Message.Chat.Type != ChatTypePrivate {
			bot.logger.Warningf("ProcessMessages", origin, nil, "ignore non-private chat %d", ding.Message.Chat.ID)
			continue
		}
		// /start is not a command
		if ding.Message.Text == "/start" {
			bot.logger.Printf("ProcessMessages", origin, nil, "chat %d is started by %s", ding.Message.Chat.ID, ding.Message.Chat.UserName)
			continue
		}
		// Find and run command in background
		go func(ding APIUpdate, beginTimeNano int64) {
			result := bot.Processor.Process(toolbox.Command{TimeoutSec: CommandTimeoutSec, Content: ding.Message.Text})
			if err := bot.ReplyTo(ding.Message.Chat.ID, result.CombinedOutput); err != nil {
				bot.logger.Warningf("ProcessMessages", ding.Message.Chat.UserName, err, "failed to send message reply")
			}
			DurationStats.Trigger(float64(time.Now().UnixNano() - beginTimeNano))
		}(ding, beginTimeNano)
	}
}

// Immediately begin processing incoming chat messages. Block caller indefinitely.
func (bot *Daemon) StartAndBlock() error {
	// Make a test API call
	testResp, testErr := inet.DoHTTP(inet.HTTPRequest{TimeoutSec: APICallTimeoutSec},
		"https://api.telegram.org/bot%s/getMe", bot.AuthorizationToken)
	if testErr != nil || testResp.StatusCode/200 != 1 {
		return fmt.Errorf("telegrambot.StartAndBlock: test failed - HTTP %d - %v %s", testResp.StatusCode, testErr, string(testResp.Body))
	}
	bot.logger.Printf("StartAndBlock", "", nil, "going to poll for messages")
	lastIdle := time.Now().Unix()
	for {
		if misc.EmergencyLockDown {
			atomic.StoreInt32(&bot.loopIsRunning, 0)
			return misc.ErrEmergencyLockDown
		}
		atomic.StoreInt32(&bot.loopIsRunning, 1)
		// Log a message if the loop has not processed messages for a while
		if time.Now().Unix()-lastIdle > 1800 {
			bot.logger.Printf("Loop", "", nil, "has been idle for %d seconds", 1800)
			lastIdle = time.Now().Unix()
		}
		// Poll for new messages
		updatesResp, updatesErr := inet.DoHTTP(inet.HTTPRequest{TimeoutSec: APICallTimeoutSec},
			"https://api.telegram.org/bot%s/getUpdates?offset=%s", bot.AuthorizationToken, bot.messageOffset)
		var newMessages APIUpdates
		if updatesErr != nil || updatesResp.StatusCode/200 != 1 {
			bot.logger.Warningf("Loop", "", updatesErr, "failed to poll due to HTTP %d %s", updatesResp.StatusCode, string(updatesResp.Body))
			goto sleepAndContinue
		}
		// Deserialise new messages
		if err := json.Unmarshal(updatesResp.Body, &newMessages); err != nil {
			bot.logger.Warningf("Loop", "", err, "failed to decode response JSON")
			goto sleepAndContinue
		}
		if !newMessages.OK {
			bot.logger.Warningf("Loop", "", nil, "API response is not OK - %s", string(updatesResp.Body))
			goto sleepAndContinue
		}
		// Process new messages
		if len(newMessages.Updates) > 0 {
			lastIdle = time.Now().Unix()
			bot.ProcessMessages(newMessages)
		}
	sleepAndContinue:
		select {
		case <-bot.stop:
			atomic.StoreInt32(&bot.loopIsRunning, 0)
			return nil
		case <-time.After(PollIntervalSec * time.Second):
		}
	}
}

// Stop previously started message handling loop.
func (bot *Daemon) Stop() {
	if atomic.CompareAndSwapInt32(&bot.loopIsRunning, 1, 0) {
		bot.stop <- true
	}
}

// Run unit tests on telegram bot. See TestSMTPD_StartAndBlock for bot setup.
func TestTelegramBot(bot *Daemon, t testingstub.T) {
	// Well then it is really difficult to test the chat routine
	// So I am going to only going to start the daemon using invalid configuration, which is definitely failing.
	if err := bot.StartAndBlock(); err == nil || strings.Index(err.Error(), "HTTP") == -1 {
		t.Fatal(err)
	}
	// Repeatedly stopping the daemon should have no negative consequence
	bot.Stop()
	bot.Stop()
}
