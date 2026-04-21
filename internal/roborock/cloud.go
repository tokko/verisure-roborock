// Package roborock/cloud.go implements a minimal Roborock cloud client
// used exclusively by cmd/fetch-tokens to retrieve local device tokens.
// Day-to-day vacuum control uses the local miIO protocol, not this.
package roborock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Regional Roborock cloud endpoints.
var roborockRegions = map[string]string{
	"eu": "https://euiot.roborock.com",
	"us": "https://usiot.roborock.com",
	"cn": "https://iot.roborock.com",
	"sg": "https://sgiot.roborock.com",
}

// CloudDevice is a device from the Roborock cloud device list.
type CloudDevice struct {
	DUID      string `json:"duid"`
	Name      string `json:"name"`
	LocalKey  string `json:"localKey"` // 32-char hex — the miIO token
	ProductID string `json:"productId"`
	Online    bool   `json:"online"`
	// Network info present on some firmware/API versions.
	NetworkInfo *struct {
		LocalIP string `json:"localIp"`
	} `json:"networkInfo,omitempty"`
}

// CloudClient authenticates with the Roborock cloud and fetches device tokens.
type CloudClient struct {
	baseURL  string
	username string
	http     *http.Client

	rruid string
	token string
}

// NewCloudClient creates a Roborock cloud client.
// region: "eu" (default), "us", "cn", "sg"
func NewCloudClient(username, region string) (*CloudClient, error) {
	if region == "" {
		region = "eu"
	}
	// Accept both "eu" and country codes like "de", "se" → map to EU.
	base, ok := roborockRegions[region]
	if !ok {
		// Country codes in the EU zone map to "eu".
		base = roborockRegions["eu"]
	}
	return &CloudClient{
		baseURL:  base,
		username: username,
		http:     &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// RequestEmailCode triggers a verification code to be sent to the user's email.
func (c *CloudClient) RequestEmailCode(ctx context.Context) error {
	return c.post(ctx, "/api/v1/sendEmailCode", map[string]string{
		"username": c.username,
		"type":     "auth",
	}, nil)
}

// LoginWithCode authenticates using the code received by email.
func (c *CloudClient) LoginWithCode(ctx context.Context, code string) error {
	var resp struct {
		Code int `json:"code"`
		Data struct {
			UID   int    `json:"uid"`
			Token string `json:"token"`
			RRuid string `json:"rruid"`
		} `json:"data"`
		Msg string `json:"msg"`
	}

	if err := c.post(ctx, "/api/v1/login/email", map[string]string{
		"username":       c.username,
		"verifycode":     strings.TrimSpace(code),
		"verifycodetype": "AUTH_EMAIL_CODE",
	}, &resp); err != nil {
		return err
	}

	if resp.Code != 200 {
		return fmt.Errorf("login error %d: %s", resp.Code, resp.Msg)
	}
	if resp.Data.Token == "" {
		return fmt.Errorf("login succeeded but no token returned")
	}

	c.token = resp.Data.Token
	c.rruid = resp.Data.RRuid
	return nil
}

// Devices fetches all devices from the account's home and returns the list.
func (c *CloudClient) Devices(ctx context.Context) ([]CloudDevice, error) {
	if c.token == "" || c.rruid == "" {
		return nil, fmt.Errorf("not authenticated — call LoginWithCode first")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/getHomeDetail", nil)
	if err != nil {
		return nil, err
	}
	// Authorization: Basic base64(rruid:token)
	creds := base64.StdEncoding.EncodeToString([]byte(c.rruid + ":" + c.token))
	req.Header.Set("Authorization", "Basic "+creds)
	req.Header.Set("Accept", "application/json")

	httpResp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}

	// The response structure varies; try both flat and nested device lists.
	var flat struct {
		Code int `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Devices         []CloudDevice `json:"devices"`
			ReceivedDevices []CloudDevice `json:"receivedDevices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &flat); err != nil {
		return nil, fmt.Errorf("parse home detail: %w (body: %.300s)", err, body)
	}
	if flat.Code != 200 {
		return nil, fmt.Errorf("getHomeDetail error %d: %s", flat.Code, flat.Msg)
	}

	devices := append(flat.Data.Devices, flat.Data.ReceivedDevices...)
	return devices, nil
}

func (c *CloudClient) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, rb)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
