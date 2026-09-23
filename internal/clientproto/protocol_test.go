package clientproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFrameV1ShapeForPing(t *testing.T) {
	b, err := Frame(TypePing, "", nil, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"type":"ping"}` {
		t.Errorf("ping frame = %s, want the exact v1 bytes", b)
	}
}

func TestFrameCarriesIDTSAndData(t *testing.T) {
	now := time.Date(2026, 9, 23, 8, 15, 2, 123e6, time.UTC)
	b, err := Frame(TypeCommand, "", Command{Name: CmdMachineInfo, ExpiresAt: "2026-09-23T08:25:02Z"}, now)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeCommand || len(env.ID) != 26 || env.TS != "2026-09-23T08:15:02.123Z" {
		t.Errorf("envelope = %+v", env)
	}
	if !strings.Contains(string(env.Data), `"name":"machine.info"`) {
		t.Errorf("data = %s", env.Data)
	}
}

func TestFrameReplyTo(t *testing.T) {
	b, _ := Frame(TypeAck, "01J8ZQ6T3M6X9K2V7B4N1C5D8E", Ack{Accepted: true}, time.Now())
	if !strings.Contains(string(b), `"reply_to":"01J8ZQ6T3M6X9K2V7B4N1C5D8E"`) {
		t.Errorf("frame = %s", b)
	}
}

func TestValidateArgs(t *testing.T) {
	cases := []struct {
		name, args string
		wantCode   string // "" = valid
	}{
		{CmdMachineInfo, ``, ""},
		{CmdMachineInfo, `{}`, ""},
		{CmdMachineInfo, `{"sections":["network","hardware"]}`, ""},
		{CmdMachineInfo, `{"sections":["gpu"]}`, ErrInvalidArgs},
		{CmdMachineInfo, `{"verbose":true}`, ErrInvalidArgs},
		{CmdEMLyManifestCheck, `{}`, ""},
		{CmdEMLyManifestCheck, `{"force":true}`, ErrInvalidArgs},
		{CmdUpdaterManifestCheck, ``, ""},
		{CmdAppsListUpgradable, `{}`, ""},
		{CmdServiceRestart, `{}`, ""},
		{CmdMachineReboot, `{}`, ""},
		{CmdMachineReboot, `{"delay_seconds":0,"when_user_active":"skip"}`, ""},
		{CmdMachineReboot, `{"delay_seconds":3600}`, ""},
		{CmdMachineReboot, `{"delay_seconds":3601}`, ErrInvalidArgs},
		{CmdMachineReboot, `{"delay_seconds":-1}`, ErrInvalidArgs},
		{CmdMachineReboot, `{"when_user_active":"force"}`, ErrInvalidArgs},
		{CmdMachineReboot, `{"message":"hi"}`, ErrInvalidArgs},
		{"machine.format_disk", `{}`, ErrUnsupportedCommand},
		{CmdMachineInfo, `[1]`, ErrInvalidArgs},
	}
	for _, c := range cases {
		got := ValidateArgs(c.name, json.RawMessage(c.args))
		switch {
		case c.wantCode == "" && got != nil:
			t.Errorf("ValidateArgs(%s, %s) = %+v, want valid", c.name, c.args, got)
		case c.wantCode != "" && (got == nil || got.Code != c.wantCode):
			t.Errorf("ValidateArgs(%s, %s) = %+v, want %s", c.name, c.args, got, c.wantCode)
		}
	}
}

func TestResultTimeout(t *testing.T) {
	if got := ResultTimeout(CmdAppsListUpgradable, nil); got != 180*time.Second {
		t.Errorf("apps.list_upgradable = %v", got)
	}
	if got := ResultTimeout(CmdMachineReboot, json.RawMessage(`{"delay_seconds":600}`)); got != 600*time.Second+15*time.Minute {
		t.Errorf("machine.reboot 600 = %v", got)
	}
	if got := ResultTimeout(CmdMachineReboot, nil); got != 300*time.Second+15*time.Minute {
		t.Errorf("machine.reboot default = %v", got)
	}
}

func TestIntersectKeepsOfferedOrderAndDropsUnknown(t *testing.T) {
	got := Intersect([]string{"b", "zzz", "a"}, []string{"a", "b", "c"})
	if strings.Join(got, ",") != "b,a" {
		t.Errorf("Intersect = %v", got)
	}
}

func TestEveryCommandHasASpec(t *testing.T) {
	for _, name := range []string{CmdMachineInfo, CmdEMLyManifestCheck, CmdUpdaterManifestCheck,
		CmdAppsListUpgradable, CmdServiceRestart, CmdMachineReboot} {
		if _, ok := Commands[name]; !ok {
			t.Errorf("no CommandSpec for %s", name)
		}
	}
	if !Commands[CmdMachineReboot].Destructive || !Commands[CmdServiceRestart].ResultViaEvent {
		t.Error("destructive/result-via-event flags wrong")
	}
}
