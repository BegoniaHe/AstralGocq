package gocq

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BegoniaHe/AstralGocq/server"
	"github.com/ProtocolScience/AstralGo/client"
	"github.com/ProtocolScience/AstralGo/utils"
	"github.com/RomiChan/websocket"
	"github.com/google/uuid"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/BegoniaHe/AstralGocq/internal/base"
	"github.com/BegoniaHe/AstralGocq/internal/download"
	"github.com/BegoniaHe/AstralGocq/modules/config"
)

// SignServerManager manages the current sign server and handles server selection.
type SignServerManager struct {
	current  atomic.Pointer[config.SignServer]
	errCount atomic.Uintptr
	client   *SignClient
}

// NewSignServerManager creates a new SignServerManager instance.
func NewSignServerManager(client *SignClient) *SignServerManager {
	return &SignServerManager{client: client}
}

// Get returns the current sign server.
func (m *SignServerManager) Get() *config.SignServer {
	if len(base.SignServers) == 1 {
		return &base.SignServers[0]
	}
	return m.current.Load()
}

// Set updates the current sign server.
func (m *SignServerManager) Set(server *config.SignServer) {
	if server == nil {
		cur := m.current.Load()
		if cur != nil && len(cur.URL) > 0 {
			log.Warnf("Current server %v unavailable, searching for a new one", cur.URL)
		}
	}
	m.current.Store(server)
}

// IncrementErrorCount increases the error count by one.
func (m *SignServerManager) IncrementErrorCount() {
	m.errCount.Add(1)
}

// HasOver checks if the error count exceeds the specified value.
func (m *SignServerManager) HasOver(count uintptr) bool {
	return m.errCount.Load() > count
}

// GetAvailableSignServer retrieves an available sign server or returns an error if none are available.
func (m *SignServerManager) GetAvailableSignServer() (*config.SignServer, error) {
	s := m.Get()
	if s != nil {
		return s, nil
	}
	if len(base.SignServers) == 0 {
		return nil, errors.New("no sign server configured")
	}
	maxCount := base.Account.MaxCheckCount
	if maxCount == 0 {
		if m.HasOver(3) {
			log.Warn("Failed to get a sign server 3 times, using main server")
			m.Set(&base.SignServers[0])
			return m.Get(), nil
		}
	} else if m.HasOver(uintptr(maxCount)) {
		log.Fatalf("Failed to get a sign server more than %v times, going offline", maxCount)
	}
	s = m.asyncCheckServers(base.SignServers)
	if s == nil {
		return nil, errors.New("no usable sign server")
	}
	return s, nil
}

func (m *SignServerManager) asyncCheckServers(servers []config.SignServer) *config.SignServer {
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		success     bool
		foundServer *config.SignServer
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i, s := range servers {
		wg.Add(1)
		go func(i int, server config.SignServer) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			default:
				if isServerAvailable(server.URL) {
					mu.Lock()
					if !success {
						log.Infof("Checking server: %v (%v/%v) ok!", server.URL, i+1, len(servers))
						m.Set(&server)
						if m.client.signRegister() {
							success = true
							foundServer = &server
							log.Infof("Using server url=%v, key=%v, auth=%v", server.URL, server.Key, server.Authorization)
							cancel()
						}
					}
					mu.Unlock()
				} else {
					log.Warnf("Checking server: %v (%v/%v) failed!", server.URL, i+1, len(servers))
				}
			}
		}(i, s)
	}

	wg.Wait()
	return foundServer
}

func isServerAvailable(signServer string) bool {
	parsedURL, err := url.Parse(signServer)
	if err != nil {
		log.Warnf("Invalid URL: %v, error: %v", signServer, err)
		return false
	}
	switch parsedURL.Scheme {
	case "http", "https":
		return isHTTPAvailable(signServer)
	case "ws", "wss":
		return isWebSocketAvailable(signServer)
	default:
		log.Errorf("Unsupported protocol: %v", parsedURL.Scheme)
		return false
	}
}

func isHTTPAvailable(url string) bool {
	httpClient := http.Client{
		Timeout: 3 * time.Second,
	}

	resp, err := httpClient.Get(url)
	if err != nil {
		log.Warnf("HTTP check failed for %v, error: %v", url, err)
		return false
	}
	bodyBytes, e := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if e == nil && gjson.GetBytes(bodyBytes, "code").Int() == 0 {
			return true
		}
	}
	return false
}

func isWebSocketAvailable(url string) bool {
	dialer := websocket.Dialer{
		HandshakeTimeout: 3 * time.Second,
	}

	conn, resp, err := dialer.Dial(url, nil)
	if err != nil {
		log.Warnf("WebSocket check failed for %v, error: %v", url, err)
		return false
	}
	_ = resp.Body.Close()
	_ = conn.Close()

	// Optionally, you can send a ping or some message to verify further

	return true
}

// SignClient handles requests to the sign server.
type SignClient struct {
	client    *client.QQClient
	manager   *SignServerManager
	ws        *websocket.Conn
	requests  map[string]chan map[string]interface{}
	requestMu sync.Mutex
}

// NewSignClient creates a new SignClient instance.
func NewSignClient(c *client.QQClient) *SignClient {
	signClient := &SignClient{client: c, requests: make(map[string]chan map[string]interface{})}
	signClient.manager = NewSignServerManager(signClient)
	return signClient
}
func (c *SignClient) resetWebsocket() {
	c.requestMu.Lock()
	if c.ws != nil {
		_ = c.ws.Close()
		c.ws = nil // Reset connection
	}
	c.requestMu.Unlock()
}
func (c *SignClient) requestSignServer(action string, data map[string]string) (string, []byte, error) {
	i := 0
	for {
		headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
		signServer, err := c.manager.GetAvailableSignServer()
		if err != nil || signServer == nil || len(signServer.URL) == 0 {
			log.Warnf("Error getting sign server: %v, using main server", err)
			c.manager.IncrementErrorCount()
			signServer = &base.SignServers[0]
		}
		data["key"] = signServer.Key
		data["uin"] = strconv.FormatInt(base.Account.Uin, 10)

		data["qua"] = device.Protocol.Version().QUA
		data["android_id"] = utils.B2S(device.AndroidId)
		data["guid"] = hex.EncodeToString(device.Guid)
		data["qimei36"] = device.QImei36
		var resp []byte
		if strings.HasPrefix(signServer.URL, "ws://") || strings.HasPrefix(signServer.URL, "wss://") {
			a, e := c.requestWebSocket(signServer, action, data)
			err = e
			resp = a
		} else {
			//HTTP ABOVE
			urlAddress := strings.TrimSuffix(signServer.URL, "/") + "/" + strings.TrimPrefix(action, "/")
			auth := signServer.Authorization
			if auth != "-" && auth != "" {
				headers["Authorization"] = auth
			}

			var bodyBytes bytes.Buffer
			for key, value := range data {
				bodyBytes.WriteString(fmt.Sprintf("%v=%v&", key, value))
			}
			bodyString := bodyBytes.String()
			bodyString = bodyString[:len(bodyString)-1]
			body := bytes.NewBufferString(bodyString)
			//log.Infof("POST: %s \n %s", urlAddress, bodyString)
			hash := md5.New()
			hash.Write([]byte("37mWT8rCCNyT2Zi11ACT8pbhe8wCRSKG"))
			hash.Write(body.Bytes())
			headers["gocq-ticket"] = hex.EncodeToString(hash.Sum(nil))

			req := download.Request{
				Method: http.MethodPost,
				Header: headers,
				URL:    urlAddress,
				Body:   body,
			}.WithTimeout(time.Duration(base.SignServerTimeout) * time.Second)
			a, e := req.Bytes()
			if e != nil {
				c.manager.Set(nil)
			}
			err = e
			resp = a
		}
		if err == nil || i == 1 {
			return signServer.URL, resp, err
		}
		i++
	}
}

func (c *SignClient) requestWebSocket(signServer *config.SignServer, action string, data map[string]string) ([]byte, error) {
	// Generate a unique echo UUID
	echoUUID := uuid.New().String()

	// Prepare JSON message
	message := map[string]interface{}{
		"type":   action,
		"params": data,
		"echo":   echoUUID,
	}

	// Create a channel to wait for the response
	responseChan := make(chan map[string]interface{})

	// Register the request
	c.requestMu.Lock()

	if c.ws == nil {
		header := http.Header{}
		network, address := server.ResolveURI(signServer.URL)
		dialer := websocket.Dialer{
			NetDial: func(_, addr string) (net.Conn, error) {
				if network == "unix" {
					host, _, err := net.SplitHostPort(addr)
					if err != nil {
						host = addr
					}
					filepath, err := base64.RawURLEncoding.DecodeString(host)
					if err == nil {
						addr = string(filepath)
					}
				}
				return net.Dial(network, addr) // support unix socket transport
			},
		}
		conn, _, err := dialer.Dial(address, header) // nolint
		if err != nil {
			return nil, err
		}
		c.ws = conn
		go c.listenResponses() // Start listening for responses
	}

	// Send message
	err := c.ws.WriteJSON(message)
	if err == nil {
		c.requests[echoUUID] = responseChan
	}
	c.requestMu.Unlock()
	if err != nil {
		c.resetWebsocket()
		return nil, err
	}
	// Set a timeout duration
	timeout := time.Duration(base.SignServerTimeout) * time.Second
	select {
	case response := <-responseChan:
		// Process the response
		c.requestMu.Lock()
		delete(c.requests, echoUUID)
		c.requestMu.Unlock()
		// Extract payload
		payload, e := json.Marshal(response["payload"])
		if e != nil {
			return nil, e
		}
		return payload, nil
	case <-time.After(timeout):
		// Handle the timeout case
		c.requestMu.Lock()
		delete(c.requests, echoUUID)
		c.requestMu.Unlock()
		c.resetWebsocket()
		return nil, errors.New("operation timed out in qsign websocket request")
	}
}

func (c *SignClient) listenResponses() {
	for {
		var response map[string]interface{}
		// ReadJSON in a function that sets c.ws to nil and panics on error
		err := func() error {
			defer func() {
				_ = recover()
			}()
			err := c.ws.ReadJSON(&response)
			if err != nil {
				return err
			}
			return nil
		}()
		if err != nil {
			break
		}
		if response == nil {
			break
		}

		// Handle the response
		echo, ok := response["echo"].(string)
		if !ok {
			log.Error("Invalid qsign websocket response format")
			continue
		}

		c.requestMu.Lock()
		if responseChan, found := c.requests[echo]; found {
			responseChan <- response
		}
		c.requestMu.Unlock()
	}
	c.resetWebsocket()
}

func (c *SignClient) signRequest(seq uint64, uin string, cmd string, buff []byte) (sign []byte, extra []byte, token []byte, err error) {
	_, response, err := c.requestSignServer(
		"sign",
		map[string]string{
			"cmd":    cmd,
			"seq":    strconv.FormatUint(seq, 10),
			"buffer": hex.EncodeToString(buff),
		},
	)
	if err != nil {
		return nil, nil, nil, err
	}
	sign, _ = hex.DecodeString(gjson.GetBytes(response, "data.sign").String())
	extra, _ = hex.DecodeString(gjson.GetBytes(response, "data.extra").String())
	token, _ = hex.DecodeString(gjson.GetBytes(response, "data.token").String())
	go c.signCallback(uin, gjson.GetBytes(response, "data.requestCallback").Array(), "sign")
	return sign, extra, token, nil
}

func (c *SignClient) signCallback(uin string, results []gjson.Result, t string) {
	for {
		if c.client.Online.Load() {
			break
		}
		time.Sleep(1 * time.Second)
	}
	for _, result := range results {
		cmd := result.Get("cmd").String()
		callbackID := result.Get("callbackId").Int()
		body, _ := hex.DecodeString(result.Get("body").String())
		ret, err := c.client.SendSsoPacket(cmd, body)
		if err != nil || len(ret) == 0 {
			log.Warnf("Callback error: %v, or response data is empty", err)
			continue
		}
		c.signSubmit(uin, cmd, callbackID, ret, t)
	}
}

func (c *SignClient) signSubmit(uin string, cmd string, callbackID int64, buffer []byte, t string) {
	buffStr := hex.EncodeToString(buffer)
	if base.Debug {
		tail := 64
		endl := "..."
		if len(buffStr) < tail {
			tail = len(buffStr)
			endl = "."
		}
		log.Debugf("submit (%v): uin=%v, cmd=%v, callbackID=%v, buffer=%v%s", t, uin, cmd, callbackID, buffStr[:tail], endl)
	}

	signServer, _, err := c.requestSignServer(
		"submit",
		map[string]string{
			"cmd":         cmd,
			"callback_id": strconv.FormatInt(callbackID, 10),
			"buffer":      buffStr,
		},
	)

	if err != nil {
		log.Warnf("Error submitting callback: %v. server: %v", err, signServer)
	}
}

// SignWhiteList retrieves the sign whitelist.
func (c *SignClient) SignWhiteList() (whitelist []string, err error) {
	_, response, err := c.requestSignServer(
		"cmd_whitelist",
		map[string]string{},
	)
	if err != nil {
		return nil, err
	}
	if gjson.GetBytes(response, "code").Int() == 0 {
		results := gjson.GetBytes(response, "data.list").Array()
		stringResults := make([]string, len(results))
		for i, result := range results {
			stringResults[i] = result.String()
		}
		return stringResults, nil
	}
	return nil, errors.New("get whitelist failed")
}
func (c *SignClient) signRegister() bool {
	signServer, resp, err := c.requestSignServer(
		"register",
		map[string]string{},
	)
	if err != nil {
		log.Warnf("Error registering QQ instance: %v. server: %v", err, signServer)
		return false
	}
	msg := gjson.GetBytes(resp, "msg")
	if !msg.Exists() {
		log.Warn("Error registering QSign")
		return false
	}
	if gjson.GetBytes(resp, "code").Int() != 0 {
		log.Warnf("Error registering QQ instance: %v. server: %v", msg, signServer)
		return false
	}
	log.Infof("Successfully registered QQ instance %v: %v", base.Account.Uin, msg)
	return true
}

// Energy requests energy data from the sign server.
func (c *SignClient) Energy(id string, sdkVersion string, salt []byte) ([]byte, error) {
	signServer, response, err := c.requestSignServer(
		"custom_energy",
		map[string]string{
			"data":    id,
			"version": sdkVersion,
			"salt":    hex.EncodeToString(salt),
		},
	)

	if err != nil {
		log.Warnf("Error getting energy sign: %v. server: %v", err, signServer)
		return nil, err
	}
	data, err := hex.DecodeString(gjson.GetBytes(response, "data").String())
	if err != nil {
		log.Warnf("Error decoding energy sign: %v (data: %v)", err, gjson.GetBytes(response, "data").String())
		return nil, err
	}
	if len(data) == 0 {
		log.Warnf("Error: data is empty.")
		return nil, errors.New("data is empty")
	}
	return data, nil
}

// Sign sends a sign request and returns the sign, extra, and token data.
func (c *SignClient) Sign(seq uint64, uin string, cmd string, buff []byte) (sign []byte, extra []byte, token []byte, err error) {
	i := 0
	for {
		i++
		if i > 5 {
			log.Warn("too many tried sign")
			return nil, nil, nil, errors.New("too many tried")
		}
		cs, e := c.manager.GetAvailableSignServer()
		if e != nil || cs == nil {
			log.Warn("nil sign-server")
			return sign, extra, token, errors.New("nil sign-server")
		}
		sign, extra, token, err = c.signRequest(seq, uin, cmd, buff)
		if err != nil {
			log.Warnf("Error getting sso sign: %v. server: %v", err, cs.URL)
			continue
		}
		break
	}
	rule := base.Account.RuleChangeSignServer
	if (len(sign) == 0 && rule >= 1) || (len(token) == 0 && rule >= 2) {
		c.manager.Set(nil)
	}
	return sign, extra, token, err
}
