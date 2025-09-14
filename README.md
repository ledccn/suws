# Su WebSocket服务

SuWS是一个轻量级高性能的WebSocket服务器，支持客户端连接管理、UID绑定解绑、群组管理、消息推送、RPC调用等功能。

## 启动服务

```shell
# 默认使用 config.json 配置文件
./suws

# 指定配置文件
./suws -c /path/to/config.json
```

## 配置文件

```json
{
  "token": "your_api_token",
  "webhook": "https://your-webhook-url.com/webhook",
  "webhook_timeout": 5,
  "log": {
    "verbose": false
  }
}
```

## WebSocket连接

### 连接地址

- ws://localhost:8788/ws
- ws://localhost:8788/websocket

### 连接流程

1. 客户端连接WebSocket服务器
2. 服务器返回初始化信息:

    ```json
    {
      "event": "init",
      "client_id": "客户端唯一标识",
      "auth": "认证字符串"
    }
    ```

3. 管理端可以使用返回的 `client_id` 和 `auth` 进行UID绑定

## API管理接口

所有HTTP 管理API接口都需要在请求头中包含 `token` 字段进行认证。
