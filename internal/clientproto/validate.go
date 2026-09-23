package clientproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Reboot argument bounds and defaults (§7.6).
const (
	RebootDefaultDelaySeconds = 300
	RebootMaxDelaySeconds     = 3600
	WhenUserActiveWarn        = "warn"
	WhenUserActiveSkip        = "skip"
)

// MachineInfoSections are the allowed values of machine.info's "sections".
var MachineInfoSections = []string{"identity", "logged_user", "network", "site", "config", "hardware", "emly", "update"}

type MachineInfoArgs struct {
	Sections []string `json:"sections,omitempty"`
}

type RebootArgs struct {
	DelaySeconds   *int   `json:"delay_seconds,omitempty"`
	WhenUserActive string `json:"when_user_active,omitempty"`
}

// Delay returns the effective delay, applying the default.
func (a RebootArgs) Delay() int {
	if a.DelaySeconds == nil {
		return RebootDefaultDelaySeconds
	}
	return *a.DelaySeconds
}

// Mode returns the effective when_user_active, applying the default.
func (a RebootArgs) Mode() string {
	if a.WhenUserActive == "" {
		return WhenUserActiveWarn
	}
	return a.WhenUserActive
}

// decodeStrict decodes raw into v rejecting unknown fields. Empty raw is
// "no arguments" and always valid. An argument the verb does not know is an
// error, not a tolerance: an ignored argument is a command run differently
// from how the operator asked (§5.1).
func decodeStrict(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func invalid(format string, args ...any) *ErrorBody {
	return &ErrorBody{Code: ErrInvalidArgs, Message: fmt.Sprintf(format, args...)}
}

// ValidateArgs checks a command's arguments. nil means valid.
func ValidateArgs(name string, raw json.RawMessage) *ErrorBody {
	switch name {
	case CmdMachineInfo:
		var a MachineInfoArgs
		if err := decodeStrict(raw, &a); err != nil {
			return invalid("machine.info: %v", err)
		}
		for _, s := range a.Sections {
			if !contains(MachineInfoSections, s) {
				return invalid("machine.info: unknown section %q", s)
			}
		}
		return nil
	case CmdMachineReboot:
		var a RebootArgs
		if err := decodeStrict(raw, &a); err != nil {
			return invalid("machine.reboot: %v", err)
		}
		if d := a.Delay(); d < 0 || d > RebootMaxDelaySeconds {
			return invalid("machine.reboot: delay_seconds must be between 0 and %d", RebootMaxDelaySeconds)
		}
		if m := a.Mode(); m != WhenUserActiveWarn && m != WhenUserActiveSkip {
			return invalid("machine.reboot: when_user_active must be %q or %q", WhenUserActiveWarn, WhenUserActiveSkip)
		}
		return nil
	case CmdEMLyManifestCheck, CmdUpdaterManifestCheck, CmdAppsListUpgradable, CmdServiceRestart:
		var none struct{}
		if err := decodeStrict(raw, &none); err != nil {
			return invalid("%s takes no arguments: %v", name, err)
		}
		return nil
	default:
		return &ErrorBody{Code: ErrUnsupportedCommand, Message: fmt.Sprintf("unknown command %q", name)}
	}
}

// ResultTimeout is how long after the ack the server waits for the outcome
// of a (valid) command. For machine.reboot it adds the requested delay.
func ResultTimeout(name string, raw json.RawMessage) time.Duration {
	spec := Commands[name]
	if name == CmdMachineReboot {
		var a RebootArgs
		_ = decodeStrict(raw, &a)
		return time.Duration(a.Delay())*time.Second + spec.ResultTimeout
	}
	return spec.ResultTimeout
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
