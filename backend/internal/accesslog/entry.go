package accesslog

import "time"

// AccessEntry 是被打包到 DB 和 (可能的)日志里的一条记录
//
// 设计为值类型语义 — Recorder 拿到指针后立即消费,Rec caller 不应修改
type AccessEntry struct {
	ID             uint      `json:"id"`
	TraceID        string    `json:"trace_id"`
	CreatedAt      time.Time `json:"created_at"`
	GatewayKeyID   string    `json:"gateway_key_id"`
	GatewayKeyName string    `json:"gateway_key_name"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	ClientIP       string    `json:"client_ip"`
	UserAgent      string    `json:"user_agent"`
	RequestedModel string    `json:"requested_model"`
	FinalModel     string    `json:"final_model"`
	ProviderName   string    `json:"provider_name"`
	Protocol       string    `json:"protocol"`
	IsStream       bool      `json:"is_stream"`
	StatusCode     int       `json:"status_code"`
	ErrorType      string    `json:"error_type"`
	LatencyMs      int       `json:"latency_ms"`
	ReqBodyPath    string    `json:"req_body_path"`
	ReqBodySize    int       `json:"req_body_size"`
	RespBodyPath   string    `json:"resp_body_path"`
	RespBodySize   int       `json:"resp_body_size"`
	// Truncated 状态由 文件名后缀 .truncated.json 标记,不在业务 struct 里(spec F1 决议)
}