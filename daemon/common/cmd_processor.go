package common

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/HouzuoGuo/laitos/misc"
	"github.com/HouzuoGuo/laitos/toolbox"
	"github.com/HouzuoGuo/laitos/toolbox/filter"
	"regexp"
	"strconv"
	"time"
)

const (
	//ErrBadProcessorConfig prefixes error messages in function "IsSaneForInternet".
	ErrBadProcessorConfig = "bad configuration: "

	/*
		PrefixCommandPLT is the magic string to prefix command input, in order to navigate around among the output and
		temporarily alter execution timeout. PLT stands for "position, length, timeout".
	*/
	PrefixCommandPLT = ".plt"
)

// ErrBadPrefix is a command execution error triggered if the command does not contain a valid toolbox feature trigger.
var ErrBadPrefix = errors.New("bad prefix or feature is not configured")

// ErrBadPLT reminds user of the proper syntax to invoke PLT magic.
var ErrBadPLT = errors.New(PrefixCommandPLT + " P L T command")

// RegexCommandWithPLT parses PLT magic parameters position, length, and timeout, all of which are integers.
var RegexCommandWithPLT = regexp.MustCompile(`[^\d]*(\d+)[^\d]+(\d+)[^\d]*(\d+)(.*)`)

var DurationStats = misc.NewStats() // DurationStats stores statistics of duration of all commands executed.

// Pre-configured environment and configuration for processing feature commands.
type CommandProcessor struct {
	Features       *toolbox.FeatureSet    // Features is the aggregation of initialised toolbox feature routines.
	CommandFilters []filter.CommandFilter // CommandFilters are applied one by one to alter input command content and/or timeout.
	ResultFilters  []filter.ResultFilter  // ResultFilters are applied one by one to alter command execution result.

	logger misc.Logger
}

// SetLogger assigns a logger to command processor and all of its filters.
func (proc *CommandProcessor) SetLogger(logger misc.Logger) {
	proc.logger = logger
	for _, b := range proc.ResultFilters {
		b.SetLogger(logger)
	}
}

/*
IsEmpty returns true only if the command processor does not have any command filter configuration, which means the
command processor is not configured for use.
Normally, a command processor configuration should at least have a PIN filter.
*/
func (proc *CommandProcessor) IsEmpty() bool {
	return proc.CommandFilters == nil || len(proc.CommandFilters) == 0
}

/*
From the prospect of Internet-facing mail processor and Twilio hooks, check that parameters are within sane range.
Return a zero-length slice if everything looks OK.
*/
func (proc *CommandProcessor) IsSaneForInternet() (errs []error) {
	errs = make([]error, 0, 0)
	// Check for nils too, just in case.
	if proc.Features == nil {
		errs = append(errs, errors.New(ErrBadProcessorConfig+"FeatureSet is not assigned"))
	} else {
		if len(proc.Features.LookupByTrigger) == 0 {
			errs = append(errs, errors.New(ErrBadProcessorConfig+"FeatureSet is not initialised or all features are lacking configuration"))
		}
	}
	if proc.CommandFilters == nil {
		errs = append(errs, errors.New(ErrBadProcessorConfig+"CommandFilters is not assigned"))
	} else {
		// Check whether PIN bridge is sanely configured
		seenPIN := false
		for _, cmdBridge := range proc.CommandFilters {
			if pin, yes := cmdBridge.(*filter.PINAndShortcuts); yes {
				if pin.PIN == "" && (pin.Shortcuts == nil || len(pin.Shortcuts) == 0) {
					errs = append(errs, errors.New(ErrBadProcessorConfig+"PIN is empty and there is no shortcut defined, hence no command will ever execute."))
				}
				if pin.PIN != "" && len(pin.PIN) < 7 {
					errs = append(errs, errors.New(ErrBadProcessorConfig+"PIN is too short, make it at least 7 characters long to be somewhat secure."))
				}
				seenPIN = true
				break
			}
		}
		if !seenPIN {
			errs = append(errs, errors.New(ErrBadProcessorConfig+"\"PINAndShortcuts\" bridge is not used, this is horribly insecure."))
		}
	}
	if proc.ResultFilters == nil {
		errs = append(errs, errors.New(ErrBadProcessorConfig+"ResultFilters is not assigned"))
	} else {
		// Check whether string linter is sanely configured
		seenLinter := false
		for _, resultBridge := range proc.ResultFilters {
			if linter, yes := resultBridge.(*filter.LintText); yes {
				if linter.MaxLength < 35 || linter.MaxLength > 4096 {
					errs = append(errs, errors.New(ErrBadProcessorConfig+"Maximum output length is not within [35, 4096]. This may cause undesired telephone cost."))
				}
				seenLinter = true
				break
			}
		}
		if !seenLinter {
			errs = append(errs, errors.New(ErrBadProcessorConfig+"\"LintText\" bridge is not used, this may cause crashes or undesired telephone cost."))
		}
	}
	return
}

/*
Process applies filters to the command, invokes toolbox feature functions to process the content, and then applies
filters to the execution result and return.
A special content prefix called "PLT prefix" alters filter settings to temporarily override timeout and max.length
settings, and it may optionally discard a number of characters from the beginning.
*/
func (proc *CommandProcessor) Process(cmd toolbox.Command) (ret *toolbox.Result) {
	// Put execution duration into statistics
	beginTimeNano := time.Now().UnixNano()
	defer func() {
		DurationStats.Trigger(float64(time.Now().UnixNano() - beginTimeNano))
	}()
	// Do not execute a command if global lock down is effective
	if misc.EmergencyLockDown {
		return &toolbox.Result{Error: misc.ErrEmergencyLockDown}
	}
	var bridgeErr error
	var matchedFeature toolbox.Feature
	var overrideLintText filter.LintText
	var hasOverrideLintText bool
	logCommandContent := cmd.Content
	// Walk the command through all bridges
	for _, cmdBridge := range proc.CommandFilters {
		cmd, bridgeErr = cmdBridge.Transform(cmd)
		if bridgeErr != nil {
			ret = &toolbox.Result{Error: bridgeErr}
			goto result
		}
	}
	// Trim spaces and expect non-empty command
	if ret = cmd.Trim(); ret != nil {
		goto result
	}
	// If bridges did not throw an error, they should have got rid of bits and pieces of command content that must not be logged.
	logCommandContent = cmd.Content
	// Look for PLT (position, length, timeout) override, it is going to affect LintText bridge.
	if cmd.FindAndRemovePrefix(PrefixCommandPLT) {
		// Find the configured LintText bridge
		for _, resultBridge := range proc.ResultFilters {
			if aBridge, isLintText := resultBridge.(*filter.LintText); isLintText {
				overrideLintText = *aBridge
				hasOverrideLintText = true
				break
			}
		}
		if !hasOverrideLintText {
			ret = &toolbox.Result{Error: errors.New("PLT is not available because LintText is not used")}
			goto result
		}
		// Parse P. L. T. <cmd> parameters
		pltParams := RegexCommandWithPLT.FindStringSubmatch(cmd.Content)
		if len(pltParams) != 5 { // 4 groups + 1
			ret = &toolbox.Result{Error: ErrBadPLT}
			goto result
		}
		var intErr error
		if overrideLintText.BeginPosition, intErr = strconv.Atoi(pltParams[1]); intErr != nil {
			ret = &toolbox.Result{Error: ErrBadPLT}
			goto result
		}
		if overrideLintText.MaxLength, intErr = strconv.Atoi(pltParams[2]); intErr != nil {
			ret = &toolbox.Result{Error: ErrBadPLT}
			goto result
		}
		if cmd.TimeoutSec, intErr = strconv.Atoi(pltParams[3]); intErr != nil {
			ret = &toolbox.Result{Error: ErrBadPLT}
			goto result
		}
		cmd.Content = pltParams[4]
		if cmd.Content == "" {
			ret = &toolbox.Result{Error: ErrBadPLT}
			goto result
		}
	}
	// Look for command's prefix among configured features
	for prefix, configuredFeature := range proc.Features.LookupByTrigger {
		if cmd.FindAndRemovePrefix(string(prefix)) {
			matchedFeature = configuredFeature
			break
		}
	}
	// Unknown command prefix or the requested feature is not configured
	if matchedFeature == nil {
		ret = &toolbox.Result{Error: ErrBadPrefix}
		goto result
	}
	// Run the feature
	proc.logger.Printf("Process", "CommandProcessor", nil, "going to run %+v", cmd)
	defer func() {
		proc.logger.Printf("Process", "CommandProcessor", nil, "finished running %+v - %s", cmd, ret.CombinedOutput)
	}()
	ret = matchedFeature.Execute(cmd)

result:
	// Command in the result structure is mainly used for logging purpose
	ret.Command = cmd
	/*
		Features may have modified command in-place to remove certain content and it's OK to do that.
		But to make log messages more meaningful, it is better to restore command content to the modified one
		after triggering bridges, and before triggering features.
	*/
	ret.Command.Content = logCommandContent
	// Walk through result bridges
	for _, resultBridge := range proc.ResultFilters {
		// LintText bridge may have been manipulated by override
		if _, isLintText := resultBridge.(*filter.LintText); isLintText && hasOverrideLintText {
			resultBridge = &overrideLintText
		}
		if err := resultBridge.Transform(ret); err != nil {
			return &toolbox.Result{Command: ret.Command, Error: bridgeErr}
		}
	}
	return
}

// Return a realistic command processor for test cases. The only feature made available and initialised is shell execution.
func GetTestCommandProcessor() *CommandProcessor {
	// Prepare feature set - the shell execution feature should be available even without configuration
	features := &toolbox.FeatureSet{}
	if err := features.Initialise(); err != nil {
		panic(err)
	}
	// Prepare realistic command bridges
	commandBridges := []filter.CommandFilter{
		&filter.PINAndShortcuts{PIN: "verysecret"},
		&filter.TranslateSequences{Sequences: [][]string{{"alpha", "beta"}}},
	}
	// Prepare realistic result bridges
	resultBridges := []filter.ResultFilter{
		&filter.ResetCombinedText{},
		&filter.LintText{TrimSpaces: true, MaxLength: 35},
		&filter.SayEmptyOutput{},
		&filter.NotifyViaEmail{},
	}
	return &CommandProcessor{
		Features:       features,
		CommandFilters: commandBridges,
		ResultFilters:  resultBridges,
	}
}

// Return a do-nothing yet sane command processor that has a random long password, rendering it unable to invoke any feature.
func GetEmptyCommandProcessor() *CommandProcessor {
	features := &toolbox.FeatureSet{}
	if err := features.Initialise(); err != nil {
		panic(err)
	}
	randPIN := make([]byte, 128)
	if _, err := rand.Read(randPIN); err != nil {
		panic(err)
	}
	return &CommandProcessor{
		Features: features,
		CommandFilters: []filter.CommandFilter{
			&filter.PINAndShortcuts{PIN: hex.EncodeToString(randPIN)},
		},
		ResultFilters: []filter.ResultFilter{
			&filter.ResetCombinedText{},
			&filter.LintText{MaxLength: 35},
			&filter.SayEmptyOutput{},
		},
	}
}

/*
GetInsaneCommandProcessor returns a command processor that does not have a sane configuration for general usage.
This is a test case helper.
*/
func GetInsaneCommandProcessor() *CommandProcessor {
	features := &toolbox.FeatureSet{}
	if err := features.Initialise(); err != nil {
		panic(err)
	}
	return &CommandProcessor{
		Features: features,
		CommandFilters: []filter.CommandFilter{
			&filter.PINAndShortcuts{PIN: "short"},
		},
		ResultFilters: []filter.ResultFilter{
			&filter.ResetCombinedText{},
			&filter.LintText{MaxLength: 10},
			&filter.SayEmptyOutput{},
		},
	}
}
