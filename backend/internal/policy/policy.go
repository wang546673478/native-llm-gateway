// Package policy 实现四种路由选择策略
// 对应规格书:priority / weight / cost / health
package policy

import (
	"errors"
	"math/rand"
)

// ProviderRoute 单条路由目标(供 router 和 policy 共用,中性类型避免 import cycle)
type ProviderRoute struct {
	Name     string
	Model    string
	Priority int
	Weight   int
}

// AliasConfig 单个别名的路由规则
// P53: 加 TargetModel — 短格式 auto-discovery 时,Router 自动找
// 所有声明该 model 的 provider
type AliasConfig struct {
	Alias       string
	Strategy    string
	Providers   []ProviderRoute
	TargetModel string // P53: 短格式标记;非空时 Router 走 auto-discovery
}

// ErrNoCandidate 没有候选
var ErrNoCandidate = errors.New("policy: no candidate")

// Policy 选择策略接口
type Policy interface {
	Name() string
	Order(candidates []ProviderRoute) ([]ProviderRoute, error)
}

// PriorityPolicy Priority 数字越小越优先
type PriorityPolicy struct{}

func NewPriorityPolicy() *PriorityPolicy { return &PriorityPolicy{} }
func (p *PriorityPolicy) Name() string    { return "priority" }

func (p *PriorityPolicy) Order(cs []ProviderRoute) ([]ProviderRoute, error) {
	if len(cs) == 0 {
		return nil, ErrNoCandidate
	}
	out := make([]ProviderRoute, len(cs))
	copy(out, cs)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Priority < out[j-1].Priority; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// WeightPolicy 按 Weight 比例随机选一个置顶
type WeightPolicy struct {
	r *rand.Rand
}

func NewWeightPolicy() *WeightPolicy {
	return &WeightPolicy{r: rand.New(rand.NewSource(rand.Int63()))}
}
func (p *WeightPolicy) Name() string { return "weight" }

func (p *WeightPolicy) Order(cs []ProviderRoute) ([]ProviderRoute, error) {
	if len(cs) == 0 {
		return nil, ErrNoCandidate
	}
	total := 0
	for _, c := range cs {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		total += w
	}

	pick := p.r.Intn(total)
	cum := 0
	pickedIdx := 0
	for i, c := range cs {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		cum += w
		if pick < cum {
			pickedIdx = i
			break
		}
	}

	out := make([]ProviderRoute, 0, len(cs))
	out = append(out, cs[pickedIdx])
	for i, c := range cs {
		if i == pickedIdx {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// CostPolicy 按 Weight 字段升序(占位实现,见 router.go 注释)
type CostPolicy struct{}

func NewCostPolicy() *CostPolicy { return &CostPolicy{} }
func (p *CostPolicy) Name() string { return "cost" }

func (p *CostPolicy) Order(cs []ProviderRoute) ([]ProviderRoute, error) {
	if len(cs) == 0 {
		return nil, ErrNoCandidate
	}
	out := make([]ProviderRoute, len(cs))
	copy(out, cs)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Weight < out[j-1].Weight; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// HealthPolicy 保持原顺序(健康过滤在 router 层完成)
type HealthPolicy struct{}

func NewHealthPolicy() *HealthPolicy { return &HealthPolicy{} }
func (p *HealthPolicy) Name() string  { return "health" }

func (p *HealthPolicy) Order(cs []ProviderRoute) ([]ProviderRoute, error) {
	if len(cs) == 0 {
		return nil, ErrNoCandidate
	}
	out := make([]ProviderRoute, len(cs))
	copy(out, cs)
	return out, nil
}
