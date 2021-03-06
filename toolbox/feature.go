// An Internet function or system function that takes a text command as input and responds with string text.
package toolbox

import (
	"errors"
	"github.com/HouzuoGuo/laitos/inet"
	"strings"
)

const (
	CombinedTextSeparator = "|" // Separate error and command output in the combined output
	SelfTestTimeoutSec    = 15  // Timeout for outgoing connections among those involved in feature self tests
)

var (
	ErrEmptyCommand     = errors.New("Empty command")
	ErrIncompleteConfig = errors.New("Incomplete configuration")
)

// Execution details for invoking a feature.
type Command struct {
	TimeoutSec int
	Content    string
}

// Modify command content to remove leading and trailing white spaces. Return error result if command becomes empty afterwards.
func (cmd *Command) Trim() *Result {
	cmd.Content = strings.TrimSpace(cmd.Content)
	if cmd.Content == "" {
		return &Result{Error: ErrEmptyCommand, Output: ""}
	}
	return nil
}

// Remove a prefix string from command content and then trim white spaces. Return true only if the prefix was found and removed.
func (cmd *Command) FindAndRemovePrefix(prefix string) (hasPrefix bool) {
	trimmedOriginal := strings.TrimSpace(cmd.Content)
	hasPrefix = strings.HasPrefix(trimmedOriginal, prefix)
	if hasPrefix {
		cmd.Content = strings.TrimSpace(strings.TrimPrefix(trimmedOriginal, prefix))
	}
	return
}

func (cmd *Command) Lines() []string {
	return strings.Split(cmd.Content, "\n")
}

type Trigger string // Trigger is a prefix string that is matched against command input to trigger a feature, each feature has a unique trigger.

// Represent a useful feature that is capable of execution and provide execution result as feedback.
type Feature interface {
	IsConfigured() bool      // Return true only if configuration is present, this is called prior to Initialise().
	SelfTest() error         // Validate and test configuration. It may work only after Initialise() succeeds.
	Initialise() error       // Prepare internal states.
	Trigger() Trigger        // Return a prefix string that is matched against command input to trigger a feature, each feature has a unique trigger.
	Execute(Command) *Result // Execute the command with trigger prefix removed, and return execution result.
}

// Feature's execution result that includes human readable output and error (if any).
type Result struct {
	Command        Command // Help CommandProcessor to keep track of command in execution result
	Error          error   // Result error if there is any
	Output         string  // Human readable normal output excluding error text
	CombinedOutput string  // Human readable error text + normal output. This is set when calling SetCombinedText() function.
}

// Return error text or empty string if error is absent.
func (result *Result) ErrText() string {
	if result.Error == nil {
		return ""
	}
	return result.Error.Error()
}

// Set and return combined error text and output text.
func (result *Result) ResetCombinedText() string {
	result.CombinedOutput = ""
	if result.Error != nil {
		result.CombinedOutput = result.Error.Error()
		if result.Output != "" {
			result.CombinedOutput += CombinedTextSeparator
		}
	}
	result.CombinedOutput += result.Output
	return result.CombinedOutput
}

// If HTTP status is not 2xx or HTTP response already has an error, return an error result. Otherwise return nil.
func HTTPErrorToResult(resp inet.HTTPResponse, err error) *Result {
	if err != nil {
		return &Result{Error: err, Output: string(resp.Body)}
	} else if respErr := resp.Non2xxToError(); respErr != nil {
		return &Result{Error: respErr, Output: string(resp.Body)}
	}
	return nil
}
