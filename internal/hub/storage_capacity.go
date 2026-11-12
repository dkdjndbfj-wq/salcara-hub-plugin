package hub

const (
	maxDurableOperations      = 65536
	maxOwnerDurableOperations = 4096
	maxDurableBatches         = 200000
)

// Aggregate counts only. Owner identities, tickets and message content never
// enter administrator capacity responses.
type StorageRecordCapacity struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Used    int64  `json:"used"`
	Limit   int64  `json:"limit"`
	Warning bool   `json:"warning"`
	Full    bool   `json:"full"`
}

func recordCapacity(kind, label string, used, limit int64) StorageRecordCapacity {
	return StorageRecordCapacity{Kind: kind, Label: label, Used: used, Limit: limit, Warning: used >= limit*80/100, Full: used >= limit}
}

func operationCapacityMessage() string {
	return "防重复执行记录已满，暂不能接收新任务。请联系管理员到“系统 → 消息与图片存储”查看容量；清理到期内容不会释放这些安全记录。"
}
