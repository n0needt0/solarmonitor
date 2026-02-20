package insight

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/chromedp/chromedp"
)

// GatewayRebooter handles rebooting the Insight Facility gateway via its web UI.
type GatewayRebooter struct {
	host     string
	username string
	password string
}

// NewGatewayRebooter creates a rebooter for the Insight gateway web UI.
func NewGatewayRebooter(host, username, password string) *GatewayRebooter {
	return &GatewayRebooter{
		host:     host,
		username: username,
		password: password,
	}
}

// Reboot logs into the gateway web UI and clicks "Restart Gateway".
// Returns nil on success. Blocks until the restart is initiated (~15s).
func (g *GatewayRebooter) Reboot(ctx context.Context) error {
	slog.Info("gateway_reboot_starting", "host", g.host)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("ignore-certificate-errors", true),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	timeoutCtx, timeoutCancel := context.WithTimeout(browserCtx, 60*time.Second)
	defer timeoutCancel()

	baseURL := fmt.Sprintf("https://%s", g.host)

	// Step 1: Navigate to login page, fill password, click Login
	slog.Info("gateway_reboot_login")
	var result string
	if err := chromedp.Run(timeoutCtx,
		chromedp.Navigate(baseURL),
		chromedp.Sleep(3*time.Second),
		// Fill password via JS (Angular input binding)
		chromedp.Evaluate(fmt.Sprintf(`
			(function() {
				var pw = document.querySelector('input[type="password"]');
				if (!pw) return 'no password field';
				pw.value = '%s';
				pw.dispatchEvent(new Event('input', { bubbles: true }));
				return 'ok';
			})()
		`, g.password), &result),
	); err != nil {
		return fmt.Errorf("gateway login page: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("gateway login: %s", result)
	}

	// Click Login button
	if err := chromedp.Run(timeoutCtx,
		chromedp.Evaluate(`
			(function() {
				var buttons = document.querySelectorAll('button');
				for (var i = 0; i < buttons.length; i++) {
					if (buttons[i].textContent.trim() === 'Login') {
						buttons[i].click();
						return 'ok';
					}
				}
				return 'no login button';
			})()
		`, &result),
		chromedp.Sleep(5*time.Second),
	); err != nil {
		return fmt.Errorf("gateway login click: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("gateway login: %s", result)
	}
	slog.Info("gateway_reboot_logged_in")

	// Step 2: Navigate to configuration page
	configURL := baseURL + "/#/gateway/configuration"
	if err := chromedp.Run(timeoutCtx,
		chromedp.Navigate(configURL),
		chromedp.Sleep(5*time.Second),
	); err != nil {
		return fmt.Errorf("gateway config page: %w", err)
	}

	// Step 3: Click "Restart Gateway" button
	slog.Info("gateway_reboot_clicking_restart")
	if err := chromedp.Run(timeoutCtx,
		chromedp.Evaluate(`
			(function() {
				var buttons = document.querySelectorAll('button');
				for (var i = 0; i < buttons.length; i++) {
					if (buttons[i].textContent.trim() === 'Restart Gateway') {
						buttons[i].click();
						return 'ok';
					}
				}
				return 'no restart button';
			})()
		`, &result),
		chromedp.Sleep(2*time.Second),
	); err != nil {
		return fmt.Errorf("gateway restart click: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("gateway restart: %s", result)
	}

	// Step 4: Confirm the restart dialog (click OK or Restart now)
	slog.Info("gateway_reboot_confirming")
	if err := chromedp.Run(timeoutCtx,
		chromedp.Evaluate(`
			(function() {
				var buttons = document.querySelectorAll('button');
				for (var i = 0; i < buttons.length; i++) {
					var text = buttons[i].textContent.trim();
					if (text === 'OK' || text === 'Restart now') {
						buttons[i].click();
						return 'confirmed: ' + text;
					}
				}
				return 'no confirm button';
			})()
		`, &result),
		chromedp.Sleep(2*time.Second),
	); err != nil {
		return fmt.Errorf("gateway restart confirm: %w", err)
	}

	slog.Info("gateway_reboot_initiated", "host", g.host, "confirm", result)
	return nil
}
