package toolbox

import (
	"errors"
	"fmt"
	"github.com/HouzuoGuo/laitos/inet"
	"github.com/HouzuoGuo/laitos/misc"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	// Captured into three groups, mail command looks like: address@domain.tld "this is email subject" this is email body
	RegexMailCommand = regexp.MustCompile(`([a-zA-Z0-9!#$%&'*+-/=?_{|}~.^]+@[a-zA-Z0-9!#$%&'*+-/=?_{|}~.^]+.[a-zA-Z0-9!#$%&'*+-/=?_{|}~.^]+)\s*"(.*)"\s*(.*)`)
	/*
		SOSEmailRecipientMagic is the magic email recipient that corresponds to a built-in list of rescue coordinate
		centre Emails.
	*/
	SOSEmailRecipientMagic = "sos@sos"
	ErrBadSendMailParam    = errors.New(`Example: addr@dom.tld "subj" body. Send SOS to sos@sos.`)
)

// Send outgoing emails.
type SendMail struct {
	MailClient inet.MailClient `json:"MailClient"`

	logger misc.Logger
}

var TestSendMail = SendMail{} // Details are set by init_feature_test.go

func (email *SendMail) IsConfigured() bool {
	return email.MailClient.IsConfigured()
}

func (email *SendMail) SelfTest() error {
	if !email.IsConfigured() {
		return ErrIncompleteConfig
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", email.MailClient.MTAHost, email.MailClient.MTAPort), SelfTestTimeoutSec*time.Second)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func (email *SendMail) Initialise() error {
	email.logger = misc.Logger{ComponentID: email.MailClient.MailFrom, ComponentName: "SendMail"}
	return nil
}

func (email *SendMail) Trigger() Trigger {
	return ".m"
}

func (email *SendMail) Execute(cmd Command) *Result {
	if errResult := cmd.Trim(); errResult != nil {
		return errResult
	}

	params := RegexMailCommand.FindStringSubmatch(cmd.Content)
	if len(params) != 4 {
		return &Result{Error: ErrBadSendMailParam}
	}
	mailTo := params[1]
	mailSubject := params[2]
	mailBody := params[3]
	if strings.TrimSpace(strings.ToLower(mailTo)) == SOSEmailRecipientMagic {
		// SOS emails are sent in background
		email.SendSOS(mailSubject, mailBody)
		return &Result{Output: "Sending SOS"}
	} else {
		// Wait for Email to be sent in foreground, but inform user if it takes too long.
		sendErrChan := make(chan error, 1)
		go func() {
			sendErrChan <- email.MailClient.Send(mailSubject, mailBody, mailTo)
		}()
		select {
		case <-time.After(time.Duration(cmd.TimeoutSec) * time.Second):
			return &Result{Output: "Sending in background"}
		case sendErr := <-sendErrChan:
			if sendErr == nil {
				// Normal result is the length of email body
				return &Result{Output: strconv.Itoa(len(mailBody))}
			} else {
				return &Result{Error: sendErr}
			}
		}
	}
}

// SendSOS delivers an SOS email to public search-and-rescue institutions.
func (email *SendMail) SendSOS(subject, body string) {
	// Prefix body text with some environment information
	body = fmt.Sprintf(`SOS HELP!
The time is %s.
This is the operator of IP address %s.
Please send help: %s`, time.Now().UTC().Format(time.RFC3339), inet.GetPublicIP(), body)

	email.logger.Warningf("SendSOS", subject, nil, "sending SOS email, body is: %s", body)

	for _, recipient := range GetAllSAREmails() {
		go func(recipient string) {
			err := email.MailClient.Send("SOS HELP "+subject, body, recipient)
			email.logger.Warningf("SendSOS", "", err, "attempted to deliver to %s", recipient)
		}(recipient)
	}
}
