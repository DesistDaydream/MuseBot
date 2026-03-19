// Package robot 实现企业微信智能机器人（AiBot）WebSocket 长连接接入。
//
// 使用前先在企业微信管理后台开启「长连接」API 模式，获取 BotID 和 Secret。
// 对应的配置项为 AIBOT_BOT_ID 和 AIBOT_SECRET。
package robot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/yincongcyincong/MuseBot/conf"
	"github.com/yincongcyincong/MuseBot/i18n"
	"github.com/yincongcyincong/MuseBot/logger"
	"github.com/yincongcyincong/MuseBot/metrics"
	"github.com/yincongcyincong/MuseBot/param"
	"github.com/yincongcyincong/MuseBot/utils"
)

const (
	aiBotWSURL         = "wss://openws.work.weixin.qq.com"
	aiBotHeartbeat     = 30 * time.Second
	aiBotWriteTimeout  = 10 * time.Second
	aiBotReconnectWait = 5 * time.Second
)

// --- WebSocket 消息协议 ---

type aiBotMsg struct {
	Cmd     string          `json:"cmd"`
	Headers aiBotHeaders    `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode int             `json:"errcode,omitempty"`
	ErrMsg  string          `json:"errmsg,omitempty"`
}

type aiBotHeaders struct {
	ReqID string `json:"req_id"`
}

type aiBotSubscribeBody struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

// aiBotMsgCallbackBody 是用户消息回调的 body。
type aiBotMsgCallbackBody struct {
	MsgID    string        `json:"msgid"`
	AIBotID  string        `json:"aibotid"`
	ChatID   string        `json:"chatid"`
	ChatType string        `json:"chattype"`
	From     aiBotFromUser `json:"from"`
	MsgType  string        `json:"msgtype"`
	Text     *aiBotTextMsg `json:"text,omitempty"`
}

// aiBotEventCallbackBody 是事件回调的 body。
type aiBotEventCallbackBody struct {
	MsgID      string        `json:"msgid"`
	CreateTime int64         `json:"create_time"`
	AIBotID    string        `json:"aibotid"`
	ChatID     string        `json:"chatid"`
	ChatType   string        `json:"chattype"`
	From       aiBotFromUser `json:"from"`
	MsgType    string        `json:"msgtype"`
	Event      aiBotEvent    `json:"event"`
}

type aiBotFromUser struct {
	UserID string `json:"userid"`
}

type aiBotTextMsg struct {
	Content string `json:"content"`
}

type aiBotEvent struct {
	EventType string `json:"eventtype"`
}

// aiBotRespondMsgBody 是普通（非流式）aibot_respond_msg 的 body。
type aiBotRespondMsgBody struct {
	MsgType string        `json:"msgtype"`
	Text    *aiBotTextMsg `json:"text,omitempty"`
}

// aiBotRespondStreamBody 是流式 aibot_respond_msg 的 body。
// msgtype 必须是 "stream"，内容在 stream.content 里，不是 text.content。
type aiBotRespondStreamBody struct {
	MsgType string       `json:"msgtype"` // 固定为 "stream"
	Stream  *aiBotStream `json:"stream"`
}

// aiBotStream 是流式消息的核心字段。
type aiBotStream struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Finish  bool   `json:"finish"`
}

type aiBotWelcomeMsgBody struct {
	MsgType string        `json:"msgtype"`
	Text    *aiBotTextMsg `json:"text,omitempty"`
}

// --- 连接管理 ---

// aiBotConn 管理企业微信智能机器人的 WebSocket 长连接。
type aiBotConn struct {
	botID  string
	secret string

	conn *websocket.Conn
	mu   sync.Mutex
}

// AiBotRobot 是面向 MuseBot Robot 接口的企业微信智能机器人实现。
type AiBotRobot struct {
	// 来自回调的上下文信息
	ReqID    string
	UserID   string
	ChatID   string
	ChatType string
	MsgID    string

	Prompt       string
	Command      string
	OriginPrompt string
	ImageContent []byte
	UserName     string

	// StreamID 是本次对话流式消息的唯一 ID，由 handleMsgCallback 在收到回调时立即创建。
	StreamID string

	Robot *RobotInfo
	conn  *aiBotConn
}

var globalAiBotConn *aiBotConn

// StartAiBotRobot 建立长连接并保持运行，ctx 取消后退出。
func StartAiBotRobot(ctx context.Context) {
	c := &aiBotConn{
		botID:  conf.BaseConfInfo.AiBotBotID,
		secret: conf.BaseConfInfo.AiBotSecret,
	}
	globalAiBotConn = c

	for {
		if err := c.runOnce(ctx); err != nil {
			logger.ErrorCtx(ctx, "aibot connection error", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(aiBotReconnectWait):
			logger.InfoCtx(ctx, "aibot reconnecting...")
		}
	}
}

// runOnce 完成一次完整的连接生命周期。
func (c *aiBotConn) runOnce(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, aiBotWSURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	logger.Info("aibot websocket connected")

	if err := c.subscribe(); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	logger.Info("aibot subscribed successfully")

	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.heartbeat(hbCtx)

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		go c.dispatch(ctx, raw)
	}
}

// subscribe 发送 aibot_subscribe 订阅请求。
func (c *aiBotConn) subscribe() error {
	body, _ := json.Marshal(aiBotSubscribeBody{
		BotID:  c.botID,
		Secret: c.secret,
	})
	return c.writeJSON(aiBotMsg{
		Cmd:     "aibot_subscribe",
		Headers: aiBotHeaders{ReqID: aiBotNewReqID()},
		Body:    body,
	})
}

// heartbeat 每 30 秒发一次 WebSocket ping 帧。
func (c *aiBotConn) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(aiBotHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(aiBotWriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				logger.Error("aibot heartbeat error", "err", err)
				return
			}
		}
	}
}

// dispatch 根据 cmd 字段分发消息。
func (c *aiBotConn) dispatch(ctx context.Context, raw []byte) {
	var msg aiBotMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		logger.ErrorCtx(ctx, "aibot unmarshal error", "err", err)
		return
	}

	switch msg.Cmd {
	case "aibot_msg_callback":
		c.handleMsgCallback(ctx, msg)
	case "aibot_event_callback":
		c.handleEventCallback(ctx, msg)
	default:
		// 收到服务端对我们发出请求的 ack 响应
		if msg.ErrCode != 0 {
			logger.WarnCtx(ctx, "aibot response error",
				"cmd", msg.Cmd, "errcode", msg.ErrCode, "errmsg", msg.ErrMsg)
		}
	}
}

// handleMsgCallback 处理用户消息回调，构造 AiBotRobot 并交给 MuseBot 框架处理。
func (c *aiBotConn) handleMsgCallback(ctx context.Context, msg aiBotMsg) {
	var body aiBotMsgCallbackBody
	if err := json.Unmarshal(msg.Body, &body); err != nil {
		logger.ErrorCtx(ctx, "aibot unmarshal msg callback", "err", err)
		return
	}

	logger.InfoCtx(ctx, "aibot message",
		"from", body.From.UserID,
		"chatType", body.ChatType,
		"chatID", body.ChatID,
		"msgType", body.MsgType)

	if body.MsgType != "text" || body.Text == nil {
		// 当前只处理文本消息，其他类型可按需扩展
		return
	}

	metrics.AppRequestCount.WithLabelValues("aibot").Inc()

	streamID := aiBotNewReqID()
	r := &AiBotRobot{
		ReqID:        msg.Headers.ReqID,
		UserID:       body.From.UserID,
		ChatID:       body.ChatID,
		ChatType:     body.ChatType,
		MsgID:        body.MsgID,
		OriginPrompt: body.Text.Content,
		StreamID:     streamID,
		conn:         c,
	}
	r.Command, r.Prompt = ParseCommand(body.Text.Content)

	// 立即发一条占位流式消息，占住 req_id 对应的回复窗口。
	// 企业微信要求必须在收到 callback 后的极短时间内响应，否则 req_id 过期。
	if err := c.RespondStream(msg.Headers.ReqID, streamID, "▌", false); err != nil {
		logger.ErrorCtx(ctx, "aibot send placeholder error", "err", err)
	}

	// 使用 from userid 作为 user context key，群聊时带上 chatid 区分会话
	ctxKey := body.From.UserID
	if body.ChatType == "group" {
		ctxKey = body.ChatID + ":" + body.From.UserID
	}
	newCtx := context.WithValue(ctx, "log_id", uuid.New().String())
	newCtx = context.WithValue(newCtx, "bot_name", conf.BaseConfInfo.BotName)
	_ = ctxKey // ctxKey 可在后续会话隔离中使用

	r.Robot = NewRobot(WithRobot(r), WithContext(newCtx))

	go func() {
		defer func() {
			if err := recover(); err != nil {
				logger.ErrorCtx(newCtx, "aibot exec panic", "err", err, "stack", string(debug.Stack()))
			}
		}()
		r.Robot.Exec()
	}()
}

// handleEventCallback 处理事件回调（进入会话、断开连接等）。
func (c *aiBotConn) handleEventCallback(ctx context.Context, msg aiBotMsg) {
	var body aiBotEventCallbackBody
	if err := json.Unmarshal(msg.Body, &body); err != nil {
		logger.ErrorCtx(ctx, "aibot unmarshal event callback", "err", err)
		return
	}

	logger.InfoCtx(ctx, "aibot event", "eventType", body.Event.EventType, "from", body.From.UserID)

	switch body.Event.EventType {
	case "enter_chat":
		welcome := i18n.GetMessage("aibot_welcome", nil)
		if welcome == "" {
			welcome = "你好！有什么可以帮你的吗？"
		}
		welcomeBody, _ := json.Marshal(aiBotWelcomeMsgBody{
			MsgType: "text",
			Text:    &aiBotTextMsg{Content: welcome},
		})
		_ = c.writeJSON(aiBotMsg{
			Cmd:     "aibot_respond_welcome_msg",
			Headers: aiBotHeaders{ReqID: msg.Headers.ReqID},
			Body:    welcomeBody,
		})
	case "disconnected_event":
		logger.Info("aibot connection replaced by a new connection")
	}
}

// respondMsg 通过长连接回复文本消息。
func (c *aiBotConn) respondMsg(reqID, text string) error {
	body, _ := json.Marshal(aiBotRespondMsgBody{
		MsgType: "text",
		Text:    &aiBotTextMsg{Content: text},
	})
	return c.writeJSON(aiBotMsg{
		Cmd:     "aibot_respond_msg",
		Headers: aiBotHeaders{ReqID: reqID},
		Body:    body,
	})
}

// RespondStream 发送或更新流式消息。相同 streamID 的调用更新同一条消息；finish=true 结束。
// 正确的 body 格式：msgtype="stream"，内容在 stream.content 里（不是 text.content）。
func (c *aiBotConn) RespondStream(reqID, streamID, text string, finish bool) error {
	body, _ := json.Marshal(aiBotRespondStreamBody{
		MsgType: "stream",
		Stream: &aiBotStream{
			ID:      streamID,
			Content: text,
			Finish:  finish,
		},
	})
	return c.writeJSON(aiBotMsg{
		Cmd:     "aibot_respond_msg",
		Headers: aiBotHeaders{ReqID: reqID},
		Body:    body,
	})
}

// writeJSON 线程安全地向 WebSocket 写入 JSON 消息。
func (c *aiBotConn) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	logger.Info("aibot send", "payload", string(data))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("aibot: connection is nil")
	}
	c.conn.SetWriteDeadline(time.Now().Add(aiBotWriteTimeout))
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func aiBotNewReqID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// --- Robot 接口实现 ---

func (r *AiBotRobot) checkValid() bool {
	return true
}

func (r *AiBotRobot) getMsgContent() string {
	return r.Command
}

func (r *AiBotRobot) requestLLM(content string) {
	go func() {
		defer func() {
			if err := recover(); err != nil {
				logger.ErrorCtx(r.Robot.Ctx, "AiBotRobot panic", "err", err, "stack", string(debug.Stack()))
			}
		}()
		if !strings.Contains(content, "/") && !strings.Contains(content, "$") && r.Prompt == "" {
			r.Prompt = content
		}
		r.Robot.ExecCmd(content, r.sendChatMessage, nil, nil)
	}()
}

func (r *AiBotRobot) sendChatMessage() {
	r.Robot.TalkingPreCheck(func() {
		r.executeLLM()
	})
}

func (r *AiBotRobot) executeLLM() {
	messageChan := &MsgChan{
		NormalMessageChan: make(chan *param.MsgInfo),
	}
	// HandleUpdate 会检测 StreamRobot 接口并调用 sendTextStream，
	// 无论 IsStreaming 如何配置都走同一条路。
	go r.Robot.HandleUpdate(messageChan, "")
	go r.Robot.ExecLLM(r.Prompt, messageChan)
}

func (r *AiBotRobot) sendImg() {
	r.Robot.TalkingPreCheck(func() {
		chatID, msgID, _ := r.Robot.GetChatIdAndMsgIdAndUserID()

		prompt := strings.TrimSpace(r.Prompt)
		if prompt == "" {
			r.Robot.SendMsg(chatID, i18n.GetMessage("photo_empty_content", nil), msgID, tgbotapi.ModeMarkdown, nil)
			return
		}

		imageContent, totalToken, err := r.Robot.CreatePhoto(prompt, r.ImageContent)
		if err != nil {
			r.Robot.SendMsg(chatID, err.Error(), msgID, tgbotapi.ModeMarkdown, nil)
			return
		}

		// AiBot 不支持直接发送图片，降级为发送文字提示
		// 如需支持图片，可通过企业微信「主动推送消息」接口实现
		_ = imageContent
		_ = totalToken
		r.Robot.SendMsg(chatID, i18n.GetMessage("photo_empty_content", nil), msgID, "", nil)
	})
}

func (r *AiBotRobot) sendVideo() {
	chatID, msgID, _ := r.Robot.GetChatIdAndMsgIdAndUserID()
	r.Robot.SendMsg(chatID, "AiBot 暂不支持视频发送", msgID, "", nil)
}

func (r *AiBotRobot) getPrompt() string {
	return r.Prompt
}

func (r *AiBotRobot) setPrompt(prompt string) {
	r.Prompt = prompt
}

func (r *AiBotRobot) getPerMsgLen() int {
	return 3500
}

func (r *AiBotRobot) sendVoiceContent(voiceContent []byte, duration int) error {
	return fmt.Errorf("aibot: voice not supported")
}

func (r *AiBotRobot) setCommand(command string) {
	r.Command = command
}

func (r *AiBotRobot) getCommand() string {
	return r.Command
}

func (r *AiBotRobot) getUserName() string {
	return r.UserID
}

func (r *AiBotRobot) executeLLMDirect() {
	r.executeLLM()
}

func (r *AiBotRobot) getImage() []byte {
	return r.ImageContent
}

func (r *AiBotRobot) setImage(image []byte) {
	r.ImageContent = image
}

// sendMedia 将图片保存为临时文件，通过 MuseBot 自身的 HTTP 服务器生成 URL，
// 再以 Markdown 图片语法通过新的流式消息发送。
// 视频暂不支持，静默丢弃。
func (r *AiBotRobot) sendMedia(media []byte, contentType, sType string) error {
	if sType != "image" {
		logger.WarnCtx(r.Robot.Ctx, "aibot: non-image media skipped", "sType", sType)
		return nil
	}
	if len(media) == 0 {
		return nil
	}

	// 保存图片到 data/media/ 目录，通过 HTTP 服务器暴露
	filename := utils.RandomFilename(contentType)
	savePath := utils.GetAbsPath("data/media/" + filename)
	if err := os.MkdirAll(utils.GetAbsPath("data/media"), 0755); err != nil {
		return fmt.Errorf("aibot mkdir: %w", err)
	}
	if err := os.WriteFile(savePath, media, 0644); err != nil {
		return fmt.Errorf("aibot write image: %w", err)
	}

	// 构造可访问的 URL（需要 HTTP_HOST 配置为外部可访问地址，例如 https://your-server.com）
	host := conf.BaseConfInfo.HTTPHost
	if host == "" {
		host = ":36060"
	}
	// HTTPHost 格式可能是 ":36060"（纯端口）或 "https://example.com"
	// 如果是纯端口，需要在前面加上协议和主机名，但这里无法自动获取外网 IP
	// 建议配置 HTTP_HOST 为完整的外网 URL，例如 "https://your-server.com"
	imageURL := strings.TrimRight(host, "/") + "/media/" + filename

	content := fmt.Sprintf("![](%s)", imageURL)
	newStreamID := aiBotNewReqID()
	if err := r.conn.RespondStream(r.ReqID, newStreamID, content, true); err != nil {
		return fmt.Errorf("aibot sendMedia stream: %w", err)
	}
	return nil
}

// SendImage 实现 llm.ImageSender 接口，供 robot.go 的 ImageSender 调用。
func (r *AiBotRobot) SendImage(ctx context.Context, imageData []byte) error {
	return r.sendMedia(imageData, utils.DetectImageFormat(imageData), "image")
}

// sendTextStream 实现 StreamRobot 接口，供 HandleUpdate 调用。
// 攒齐 channel 里的所有内容后，统一以 finish=true 发一条流式消息。
// 不区分 IsStreaming 模式——AiBot 永远只发一条最终消息，避免重复和 version conflict。
func (r *AiBotRobot) sendTextStream(messageChan *MsgChan) {
	if messageChan.NormalMessageChan == nil {
		return
	}

	var accumulated strings.Builder
	for msg := range messageChan.NormalMessageChan {
		if msg == nil {
			continue
		}
		accumulated.WriteString(msg.Content)
	}

	if accumulated.Len() > 0 {
		if err := r.conn.RespondStream(r.ReqID, r.StreamID, accumulated.String(), true); err != nil {
			logger.ErrorCtx(r.Robot.Ctx, "aibot respond stream error", "err", err)
		}
	}
}
