// Package xiaomi implements a minimal Xiaomi cloud API client for
// fetching Roborock device tokens. It is used only by the fetch-tokens
// command — not by the main server (which uses local miIO).
package xiaomi

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	serviceURL = "https://account.xiaomi.com/pass/serviceLogin"
	authURL    = "https://account.xiaomi.com/pass/serviceLoginAuth2"
	// Country-specific device list URL. %s = country code (de, us, sg, cn, ...).
	// EU users typically use "de".
	deviceListURL = "https://%sapi.io.mi.com/app/home/device_list"
)

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

// NewCloudClient creates a client for the given country (e.g. "de" for EU).
func NewCloudClient(email, password, country string) (*CloudClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &CloudClient{
		email:   email,
		password: password,
		country: country,
		jar:     jar,
		http:    &http.Client{Jar: jar, Timeout: 30 * time.Second},
	}, nil
}

// Login authenticates with the Xiaomi account service.
func (c *CloudClient) Login(ctx context.Context) error {
	// Step 1: GET the login page to retrieve the _sign parameter.
	sign, err := c.fetchSign(ctx)
	if err != nil {
		return fmt.Errorf("fetch sign: %w", err)
	}

	// Step 2: POST credentials to get ssecurity + userId + location.
	location, err := c.postCredentials(ctx, sign)
	if err != nil {
		return fmt.Errorf("post credentials: %w", err)
	}

	// Step 3: Follow the location URL to get the serviceToken cookie.
	if err := c.fetchServiceToken(ctx, location); err != nil {
		return fmt.Errorf("fetch service token: %w", err)
	}

	return nil
}

// fetchSign GETs the serviceLogin page and extracts the _sign field.
func (c *CloudClient) fetchSign(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serviceURL, nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	q.Set("sid", "xiaomiio")
	q.Set("_json", "true")
	req.URL.RawQuery = q.Encode()
	c.setCommonHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// Response is prefixed with "&&&START&&&" — strip it.
	body = stripPrefix(body)

	var result struct {
		Sign string `json:"_sign"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse sign response: %w", err)
	}
	return result.Sign, nil
}

// postCredentials submits email + hashed password and returns the location URL.
func (c *CloudClient) postCredentials(ctx context.Context, sign string) (string, error) {
	// Password is transmitted as uppercase MD5 hash.
	h := md5.Sum([]byte(c.password))
	hash := strings.ToUpper(fmt.Sprintf("%x", h))

	form := url.Values{}
	form.Set("_json", "true")
	form.Set("user", c.email)
	form.Set("hash", hash)
	form.Set("_sign", sign)
	form.Set("sid", "xiaomiio")
	form.Set("serviceParam", `{"checkSafePhone":false}`)

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
	nonce := genNonce()
	signedNonce := signNonce(c.ssecurity, nonce)

	data := `{"getVirtualModel":true,"getHuamiDevices":0}`

	// Build the form fields, then sign and encrypt them.
	fields := map[string]string{
		"data": data,
	}
	// rc4_hash__ is the signature of the unencrypted fields.
	fields["rc4_hash__"] = signData("/home/device_list", fields, signedNonce)

	// Encrypt each field value with RC4.
	encrypted := make(map[string]string, len(fields))
	for k, v := range fields {
		enc, err := encryptRC4(signedNonce, v)
		if err != nil {
			return nil, fmt.Errorf("encrypt field %s: %w", k, err)
		}
		encrypted[k] = enc
	}

	// Add the outer signature (of the encrypted values).
	encrypted["signature"] = signData("/home/device_list", encrypted, signedNonce)
	encrypted["ssecurity"] = c.ssecurity
	encrypted["_nonce"] = nonce

	form := url.Values{}
	for k, v := range encrypted {
		form.Set(k, v)
	}

	apiURL := fmt.Sprintf(deviceListURL, c.country)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

	// Decrypt the response body with RC4.
	decrypted, err := decryptRC4(signedNonce, string(body))
	if err != nil {
		// Response may be plaintext on some regions/versions — try parsing directly.
		decrypted = string(body)
	}

	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Result  struct {
			List []Device `json:"list"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(decrypted), &result); err != nil {
		return nil, fmt.Errorf("parse device list: %w (body: %.200s)", err, decrypted)
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("device list error %d: %s", result.Code, result.Message)
	}

	return result.Result.List, nil
}

// --- Crypto helpers ---

// genNonce generates a miCloud nonce: base64(4 random bytes + 4-byte big-endian minute-timestamp).
func genNonce() string {
	var b [8]byte
	rand.Read(b[:4])
	binary.BigEndian.PutUint32(b[4:], uint32(time.Now().UnixMilli()/60000))
	return base64.StdEncoding.EncodeToString(b[:])
}

// signNonce computes base64(SHA-256(base64-decoded(ssecurity) + base64-decoded(nonce))).
func signNonce(ssecurity, nonce string) string {
	sec, _ := base64.StdEncoding.DecodeString(ssecurity)
	n, _ := base64.StdEncoding.DecodeString(nonce)
	h := sha256.Sum256(append(sec, n...))
	return base64.StdEncoding.EncodeToString(h[:])
}

// signData computes an HMAC-SHA256 signature over sorted key=value pairs.
func signData(uri string, params map[string]string, signedNonce string) string {
	// Sort keys for deterministic output.
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := []string{uri}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, params[k]))
	}
	parts = append(parts, signedNonce)
	msg := strings.Join(parts, "&")

	key, _ := base64.StdEncoding.DecodeString(signedNonce)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
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
