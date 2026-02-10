package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/n0needt0/solarcontrol/controller"
	"github.com/n0needt0/solarcontrol/insight"
	"github.com/n0needt0/solarcontrol/telegram"
	"github.com/n0needt0/solarcontrol/wattnode"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	showVersion := flag.Bool("version", false, "Show version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("solarcontrol %s\n", version)
		os.Exit(0)
	}

	// Load config
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Setup logging
	setupLogging(cfg.Logging.Level, cfg.Logging.File)

	slog.Info("starting solarcontrol", "version", version)

	// Connect to WattNode
	wn, err := wattnode.NewReader(
		cfg.WattNode.Port,
		cfg.WattNode.Baud,
		cfg.WattNode.UnitID,
		cfg.WattNode.ScaleFactor,
	)
	if err != nil {
		slog.Error("failed to connect to WattNode", "error", err)
		os.Exit(1)
	}
	defer wn.Close()
	slog.Info("connected to WattNode", "port", cfg.WattNode.Port)

	// Fix WattNode config for split-phase 120/240V
	if err := wn.WriteConfigRegister(wattnode.RegCtAmpsC, 0); err != nil {
		slog.Warn("failed to set CtAmpsC=0", "error", err)
	}
	if err := wn.WriteConfigRegister(wattnode.RegConnectionType, 2); err != nil {
		slog.Warn("failed to set ConnectionType=2", "error", err)
	}
	if err := wn.WriteConfigRegister(wattnode.RegPhaseOffset, 180); err != nil {
		slog.Warn("failed to set PhaseOffset=180", "error", err)
	}
	slog.Info("wattnode config applied", "ct_amps_c", 0, "connection_type", 2, "phase_offset", 180)

	// Connect to Insight
	ins := insight.NewClient(
		cfg.Insight.Host,
		cfg.Insight.ReadPort,
		cfg.Insight.WritePort,
		cfg.Insight.MinGapMs,
		cfg.Insight.TimeoutMs,
	)
	if err := ins.Connect(); err != nil {
		slog.Error("failed to connect to Insight", "error", err)
		os.Exit(1)
	}
	defer ins.Close()
	slog.Info("connected to Insight", "host", cfg.Insight.Host)

	// Zero all inverters on startup if configured
	if cfg.Startup.ZeroAllOnStart {
		slog.Info("zeroing all inverters on startup")
		unitIDs := cfg.Inverters.AllUnitIDs()
		if err := ins.IdleAllInverters(unitIDs); err != nil {
			slog.Error("failed to idle inverters", "error", err)
			// Continue anyway - not fatal
		}
	}

	// Read WattNode voltages for diagnostics
	diag, err := wn.ReadDiagConfig()
	if err != nil {
		slog.Error("failed to read WattNode diag", "error", err)
	} else {
		slog.Info("wattnode diag",
			"voltage_a", diag.VoltageA,
			"voltage_b", diag.VoltageB,
			"energy_total_kwh", diag.EnergySum,
			"energy_a_kwh", diag.EnergyA,
			"energy_b_kwh", diag.EnergyB,
			"ct_amps_a", diag.CtAmpsA,
			"ct_amps_b", diag.CtAmpsB,
			"connection_type", diag.ConnectionType,
			"config_regs_1602_1621", diag.ConfigRegs,
		)
	}

	// Test WattNode read
	power, err := wn.Read()
	if err != nil {
		slog.Error("failed to read WattNode", "error", err)
	} else {
		slog.Info("wattnode test read",
			"l1_w", power.L1,
			"l2_w", power.L2,
			"total_w", power.Total,
		)
	}

	// Test BMS read
	bms, err := ins.ReadBatteryStatus()
	if err != nil {
		slog.Error("failed to read BMS", "error", err)
	} else {
		slog.Info("bms test read",
			"soc", bms.SOC,
			"power", bms.Power,
			"total_soc", bms.TotalSOC(),
		)
	}

	// Build controller config
	ctrlCfg := &controller.Config{
		MasterUnitID: cfg.Inverters.MasterUnitID,
		SlaveUnitIDs: cfg.Inverters.SlaveUnitIDs,
		IdleOrder:    cfg.Inverters.IdleOrder,

		ChargeStartHour: cfg.Charge.StartHour,
		ChargeEndHour:   cfg.Charge.EndHour,

		StartPerInvW:    cfg.Charge.StartPerInvW,
		MaxPerInvW:      cfg.Charge.MaxPerInvW,
		MaxTotalW:       cfg.Charge.MaxTotalW,
		ExportStartW:    cfg.Charge.ExportStartW,
		RampUpHoldSec:   cfg.Charge.RampUpHoldSec,
		RampDownHoldSec: cfg.Charge.RampDownHoldSec,
		DeadBandExportW: cfg.Charge.DeadBandExportW,
		DeadBandImportW: cfg.Charge.DeadBandImportW,

		DischargePerInvW: cfg.Discharge.PerInverterW,

		LegExportThresholdW: cfg.NightGuard.LegExportThresholdW,
		ResumeAllowed:       cfg.NightGuard.ResumeAllowed,

		MaxReadFailures: cfg.Safety.MaxReadFailures,

		GridReadInterval: time.Duration(cfg.WattNode.ReadIntervalSec) * time.Second,
		BMSReadInterval:  time.Duration(cfg.Insight.SOCReadIntervalSec) * time.Second,
	}

	// Create controller
	ctrl := controller.New(ctrlCfg, ins, wn)

	// Create telegram bot
	bot := telegram.NewBot(
		cfg.Telegram.BotToken,
		cfg.Telegram.ChatID,
		cfg.Telegram.PollTimeoutSec,
	)

	// Create telegram handler and connect to bot
	tgHandler := controller.NewTelegramHandler(ctrl, bot, cfg.Telegram.ManualStepW)
	bot.SetHandler(tgHandler)
	ctrl.SetAlerter(tgHandler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start controller
	if err := ctrl.Start(ctx); err != nil {
		slog.Error("failed to start controller", "error", err)
		os.Exit(1)
	}

	// Start telegram bot
	if err := bot.Start(ctx); err != nil {
		slog.Error("failed to start telegram bot", "error", err)
		// Continue without telegram - not fatal
	}

	slog.Info("solarcontrol running")

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutdown signal received")
	cancel()
	bot.Stop()
	ctrl.Stop()

	slog.Info("solarcontrol stopped")
}

func setupLogging(level, file string) {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: logLevel}

	var handler slog.Handler
	if file != "" {
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			// Fall back to stdout
			handler = slog.NewTextHandler(os.Stdout, opts)
		} else {
			handler = slog.NewTextHandler(f, opts)
		}
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
}
