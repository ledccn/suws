package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Client WebSocket客户端连接
type Client struct {
	ID       string                 // 客户端ID（只读）
	Auth     string                 // 认证信息（只读）
	UID      string                 // 用户ID（绑定成功才有值）
	Conn     *websocket.Conn        // WebSocket连接
	Send     chan []byte            // 发送数据通道
	LastPing time.Time              // 最后ping时间（指客户端发送ping字符串或响应pong帧的时间）
	Session  map[string]interface{} // 客户端级Session
	mutex    sync.RWMutex           // 客户端读写锁
}

// Hub 维护活跃客户端连接
type Hub struct {
	register   chan *Client                      // 注册通道
	unregister chan *Client                      // 注销通道
	clients    map[string]*Client                // 使用client ID作为key的映射
	uidMap     map[string][]string               // UID到client_id列表的映射
	sessions   map[string]map[string]interface{} // UID到Session的映射（用户级Session）
	groups     map[string]map[string]int64       // 群组名到client_id到毫秒时间戳的映射（群组管理）
	rpcCalls   map[string]chan interface{}       // RPC调用等待通道映射（唯一key:uid + _id + _method）

	// 细粒度锁
	clientsMutex  sync.RWMutex // 保护clients映射
	uidMapMutex   sync.RWMutex // 保护uidMap映射
	sessionsMutex sync.RWMutex // 保护sessions映射
	groupsMutex   sync.RWMutex // 保护groups映射
	rpcMutex      sync.RWMutex // 保护rpcCalls映射
}

// BindRequest HTTP请求参数结构体，用于绑定和取消绑定
type BindRequest struct {
	ClientID string `json:"client_id"` // 客户端ID
	UID      string `json:"uid"`       // 用户ID
	Auth     string `json:"auth"`      // 认证信息（绑定时必须、解绑时可选）
}

// WebhookPayload webhook请求载荷
type WebhookPayload struct {
	UID      string `json:"uid"`       // 用户ID
	ClientID string `json:"client_id"` // 客户端ID
	Event    string `json:"event"`     // 事件名称
	Time     string `json:"time"`      // 时间 RFC3339
}

// Config 配置结构体
type Config struct {
	Port                       string    `json:"port"`                            // 服务监听端口
	Token                      string    `json:"token"`                           // 管理token
	Log                        LogConfig `json:"log"`                             // Log日志配置
	Webhook                    string    `json:"webhook"`                         // webhook URL
	WebhookTimeout             uint8     `json:"webhook_timeout"`                 // webhook超时时间（秒）
	WebhookMaxIdleConns        int       `json:"webhook_max_idle_conns"`          // 最大空闲连接数
	WebhookMaxIdleConnsPerHost int       `json:"webhook_max_idle_conns_per_host"` // 每主机最大空闲连接数
	WebhookIdleConnTimeout     int       `json:"webhook_idle_conn_timeout"`       // 空闲连接超时时间(秒)
}

// LogConfig Log配置结构体
type LogConfig struct {
	Verbose bool `json:"verbose"`
}

// StatResponse 统计信息响应结构体
type StatResponse struct {
	StartTime       time.Time `json:"start_time"`       // 启动时间
	Uptime          string    `json:"uptime"`           // 运行时间
	Goroutines      int       `json:"goroutines"`       // Goroutines数量
	MemoryAllocated string    `json:"memory_allocated"` // 内存使用量
	MemoryUsed      string    `json:"memory_used"`      // 内存占用量
	GCDetails       string    `json:"gc_details"`       // GC详细信息
	OnlineClients   int       `json:"online_clients"`   // 在线客户端数量
	OnlineUIDs      int       `json:"online_uids"`      // 在线UID数量
}

// RPCRequest RPC请求参数结构体
type RPCRequest struct {
	ID     uint64      `json:"_id"`            // 调用上下文 ID
	Method string      `json:"_method"`        // 调用方法名称
	Args   interface{} `json:"args,omitempty"` // 调用参数
}

// SendToAllRequest sendToAll接口的请求参数结构体
type SendToAllRequest struct {
	Data             string   `json:"data"`                         // 发送数据
	ClientIDs        []string `json:"client_ids,omitempty"`         // 指定client_id列表
	ExcludeClientIDs []string `json:"exclude_client_ids,omitempty"` // 排除client_id列表
	ExcludeUIDs      []string `json:"exclude_uids,omitempty"`       // 排除UID列表
}

// SendToGroupRequest sendToGroup接口的请求参数结构体
type SendToGroupRequest struct {
	Group            string   `json:"group"`                        // 群组名
	Data             string   `json:"data"`                         // 发送数据
	ClientIDs        []string `json:"client_ids,omitempty"`         // 指定client_id列表
	ExcludeClientIDs []string `json:"exclude_client_ids,omitempty"` // 排除client_id列表
	ExcludeUIDs      []string `json:"exclude_uids,omitempty"`       // 排除UID列表
}

// GroupRequest 群组操作请求参数结构体
type GroupRequest struct {
	Group    string `json:"group"`     // 群组名
	ClientID string `json:"client_id"` // 客户端ID
}

// 错误代码常量定义
const (
	ErrCodeSuccess                            = 0   // 成功
	ErrCodeInvalidParams                      = 100 // 参数错误
	ErrCodeMissingClientID                    = 101 // 缺少client_id参数
	ErrCodeMissingUID                         = 102 // 缺少uid参数
	ErrCodeMissingAuth                        = 103 // 缺少auth参数
	ErrCodeClientNotFound                     = 104 // 客户端不在线或不存在
	ErrCodeAuthFailed                         = 105 // 认证失败，auth不匹配
	ErrCodeUIDMismatch                        = 106 // 指定的UID与客户端当前绑定的UID不匹配
	ErrCodeMissingToken                       = 107 // 缺少token
	ErrCodeInvalidToken                       = 108 // token无效
	ErrCodeReadBodyFailed                     = 109 // 读取请求体失败
	ErrCodeEmptyBody                          = 110 // 请求体不能为空
	ErrCodeUserNotOnline                      = 111 // 用户不在线
	ErrCodeSerializeRPCFailed                 = 112 // 序列化RPC请求失败
	ErrCodeUnableToSendRPC                    = 113 // 无法发送RPC请求，可能是通道已满或连接已关闭
	ErrCodeRPCTimeout                         = 114 // RPC调用超时
	ErrCodeMissingUIDHeader                   = 115 // 缺少uid请求头
	ErrCodeMissingID                          = 116 // 缺少_id参数或_id不能为0
	ErrCodeMissingMethod                      = 117 // 缺少_method参数
	ErrCodeUIDNotOnline                       = 118 // UID不在线或不存在
	ErrCodeSendFailed                         = 119 // 发送失败，可能是通道已满或连接已关闭
	ErrCodeSendSerializeFailed                = 120 // 序列化发送数据失败
	ErrCodeGroupNotFound                      = 121 // 群组不存在
	ErrCodeNotInGroup                         = 122 // 客户端不在群组中
	ErrCodeMissingGroup                       = 123 // 缺少group参数
	ErrCodeMissingData                        = 124 // 缺少data参数
	ErrCodeGroupEmpty                         = 125 // 群组内客户端为空
	ErrCodeSendFailedGroupEmpty               = 126 // 发送失败，指定的群组客户端为空
	ErrCodeSendFailedGroupEmptyExcludeClients = 127 // 发送失败，排除client_ids后群组客户端为空
	ErrCodeSendFailedGroupEmptyExcludeUIDs    = 128 // 发送失败，排除UID后群组客户端为空
	ErrCodeSendFailedEmpty                    = 129 // 发送失败，指定的客户端为空
	ErrCodeSendFailedEmptyExcludeClients      = 130 // 发送失败，排除client_ids后客户端为空
	ErrCodeSendFailedEmptyExcludeUIDs         = 131 // 发送失败，排除UID后客户端为空
	ErrCodeMissingTimestamp                   = 132 // 缺少Timestamp请求头
	ErrCodeMissingSignature                   = 133 // 缺少Signature请求头
	ErrCodeInvalidTimestamp                   = 134 // 时间戳格式错误
	ErrCodeTimestampExpired                   = 135 // 时间戳已过期
	ErrCodeInvalidSignature                   = 136 // 签名无效
	ErrCodeConfigFileNotFound                 = 137 // 配置文件未找到
	ErrCodeConfigFileReadFailed               = 138 // 配置文件读取失败
	ErrCodeConfigFileParseFailed              = 139 // 配置文件解析失败
	ErrCodeConfigReloadFailed                 = 140 // 配置重载失败
)

// ErrorMessageMap 错误消息映射
var ErrorMessageMap = map[int]string{
	ErrCodeSuccess:                            "成功",
	ErrCodeInvalidParams:                      "SuWS参数错误",
	ErrCodeMissingClientID:                    "缺少client_id参数",
	ErrCodeMissingUID:                         "缺少uid参数",
	ErrCodeMissingAuth:                        "缺少auth参数",
	ErrCodeClientNotFound:                     "客户端不在线或不存在",
	ErrCodeAuthFailed:                         "认证失败，auth不匹配",
	ErrCodeUIDMismatch:                        "指定的UID与客户端当前绑定的UID不匹配",
	ErrCodeMissingToken:                       "缺少token",
	ErrCodeInvalidToken:                       "token无效",
	ErrCodeReadBodyFailed:                     "读取请求体失败",
	ErrCodeEmptyBody:                          "请求体不能为空",
	ErrCodeUserNotOnline:                      "用户不在线",
	ErrCodeSerializeRPCFailed:                 "序列化RPC请求失败",
	ErrCodeUnableToSendRPC:                    "无法发送RPC请求，可能是通道已满或连接已关闭",
	ErrCodeRPCTimeout:                         "RPC调用超时",
	ErrCodeMissingUIDHeader:                   "缺少uid请求头",
	ErrCodeMissingID:                          "缺少_id参数或_id不能为0",
	ErrCodeMissingMethod:                      "缺少_method参数",
	ErrCodeUIDNotOnline:                       "UID不在线或不存在",
	ErrCodeSendFailed:                         "发送失败，可能是通道已满或连接已关闭",
	ErrCodeSendSerializeFailed:                "序列化发送数据失败",
	ErrCodeGroupNotFound:                      "群组不存在",
	ErrCodeNotInGroup:                         "客户端不在群组中",
	ErrCodeMissingGroup:                       "缺少group参数",
	ErrCodeMissingData:                        "缺少data参数",
	ErrCodeGroupEmpty:                         "群组内客户端为空",
	ErrCodeSendFailedGroupEmpty:               "发送失败，指定的群组客户端为空",
	ErrCodeSendFailedGroupEmptyExcludeClients: "发送失败，排除client_ids后群组客户端为空",
	ErrCodeSendFailedGroupEmptyExcludeUIDs:    "发送失败，排除UID后群组客户端为空",
	ErrCodeSendFailedEmpty:                    "发送失败，指定的客户端为空",
	ErrCodeSendFailedEmptyExcludeClients:      "发送失败，排除client_ids后客户端为空",
	ErrCodeSendFailedEmptyExcludeUIDs:         "发送失败，排除UID后客户端为空",
	ErrCodeMissingTimestamp:                   "缺少Timestamp请求头",
	ErrCodeMissingSignature:                   "缺少Signature请求头",
	ErrCodeInvalidTimestamp:                   "时间戳格式错误",
	ErrCodeTimestampExpired:                   "时间戳已过期",
	ErrCodeInvalidSignature:                   "签名无效",
	ErrCodeConfigFileNotFound:                 "配置文件未找到",
	ErrCodeConfigFileReadFailed:               "配置文件读取失败",
	ErrCodeConfigFileParseFailed:              "配置文件解析失败",
	ErrCodeConfigReloadFailed:                 "配置重载失败",
}

// ResponseInfo 响应信息结构体
type ResponseInfo struct {
	Code    int         `json:"code"`           // 响应码
	Message string      `json:"message"`        // 响应描述信息
	Data    interface{} `json:"data,omitempty"` // 响应数据
}

// NewSuccessResponse 创建成功响应
func NewSuccessResponse(data interface{}) ResponseInfo {
	return ResponseInfo{
		Code:    ErrCodeSuccess,
		Message: ErrorMessageMap[ErrCodeSuccess],
		Data:    data,
	}
}

// NewSuccessResponseWithMessage 创建带自定义消息的成功响应
func NewSuccessResponseWithMessage(message string, data interface{}) ResponseInfo {
	return ResponseInfo{
		Code:    ErrCodeSuccess,
		Message: message,
		Data:    data,
	}
}

// NewErrorResponse 创建错误响应
func NewErrorResponse(code int) ResponseInfo {
	return ResponseInfo{
		Code:    code,
		Message: ErrorMessageMap[code],
	}
}

// NewCustomErrorResponse 创建自定义错误响应
func NewCustomErrorResponse(code int, message string) ResponseInfo {
	return ResponseInfo{
		Code:    code,
		Message: message,
	}
}

// Webhook事件名称常量定义
const (
	WebhookEventBind    = "bind"    // 客户端绑定UID事件
	WebhookEventUnbind  = "unbind"  // 客户端解绑UID事件
	WebhookEventOnline  = "online"  // UID上线事件
	WebhookEventOffline = "offline" // UID下线事件
	WebhookEventPing    = "ping"    // 客户端心跳事件（请求）
	WebhookEventPong    = "pong"    // 客户端心跳事件（响应）
)

// 全局变量
var hub *Hub
var config *Config
var configMutex sync.RWMutex       // 保护配置的读写锁
var configPath string              // 配置文件路径
var startTime time.Time            // 服务启动时间
var webhookHTTPClient *http.Client // webhook HTTP客户端（带连接池）

// Build variables (set via ldflags)
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "2025年9月10日"
)

// newHub 创建一个新的Hub实例
func newHub() *Hub {
	return &Hub{
		clients:    make(map[string]*Client),
		register:   make(chan *Client, 100), // 增加缓冲区大小
		unregister: make(chan *Client, 100), // 增加缓冲区大小
		uidMap:     make(map[string][]string),
		sessions:   make(map[string]map[string]interface{}),
		rpcCalls:   make(map[string]chan interface{}),
		groups:     make(map[string]map[string]int64),
	}
}

// 加载配置文件
func loadConfig(configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %v", err)
	}

	c := &Config{}
	if err := json.Unmarshal(data, c); err != nil {
		return fmt.Errorf("解析配置文件失败: %v", err)
	}
	if c.Token == "" {
		return fmt.Errorf("配置文件内 token 为空")
	}

	// 设置默认值
	if c.WebhookTimeout == 0 {
		c.WebhookTimeout = 5
	}
	if c.WebhookMaxIdleConns == 0 {
		c.WebhookMaxIdleConns = 100
	}
	if c.WebhookMaxIdleConnsPerHost == 0 {
		c.WebhookMaxIdleConnsPerHost = 5
	}
	if c.WebhookIdleConnTimeout == 0 {
		c.WebhookIdleConnTimeout = 90
	}
	if c.Webhook != "" {
		fmt.Println("WebHook已启用", c.Webhook)
		fmt.Println("WebHook超时时间", c.WebhookTimeout, "秒")
	}

	// 更新全局配置
	configMutex.Lock()
	config = c
	configMutex.Unlock()

	return nil
}

// run 运行hub主循环
func (h *Hub) run() {
	go h.heartbeatRoutine()

	for {
		select {
		case client := <-h.register:
			h.clientsMutex.Lock()
			h.clients[client.ID] = client
			h.clientsMutex.Unlock()
			slog.Debug(fmt.Sprintf("客户端 %s 已连接", client.ID))

		case client := <-h.unregister:
			h.clientsMutex.Lock()
			if _, ok := h.clients[client.ID]; ok {
				h.unregisterClient(client)
				slog.Debug(fmt.Sprintf("客户端 %s 已断开", client.ID))
			}
			h.clientsMutex.Unlock()
		}
	}
}

// heartbeatRoutine 心跳检测独立goroutine
func (h *Hub) heartbeatRoutine() {
	// 心跳检测定时器，每 X 秒检查一次心跳
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	var heartbeatCounter uint8 // 心跳检测频率计数器

	for {
		select {
		case <-ticker.C:
			// 每 X 秒计数器 +1
			heartbeatCounter++
			// 当计数器值大于或等于间隔值时，执行心跳检测逻辑
			if heartbeatCounter >= h.calculateHeartbeatInterval() {
				heartbeatCounter = 0

				now := time.Now()
				var timeoutClients []*Client
				h.clientsMutex.RLock()
				if len(h.clients) == 0 {
					h.clientsMutex.RUnlock()
					continue
				}

				// 收集超时客户端
				for id, client := range h.clients {
					client.mutex.RLock()
					lastPing := client.LastPing
					client.mutex.RUnlock()

					// 每30秒发送一个心跳包，如果超过 30秒 * 1.5 = 45秒 没有收到心跳包，则断开连接
					if now.Sub(lastPing) > 45*time.Second {
						timeoutClients = append(timeoutClients, client)
						slog.Debug(fmt.Sprintf("客户端 %s 因心跳超时已断开 (上次心跳: %v, 超时时间: %v)", id, lastPing.Format(time.RFC3339), now.Sub(lastPing)))
					}
				}
				h.clientsMutex.RUnlock()

				// 批量处理超时客户端
				for _, client := range timeoutClients {
					h.unregister <- client
				}
			}
		}
	}
}

// unregisterClient 内部注销客户端方法（必须加锁后才能调用）
func (h *Hub) unregisterClient(client *Client) {
	if client.UID != "" {
		// 如果客户端已绑定UID，先解绑
		h.unbindUidInternal(client)
	}

	// 从所有群组中移除客户端
	h.groupsMutex.Lock()
	for groupName, clients := range h.groups {
		if _, exists := clients[client.ID]; exists {
			delete(clients, client.ID)
			// 如果群组为空，清理群组
			if len(clients) == 0 {
				delete(h.groups, groupName)
			}
		}
	}
	h.groupsMutex.Unlock()

	// 从clients映射中删除
	delete(h.clients, client.ID)

	// 关闭发送通道
	close(client.Send)

	// 关闭WebSocket连接
	client.Conn.Close()
}

// unbindUidInternal 内部解绑方法
func (h *Hub) unbindUidInternal(client *Client) {
	if client.UID == "" {
		return
	}

	h.uidMapMutex.Lock()
	defer h.uidMapMutex.Unlock()

	clientIDs := h.uidMap[client.UID]
	remainingClients := make([]string, 0)

	// 找到要移除的client_id并过滤掉
	for _, id := range clientIDs {
		if id != client.ID {
			remainingClients = append(remainingClients, id)
		}
	}

	var event = WebhookEventUnbind
	// 检查是否还有其他客户端连接到这个UID
	if len(remainingClients) == 0 {
		event = WebhookEventOffline
		// 没有其他客户端连接到这个UID，清理UID映射和用户session
		delete(h.uidMap, client.UID)

		h.sessionsMutex.Lock()
		delete(h.sessions, client.UID)
		h.sessionsMutex.Unlock()

		slog.Debug(fmt.Sprintf("UID %s 已完全离线，清理相关session", client.UID))
	} else {
		// 还有其他客户端连接到这个UID，更新映射
		h.uidMap[client.UID] = remainingClients
	}

	// 触发webhook
	h.sendWebhook(client.UID, client.ID, event)

	client.UID = ""
}

// 处理WebSocket连接
func handleWebSocket(c *gin.Context) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true // 允许所有来源
		},
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		slog.Debug("升级WebSocket失败:", err)
		return
	}

	// 生成client_id和auth
	clientID := uuid.New().String()
	auth := generateNanoMd5()

	client := &Client{
		ID:       clientID,
		Auth:     auth,
		Conn:     conn,
		Send:     make(chan []byte, 512),
		LastPing: time.Now(),
		Session:  make(map[string]interface{}), // 初始化客户端会话
	}

	// 注册客户端
	hub.register <- client

	// 发送初始信息
	initMsg := map[string]interface{}{
		"event":     "init",
		"client_id": clientID,
		"auth":      auth,
	}

	jsonInitMsg, _ := json.Marshal(initMsg)
	client.Send <- jsonInitMsg

	// 启动读写goroutine
	go client.writePump()
	go client.readPump()
}

// calculateHeartbeatInterval 根据当前连接数计算心跳检测间隔值
func (h *Hub) calculateHeartbeatInterval() uint8 {
	h.clientsMutex.RLock()
	clientCount := len(h.clients)
	h.clientsMutex.RUnlock()

	switch {
	case clientCount < 100:
		return 1
	case clientCount < 500:
		return 2
	case clientCount < 1000:
		return 3
	case clientCount < 5000:
		return 4
	case clientCount < 10000:
		return 5
	default:
		return 6
	}
}

// generateNanoMd5 生成auth字符串（纳秒时间戳md5后的32字节小写字符串）
func generateNanoMd5() string {
	nanoStr := strconv.FormatInt(time.Now().UnixNano(), 10)
	hash := md5.Sum([]byte(nanoStr))
	return hex.EncodeToString(hash[:])
}

// writePump 处理向客户端写入消息
func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second) // 每30秒发送一次ping帧
	defer func() {
		ticker.Stop()
		hub.unregister <- c
	}()

	for {
		select {
		case message, ok := <-c.Send:
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			err := c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err != nil {
				return
			}
			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				// 明确处理超时和其他写入错误
				slog.Debug(fmt.Sprintf("客户端 %s 写入超时: %v", c.ID, err))
				return
			}
		case <-ticker.C:
			err := c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err != nil {
				return
			}
			// 服务端向客户端发送 Ping 帧
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				slog.Debug(fmt.Sprintf("客户端 %s Ping超时: %v", c.ID, err))
				return
			}
		}
	}
}

// readPump 处理从客户端读取消息
func (c *Client) readPump() {
	defer func() {
		hub.unregister <- c
	}()

	// 1. 限制消息大小
	c.Conn.SetReadLimit(262144) // 512KB（524288） 或 256KB（262144）
	// 2. 设置读取超时
	err := c.Conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	if err != nil {
		return
	}
	// 3. 服务端收到 Pong 帧后，触发 PongHandler
	c.Conn.SetPongHandler(func(string) error {
		return c.updateLastPing()
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Debug(fmt.Sprintf("WebSocket错误: %v", err))
			}
			break
		}

		msg := string(message)
		// 处理心跳包
		if msg == WebhookEventPing {
			err := c.updateLastPing()
			if err != nil {
				break
			}

			c.Send <- []byte(WebhookEventPong)
			continue
		}

		// 处理RPC响应
		if c.UID != "" {
			var rpcResp map[string]interface{}
			if err := json.Unmarshal(message, &rpcResp); err == nil {
				_, hasID := rpcResp["_id"]
				_, hasMethod := rpcResp["_method"]
				if hasID && hasMethod {
					hub.handleRPCResponse(c.UID, rpcResp)
					continue
				}
			}
		}

		// 可以处理其他消息类型
		slog.Debug(fmt.Sprintf("收到来自客户端 %s 的消息: %s", c.ID, msg))
	}
}

// updateLastPing 更新客户端最后心跳时间并重置读取超时
func (c *Client) updateLastPing() error {
	c.mutex.Lock()
	oldPing := c.LastPing
	c.LastPing = time.Now()
	c.mutex.Unlock()

	// 重置读取超时
	err := c.Conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	if err != nil {
		return err
	}

	// 只有当LastPing有实质性更新时才触发webhook
	if time.Since(oldPing) > 10*time.Second {
		hub.sendWebhook(c.UID, c.ID, WebhookEventPing)
	}

	return nil
}

// sendToClients 向指定客户端列表发送消息并返回成功和失败数量
func (h *Hub) sendToClients(targetClients map[string]*Client, data string) map[string]interface{} {
	successCount := 0
	failureCount := 0

	for _, client := range targetClients {
		select {
		case client.Send <- []byte(data):
			successCount++
		default:
			// 发送失败，可能是通道已满或连接已关闭
			failureCount++
		}
	}

	return map[string]interface{}{
		"success": successCount,
		"failure": failureCount,
	}
}

// sendToAll 向所有客户端或指定客户端发送字符串数据
func (h *Hub) sendToAll(req SendToAllRequest) ResponseInfo {
	h.clientsMutex.RLock()
	defer h.clientsMutex.RUnlock()

	// 优化：如果没有任何客户端连接，直接返回
	if len(h.clients) == 0 {
		return NewErrorResponse(ErrCodeClientNotFound)
	}

	// 创建要发送的客户端列表
	targetClients := make(map[string]*Client)

	// 如果指定了client_ids，则只向这些客户端发送
	if len(req.ClientIDs) > 0 {
		for _, clientID := range req.ClientIDs {
			if client, ok := h.clients[clientID]; ok {
				targetClients[clientID] = client
			}
		}

		// 优化：如果目标客户端为空，直接返回
		if len(targetClients) == 0 {
			return NewErrorResponse(ErrCodeSendFailedEmpty)
		}
	} else {
		// 否则向所有客户端发送
		for clientID, client := range h.clients {
			targetClients[clientID] = client
		}
	}

	// 如果指定了要排除的client_ids，则从目标客户端中移除
	if len(req.ExcludeClientIDs) > 0 {
		for _, clientID := range req.ExcludeClientIDs {
			delete(targetClients, clientID)
		}

		// 优化：如果目标客户端为空，直接返回
		if len(targetClients) == 0 {
			return NewErrorResponse(ErrCodeSendFailedEmptyExcludeClients)
		}
	}

	// 如果指定了要排除的uids，则从目标客户端中移除这些uid对应的客户端
	if len(req.ExcludeUIDs) > 0 {
		h.uidMapMutex.RLock()
		for _, uid := range req.ExcludeUIDs {
			if clientIDs, ok := h.uidMap[uid]; ok {
				for _, clientID := range clientIDs {
					delete(targetClients, clientID)
				}
			}
		}
		h.uidMapMutex.RUnlock()

		// 优化：如果目标客户端为空，直接返回
		if len(targetClients) == 0 {
			return NewErrorResponse(ErrCodeSendFailedEmptyExcludeUIDs)
		}
	}

	// 发送消息并统计成功和失败数量
	result := h.sendToClients(targetClients, req.Data)
	return NewSuccessResponse(result)
}

// sendToClient 向指定client_id发送数据
func (h *Hub) sendToClient(clientID string, body []byte) ResponseInfo {
	h.clientsMutex.RLock()
	client, ok := h.clients[clientID]
	h.clientsMutex.RUnlock()
	if !ok {
		return NewErrorResponse(ErrCodeClientNotFound)
	}

	select {
	case client.Send <- body:
		return NewSuccessResponse(nil)
	default:
		return NewErrorResponse(ErrCodeSendFailed)
	}
}

// closeClient 断开与指定client_id的客户端连接
func (h *Hub) closeClient(clientID string) bool {
	h.clientsMutex.RLock()
	client, ok := h.clients[clientID]
	h.clientsMutex.RUnlock()
	if !ok {
		return false
	}

	h.unregister <- client
	slog.Debug(fmt.Sprintf("客户端 %s 已被主动断开连接", clientID))

	return true
}

// isClientOnline 判断指定client_id是否在线
func (h *Hub) isClientOnline(clientID string) bool {
	h.clientsMutex.RLock()
	_, ok := h.clients[clientID]
	h.clientsMutex.RUnlock()
	return ok
}

// bindUid 将client_id与uid绑定
func (h *Hub) bindUid(clientID, uid, auth string) ResponseInfo {
	h.clientsMutex.Lock()
	defer h.clientsMutex.Unlock()
	client, ok := h.clients[clientID]
	if !ok {
		return NewErrorResponse(ErrCodeClientNotFound)
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()
	if client.Auth != auth {
		return NewErrorResponse(ErrCodeAuthFailed)
	}

	if client.UID != "" {
		if client.UID == uid {
			// 重复绑定，直接返回
			return NewSuccessResponse(nil)
		} else {
			// 如果客户端已经绑定了其他UID，先解绑
			h.unbindUidInternal(client)
		}
	}

	// 绑定新的UID
	client.UID = uid

	var event = WebhookEventBind
	h.uidMapMutex.Lock()
	h.uidMap[uid] = append(h.uidMap[uid], clientID)
	if len(h.uidMap[uid]) == 1 {
		event = WebhookEventOnline
	}
	h.uidMapMutex.Unlock()

	slog.Debug(fmt.Sprintf("客户端 %s 已绑定到UID %s", clientID, uid))

	// 触发webhook
	h.sendWebhook(uid, clientID, event)

	return NewSuccessResponse(nil)
}

// unbindUid 将client_id与uid解绑
func (h *Hub) unbindUid(clientID, uid, auth string) ResponseInfo {
	h.clientsMutex.Lock()
	defer h.clientsMutex.Unlock()
	client, ok := h.clients[clientID]
	if !ok {
		return NewErrorResponse(ErrCodeClientNotFound)
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()
	if client.UID != uid {
		return NewErrorResponse(ErrCodeUIDMismatch)
	}

	// （可选的）解绑时验证auth
	if auth != "" && auth != client.Auth {
		return NewErrorResponse(ErrCodeAuthFailed)
	}

	h.unbindUidInternal(client)
	slog.Debug(fmt.Sprintf("客户端 %s 已从UID %s 解绑", clientID, uid))
	return NewSuccessResponse(nil)
}

// isUidOnline 判断UID是否在线
func (h *Hub) isUidOnline(uid string) bool {
	h.uidMapMutex.RLock()
	defer h.uidMapMutex.RUnlock()
	clientIDs, ok := h.uidMap[uid]
	if !ok {
		return false
	}

	// 检查这些client_id是否仍然有效（客户端仍然在线）
	h.clientsMutex.RLock()
	defer h.clientsMutex.RUnlock()
	for _, clientID := range clientIDs {
		if _, clientExists := h.clients[clientID]; clientExists {
			return true
		}
	}

	return false
}

// sendToUid 向UID绑定的所有在线client_id发送字符串数据
func (h *Hub) sendToUid(uid string, body []byte) ResponseInfo {
	h.uidMapMutex.RLock()
	clientIDs, ok := h.uidMap[uid]
	h.uidMapMutex.RUnlock()
	if !ok || len(clientIDs) == 0 {
		return NewErrorResponse(ErrCodeUIDNotOnline)
	}

	h.clientsMutex.RLock()
	defer h.clientsMutex.RUnlock()

	sent := false
	for _, clientID := range clientIDs {
		// 检查客户端是否仍然在线
		client, clientExists := h.clients[clientID]
		if !clientExists {
			continue
		}

		select {
		case client.Send <- body:
			sent = true
		default:
			// 发送失败，可能是通道已满或连接已关闭
		}
	}
	if sent {
		return NewSuccessResponse(nil)
	} else {
		return NewErrorResponse(ErrCodeSendFailed)
	}
}

// getUidSession 获取某个UID对应的session
func (h *Hub) getUidSession(uid string) ResponseInfo {
	h.uidMapMutex.RLock()
	clientIDs, ok := h.uidMap[uid]
	h.uidMapMutex.RUnlock()
	if !ok || len(clientIDs) == 0 {
		return NewErrorResponse(ErrCodeUIDNotOnline)
	}

	h.sessionsMutex.RLock()
	session, ok := h.sessions[uid]
	h.sessionsMutex.RUnlock()
	if ok {
		return NewSuccessResponse(map[string]interface{}{
			"session": session,
		})
	} else {
		return NewSuccessResponse(map[string]interface{}{
			"session": nil,
		})
	}
}

// setUidSession 设置某个UID对应的session
func (h *Hub) setUidSession(uid string, session map[string]interface{}) ResponseInfo {
	h.uidMapMutex.RLock()
	defer h.uidMapMutex.RUnlock()
	clientIDs, ok := h.uidMap[uid]
	if !ok || len(clientIDs) == 0 {
		return NewErrorResponse(ErrCodeUIDNotOnline)
	}

	h.sessionsMutex.Lock()
	h.sessions[uid] = session
	h.sessionsMutex.Unlock()
	return NewSuccessResponse(nil)
}

// getClientSession 获取某个client_id对应的session
func (h *Hub) getClientSession(clientID string) ResponseInfo {
	h.clientsMutex.RLock()
	client, ok := h.clients[clientID]
	h.clientsMutex.RUnlock()
	if !ok {
		return NewErrorResponse(ErrCodeClientNotFound)
	}
	client.mutex.RLock()
	session := client.Session
	client.mutex.RUnlock()

	return NewSuccessResponse(map[string]interface{}{
		"session": session,
	})
}

// setClientSession 设置某个client_id对应的session
func (h *Hub) setClientSession(clientID string, session map[string]interface{}) ResponseInfo {
	h.clientsMutex.RLock()
	defer h.clientsMutex.RUnlock()
	client, ok := h.clients[clientID]
	if !ok {
		return NewErrorResponse(ErrCodeClientNotFound)
	}

	client.mutex.Lock()
	client.Session = session
	client.mutex.Unlock()
	return NewSuccessResponse(nil)
}

// handleRPCResponse 处理来自客户端的RPC响应
func (h *Hub) handleRPCResponse(uid string, response map[string]interface{}) {
	id, hasID := response["_id"]
	method, hasMethod := response["_method"]

	if hasID && hasMethod {
		var idStr string
		switch v := id.(type) {
		case uint64:
			idStr = strconv.FormatUint(v, 10)
		case uint32:
			idStr = strconv.FormatUint(uint64(v), 10)
		case int64:
			idStr = strconv.FormatInt(v, 10)
		case int32:
			idStr = strconv.FormatInt(int64(v), 10)
		case int:
			idStr = strconv.Itoa(v)
		case float64:
			idStr = strconv.FormatInt(int64(v), 10)
		case string:
			idStr = v
		default:
			idStr = fmt.Sprintf("%v", v)
		}

		// RCP等待通道：唯一KEY = uid + _id + _method
		rpcKey := fmt.Sprintf("%s_%s_%s", uid, idStr, method)

		h.rpcMutex.RLock()
		rpcChan, ok := h.rpcCalls[rpcKey]
		h.rpcMutex.RUnlock()

		if ok {
			// 发送响应到等待的RPC调用
			select {
			case rpcChan <- response:
			default:
				// 通道已满或关闭
			}
		}
	}
}

// callRPC 发起RPC调用并等待响应
func (h *Hub) callRPC(uid string, body []byte, req *RPCRequest) ResponseInfo {
	h.uidMapMutex.RLock()
	clientIDs, ok := h.uidMap[uid]
	h.uidMapMutex.RUnlock()
	if !ok || len(clientIDs) == 0 {
		return NewErrorResponse(ErrCodeUIDNotOnline)
	}

	// 构造RPC请求消息
	jsonMessage := body

	// RCP等待通道：唯一KEY = uid + _id + _method
	rpcKey := fmt.Sprintf("%s_%d_%s", uid, req.ID, req.Method)
	rpcChan := make(chan interface{}, 1)

	h.rpcMutex.Lock()
	h.rpcCalls[rpcKey] = rpcChan
	h.rpcMutex.Unlock()

	// 确保在函数退出时清理rpcCalls
	defer func() {
		h.rpcMutex.Lock()
		delete(h.rpcCalls, rpcKey)
		h.rpcMutex.Unlock()
	}()

	// 发送RPC请求
	sent := false
	h.clientsMutex.RLock()
	for _, clientID := range clientIDs {
		client, clientExists := h.clients[clientID]
		if !clientExists {
			continue
		}

		select {
		case client.Send <- jsonMessage:
			sent = true
		default:
			// 发送失败，可能是通道已满或连接已关闭
		}
	}
	h.clientsMutex.RUnlock()

	if !sent {
		return NewErrorResponse(ErrCodeUnableToSendRPC)
	}

	// 等待响应或超时
	select {
	case result := <-rpcChan:
		return NewSuccessResponse(result)
	case <-time.After(10 * time.Second):
		return NewErrorResponse(ErrCodeRPCTimeout)
	}
}

// joinGroup 将客户端加入群组
func (h *Hub) joinGroup(groupName, clientID string) ResponseInfo {
	h.clientsMutex.RLock()
	defer h.clientsMutex.RUnlock()
	_, clientExists := h.clients[clientID]
	if !clientExists {
		return NewErrorResponse(ErrCodeClientNotFound)
	}

	// 初始化群组映射
	h.groupsMutex.Lock()
	if h.groups[groupName] == nil {
		h.groups[groupName] = make(map[string]int64)
	}

	// 将客户端添加到群组，记录加入毫秒时间戳
	h.groups[groupName][clientID] = time.Now().UnixMilli()
	h.groupsMutex.Unlock()
	slog.Debug(fmt.Sprintf("客户端 %s 已加入群组 %s", clientID, groupName))

	return NewSuccessResponse(nil)
}

// leaveGroup 将客户端从群组中移除
func (h *Hub) leaveGroup(groupName, clientID string) ResponseInfo {
	h.groupsMutex.Lock()
	defer h.groupsMutex.Unlock()
	group, ok := h.groups[groupName]
	if !ok {
		return NewErrorResponse(ErrCodeGroupNotFound)
	}

	// 检查客户端是否在群组中
	if _, inGroup := group[clientID]; !inGroup {
		return NewErrorResponse(ErrCodeNotInGroup)
	}

	// 从群组中移除客户端
	delete(group, clientID)

	// 如果群组为空，清理群组
	if len(group) == 0 {
		delete(h.groups, groupName)
	}

	slog.Debug(fmt.Sprintf("客户端 %s 已从群组 %s 离开", clientID, groupName))

	return NewSuccessResponse(nil)
}

// ungroup 解散整个群组
func (h *Hub) ungroup(groupName string) ResponseInfo {
	h.groupsMutex.Lock()
	defer h.groupsMutex.Unlock()
	_, ok := h.groups[groupName]
	if !ok {
		return NewErrorResponse(ErrCodeGroupNotFound)
	}

	delete(h.groups, groupName)
	slog.Debug(fmt.Sprintf("群组 %s 已被解散", groupName))

	return NewSuccessResponse(nil)
}

// sendToGroup 向群组中的所有客户端发送消息，参考 sendToAll 的实现
func (h *Hub) sendToGroup(req SendToGroupRequest) ResponseInfo {
	h.groupsMutex.RLock()
	group, ok := h.groups[req.Group]
	h.groupsMutex.RUnlock()
	if !ok || len(group) == 0 {
		return NewErrorResponse(ErrCodeGroupEmpty)
	}

	// 创建要发送的客户端列表
	targetClients := make(map[string]*Client)

	h.clientsMutex.RLock()
	defer h.clientsMutex.RUnlock()

	// 如果指定了client_ids，则只向这些客户端发送
	if len(req.ClientIDs) > 0 {
		for _, clientID := range req.ClientIDs {
			// 检查客户端是否在群组中
			if _, inGroup := group[clientID]; inGroup {
				if client, ok := h.clients[clientID]; ok {
					targetClients[clientID] = client
				}
			}
		}
	} else {
		// 否则向群组中的所有客户端发送
		for clientID := range group {
			if client, ok := h.clients[clientID]; ok {
				targetClients[clientID] = client
			}
		}
	}

	// 优化：如果目标客户端为空，直接返回
	if len(targetClients) == 0 {
		return NewErrorResponse(ErrCodeSendFailedGroupEmpty)
	}

	// 如果指定了要排除的client_ids，则从目标客户端中移除
	if len(req.ExcludeClientIDs) > 0 {
		for _, clientID := range req.ExcludeClientIDs {
			delete(targetClients, clientID)
		}

		// 优化：如果目标客户端为空，直接返回
		if len(targetClients) == 0 {
			return NewErrorResponse(ErrCodeSendFailedGroupEmptyExcludeClients)
		}
	}

	// 如果指定了要排除的uids，则从目标客户端中移除这些uid对应的客户端
	if len(req.ExcludeUIDs) > 0 {
		h.uidMapMutex.RLock()
		for _, uid := range req.ExcludeUIDs {
			if clientIDs, ok := h.uidMap[uid]; ok {
				for _, clientID := range clientIDs {
					delete(targetClients, clientID)
				}
			}
		}
		h.uidMapMutex.RUnlock()

		// 优化：如果目标客户端为空，直接返回
		if len(targetClients) == 0 {
			return NewErrorResponse(ErrCodeSendFailedGroupEmptyExcludeUIDs)
		}
	}

	// 发送消息并统计成功和失败数量
	result := h.sendToClients(targetClients, req.Data)
	return NewSuccessResponse(result)
}

// getClientIdCountByGroup 获取群组中的在线客户端数量
func (h *Hub) getClientIdCountByGroup(groupName string) ResponseInfo {
	h.groupsMutex.RLock()
	group, ok := h.groups[groupName]
	h.groupsMutex.RUnlock()
	if !ok {
		return NewErrorResponse(ErrCodeGroupNotFound)
	}

	count := 0
	h.clientsMutex.RLock()
	for clientID := range group {
		if _, clientExists := h.clients[clientID]; clientExists {
			count++
		}
	}
	h.clientsMutex.RUnlock()

	return NewSuccessResponse(map[string]interface{}{
		"count": count,
	})
}

// getClientSessionsByGroup 获取群组中所有客户端的session信息
func (h *Hub) getClientSessionsByGroup(groupName string) ResponseInfo {
	h.groupsMutex.RLock()
	group, ok := h.groups[groupName]
	h.groupsMutex.RUnlock()
	if !ok || len(group) == 0 {
		return NewErrorResponse(ErrCodeGroupNotFound)
	}

	sessions := make(map[string]interface{})
	h.clientsMutex.RLock()
	for clientID := range group {
		// 检查客户端是否在线
		if client, clientExists := h.clients[clientID]; clientExists {
			client.mutex.RLock()
			sessions[clientID] = client.Session
			client.mutex.RUnlock()
		}
	}
	h.clientsMutex.RUnlock()

	return NewSuccessResponse(map[string]interface{}{
		"sessions": sessions,
	})
}

// getClientIdListByGroup 获取群组中的所有client_id到毫秒时间戳的映射
func (h *Hub) getClientIdListByGroup(groupName string) ResponseInfo {
	h.groupsMutex.RLock()
	group, ok := h.groups[groupName]
	h.groupsMutex.RUnlock()
	if !ok {
		return NewErrorResponse(ErrCodeGroupNotFound)
	}

	// 过滤出仍然在线的client_id及其加入时间戳
	clients := make(map[string]int64)
	h.clientsMutex.RLock()
	for clientID, timestamp := range group {
		if _, clientExists := h.clients[clientID]; clientExists {
			clients[clientID] = timestamp
		}
	}
	h.clientsMutex.RUnlock()

	if len(clients) == 0 {
		return NewSuccessResponse(map[string]interface{}{
			"clients": nil,
		})
	}

	return NewSuccessResponse(map[string]interface{}{
		"clients": clients,
	})
}

// getUidListByGroup 获取群组中所有客户端绑定的UID列表
func (h *Hub) getUidListByGroup(groupName string) ResponseInfo {
	h.groupsMutex.RLock()
	group, ok := h.groups[groupName]
	h.groupsMutex.RUnlock()
	if !ok {
		return NewErrorResponse(ErrCodeGroupNotFound)
	}

	// 收集群组中所有在线客户端绑定的UID
	uidSet := make(map[string]bool)
	h.clientsMutex.RLock()
	for clientID := range group {
		if client, clientExists := h.clients[clientID]; clientExists {
			client.mutex.RLock()
			if client.UID != "" {
				uidSet[client.UID] = true
			}
			client.mutex.RUnlock()
		}
	}
	h.clientsMutex.RUnlock()

	if len(uidSet) == 0 {
		return NewSuccessResponse(map[string]interface{}{
			"uids": nil,
		})
	}

	uids := make([]string, 0, len(uidSet))
	for uid := range uidSet {
		uids = append(uids, uid)
	}

	return NewSuccessResponse(map[string]interface{}{
		"uids": uids,
	})
}

// getUidCountByGroup 获取群组中客户端绑定的UID数量
func (h *Hub) getUidCountByGroup(groupName string) ResponseInfo {
	h.groupsMutex.RLock()
	group, ok := h.groups[groupName]
	h.groupsMutex.RUnlock()
	if !ok {
		return NewErrorResponse(ErrCodeGroupNotFound)
	}

	// 统计群组中不同UID的数量
	uidSet := make(map[string]bool)
	h.clientsMutex.RLock()
	for clientID := range group {
		if client, clientExists := h.clients[clientID]; clientExists {
			if client.UID != "" {
				client.mutex.RLock()
				uidSet[client.UID] = true
				client.mutex.RUnlock()
			}
		}
	}
	h.clientsMutex.RUnlock()

	return NewSuccessResponse(map[string]interface{}{
		"count": len(uidSet),
	})
}

// getGroupByUid 获取指定UID所属的所有群组及详细信息
func (h *Hub) getGroupByUid(uid string) ResponseInfo {
	h.uidMapMutex.RLock()
	clientIDs, ok := h.uidMap[uid]
	h.uidMapMutex.RUnlock()
	if !ok || len(clientIDs) == 0 {
		return NewErrorResponse(ErrCodeUIDNotOnline)
	}

	// 收集该UID所属的所有群组及详细信息
	groupInfo := make(map[string]map[string]int64)

	// 遍历该UID绑定的所有客户端
	h.clientsMutex.RLock()
	h.groupsMutex.RLock()
	for _, clientID := range clientIDs {
		// 检查客户端是否在线
		if _, clientExists := h.clients[clientID]; clientExists {
			// 检查该客户端属于哪些群组
			for groupName, clients := range h.groups {
				// 如果该客户端在群组中，记录其加入时间
				if timestamp, inGroup := clients[clientID]; inGroup {
					if groupInfo[groupName] == nil {
						groupInfo[groupName] = make(map[string]int64)
					}
					groupInfo[groupName][clientID] = timestamp
				}
			}
		}
	}
	h.groupsMutex.RUnlock()
	h.clientsMutex.RUnlock()

	if len(groupInfo) == 0 {
		return NewSuccessResponse(map[string]interface{}{
			"groups": nil,
		})
	}

	return NewSuccessResponse(map[string]interface{}{
		"groups": groupInfo,
	})
}

// sendWebhook 发送webhook请求
func (h *Hub) sendWebhook(uid, clientID, event string) {
	// 读取配置（使用读锁）
	configMutex.RLock()
	webhookURL := config.Webhook
	token := config.Token
	configMutex.RUnlock()

	if webhookURL == "" || uid == "" {
		return
	}

	// 创建一个goroutine，避免阻塞
	go func() {
		payload := WebhookPayload{
			UID:      uid,
			ClientID: clientID,
			Event:    event,
			Time:     time.Now().Format(time.RFC3339),
		}

		jsonPayload, err := json.Marshal(payload)
		if err != nil {
			slog.Debug(fmt.Sprintf("序列化webhook载荷失败: %v", err))
			return
		}

		// 生成毫秒时间戳
		timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)

		// 生成签名: body + 毫秒时间戳 + token
		signature := generateSignature(string(jsonPayload), timestamp, token)

		// 创建HTTP请求
		req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(jsonPayload))
		if err != nil {
			slog.Debug(fmt.Sprintf("创建webhook请求失败: %v", err))
			return
		}

		// 设置请求头
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Timestamp", timestamp)
		req.Header.Set("Signature", signature)

		// 使用连接池客户端发送请求
		resp, err := webhookHTTPClient.Do(req)
		if err != nil {
			slog.Debug(fmt.Sprintf("发送webhook请求失败: %v", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			slog.Debug(fmt.Sprintf("webhook发送成功: uid=%s, client_id=%s, event=%s", uid, clientID, event))
		} else {
			slog.Debug(fmt.Sprintf("webhook返回错误状态码: %d, uid=%s, client_id=%s, event=%s", resp.StatusCode, uid, clientID, event))
		}
	}()
}

// handleStats 处理统计信息请求
func handleStats(c *gin.Context) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	// 计算运行时间
	uptime := time.Since(startTime)

	// 获取在线客户端和UID数量
	hub.clientsMutex.RLock()
	onlineClients := len(hub.clients)
	hub.clientsMutex.RUnlock()

	hub.uidMapMutex.RLock()
	onlineUIDs := len(hub.uidMap)
	hub.uidMapMutex.RUnlock()

	stats := StatResponse{
		StartTime:       startTime,                                                             // 启动时间
		Uptime:          uptime.String(),                                                       // 运行时间
		Goroutines:      runtime.NumGoroutine(),                                                // Goroutines数量
		MemoryAllocated: formatBytes(ms.Alloc),                                                 // 内存使用量
		MemoryUsed:      formatBytes(ms.Sys),                                                   // 内存占用量
		GCDetails:       fmt.Sprintf("GC次数: %d, 下次GC目标: %s", ms.NumGC, formatBytes(ms.NextGC)), // GC详细信息
		OnlineClients:   onlineClients,                                                         // 当前在线客户端数量
		OnlineUIDs:      onlineUIDs,                                                            // 当前UID数量
	}

	resp := NewSuccessResponseWithMessage("统计信息获取成功", stats)
	c.JSON(http.StatusOK, resp)
}

// formatBytes 将字节数转换为人类可读的格式
func formatBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// generateSignature 生成签名
// 算法: data + timestamp + token 的MD5值
func generateSignature(data, timestamp, token string) string {
	signData := data + timestamp + token
	hash := md5.Sum([]byte(signData))
	return hex.EncodeToString(hash[:])
}

// verifySignature 验证签名
// 算法: data + timestamp + token 的MD5值
func verifySignature(data, timestamp, token, signature string) bool {
	expectedSignature := generateSignature(data, timestamp, token)
	return expectedSignature == signature
}

// handleVersion 处理版本信息请求
func handleVersion(c *gin.Context) {
	versionInfo := map[string]interface{}{
		"version":    Version,
		"commit":     Commit,
		"build_time": BuildTime,
	}
	c.JSON(http.StatusOK, NewSuccessResponse(versionInfo))
}

// handleHealthCheck 处理健康检查请求
func handleHealthCheck(c *gin.Context) {
	hub.clientsMutex.RLock()
	onlineClients := len(hub.clients)
	hub.clientsMutex.RUnlock()

	hub.uidMapMutex.RLock()
	onlineUIDs := len(hub.uidMap)
	hub.uidMapMutex.RUnlock()

	hub.groupsMutex.RLock()
	onlineGroups := len(hub.groups)
	hub.groupsMutex.RUnlock()

	hub.rpcMutex.RLock()
	rpcWaiting := len(hub.rpcCalls)
	hub.rpcMutex.RUnlock()

	healthInfo := map[string]interface{}{
		"start_time":     startTime,
		"uptime":         time.Since(startTime).String(),
		"online_clients": onlineClients,
		"online_uids":    onlineUIDs,
		"online_groups":  onlineGroups,
		"rpc_waiting":    rpcWaiting,
	}
	c.JSON(http.StatusOK, NewSuccessResponse(healthInfo))
}

// handleRuntimeInfo 处理运行时信息请求
func handleRuntimeInfo(c *gin.Context) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	runtimeInfo := map[string]interface{}{
		// Go运行时版本信息
		"version": runtime.Version(),
		// 当前Go程序可以使用的最大处理器核心数
		"gomaxprocs": runtime.GOMAXPROCS(0),
		// 系统中逻辑CPU的核心数
		"num_cpu": runtime.NumCPU(),
		// 当前存在的协程数量
		"num_goroutine": runtime.NumGoroutine(),
		// CGO调用的总次数
		"num_cgo_call": runtime.NumCgoCall(),
		// 程序运行的架构（如amd64、arm64等）
		"arch": runtime.GOARCH,
		// 程序运行的操作系统（如linux、windows、darwin等）
		"os": runtime.GOOS,
		// 使用的编译器（通常是gc）
		"compiler": runtime.Compiler,
		// 内存统计信息详细数据
		"mem_stats": map[string]interface{}{
			// 当前已分配但尚未释放的内存字节数
			"alloc": formatBytes(ms.Alloc),
			// 累计分配的内存总量（包括已释放的）
			"total_alloc": formatBytes(ms.TotalAlloc),
			// 从操作系统获取的内存总量
			"sys": formatBytes(ms.Sys),
			// 指针查找次数
			"lookups": ms.Lookups,
			// 累计malloc调用次数
			"mallocs": ms.Mallocs,
			// 累计free调用次数
			"frees": ms.Frees,
			// 堆内存中已分配但尚未释放的字节数
			"heap_alloc": formatBytes(ms.HeapAlloc),
			// 堆内存中从操作系统获取的内存总量
			"heap_sys": formatBytes(ms.HeapSys),
			// 堆内存中未使用但尚未归还给操作系统的内存
			"heap_idle": formatBytes(ms.HeapIdle),
			// 堆内存中正在使用的内存
			"heap_inuse": formatBytes(ms.HeapInuse),
			// 已归还给操作系统的堆内存
			"heap_released": formatBytes(ms.HeapReleased),
			// 堆内存中对象的数量
			"heap_objects": ms.HeapObjects,
			// 栈内存中正在使用的内存
			"stack_inuse": formatBytes(ms.StackInuse),
			// 从操作系统获取的栈内存总量
			"stack_sys": formatBytes(ms.StackSys),
			// mspan结构使用的内存（已使用）
			"mspan_inuse": formatBytes(ms.MSpanInuse),
			// mspan结构使用的内存（从系统获取的总量）
			"mspan_sys": formatBytes(ms.MSpanSys),
			// mcache结构使用的内存（已使用）
			"mcache_inuse": formatBytes(ms.MCacheInuse),
			// mcache结构使用的内存（从系统获取的总量）
			"mcache_sys": formatBytes(ms.MCacheSys),
			// Profiling bucket hash table使用的内存
			"buck_hash_sys": formatBytes(ms.BuckHashSys),
			// 垃圾回收器元数据使用的内存
			"gc_sys": formatBytes(ms.GCSys),
			// 其他系统分配使用的内存
			"other_sys": formatBytes(ms.OtherSys),
			// 下一次GC的目标堆大小
			"next_gc": formatBytes(ms.NextGC),
			// 上一次GC执行的时间（Unix时间戳格式）
			"last_gc": time.Unix(0, int64(ms.LastGC)).Format(time.RFC3339),
			// 所有GC暂停时间的总和
			"pause_total_ns": time.Duration(ms.PauseTotalNs).String(),
			// GC执行的总次数
			"num_gc": ms.NumGC,
			// 强制GC执行的次数
			"num_forced_gc": ms.NumForcedGC,
			// GC使用的CPU时间占比
			"gc_cpu_fraction": ms.GCCPUFraction,
		},
		// 服务启动时间
		"start_time": startTime,
		// 服务运行时间
		"uptime": time.Since(startTime).String(),
	}

	c.JSON(http.StatusOK, NewSuccessResponse(runtimeInfo))
}

// handleErrorMessageMap 处理错误消息映射表请求
func handleErrorMessageMap(c *gin.Context) {
	c.JSON(http.StatusOK, NewSuccessResponse(ErrorMessageMap))
}

// HTTP处理函数，向所有客户端或指定客户端发送字符串数据
func sendToAllHandler(c *gin.Context) {
	var req SendToAllRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.Data == "" {
		resp := NewCustomErrorResponse(ErrCodeInvalidParams, "缺少data参数")
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.sendToAll(req)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，向指定client_id发送数据
func sendToClientHandler(c *gin.Context) {
	clientID := c.Query("client_id")
	if clientID == "" {
		resp := NewErrorResponse(ErrCodeMissingClientID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	// 读取请求体作为字符串
	body, err := c.GetRawData()
	if err != nil {
		resp := NewErrorResponse(ErrCodeReadBodyFailed)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if len(body) == 0 {
		resp := NewErrorResponse(ErrCodeEmptyBody)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.sendToClient(clientID, body)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，断开指定client_id的客户端连接
func closeClientHandler(c *gin.Context) {
	var req struct {
		ClientID string `json:"client_id"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.ClientID == "" {
		resp := NewErrorResponse(ErrCodeMissingClientID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if hub.closeClient(req.ClientID) {
		resp := NewSuccessResponse(nil)
		c.JSON(http.StatusOK, resp)
	} else {
		resp := NewErrorResponse(ErrCodeClientNotFound)
		c.JSON(http.StatusOK, resp)
	}
}

// HTTP处理函数，判断指定client_id是否在线
func isClientOnlineHandler(c *gin.Context) {
	clientID := c.Query("client_id")
	if clientID == "" {
		resp := NewErrorResponse(ErrCodeMissingClientID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	online := hub.isClientOnline(clientID)
	data := map[string]interface{}{
		"online": online,
	}
	resp := NewSuccessResponse(data)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，用于处理绑定UID的请求
func bindUidHandler(c *gin.Context) {
	var req BindRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	// 检查必要参数
	if req.ClientID == "" {
		resp := NewErrorResponse(ErrCodeMissingClientID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.UID == "" {
		resp := NewErrorResponse(ErrCodeMissingUID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.Auth == "" {
		resp := NewErrorResponse(ErrCodeMissingAuth)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.bindUid(req.ClientID, req.UID, req.Auth)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，解绑UID
func unbindUidHandler(c *gin.Context) {
	var req BindRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	// 检查必要参数
	if req.ClientID == "" {
		resp := NewErrorResponse(ErrCodeMissingClientID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.UID == "" {
		resp := NewErrorResponse(ErrCodeMissingUID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.unbindUid(req.ClientID, req.UID, req.Auth)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，判断UID是否在线
func isUidOnlineHandler(c *gin.Context) {
	uid := c.Query("uid")
	if uid == "" {
		resp := NewErrorResponse(ErrCodeMissingUID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	online := hub.isUidOnline(uid)
	data := map[string]interface{}{
		"online": online,
	}
	resp := NewSuccessResponse(data)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，发送字符串数据到指定UID的所有在线客户端
func sendToUidHandler(c *gin.Context) {
	uid := c.Query("uid")
	if uid == "" {
		resp := NewErrorResponse(ErrCodeMissingUID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	// 读取请求体作为字符串
	body, err := c.GetRawData()
	if err != nil {
		resp := NewErrorResponse(ErrCodeReadBodyFailed)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if len(body) == 0 {
		resp := NewErrorResponse(ErrCodeEmptyBody)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	c.JSON(http.StatusOK, hub.sendToUid(uid, body))
}

// HTTP处理函数，获取UID的session
func getUidSessionHandler(c *gin.Context) {
	uid := c.Query("uid")
	if uid == "" {
		resp := NewErrorResponse(ErrCodeMissingUID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	c.JSON(http.StatusOK, hub.getUidSession(uid))
}

// HTTP处理函数，设置UID的session
func setUidSessionHandler(c *gin.Context) {
	uid := c.Query("uid")
	if uid == "" {
		resp := NewErrorResponse(ErrCodeMissingUID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	var session map[string]interface{}
	if err := c.ShouldBindJSON(&session); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	c.JSON(http.StatusOK, hub.setUidSession(uid, session))
}

// HTTP处理函数，获取client_id的session
func getClientSessionHandler(c *gin.Context) {
	clientID := c.Query("client_id")
	if clientID == "" {
		resp := NewErrorResponse(ErrCodeMissingClientID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	c.JSON(http.StatusOK, hub.getClientSession(clientID))
}

// HTTP处理函数，设置client_id的session
func setClientSessionHandler(c *gin.Context) {
	clientID := c.Query("client_id")
	if clientID == "" {
		resp := NewErrorResponse(ErrCodeMissingClientID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	var session map[string]interface{}
	if err := c.ShouldBindJSON(&session); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	c.JSON(http.StatusOK, hub.setClientSession(clientID, session))
}

// HTTP处理函数，RPC调用接口
func rpcHandler(c *gin.Context) {
	// 从请求头获取UID
	uid := c.GetHeader("uid")
	if uid == "" {
		resp := NewErrorResponse(ErrCodeMissingUIDHeader)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	// 读取请求体作为字符串
	body, err := c.GetRawData()
	if err != nil {
		resp := NewErrorResponse(ErrCodeReadBodyFailed)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if len(body) == 0 {
		resp := NewErrorResponse(ErrCodeEmptyBody)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	req := &RPCRequest{}
	if err := json.Unmarshal(body, req); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.ID == 0 {
		resp := NewErrorResponse(ErrCodeMissingID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.Method == "" {
		resp := NewErrorResponse(ErrCodeMissingMethod)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	c.JSON(http.StatusOK, hub.callRPC(uid, body, req))
}

// HTTP处理函数，根据UID获取所有绑定的在线client_id
func getClientIdByUidHandler(c *gin.Context) {
	uid := c.Query("uid")
	if uid == "" {
		resp := NewErrorResponse(ErrCodeMissingUID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	hub.uidMapMutex.RLock()
	clientIDs, ok := hub.uidMap[uid]
	hub.uidMapMutex.RUnlock()
	if !ok {
		resp := NewErrorResponse(ErrCodeUIDNotOnline)
		c.JSON(http.StatusOK, resp)
		return
	}

	onlineClientIDs := make([]string, 0)
	hub.clientsMutex.RLock()
	for _, clientID := range clientIDs {
		if _, clientExists := hub.clients[clientID]; clientExists {
			onlineClientIDs = append(onlineClientIDs, clientID)
		}
	}
	hub.clientsMutex.RUnlock()

	data := map[string]interface{}{
		"client_ids": onlineClientIDs,
	}
	resp := NewSuccessResponse(data)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，根据client_id获取绑定的UID
func getUidByClientIdHandler(c *gin.Context) {
	clientID := c.Query("client_id")
	if clientID == "" {
		resp := NewErrorResponse(ErrCodeMissingClientID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	hub.clientsMutex.RLock()
	client, exists := hub.clients[clientID]
	hub.clientsMutex.RUnlock()

	if !exists {
		resp := NewErrorResponse(ErrCodeClientNotFound)
		c.JSON(http.StatusOK, resp)
		return
	}

	client.mutex.RLock()
	defer client.mutex.RUnlock()
	var data map[string]interface{}
	if client.UID == "" {
		data = map[string]interface{}{
			"uid": nil,
		}
	} else {
		data = map[string]interface{}{
			"uid": client.UID,
		}
	}

	resp := NewSuccessResponse(data)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取全局所有在线UID数量
func getAllUidCountHandler(c *gin.Context) {
	hub.uidMapMutex.RLock()
	onlineUidCount := len(hub.uidMap)
	hub.uidMapMutex.RUnlock()

	data := map[string]interface{}{
		"count": onlineUidCount,
	}
	resp := NewSuccessResponse(data)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取当前在线连接总数
func getAllClientCountHandler(c *gin.Context) {
	hub.clientsMutex.RLock()
	count := len(hub.clients)
	hub.clientsMutex.RUnlock()

	data := map[string]interface{}{
		"count": count,
	}
	resp := NewSuccessResponse(data)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取全局所有在线client_id列表
func getAllClientIdListHandler(c *gin.Context) {
	hub.clientsMutex.RLock()
	defer hub.clientsMutex.RUnlock()

	if len(hub.clients) == 0 {
		data := map[string]interface{}{
			"client_ids": nil,
		}
		resp := NewSuccessResponse(data)
		c.JSON(http.StatusOK, resp)
		return
	}

	clientIDs := make([]string, 0, len(hub.clients))
	for clientID := range hub.clients {
		clientIDs = append(clientIDs, clientID)
	}

	data := map[string]interface{}{
		"client_ids": clientIDs,
	}
	resp := NewSuccessResponse(data)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取所有在线uid列表
func getAllUidListHandler(c *gin.Context) {
	hub.uidMapMutex.RLock()
	defer hub.uidMapMutex.RUnlock()
	uidLen := len(hub.uidMap)
	if uidLen == 0 {
		data := map[string]interface{}{
			"uids": nil,
		}
		resp := NewSuccessResponse(data)
		c.JSON(http.StatusOK, resp)
		return
	}

	uids := make([]string, 0, uidLen)
	for uid := range hub.uidMap {
		uids = append(uids, uid)
	}

	if len(uids) == 0 {
		data := map[string]interface{}{
			"uids": nil,
		}
		resp := NewSuccessResponse(data)
		c.JSON(http.StatusOK, resp)
		return
	}

	data := map[string]interface{}{
		"uids": uids,
	}
	resp := NewSuccessResponse(data)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取所有在线客户端的session信息
func getAllClientSessionsHandler(c *gin.Context) {
	hub.clientsMutex.RLock()
	defer hub.clientsMutex.RUnlock()

	if len(hub.clients) == 0 {
		data := map[string]interface{}{
			"sessions": map[string]interface{}{},
		}
		resp := NewSuccessResponse(data)
		c.JSON(http.StatusOK, resp)
		return
	}

	sessions := make(map[string]interface{})
	for clientID, client := range hub.clients {
		client.mutex.RLock()
		sessions[clientID] = client.Session
		client.mutex.RUnlock()
	}

	data := map[string]interface{}{
		"sessions": sessions,
	}
	resp := NewSuccessResponse(data)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，将客户端加入群组
func joinGroupHandler(c *gin.Context) {
	var req GroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	// 检查必要参数
	if req.Group == "" {
		resp := NewErrorResponse(ErrCodeMissingGroup)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.ClientID == "" {
		resp := NewErrorResponse(ErrCodeMissingClientID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.joinGroup(req.Group, req.ClientID)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，将客户端从群组中移除
func leaveGroupHandler(c *gin.Context) {
	var req GroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	// 检查必要参数
	if req.Group == "" {
		resp := NewErrorResponse(ErrCodeMissingGroup)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.ClientID == "" {
		resp := NewErrorResponse(ErrCodeMissingClientID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.leaveGroup(req.Group, req.ClientID)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，解散群组
func ungroupHandler(c *gin.Context) {
	var req struct {
		Group string `json:"group"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	// 检查必要参数
	if req.Group == "" {
		resp := NewErrorResponse(ErrCodeMissingGroup)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.ungroup(req.Group)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，向群组发送消息
func sendToGroupHandler(c *gin.Context) {
	var req SendToGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp := NewErrorResponse(ErrCodeInvalidParams)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.Group == "" {
		resp := NewErrorResponse(ErrCodeMissingGroup)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	if req.Data == "" {
		resp := NewErrorResponse(ErrCodeMissingData)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	c.JSON(http.StatusOK, hub.sendToGroup(req))
}

// HTTP处理函数，获取群组中的客户端数量
func getClientIdCountByGroupHandler(c *gin.Context) {
	group := c.Query("group")
	if group == "" {
		resp := NewErrorResponse(ErrCodeMissingGroup)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.getClientIdCountByGroup(group)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取群组中所有客户端的session信息
func getClientSessionsByGroupHandler(c *gin.Context) {
	group := c.Query("group")
	if group == "" {
		resp := NewErrorResponse(ErrCodeMissingGroup)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.getClientSessionsByGroup(group)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取群组中的所有client_id列表
func getClientIdListByGroupHandler(c *gin.Context) {
	group := c.Query("group")
	if group == "" {
		resp := NewErrorResponse(ErrCodeMissingGroup)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.getClientIdListByGroup(group)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取群组中所有客户端绑定的UID列表
func getUidListByGroupHandler(c *gin.Context) {
	group := c.Query("group")
	if group == "" {
		resp := NewErrorResponse(ErrCodeMissingGroup)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.getUidListByGroup(group)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取群组中客户端绑定的UID数量
func getUidCountByGroupHandler(c *gin.Context) {
	group := c.Query("group")
	if group == "" {
		resp := NewErrorResponse(ErrCodeMissingGroup)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.getUidCountByGroup(group)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取全局所有在线群组的组名数组
func getAllGroupIdListHandler(c *gin.Context) {
	hub.groupsMutex.RLock()
	defer hub.groupsMutex.RUnlock()
	groupsLen := len(hub.groups)
	if groupsLen == 0 {
		resp := NewSuccessResponse(map[string]interface{}{
			"groups": nil,
		})
		c.JSON(http.StatusOK, resp)
		return
	}

	hub.clientsMutex.RLock()
	defer hub.clientsMutex.RUnlock()
	// 收集所有群组名
	groupNames := make([]string, 0, groupsLen)
	for groupName := range hub.groups {
		// 检查群组中是否有在线客户端
		hasOnlineClients := false
		for clientID := range hub.groups[groupName] {
			if _, clientExists := hub.clients[clientID]; clientExists {
				hasOnlineClients = true
				break
			}
		}

		// 只有当群组中有在线客户端时才添加到结果中
		if hasOnlineClients {
			groupNames = append(groupNames, groupName)
		}
	}

	if len(groupNames) == 0 {
		resp := NewSuccessResponse(map[string]interface{}{
			"groups": nil,
		})
		c.JSON(http.StatusOK, resp)
		return
	}

	resp := NewSuccessResponse(map[string]interface{}{
		"groups": groupNames,
	})
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取指定UID所属的所有群组
func getGroupByUidHandler(c *gin.Context) {
	uid := c.Query("uid")
	if uid == "" {
		resp := NewErrorResponse(ErrCodeMissingUID)
		c.JSON(http.StatusBadRequest, resp)
		return
	}

	resp := hub.getGroupByUid(uid)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，重新加载配置
func reloadConfigHandler(c *gin.Context) {
	// 读取配置文件路径（使用读锁）
	configMutex.RLock()
	currentConfigPath := configPath
	configMutex.RUnlock()

	if currentConfigPath == "" {
		resp := NewErrorResponse(ErrCodeConfigFileNotFound)
		c.JSON(http.StatusInternalServerError, resp)
		return
	}

	// 尝试重新加载配置
	if err := loadConfig(currentConfigPath); err != nil {
		resp := NewCustomErrorResponse(ErrCodeConfigReloadFailed, fmt.Sprintf("配置重载失败: %v", err))
		c.JSON(http.StatusInternalServerError, resp)
		return
	}

	// 更新webhook HTTP客户端配置
	configMutex.RLock()
	transport := &http.Transport{
		MaxIdleConns:        config.WebhookMaxIdleConns,
		MaxIdleConnsPerHost: config.WebhookMaxIdleConnsPerHost,
		IdleConnTimeout:     time.Duration(config.WebhookIdleConnTimeout) * time.Second,
	}
	webhookHTTPClient = &http.Client{
		Transport: transport,
		Timeout:   time.Duration(config.WebhookTimeout) * time.Second,
	}
	configMutex.RUnlock()

	resp := NewSuccessResponseWithMessage("配置重载成功", nil)
	c.JSON(http.StatusOK, resp)
}

// HTTP处理函数，获取当前配置
func getConfigHandler(c *gin.Context) {
	configMutex.RLock()
	currentConfig := *config
	configMutex.RUnlock()

	resp := NewSuccessResponse(currentConfig)
	c.JSON(http.StatusOK, resp)
}

// authMiddleware HTTP请求TOKEN验证中间件
func authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 获取请求头中的TOKEN
		token := c.GetHeader("Token")

		if token == "" {
			resp := NewErrorResponse(ErrCodeMissingToken)
			c.JSON(http.StatusUnauthorized, resp)
			c.Abort()
			return
		}

		configMutex.RLock()
		expectedToken := config.Token
		configMutex.RUnlock()

		if token != expectedToken {
			resp := NewErrorResponse(ErrCodeInvalidToken)
			c.JSON(http.StatusUnauthorized, resp)
			c.Abort()
			return
		}

		c.Next()
	}
}

// SignatureMiddleware 签名验证中间件
func signatureMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 获取请求头中的时间戳和签名
		timestamp := c.GetHeader("Timestamp")
		signature := c.GetHeader("Signature")

		if timestamp == "" {
			resp := NewErrorResponse(ErrCodeMissingTimestamp)
			c.JSON(http.StatusBadRequest, resp)
			c.Abort()
			return
		}

		if signature == "" {
			resp := NewErrorResponse(ErrCodeMissingSignature)
			c.JSON(http.StatusBadRequest, resp)
			c.Abort()
			return
		}

		// 验证时间戳格式
		ts, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil {
			resp := NewErrorResponse(ErrCodeInvalidTimestamp)
			c.JSON(http.StatusBadRequest, resp)
			c.Abort()
			return
		}

		// 检查时间戳是否过期（允许1分钟的时间差）
		currentTime := time.Now().UnixMilli()
		if abs(currentTime-ts) > 60*1000 {
			resp := NewErrorResponse(ErrCodeTimestampExpired)
			c.JSON(http.StatusBadRequest, resp)
			c.Abort()
			return
		}

		body, err := c.GetRawData()
		if err != nil {
			resp := NewErrorResponse(ErrCodeReadBodyFailed)
			c.JSON(http.StatusBadRequest, resp)
			return
		}

		configMutex.RLock()
		token := config.Token
		configMutex.RUnlock()

		if !verifySignature(string(body), timestamp, token, signature) {
			resp := NewErrorResponse(ErrCodeInvalidSignature)
			c.JSON(http.StatusUnauthorized, resp)
			c.Abort()
			return
		}

		c.Next()
	}
}

// abs 返回绝对值
func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// 主函数
func main() {
	var (
		configPathFlag = flag.String("c", "config.json", "配置文件路径")
		showVersion    = flag.Bool("v", false, "显示版本信息")
	)
	var secureMode bool // 安全模式标志
	flag.BoolVar(&secureMode, "s", false, "安全模式（true时启用签名验证，false时使用token验证）")
	flag.Parse()

	startTime = time.Now() // 记录启动时间
	fmt.Printf("suws.cn WebSocket Server\n")
	fmt.Printf("Version:    %s\n", Version)
	fmt.Printf("Commit:     %s\n", Commit)
	fmt.Printf("Build Time: %s\n", BuildTime)
	if *showVersion {
		os.Exit(0)
	}

	// 保存配置文件路径
	configPath = *configPathFlag

	// 加载配置文件
	if err := loadConfig(configPath); err != nil {
		slog.Error("加载配置文件失败: %v", err)
		os.Exit(1)
	}

	// Set Gin mode based on verbose flag
	if config.Log.Verbose {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// Configure logging
	var logLevel slog.Level
	if config.Log.Verbose {
		logLevel = slog.LevelDebug
	} else {
		logLevel = slog.LevelInfo
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	// 初始化带连接池的 webhook HTTP 客户端
	transport := &http.Transport{
		MaxIdleConns:        config.WebhookMaxIdleConns,
		MaxIdleConnsPerHost: config.WebhookMaxIdleConnsPerHost,
		IdleConnTimeout:     time.Duration(config.WebhookIdleConnTimeout) * time.Second,
	}
	webhookHTTPClient = &http.Client{
		Transport: transport,
		Timeout:   time.Duration(config.WebhookTimeout) * time.Second,
	}

	// 初始化hub
	hub = newHub()
	go hub.run()

	// 创建gin路由器
	router := gin.Default()
	router.StaticFile("/favicon.ico", "./favicon.ico")

	// 添加统计信息路由
	router.GET("/", handleStats)

	// WebSocket接入点（无需认证）
	router.GET("/ws", handleWebSocket)
	router.GET("/websocket", handleWebSocket)

	// 添加公共路由分组（无需认证）
	public := router.Group("/public")
	{
		public.GET("/stats", handleStats)
		public.GET("/version", handleVersion)
		public.GET("/health", handleHealthCheck)
		public.GET("/runtime", handleRuntimeInfo)
		public.GET("/errorMessages", handleErrorMessageMap)
	}

	// 创建需要认证的API分组
	api := router.Group("/api")
	if secureMode {
		fmt.Println("当前运行模式：安全模式（签名验证）")
		api.Use(signatureMiddleware())
	} else {
		fmt.Println("当前运行模式：普通模式（Token验证）")
		api.Use(authMiddleware())
	}
	{
		api.POST("/sendToAll", sendToAllHandler)
		api.POST("/sendToClient", sendToClientHandler)
		api.POST("/closeClient", closeClientHandler)
		api.GET("/isOnline", isClientOnlineHandler)
		api.POST("/bindUid", bindUidHandler)
		api.POST("/unbindUid", unbindUidHandler)
		api.GET("/isUidOnline", isUidOnlineHandler)
		api.POST("/sendToUid", sendToUidHandler)
		api.GET("/getUidSession", getUidSessionHandler)        // 【GatewayWorker无此接口】
		api.POST("/setUidSession", setUidSessionHandler)       // 【GatewayWorker无此接口】
		api.GET("/getClientSession", getClientSessionHandler)  // 别名，功能同 getSession
		api.POST("/setClientSession", setClientSessionHandler) // 别名，功能同 setSession
		api.GET("/getSession", getClientSessionHandler)
		api.POST("/setSession", setClientSessionHandler)
		api.POST("/rpc", rpcHandler) // 【GatewayWorker无此接口】
		api.GET("/getClientIdByUid", getClientIdByUidHandler)
		api.GET("/getUidByClientId", getUidByClientIdHandler)
		api.GET("/getAllUidCount", getAllUidCountHandler)
		api.GET("/getAllClientIdCount", getAllClientCountHandler)
		api.GET("/getAllClientCount", getAllClientCountHandler) // 别名，功能同 getAllClientIdCount
		api.GET("/getAllClientIdList", getAllClientIdListHandler)
		api.GET("/getAllUidList", getAllUidListHandler)
		api.GET("/getAllClientSessions", getAllClientSessionsHandler)

		// 群组管理接口
		api.POST("/joinGroup", joinGroupHandler)
		api.POST("/leaveGroup", leaveGroupHandler)
		api.POST("/ungroup", ungroupHandler)
		api.POST("/sendToGroup", sendToGroupHandler)
		api.GET("/getClientIdCountByGroup", getClientIdCountByGroupHandler)
		api.GET("/getClientSessionsByGroup", getClientSessionsByGroupHandler)
		api.GET("/getClientIdListByGroup", getClientIdListByGroupHandler)
		api.GET("/getUidListByGroup", getUidListByGroupHandler)
		api.GET("/getUidCountByGroup", getUidCountByGroupHandler)
		api.GET("/getAllGroupIdList", getAllGroupIdListHandler)
		api.GET("/getGroupByUid", getGroupByUidHandler) // 【GatewayWorker无此接口】

		// 配置热更新接口
		api.POST("/reloadConfig", reloadConfigHandler) // 【GatewayWorker无此接口】
		api.GET("/getConfig", getConfigHandler)        // 【GatewayWorker无此接口】
	}

	port := config.Port
	// 监听端口，默认8788
	if port == "" || port == "0" {
		port = "8788"
	}

	fmt.Printf("WebSocket服务器启动，监听端口 %s...\n", port)
	err := router.Run(":" + port)
	if err != nil {
		slog.Error("启动WebSocket服务器失败: %v", err)
		os.Exit(1)
	}
}
