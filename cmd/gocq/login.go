package gocq

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ProtocolScience/AstralGo/client"
	"github.com/gorilla/websocket"
	"github.com/mattn/go-colorable"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"gopkg.ilharper.com/x/isatty"

	"github.com/ProtocolScience/AstralGocq/global"
)

const (
	qrCodeFile   = "qrcode.png"
	captchaFile  = "captcha.jpg"
	wsTimeout    = 2 * time.Minute
	httpTimeout  = 30 * time.Second
	pollInterval = time.Second
)

var console = bufio.NewReader(os.Stdin)

func readLine() (str string) {
	str, _ = console.ReadString('\n')
	str = strings.TrimSpace(str)
	return
}

/*
func readLineTimeout(t time.Duration) {
	r := make(chan string)
	go func() {
		select {
		case r <- readLine():
		case <-time.After(t):
		}
	}()
	select {
	case <-r:
	case <-time.After(t):
	}
}*/

func readIfTTY(de string) (str string) {
	if isatty.Isatty(os.Stdin.Fd()) {
		return readLine()
	}
	log.Warnf("未检测到输入终端，自动选择%s.", de)
	return de
}

var cli *client.QQClient
var device *client.DeviceInfo

// ErrSMSRequestError SMS请求出错
var ErrSMSRequestError = errors.New("sms request error")

func commonLogin() error {
	res, err := cli.Login()
	if err != nil {
		return errors.Wrap(err, "initial login failed")
	}
	return loginResponseProcessor(res)
}

func printQRCode(imgData []byte) {
	const (
		black = "\033[48;5;0m  \033[0m"
		white = "\033[48;5;7m  \033[0m"
	)

	img, err := png.Decode(bytes.NewReader(imgData))
	if err != nil {
		log.Errorf("Failed to decode QR code: %v", err)
		return
	}

	grayImg, ok := img.(*image.Gray)
	if !ok {
		log.Errorf("Expected grayscale image, got %T", img)
		return
	}

	data := grayImg.Pix
	bound := img.Bounds().Max.X

	// 预分配足够的缓冲区
	buf := make([]byte, 0, (bound*4+1)*(bound))

	for y := 0; y < bound; y++ {
		i := y * bound
		for x := 0; x < bound; x++ {
			if data[i] != 255 {
				buf = append(buf, white...)
			} else {
				buf = append(buf, black...)
			}
			i++
		}
		buf = append(buf, '\n')
	}

	// 一次性写入，减少系统调用
	if _, err := colorable.NewColorableStdout().Write(buf); err != nil {
		log.Errorf("Failed to write QR code to stdout: %v", err)
	}
}

func qrcodeLogin() error {
	rsp, err := cli.FetchQRCodeCustomSize(1, 2, 1)
	if err != nil {
		return errors.Wrap(err, "fetch QR code failed")
	}

	if err := os.WriteFile(qrCodeFile, rsp.ImageData, 0o644); err != nil {
		log.Warnf("Failed to write QR code to file: %v", err)
	}
	defer func() {
		if err := os.Remove(qrCodeFile); err != nil {
			log.Warnf("Failed to remove QR code file: %v", err)
		}
	}()

	if cli.Uin != 0 {
		log.Infof("请使用账号 %v 登录手机QQ扫描二维码 (%s) : ", cli.Uin, qrCodeFile)
	} else {
		log.Infof("请使用手机QQ扫描二维码 (%s) : ", qrCodeFile)
	}

	time.Sleep(pollInterval)
	printQRCode(rsp.ImageData)

	s, err := cli.QueryQRCodeStatus(rsp.Sig)
	if err != nil {
		return errors.Wrap(err, "query QR code status failed")
	}

	prevState := s.State
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for range ticker.C {
		s, err := cli.QueryQRCodeStatus(rsp.Sig)
		if err != nil || s == nil {
			continue
		}

		if prevState == s.State {
			continue
		}

		prevState = s.State
		switch s.State {
		case client.QRCodeCanceled:
			log.Fatalf("扫码被用户取消.")
		case client.QRCodeTimeout:
			log.Fatalf("二维码过期")
		case client.QRCodeWaitingForConfirm:
			log.Infof("扫码成功, 请在手机端确认登录.")
		case client.QRCodeConfirmed:
			res, err := cli.QRCodeLogin(s.LoginInfo)
			if err != nil {
				return errors.Wrap(err, "QR code login failed")
			}
			return loginResponseProcessor(res)
		case client.QRCodeImageFetch, client.QRCodeWaitingForScan:
			// ignore
		}
	}

	return nil // 不会执行到这里，但需要返回值
}

func loginResponseProcessor(res *client.LoginResponse) error {
	var err error
	for {
		if err != nil {
			return errors.Wrap(err, "login process failed")
		}

		if res.Success {
			return nil
		}

		var text string
		switch res.Error {
		case client.SliderNeededError:
			log.Warnf("登录需要滑条验证码, 请验证后重试.")
			ticket := getTicket(res.VerifyUrl)
			if ticket == "" {
				log.Infof("按 Enter 继续....")
				readLine()
				os.Exit(0)
			}
			res, err = cli.SubmitTicket(ticket)

		case client.NeedCaptcha:
			log.Warnf("登录需要验证码.")
			if err := os.WriteFile(captchaFile, res.CaptchaImage, 0o644); err != nil {
				log.Warnf("Failed to write captcha to file: %v", err)
			}
			log.Warnf("请输入验证码 (%s)： (Enter 提交)", captchaFile)
			text = readLine()
			global.DelFile(captchaFile)
			res, err = cli.SubmitCaptcha(text, res.CaptchaSign)

		case client.SMSNeededError:
			log.Warnf("账号已开启设备锁, 按 Enter 向手机 %v 发送短信验证码.", res.SMSPhone)
			readLine()
			if !cli.RequestSMS() {
				log.Warnf("发送验证码失败，可能是请求过于频繁.")
				return errors.WithStack(ErrSMSRequestError)
			}
			log.Warn("请输入短信验证码： (Enter 提交)")
			text = readLine()
			res, err = cli.SubmitSMS(text)

		case client.SMSOrVerifyNeededError:
			log.Warnf("账号已开启设备锁，请选择验证方式:")
			log.Warnf("1. 向手机 %v 发送短信验证码", res.SMSPhone)
			log.Warnf("2. 使用手机QQ扫码验证.")
			log.Warn("请输入(1 - 2)：")
			text = readIfTTY("2")
			if strings.Contains(text, "1") {
				if !cli.RequestSMS() {
					log.Warnf("发送验证码失败，可能是请求过于频繁.")
					return errors.WithStack(ErrSMSRequestError)
				}
				log.Warn("请输入短信验证码： (Enter 提交)")
				text = readLine()
				res, err = cli.SubmitSMS(text)
				continue
			}
			fallthrough

		case client.UnsafeDeviceError:
			log.Warnf("账号已开启设备锁，请前往 -> %v", res.VerifyUrl)
			log.Infof("按 Enter 继续....")
			readLine()
			res, err = cli.PasswordLogin()

		case client.OtherLoginError, client.UnknownLoginError, client.TooManySMSRequestError:
			msg := res.ErrorMessage
			log.Warnf("登录失败: %v Code: %v", msg, res.Code)

			// 使用map优化多个case判断
			codeMessages := map[int]string{
				235: "设备信息被封禁, 请删除 device.json 后重试.",
				237: "登录过于频繁, 请在手机QQ登录并根据提示完成认证后等一段时间重试",
				45:  "你的账号被限制登录, 请配置 SignServer 后重试",
			}
			code, err := strconv.Atoi(string(res.Code))
			if err != nil {
				log.Warnf("未知错误码: %v", res.Code)
			} else {
				if message, exists := codeMessages[code]; exists {
					log.Warnf(message)
				}
			}

			log.Infof("按 Enter 继续....")
			readLine()
			os.Exit(0)
		}
	}
}

func getTicket(u string) string {
	ticket := ""
	req := fmt.Sprintf("wss://gt-manual.xingdream.top/captcha/slider?key=%d", cli.Uin)

	log.Infof("尝试连接 WebSocket: %s", req)

	// 建立 WebSocket 连接
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = httpTimeout

	conn, _, err := dialer.Dial(req, nil)
	if err != nil {
		log.Errorf("WebSocket 连接失败: %v", err)
		log.Warn("请访问以下 URL 手动完成验证:")
		log.Warn(u)
		return readLine()
	}
	defer conn.Close()

	// 发送注册请求
	registerData := map[string]interface{}{
		"type":    "register",
		"payload": map[string]string{"url": u},
	}

	if err := conn.WriteJSON(registerData); err != nil {
		log.Errorf("发送 WebSocket 注册消息失败: %v", err)
		log.Warn("请访问以下 URL 手动完成验证:")
		log.Warn(u)
		return readLine()
	}

	log.Infof("\n----请打开下方链接并在2分钟内进行验证----\n%s\n----完成后将自动进行登录----",
		strings.Replace(req, "wss://", "https://", 1))

	// 创建HTTP客户端，仅使用IPv4
	httpClient := &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   httpTimeout,
				KeepAlive: httpTimeout,
				DualStack: false, // 强制使用 IPv4
			}).DialContext,
		},
	}

	// 监听 WebSocket 消息
	done := make(chan struct{})
	errorOccurred := make(chan struct{})

	go func() {
		defer close(done)

		// 设置读取超时
		_ = conn.SetReadDeadline(time.Now().Add(wsTimeout))

		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Warnf("WebSocket 读取错误: %v", err)
				close(errorOccurred)
				return
			}

			var data map[string]interface{}
			if err := json.Unmarshal(message, &data); err != nil {
				log.Warnf("解析 WebSocket 消息失败: %v", err)
				continue
			}

			msgType, ok := data["type"].(string)
			if !ok {
				continue
			}

			switch msgType {
			case "ticket":
				if payload, ok := data["payload"].(map[string]interface{}); ok {
					if ticketStr, ok := payload["ticket"].(string); ok && ticketStr != "" {
						ticket = ticketStr
						log.Infof("获取 Ticket 成功: %s", ticket)
						return
					}
				}

			case "handle":
				if payload, ok := data["payload"].(map[string]interface{}); ok {
					url, urlOk := payload["url"].(string)
					method, methodOk := payload["method"].(string)

					if !urlOk || url == "" {
						log.Warnf("无效的URL")
						continue
					}

					if !methodOk || method == "" {
						method = "GET"
					}

					var body io.Reader
					if method != "GET" { // 仅非 GET 请求才附带 body
						if b, ok := payload["body"].(string); ok && len(b) > 0 {
							body = strings.NewReader(b)
						}
					}

					// 创建请求
					req, err := http.NewRequest(method, url, body)
					if err != nil {
						log.Warnf("构造请求失败: %v", err)
						continue
					}

					// 添加请求头
					if headers, ok := payload["headers"].(map[string]interface{}); ok {
						for k, v := range headers {
							if strVal, ok := v.(string); ok {
								req.Header.Set(k, strVal)
							}
						}
					}

					// 发送请求
					rsp, err := httpClient.Do(req)
					if err != nil {
						log.Warnf("发送请求失败: %v", err)
						continue
					}

					// 确保响应体关闭
					responseData, err := func(rsp *http.Response) ([]byte, error) {
						defer rsp.Body.Close()
						return io.ReadAll(rsp.Body)
					}(rsp)

					if err != nil {
						log.Warnf("读取响应失败: %v", err)
						continue
					}

					// 返回结果
					payload["result"] = base64.StdEncoding.EncodeToString(responseData)

					// 转换响应头为可序列化格式
					headers := make(map[string][]string)
					for k, v := range rsp.Header {
						headers[k] = v
					}
					payload["response_headers"] = headers

					if err := conn.WriteJSON(data); err != nil {
						log.Warnf("发送处理结果失败: %v", err)
					}
				}
			}
		}
	}()

	// 等待 ticket 或超时
	select {
	case <-done:
		// 已经获取到ticket
	case <-errorOccurred:
		log.Warn("验证失败，请手动完成验证")
		log.Warn("请访问以下 URL 手动完成验证:")
		log.Warn(u)
		ticket = readLine()
	case <-time.After(wsTimeout):
		log.Warn("获取 Ticket 超时，请手动完成验证")
		log.Warn("请访问以下 URL 手动完成验证:")
		log.Warn(u)
		ticket = readLine()
	}

	// 连接已经在defer中关闭
	return ticket
}
