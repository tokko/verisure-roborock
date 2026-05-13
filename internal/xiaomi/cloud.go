// Package xiaomi implements a minimal Xiaomi cloud API client for fetching
// Roborock device tokens and sending cloud RPC commands.
package xiaomi

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	serviceURL = "https://account.xiaomi.com/pass/serviceLogin"
	authURL    = "https://account.xiaomi.com/pass/serviceLoginAuth2"
)

type loginInfo struct {
	Sign         string
	QS           string
	Callback     string
	ServiceParam string
}

// Auth holds the reusable Xiaomi Cloud credentials needed for signed API calls.
type Auth struct {
	UserID       string    `json:"user_id"`
	SSecurity    string    `json:"ssecurity"`
	ServiceToken string    `json:"service_token"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

func (a Auth) Valid() bool {
	return a.UserID != "" && a.SSecurity != "" && a.ServiceToken != ""
}

// Device holds the relevant fields for a discovered Xiaomi device.
type Device struct {
	DID     string `json:"did"`
	Name    string `json:"name"`
	Model   string `json:"model"`
	LocalIP string `json:"localip"`
	Token   string `json:"token"` // 32-char hex after decryption
	MAC     string `json:"mac"`
}

// IsRoborock reports whether the device is a Roborock vacuum.
func (d Device) IsRoborock() bool {
	return strings.HasPrefix(d.Model, "roborock.")
}

// CloudClient authenticates with the Xiaomi cloud and fetches device tokens.
type CloudClient struct {
	email    string
	password string
	country  string

	ssecurity    string
	userId       string
	serviceToken string

	http *http.Client
	jar  *cookiejar.Jar
}

// RPC sends a miIO JSON-RPC command through Xiaomi Cloud's /home/rpc/{did}
// endpoint and returns the JSON-RPC result field.
func (c *CloudClient) RPC(ctx context.Context, did, method string, id uint32, params any) (json.RawMessage, error) {
	if c.serviceToken == "" || c.ssecurity == "" || c.userId == "" {
		return nil, fmt.Errorf("not authenticated - call Login first")
	}
	raw, err := c.request(ctx, "/home/rpc/"+did, map[string]any{
		"id":     id,
		"method": method,
		"params": params,
	})
	if err != nil {
		return nil, err
	}

	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("parse rpc envelope: %w (body: %.200s)", err, raw)
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("rpc error %d: %s", envelope.Code, envelope.Message)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return nil, fmt.Errorf("rpc response missing result")
	}

	result := envelope.Result
	var quoted string
	if err := json.Unmarshal(envelope.Result, &quoted); err == nil {
		result = []byte(quoted)
	}

	var rpc struct {
		ID     uint32          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(result, &rpc); err == nil && (len(rpc.Result) > 0 || rpc.Error != nil) {
		if rpc.Error != nil {
			return nil, fmt.Errorf("device rpc error %d: %s", rpc.Error.Code, rpc.Error.Message)
		}
		return rpc.Result, nil
	}

	return result, nil
}

func (c *CloudClient) SetAuth(auth Auth) error {
	if !auth.Valid() {
		return fmt.Errorf("xiaomi auth is incomplete")
	}
	c.userId = auth.UserID
	c.ssecurity = auth.SSecurity
	c.serviceToken = auth.ServiceToken
	return nil
}

func (c *CloudClient) Auth() Auth {
	return Auth{
		UserID:       c.userId,
		SSecurity:    c.ssecurity,
		ServiceToken: c.serviceToken,
		UpdatedAt:    time.Now().UTC(),
	}
}

func LoadAuth(path string) (Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Auth{}, err
	}
	var auth Auth
	if err := json.Unmarshal(data, &auth); err != nil {
		return Auth{}, err
	}
	if !auth.Valid() {
		return Auth{}, fmt.Errorf("xiaomi auth file %s is incomplete", path)
	}
	return auth, nil
}

func SaveAuth(path string, auth Auth) error {
	if !auth.Valid() {
		return fmt.Errorf("xiaomi auth is incomplete")
	}
	auth.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NewCloudClient creates a client for the given country (e.g. "de" for EU).
func NewCloudClient(email, password, country string) (*CloudClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &CloudClient{
		email:    email,
		password: password,
		country:  country,
		jar:      jar,
		http:     &http.Client{Jar: jar, Timeout: 30 * time.Second},
	}, nil
}

// Login authenticates with the Xiaomi account service.
func (c *CloudClient) Login(ctx context.Context) error {
	// Step 1: GET the login page to retrieve the _sign parameter.
	info, err := c.fetchLoginInfo(ctx)
	if err != nil {
		return fmt.Errorf("fetch login info: %w", err)
	}

	// Step 2: POST credentials to get ssecurity + userId + location.
	location, err := c.postCredentials(ctx, info)
	if err != nil {
		return fmt.Errorf("post credentials: %w", err)
	}

	// Step 3: Follow the location URL to get the serviceToken cookie.
	if err := c.fetchServiceToken(ctx, location); err != nil {
		return fmt.Errorf("fetch service token: %w", err)
	}

	return nil
}

// fetchLoginInfo GETs the serviceLogin page and extracts fields required by serviceLoginAuth2.
func (c *CloudClient) fetchLoginInfo(ctx context.Context) (loginInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serviceURL, nil)
	if err != nil {
		return loginInfo{}, err
	}
	q := req.URL.Query()
	q.Set("sid", "xiaomiio")
	q.Set("_json", "true")
	q.Set("callback", "https://sts.api.io.mi.com/sts")
	req.URL.RawQuery = q.Encode()
	c.setCommonHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return loginInfo{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return loginInfo{}, err
	}
	// Response is prefixed with "&&&START&&&" — strip it.
	body = stripPrefix(body)

	var result struct {
		Sign         string `json:"_sign"`
		QS           string `json:"qs"`
		Callback     string `json:"callback"`
		ServiceParam string `json:"serviceParam"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return loginInfo{}, fmt.Errorf("parse sign response: %w", err)
	}
	if result.Sign == "" {
		return loginInfo{}, fmt.Errorf("login response missing _sign")
	}
	return loginInfo{
		Sign:         result.Sign,
		QS:           firstNonEmpty(result.QS, "?sid=xiaomiio&_json=true"),
		Callback:     firstNonEmpty(result.Callback, "https://sts.api.io.mi.com/sts"),
		ServiceParam: firstNonEmpty(result.ServiceParam, `{"checkSafePhone":false}`),
	}, nil
}

// postCredentials submits email + hashed password and returns the location URL.
func (c *CloudClient) postCredentials(ctx context.Context, info loginInfo) (string, error) {
	// Password is transmitted as uppercase MD5 hash.
	h := md5.Sum([]byte(c.password))
	hash := strings.ToUpper(fmt.Sprintf("%x", h))

	form := url.Values{}
	form.Set("_json", "true")
	form.Set("user", c.email)
	form.Set("hash", hash)
	form.Set("_sign", info.Sign)
	form.Set("sid", "xiaomiio")
	form.Set("qs", info.QS)
	form.Set("callback", info.Callback)
	form.Set("serviceParam", info.ServiceParam)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.URL.RawQuery = "sid=xiaomiio"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.setCommonHeaders(req)

	// Don't follow redirects — we need the location from the response body.
	client := *c.http
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	body = stripPrefix(body)

	var result struct {
		Code        int    `json:"code"`
		Description string `json:"description"`
		SSecurity   string `json:"ssecurity"`
		UserID      string `json:"userId"`
		Location    string `json:"location"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse auth response: %w (body: %s)", err, body)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("login failed (code %d): %s", result.Code, result.Description)
	}

	c.ssecurity = result.SSecurity
	c.userId = result.UserID
	return result.Location, nil
}

// fetchServiceToken follows the location URL to obtain the serviceToken cookie.
func (c *CloudClient) fetchServiceToken(ctx context.Context, location string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	// Extract serviceToken from cookie jar.
	u, _ := url.Parse(location)
	for _, cookie := range c.jar.Cookies(u) {
		if cookie.Name == "serviceToken" {
			c.serviceToken = cookie.Value
			return nil
		}
	}
	return fmt.Errorf("serviceToken cookie not found after following location")
}

// Devices fetches the full device list and returns Roborock devices with decrypted tokens.
func (c *CloudClient) Devices(ctx context.Context) ([]Device, error) {
	raw, err := c.request(ctx, "/home/device_list", map[string]any{
		"getVirtualModel": true,
		"getHuamiDevices": 0,
	})
	if err != nil {
		return nil, err
	}
	return parseDeviceList(raw)
}

func (c *CloudClient) request(ctx context.Context, path string, data any) (json.RawMessage, error) {
	nonce := genNonce()
	signedNonce := signNonce(c.ssecurity, nonce)

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	// Build the form fields, then sign and encrypt them.
	fields := map[string]string{
		"data": string(payload),
	}
	// rc4_hash__ is the signature of the unencrypted fields.
	fields["rc4_hash__"] = signData(path, fields, signedNonce)

	// Encrypt each field value with RC4.
	encrypted := make(map[string]string, len(fields)+3)
	for k, v := range fields {
		enc, err := encryptRC4(signedNonce, v)
		if err != nil {
			return nil, fmt.Errorf("encrypt field %s: %w", k, err)
		}
		encrypted[k] = enc
	}

	// Add the outer signature (of the encrypted values).
	encrypted["signature"] = signData(path, encrypted, signedNonce)
	encrypted["ssecurity"] = c.ssecurity
	encrypted["_nonce"] = nonce

	form := url.Values{}
	for k, v := range encrypted {
		form.Set(k, v)
	}

	apiURL := fmt.Sprintf("https://%s/app%s", apiHost(c.country), path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("MIOT-ENCRYPT-ALGORITHM", "ENCRYPT-RC4")
	req.Header.Set("x-xiaomi-protocal-flag-cli", "PROTOCAL-HTTP2")
	req.Header.Set("mishop-client-id", "180100041079")
	req.Header.Set("User-Agent", "APP/com.xiaomi.mihome APPV/6.0.103")
	c.setCommonHeaders(req)

	// Set auth cookies manually.
	req.AddCookie(&http.Cookie{Name: "userId", Value: c.userId})
	req.AddCookie(&http.Cookie{Name: "serviceToken", Value: c.serviceToken})
	req.AddCookie(&http.Cookie{Name: "yetAnotherServiceToken", Value: c.serviceToken})

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

	// Decrypt the response body with RC4.
	decrypted, err := decryptRC4(signedNonce, string(body))
	if err != nil {
		// Response may be plaintext on some regions/versions — try parsing directly.
		decrypted = string(body)
	}

	return json.RawMessage(decrypted), nil
}

func parseDeviceList(raw json.RawMessage) ([]Device, error) {
	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Result  struct {
			List []Device `json:"list"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse device list: %w (body: %.200s)", err, raw)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("device list error %d: %s", result.Code, result.Message)
	}
	return result.Result.List, nil
}

// --- Crypto helpers ---

// genNonce generates a miCloud nonce: base64(8 random bytes + 4-byte big-endian minute-timestamp).
func genNonce() string {
	var b [12]byte
	rand.Read(b[:8])
	binary.BigEndian.PutUint32(b[8:], uint32(time.Now().UnixMilli()/60000))
	return base64.StdEncoding.EncodeToString(b[:])
}

// signNonce computes base64(SHA-256(base64-decoded(ssecurity) + base64-decoded(nonce))).
func signNonce(ssecurity, nonce string) string {
	sec, _ := base64.StdEncoding.DecodeString(ssecurity)
	n, _ := base64.StdEncoding.DecodeString(nonce)
	h := sha256.Sum256(append(sec, n...))
	return base64.StdEncoding.EncodeToString(h[:])
}

// signData computes Xiaomi's RC4 request signature over sorted key=value pairs.
func signData(uri string, params map[string]string, signedNonce string) string {
	// Sort keys for deterministic output.
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := []string{"POST", uri}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, params[k]))
	}
	parts = append(parts, signedNonce)
	msg := strings.Join(parts, "&")

	sum := sha1.Sum([]byte(msg))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// encryptRC4 encrypts a payload string with RC4, skipping the first 1024 bytes of keystream.
func encryptRC4(signedNonce, payload string) (string, error) {
	key, _ := base64.StdEncoding.DecodeString(signedNonce)
	c, err := rc4.NewCipher(key)
	if err != nil {
		return "", err
	}
	// Discard first 1024 bytes of keystream (matches python-miio behaviour).
	skip := make([]byte, 1024)
	c.XORKeyStream(skip, skip)

	plaintext := []byte(payload)
	ciphertext := make([]byte, len(plaintext))
	c.XORKeyStream(ciphertext, plaintext)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decryptRC4 decrypts a base64-encoded RC4 ciphertext.
func decryptRC4(signedNonce, b64ciphertext string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(b64ciphertext)
	if err != nil {
		return "", err
	}
	key, _ := base64.StdEncoding.DecodeString(signedNonce)
	c, err := rc4.NewCipher(key)
	if err != nil {
		return "", err
	}
	skip := make([]byte, 1024)
	c.XORKeyStream(skip, skip)

	plaintext := make([]byte, len(ciphertext))
	c.XORKeyStream(plaintext, ciphertext)
	return string(plaintext), nil
}

// stripPrefix removes the "&&&START&&&" prefix that Xiaomi prepends to JSON responses.
func stripPrefix(b []byte) []byte {
	const prefix = "&&&START&&&"
	if strings.HasPrefix(string(b), prefix) {
		return b[len(prefix):]
	}
	return b
}

func (c *CloudClient) setCommonHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "APP/com.xiaomi.mihome APPV/6.0.103 iosPassportSDK/3.9.0 iOS/14.4 miHSTS")
	req.Header.Set("Accept", "application/json")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func apiHost(country string) string {
	if country == "" || country == "cn" {
		return "api.io.mi.com"
	}
	return country + ".api.io.mi.com"
}
