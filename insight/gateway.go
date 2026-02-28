package insight

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// GatewayRebooter handles rebooting the Insight Facility gateway via its REST API.
type GatewayRebooter struct {
	host     string
	username string
	password string
	client   *http.Client
}

// NewGatewayRebooter creates a rebooter for the Insight gateway.
func NewGatewayRebooter(host, username, password string) *GatewayRebooter {
	return &GatewayRebooter{
		host:     host,
		username: username,
		password: password,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

type authResponse struct {
	Session string `json:"session"`
}

type varsResponse struct {
	OTK string `json:"OTK"`
}

// Reboot authenticates to the gateway API and sends the reboot command.
// Three HTTP calls: login → get OTK → set reboot. No browser needed.
func (g *GatewayRebooter) Reboot(ctx context.Context) error {
	baseURL := fmt.Sprintf("https://%s", g.host)

	// Step 1: Login — POST /auth
	slog.Info("gateway_reboot_login", "host", g.host)
	authBody := fmt.Sprintf("username=%s&password=%s&session=true", g.username, g.password)
	session, err := g.postAPI(ctx, baseURL+"/auth", authBody, "", "")
	if err != nil {
		return fmt.Errorf("gateway login: %w", err)
	}

	var auth authResponse
	if err := json.Unmarshal([]byte(session), &auth); err != nil {
		return fmt.Errorf("gateway login parse: %w (body: %s)", err, session)
	}
	if auth.Session == "" {
		return fmt.Errorf("gateway login: no session token (body: %s)", session)
	}
	slog.Info("gateway_reboot_logged_in")

	// Step 2: Get OTK — POST /vars with any variable read
	varsBody := "name=SERIALNUM"
	varsResp, err := g.postAPI(ctx, baseURL+"/vars", varsBody, auth.Session, "")
	if err != nil {
		return fmt.Errorf("gateway get OTK: %w", err)
	}

	var vars varsResponse
	if err := json.Unmarshal([]byte(varsResp), &vars); err != nil {
		return fmt.Errorf("gateway OTK parse: %w (body: %s)", err, varsResp)
	}
	if vars.OTK == "" {
		return fmt.Errorf("gateway OTK: no OTK in response (body: %s)", varsResp)
	}
	slog.Info("gateway_reboot_otk_received")

	// Step 3: Reboot — POST /set with reboot command
	setBody := "/SCB/LSYS/REBOOT= 1"
	setResp, err := g.postAPI(ctx, baseURL+"/set", setBody, auth.Session, vars.OTK)
	if err != nil {
		return fmt.Errorf("gateway reboot command: %w", err)
	}

	slog.Info("gateway_reboot_initiated", "host", g.host, "response", setResp)
	return nil
}

// devListResponse represents the DEVLIST response from the gateway API.
type devListResponse struct {
	Values []struct {
		Value []devListEntry `json:"value"`
	} `json:"values"`
	OTK string `json:"OTK"`
}

type devListEntry struct {
	Name       string            `json:"name"`
	Instance   int               `json:"instance"`
	IsActive   string            `json:"isActive"`
	Attributes map[string]string `json:"attributes"`
}

// CycleInverters puts all XW Pro inverters into standby, waits, then sets them
// back to operating. This resets the inverter's internal EPC state machine,
// which can get stuck accepting Modbus writes without actually acting on them.
func (g *GatewayRebooter) CycleInverters(ctx context.Context) error {
	baseURL := fmt.Sprintf("https://%s", g.host)

	// Step 1: Login
	slog.Info("inverter_cycle_login", "host", g.host)
	authBody := fmt.Sprintf("username=%s&password=%s&session=true", g.username, g.password)
	session, err := g.postAPI(ctx, baseURL+"/auth", authBody, "", "")
	if err != nil {
		return fmt.Errorf("gateway login: %w", err)
	}

	var auth authResponse
	if err := json.Unmarshal([]byte(session), &auth); err != nil {
		return fmt.Errorf("gateway login parse: %w (body: %s)", err, session)
	}
	if auth.Session == "" {
		return fmt.Errorf("gateway login: no session (body: %s)", session)
	}

	// Step 2: Get DEVLIST to find XW inverter instances
	devResp, err := g.postAPI(ctx, baseURL+"/vars", "name=DEVLIST", auth.Session, "")
	if err != nil {
		return fmt.Errorf("get DEVLIST: %w", err)
	}

	var devList devListResponse
	if err := json.Unmarshal([]byte(devResp), &devList); err != nil {
		return fmt.Errorf("parse DEVLIST: %w (body: %.200s)", err, devResp)
	}

	// Extract XW instances (xanbus interface only)
	var xwInstances []int
	if len(devList.Values) > 0 {
		for _, dev := range devList.Values[0].Value {
			if dev.Name == "XW" && dev.Attributes["interface"] == "xanbus" && dev.IsActive == "true" {
				xwInstances = append(xwInstances, dev.Instance)
			}
		}
	}
	if len(xwInstances) == 0 {
		return fmt.Errorf("no active XW inverters found in DEVLIST")
	}

	otk := devList.OTK
	slog.Info("inverter_cycle_found", "count", len(xwInstances), "instances", xwInstances)

	// Step 3: Set all to standby (CFG_OPMODE = 2)
	for _, inst := range xwInstances {
		setBody := fmt.Sprintf("[%d]/XW/DEV/CFG_OPMODE= 2", inst)
		resp, err := g.postAPI(ctx, baseURL+"/set", setBody, auth.Session, otk)
		if err != nil {
			return fmt.Errorf("set standby instance %d: %w", inst, err)
		}
		// Update OTK from response
		var setResp varsResponse
		if err := json.Unmarshal([]byte(resp), &setResp); err == nil && setResp.OTK != "" {
			otk = setResp.OTK
		}
		slog.Info("inverter_standby", "instance", inst)
	}

	// Step 4: Wait for inverters to fully enter standby
	slog.Info("inverter_cycle_waiting", "seconds", 10)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
	}

	// Step 5: Set all back to operating (CFG_OPMODE = 3)
	for _, inst := range xwInstances {
		setBody := fmt.Sprintf("[%d]/XW/DEV/CFG_OPMODE= 3", inst)
		resp, err := g.postAPI(ctx, baseURL+"/set", setBody, auth.Session, otk)
		if err != nil {
			return fmt.Errorf("set operating instance %d: %w", inst, err)
		}
		var setResp varsResponse
		if err := json.Unmarshal([]byte(resp), &setResp); err == nil && setResp.OTK != "" {
			otk = setResp.OTK
		}
		slog.Info("inverter_operating", "instance", inst)
	}

	slog.Info("inverter_cycle_complete", "count", len(xwInstances))
	return nil
}

// postAPI sends a POST request to the gateway API.
func (g *GatewayRebooter) postAPI(ctx context.Context, url, body, authToken, otk string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/plain")
	if authToken != "" {
		req.Header.Set("authToken", authToken)
	}
	if otk != "" {
		req.Header.Set("otk", otk)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return string(respBody), nil
}
