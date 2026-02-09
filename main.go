package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/n0needt0/solarcontrol/insight"
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
	)
	if err != nil {
		slog.Error("failed to connect to WattNode", "error", err)
		os.Exit(1)
	}
	defer wn.Close()
	slog.Info("connected to WattNode", "port", cfg.WattNode.Port)

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

	// Test WattNode read
	power, err := wn.Read()
	if err != nil {
		slog.Error("failed to read WattNode", "error", err)
	} else {
		slog.Info("wattnode read",
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
		slog.Info("bms read",
			"soc", bms.SOC,
			"power", bms.Power,
			"total_soc", bms.TotalSOC(),
		)
	}

	// Wait for shutdown signal
	slog.Info("solarcontrol ready - waiting for shutdown signal")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutting down")
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
