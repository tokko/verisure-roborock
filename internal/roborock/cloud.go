// Package roborock/cloud.go implements a minimal Roborock cloud client
// used exclusively by cmd/fetch-tokens to retrieve local device tokens.
// Day-to-day vacuum control uses the local miIO protocol, not this.
package roborock

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Regional login endpoints — only used for email OTP login.
var roborockLoginEndpoints = map[string]string{
	"eu": "https://euiot.roborock.com",
	"us": "https://usiot.roborock.com",
	"cn": "https://iot.roborock.com",
	"sg": "https://sgiot.roborock.com",
}

// CloudDevice is a device returned by the Roborock cloud.
type CloudDevice struct {
	DUID      string `json:"duid"`
	Name      string `json:"name"`
	LocalKey  string `json:"localKey"` // 16-char — the miIO token
	ProductID string `json:"productId"`
	Online    bool   `json:"online"`
	// NetworkInfo: present on some firmware/API versions.
	NetworkInfo *struct {
		LocalIP string `json:"localIp"`
	} `json:"networkInfo,omitempty"`
	// DeviceStatus: v3 API returns IP inside status[101][81].ipAdress
	DeviceStatus json.RawMessage `json:"deviceStatus,omitempty"`
}

// LocalIP returns the device's LAN IP if known, or empty string.
func (d *CloudDevice) LocalIP() string {
	if d.NetworkInfo != nil && d.NetworkInfo.LocalIP != "" {
		return d.NetworkInfo.LocalIP
	}
	// v3 API: deviceStatus can be a map where key "101" holds a nested object
	// with key "81" containing {"ipAdress": "x.x.x.x"}.
	if d.DeviceStatus == nil {
		return ""
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(d.DeviceStatus, &outer); err != nil {
		return ""
	}
	raw101, ok := outer["101"]
	if !ok {
		return ""
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw101, &inner); err != nil {
		return ""
	}
	raw81, ok := inner["81"]
	if !ok {
		return ""
	}
	var wifi struct {
		IP string `json:"ipAdress"` // sic — Roborock typo in the API
	}
	if err := json.Unmarshal(raw81, &wifi); err != nil {
		return ""
	}
	return wifi.IP
}

// rriotCreds holds the v2 IoT credentials from the login response.
type rriotCreds struct {
	U string `json:"u"` // username for Hawk auth
	S string `json:"s"` // password for Hawk auth
	H string `json:"h"` // HMAC key
	K string `json:"k"`
	R struct {
		R string `json:"r"` // region code
		A string `json:"a"` // API base URL (e.g. https://api-eu.roborock.com)
	} `json:"r"`
}

// CloudClient authenticates with the Roborock cloud and fetches device tokens.
type CloudClient struct {
	loginBaseURL string
	username     string
	clientID     string // header_clientid: base64(md5(username))
	http         *http.Client

	// Populated after LoginWithCode.
	token   string
	rruid   string
	rriot   rriotCreds
	authCID string // header_clientid for authenticated calls: base64(md5(rruid))
}

// clientIDFor computes base64(md5(s)).
func clientIDFor(s string) string {
	h := md5.Sum([]byte(s))
	return base64.StdEncoding.EncodeToString(h[:])
}

// NewCloudClient creates a Roborock cloud client.
// region: "eu" (default), "us", "cn", "sg", or any EU country code.
func NewCloudClient(username, region string) (*CloudClient, error) {
	if region == "" {
		region = "eu"
	}
	base, ok := roborockLoginEndpoints[region]
	if !ok {
		base = roborockLoginEndpoints["eu"] // country codes → EU
	}
	return &CloudClient{
		loginBaseURL: base,
		username:     username,
		clientID:     clientIDFor(username),
		http:         &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// RequestEmailCode triggers a one-time code to be sent to the user's email.
func (c *CloudClient) RequestEmailCode(ctx context.Context) (string, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := c.postForm(ctx, c.loginBaseURL, "/api/v1/sendEmailCode", url.Values{
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

// LoginWithCode authenticates using the OTP received by email.
func (c *CloudClient) LoginWithCode(ctx context.Context, code string) error {
	body, err := c.postFormRaw(ctx, c.loginBaseURL, "/api/v1/loginWithCode", url.Values{
		"username":       {c.username},
		"verifycode":     {strings.TrimSpace(code)},
		"verifycodetype": {"AUTH_EMAIL_CODE"},
	}, c.clientID)
	if err != nil {
		return err
	}

	if os.Getenv("ROBOROCK_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[roborock debug] loginWithCode: %s\n", body)
	}

	var resp struct {
		Code int `json:"code"`
		Data struct {
			Token string     `json:"token"`
			RRuid string     `json:"rruid"`
			Rriot rriotCreds `json:"rriot"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse login response: %w", err)
	}
	if resp.Code != 200 {
		return fmt.Errorf("login error %d: %s", resp.Code, resp.Msg)
	}
	if resp.Data.Token == "" {
		return fmt.Errorf("login succeeded but no token in response")
	}

	c.token = resp.Data.Token
	c.rruid = resp.Data.RRuid
	c.rriot = resp.Data.Rriot
	c.authCID = clientIDFor(c.rruid)

	fmt.Fprintf(os.Stderr, "[roborock] logged in: rruid=%s api=%s\n", c.rruid, c.rriot.R.A)
	return nil
}

// Devices fetches all devices from the account.
// It first gets the home ID from the old endpoint, then fetches device details from the new API.
func (c *CloudClient) Devices(ctx context.Context) ([]CloudDevice, error) {
	if c.token == "" {
		return nil, fmt.Errorf("not authenticated — call LoginWithCode first")
	}

	// Step 1: get home ID from old iot endpoint.
	// Authorization = raw token value (no scheme prefix).
	homeID, err := c.getHomeID(ctx)
	if err != nil {
		return nil, fmt.Errorf("getHomeDetail: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[roborock] home id: %d\n", homeID)

	// Step 2: fetch devices from new API endpoint using Hawk auth.
	return c.getHomeDevices(ctx, homeID)
}

// getHomeID calls GET /api/v1/getHomeDetail on the old iot endpoint and returns rrHomeId.
func (c *CloudClient) getHomeID(ctx context.Context) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.loginBaseURL+"/api/v1/getHomeDetail", nil)
	if err != nil {
		return 0, err
	}
	// Authorization is the raw token — no "Rriot" or "Bearer" prefix.
	req.Header.Set("Authorization", c.token)
	req.Header.Set("header_clientid", c.authCID)
	req.Header.Set("Accept", "application/json")

	if os.Getenv("ROBOROCK_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[roborock debug] getHomeDetail: GET %s/api/v1/getHomeDetail\n", c.loginBaseURL)
		fmt.Fprintf(os.Stderr, "  Authorization: %s...\n", c.token[:min(20, len(c.token))])
	}

	body, err := c.doRequest(req)
	if err != nil {
		return 0, err
	}
	if os.Getenv("ROBOROCK_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[roborock debug] getHomeDetail response: %s\n", body)
	}

	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			RRHomeID int64 `json:"rrHomeId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("parse response: %w (body: %.300s)", err, body)
	}
	if resp.Code != 200 {
		return 0, fmt.Errorf("error %d: %s (body: %.300s)", resp.Code, resp.Msg, body)
	}
	return resp.Data.RRHomeID, nil
}

// getHomeDevices calls GET /v3/user/homes/{id} on the new API endpoint.
func (c *CloudClient) getHomeDevices(ctx context.Context, homeID int64) ([]CloudDevice, error) {
	apiBase := c.rriot.R.A
	if apiBase == "" {
		return nil, fmt.Errorf("rriot.r.a is empty — cannot determine new API endpoint")
	}

	urlPath := fmt.Sprintf("/v3/user/homes/%d", homeID)
	auth := c.hawkAuth(urlPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+urlPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")

	if os.Getenv("ROBOROCK_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[roborock debug] getHomeDevices: GET %s%s\n", apiBase, urlPath)
		fmt.Fprintf(os.Stderr, "  Authorization: %s\n", auth)
	}

	body, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	if os.Getenv("ROBOROCK_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[roborock debug] getHomeDevices response: %s\n", body)
	}

	// Parse v3 home response: {"success": true, "result": {...}}
	// Devices may be nested under result.devices or result.rooms[].devices etc.
	var homeResp struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Code    int             `json:"code"`
		Msg     string          `json:"msg"`
	}
	if err := json.Unmarshal(body, &homeResp); err != nil {
		return nil, fmt.Errorf("parse home response: %w (body: %.300s)", err, body)
	}
	if !homeResp.Success {
		return nil, fmt.Errorf("getHomeDevices error %d: %s (body: %.300s)", homeResp.Code, homeResp.Msg, body)
	}

	// Extract devices from various possible locations in the result.
	var result struct {
		Devices         []CloudDevice `json:"devices"`
		ReceivedDevices []CloudDevice `json:"receivedDevices"`
		Rooms           []struct {
			Devices []CloudDevice `json:"devices"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal(homeResp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse result: %w (result: %.300s)", err, homeResp.Result)
	}

	devices := append(result.Devices, result.ReceivedDevices...)
	for _, room := range result.Rooms {
		devices = append(devices, room.Devices...)
	}
	return devices, nil
}

// hawkAuth builds the Hawk Authorization header for the v3 API.
// Format (from python-roborock web_api.py):
//
//	prestr = u:s:nonce:timestamp:md5(urlPath)::
//	mac    = base64(HMAC-SHA256(h, prestr))
//	header = Hawk id="u",s="s",ts="ts",nonce="nonce",mac="mac"
func (c *CloudClient) hawkAuth(urlPath string) string {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	nonce := tokenURLSafe(6) // 6 random bytes → ~8 base64 chars

	urlMD5 := md5Hex(urlPath)
	// params and formdata are both empty for simple GETs.
	prestr := strings.Join([]string{c.rriot.U, c.rriot.S, nonce, ts, urlMD5, "", ""}, ":")

	h := hmac.New(sha256.New, []byte(c.rriot.H))
	h.Write([]byte(prestr))
	mac := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return fmt.Sprintf(`Hawk id="%s",s="%s",ts="%s",nonce="%s",mac="%s"`,
		c.rriot.U, c.rriot.S, ts, nonce, mac)
}

// doRequest executes an HTTP request and returns the response body.
func (c *CloudClient) doRequest(req *http.Request) ([]byte, error) {
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

// postForm sends a form-encoded POST and decodes JSON into out.
func (c *CloudClient) postForm(ctx context.Context, base, path string, form url.Values, out any, clientID string) error {
	body, err := c.postFormRaw(ctx, base, path, form, clientID)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// postFormRaw sends a form-encoded POST and returns the raw response body.
func (c *CloudClient) postFormRaw(ctx context.Context, base, path string, form url.Values, clientID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if clientID != "" {
		req.Header.Set("header_clientid", clientID)
	}
	return c.doRequest(req)
}

// --- helpers ---

func md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}

// tokenURLSafe returns n random bytes encoded as URL-safe base64 (like Python's secrets.token_urlsafe).
func tokenURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fallback to math/rand if crypto/rand fails (should never happen).
		for i := range b {
			n, _ := rand.Int(rand.Reader, big.NewInt(256))
			b[i] = byte(n.Int64())
		}
	}
	return base64.URLEncoding.EncodeToString(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
