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
	"os"
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

	rruid    string
	token    string
	authCID  string // header_clientid for authenticated requests: base64(md5(rruid))
}

// clientIDFor computes the header_clientid value for a given identifier.
// header_clientid = base64(md5(identifier))
func clientIDFor(identifier string) string {
	h := md5.Sum([]byte(identifier))
	return base64.StdEncoding.EncodeToString(h[:])
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
		clientID: clientIDFor(username),
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
	// Capture raw body for debug logging.
	body, err := c.postFormRaw(ctx, "/api/v1/loginWithCode", url.Values{
		"username":       {c.username},
		"verifycode":     {strings.TrimSpace(code)},
		"verifycodetype": {"AUTH_EMAIL_CODE"},
	}, c.clientID)
	if err != nil {
		return err
	}

	// Debug: print raw response when ROBOROCK_DEBUG=1
	if os.Getenv("ROBOROCK_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[roborock debug] loginWithCode raw response: %s\n", body)
	}

	var resp struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
		Msg  string          `json:"msg"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse login response: %w (body: %.300s)", err, body)
	}
	if resp.Code != 200 {
		return fmt.Errorf("login error %d: %s", resp.Code, resp.Msg)
	}

	// Parse the data object — field names vary across API versions.
	// Try the most common names first.
	var data struct {
		// Common field names seen in the wild
		Token  string `json:"token"`
		RRuid  string `json:"rruid"`
		Rruid  string `json:"Rruid"` // some versions capitalise
		UID    int    `json:"uid"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return fmt.Errorf("parse login data: %w (data: %.300s)", err, resp.Data)
	}

	// Always print parsed fields so operator can verify.
	fmt.Fprintf(os.Stderr, "[roborock] login data: token=%q rruid=%q uid=%d\n",
		data.Token, firstNonEmptyStr(data.RRuid, data.Rruid), data.UID)

	if data.Token == "" {
		return fmt.Errorf("login succeeded (code 200) but no token in response data: %s", resp.Data)
	}

	c.token = data.Token
	c.rruid = firstNonEmptyStr(data.RRuid, data.Rruid)

	// After login the header_clientid switches to base64(md5(rruid)).
	if c.rruid != "" {
		c.authCID = clientIDFor(c.rruid)
	} else {
		c.authCID = c.clientID // fallback to email-based CID
	}
	return nil
}

// Devices fetches all devices from the account's home and returns the list.
func (c *CloudClient) Devices(ctx context.Context) ([]CloudDevice, error) {
	if c.token == "" {
		return nil, fmt.Errorf("not authenticated — call LoginWithCode first")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/getHomeDetail", nil)
	if err != nil {
		return nil, err
	}
	// Authorization uses "Rriot {token}" scheme (not Basic).
	req.Header.Set("Authorization", "Rriot "+c.token)
	req.Header.Set("header_clientid", c.authCID)
	req.Header.Set("Accept", "application/json")

	if os.Getenv("ROBOROCK_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[roborock debug] getHomeDetail headers: Authorization=Rriot %s header_clientid=%s\n",
			c.token[:min(8, len(c.token))]+"...", c.authCID)
	}

	httpResp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}

	if os.Getenv("ROBOROCK_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[roborock debug] getHomeDetail raw response: %s\n", body)
	}

	// The response structure varies; try both flat and nested device lists.
	var flat struct {
		Code int    `json:"code"`
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
		return nil, fmt.Errorf("getHomeDetail error %d: %s (body: %.300s)", flat.Code, flat.Msg, body)
	}

	devices := append(flat.Data.Devices, flat.Data.ReceivedDevices...)
	return devices, nil
}

// postForm sends a POST with application/x-www-form-urlencoded body and decodes JSON into out.
func (c *CloudClient) postForm(ctx context.Context, path string, form url.Values, out any, clientID string) error {
	body, err := c.postFormRaw(ctx, path, form, clientID)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// postFormRaw sends a POST and returns the raw response body.
func (c *CloudClient) postFormRaw(ctx context.Context, path string, form url.Values, clientID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if clientID != "" {
		req.Header.Set("header_clientid", clientID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
