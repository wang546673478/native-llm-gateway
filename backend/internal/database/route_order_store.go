package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// RouteOrderStore 访问 route_order 排序改写表(Level 2/3 优先级改写)。
// 只存「用户改写」;默认排序(未改写)由 created_at 派生,本表无行 = 零代价。
type RouteOrderStore interface {
	// ListByScope 取某作用域的改写(Scope=provider → 层内 provider 排序;
	// Scope=key 且 Provider 非空 → 某 provider 内 key 排序)。
	// billingSource 非空时按层过滤(provider 序按 token_plan/api 分别存)。
	ListByScope(ctx context.Context, scope string, provider string, billingSource string) ([]RouteOrder, error)
	// Replace 整体替换某作用域的改写列表(前端拖拽后落库)。
	// 同一 (scope,provider,billing_source) 作用域先删后插,保证与界面一致。
	Replace(ctx context.Context, scope, provider, billingSource string, names []string) error
	// ResetScope 删除某作用域全部改写 → 回到默认 created_at 顺序。
	ResetScope(ctx context.Context, scope string, provider string) error
	// DeleteByProvider 删掉某个 provider/面的全部排序改写,返回删除行数。
	// provider 列和 name 列都是普通字符串(非外键),厂商/中转站被硬删后
	// 这些行会成孤儿 —— 且 scope=provider 的孤儿仍占着层内 seq 名次,
	// 把活着的候选往后挤(实测占了 api 层 seq 0/1 两个最高优先级位)。
	DeleteByProvider(ctx context.Context, provider string) (int64, error)
}

type gormRouteOrderStore struct{ db *gorm.DB }

// NewRouteOrderStore 构造实现
func NewRouteOrderStore(db *gorm.DB) RouteOrderStore { return &gormRouteOrderStore{db: db} }

func (s *gormRouteOrderStore) ListByScope(ctx context.Context, scope string, provider string, billingSource string) ([]RouteOrder, error) {
	var out []RouteOrder
	q := s.db.WithContext(ctx).Model(&RouteOrder{}).Where("scope = ?", scope)
	if provider != "" {
		q = q.Where("provider = ?", provider)
	}
	if billingSource != "" {
		q = q.Where("billing_source = ?", billingSource)
	}
	if err := q.Order("seq ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

func (s *gormRouteOrderStore) Replace(ctx context.Context, scope, provider, billingSource string, names []string) error {
	// 一次事务:先删「同一作用域(scope+provider+billing_source)」的旧行,再插入新顺序。
	// 关键:必须按 billing_source 过滤 —— 否则删错层(如保存 token_plan 会误删 api 层),
	// provider 改写跨层互相覆盖(2026-08-10 实测:token_plan 层 provider 顺序被 api 层清掉)。
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Where("scope = ?", scope)
		if provider != "" {
			q = q.Where("provider = ?", provider)
		}
		if billingSource != "" {
			q = q.Where("billing_source = ?", billingSource)
		}
		if err := q.Delete(&RouteOrder{}).Error; err != nil {
			return fmt.Errorf("clear route_order: %w", err)
		}
		for i, n := range names {
			r := RouteOrder{
				Scope:         scope,
				Provider:      provider,
				Name:          n,
				BillingSource: billingSource,
				Seq:           i, // 小在前
			}
			if err := tx.Create(&r).Error; err != nil {
				return fmt.Errorf("insert route_order %q: %w", n, err)
			}
		}
		return nil
	})
}

func (s *gormRouteOrderStore) DeleteByProvider(ctx context.Context, provider string) (int64, error) {
	// 空串直接返回:provider="" 会匹配 scope=provider 行的空 provider 列,
	// 一把删光全部层内 provider 排序(RouteOrder 的 provider 列在 scope=provider
	// 时本就是空的,名字存在 name 列)。
	if provider == "" {
		return 0, nil
	}
	// 两个位置都要删,少一个就留下孤儿:
	//   scope=provider → 名字在 name 列(该 provider 在层内的名次)
	//   scope=key      → 名字在 provider 列(该 provider 内部的 key 顺序)
	res := s.db.WithContext(ctx).
		Where("(scope = ? AND name = ?) OR (scope = ? AND provider = ?)",
			RouteScopeProvider, provider, RouteScopeKey, provider).
		Delete(&RouteOrder{})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

func (s *gormRouteOrderStore) ResetScope(ctx context.Context, scope string, provider string) error {
	q := s.db.WithContext(ctx).Where("scope = ?", scope)
	if provider != "" {
		q = q.Where("provider = ?", provider)
	}
	return q.Delete(&RouteOrder{}).Error
}
