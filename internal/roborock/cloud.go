// Package roborock/cloud.go implements a minimal Roborock cloud client
// used exclusively by cmd/fetch-tokens to retrieve local device tokens.
// Day-to-day vacuum control uses the local miIO protocol, not this.
package roborock

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	clientID string // header_clientid: base64(md5(username)), required by Roborock API
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
	// header_clientid = base64(md5(username)) — required by the Roborock API.
	h := md5.Sum([]byte(username))
	cid := base64.StdEncoding.EncodeToString(h[:])

	return &CloudClient{
		baseURL:  base,
		username: username,
		clientID: cid,
		http:     &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// RequestEmailCode triggers a verification code to be sent to the user's email.
// Returns the raw API response message so the caller can surface it for debugging.
func (c *CloudClient) RequestEmailCode(ctx context.Context) (string, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}
	if err := c.postForm(ctx, "/api/v1/sendEmailCode", url.Values{
		"username": {c.username},
		"type":     {"auth"},
	}, &resp, c.clientID); err != nil {
		return "", err
	}
	if resp.Code != 200 {
		return resp.Msg, fmt.Errorf("send code error %d: %s", resp.Code, resp.Msg)
	}
	return resp.Msg, nil
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

	if err := c.postForm(ctx, "/api/v1/loginWithCode", url.Values{
		"username":       {c.username},
		"verifycode":     {strings.TrimSpace(code)},
		"verifycodetype": {"AUTH_EMAIL_CODE"},
	}, &resp, c.clientID); err != nil {
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

// postForm sends a POST with application/x-www-form-urlencoded body.
// clientID is set as the header_clientid header required by the Roborock API.
func (c *CloudClient) postForm(ctx context.Context, path string, form url.Values, out any, clientID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if clientID != "" {
		req.Header.Set("header_clientid", clientID)
	}

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
