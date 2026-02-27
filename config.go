package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	WattNode   WattNodeConfig   `yaml:"wattnode"`
	Insight    InsightConfig    `yaml:"insight"`
	Inverters  InvertersConfig  `yaml:"inverters"`
	Charge     ChargeConfig     `yaml:"charge"`
	Discharge  DischargeConfig  `yaml:"discharge"`
	NightGuard NightGuardConfig `yaml:"night_guard"`
	Safety     SafetyConfig     `yaml:"safety"`
	Startup    StartupConfig    `yaml:"startup"`
	Logging    LoggingConfig    `yaml:"logging"`
	Telegram   TelegramConfig   `yaml:"telegram"`
}

type WattNodeConfig struct {
	Port            string  `yaml:"port"`
	Baud            int     `yaml:"baud"`
	UnitID          byte    `yaml:"unit_id"`
	ReadIntervalSec int     `yaml:"read_interval_sec"`
	ScaleFactor     float32 `yaml:"scale_factor"`
}

type InsightConfig struct {
	Host               string `yaml:"host"`
	ReadPort           int    `yaml:"read_port"`
	WritePort          int    `yaml:"write_port"`
	MinGapMs           int    `yaml:"min_gap_ms"`
	TimeoutMs          int    `yaml:"timeout_ms"`
	SOCReadIntervalSec int    `yaml:"soc_read_interval_sec"`
	KeepaliveSec       int    `yaml:"keepalive_sec"`
	GatewayUser        string `yaml:"gateway_user"`
	GatewayPassword    string `yaml:"gateway_password"`
}

type InvertersConfig struct {
	MasterUnitID byte   `yaml:"master_unit_id"`
	SlaveUnitIDs []byte `yaml:"slave_unit_ids"`
	IdleOrder    []byte `yaml:"idle_order"`
}

// AllUnitIDs returns all inverter unit IDs in order (master first)
func (c *InvertersConfig) AllUnitIDs() []byte {
	ids := make([]byte, 0, 4)
	ids = append(ids, c.MasterUnitID)
	ids = append(ids, c.SlaveUnitIDs...)
	return ids
}

type ChargeConfig struct {
	StartHour        int `yaml:"start_hour"`
	EndHour          int `yaml:"end_hour"`
	StartPerInvW     int `yaml:"start_per_inverter_w"`
	MaxPerInvW       int `yaml:"max_per_inverter_w"`
	MaxTotalW        int `yaml:"max_total_w"`
	ExportStartW      int `yaml:"export_start_w"`
	RampUpHoldSec     int `yaml:"ramp_up_hold_sec"`
	RampDownHoldSec   int `yaml:"ramp_down_hold_sec"`
	DeadBandExportW   int `yaml:"dead_band_export_w"`
	DeadBandImportW  int `yaml:"dead_band_import_w"`
}

type DischargeConfig struct {
	PerInverterW    int `yaml:"per_inverter_w"`
	MaxPerInvW      int `yaml:"max_per_inverter_w"`
}

type NightGuardConfig struct {
	LegExportThresholdW int  `yaml:"leg_export_threshold_w"`
	ResumeAllowed       bool `yaml:"resume_allowed"`
}

type SafetyConfig struct {
	MaxReadFailures int `yaml:"max_read_failures"`
}

type StartupConfig struct {
	ZeroAllOnStart bool `yaml:"zero_all_on_start"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

type TelegramConfig struct {
	BotToken       string `yaml:"bot_token"`
	ChatID         string `yaml:"chat_id"`
	PollTimeoutSec int    `yaml:"poll_timeout_sec"`
	ManualStepW    int    `yaml:"manual_step_w"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.WattNode.Port == "" {
		return fmt.Errorf("wattnode.port required")
	}
	if c.Insight.Host == "" {
		return fmt.Errorf("insight.host required")
	}
	if c.Inverters.MasterUnitID == 0 {
		return fmt.Errorf("inverters.master_unit_id required")
	}
	if len(c.Inverters.SlaveUnitIDs) != 3 {
		return fmt.Errorf("inverters.slave_unit_ids must have 3 entries")
	}
	if len(c.Inverters.IdleOrder) != 4 {
		return fmt.Errorf("inverters.idle_order must have 4 entries")
	}
	if c.Charge.StartHour >= c.Charge.EndHour {
		return fmt.Errorf("charge.start_hour must be before end_hour")
	}
	// Default discharge max to charge max (hardware limit per inverter)
	if c.Discharge.MaxPerInvW == 0 {
		c.Discharge.MaxPerInvW = c.Charge.MaxPerInvW
	}
	return nil
}
